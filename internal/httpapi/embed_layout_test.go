package httpapi_test

import (
	"fmt"
	"math"
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
	for _, tc := range []struct {
		name  string
		embed config.Embed
		want  []string
	}{
		{"unset accent", config.Embed{Layout: "whatsapp"}, []string{`"layout":"whatsapp"`, `"accent_set":false`}},
		{"explicit accent", config.Embed{Layout: "whatsapp", Accent: "#e11d48"}, []string{`"accent":"#e11d48"`, `"accent_set":true`}},
		{"explicit indigo", config.Embed{Layout: "whatsapp", Accent: "#6366f1"}, []string{`"accent":"#6366f1"`, `"accent_set":true`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{Server: config.Server{Secret: "s"}, Embed: tc.embed}
			cfg.SetDefaults()
			srv := httpapi.New(cfg, nil, nil, nil)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/embed", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("/embed: %d", rec.Code)
			}
			for _, want := range tc.want {
				if !strings.Contains(rec.Body.String(), want) {
					t.Errorf("/embed must carry %s: %s", want, rec.Body.String())
				}
			}
		})
	}
}

// layoutStub is a DOM stub for applyDom: a composer pill in the DOM order the
// markup has (attach before the input), a matchMedia the test can flip to dark.
const layoutStub = `
	var el = {dataset: {}, style: {props: {}, setProperty: function (k, v) { this.props[k] = v; }, removeProperty: function (k) { delete this.props[k]; }}};
	function part(cls) { return {cls: cls}; }
	var input = part('hfy-input'), attach = part('hfy-attach'), camera = part('hfy-camera');
	var pill = {children: [attach, input, camera],
		insertBefore: function (n, ref) { this.children.splice(this.children.indexOf(n), 1); this.children.splice(this.children.indexOf(ref), 0, n); },
		querySelector: function (s) { return this.children.filter(function (x) { return x.cls === s.slice(1); })[0] || null; }};
	[input, attach, camera].forEach(function (x) {
		Object.defineProperty(x, 'nextElementSibling', {get: function () { return pill.children[pill.children.indexOf(x) + 1] || null; }});
	});
	var doc = {documentElement: el, title: '', addEventListener: function () {},
		querySelector: function (s) { return s === '.hfy-composer .hfy-pill' ? pill : null; },
		querySelectorAll: function () { return []; }};
	var dark = false;
	var matchMediaStub = function (q) { return {matches: /dark/.test(q) ? dark : false}; };
	var win = {addEventListener: function () {}, matchMedia: matchMediaStub};
	win.parent = win;
	var nav = {language: 'en'};
	function eq(a, b, m) { if (JSON.stringify(a) !== JSON.stringify(b)) throw new Error((m || 'eq') + ': ' + JSON.stringify(a) + ' != ' + JSON.stringify(b)); }
	function truthy(v, m) { if (!v) throw new Error('falsy: ' + m); }
`

// runLayoutNode runs body under node with the real ui.js loaded against layoutStub.
// body gets api (hfyEmbed, resolve, inbound, bridge, setClient) and boot(rawConfig),
// which loads a component with that page config the way the page does.
func runLayoutNode(t *testing.T, body string) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("no node")
	}
	src, err := os.ReadFile("embed/ui.js")
	if err != nil {
		t.Fatal(err)
	}
	quoted := strconv.Quote(string(src) + `
;return {hfyEmbed: window.hfyEmbed, resolve: hfyResolveConfig, inbound: hfyHandleInbound, bridge: hfyBridge, setClient: function (v) { hfyClient = v }};`)
	driver := layoutStub + `
		var api = new Function('window', 'document', 'navigator', 'requestAnimationFrame', 'matchMedia', ` + quoted + `)(win, doc, nav, function () {}, matchMediaStub);
		function boot(raw) {
			win.__HFY_EMBED__ = raw;
			var c = api.hfyEmbed();
			c.config = api.resolve();
			c.applyDom();
			api.bridge.component = c;
			return c;
		}
	` + body
	out, err := exec.Command("node", "-e", driver).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}

