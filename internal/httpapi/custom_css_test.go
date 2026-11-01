package httpapi

import (
	"bytes"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alash3al/hfy/internal/config"
)

func writeCSS(t *testing.T, content []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "brand.css")
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Every form of path and content the operator can hand over is decided: read,
// or refused at startup with the path and the reason.
func TestReadCustomCSS(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.css")
	target := writeCSS(t, []byte(":root{--hfy-bg:#fff}"))
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(dir, "dangling.css")
	if err := os.Symlink(filepath.Join(dir, "nope.css"), dangling); err != nil {
		t.Fatal(err)
	}
	dotted := filepath.Join(filepath.Dir(target), "sub", "..", filepath.Base(target))
	if err := os.Mkdir(filepath.Join(filepath.Dir(target), "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	big := bytes.Repeat([]byte("a{}\n"), maxCustomCSS/4)

	for _, tc := range []struct {
		name    string
		path    string
		want    string // the bytes served, when it loads
		wantErr string // a fragment of the error, when it does not
	}{
		{"absolute", target, ":root{--hfy-bg:#fff}", ""},
		{"symlink is followed", link, ":root{--hfy-bg:#fff}", ""},
		{"dot-dot in the path", dotted, ":root{--hfy-bg:#fff}", ""},
		{"empty file", writeCSS(t, nil), "", ""},
		{"BOM is dropped", writeCSS(t, []byte("\xef\xbb\xbf.a{color:red}")), ".a{color:red}", ""},
		{"BOM alone", writeCSS(t, []byte("\xef\xbb\xbf")), "", ""},
		{"CRLF is kept", writeCSS(t, []byte(".a{\r\n color:red\r\n}\r\n")), ".a{\r\n color:red\r\n}\r\n", ""},
		{"exactly the cap", writeCSS(t, big), string(big), ""},
		{"one byte over the cap", writeCSS(t, append(append([]byte{}, big...), '\n')), "", "larger than 64 KiB"},
		{"way over the cap", writeCSS(t, bytes.Repeat([]byte("x"), 8<<20)), "", "larger than 64 KiB"},
		{"missing", filepath.Join(dir, "nope.css"), "", "no such file"},
		{"dangling symlink", dangling, "", "no such file"},
		{"directory", dir, "", "is a directory"},
		{"/dev/zero", "/dev/zero", "", "not a regular file"},
		{"/dev/null", "/dev/null", "", "not a regular file"},
		{"FIFO", fifo, "", "not a regular file"},
		{"invalid UTF-8", writeCSS(t, []byte(".a{content:\"\xff\xfe\"}")), "", "not valid UTF-8"},
		{"NUL", writeCSS(t, []byte(".a{color:red}\x00")), "", "NUL"},
		{"@import", writeCSS(t, []byte("@import url(https://fonts.example.com/x.css);")), "", "@import"},
		{"@IMPORT in another case", writeCSS(t, []byte("@IMPORT 'x.css';")), "", "@import"},
		{"@import inside a comment is fine", writeCSS(t, []byte("/* @import 'x.css'; */ .a{}")), "/* @import 'x.css'; */ .a{}", ""},
		{"@import after an unterminated comment is a comment", writeCSS(t, []byte(".a{} /* @import 'x.css';")), ".a{} /* @import 'x.css';", ""},
		{"empty path (config never passes one)", "", "", "no such file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan struct{})
			var got []byte
			var err error
			go func() { got, err = ReadCustomCSS(tc.path); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("ReadCustomCSS did not return: a FIFO or device blocked startup")
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.HasPrefix(err.Error(), "embed.custom_css ") {
					t.Fatalf("got %q, %v; want an error with %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil || string(got) != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

type endless struct{ read int }

func (e *endless) Read(p []byte) (int, error) {
	e.read += len(p)
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

// A source that never ends is refused after one byte past the cap, not read to the end.
func TestReadCappedStopsAtTheCap(t *testing.T) {
	src := &endless{}
	done := make(chan error, 1)
	go func() { _, err := readCapped(src); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "larger than 64 KiB") {
			t.Errorf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readCapped did not stop")
	}
	if src.read > maxCustomCSS+64<<10 {
		t.Errorf("read %d bytes of an endless source", src.read)
	}
}

func TestReadCustomCSSNamesThePath(t *testing.T) {
	_, err := ReadCustomCSS("/nonexistent/brand.css")
	if err == nil || err.Error() != "embed.custom_css /nonexistent/brand.css: no such file or directory" {
		t.Errorf("%v", err)
	}
}

// A remote url() is allowed by the page's CSP for https images only: it is
// loaded, and said so at startup.
func TestReadCustomCSSWarnsOnRemoteURL(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)
	for css, warns := range map[string]bool{
		".a{background:url(https://cdn.example.com/a.png)}":    true,
		".a{background:url( 'http://cdn.example.com/a.png' )}": true,
		".a{background:url(//cdn.example.com/a.png)}":          true,
		".a{background:url(data:image/png;base64,AAAA)}":       false,
		".a{background:url(/embed/x.png)}":                     false,
		"/* url(https://example.com/a.png) */ .a{}":            false,
	} {
		logs.Reset()
		if _, err := ReadCustomCSS(writeCSS(t, []byte(css))); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(logs.String(), "remote url()"); got != warns {
			t.Errorf("%s: warned %v, want %v (%s)", css, got, warns, logs.String())
		}
	}
}

func customServer(t *testing.T, css []byte, embed config.Embed) (*Server, http.Handler) {
	t.Helper()
	cfg := &config.Config{Server: config.Server{Secret: "s"}, Embed: embed}
	cfg.SetDefaults()
	srv := New(cfg, nil, nil, nil)
	if css != nil {
		if err := srv.EnableCustomCSS(css); err != nil {
			t.Fatal(err)
		}
	}
	return srv, srv.Handler()
}

// The sheet is served like every asset (hashed, immutable, compressed), and
// linked last: after ui.css and after the layout's sheet.
func TestCustomCSSIsServedAndLinkedLast(t *testing.T) {
	css := []byte(":root{--hfy-accent-soft:#fee}\n")
	srv, h := customServer(t, css, config.Embed{Layout: "whatsapp"})
	custom := srv.assets.byName[customCSSName]
	if custom == nil {
		t.Fatal("not registered")
	}
	url := "/embed/" + assetsPrefix + custom.hashed
	if !strings.HasPrefix(custom.hashed, "custom.") || !strings.HasSuffix(custom.hashed, ".css") {
		t.Errorf("hashed name %q", custom.hashed)
	}
	w := get(h, http.MethodGet, url)
	if w.Code != 200 || w.Body.String() != string(css) || w.Header().Get("Cache-Control") != immutable || w.Header().Get("Content-Type") != stylesheet {
		t.Errorf("%s: %d %q %v", url, w.Code, w.Body.String(), w.Header())
	}
	if w := get(h, http.MethodGet, "/embed/custom.css"); w.Code != 200 || w.Header().Get("Cache-Control") != "no-cache" || w.Header().Get("ETag") == "" {
		t.Errorf("/embed/custom.css: %d %v", w.Code, w.Header())
	}
	if w := get(h, http.MethodGet, url, "Accept-Encoding", "br"); w.Code != 200 || w.Header().Get("Content-Encoding") != "" {
		// a 29-byte sheet is smaller as it is: the registry only compresses what shrinks
		t.Errorf("tiny sheet: %d %q", w.Code, w.Header().Get("Content-Encoding"))
	}
	page := get(h, http.MethodGet, "/embed").Body.String()
	ui, layout, link := strings.Index(page, "/embed/assets/ui."), strings.Index(page, "data-hfy-layout"), strings.Index(page, `href="`+url+`" data-hfy-custom>`)
	if ui <= 0 || ui >= layout || layout >= link {
		t.Errorf("order: ui.css %d, layout sheet %d, custom %d (want ascending)", ui, layout, link)
	}
	if strings.Count(page, "data-hfy-custom") != 1 || strings.Contains(page, string(css)) {
		t.Error("the page links the sheet once and never carries its contents")
	}
}

func TestCustomCSSOnTheDefaultLayout(t *testing.T) {
	srv, h := customServer(t, []byte(".a{}"), config.Embed{})
	page := get(h, http.MethodGet, "/embed").Body.String()
	if strings.Contains(page, "data-hfy-layout") || !strings.Contains(page, "/embed/"+assetsPrefix+srv.assets.byName[customCSSName].hashed) {
		t.Error("custom sheet only, after ui.css")
	}
}

// Without the setting nothing changes: no link, no route, and one registry
// shared by all servers stays untouched by a server that has a sheet.
func TestCustomCSSIsPerServer(t *testing.T) {
	shared, _ := embeddedAssets()
	_, with := customServer(t, []byte(".a{}"), config.Embed{})
	_, without := customServer(t, nil, config.Embed{})
	if shared.byName[customCSSName] != nil {
		t.Fatal("the process-wide registry gained the sheet")
	}
	if strings.Contains(get(without, http.MethodGet, "/embed").Body.String(), "data-hfy-custom") {
		t.Error("a link without custom_css")
	}
	if w := get(without, http.MethodGet, "/embed/custom.css"); w.Code != http.StatusNotFound {
		t.Errorf("/embed/custom.css without the setting: %d", w.Code)
	}
	if w := get(with, http.MethodGet, "/embed/custom.css"); w.Code != 200 {
		t.Errorf("/embed/custom.css with it: %d", w.Code)
	}
}

// The file is read once: editing it changes nothing until a new server is built.
func TestCustomCSSChangesNeedARestart(t *testing.T) {
	p := writeCSS(t, []byte(".v1{}"))
	read := func() (string, string) {
		css, err := ReadCustomCSS(p)
		if err != nil {
			t.Fatal(err)
		}
		srv, h := customServer(t, css, config.Embed{})
		return srv.assets.byName[customCSSName].hashed, get(h, http.MethodGet, "/embed/custom.css").Body.String()
	}
	srv, h := func() (*Server, http.Handler) {
		css, _ := ReadCustomCSS(p)
		return customServer(t, css, config.Embed{})
	}()
	v1 := srv.assets.byName[customCSSName].hashed
	if err := os.WriteFile(p, []byte(".v2{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := get(h, http.MethodGet, "/embed/custom.css").Body.String(); got != ".v1{}" {
		t.Errorf("a running server serves %q after the edit", got)
	}
	if hashed, body := read(); hashed == v1 || body != ".v2{}" {
		t.Errorf("a new server: %s %q, want a new hash and the new bytes", hashed, body)
	}
}

func TestEnableCustomCSSAfterHandlerPanics(t *testing.T) {
	srv, _ := customServer(t, nil, config.Embed{})
	defer func() {
		if r := recover(); r == nil {
			t.Error("EnableCustomCSS after Handler did not panic")
		}
	}()
	_ = srv.EnableCustomCSS([]byte(".a{}"))
}

// The CSS never reaches the page, and the sheet's link is covered by the
// CSP as it is: same-origin, no inline style.
func TestCustomCSSLeavesTheCSPAlone(t *testing.T) {
	_, h := customServer(t, []byte(".a{}"), config.Embed{})
	w := get(h, http.MethodGet, "/embed")
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "style-src 'self';") || strings.Contains(csp, "unsafe-inline") {
		t.Errorf("CSP %s", csp)
	}
}
