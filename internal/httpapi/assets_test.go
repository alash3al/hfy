package httpapi

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/andybalholm/brotli"

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
	a, err := newAssets(fsys, encoders)
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

// fakeEncoders shrink anything to the encoding's name: negotiation is tested
// without depending on a real compressor. calls counts compressions.
func fakeEncoders(calls *int) []encoding {
	fake := func(name string) encoding {
		return encoding{name, func([]byte) ([]byte, error) { *calls++; return []byte(name), nil }}
	}
	return []encoding{fake("br"), fake("gzip")}
}

func negotiated(t *testing.T) *content {
	t.Helper()
	var calls int
	c, err := newContent("text/plain", []byte("a body long enough to shrink"), true, fakeEncoders(&calls))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAcceptEncoding(t *testing.T) {
	const none = "406"
	for _, tc := range []struct {
		header []string // one header line each; nil = no header
		want   string   // "" is identity
	}{
		{nil, ""},
		{[]string{""}, ""},
		{[]string{"identity"}, ""},
		{[]string{"gzip"}, "gzip"},
		{[]string{"br"}, "br"},
		{[]string{"GZIP"}, "gzip"},
		{[]string{"br, gzip"}, "br"},
		{[]string{"gzip, br"}, "br"},
		{[]string{"deflate, gzip;q=0.8"}, "gzip"},
		{[]string{"gzip;q=1, br;q=0.5"}, "gzip"},
		{[]string{"br;q=0.9, gzip;q=0.9"}, "br"},
		{[]string{"gzip;q=0.5, identity"}, ""},
		{[]string{"br;q=0"}, ""},
		{[]string{"br;q=0, gzip"}, "gzip"},
		{[]string{"br;q=0.0, gzip;q=0.000"}, ""},
		{[]string{"gzip;Q=0"}, ""},
		{[]string{"gzip ; q = 0"}, ""},
		{[]string{"gzip;q=0, br;q=0, *;q=0.1"}, ""},
		{[]string{"*"}, "br"},
		{[]string{"*;q=0"}, none},
		{[]string{"*;q=0, gzip"}, "gzip"},
		{[]string{"*;q=0, identity"}, ""},
		{[]string{"br;q=0, *"}, "gzip"},
		{[]string{"identity;q=0"}, none},
		{[]string{"identity;q=0, gzip"}, "gzip"},
		{[]string{"identity;q=0, gzip;q=0.001"}, "gzip"},
		{[]string{"identity;q=0, br;q=0, gzip;q=0"}, none},
		{[]string{"compress"}, ""},
		{[]string{"br;q=0, br"}, ""},
		{[]string{"br, br;q=0"}, "br"},
		{[]string{"br", "gzip"}, "br"},
		{[]string{"gzip", "br;q=0"}, "gzip"},
		{[]string{"zzz!!;;;,,,"}, ""},
		{[]string{",,,"}, ""},
		{[]string{";q=1"}, ""},
		{[]string{"gzip;q=abc"}, ""},
		{[]string{"gzip;q="}, ""},
		{[]string{"gzip;q=2"}, ""},
		{[]string{"gzip;q=-1"}, ""},
		{[]string{"gzip;q=1.0001"}, ""},
		{[]string{"gzip;q=1.5"}, ""},
		{[]string{"gzip;q=0x1"}, ""},
		{[]string{"gzip\x00"}, ""},
		{[]string{"gzip;q=1;extension=1"}, "gzip"},
		{[]string{strings.Repeat("a,", 1<<19) + "br"}, ""},
		{[]string{strings.Repeat("gzip,", 2000)}, ""},
		{append(slices.Repeat([]string{"x"}, 5000), "br"), ""},
	} {
		name := strings.Join(tc.header, "|")
		if len(name) > 40 {
			name = name[:40] + "..."
		}
		t.Run(name, func(t *testing.T) {
			var hdr []string
			for _, v := range tc.header {
				hdr = append(hdr, "Accept-Encoding", v)
			}
			c := negotiated(t)
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				w := do(c, method, hdr...)
				if got := w.Header().Get("Vary"); got != "Accept-Encoding" {
					t.Errorf("%s: Vary %q", method, got)
				}
				if tc.want == none {
					if w.Code != http.StatusNotAcceptable {
						t.Errorf("%s: %d, want 406", method, w.Code)
					}
					continue
				}
				if w.Code != 200 || w.Header().Get("Content-Encoding") != tc.want {
					t.Errorf("%s: %d Content-Encoding %q, want %q", method, w.Code, w.Header().Get("Content-Encoding"), tc.want)
				}
			}
		})
	}
}

