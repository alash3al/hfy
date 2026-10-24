package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/files"
	"github.com/alash3al/hfy/internal/svc"
	"github.com/alash3al/hfy/internal/types"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"gocloud.dev/blob"
	"gocloud.dev/gcerrors"
)

// fsEnabled reports whether filesystem uploads are usable: configured and
// EnableFilesystem was called with a bucket.
func (s *Server) fsEnabled() bool {
	return s != nil && s.cfg != nil && s.cfg.Filesystem != nil && s.bucket != nil
}

func allowedMime(types []string, mime string) bool {
	return slices.ContainsFunc(types, func(t string) bool { return strings.EqualFold(t, mime) })
}

func newUploadToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand never fails
	return base64.RawURLEncoding.EncodeToString(b)
}

type requestUploadRequest struct {
	Mime string `json:"mime"`
	Size int64  `json:"size"` // bytes, always required
}

type requestUploadResponse struct {
	File      string    `json:"file"`
	UploadURL string    `json:"upload_url"`
	ExpiresAt time.Time `json:"expires_at"`
	Direct    bool      `json:"direct"`
}

// requestUpload grants one upload slot: a fresh object key under the
// conversation's prefix and where to PUT its bytes.
func (s *Server) requestUpload(w http.ResponseWriter, r *http.Request, g *db.GrantRow) error {
	if !s.fsEnabled() {
		return &httpError{http.StatusNotFound, "file uploads are not configured"}
	}
	var req requestUploadRequest
	if err := bindJSON(r, &req); err != nil {
		return err
	}
	mime := strings.ToLower(strings.TrimSpace(req.Mime))
	if !allowedMime(s.cfg.Filesystem.Types, mime) {
		return &httpError{http.StatusBadRequest, "file type is not allowed"}
	}
	if req.Size <= 0 {
		return &httpError{http.StatusBadRequest, "size is required (bytes, greater than 0)"}
	}
	if req.Size > int64(s.cfg.Filesystem.MaxBytes) {
		return &httpError{http.StatusBadRequest, "file too large"}
	}
	// Slots are temporary; the verified copies events name live under
	// conversations/ and are made by verifyFileAttachments.
	key := "uploads/" + g.ConversationUUID.String() + "/" + newUploadToken() + files.ExtForMIME(mime)
	expires := time.Now().Add(s.cfg.Filesystem.UploadTTL.D())
	resp := requestUploadResponse{File: key, ExpiresAt: expires}
	if files.Direct(files.Scheme(s.cfg.Filesystem.DSN)) {
		u, err := s.bucket.SignedURL(r.Context(), key, &blob.SignedURLOptions{
			Method: http.MethodPut, Expiry: s.cfg.Filesystem.UploadTTL.D(), ContentType: mime,
			// Pin the content length: S3 would otherwise accept a body of
			// any size, past filesystem.max_bytes included.
			BeforeSign: func(as func(any) bool) error {
				var in *s3.PutObjectInput
				if as(&in) {
					in.ContentLength = aws.Int64(req.Size)
				}
				return nil
			},
		})
		if err != nil {
			return err
		}
		resp.UploadURL, resp.Direct = u, true
	} else {
		// The proxied PUT carries its own expiry: a signature over key+mime.
		sig := files.SignPut(s.cfg.Server.Secret, key, mime, expires)
		resp.UploadURL = "/v1/conversation/uploads/" + key + "?exp=" + strconv.FormatInt(expires.Unix(), 10) + "&sig=" + sig
	}
	return writeJSON(w, http.StatusCreated, resp)
}

