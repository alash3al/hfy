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

		async function conflictStopsTryingThatToken() {
			const conflict = () => ({ status: 409, body: { error: 'grant is not renewable' } });
			// the host keeps handing over the same token: after the first 409 it is not tried again
			const w = rig({ exchangeReply: conflict, hostTokens: ['T1', 'T1', 'T1'] });
			await w.c.connect();
			await fire();
			assert.equal(w.exchanges, 1);
			assert.deepEqual(w.hostCalls, ['initial', 'expired'], 'falls back to the host');
			await fire();
			assert.equal(w.exchanges, 1, 'a 409 is remembered for that token: no exchange on every refresh');
			assert.deepEqual(w.hostCalls, ['initial', 'expired', 'expired']);
			// new tokens from the host are learned afresh: one wasted request per refresh, no more
			const v = rig({ exchangeReply: conflict, hostTokens: ['T1', 'H2', 'H3', 'H4'] });
			await v.c.connect();
			await fire();
			assert.match(v.live()[0].url, /token=H2/);
			await fire();
			assert.equal(v.exchanges, 2);
			assert.deepEqual(v.exchangeCalls().map((f) => f.bearer), ['T1', 'H2']);
			assert.match(v.live()[0].url, /token=H3/);
		}

		async function aCappedGrantDoesNotDisableTheNextOne() {
			// renewable once (T2), then 409 at its cap, then the host hands over a new renewable grant
			let n = 0;
			const w = rig({ hostTokens: ['T1', 'H2', 'H3'], exchangeReply: () => {
				n++;
				if (n === 2) return { status: 409, body: { error: 'grant is not renewable' } };
				return { status: 200, body: { token: 'X' + n, expires_at: inSecs(60), renewable: true } };
			} });
			await w.c.connect();
			await fire(); // T1 -> X1, learned renewable
			assert.match(w.live()[0].url, /token=X1/);
			await fire(); // 409: capped, so the host
			assert.deepEqual(w.hostCalls, ['initial', 'expired']);
			assert.match(w.live()[0].url, /token=H2/);
			await fire(); // the host's new grant IS exchanged
			assert.equal(w.exchanges, 3);
			assert.deepEqual(w.exchangeCalls().map((f) => f.bearer), ['T1', 'X1', 'H2']);
			assert.match(w.live()[0].url, /token=X3/);
			assert.deepEqual(w.hostCalls, ['initial', 'expired']);
			// and the one after a grant that never renewed
			let m = 0;
			const v = rig({ hostTokens: ['T1', 'H2', 'H3'], exchangeReply: () => (++m === 1
				? { status: 409, body: { error: 'x' } } : { status: 200, body: { token: 'Y' + m, expires_at: inSecs(60), renewable: true } }) });
			await v.c.connect();
			await fire(); // T1: 409 -> host H2
			await fire(); // H2: exchanged
			assert.match(v.live()[0].url, /token=Y2/);
			assert.deepEqual(v.hostCalls, ['initial', 'expired']);
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

		const lastMs = () => timers.filter((t) => !t.off).map((t) => t.ms);
		const fireLast = async () => { const t = timers.filter((x) => !x.off).pop(); t.off = true; t.fn(); await flush(); };
		const tooEarly = (secs) => () => ({ status: 429, body: { error: 'too early to renew', retry_after: secs } });

		async function tooEarlyKeepsTheTokenAndWaits() {
			const w = rig({ exchangeReply: tooEarly(40) });
			await w.c.connect();
			const states = [];
			w.c.onState((st) => states.push(st));
			await fire(); // the refresh timer: 429
			assert.equal(w.exchanges, 1);
			assert.deepEqual(w.hostCalls, ['initial'], 'a 429 does not ask the host');
			assert.equal(w.sources.length, 1, 'the stream stands: the token did not change');
			assert.equal(w.c.state, 'live');
			assert.deepEqual(states.filter((x) => x !== 'refreshing' && x !== 'live'), []);
			const next = lastMs();
			assert.equal(next.length, 1, 'refresh is rescheduled');
			assert.ok(next[0] >= 40000 - 100, 'not before retry_after: ' + next[0]);
			await w.c.send('still works');
			assert.equal(authOf(w, '/v1/conversation/messages').pop(), 'T1');
			// visibility inside the wait window does nothing, even when the token is due
			const v = rig({ hostTtl: 10, exchangeReply: tooEarly(40) });
			await v.c.connect();
			v.visible(); await flush();
			assert.equal(v.exchanges, 1);
			v.visible(); await flush();
			assert.equal(v.exchanges, 1, 'no second exchange inside the retry window');
			assert.deepEqual(v.hostCalls, ['initial']);
		}

		async function aSkewedClockDoesNotHammer() {
			// the expiry is ten minutes in the past by this client's clock
			const w = rig({ hostTtl: -600, exchangeReply: tooEarly(20) });
			await w.c.connect();
			assert.ok(lastMs()[0] >= 5000, 'an expiry in the past is not a 1 s loop: ' + lastMs()[0]);
			for (let i = 1; i <= 3; i++) {
				await fire();
				assert.equal(w.exchanges, i, 'one exchange per timer');
				assert.ok(lastMs()[0] >= 20000 - 100, 'the next one waits out the retry window: ' + lastMs()[0]);
			}
			assert.deepEqual(w.hostCalls, ['initial']);
			// an expiry years away does not overflow setTimeout (which would fire at once)
			const far = rig({ hostTtl: 60 * 60 * 24 * 365 * 5 });
			await far.c.connect();
			assert.ok(lastMs()[0] > 1000 && lastMs()[0] <= 2147483647, 'timer ' + lastMs()[0]);
		}

		async function legitTimingStaysAboveAQuarter() {
			// a 60 s grant: the server needs a quarter (15 s) of age; a client clock up to
			// half a lifetime fast must still ask no sooner
			for (let fast = 0; fast <= 30; fast++) {
				const w = rig({ hostTtl: 60 - fast });
				await w.c.connect();
				assert.ok(lastMs()[0] >= 15000 - 50, fast + ' s fast: refresh in ' + lastMs()[0] + ' ms');
			}
		}

		async function reconnectExchangesAGoodTokenBeforeAskingTheHost() {
			const w = rig();
			await w.c.connect();
			w.sources[0].readyState = 2; w.sources[0].onerror(); // the stream dropped
			await fireLast(); // the reconnect timer
			assert.deepEqual(w.exchangeCalls().map((f) => f.bearer), ['T1']);
			assert.deepEqual(w.hostCalls, ['initial'], 'a good token is exchanged, not re-asked');
			assert.match(w.live()[0].url, /token=T2/);
			const e = rig({ hostTtl: -5 }); // a token that is past its expiry: only the host can help
			await e.c.connect();
			e.sources[0].readyState = 2; e.sources[0].onerror();
			await fireLast();
			assert.equal(e.exchanges, 0);
			assert.deepEqual(e.hostCalls, ['initial', 'unauthorized']);
			const d = rig({ exchangeReply: tooEarly(30) }); // a 429 keeps the token and the stream reconnects with it
			await d.c.connect();
			d.sources[0].readyState = 2; d.sources[0].onerror();
			await fireLast();
			assert.deepEqual(d.hostCalls, ['initial']);
			assert.match(d.live()[0].url, /token=T1/);
		}

		async function noHook() {
			const c = new HfyClient({ api: 'http://x', fetch: async () => ({ status: 200, text: async () => '{}' }), EventSource: function () { this.close = () => {}; },
				onTokenNeeded: async () => 'tok' });
			await c.connect();
		}

		(async () => {
			for (const t of [exchangeBeforeHost, conflictStopsTryingThatToken, aCappedGrantDoesNotDisableTheNextOne, failureFallsBackToTheHost, atTheCap, staleTokenRetriesWithoutTheHost,
				deadTokenAsksTheHost, oneExchangeAtATime, visibility, tooEarlyKeepsTheTokenAndWaits, aSkewedClockDoesNotHammer,
				legitTimingStaysAboveAQuarter, reconnectExchangesAGoodTokenBeforeAskingTheHost, noHook]) {
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
