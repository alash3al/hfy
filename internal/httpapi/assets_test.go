package httpapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/alash3al/hfy/internal/config"
)

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":  {Data: []byte("<html>page</html>")},
		"app.js":      {Data: []byte("console.log('app')")},
		"app.css":     {Data: []byte("body{margin:0}")},
		"sub/deep.js": {Data: []byte("var deep=1")},
		"logo.png":    {Data: []byte("\x89PNG....")},
		"README":      {Data: []byte("no extension")},
	}
}

func mustAssets(t *testing.T, fsys fstest.MapFS) *assets {
	t.Helper()
	a, err := newAssets(fsys)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// do serves c with the given request headers (name, value pairs).
func do(c *content, method string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/x", nil)
	for i := 0; i < len(hdr); i += 2 {
		r.Header.Add(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	serveContent(w, r, c, "no-cache")
	return w
}

func TestAssetsRegistryEnumeratesTheTree(t *testing.T) {
	a := mustAssets(t, testFS())
	for name, ctype := range map[string]string{
		"app.js":      "text/javascript; charset=utf-8",
		"app.css":     "text/css; charset=utf-8",
		"sub/deep.js": "text/javascript; charset=utf-8",
		"logo.png":    "image/png",
		"README":      "application/octet-stream",
	} {
		got := a.byName[name]
		if got == nil {
			t.Errorf("%s: not in the registry", name)
			continue
		}
		if got.content.ctype != ctype {
			t.Errorf("%s: content type %q, want %q", name, got.content.ctype, ctype)
		}
	}
	if _, ok := a.byName[pageName]; ok {
		t.Error("the page template is served only by the page route")
	}
	if string(a.template) != "<html>page</html>" {
		t.Errorf("template: %q", a.template)
	}
	// A file added to the tree is picked up with no code change.
	fsys := testFS()
	fsys["layouts/whatsapp.css"] = &fstest.MapFile{Data: []byte(".a{}")}
	if mustAssets(t, fsys).byName["layouts/whatsapp.css"] == nil {
		t.Error("a new file is not picked up")
	}
}

func TestETagFollowsContent(t *testing.T) {
	etag := func(data string) string {
		fsys := testFS()
		fsys["app.js"] = &fstest.MapFile{Data: []byte(data), ModTime: fsys["app.js"].ModTime}
		return mustAssets(t, fsys).byName["app.js"].content.identity.etag
	}
	a, b := etag("one"), etag("two")
	if a == b {
		t.Errorf("different content, same etag %s", a)
	}
	// Same bytes, different size metadata and mtime: same validator.
	fsys := testFS()
	fsys["app.js"] = &fstest.MapFile{Data: []byte("one"), Mode: 0o600}
	if got := mustAssets(t, fsys).byName["app.js"].content.identity.etag; got != a {
		t.Errorf("etag %s depends on more than content, want %s", got, a)
	}
	if !strings.HasPrefix(a, `"`) || !strings.HasSuffix(a, `"`) || strings.HasPrefix(a, "W/") {
		t.Errorf("etag must be a strong quoted tag: %s", a)
	}
}

func TestServeContentBasics(t *testing.T) {
	c := mustAssets(t, testFS()).byName["app.js"].content
	w := do(c, http.MethodGet)
	if w.Code != 200 || w.Body.String() != "console.log('app')" {
		t.Fatalf("%d %q", w.Code, w.Body)
	}
	h := w.Header()
	if h.Get("ETag") != c.identity.etag || h.Get("Cache-Control") != "no-cache" || h.Get("X-Content-Type-Options") != "nosniff" ||
		h.Get("Content-Length") != "18" || h.Get("Content-Type") != "text/javascript; charset=utf-8" {
		t.Errorf("headers: %v", h)
	}
	// HEAD: the same headers as GET, no body.
	hw := do(c, http.MethodHead)
	if hw.Code != 200 || hw.Body.Len() != 0 {
		t.Errorf("HEAD: %d %q", hw.Code, hw.Body)
	}
	if hw.Header().Get("Content-Length") != "18" || hw.Header().Get("ETag") != h.Get("ETag") {
		t.Errorf("HEAD headers: %v", hw.Header())
	}
	// Range is ignored: the whole body, 200.
	rw := do(c, http.MethodGet, "Range", "bytes=0-3")
	if rw.Code != 200 || rw.Body.String() != "console.log('app')" || rw.Header().Get("Content-Range") != "" {
		t.Errorf("Range: %d %q %v", rw.Code, rw.Body, rw.Header())
	}
}

func TestConditionalRequests(t *testing.T) {
	c := mustAssets(t, testFS()).byName["app.js"].content
	tag := c.identity.etag
	for _, tc := range []struct {
		name   string
		header []string
		want   int
	}{
		{"strong", []string{tag}, 304},
		{"weak", []string{"W/" + tag}, 304},
		{"list", []string{`"a", ` + tag + `, "b"`}, 304},
		{"list no space", []string{`"a",` + tag}, 304},
		{"list weak member", []string{`"a", W/` + tag}, 304},
		{"tabs", []string{"\t" + tag + "\t"}, 304},
		{"star", []string{"*"}, 304},
		{"star padded", []string{" * "}, 304},
		{"two header lines", []string{`"a"`, tag}, 304},
		{"mismatch", []string{`"nope"`}, 200},
		{"unquoted", []string{strings.Trim(tag, `"`)}, 200},
		{"lowercase weak prefix", []string{"w/" + tag}, 200},
		{"star inside a list", []string{`"a", *`}, 200},
		{"empty", []string{""}, 200},
		{"only commas", []string{",,,"}, 200},
		{"NUL", []string{"\x00" + tag}, 200},
		{"NUL inside", []string{tag[:5] + "\x00" + tag[5:]}, 200},
		{"truncated", []string{tag[:len(tag)-1]}, 200},
		{"huge list ending in the tag", []string{strings.Repeat(`"x",`, 5000) + tag}, 200},
		{"huge junk", []string{strings.Repeat("A", 1<<20)}, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hdr []string
			for _, v := range tc.header {
				hdr = append(hdr, "If-None-Match", v)
			}
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				w := do(c, method, hdr...)
				if w.Code != tc.want {
					t.Fatalf("%s: %d, want %d", method, w.Code, tc.want)
				}
				if tc.want == 304 {
					if w.Body.Len() != 0 || w.Header().Get("ETag") != tag || w.Header().Get("Cache-Control") != "no-cache" || w.Header().Get("Content-Length") != "" {
						t.Errorf("304 must carry ETag and Cache-Control, no body or length: %v %q", w.Header(), w.Body)
					}
				}
			}
		})
	}
}

func testServer(t *testing.T) http.Handler {
	t.Helper()
	cfg := &config.Config{Server: config.Server{Secret: "s"}}
	cfg.SetDefaults()
	return New(cfg, nil, nil, nil).Handler()
}

func get(h http.Handler, method, target string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	for i := 0; i < len(hdr); i += 2 {
		r.Header.Add(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// Every embedded file is served at /embed/<name> and revalidates; the
// document does too, and it is never served as a raw asset.
func TestEmbedRoutes(t *testing.T) {
	h := testServer(t)
	a, err := embeddedAssets()
	if err != nil {
		t.Fatal(err)
	}
	if len(a.byName) == 0 {
		t.Fatal("no embedded assets")
	}
	for name, as := range a.byName {
		w := get(h, http.MethodGet, "/embed/"+name)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-cache" || w.Header().Get("ETag") != as.content.identity.etag {
			t.Errorf("%s: %d %v", name, w.Code, w.Header())
		}
		if !bytes.Equal(w.Body.Bytes(), as.content.identity.body) {
			t.Errorf("%s: body differs from the embedded file", name)
		}
		if w := get(h, http.MethodGet, "/embed/"+name, "If-None-Match", as.content.identity.etag); w.Code != 304 {
			t.Errorf("%s: conditional: %d", name, w.Code)
		}
	}
	page := get(h, http.MethodGet, "/embed")
	if page.Code != 200 || page.Header().Get("ETag") == "" || page.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("page: %d %v", page.Code, page.Header())
	}
	if w := get(h, http.MethodGet, "/embed", "If-None-Match", page.Header().Get("ETag")); w.Code != 304 || w.Header().Get("Content-Security-Policy") == "" {
		t.Errorf("page conditional: %d %v", w.Code, w.Header())
	}
	if w := get(h, http.MethodHead, "/embed"); w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Length") != page.Header().Get("Content-Length") {
		t.Errorf("page HEAD: %d %v", w.Code, w.Header())
	}
	for _, target := range []string{
		"/embed/index.html", // the template, with its placeholder
		"/embed/", "/embed/ui.js/", "/embed/UI.JS", "/embed/nope.js",
		"/embed/%2e%2e/go.mod", "/embed/..%2fui.js", "/embed/ui.js%00", "/embed/%252e%252e/x", "/embed/ui%2ejs/x",
	} {
		if w := get(h, http.MethodGet, target); w.Code == 200 {
			t.Errorf("%s: served (%d)", target, w.Code)
		}
	}
	if w := get(h, http.MethodPost, "/embed/ui.js"); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", w.Code)
	}
}