// cssDecls returns the declarations of the first flat rule whose selector is sel
// (written with its opening brace, e.g. `[data-density="compact"]{`).
func cssDecls(t *testing.T, css, sel string) map[string]string {
	t.Helper()
	_, after, ok := strings.Cut(css, sel)
	if !ok {
		t.Fatalf("no block %s", sel)
	}
	body, _, _ := strings.Cut(after, "}")
	decls := map[string]string{}
	for d := range strings.SplitSeq(body, ";") {
		if name, value, ok := strings.Cut(strings.TrimSpace(d), ":"); ok {
			decls[name] = strings.TrimSpace(value)
		}
	}
	return decls
}

func readUICSS(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("embed/ui.css")
	if err != nil {
		t.Fatal(err)
	}
	return string(src)
}

// Theme row: light and dark are fixed, auto follows the system.
func TestEmbedWhatsappTheme(t *testing.T) {
	runLayoutNode(t, `
		boot({layout: 'whatsapp', theme: 'auto'});
		eq(el.dataset.theme, 'light', 'auto follows a light system');
		dark = true;
		boot({layout: 'whatsapp', theme: 'auto'});
		eq(el.dataset.theme, 'dark', 'auto follows a dark system');
		boot({layout: 'whatsapp', theme: 'light'});
		eq(el.dataset.theme, 'light', 'light wins over a dark system');
		boot({layout: 'whatsapp', theme: 'dark'});
		eq(el.dataset.theme, 'dark', 'dark wins over a light system');
		eq(el.dataset.layout, 'whatsapp', 'layout set');
		dark = false;
	`)
}

// Density row: compact, comfortable and spacious set different font, gap and
// padding values, and the whatsapp palette block leaves those tokens to density.
func TestEmbedWhatsappDensity(t *testing.T) {
	css := readUICSS(t)
	base := cssDecls(t, css, ":root{")
	rows := map[string]map[string]string{
		"comfortable": base,
		"compact":     cssDecls(t, css, `[data-density="compact"]{`),
		"spacious":    cssDecls(t, css, `[data-density="spacious"]{`),
	}
	seen := map[string]string{}
	for name, d := range rows {
		key := d["--hfy-fs"] + "|" + d["--hfy-gap"] + "|" + d["--hfy-pad-y"]
		if other, dup := seen[key]; dup {
			t.Errorf("density %s and %s set the same font, gap and padding", name, other)
		}
		seen[key] = name
	}
	if rows["comfortable"]["--hfy-fs"] != "15px" || rows["spacious"]["--hfy-fs"] != "16px" || rows["compact"]["--hfy-fs"] != "14px" {
		t.Errorf("density font sizes: %v", seen)
	}
	palette := cssDecls(t, css, `[data-layout="whatsapp"]{`)
	for _, tok := range []string{"--hfy-fs", "--hfy-gap", "--hfy-gap-tight", "--hfy-pad-y", "--hfy-pad-x", "--hfy-lh"} {
		if _, ok := palette[tok]; ok {
			t.Errorf("whatsapp must not fix %s: density owns it", tok)
		}
	}
	runLayoutNode(t, `
		boot({layout: 'whatsapp', density: 'spacious'});
		eq(el.dataset.density, 'spacious', 'density applied');
	`)
}

