package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/db/dbtest"
	"github.com/alash3al/hfy/internal/files"
	"github.com/alash3al/hfy/internal/httpapi"
	"github.com/alash3al/hfy/internal/svc"

	"gocloud.dev/blob"
)

// startFiles is the upload harness: the same server start() builds, minus the
// provider runtime, with a mem:// bucket enabled — slots, proxied PUTs,
// serving and history need only storage, grants and the store.
func startFiles(t *testing.T, fs config.Filesystem) *fsapi {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	cfg := &config.Config{
		Server:     config.Server{Secret: secret},
		Providers:  map[string]config.Provider{"fake": {Type: "mock", Mock: config.MockProvider{Reply: "hi"}}},
		Agent:      config.Agent{Model: "fake/m", System: "You help.", Settings: config.Settings{ContextWindow: 1000, Vision: true}},
		Filesystem: &fs,
	}
	cfg.SetDefaults()
	store := dbtest.New(t, nil)
	srv := httpapi.New(cfg, store, svc.New(store, cfg), nil)
	bucket, err := files.Open(ctx, fs.DSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bucket.Close() })
	srv.EnableFilesystem(bucket)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &fsapi{t: t, url: ts.URL, store: store}
}

type fsapi struct {
	t     *testing.T
	url   string
	store *db.Store
}