func TestVaryOnEveryAnswer(t *testing.T) {
	c := negotiated(t)
	tag := c.encoded[0].etag
	for name, w := range map[string]*httptest.ResponseRecorder{
		"200":      do(c, http.MethodGet),
		"HEAD":     do(c, http.MethodHead),
		"304":      do(c, http.MethodGet, "Accept-Encoding", "br", "If-None-Match", tag),
		"406":      do(c, http.MethodGet, "Accept-Encoding", "identity;q=0"),
		"encoded":  do(c, http.MethodGet, "Accept-Encoding", "gzip"),
		"identity": do(c, http.MethodGet, "Accept-Encoding", "identity"),
	} {
		if got := w.Header().Values("Vary"); !slices.Contains(got, "Accept-Encoding") {
			t.Errorf("%s: Vary %v", name, got)
		}
	}
	// 304s of the encoded representation still say which it is.
	if w := do(c, http.MethodGet, "Accept-Encoding", "br", "If-None-Match", tag); w.Code != 304 || w.Header().Get("ETag") != tag {
		t.Errorf("304: %d %v", w.Code, w.Header())
	}
}

// Each representation has its own validator, the hash of the bytes sent.
func TestETagPerRepresentation(t *testing.T) {
	c := negotiated(t)
	br, gz, id := c.encoded[0], c.encoded[1], c.identity
	if br.etag == gz.etag || br.etag == id.etag || gz.etag == id.etag {
		t.Fatalf("etags must differ: %s %s %s", br.etag, gz.etag, id.etag)
	}
	for _, rep := range []representation{br, gz, id} {
		sum := sha256.Sum256(rep.body)
		if want := `"` + hex.EncodeToString(sum[:16]) + `"`; rep.etag != want {
			t.Errorf("%q: etag %s is not the hash of the bytes sent (%s)", rep.coding, rep.etag, want)
		}
	}
	// An etag learned for one encoding does not revalidate another.
	if w := do(c, http.MethodGet, "Accept-Encoding", "gzip", "If-None-Match", br.etag); w.Code != 200 || w.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("br etag against a gzip response: %d", w.Code)
	}
	if w := do(c, http.MethodGet, "Accept-Encoding", "gzip", "If-None-Match", gz.etag); w.Code != 304 {
		t.Errorf("gzip etag: %d", w.Code)
	}
	if w := do(c, http.MethodGet, "If-None-Match", br.etag); w.Code != 200 {
		t.Errorf("br etag against identity: %d", w.Code)
	}
}

func TestCompressedLengthAndBody(t *testing.T) {
	c := negotiated(t)
	w := do(c, http.MethodGet, "Accept-Encoding", "gzip")
	if w.Body.String() != "gzip" || w.Header().Get("Content-Length") != "4" {
		t.Errorf("%q %v", w.Body, w.Header())
	}
	hw := do(c, http.MethodHead, "Accept-Encoding", "gzip")
	if hw.Body.Len() != 0 || hw.Header().Get("Content-Length") != "4" || hw.Header().Get("Content-Encoding") != "gzip" || hw.Header().Get("ETag") != w.Header().Get("ETag") {
		t.Errorf("HEAD %v", hw.Header())
	}
}

func TestGzipIsRealAndSkippedWhenItDoesNotShrink(t *testing.T) {
	big := strings.Repeat("function hello(){return 1}\n", 200)
	fsys := fstest.MapFS{
		"big.js":   {Data: []byte(big)},
		"tiny.js":  {Data: []byte("x")},
		"pic.png":  {Data: []byte(big)}, // compressible bytes, but an image type: never compressed
		"noise.js": {Data: noise(4096)},
	}
	a := mustAssets(t, fsys)
	c := a.byName["big.js"].content
	i := slices.IndexFunc(c.encoded, func(r representation) bool { return r.coding == "gzip" })
	if i < 0 || len(c.encoded[i].body) >= len(big)/4 {
		t.Fatalf("big.js: encodings %+v", c.encoded)
	}
	zr, err := gzip.NewReader(bytes.NewReader(c.encoded[i].body))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(zr)
	if string(got) != big {
		t.Error("gzip does not round-trip")
	}
	for _, name := range []string{"tiny.js", "pic.png", "noise.js"} {
		if n := len(a.byName[name].content.encoded); n != 0 {
			t.Errorf("%s: %d encodings, want none", name, n)
		}
		if w := do(a.byName[name].content, http.MethodGet, "Accept-Encoding", "gzip, br"); w.Header().Get("Content-Encoding") != "" || w.Header().Get("Vary") != "Accept-Encoding" {
			t.Errorf("%s: %v", name, w.Header())
		}
	}
	if w := do(a.byName["tiny.js"].content, http.MethodGet, "Accept-Encoding", "identity;q=0, gzip"); w.Code != http.StatusNotAcceptable {
		t.Errorf("only gzip acceptable but none made: %d", w.Code)
	}
}