// Lang and dir row: the page follows them, and the RTL mirror covers the whatsapp layout.
func TestEmbedWhatsappLangDir(t *testing.T) {
	css := readUICSS(t)
	if !strings.Contains(css, `[dir="rtl"]:is([data-layout="bubbles"],[data-layout="whatsapp"]) .is-head .hfy-bubble::before`) {
		t.Error("the RTL tail mirror must cover whatsapp")
	}
	runLayoutNode(t, `
		boot({layout: 'whatsapp', lang: 'ar', dir: 'auto'});
		eq(el.lang, 'ar', 'lang from config');
		eq(el.dir, 'rtl', 'dir follows an RTL lang');
		boot({layout: 'whatsapp', lang: 'en', dir: 'rtl'});
		eq(el.dir, 'rtl', 'dir forced to rtl');
		boot({layout: 'whatsapp', lang: 'ar', dir: 'ltr'});
		eq(el.dir, 'ltr', 'dir forced to ltr');
	`)
}

// Tool calls row: hidden, summary and details each keep their meaning under whatsapp.
func TestEmbedWhatsappToolCalls(t *testing.T) {
	runLayoutNode(t, `
		function env(type, data, actor, parent, id) {
			return {id: id || (env.seq = (env.seq || 1000) + 1), type: type, parent: parent, actor: actor, data: data, created_at: '2026-01-02T10:00:00Z'};
		}
		var agent = {type: 'agent', ref: 'main', uuid: 'agent-main'}, u1 = {type: 'external', ref: 'u1', uuid: 'u1'};
		function toolBlock(mode) {
			var c = boot({layout: 'whatsapp', tool_calls: mode});
			api.setClient({viewer: {uuid: 'u1'}});
			c.fold(env('turn', {agent: ''}, u1, 0, 101), false);
			c.fold(env('message.text', {text: 'hi'}, u1, 101), false);
			c.fold(env('tool_call', {call_id: '101-t', name: 'shop.lookup', arguments: '{}'}, agent, 101), false);
			c.fold(env('tool_result', {call_id: '101-t', content: 'ok'}, agent, 101), false);
			c.fold(env('turn.state', {state: 'completed'}, agent, 101), false);
			c.relayout();
			return c.turns[0].blocks.filter(function (b) { return b.kind === 'tool'; })[0];
		}
		var hidden = toolBlock('hidden');
		truthy(!hidden.showSummary && !hidden.showDetails && /is-hidden/.test(hidden.rowCls), 'hidden: no row, block hidden');
		var summary = toolBlock('summary');
		truthy(summary.showSummary && !summary.showDetails, 'summary: friendly row only');
		var details = toolBlock('details');
		truthy(details.showDetails && !details.showSummary, 'details: arguments and results');
	`)
}

// Accent row (explicit): the set accent drives send, links, focus, approvals and
// the pill, and its tint reaches the own bubbles only (contrast in its own test).
func TestEmbedWhatsappAccentExplicit(t *testing.T) {
	css := readUICSS(t)
	for _, rule := range []string{`.hfy-send{`, `.hfy-jump{`, `.hfy-btn-approve{`} {
		if got := cssDecls(t, css, rule)["background"]; got != "var(--hfy-accent)" {
			t.Errorf("%s background = %q, the accent must drive it", rule, got)
		}
	}
	for _, want := range []string{
		"outline:2px solid var(--hfy-accent);outline-offset:2px}",
		"--hfy-accent-text:color-mix(in srgb,var(--hfy-accent) 58%,#000)",
		"--hfy-ring:color-mix(in srgb,var(--hfy-accent) 40%,transparent)",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("accent must drive %q", want)
		}
	}
	runLayoutNode(t, `
		var accents = ['#ffee00', '#111111', '#6366f1', '#e11d48', '#00a884', '#ffffff'];
		accents.forEach(function (a) {
			boot({layout: 'whatsapp', accent: a, accent_set: true});
			eq(el.style.props['--hfy-accent'], a, 'accent ' + a + ' drives the UI');
			truthy(el.style.props['--hfy-on-accent'], 'on-accent set for ' + a);
			eq(el.dataset.accentSet, '', 'accent-set flag for ' + a);
		});
	`)
}