func (a *fsapi) do(method, path, bearer string, body any) (int, map[string]any) {
	a.t.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req, err := http.NewRequest(method, a.url+path, strings.NewReader(string(b)))
	if err != nil {
		a.t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Content-Type", "application/json")
	return a.send(req)
}

// send runs one request and decodes any JSON body (empty answers — a 204
// PUT, a file GET — decode the same).
func (a *fsapi) send(req *http.Request) (int, map[string]any) {
	a.t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// granted is the common start: a conversation and one grant to it, plus a
// second conversation the first token is not granted to.
func (a *fsapi) granted() (token, conv string) {
	a.t.Helper()
	code, out := a.do("POST", "/v1/conversations", secret, nil)
	if code != 201 {
		a.t.Fatalf("create conversation: %d %v", code, out)
	}
	conv = out["id"].(string)
	code, out = a.do("POST", "/v1/grants", secret, map[string]any{"conversation": conv, "actor": "u1"})
	if code != 201 {
		a.t.Fatalf("create grant: %d %v", code, out)
	}
	return out["token"].(string), conv
}

// put PUTs bytes to a proxied slot (the slot route does not speak JSON).
func (a *fsapi) put(token, path, contentType, body string) (int, map[string]any) {
	a.t.Helper()
	req, err := http.NewRequest(http.MethodPut, a.url+path, strings.NewReader(body))
	if err != nil {
		a.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return a.send(req)
}

// slotPath returns the PUT path for a slot key, signed for mime until exp.
func slotPath(key, mime string, exp time.Time) string {
	return "/v1/conversation/uploads/" + key + "?exp=" + strconv.FormatInt(exp.Unix(), 10) +
		"&sig=" + files.SignPut(secret, key, mime, exp)
}

func TestUploadsDisabled(t *testing.T) {
	a := start(t, nil)
	token := a.newToken()

	code, out := a.do("POST", client+"/uploads", token, map[string]any{"mime": "image/png", "size": 10})
	if code != http.StatusNotFound || out["error"] != "file uploads are not configured" {
		t.Fatalf("upload slot without filesystem: %d %v", code, out)
	}
	code, out = a.do("POST", client+"/messages", token, map[string]any{
		"text":        "hi",
		"attachments": []map[string]any{{"file": "uploads/x/a.txt", "mime": "text/plain"}},
	})
	if code != http.StatusBadRequest || out["error"] != "file uploads are not configured" {
		t.Fatalf("message with file attachment without filesystem: %d %v", code, out)
	}
}

// A slot is a fresh key under the conversation's uploads/ prefix, size-bounded
// by filesystem.max_bytes, with an hfy-served PUT URL carrying exp and sig.
func TestRequestUploadSlots(t *testing.T) {
	a := startFiles(t, config.Filesystem{DSN: "mem://", Types: []string{"text/plain", "image/png"}, MaxBytes: 4096})
	token, conv := a.granted()

	// the embed page tells its composer what it may upload
	if resp, err := http.Get(a.url + "/embed"); err != nil {
		t.Fatal(err)
	} else {
		var page strings.Builder
		_, _ = io.Copy(&page, resp.Body)
		resp.Body.Close()
		if !strings.Contains(page.String(), `"uploads":{"types":["text/plain","image/png"],"max_bytes":4096}`) {
			t.Fatalf("embed page uploads config missing: %s", page.String())
		}
	}

	for name, c := range map[string]struct {
		body map[string]any
		want int
	}{
		"disallowed mime": {map[string]any{"mime": "image/gif", "size": 10}, 400},
		"missing size":    {map[string]any{"mime": "text/plain"}, 400},
		"zero size":       {map[string]any{"mime": "text/plain", "size": 0}, 400},
		"negative size":   {map[string]any{"mime": "text/plain", "size": -1}, 400},
		"oversized size":  {map[string]any{"mime": "text/plain", "size": 4097}, 400},
		"good":            {map[string]any{"mime": "text/plain", "size": 10}, 201},
	} {
		code, out := a.do("POST", client+"/uploads", token, c.body)
		if code != c.want {
			t.Errorf("%s: got %d %v, want %d", name, code, out, c.want)
		}
		if code != 201 {
			continue
		}
		key := out["file"].(string)
		if !strings.HasPrefix(key, "uploads/"+conv+"/") {
			t.Errorf("slot key %q is not under uploads/%s/", key, conv)
		}
		if out["direct"] != false {
			t.Errorf("proxied slot must not claim direct: %v", out)
		}
		u, err := url.Parse(out["upload_url"].(string))
		if err != nil || u.Path != "/v1/conversation/uploads/"+key ||
			u.Query().Get("exp") == "" || u.Query().Get("sig") == "" {
			t.Errorf("upload_url lacks exp&sig or does not name the key: %v", out["upload_url"])
		}
	}
}

// A proxied slot PUT: its signature (and expiry, prefix, escaping and mime)
// is checked, the size is bounded, the slot is immutable once written, and an
// oversized upload commits nothing.
func TestPutUploadRules(t *testing.T) {
	a := startFiles(t, config.Filesystem{DSN: "mem://", Types: []string{"text/plain", "image/png"}, MaxBytes: 4096})
	token, conv := a.granted()
	_, other := a.granted() // a conversation this token is not granted to
	exp := time.Now().Add(time.Minute)
	for name, c := range map[string]struct{ path, contentType, body string }{
		"bad sig":       {slotPath("uploads/"+conv+"/a.txt", "text/plain", exp) + "more", "text/plain", "hi"},
		"expired":       {slotPath("uploads/"+conv+"/b.txt", "text/plain", exp.Add(-2*time.Minute)), "text/plain", "hi"},
		"other conv":    {slotPath("uploads/"+other+"/c.txt", "text/plain", exp), "text/plain", "hi"},
		"other prefix":  {slotPath("secrets/all.txt", "text/plain", exp), "text/plain", "hi"},
		"key with ..":   {slotPath("uploads/"+conv+"/../.."+"/c.txt", "text/plain", exp), "text/plain", "hi"},
		"mime mismatch": {slotPath("uploads/"+conv+"/d.txt", "text/plain", exp), "image/png", "hi"},
		"oversize":      {slotPath("uploads/"+conv+"/e.txt", "text/plain", exp), "text/plain", strings.Repeat("x", 4097)},
	} {
		code, out := a.put(token, c.path, c.contentType, c.body)
		want := map[string]int{"bad sig": 403, "expired": 403, "other conv": 403, "other prefix": 403,
			"key with ..": 403, "mime mismatch": 403, "oversize": 413}
		if code != want[name] {
			t.Errorf("%s: got %d %v, want %d", name, code, out, want[name])
		}
	}
	code, out := a.put(token, slotPath("uploads/"+conv+"/g.txt", "text/plain", exp), "text/plain", "hi")
	if code != 204 {
		t.Fatalf("good PUT: %d %v", code, out)
	}
	// a second PUT of the same key is a conflict: the slot is immutable
	if code, _ := a.put(token, slotPath("uploads/"+conv+"/g.txt", "text/plain", exp), "text/plain", "hi"); code != 409 {
		t.Fatalf("second PUT: %d", code)
	}
	// the aborted oversized write committed nothing: that slot is still fresh
	if code, _ := a.put(token, slotPath("uploads/"+conv+"/e.txt", "text/plain", exp), "text/plain", "ok"); code != 204 {
		t.Fatalf("oversized slot still writable after the abort: %d", code)
	}
}

// A message names its slot; the stored event names a fresh verified copy
// under conversations/, and only the served history carries a URL.
func TestPostMessageWithFile(t *testing.T) {
	a := startFiles(t, config.Filesystem{DSN: "mem://", Types: []string{"text/plain", "image/png"}, MaxBytes: 4096})
	token, conv := a.granted()
	_, other := a.granted()
	slot := "uploads/" + conv + "/n.txt"
	code, out := a.put(token, slotPath(slot, "text/plain", time.Now().Add(time.Minute)), "text/plain", "hi")
	if code != 204 {
		t.Fatalf("put slot: %d %v", code, out)
	}

	for name, c := range map[string]struct {
		att  map[string]any
		want int
	}{
		"other conversation prefix": {map[string]any{"file": "uploads/" + other + "/x.txt", "mime": "text/plain"}, 400},
		"not uploaded":              {map[string]any{"file": slot + "missing.txt", "mime": "text/plain"}, 400},
		"declared mime != stored":   {map[string]any{"file": slot, "mime": "image/png"}, 400},
	} {
		code, out = a.do("POST", client+"/messages", token,
			map[string]any{"text": "hi", "attachments": []map[string]any{c.att}})
		if code != c.want {
			t.Errorf("%s: got %d %v, want %d", name, code, out, c.want)
		}
	}

	code, out = a.do("POST", client+"/messages", token,
		map[string]any{"text": "hi", "attachments": []map[string]any{{"file": slot, "mime": "text/plain"}}})
	if code != 202 {
		t.Fatalf("post: %d %v", code, out)
	}
	code, hist := a.do("GET", client+"/history", token, nil)
	if code != 200 {
		t.Fatalf("history: %d", code)
	}
	atts := servedAttachments(t, hist)
	if len(atts) != 1 {
		t.Fatalf("message.text attachments: %v", atts)
	}
	file, _ := atts[0]["file"].(string)
	if !strings.HasPrefix(file, "conversations/"+conv+"/") || file == slot {
		t.Fatalf("the event must name a fresh conversations/ copy, got %q", file)
	}
	u, _ := atts[0]["url"].(string)
	if !strings.HasPrefix(u, "/v1/files/") || !strings.Contains(u, "exp=") || !strings.Contains(u, "sig=") {
		t.Fatalf("served history lacks a signed url: %q", u)
	}
	// the stored event names the key only, never a URL
	var stored string
	if err := a.store.Pool.QueryRow(t.Context(),
		"SELECT data FROM conversation_events WHERE type = 'message.text'").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, `"url"`) || !strings.Contains(stored, strconv.Quote(file)) {
		t.Fatalf("stored event must name %q and carry no url: %s", file, stored)
	}
}

// A posted file is indexed in conversation_files: exactly one row, naming the
// verified copy (never the slot), with the stored size and the original
// name, tied to the message's event, the conversation and the uploader.
func TestPostMessageFileIndex(t *testing.T) {
	a := startFiles(t, config.Filesystem{DSN: "mem://", Types: []string{"text/plain"}, MaxBytes: 4096})
	token, conv := a.granted()
	slot := "uploads/" + conv + "/n.txt"
	if code, out := a.put(token, slotPath(slot, "text/plain", time.Now().Add(time.Minute)), "text/plain", "hi"); code != 204 {
		t.Fatalf("put slot: %d %v", code, out)
	}
	code, out := a.do("POST", client+"/messages", token, map[string]any{
		"text":        "hi",
		"attachments": []map[string]any{{"file": slot, "mime": "text/plain", "name": "note.txt"}},
	})
	if code != 202 {
		t.Fatalf("post: %d %v", code, out)
	}
	var files int
	if err := a.store.Pool.QueryRow(t.Context(), "SELECT count(*) FROM conversation_files").Scan(&files); err != nil || files != 1 {
		t.Fatalf("exactly one indexed row: %d (%v)", files, err)
	}
	var key, mime, name string
	var size int64
	var verified, convOK, eventOK, actorOK bool
	err := a.store.Pool.QueryRow(t.Context(), `
		SELECT f.key, f.mime, f.size, f.name, f.key = $1,
		       c.uuid::text = $2,
		       EXISTS (SELECT 1 FROM conversation_events e WHERE e.id = f.event_id AND e.type = 'message.text'),
		       EXISTS (SELECT 1 FROM actors u WHERE u.id = f.actor_id AND u.type = 'external' AND u.ref = 'u1')
		FROM conversation_files f JOIN conversations c ON c.id = f.conversation_id`,
		slot, conv).Scan(&key, &mime, &size, &name, &verified, &convOK, &eventOK, &actorOK)
	if err != nil {
		t.Fatal(err)
	}
	if verified || !convOK || !eventOK || !actorOK {
		t.Fatalf("the row must name the verified copy of the message, not the slot: slot=%v conv=%v event=%v actor=%v",
			verified, convOK, eventOK, actorOK)
	}
	if !strings.HasPrefix(key, "conversations/"+conv+"/") {
		t.Fatalf("indexed key %q is not a conversations/ copy", key)
	}
	if mime != "text/plain" || size != 2 || name != "note.txt" {
		t.Fatalf("stored facts: %s %d %q", mime, size, name)
	}
	// the stored event carries the name too, and never the slot key
	var raw []byte
	if err := a.store.Pool.QueryRow(t.Context(),
		"SELECT data FROM conversation_events WHERE type = 'message.text'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var data struct {
		Attachments []map[string]any `json:"attachments"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Attachments) != 1 || data.Attachments[0]["name"] != "note.txt" || strings.Contains(string(raw), slot) {
		t.Fatalf("stored event: %s", raw)
	}
}

// servedAttachments returns the message.text event's attachments of a history
// page, or fails.
func servedAttachments(t *testing.T, hist map[string]any) []map[string]any {
	t.Helper()
	for _, tu := range hist["turns"].([]any) {
		for _, e := range tu.(map[string]any)["events"].([]any) {
			ev := e.(map[string]any)
			if ev["type"] != "message.text" {
				continue
			}
			data, _ := ev["data"].(map[string]any)
			atts, _ := data["attachments"].([]any)
			out := make([]map[string]any, len(atts))
			for i, at := range atts {
				out[i] = at.(map[string]any)
			}
			return out
		}
	}
	t.Fatal("no message.text event in history")
	return nil
}

// filesystem.public_url makes download URLs absolute for providers that
// cannot reach hfy itself (file:// storage).
func TestDownloadURLPublicPrefix(t *testing.T) {
	a := startFiles(t, config.Filesystem{
		DSN: "mem://", Types: []string{"text/plain"}, MaxBytes: 4096, PublicURL: "https://pub.example.com/",
	})
	token, conv := a.granted()
	slot := "uploads/" + conv + "/n.txt"
	if code, out := a.put(token, slotPath(slot, "text/plain", time.Now().Add(time.Minute)), "text/plain", "hi"); code != 204 {
		t.Fatalf("put slot: %d %v", code, out)
	}
	if code, _ := a.do("POST", client+"/messages", token,
		map[string]any{"text": "hi", "attachments": []map[string]any{{"file": slot, "mime": "text/plain"}}}); code != 202 {
		t.Fatal(code)
	}
	_, hist := a.do("GET", client+"/history", token, nil)
	atts := servedAttachments(t, hist)
	if len(atts) != 1 {
		t.Fatalf("attachments: %v", atts)
	}
	if u := atts[0]["url"].(string); !strings.HasPrefix(u, "https://pub.example.com/v1/files/") {
		t.Fatalf("download url must be absolute under public_url: %q", u)
	}
}

// An hfy-served file needs its signed URL: without it (or with it expired or
// tampered) a 403; with it the bytes plus the sandboxing headers. A PUT
// signature must not work as a GET signature: the domains differ.
func TestServeFileSignedURL(t *testing.T) {
	cfg := &config.Config{
		Server:     config.Server{Secret: secret},
		Filesystem: &config.Filesystem{DSN: "mem://", Types: []string{"image/png", "text/plain"}},
	}
	cfg.SetDefaults()
	ctx := context.Background()
	bucket, err := files.Open(ctx, "mem://")
	if err != nil {
		t.Fatal(err)
	}
	defer bucket.Close()
	srv := httpapi.New(cfg, nil, nil, nil)
	srv.EnableFilesystem(bucket)
	keys := map[string]string{
		"conversations/c1/a.png": "pngdata",
		"conversations/c1/b.txt": "noted",
	}
	for key, data := range keys {
		ct := "text/plain"
		if strings.HasSuffix(key, ".png") {
			ct = "image/png"
		}
		if err := bucket.WriteAll(ctx, key, []byte(data), &blob.WriterOptions{ContentType: ct}); err != nil {
			t.Fatal(err)
		}
	}
	exp := time.Now().Add(time.Minute)
	get := func(key, sig string, e time.Time) (int, *httptest.ResponseRecorder) {
		rec := httptest.NewRecorder()
		p := "/v1/files/" + key + "?exp=" + strconv.FormatInt(e.Unix(), 10) + "&sig=" + url.QueryEscape(sig)
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		return rec.Code, rec
	}
	signed := func(key string, e time.Time) string { return files.SignGet(secret, key, e) }

	if code, _ := get("conversations/c1/a.png", signed("conversations/c1/a.png", exp), exp.Add(-time.Minute)); code != http.StatusForbidden {
		t.Fatalf("expired: %d", code)
	}
	if code, _ := get("conversations/c1/a.png", signed("conversations/c1/other.png", exp), exp); code != http.StatusForbidden {
		t.Fatalf("tampered: %d", code)
	}
	// a PUT signature is the wrong domain for a GET, whatever the key
	if code, _ := get("conversations/c1/a.png", files.SignPut(secret, "conversations/c1/a.png", "image/png", exp), exp); code != http.StatusForbidden {
		t.Fatalf("PUT sig as GET: %d", code)
	}
	code, rec := get("conversations/c1/a.png", signed("conversations/c1/a.png", exp), exp)
	if code != http.StatusOK || rec.Body.String() != "pngdata" || rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("signed file: %d %q %v", code, rec.Body.String(), rec.Header())
	}
	h := rec.Header()
	if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Content-Security-Policy") != "sandbox; default-src 'none'" {
		t.Fatalf("sandboxing headers: %v", h)
	}
	if h.Get("Content-Disposition") != "inline" {
		t.Fatalf("an image must be inline: %v", h)
	}
	if code, rec = get("conversations/c1/b.txt", signed("conversations/c1/b.txt", exp), exp); code != http.StatusOK ||
		rec.Body.String() != "noted" || rec.Header().Get("Content-Disposition") != "attachment" {
		t.Fatalf("text file: %d %q %v", code, rec.Body.String(), rec.Header())
	}
	if code, _ = get("conversations/c1/missing.png", signed("conversations/c1/missing.png", exp), exp); code != http.StatusNotFound {
		t.Fatalf("missing file: %d", code)
	}
}