// noise is deterministic bytes that do not compress.
func noise(n int) []byte {
	out := make([]byte, 0, n)
	sum := sha256.Sum256(nil)
	for len(out) < n {
		out = append(out, sum[:]...)
		sum = sha256.Sum256(sum[:])
	}
	return out[:n]
}

// Compression happens when the registry is built; serving never compresses.
func TestNoCompressionPerRequest(t *testing.T) {
	var calls int
	fsys := testFS()
	fsys["index.html"] = &fstest.MapFile{Data: []byte(strings.Repeat("<p>page</p>", 100))}
	a, err := newAssets(fsys, fakeEncoders(&calls))
	if err != nil {
		t.Fatal(err)
	}
	built := calls
	if built == 0 {
		t.Fatal("nothing was compressed at build time")
	}
	for range 50 {
		for _, as := range a.byName {
			for _, ae := range []string{"", "gzip", "br", "gzip, br", "identity;q=0, br"} {
				do(as.content, http.MethodGet, "Accept-Encoding", ae)
				do(as.content, http.MethodHead, "Accept-Encoding", ae, "If-None-Match", as.content.identity.etag)
			}
		}
	}
	if calls != built {
		t.Errorf("%d compressions at request time", calls-built)
	}
}

// Over HTTP, each real file decodes back to the embedded bytes.
func TestEmbedGzipRoundTrip(t *testing.T) {
	h := testServer(t)
	a, _ := embeddedAssets()
	for name, as := range a.byName {
		w := get(h, http.MethodGet, "/embed/"+name, "Accept-Encoding", "gzip")
		if w.Header().Get("Content-Encoding") != "gzip" {
			if as.content.identity.coding == "" && len(as.content.encoded) == 0 {
				continue
			}
			t.Errorf("%s: not gzipped", name)
			continue
		}
		if w.Header().Get("Content-Length") != strconv.Itoa(w.Body.Len()) {
			t.Errorf("%s: Content-Length %s, body %d", name, w.Header().Get("Content-Length"), w.Body.Len())
		}
		zr, err := gzip.NewReader(w.Body)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(zr)
		if err != nil || !bytes.Equal(got, as.content.identity.body) {
			t.Errorf("%s: gzip does not round-trip (%v)", name, err)
		}
	}
}

func TestBrotliIsRealAndPreferred(t *testing.T) {
	big := strings.Repeat("function hello(){return 1}\n", 200)
	a := mustAssets(t, fstest.MapFS{"big.js": {Data: []byte(big)}})
	c := a.byName["big.js"].content
	if len(c.encoded) != 2 || c.encoded[0].coding != "br" || c.encoded[1].coding != "gzip" {
		t.Fatalf("encodings: %+v", c.encoded)
	}
	w := do(c, http.MethodGet, "Accept-Encoding", "gzip, deflate, br")
	if w.Header().Get("Content-Encoding") != "br" {
		t.Fatalf("browser header got %q", w.Header().Get("Content-Encoding"))
	}
	got, err := io.ReadAll(brotli.NewReader(w.Body))
	if err != nil || string(got) != big {
		t.Errorf("brotli does not round-trip (%v)", err)
	}
	if w := do(c, http.MethodGet, "Accept-Encoding", "br;q=0, gzip"); w.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("br;q=0 got %q", w.Header().Get("Content-Encoding"))
	}
}

// The first-load weight of the UI is a budget: brotli bytes of everything the
// page needs before it can run. Exceeding it fails the build; raise it only on purpose.
const shellBudget = 60 << 10

