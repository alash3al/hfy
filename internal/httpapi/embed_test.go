package httpapi_test

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/httpapi"
)

func TestEmbed(t *testing.T) {
	cfg := &config.Config{
		Server: config.Server{Secret: "s", CORSOrigins: []string{"https://shop.example.com"}},
		Embed:  config.Embed{Accent: "#123abc"},
	}
	cfg.SetDefaults()
	srv := httpapi.New(cfg, nil, nil, nil)

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	rec := get("/embed")
	if rec.Code != http.StatusOK {
		t.Fatalf("/embed: %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("content type: %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"#123abc", "https://shop.example.com", `"parents":["https://shop.example.com"]`, `"uploads":null`} {
		if !strings.Contains(body, want) {
			t.Errorf("/embed body missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "__HFY_CONFIG_JSON__") {
		t.Errorf("placeholder not replaced: %s", body)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors https://shop.example.com") {
		t.Errorf("CSP frame-ancestors: %q", csp)
	}
	// The CSP sha256 must be the hash of the exact inline script served,
	// or the browser blocks the config script and the page stays dead.
	_, script, _ := strings.Cut(body, "<script>window.__HFY_EMBED__=")
	script, _, _ = strings.Cut(script, "</script>")
	sum := sha256.Sum256([]byte("window.__HFY_EMBED__=" + script))
	if want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"; !strings.Contains(csp, want) {
		t.Errorf("CSP lacks inline script hash %s: %q", want, csp)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("headers: %v", rec.Header())
	}

	rec = get("/embed/client.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("/embed/client.js: %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("client.js content type: %q", ct)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("client.js headers: %v", rec.Header())
	}
}
