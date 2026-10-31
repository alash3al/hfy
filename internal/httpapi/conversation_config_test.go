package httpapi_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alash3al/hfy/internal/config"
)

// GET /v1/conversation/config is what a client (with no page to embed) reads
// instead of the embed config: 401 like every client route, null uploads
// without a filesystem, the exact limits with one, and never the secret.
func TestConversationConfig(t *testing.T) {
	a := start(t, nil)
	for name, tok := range map[string]string{"none": "", "bad": "hfy_nope", "the secret": secret} {
		if code, _ := a.do("GET", client+"/config", tok, nil); code != http.StatusUnauthorized {
			t.Errorf("%s: %d", name, code)
		}
	}
	token := a.newToken()
	code, out := a.do("GET", client+"/config", token, nil)
	if code != http.StatusOK {
		t.Fatalf("valid token: %d %v", code, out)
	}
	uploads, present := out["uploads"]
	if !present || uploads != nil {
		t.Fatalf("uploads must be null without a filesystem: %v %v", present, uploads)
	}

	stale, _ := a.grant(a.newConversation(), map[string]any{"expires_in": 1})
	time.Sleep(1100 * time.Millisecond)
	// every grant-based route rechecks: a stale grant fails closed with the
	// same 401 as one that never existed
	if code, out := a.do("GET", client+"/config", stale, nil); code != http.StatusUnauthorized || out["error"] != "invalid or expired token" {
		t.Fatalf("stale token: %d %v", code, out)
	}

	code, out = a.do("POST", client+"/config", token, nil)
	if code != http.StatusMethodNotAllowed || out["error"] != "method not allowed" {
		t.Fatalf("wrong method: %d %v", code, out)
	}
}

// With a filesystem the answer is the embed page's uploads config, exactly.
func TestConversationConfigUploads(t *testing.T) {
	a := startFiles(t, config.Filesystem{DSN: "mem://", Types: []string{"text/plain", "image/png"}, MaxBytes: 4096})
	token, _ := a.granted()
	code, out := a.do("GET", client+"/config", token, nil)
	if code != http.StatusOK {
		t.Fatalf("config: %d %v", code, out)
	}
	up := out["uploads"].(map[string]any)
	if got, _ := up["types"].([]any); len(got) != 2 || got[0] != "text/plain" || got[1] != "image/png" {
		t.Fatalf("types: %v", up)
	}
	if up["max_bytes"].(float64) != 4096 {
		t.Fatalf("max_bytes: %v", up)
	}
}

// The config route answers browsers like the other client routes: its
// preflight is 204 with the CORS headers, errors included.
func TestConversationConfigCORS(t *testing.T) {
	const site = "https://shop.example.com"
	a := start(t, func(c *config.Config) { c.Server.CORSOrigins = []string{site} })
	token := a.newToken()
	r := a.browser(http.MethodOptions, client+"/config", site, "")
	if r.code != http.StatusNoContent || r.Get("Access-Control-Allow-Origin") != site ||
		!strings.Contains(r.Get("Access-Control-Allow-Methods"), "GET") ||
		!strings.Contains(strings.ToLower(r.Get("Access-Control-Allow-Headers")), "authorization") ||
		r.Get("Access-Control-Max-Age") != "3600" {
		t.Fatalf("preflight: %d %v", r.code, r.Header)
	}
	if r := a.browser(http.MethodGet, client+"/config", site, "Bearer "+token); r.code != http.StatusOK ||
		r.Get("Access-Control-Allow-Origin") != site {
		t.Fatalf("GET: %d %v", r.code, r.Header)
	}
	if v := a.browser(http.MethodGet, client+"/config", "https://evil.example.com", "Bearer "+token).Get("Access-Control-Allow-Origin"); v != "" {
		t.Fatalf("answered an unlisted origin: %q", v)
	}
}