func TestShellSizeBudget(t *testing.T) {
	h := testServer(t)
	shell := []string{"/embed", "/embed/alpine.min.js", "/embed/client.js", "/embed/ui.js", "/embed/ui.css"}
	var raw, gz, br int
	var report strings.Builder
	for _, target := range shell {
		size := func(ae string) int {
			w := get(h, http.MethodGet, target, "Accept-Encoding", ae)
			if w.Code != 200 {
				t.Fatalf("%s: %d", target, w.Code)
			}
			return w.Body.Len()
		}
		r, g, b := size("identity"), size("gzip"), size("br")
		raw, gz, br = raw+r, gz+g, br+b
		fmt.Fprintf(&report, "\n  %-22s raw %6d  gzip %6d  brotli %6d", target, r, g, b)
	}
	t.Logf("shell:%s\n  %-22s raw %6d  gzip %6d  brotli %6d (budget %d)", report.String(), "total", raw, gz, br, shellBudget)
	if br > shellBudget {
		t.Errorf("shell is %d bytes of brotli, over the %d budget (gzip %d, raw %d):%s", br, shellBudget, gz, raw, report.String())
	}
}

func TestHashedNameFollowsContent(t *testing.T) {
	hashed := func(name, data string) string {
		fsys := testFS()
		fsys[name] = &fstest.MapFile{Data: []byte(data)}
		return mustAssets(t, fsys).byName[name].hashed
	}
	a, b := hashed("app.js", "one"), hashed("app.js", "two")
	if a == b {
		t.Errorf("different content, same hashed name %s", a)
	}
	if again := hashed("app.js", "one"); again != a {
		t.Errorf("same content, different hashed name: %s vs %s", again, a)
	}
	if !regexp.MustCompile(`^app\.[0-9a-f]{12}\.js$`).MatchString(a) {
		t.Errorf("shape: %s", a)
	}
	for name, want := range map[string]string{
		"sub/deep.js":      `^sub/deep\.[0-9a-f]{12}\.js$`,
		"alpine.min.js":    `^alpine\.min\.[0-9a-f]{12}\.js$`,
		"README":           `^README\.[0-9a-f]{12}$`,
		"layouts/w.css":    `^layouts/w\.[0-9a-f]{12}\.css$`,
		"dir.v2/noext":     `^dir\.v2/noext\.[0-9a-f]{12}$`,
		"x.tar.gz-ish.css": `^x\.tar\.gz-ish\.[0-9a-f]{12}\.css$`,
	} {
		if got := hashed(name, "z"); !regexp.MustCompile(want).MatchString(got) {
			t.Errorf("%s -> %s, want %s", name, got, want)
		}
	}
}

func TestRegistryRewritesTheDocument(t *testing.T) {
	fsys := testFS()
	fsys["index.html"] = &fstest.MapFile{Data: []byte(`<link href="/embed/app.css"><script src="/embed/app.js"></script><script src='/embed/sub/deep.js'></script>` +
		`<script src="/embed/app.js.map"></script><a href="/embed/missing.js"><img src="https://x/embed/app.js">`)}
	a := mustAssets(t, fsys)
	page := string(a.template)
	for _, name := range []string{"app.css", "app.js", "sub/deep.js"} {
		if !strings.Contains(page, "/embed/assets/"+a.byName[name].hashed) {
			t.Errorf("%s not rewritten: %s", name, page)
		}
	}
	for _, kept := range []string{`"/embed/app.js.map"`, `"/embed/missing.js"`, `https://x/embed/app.js`} {
		if !strings.Contains(page, kept) {
			t.Errorf("%s must be left alone: %s", kept, page)
		}
	}
	if strings.Contains(page, `"/embed/app.js"`) || strings.Contains(page, `'/embed/sub/deep.js'`) {
		t.Errorf("unhashed reference left: %s", page)
	}
	if _, err := newAssets(fstest.MapFS{"assets/x.js": {Data: []byte("x")}}, nil); err == nil {
		t.Error("a file under assets/ would shadow the hashed namespace")
	}
}

