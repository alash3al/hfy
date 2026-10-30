package httpapi_test

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
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

// The card module is pure JavaScript: exercised under node with a DOM stub.
func TestEmbedCardJS(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("no node")
	}
	src, err := os.ReadFile("embed/ui.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	i := strings.Index(s, "function hfyCardData")
	if i < 0 {
		t.Fatal("cards section not found")
	}
	s = s[i:]
	section, _, ok := strings.Cut(s, "/* ---- markdown")
	if !ok {
		t.Fatal("cards section not terminated")
	}
	// a JS string literal holding the section plus its export footer
	quotedSection := strconv.Quote(section + "; return {hfyCardData: hfyCardData, hfyCardEl: hfyCardEl};")
	script := `
		function stubReader(tag) {
			return function () {
				var o = {tag: tag, children: [], attributes: {}, style: {}, className: '', alt: '', loading: '', textContent: '', disabled: false, events: {},
					setAttribute: function (k, v) { o.attributes[k] = String(v) },
					appendChild: function (x) { o.children.push(x); return x },
					replaceChildren: function () { o.children.length = 0 },
					addEventListener: function (type, fn) { (o.events[type] = o.events[type] || []).push(fn) }};
				Object.defineProperty(o, 'src', {get: function () { return o._src || '' }, set: function (v) { o._src = v }});
				Object.defineProperty(o, 'href', {get: function () { return o._href || '' }, set: function (v) { o._href = v }});
				return o;
			};
		}
		var host = {createElement: stubReader('div'), createDocumentFragment: stubReader('frag')};
		var sends = [], actions = [];
		var g = {
			hfyClient: {send: function (t) { sends.push(t); return Promise.resolve() }},
			hfySendToHost: function (type, p) { actions.push([type, p]) }
		};
		var api = new Function('document', 'hfySafeUrl', 'hfyClient', 'hfySendToHost', ` + quotedSection + `)(host,
			function (s) { return /^https:/i.test(s) ? s : '' }, g.hfyClient, g.hfySendToHost);
		function eq(a, b) { if (JSON.stringify(a) !== JSON.stringify(b)) throw new Error('eq failed: ' + JSON.stringify(a) + ' != ' + JSON.stringify(b)) }
		function truthy(v, m) { if (!v) throw new Error('falsy: ' + m) }
		var card = {v: 1, type: 'card', title: 'Margherita', badge: 'EUR 12', body: 'hot', kv: [{label: 'size', value: 'L'}],
			buttons: [{link: {label: 'menu', url: 'https://x.example/m'}}, {send: {label: 'Order it'}}, {host: {label: 'pay', name: 'pay_host', payload: {amount: 12}}}]};
		eq(api.hfyCardData(null), null);
		eq(api.hfyCardData({type: 'nope'}), null);
		eq(api.hfyCardData({type: 'card'}), null);
		eq(api.hfyCardData({type: 'carousel'}), null);
		eq(api.hfyCardData({type: 'carousel', cards: []}), null);
		eq(api.hfyCardData({type: 'carousel', cards: [card, {type: 'card', title: 'T'}]}).length, 2);
		var el = api.hfyCardEl(card, host, 'c1', true);
		eq(el.children[0].children[0].textContent, 'Margherita');
		var btns = el.children[el.children.length - 1].children;
		eq(btns.length, 3);
		eq(btns[0].children.length, 0);
		eq(btns[0].href, 'https://x.example/m');
		eq(btns[1].textContent, 'Order it');
		truthy(!btns[1].disabled, 'send live');
		btns[1].events.click[0]();
		eq(sends[0], 'Order it');
		btns[2].events.click[0]();
		eq(actions[0][0], 'action');
		eq(actions[0][1].name, 'pay_host');
		eq(actions[0][1].call_id, 'c1');
		var e2 = api.hfyCardEl(card, host, 'c1', false);
		var b2 = e2.children[e2.children.length - 1].children;
		truthy(b2[1].disabled && b2[2].disabled, 'muted buttons');
		b2[1].events.click[0]();
		eq(sends.length, 1);
		var e3 = api.hfyCardEl({v: 1, type: 'card', title: 'T', buttons: [{link: {label: 'no', url: 'http://x.example/a'}}]}, host, 'c1', true);
		e3.children.forEach(function (ch) { eq(ch.className.indexOf('btns'), -1, 'insecure link dropped: no button row should exist') });
		// labels are text, never markup: <script> stays characters
		var sneaky = api.hfyCardEl({v: 1, type: 'card', title: '<script>alert(1)</script>', body: '<img src=x onerror=alert(2)>'}, host, 'c1', true);
		eq(sneaky.children[0].children[0].textContent, '<script>alert(1)</script>');
		truthy(!sneaky.children.some(function (ch) { return ch.tag === 'script' || ch.tag === 'img' }), 'no elements from markup');
		// send sends exactly the label, nothing prepended
		var mini = api.hfyCardEl({v: 1, type: 'card', title: 'T', buttons: [{send: {label: 'Order it'}}]}, host, 'c1', true);
		mini.children[mini.children.length - 1].children[0].events.click[0]();
		eq(sends[1], 'Order it');
		`
	out, err := exec.Command("node", "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