// putUpload receives an upload's bytes for proxied (non-S3) drivers.
func (s *Server) putUpload(w http.ResponseWriter, r *http.Request, g *db.GrantRow) error {
	if !s.fsEnabled() {
		return &httpError{http.StatusNotFound, "file uploads are not configured"}
	}
	if files.Direct(files.Scheme(s.cfg.Filesystem.DSN)) {
		return &httpError{http.StatusBadRequest, "use the presigned URL"}
	}
	key := r.PathValue("key")
	if !strings.HasPrefix(key, "uploads/"+g.ConversationUUID.String()+"/") || !validFileKey(key) {
		return &httpError{http.StatusForbidden, "forbidden"}
	}
	ct, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
	mime := strings.ToLower(strings.TrimSpace(ct))
	if !allowedMime(s.cfg.Filesystem.Types, mime) {
		return &httpError{http.StatusBadRequest, "file type is not allowed"}
	}
	expUnix, err := strconv.ParseInt(r.URL.Query().Get("exp"), 10, 64)
	if err != nil || !files.VerifyPut(s.cfg.Server.Secret, key, mime, r.URL.Query().Get("sig"), time.Unix(expUnix, 0)) {
		return &httpError{http.StatusForbidden, "invalid or expired upload slot"}
	}
	if _, err := s.bucket.Attributes(r.Context(), key); err == nil {
		return &httpError{http.StatusConflict, "file already uploaded"}
	}
	// Uploads may be slow; the server's global read deadline is for API calls.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(10 * time.Minute))
	// Stream: an upload must never be buffered whole. If it proves oversized,
	// cancel the writer's own context before Close so nothing is committed.
	max := int64(s.cfg.Filesystem.MaxBytes)
	wctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	bw, err := s.bucket.NewWriter(wctx, key, &blob.WriterOptions{ContentType: mime, IfNotExist: true})
	if err != nil {
		return err
	}
	n, err := io.Copy(bw, io.LimitReader(r.Body, max+1))
	if err != nil {
		cancel() // abort before Close: a partial write must never commit
		_ = bw.Close()
		return err
	}
	if n > max {
		cancel()
		_ = bw.Close()
		return &httpError{http.StatusRequestEntityTooLarge, "file too large"}
	}
	if err := bw.Close(); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// serveFile streams one stored file to whoever holds its transient signed
// URL (no grant: providers and browsers fetch with the URL alone).
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request) error {
	if !s.fsEnabled() {
		return &httpError{http.StatusNotFound, "file uploads are not configured"}
	}
	key := r.PathValue("key")
	expUnix, err := strconv.ParseInt(r.URL.Query().Get("exp"), 10, 64)
	if err != nil || !files.VerifyGet(s.cfg.Server.Secret, key, r.URL.Query().Get("sig"), time.Unix(expUnix, 0)) {
		return &httpError{http.StatusForbidden, "invalid or expired link"}
	}
	ctx := r.Context()
	attrs, err := s.bucket.Attributes(ctx, key)
	if err != nil {
		if gcerrors.Code(err) == gcerrors.NotFound {
			return &httpError{http.StatusNotFound, "not found"}
		}
		return err
	}
	rd, err := s.bucket.NewReader(ctx, key, nil)
	if err != nil {
		if gcerrors.Code(err) == gcerrors.NotFound {
			return &httpError{http.StatusNotFound, "not found"}
		}
		return err
	}
	defer rd.Close()
	h := w.Header()
	h.Set("Content-Type", attrs.ContentType)
	if attrs.Size > 0 {
		h.Set("Content-Length", strconv.FormatInt(attrs.Size, 10))
	}
	h.Set("Cache-Control", "private, max-age=300")
	// Stored files are user content on the API origin: never let them run.
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	switch strings.ToLower(attrs.ContentType) {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		h.Set("Content-Disposition", "inline")
	default:
		h.Set("Content-Disposition", "attachment")
	}
	_, err = io.Copy(w, rd)
	return err
}

// downloadURL returns a fresh transient GET URL for key: presigned for
// direct (S3) storage, an hfy-served signed path otherwise.
func (s *Server) downloadURL(key string) string {
	if !s.fsEnabled() {
		return ""
	}
	if files.Direct(files.Scheme(s.cfg.Filesystem.DSN)) {
		u, err := s.bucket.SignedURL(context.Background(), key, &blob.SignedURLOptions{
			Method: http.MethodGet, Expiry: s.cfg.Filesystem.DownloadTTL.D(),
		})
		if err != nil {
			slog.Error("signing a download URL failed", "err", err)
			return ""
		}
		return u
	}
	exp := time.Now().Add(s.cfg.Filesystem.DownloadTTL.D())
	path := "/v1/files/" + key + "?exp=" + strconv.FormatInt(exp.Unix(), 10) + "&sig=" +
		files.SignGet(s.cfg.Server.Secret, key, exp)
	if s.cfg.Filesystem.PublicURL != "" {
		// A provider that cannot reach hfy (file:// storage on another
		// machine) needs the URL the config vouches for.
		return strings.TrimRight(s.cfg.Filesystem.PublicURL, "/") + path
	}
	return path
}

// fillAttachmentURLs fills transient download URLs on a message.text event's
// file attachments, leaving the stored event (file keys only) untouched. Any
// failure returns the event unchanged.
func (s *Server) fillAttachmentURLs(env types.Event) types.Event {
	if env.Type != types.EventMessageText || !s.fsEnabled() {
		return env
	}
	// Work on the raw JSON so fields this build doesn't know (a newer
	// instance's additions during a rolling deploy) survive the fill.
	var p map[string]any
	if err := json.Unmarshal(env.Data, &p); err != nil {
		return env
	}
	atts, _ := p["attachments"].([]any)
	changed := false
	for _, a := range atts {
		at, _ := a.(map[string]any)
		file, _ := at["file"].(string)
		url, _ := at["url"].(string)
		if file != "" && url == "" {
			if u := s.downloadURL(file); u != "" {
				at["url"], changed = u, true
			}
		}
	}
	if !changed {
		return env
	}
	data, err := json.Marshal(p)
	if err != nil {
		return env
	}
	env.Data = data
	return env
}

