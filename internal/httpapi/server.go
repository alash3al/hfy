// Package httpapi is the HTTP surface: net/http routes, the two auth checks
// (the server secret, a grant's token) and SSE. One file per action, each holding its own DTOs.
package httpapi

import (
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/metrics"
	"github.com/alash3al/hfy/internal/svc"

	"gocloud.dev/blob"
)

type Server struct {
	cfg     *config.Config
	store   *db.Store
	svc     *svc.Service
	hub     *Hub
	bucket  *blob.Bucket // filesystem storage; nil = uploads disabled
	handler http.Handler
}

// EnableFilesystem turns on filesystem uploads backed by b.
func (s *Server) EnableFilesystem(b *blob.Bucket) { s.bucket = b }

func New(cfg *config.Config, store *db.Store, s *svc.Service, hub *Hub) *Server {
	srv := &Server{cfg: cfg, store: store, svc: s, hub: hub}
	mux := http.NewServeMux()
	// route serves pattern and answers its path's other methods: OPTIONS (a
	// client route's CORS preflight) with 204, the rest with 405.
	route := func(pattern string, h http.Handler) {
		method, path, _ := strings.Cut(pattern, " ")
		mux.Handle(pattern, h)
		mux.Handle(path, handler(func(w http.ResponseWriter, r *http.Request) error {
			w.Header().Set("Allow", method+", OPTIONS")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return nil
			}
			return &httpError{http.StatusMethodNotAllowed, "method not allowed"}
		}))
	}

	// Operational endpoints are unversioned; the API is under /v1.
	route("GET /healthz", http.HandlerFunc(Healthz))
	route("GET /readyz", Readyz(store))

	route("GET /embed", http.HandlerFunc(srv.serveEmbed))
	route("GET /embed/client.js", serveEmbedAsset("client.js", "text/javascript"))
	route("GET /embed/ui.js", serveEmbedAsset("ui.js", "text/javascript"))
	route("GET /embed/ui.css", serveEmbedAsset("ui.css", "text/css"))
	route("GET /embed/alpine.min.js", serveEmbedAsset("alpine.min.js", "text/javascript"))

	route("POST /v1/conversations", srv.requireSecret(srv.createConversation))
	route("POST /v1/grants", srv.requireSecret(srv.createGrant))
	route("DELETE /v1/grants/{id}", srv.requireSecret(srv.revokeGrant))

	// A client's routes name no conversation: its grant holds it. Only the
	// stream takes the token as ?token=, since EventSource cannot set headers.
	route("POST /v1/conversation/messages", srv.withGrant(false, srv.postMessage))
	route("GET /v1/conversation/events", srv.withGrant(true, srv.streamEvents))
	route("GET /v1/conversation/history", srv.withGrant(false, srv.history))
	route("POST /v1/conversation/approvals", srv.withGrant(false, srv.approve))
	route("POST /v1/conversation/stop", srv.withGrant(false, srv.stop))
	route("POST /v1/conversation/uploads", srv.withGrant(false, srv.requestUpload))
	route("PUT /v1/conversation/uploads/{key...}", srv.withGrant(false, srv.putUpload))
	// No grant: a transient signed URL (see uploads.go) is the credential.
	route("GET /v1/files/{key...}", handler(srv.serveFile))

	mux.Handle("/", handler(func(http.ResponseWriter, *http.Request) error { // the mux's own 404 is plain text
		return &httpError{http.StatusNotFound, "not found"}
	}))
	// MaxBytesHandler copies the request, so it stays outside observe, which
	// reads the pattern the mux sets on the request it was handed. Proxied
	// uploads bound themselves by the filesystem limit instead.
	inner := observe(srv.cors(mux))
	limited := http.MaxBytesHandler(inner, maxBody)
	srv.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v1/conversation/uploads/") {
			inner.ServeHTTP(w, r)
			return
		}
		limited.ServeHTTP(w, r)
	})
	return srv
}

func (s *Server) Handler() http.Handler { return s.handler }

// Healthz is liveness: the process serves requests. It never checks
// dependencies, so a database outage does not restart every instance.
func Healthz(w http.ResponseWriter, _ *http.Request) {
	writeStatus(w, http.StatusOK, "ok")
}

// Readyz is readiness: this instance can reach Postgres.
func Readyz(store *db.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := store.Ping(ctx); err != nil {
			writeStatus(w, http.StatusServiceUnavailable, "database unreachable")
			return
		}
		writeStatus(w, http.StatusOK, "ok")
	}
}

func writeStatus(w http.ResponseWriter, code int, status string) {
	_ = writeJSON(w, code, map[string]string{"status": status}) // the client is gone; nothing to do
}

func writeJSON(w http.ResponseWriter, code int, v any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	return json.NewEncoder(w).Encode(v)
}

// observe records request counts and latency by route pattern (never the raw
// path, which would be one series per conversation).
func observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		route := r.Pattern // set by the mux on this same request
		if _, path, ok := strings.Cut(route, " "); ok {
			route = path
		}
		if route == "" || route == "/" { // "/" is the 404 for everything else
			route = "unmatched"
		}
		metrics.HTTPRequests.WithLabelValues(route, r.Method, strconv.Itoa(cmp.Or(sw.code, http.StatusOK))).Inc()
		if !strings.HasSuffix(route, "/events") { // streams last minutes; not latency
			metrics.Since(metrics.HTTPDuration.WithLabelValues(route, r.Method), start)
		}
	})
}

