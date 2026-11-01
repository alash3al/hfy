package httpapi

import (
	"os"
	"os/exec"
	"strconv"
	"testing"
)

// The client's token chain under node with a fake fetch, EventSource and
// timers: exchange before the host, 409 stops it, a 401 whose token changed
// retries without the host, the visibility hook, one exchange at a time.
func TestEmbedClientExchangeChain(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("no node")
	}
	src, err := os.ReadFile("embed/client.js")
	if err != nil {
		t.Fatal(err)
	}
	driver := `
		const assert = require('node:assert/strict');
		const timers = [];
		globalThis.setTimeout = (fn, ms) => { const t = { fn, ms, off: false, unref() {} }; timers.push(t); return t; };
		globalThis.clearTimeout = (t) => { if (t) t.off = true; };
		const fire = async () => { // fire what is pending now
			const due = timers.splice(0).filter((t) => !t.off);
			for (const t of due) t.fn();
			await flush();
		};
		const flush = async () => { for (let i = 0; i < 20; i++) await new Promise((r) => setImmediate(r)); };
		const HfyClient = new Function('module', 'window', ` + strconv.Quote(string(src)) + ` + '\nreturn module.exports;')({ exports: {} });

		const inSecs = (s) => new Date(Date.now() + s * 1000).toISOString();
		function rig(opts) {
			opts = opts || {};
			timers.length = 0;
			const w = { hostCalls: [], fetches: [], sources: [], visible: null, exchanges: 0, hold: null };
			// server: tokens named by their state
			w.valid = new Set(['T1']);
			w.exchangeReply = opts.exchangeReply || ((n, bearer) => ({ status: 200, body: { id: 'chain', token: 'T' + (n + 1), expires_at: inSecs(60), renewable: true } }));
			w.fetch = async (url, init) => {
				const path = url.replace('http://x', '');
				const bearer = ((init.headers || {}).Authorization || '').replace('Bearer ', '');
				w.fetches.push({ path, bearer, method: init.method });
				let status = 200, body = {};
				if (path === '/v1/conversation/grant/exchange') {
					w.exchanges++;
					if (w.gate) await w.gate;
					const r = w.exchangeReply(w.exchanges, bearer);
					status = r.status; body = r.body;
					if (status === 200) w.valid.add(body.token);
				} else {
					if (w.hold && path === '/v1/conversation/messages') await w.hold(bearer);
					if (!w.valid.has(bearer)) status = 401, body = { error: 'invalid or expired token' };
					else if (path.startsWith('/v1/conversation/history')) body = { turns: [], viewer: {} };
					else body = { id: 'ok' };
				}
				const text = JSON.stringify(body);
				return { status, ok: status >= 200 && status < 300, text: async () => text };
			};
			w.ES = class { constructor(url) { this.url = url; this.readyState = 1; w.sources.push(this); }
				addEventListener() {} close() { this.readyState = 2; this.closed = true; } };
			let n = 0;
			const c = new HfyClient({
				api: 'http://x', fetch: w.fetch, EventSource: w.ES,
				onTokenNeeded: async (reason) => { w.hostCalls.push(reason); const tok = (opts.hostTokens || ['T1', 'H2', 'H3', 'H4'])[n++];
					w.valid.add(tok); return { token: tok, expires_at: inSecs(opts.hostTtl || 60) }; },
				onVisible: (cb) => { w.visible = cb; },
			});
			w.c = c;
			w.live = () => w.sources.filter((s) => !s.closed);
			w.exchangeCalls = () => w.fetches.filter((f) => f.path.endsWith('/grant/exchange'));
			return w;
		}
		const authOf = (w, path) => w.fetches.filter((f) => f.path === path).map((f) => f.bearer);

		async function exchangeBeforeHost() {
			const w = rig();
			await w.c.connect();
			assert.deepEqual(w.hostCalls, ['initial']);
			w.sources[0].onmessage({ data: JSON.stringify({ id: 'e1', type: 'turn' }) });
			await fire(); // the refresh timer
			assert.deepEqual(w.hostCalls, ['initial'], 'the host is not asked while the exchange works');
			assert.deepEqual(w.exchangeCalls().map((f) => f.bearer), ['T1']);
			assert.equal(w.sources.length, 2, 'the stream reconnects');
			assert.equal(w.sources[0].closed, true);
			assert.match(w.sources[1].url, /[?]token=T2&after=e1$/, 'resumes from the last event with the new token');
			assert.equal(w.c.state, 'live');
			await w.c.send('hi');
			assert.equal(authOf(w, '/v1/conversation/messages').pop(), 'T2');
			await fire(); // renewed again: T2 -> T3
			assert.deepEqual(w.exchangeCalls().map((f) => f.bearer), ['T1', 'T2']);
			assert.deepEqual(w.hostCalls, ['initial']);
		}

		async function conflictStopsTrying() {
			const w = rig({ exchangeReply: () => ({ status: 409, body: { error: 'grant is not renewable' } }), hostTokens: ['T1', 'H2', 'H3'] });
			await w.c.connect();
			await fire();
			assert.equal(w.exchanges, 1);
			assert.deepEqual(w.hostCalls, ['initial', 'expired'], 'falls back to the host');
			assert.match(w.live()[0].url, /token=H2/);
			await fire();
			assert.equal(w.exchanges, 1, 'a 409 is remembered: no more exchanges');
			assert.deepEqual(w.hostCalls, ['initial', 'expired', 'expired']);
		}

		async function failureFallsBackToTheHost() {
			for (const reply of [() => ({ status: 401, body: { error: 'x' } }), () => ({ status: 500, body: {} }), () => ({ status: 200, body: {} }),
				() => { throw new Error('offline'); }]) {
				const w = rig({ exchangeReply: reply });
				await w.c.connect();
				await fire();
				assert.deepEqual(w.hostCalls, ['initial', 'expired']);
				assert.match(w.live()[0].url, /token=H2/);
				assert.equal(w.c.state, 'live');
				await fire();
				assert.equal(w.exchanges, 2, 'not a 409: it is tried again');
			}
		}

		async function atTheCap() {
			let calls = 0;
			const w = rig({ exchangeReply: () => ({ status: 200, body: { token: 'T' + (++calls + 1), expires_at: inSecs(60), renewable: false } }) });
			await w.c.connect();
			await fire();
			assert.match(w.live()[0].url, /token=T2/);
			await fire();
			assert.equal(w.exchanges, 1, 'at the cap the host is next');
			assert.deepEqual(w.hostCalls, ['initial', 'expired']);
			await fire(); // the host's new grant may renew again
			assert.equal(w.exchanges, 2);
		}

		async function staleTokenRetriesWithoutTheHost() {
			const w = rig();
			await w.c.connect();
			let release;
			w.hold = (bearer) => bearer === 'T1' ? new Promise((r) => { release = r; }) : null; // the first send waits at the server
			const sending = w.c.send('hello');
			await flush();
			w.hold = null;
			w.valid.delete('T1'); // the server retired T1 in the meantime
			await fire(); // the timer swaps the token while the request is out
			assert.match(w.live()[0].url, /token=T2/);
			release();
			await sending;
			assert.deepEqual(authOf(w, '/v1/conversation/messages'), ['T1', 'T2']);
			assert.deepEqual(w.hostCalls, ['initial'], 'a 401 for a token that changed asks nobody');
		}

		async function deadTokenAsksTheHost() {
			const w = rig();
			await w.c.connect();
			w.valid.delete('T1');
			await w.c.send('hello');
			assert.deepEqual(w.hostCalls, ['initial', 'unauthorized']);
			assert.equal(w.exchanges, 0, 'a token the server refused cannot be exchanged');
			assert.deepEqual(authOf(w, '/v1/conversation/messages'), ['T1', 'H2']);
		}

		async function oneExchangeAtATime() {
			const w = rig();
			await w.c.connect();
			let open;
			w.gate = new Promise((r) => { open = r; });
			await fire(); // the timer's exchange waits at the server
			assert.equal(w.exchanges, 1);
			w.valid.delete('T1'); // the server refuses T1 from here on
			const sending = w.c.send('x'); // sent with T1, answered 401 while the exchange is out
			await flush();
			open();
			await sending;
			await flush();
			assert.equal(w.exchanges, 1, 'still one exchange');
			assert.deepEqual(w.hostCalls, ['initial']);
		}

		async function visibility() {
			const w = rig();
			await w.c.connect();
			assert.equal(typeof w.visible, 'function', 'the hook is registered');
			w.visible();
			await flush();
			assert.equal(w.exchanges, 0, 'a token with plenty of life is left alone');
			w.hostCalls.length = 0;
			const w2 = rig({ hostTtl: 10 }); // inside the skew already: due now
			await w2.c.connect();
			w2.visible();
			await flush();
			assert.equal(w2.exchanges, 1);
			assert.match(w2.live()[0].url, /token=T2/);
			assert.deepEqual(w2.hostCalls, ['initial']);
			w2.c.close();
			w2.visible();
			await flush();
			assert.equal(w2.exchanges, 1, 'a closed client ignores it');
		}

		async function noHook() {
			const c = new HfyClient({ api: 'http://x', fetch: async () => ({ status: 200, text: async () => '{}' }), EventSource: function () { this.close = () => {}; },
				onTokenNeeded: async () => 'tok' });
			await c.connect();
		}

		(async () => {
			for (const t of [exchangeBeforeHost, conflictStopsTrying, failureFallsBackToTheHost, atTheCap, staleTokenRetriesWithoutTheHost,
				deadTokenAsksTheHost, oneExchangeAtATime, visibility, noHook]) {
				try { await t(); } catch (e) { console.log('FAIL ' + t.name + ': ' + e.stack); process.exitCode = 1; }
			}
			if (!process.exitCode) console.log('ok');
		})();
	`
	out, err := exec.Command("node", "-e", driver).CombinedOutput()
	if err != nil || string(out) != "ok\n" { // a promise that never settles would exit silently
		t.Fatalf("node: %v\n%s", err, out)
	}
}