// validFileKey rejects keys that could escape their prefix on any driver.
func validFileKey(key string) bool {
	return !strings.Contains(key, "..") && !strings.Contains(key, "//") && !strings.Contains(key, "\\")
}

// verifyFileAttachments checks a message's file attachments against storage:
// they must be slot keys under this conversation's uploads/ prefix, exist,
// and fit the limits. Each accepted slot is copied to a fresh verified key
// under conversations/ (the immutable object events name), which is then
// re-read: between the slot's attributes and the copy a direct-storage client
// could have replaced it. The slots stay writable until their expiry, so
// retention later deletes the empties.
// It also returns per-file facts for the conversation_files index: the
// verified key, the mime, the verified copy's size and the client's name.
// On failure the made copies are deleted and (nil, nil, err) returned.
func (s *Server) verifyFileAttachments(r *http.Request, g *db.GrantRow, atts []types.Attachment) ([]types.Attachment, []svc.PostedFile, error) {
	hasFile := slices.ContainsFunc(atts, func(a types.Attachment) bool { return a.File != "" })
	if !hasFile {
		return atts, nil, nil
	}
	if !s.fsEnabled() {
		return nil, nil, &httpError{http.StatusBadRequest, "file uploads are not configured"}
	}
	limit := 10
	if s.cfg.Attachments != nil && s.cfg.Attachments.MaxCount > 0 {
		limit = s.cfg.Attachments.MaxCount
	}
	fileCount := 0
	for _, at := range atts {
		if at.File != "" {
			fileCount++
		}
	}
	if fileCount > limit {
		return nil, nil, &httpError{http.StatusBadRequest, "too many file attachments"}
	}
	out := slices.Clone(atts)
	slotPrefix := "uploads/" + g.ConversationUUID.String() + "/"
	prefix := "conversations/" + g.ConversationUUID.String() + "/"
	max := int64(s.cfg.Filesystem.MaxBytes)
	var made []string
	var posted []svc.PostedFile
	// verifyAll leaves verified keys in made so any failure can clean up.
	verifyAll := func() error {
		for i, at := range out {
			if at.File == "" {
				continue
			}
			if !strings.HasPrefix(at.File, slotPrefix) || !validFileKey(at.File) {
				return &httpError{http.StatusBadRequest, "attachment file is outside this conversation"}
			}
			if !allowedMime(s.cfg.Filesystem.Types, at.Mime) {
				return &httpError{http.StatusBadRequest, "attachment type is not allowed"}
			}
			attrs, err := s.bucket.Attributes(r.Context(), at.File)
			if err != nil {
				if gcerrors.Code(err) == gcerrors.NotFound {
					return &httpError{http.StatusBadRequest, "attachment not uploaded"}
				}
				return err
			}
			if attrs.Size > max {
				return &httpError{http.StatusBadRequest, "attachment too large"}
			}
			if attrs.Size <= 0 {
				return &httpError{http.StatusBadRequest, "attachment is empty"}
			}
			if attrs.ContentType != "" && !strings.EqualFold(attrs.ContentType, at.Mime) {
				return &httpError{http.StatusBadRequest, "attachment type does not match its content"}
			}
			verified := prefix + newUploadToken() + files.ExtForMIME(at.Mime)
			if err := s.bucket.Copy(r.Context(), verified, at.File, nil); err != nil {
				return err
			}
			made = append(made, verified)
			// Re-read the copy: with direct storage a client could have
			// rewritten its slot between the checks above and the copy.
			copied, err := s.bucket.Attributes(r.Context(), verified)
			if err != nil {
				return err
			}
			if copied.Size > max {
				return &httpError{http.StatusBadRequest, "attachment too large"}
			}
			if copied.Size <= 0 {
				return &httpError{http.StatusBadRequest, "attachment is empty"}
			}
			if copied.ContentType != "" && !strings.EqualFold(copied.ContentType, at.Mime) {
				return &httpError{http.StatusBadRequest, "attachment type does not match its content"}
			}
			out[i].File = verified
			posted = append(posted, svc.PostedFile{Key: verified, Mime: at.Mime, Size: copied.Size, Name: at.Name})
		}
		return nil
	}
	if err := verifyAll(); err != nil {
		for _, k := range made {
			_ = s.bucket.Delete(r.Context(), k)
		}
		return nil, nil, err
	}
	return out, posted, nil
}
