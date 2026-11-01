package httpapi_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/httpapi"
)

func TestEmbedLayoutConfig(t *testing.T) {
	cfg := &config.Config{Server: config.Server{Secret: "s"}, Embed: config.Embed{Layout: "whatsapp"}}
	cfg.SetDefaults()
	srv := httpapi.New(cfg, nil, nil, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/embed", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"layout":"whatsapp"`) {
		t.Errorf("/embed must carry layout whatsapp: %d %s", rec.Code, rec.Body.String())
	}
}

// Per-viewer layout: the page boots with the configured layout, a configure
// message switches it, and an unknown layout is ignored (bubbles on boot).
func TestEmbedLayoutBridge(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("no node")
	}
	src, err := os.ReadFile("embed/ui.js")
	if err != nil {
		t.Fatal(err)
	}
	quoted := strconv.Quote(string(src) + `
;return {hfyEmbed: window.hfyEmbed, resolve: hfyResolveConfig, inbound: hfyHandleInbound, bridge: hfyBridge};`)
	driver := `
		var el = {dataset: {}, style: {setProperty: function () {}, removeProperty: function () {}}};
		var win = { addEventListener: function () {} };
		win.parent = win;
		var doc = { documentElement: el, addEventListener: function () {}, querySelector: function () { return null },
			querySelectorAll: function () { return [] } };
		var nav = { language: 'en' };
		var api = new Function('window', 'document', 'navigator', 'requestAnimationFrame', ` + quoted + `)(win, doc, nav, function () {});
		function eq(a, b) { if (a !== b) throw new Error('eq failed: ' + JSON.stringify(a) + ' != ' + JSON.stringify(b)) }
		function boot(raw) {
			win.__HFY_EMBED__ = raw;
			var c = api.hfyEmbed();
			c.config = api.resolve();
			c.applyDom();
			api.bridge.component = c;
			return c;
		}
		boot({layout: 'whatsapp'});
		eq(el.dataset.layout, 'whatsapp');
		boot({layout: 'chat'});
		eq(el.dataset.layout, 'bubbles');
		boot(null);
		eq(el.dataset.layout, 'bubbles');
		api.inbound({v: 1, type: 'configure', layout: 'whatsapp'});
		eq(el.dataset.layout, 'whatsapp');
		api.inbound({v: 1, type: 'configure', layout: 'chat'});
		eq(el.dataset.layout, 'whatsapp');
		api.inbound({v: 1, type: 'configure', layout: 'transcript'});
		eq(el.dataset.layout, 'transcript');
		api.inbound({v: 1, type: 'configure', lang: 'en'});
		eq(el.dataset.layout, 'transcript');
		api.inbound({v: 1, type: 'configure', layout: 'bubbles'});
		eq(el.dataset.layout, 'bubbles');
	`
	out, err := exec.Command("node", "-e", driver).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}

// The palette is a fixed set of constants: pin the light and dark values so a
// retune is a deliberate change here and in the stylesheet together.
func TestEmbedWhatsappPalette(t *testing.T) {
	src, err := os.ReadFile("embed/ui.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(src)
	for _, tc := range []struct {
		selector string
		want     map[string]string
	}{
		{`[data-layout="whatsapp"]{`, map[string]string{
			"--hfy-wa-wall": "#efeae2", "--hfy-wa-out": "#d9fdd3", "--hfy-wa-in": "#fff", "--hfy-wa-text": "#111b21",
			"--hfy-wa-muted": "#667781", "--hfy-wa-read": "#53bdeb", "--hfy-wa-bar": "#f0f2f5", "--hfy-wa-pill": "#fff",
			"--hfy-wa-send": "#00a884",
		}},
		{`[data-layout="whatsapp"][data-theme="dark"]{`, map[string]string{
			"--hfy-wa-wall": "#0b141a", "--hfy-wa-out": "#005c4b", "--hfy-wa-in": "#202c33", "--hfy-wa-text": "#e9edef",
			"--hfy-wa-muted": "#8696a0", "--hfy-wa-read": "#53bdeb", "--hfy-wa-bar": "#202c33", "--hfy-wa-pill": "#2a3942",
		}},
	} {
		_, after, ok := strings.Cut(css, tc.selector)
		if !ok {
			t.Fatalf("no block %s", tc.selector)
		}
		body, _, _ := strings.Cut(after, "}")
		decls := map[string]string{}
		for d := range strings.SplitSeq(body, ";") {
			if name, value, ok := strings.Cut(strings.TrimSpace(d), ":"); ok {
				decls[name] = value
			}
		}
		for name, want := range tc.want {
			if decls[name] != want {
				t.Errorf("%s %s = %q, want %q", tc.selector, name, decls[name], want)
			}
		}
	}
}

var (
	cssCommentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)
	cssRuleRe    = regexp.MustCompile(`([^{}]+)\{([^{}]*)\}`)
	cssSharedRe  = regexp.MustCompile(`:is\(\[data-layout="bubbles"\],\[data-layout="whatsapp"\]\)`)
)

// bubblesRules lists the flat stylesheet rules the bubbles layout applies. The
// whatsapp layout shares them through :is(), folded back here, and rules that
// name whatsapp on their own are left out.
func bubblesRules(css string) string {
	var out strings.Builder
	for _, m := range cssRuleRe.FindAllStringSubmatch(cssCommentRe.ReplaceAllString(css, ""), -1) {
		sel := strings.Join(strings.Fields(cssSharedRe.ReplaceAllString(m[1], `[data-layout="bubbles"]`)), " ")
		if !strings.Contains(sel, `[data-layout="bubbles"]`) || strings.Contains(sel, "whatsapp") {
			continue
		}
		fmt.Fprintf(&out, "%s{%s}\n", sel, strings.Join(strings.Fields(m[2]), " "))
	}
	return out.String()
}

// The bubbles layout looks exactly as before the whatsapp layout: its rules,
// with whatsapp folded out, match the golden taken before the change.
func TestEmbedBubblesRulesUnchanged(t *testing.T) {
	src, err := os.ReadFile("embed/ui.css")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/bubbles_rules.golden")
	if err != nil {
		t.Fatal(err)
	}
	if got := bubblesRules(string(src)); got != string(want) {
		t.Errorf("bubbles rules changed; the bubbles layout must not change:\n%s", got)
	}
}