// Only hashed URLs are immutable; the unhashed names and the page revalidate.
func TestImmutableOnlyOnHashedURLs(t *testing.T) {
	h := testServer(t)
	a, _ := embeddedAssets()
	for name, as := range a.byName {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			hashed := get(h, method, "/embed/assets/"+as.hashed)
			plain := get(h, method, "/embed/"+name)
			if hashed.Code != 200 || plain.Code != 200 {
				t.Fatalf("%s %s: %d / %d", method, name, hashed.Code, plain.Code)
			}
			if got := hashed.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
				t.Errorf("%s hashed Cache-Control: %q", name, got)
			}
			if got := plain.Header().Get("Cache-Control"); got != "no-cache" || strings.Contains(got, "immutable") {
				t.Errorf("%s unhashed Cache-Control: %q", name, got)
			}
			// The same bytes under both names, so they validate alike.
			if hashed.Header().Get("ETag") != plain.Header().Get("ETag") || !bytes.Equal(hashed.Body.Bytes(), plain.Body.Bytes()) {
				t.Errorf("%s: the two URLs serve different things", name)
			}
			if got := hashed.Header().Get("Vary"); got != "Accept-Encoding" {
				t.Errorf("%s: Vary %q", name, got)
			}
		}
		// Conditional requests on the immutable URL keep its caching headers.
		w := get(h, http.MethodGet, "/embed/assets/"+as.hashed, "If-None-Match", as.content.identity.etag)
		if w.Code != 304 || w.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
			t.Errorf("%s: %d %v", name, w.Code, w.Header())
		}
	}
	page := get(h, http.MethodGet, "/embed")
	if page.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("page Cache-Control: %q", page.Header().Get("Cache-Control"))
	}
}

func TestHashedURLHostileInput(t *testing.T) {
	h := testServer(t)
	a, _ := embeddedAssets()
	ui := a.byName["ui.js"]
	stem := strings.TrimSuffix(ui.hashed, ".js") // ui.<hash>
	hash := strings.TrimPrefix(stem, "ui.")
	for _, target := range []string{
		"/embed/assets/ui.js",                                 // known name, no hash
		"/embed/assets/ui." + strings.ToUpper(hash) + ".js",   // hashes are lowercase
		"/embed/assets/UI." + hash + ".js",                    // names are case sensitive
		"/embed/assets/ui." + hash[:11] + ".js",               // truncated
		"/embed/assets/ui." + hash + "0.js",                   // extended
		"/embed/assets/" + ui.hashed + "/",                    // trailing slash
		"/embed/assets/" + ui.hashed + "%00",                  // NUL
		"/embed/assets/" + ui.hashed + ".gz",                  // suffix
		"/embed/assets/",                                      // the namespace itself
		"/embed/assets",                                       // without the slash
		"/embed/assets//" + ui.hashed,                         // empty segment
		"/embed/assets/%2e%2e/" + ui.hashed,                   // encoded dot segments
		"/embed/assets/..%2f" + ui.hashed,                     // encoded slash
		"/embed/assets/%252e%252e/" + ui.hashed,               // double encoding
		"/embed/assets/..%252fui.js",                          // double-encoded slash
		"/embed/assets/ui.%2e" + hash + ".js",                 // encoded dot inside
		"/embed/assets/%75i." + hash + ".js/..",               // escaped letter plus dot segment
		"/embed/assets/assets/" + ui.hashed,                   // nested namespace
		"/embed/assets/index.html",                            // the template
		"/embed/assets/../../../etc/passwd",                   // traversal
		"/embed/assets/" + strings.Repeat("a", 1<<16) + ".js", // long
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			w := get(h, method, target)
			if w.Code == 200 {
				t.Errorf("%s %.80s: served", method, target)
			}
			if w.Code == 404 && strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
				t.Errorf("%.80s: a 404 must never be cached as immutable", target)
			}
		}
	}
	// An unknown hash of an unknown stem never turns into a redirect.
	if w := get(h, http.MethodGet, "/embed/assets/nope.000000000000.js"); w.Code != 404 || w.Header().Get("Location") != "" || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("unknown stem: %d %v", w.Code, w.Header())
	}
	// A query string changes nothing; the right file is served.
	if w := get(h, http.MethodGet, "/embed/assets/"+ui.hashed+"?v=1"); w.Code != 200 {
		t.Errorf("query: %d", w.Code)
	}
	if w := get(h, http.MethodPost, "/embed/assets/"+ui.hashed); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", w.Code)
	}
}

var embedRef = regexp.MustCompile(`(?:src|href)=["'](/embed/[^"']*)["']`)

