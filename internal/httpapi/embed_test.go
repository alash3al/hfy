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

// A stop lands as a tool_result plus a terminal turn state, never as an
// approval.decided: the pending ask settles to "Stopped", the sheet closes
// and the pill disappears, on the live-event path and the history path alike.
func TestEmbedFoldStoppedSettlesAsks(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("no node")
	}
	src, err := os.ReadFile("embed/ui.js")
	if err != nil {
		t.Fatal(err)
	}
	quoted := strconv.Quote(string(src) + `
;return {hfyEmbed: window.hfyEmbed, setClient: function (v) { hfyClient = v }};`)
	driver := `
		var win = { addEventListener: function () {} };
		win.parent = win;
		var doc = { addEventListener: function () {}, querySelector: function () { return null },
			querySelectorAll: function () { return [] } };
		var nav = { language: 'en' };
		var api = new Function('window', 'document', 'navigator', 'requestAnimationFrame', ` + quoted + `)(win, doc, nav, function () {});
		var agent = { type: 'agent', ref: 'main', uuid: 'agent-main' },
			u1 = { type: 'external', ref: 'u1', uuid: 'u1' };
		function env(type, data, actor, parent, id) {
			return { id: id || (env.seq = (env.seq || 1000) + 1), type: type, parent: parent, actor: actor,
				data: data, created_at: '2026-01-02T10:00:00Z' };
		}
		function blockOf(c, callId) {
			for (var i = 0; i < c.turns.length; i++) {
				var bs = c.turns[i].blocks;
				for (var j = 0; j < bs.length; j++) if (bs[j].kind === 'tool' && bs[j].callId === callId) return bs[j];
			}
			throw new Error('no tool block ' + callId);
		}
		function truthy(v, m) { if (!v) throw new Error('falsy: ' + m) }
		function falsy(v, m) { if (v) throw new Error('truthy: ' + m + ' ' + JSON.stringify(v)) }
		function settled(c, b, m) {
			falsy(b.showApproval, m + ': approval cleared');
			falsy(b.showPendingOwn, m + ': no row awaits the viewer');
			if (b.hasQuestions) {
				truthy(b.askState === 'stopped', m + ': ask settled to stopped');
				truthy(b.askStateText === 'Stopped', m + ': row label says Stopped');
				falsy(b.askCanOpen, m + ': no Open button');
			}
		}
		function drive(isH) {
			var c = api.hfyEmbed(); api.setClient({ viewer: { uuid: 'u1' } });
			// the wire sequence a stopped ask produces: turn, text, call,
			// questions held (approval.requested), the one tool_result, then stopped
			c.fold(env('turn', { agent: '' }, u1, 0, 101), isH);
			c.fold(env('message.text', { text: 'ask me' }, u1, 101), isH);
			c.fold(env('turn.state', { state: 'running' }, agent, 101), isH);
			c.fold(env('tool_call', { call_id: '101-q', name: 'hfy.ask_user', arguments: {questions: [{id: 'a', text: 'How?', type: 'single', options: [{value: 'x', label: 'X'}]}]} }, agent, 101), isH);
			c.fold(env('approval.requested', { call_id: '101-q', questions: {questions: [{id: 'a', text: 'How?', type: 'single', options: [{value: 'x', label: 'X'}]}]} }, agent, 101), isH);
			c.relayout();
			var q = blockOf(c, '101-q');
			truthy(q.showApproval && q.askState === 'pending', isH + ': ask is pending before the stop');
			truthy(c.pendingOwn.length === 1 && c.sheetOpen, isH + ': sheet is open');
			truthy(c.showActionPill || c.pendingOwn.length, isH + ': action pill armed');
			// also a plain approval in the same turn: same settle, no questions
			c.fold(env('tool_call', { call_id: '101-p', name: 'shop.pay', arguments: '{}' }, agent, 101), isH);
			c.fold(env('approval.requested', { call_id: '101-p' }, agent, 101), isH);
			c.relayout();
			truthy(c.pendingOwn.length === 2, isH + ': plain approval joins the sheet');
			// the stop: tool_result (held text) then the terminal state
			c.fold(env('tool_result', { call_id: '101-q', content: 'your pizza\n\nThe user stopped before answering.' }, agent, 101), isH);
			settled(c, blockOf(c, '101-q'), isH + ': ask');
			// the plain approval settles with the turn state, having no result of its own
			c.fold(env('turn.state', { state: 'stopped' }, agent, 101), isH);
			settled(c, blockOf(c, '101-p'), isH + ': plain approval');
			var p = blockOf(c, '101-p');
			truthy(p.status === 'interrupted', isH + ': plain approval shows Interrupted');
			truthy(c.turns[0].state === 'stopped', isH + ': turn stopped');
			falsy(c.sheetOpen, isH + ': sheet closed');
			falsy(c.showActionPill, isH + ': pill gone');
			truthy(c.pendingOwn.length === 0 && c.pendingCount === '0', isH + ': nothing pending');
			// a late real decision still wins (never dropped by the settled flag)
			c.fold(env('approval.decided', { call_id: '101-q', approve: true, answers: {a: 'x'} }, agent, 101), isH);
			var qq = blockOf(c, '101-q');
			truthy(qq.decision === 'allowed' && qq.askState === 'answered', isH + ': late decision overrides');
			return true;
		}
		truthy(drive(false), 'live path');
		truthy(drive(true), 'history path');
	`
	out, err := exec.Command("node", "-e", driver).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}