// Accent row (unset): whatsapp keeps its own green, and the own bubbles stay on
// the palette's #d9fdd3 (light) and #005c4b (dark).
func TestEmbedWhatsappAccentUnset(t *testing.T) {
	css := readUICSS(t)
	light := cssDecls(t, css, `[data-layout="whatsapp"]{`)
	dark := cssDecls(t, css, `[data-layout="whatsapp"][data-theme="dark"]{`)
	if light["--hfy-accent"] != "var(--hfy-wa-send)" || light["--hfy-wa-send"] != "#00a884" {
		t.Errorf("unset accent must be the whatsapp green: %v", light)
	}
	if light["--hfy-wa-out"] != "#d9fdd3" || dark["--hfy-wa-out"] != "#005c4b" {
		t.Errorf("unset own bubbles: light %q dark %q", light["--hfy-wa-out"], dark["--hfy-wa-out"])
	}
	if !strings.Contains(css, `[data-layout="whatsapp"][data-accent-set]{--hfy-wa-out:color-mix(in srgb,var(--hfy-accent) 15%,#fff)}`) {
		t.Error("an explicit accent must tint the own bubbles")
	}
	runLayoutNode(t, `
		boot({layout: 'whatsapp'});
		eq(el.style.props['--hfy-accent'], undefined, 'no inline accent when unset');
		eq(el.dataset.accentSet, undefined, 'no accent-set flag when unset');
		boot({layout: 'whatsapp', accent: '#6366f1', accent_set: false});
		eq(el.style.props['--hfy-accent'], undefined, 'an indigo default without accent_set is unset');
	`)
}

// Accent row (indigo): an explicit #6366f1 is a set accent, same as any other.
func TestEmbedWhatsappIndigoExplicit(t *testing.T) {
	runLayoutNode(t, `
		boot({layout: 'whatsapp', accent: '#6366f1', accent_set: true});
		eq(el.style.props['--hfy-accent'], '#6366f1', 'explicit indigo drives the UI');
		eq(el.dataset.accentSet, '', 'explicit indigo sets the flag');
	`)
}

// Accent row (contrast): bubble text on the own tint is at least 4.5:1 in both
// themes for every accent, computed with the stylesheet's own percentages.
func TestEmbedWhatsappAccentContrast(t *testing.T) {
	css := readUICSS(t)
	lightPct := regexp.MustCompile(`\[data-layout="whatsapp"\]\[data-accent-set\]\{--hfy-wa-out:color-mix\(in srgb,var\(--hfy-accent\) (\d+)%,#fff\)\}`).FindStringSubmatch(css)
	darkPct := regexp.MustCompile(`\[data-layout="whatsapp"\]\[data-theme="dark"\]\[data-accent-set\]\{--hfy-wa-out:color-mix\(in srgb,var\(--hfy-accent\) (\d+)%,#202c33\)\}`).FindStringSubmatch(css)
	if lightPct == nil || darkPct == nil {
		t.Fatal("own-bubble tint rules not found")
	}
	lp, _ := strconv.ParseFloat(lightPct[1], 64)
	dp, _ := strconv.ParseFloat(darkPct[1], 64)
	lightText := rgbOf(t, cssDecls(t, css, `[data-layout="whatsapp"]{`)["--hfy-wa-text"])
	darkText := rgbOf(t, cssDecls(t, css, `[data-layout="whatsapp"][data-theme="dark"]{`)["--hfy-wa-text"])
	for _, accent := range []string{"#ffee00", "#fef9c3", "#ffffff", "#111111", "#1f2937", "#6366f1", "#e11d48", "#00a884"} {
		a := rgbOf(t, accent)
		if got := contrast(lightText, mix(a, lp/100, rgbOf(t, "#fff"))); got < 4.5 {
			t.Errorf("light own bubble, accent %s: text contrast %.2f < 4.5", accent, got)
		}
		if got := contrast(darkText, mix(a, dp/100, rgbOf(t, "#202c33"))); got < 4.5 {
			t.Errorf("dark own bubble, accent %s: text contrast %.2f < 4.5", accent, got)
		}
	}
}