// statusWriter records the status a response started with (0: not yet).
// Unwrap lets http.ResponseController (the stream's flushes and write
// deadlines) reach the connection.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// maxBody bounds every request body (a message with its attachment
// references is the largest), so no client makes the server allocate freely.
const maxBody = 2 << 20

// cors lets pages on the configured origins call the client routes, errors
// included; the backend routes never answer a browser. Tokens travel in a
// header, never in cookies, so no credentials mode; preflights are cached an hour.
func (s *Server) cors(next http.Handler) http.Handler {
	origins := s.cfg.Server.CORSOrigins
	if len(origins) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/conversation/") {
			h := w.Header()
			h.Add("Vary", "Origin")
			origin := r.Header.Get("Origin")
			if origin != "" && origins[0] == "*" {
				origin = "*"
			}
			if origin != "" && slices.Contains(origins, origin) {
				h.Set("Access-Control-Allow-Origin", origin)
				if r.Method == http.MethodOptions {
					h.Set("Access-Control-Allow-Methods", "GET,POST,PUT")
					h.Set("Access-Control-Allow-Headers", "Authorization,Content-Type,Last-Event-ID")
					h.Set("Access-Control-Max-Age", "3600")
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// bindJSON decodes a request body as JSON whatever its Content-Type says:
// the API speaks only JSON, and `curl -d` labels it a form. An empty body is
// an empty object.
func bindJSON(r *http.Request, v any) error {
	err := json.NewDecoder(r.Body).Decode(v)
	if _, tooBig := errors.AsType[*http.MaxBytesError](err); tooBig {
		return &httpError{http.StatusRequestEntityTooLarge, "body over 2 MiB"}
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return &httpError{http.StatusBadRequest, "invalid json body"}
	}
	return nil
}

// handler is an endpoint. The error it returns is answered in JSON, unless
// the response has started (a stream).
type handler func(w http.ResponseWriter, r *http.Request) error

func (h handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sw := &statusWriter{ResponseWriter: w}
	if err := h(sw, r); err != nil && sw.code == 0 {
		writeError(sw, r, err)
	}
}

// httpError is an answer: its status and what the client reads.
type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

type errorBody struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	code, msg := http.StatusInternalServerError, "internal error"
	he, isHTTP := errors.AsType[*httpError](err)
	bad, isBad := errors.AsType[*svc.BadRequestError](err)
	switch {
	case isHTTP:
		code, msg = he.code, he.msg
	case isBad:
		code, msg = http.StatusBadRequest, bad.Msg
	case errors.Is(err, svc.ErrNoPendingApproval):
		code, msg = http.StatusConflict, svc.ErrNoPendingApproval.Error()
	case errors.Is(err, svc.ErrNotTurnOwner):
		code, msg = http.StatusForbidden, svc.ErrNotTurnOwner.Error()
	case db.IsNotFound(err):
		code, msg = http.StatusNotFound, "not found"
	default:
		slog.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	}
	_ = writeJSON(w, code, errorBody{Error: msg}) // the client is gone; nothing to do
}

func bearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[len("Bearer "):])
	}
	return ""
}

// requireSecret guards the server-to-server endpoints. Hashing first makes the
// comparison constant-time regardless of the secret's length.
func (s *Server) requireSecret(next handler) handler {
	want := sha256.Sum256([]byte(s.cfg.Server.Secret))
	return func(w http.ResponseWriter, r *http.Request) error {
		got := sha256.Sum256([]byte(bearer(r)))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			return &httpError{http.StatusUnauthorized, "invalid server secret"}
		}
		return next(w, r)
	}
}

// withGrant resolves the request's token to its grant (who acts, in which
// conversation, as which agent, with which vars) and hands it to h. A token
// that is unknown, expired or revoked gets one answer, so a client learns
// nothing about which.
func (s *Server) withGrant(queryToken bool, h func(w http.ResponseWriter, r *http.Request, g *db.GrantRow) error) handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		token := bearer(r)
		if token == "" && queryToken {
			token = r.URL.Query().Get("token")
		}
		if token == "" {
			return &httpError{http.StatusUnauthorized, "missing token"}
		}
		g, err := s.store.GrantByToken(r.Context(), hashToken(token))
		if db.IsNotFound(err) {
			return &httpError{http.StatusUnauthorized, "invalid or expired token"}
		}
		if err != nil {
			return err
		}
		if _, ok := s.cfg.AgentNamed(g.Agent); !ok {
			slog.Error("a grant names an agent the config no longer has", "agent", g.Agent, "grant", g.ID)
			return &httpError{http.StatusForbidden, "forbidden"} // fail closed
		}
		if err := h(w, r, &g); err != nil {
			return fmt.Errorf("conversation %s: %w", g.ConversationUUID, err) // client URLs name no conversation; logs should
		}
		return nil
	}
}