// The fold pass is pure JavaScript too: card buttons must settle from the
// final folded state, on the live-event path and the history path alike.
func TestEmbedFoldCardLiveness(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("no node")
	}
	src, err := os.ReadFile("embed/ui.js")
	if err != nil {
		t.Fatal(err)
	}
	quoted := strconv.Quote(string(src) + `
;return {hfyEmbed: window.hfyEmbed, setClient: function (v) { hfyClient = v }};`)
	driver := `
		var win = { addEventListener: function () {} };
		win.parent = win;
		var doc = { addEventListener: function () {}, querySelector: function () { return null },
			querySelectorAll: function () { return [] } };
		var nav = { language: 'en' };
		var api = new Function('window', 'document', 'navigator', 'requestAnimationFrame', ` + quoted + `)(win, doc, nav, function () {});
		var agent = { type: 'agent', ref: 'main', uuid: 'agent-main' },
			u1 = { type: 'external', ref: 'u1', uuid: 'u1' },
			u2 = { type: 'external', ref: 'u2', uuid: 'u2' };
		function env(type, data, actor, parent, id) {
			return { id: id || (env.seq = (env.seq || 1000) + 1), type: type, parent: parent, actor: actor,
				data: data, created_at: '2026-01-02T10:00:00Z' };
		}
		function card() { return { v: 1, type: 'card', title: 'Margherita', body: '12 EUR' }; }
		// One full turn: own, card-carrying, ending completed; the turn event
		// itself gets the id, its children parent on it.
		function ownCardTurn(c, isH, id) {
			c.fold(env('turn', { agent: '' }, u1, 0, id), isH);
			c.fold(env('message.text', { text: 'buy pizza' }, u1, id), isH);
			c.fold(env('tool_call', { call_id: id + '-c', name: 'shop.pay', arguments: '{}' }, agent, id), isH);
			c.fold(env('tool_result', { call_id: id + '-c', content: 'ok', ui: card() }, agent, id), isH);
			c.fold(env('turn.state', { state: 'running' }, agent, id), isH);
			c.fold(env('turn.state', { state: 'completed' }, agent, id), isH);
		}
		function blockOf(c, callId) {
			for (var i = 0; i < c.turns.length; i++) {
				var bs = c.turns[i].blocks;
				for (var j = 0; j < bs.length; j++) if (bs[j].kind === 'tool' && bs[j].callId === callId) return bs[j];
			}
			throw new Error('no tool block ' + callId);
		}
		function truthy(v, m) { if (!v) throw new Error('falsy: ' + m) }
		function falsy(v, m) { if (v) throw new Error('truthy: ' + m + ' ' + JSON.stringify(v)) }
		function drive(isH) {
			var c = api.hfyEmbed(); api.setClient({ viewer: { uuid: 'u1' } });
			// T1: own, completed, newest => the card's send/host buttons are live.
			ownCardTurn(c, isH, 101);
			var t = c.turns[0];
			truthy(t.own, 'own turn');
			truthy(t.state === 'completed', 'completed turn');
			truthy(blockOf(c, '101-c').cardButtonsLive, isH + ': card live on the own completed newest turn');
			// T2: the same kind of turn, but still running => disabled, and it
			// demotes the older card too.
			c.fold(env('turn', { agent: '' }, u1, 0, 201), isH);
			c.fold(env('message.text', { text: 'another one' }, u1, 201), isH);
			c.fold(env('turn.state', { state: 'running' }, agent, 201), isH);
			c.fold(env('tool_call', { call_id: '201-c', name: 'shop.pay', arguments: '{}' }, agent, 201), isH);
			c.fold(env('tool_result', { call_id: '201-c', content: 'ok', ui: card() }, agent, 201), isH);
			falsy(blockOf(c, '201-c').cardButtonsLive, isH + ': card disabled while its turn runs');
			falsy(blockOf(c, '101-c').cardButtonsLive, isH + ': an older turn\'s card is disabled');
			// T2 completes => its card is live; T1 stays disabled.
			c.fold(env('turn.state', { state: 'completed' }, agent, 201), isH);
			truthy(blockOf(c, '201-c').cardButtonsLive, isH + ': card live once its turn completed');
			falsy(blockOf(c, '101-c').cardButtonsLive, isH + ': only the newest turn is live');
			// T3: newest but not the viewer's own => send/host disabled, and a
			// link button would still be clickable (its rendering never gates on liveness).
			c.fold(env('turn', { agent: '' }, u2, 0, 301), isH);
			c.fold(env('message.text', { text: 'from someone else' }, u2, 301), isH);
			c.fold(env('turn.state', { state: 'running' }, agent, 301), isH);
			c.fold(env('tool_call', { call_id: '301-c', name: 'shop.pay', arguments: '{}' }, agent, 301), isH);
			c.fold(env('tool_result', { call_id: '301-c', content: 'ok',
				ui: { v: 1, type: 'card', title: 'Capriciosa', body: '14 EUR',
					buttons: [{ link: { label: 'menu', url: 'https://cdn.example.com/m' } }] } }, agent, 301), isH);
			c.fold(env('turn.state', { state: 'completed' }, agent, 301), isH);
			falsy(blockOf(c, '301-c').cardButtonsLive, isH + ': a non-owner turn never lives');
			falsy(blockOf(c, '201-c').cardButtonsLive, isH + ': owner card demoted by the non-owner turn');
			// the 'assistant is typing' dots die with the turn: every turn is terminal here
			for (var t2 = 0; t2 < c.turns.length; t2++)
				falsy(c.turns[t2].showTyping, isH + ': no typing dots after all turns settle');
			return true;
		}
		truthy(drive(false), 'live path');
		truthy(drive(true), 'history path');
	`
	out, err := exec.Command("node", "-e", driver).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