// Neutral surfaces row: wallpaper, incoming, text, timestamps, composer bar and
// pill never read the accent; only the own-bubble tint does.
func TestEmbedWhatsappNeutralSurfaces(t *testing.T) {
	css := readUICSS(t)
	base := cssDecls(t, css, `[data-layout="whatsapp"]{`)
	dark := cssDecls(t, css, `[data-layout="whatsapp"][data-theme="dark"]{`)
	for name, decls := range map[string]map[string]string{"light": base, "dark": dark} {
		for prop, val := range decls {
			if strings.Contains(val, "--hfy-accent") && prop != "--hfy-accent" && prop != "--hfy-on-accent" {
				t.Errorf("%s: neutral token %s reads the accent: %s", name, prop, val)
			}
		}
	}
	for _, sel := range []string{`[data-layout="whatsapp"][data-accent-set]{`, `[data-layout="whatsapp"][data-theme="dark"][data-accent-set]{`} {
		if decls := cssDecls(t, css, sel); len(decls) != 1 || decls["--hfy-wa-out"] == "" {
			t.Errorf("%s must change only the own tint, got %v", sel, decls)
		}
	}
	for _, want := range []string{
		`:is([data-layout="bubbles"],[data-layout="whatsapp"]) .hfy-messages{background-color:var(--hfy-wall)`,
		`[data-layout="whatsapp"] .hfy-composer-wrap{background:var(--hfy-wa-bar)}`,
		`[data-layout="whatsapp"] .hfy-pill{background:var(--hfy-wa-pill)}`,
	} {
		if !strings.Contains(css, want) {
			t.Errorf("neutral surface rule missing: %s", want)
		}
	}
}

// The bubbles layout keeps the golden rules byte for byte.
func TestEmbedBubblesRulesUnchanged(t *testing.T) {
	want, err := os.ReadFile("testdata/bubbles_rules.golden")
	if err != nil {
		t.Fatal(err)
	}
	if got := bubblesRules(readUICSS(t)); got != string(want) {
		t.Errorf("bubbles rules changed; the bubbles layout must not change:\n%s", got)
	}
}

// The palette is a fixed set of constants: pin the light and dark values so a
// retune is a deliberate change here and in the stylesheet together.
func TestEmbedWhatsappPalette(t *testing.T) {
	css := readUICSS(t)
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
		decls := cssDecls(t, css, tc.selector)
		for name, want := range tc.want {
			if decls[name] != want {
				t.Errorf("%s %s = %q, want %q", tc.selector, name, decls[name], want)
			}
		}
	}
}

// rgbOf parses a #rgb or #rrggbb colour into channel values 0-255.
func rgbOf(t *testing.T, hex string) [3]float64 {
	t.Helper()
	h := strings.TrimPrefix(strings.TrimSpace(hex), "#")
	if len(h) == 3 {
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	var out [3]float64
	for i := range 3 {
		v, err := strconv.ParseUint(h[2*i:2*i+2], 16, 8)
		if err != nil {
			t.Fatalf("colour %q: %v", hex, err)
		}
		out[i] = float64(v)
	}
	return out
}

// mix is color-mix(in srgb, a p, b) for opaque colours.
func mix(a [3]float64, p float64, b [3]float64) [3]float64 {
	var out [3]float64
	for i := range 3 {
		out[i] = a[i]*p + b[i]*(1-p)
	}
	return out
}

func luminance(c [3]float64) float64 {
	lin := func(v float64) float64 {
		v /= 255
		if v <= 0.03928 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(c[0]) + 0.7152*lin(c[1]) + 0.0722*lin(c[2])
}

// contrast is the WCAG 2.x contrast ratio.
func contrast(a, b [3]float64) float64 {
	x, y := luminance(a), luminance(b)
	if x < y {
		x, y = y, x
	}
	return (x + 0.05) / (y + 0.05)
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