// Every asset the page references exists, and the served page names each at
// its hashed URL, which answers 200 with the right type.
func TestPageAssetsExist(t *testing.T) {
	h := testServer(t)
	a, _ := embeddedAssets()
	raw, err := embedFS.ReadFile("embed/" + pageName)
	if err != nil {
		t.Fatal(err)
	}
	refs := embedRef.FindAllStringSubmatch(string(raw), -1)
	if len(refs) < 4 {
		t.Fatalf("expected the page to reference its scripts and stylesheet, found %v", refs)
	}
	page := get(h, http.MethodGet, "/embed").Body.String()
	for _, m := range refs {
		name := strings.TrimPrefix(m[1], "/embed/")
		as := a.byName[name]
		if as == nil {
			t.Errorf("index.html references %s, which is not embedded", m[1])
			continue
		}
		if !strings.Contains(page, `"/embed/assets/`+as.hashed+`"`) {
			t.Errorf("%s is not served at its hashed URL", m[1])
		}
		if strings.Contains(page, `"`+m[1]+`"`) {
			t.Errorf("%s is still referenced unhashed", m[1])
		}
		w := get(h, http.MethodGet, "/embed/assets/"+as.hashed)
		if w.Code != 200 || w.Header().Get("Content-Type") != as.content.ctype {
			t.Errorf("%s: %d %q", as.hashed, w.Code, w.Header().Get("Content-Type"))
		}
	}
	// Every URL in the served page resolves.
	for _, m := range embedRef.FindAllStringSubmatch(page, -1) {
		if w := get(h, http.MethodGet, m[1]); w.Code != 200 {
			t.Errorf("%s: %d", m[1], w.Code)
		}
	}
}

// The page's CSP does not change with hashed URLs: same-origin files, and
// nothing inline but the one script allowed by its hash.
func TestPageCSPUnchanged(t *testing.T) {
	cfg := &config.Config{Server: config.Server{Secret: "s", CORSOrigins: []string{"https://shop.example.com"}}}
	cfg.SetDefaults()
	w := get(New(cfg, nil, nil, nil).Handler(), http.MethodGet, "/embed")
	csp := w.Header().Get("Content-Security-Policy")
	want := regexp.MustCompile(`^default-src 'self'; script-src 'self' 'sha256-[A-Za-z0-9+/]{43}='; style-src 'self'; img-src 'self' data: https:; connect-src 'self'; font-src 'self'; frame-ancestors https://shop\.example\.com$`)
	if !want.MatchString(csp) {
		t.Errorf("CSP: %s", csp)
	}
	if n := strings.Count(w.Body.String(), "<script>"); n != 1 {
		t.Errorf("%d inline scripts, want the one config script", n)
	}
	if strings.Contains(w.Body.String(), "<style") {
		t.Error("inline style")
	}
}

// HEAD answers with exactly the headers of GET, and no body.
func TestHeadMatchesGet(t *testing.T) {
	h := testServer(t)
	a, _ := embeddedAssets()
	ui := a.byName["ui.js"]
	for _, target := range []string{"/embed", "/embed/ui.js", "/embed/assets/" + ui.hashed, "/embed/ui.css", "/embed/assets/nope.js", "/embed/nope"} {
		for _, hdr := range [][]string{
			nil,
			{"Accept-Encoding", "br"},
			{"Accept-Encoding", "gzip"},
			{"Accept-Encoding", "identity;q=0"},
			{"If-None-Match", "*"},
			{"If-None-Match", ui.content.identity.etag},
			{"Range", "bytes=0-9"},
		} {
			g, hd := get(h, http.MethodGet, target, hdr...), get(h, http.MethodHead, target, hdr...)
			// (the recorder, unlike a server, does not drop the text of an error answered to HEAD)
			if g.Code != hd.Code || (hd.Code < 400 && hd.Body.Len() != 0) || !maps.EqualFunc(g.Header(), hd.Header(), slices.Equal) {
				t.Errorf("%s %v: GET %d %v, HEAD %d %v", target, hdr, g.Code, g.Header(), hd.Code, hd.Header())
			}
		}
	}
	// Thousands of header lines are bounded, not matched.
	var many []string
	for range 5000 {
		many = append(many, "If-None-Match", `"x"`)
	}
	many = append(many, "If-None-Match", ui.content.identity.etag)
	if w := get(h, http.MethodGet, "/embed/ui.js", many...); w.Code != 200 {
		t.Errorf("many If-None-Match lines: %d", w.Code)
	}
}

func BenchmarkServeAsset(b *testing.B) {
	cfg := &config.Config{Server: config.Server{Secret: "s"}}
	cfg.SetDefaults()
	h := New(cfg, nil, nil, nil).Handler()
	a, _ := embeddedAssets()
	for _, c := range []struct{ name, target, ae, inm string }{
		{"ui.js identity", "/embed/assets/" + a.byName["ui.js"].hashed, "", ""},
		{"ui.js gzip", "/embed/assets/" + a.byName["ui.js"].hashed, "gzip", ""},
		{"ui.js br", "/embed/assets/" + a.byName["ui.js"].hashed, "gzip, deflate, br", ""},
		{"ui.js 304", "/embed/ui.js", "br", "*"},
		{"page br", "/embed", "br", ""},
	} {
		b.Run(c.name, func(b *testing.B) {
			r := httptest.NewRequest(http.MethodGet, c.target, nil)
			if c.ae != "" {
				r.Header.Set("Accept-Encoding", c.ae)
			}
			if c.inm != "" {
				r.Header.Set("If-None-Match", c.inm)
			}
			h.ServeHTTP(httptest.NewRecorder(), r) // builds the page
			b.ReportAllocs()
			for b.Loop() {
				h.ServeHTTP(&discard{header: http.Header{}}, r)
			}
		})
	}
}

// discard is a ResponseWriter that keeps nothing, so a benchmark measures serving alone.
type discard struct{ header http.Header }

func (d *discard) Header() http.Header         { return d.header }
func (d *discard) Write(p []byte) (int, error) { return len(p), nil }
func (d *discard) WriteHeader(int)             {}

// A page and an instance of different versions (a rolling deploy) still get
// the file: the current bytes, and never anything a cache may keep.
func TestStaleHashServesCurrentBytes(t *testing.T) {
	h := testServer(t)
	a, _ := embeddedAssets()
	for name, as := range a.byName {
		ext := path.Ext(name)
		stale := "/embed/assets/" + strings.TrimSuffix(name, ext) + ".000000000000" + ext
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			for _, ae := range []string{"", "gzip", "br"} {
				w := get(h, method, stale, "Accept-Encoding", ae)
				fresh := get(h, method, "/embed/assets/"+as.hashed, "Accept-Encoding", ae)
				if w.Code != 200 {
					t.Errorf("%s %s [%s]: %d", method, stale, ae, w.Code)
					continue
				}
				if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
					t.Errorf("%s: Cache-Control %q, want no-store", stale, cc)
				}
				if w.Header().Get("Vary") != "Accept-Encoding" || w.Header().Get("ETag") != fresh.Header().Get("ETag") ||
					w.Header().Get("Content-Encoding") != fresh.Header().Get("Content-Encoding") || w.Header().Get("Content-Type") != as.content.ctype {
					t.Errorf("%s [%s]: headers %v, want those of the current file %v", stale, ae, w.Header(), fresh.Header())
				}
				if method == http.MethodGet && !bytes.Equal(w.Body.Bytes(), fresh.Body.Bytes()) {
					t.Errorf("%s [%s]: not the current bytes", stale, ae)
				}
			}
		}
		if w := get(h, http.MethodGet, stale, "If-None-Match", as.content.identity.etag); w.Code != 304 || w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: conditional %d %v", stale, w.Code, w.Header())
		}
	}
	// The same through a stale hash that names a file that has no extension or a directory.
	reg := mustAssets(t, testFS())
	for rest, want := range map[string]string{
		"app.ffffffffffff.js":        "app.js",
		"sub/deep.ffffffffffff.js":   "sub/deep.js",
		"README.ffffffffffff":        "README",
		reg.byName["app.js"].hashed:  "app.js",
		"app.ffffffffffff.css":       "app.css",
		"sub/deep.ffffffffffff.css":  "",
		"sub/app.ffffffffffff.js":    "",
		"app.ffffffffffff.png":       "",
		"app.fffffffffff.js":         "",
		"app.fffffffffffff.js":       "",
		"app.FFFFFFFFFFFF.js":        "",
		"assets/app.ffffffffffff.js": "",
		"app.js":                     "",
		"":                           "",
	} {
		as, _ := reg.hashed(rest)
		got := ""
		if as != nil {
			got = as.name
		}
		if got != want {
			t.Errorf("%q -> %q, want %q", rest, got, want)
		}
	}
}
