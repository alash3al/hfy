'use strict';

// hfy client: history + SSE stream with token refresh. No DOM/storage.

var EVENT_TYPES = [
  'turn', 'turn.state', 'turn.cancel_requested', 'message.text', 'reasoning',
  'tool_call', 'tool_call.interrupted', 'tool_result', 'approval.requested',
  'approval.decided', 'model.usage', 'summary', 'text.delta', 'reasoning.delta',
];

// expires_at arrives as RFC3339; also tolerate Date, epoch s/ms, camelCase.
function parseExpiryMs(result) {
  if (!result || typeof result !== 'object') return null;
  var v = result.expires_at != null ? result.expires_at : result.expiresAt;
  if (v == null) return null;
  if (v instanceof Date) return isNaN(v.getTime()) ? null : v.getTime();
  if (typeof v === 'number') return v < 1e12 ? v * 1000 : v; // seconds vs ms
  if (typeof v === 'string') {
    var ms = Date.parse(v);
    if (!isNaN(ms)) return ms;
    var n = Number(v);
    if (!isNaN(n)) return n < 1e12 ? n * 1000 : n;
  }
  return null;
}

function timer(fn, ms) {
  var t = setTimeout(fn, ms);
  if (t && typeof t.unref === 'function') t.unref(); // Node tests must not hang
  return t;
}

class HfyClient {
  #api;
  #onTokenNeeded;
  #fetchFn;
  #EventSourceFn;
  #refreshSkewMs;
  #token = null;
  #expiresAtMs = null;
  #eventListeners = new Set();
  #stateListeners = new Set();
  // shortcut: unbounded Set; a long-lived client grows ~1 entry per event id.
  #seen = new Set();
  #lastEventId = null;
  #nextCursor = null;
  #viewer = null;
  #eventSource = null;
  #refreshTimer = null;
  #reconnectTimer = null;
  #reconnectAttempts = 0;
  #state = 'idle';
  #closed = false;

  constructor(opts) {
    opts = opts || {};
    if (typeof opts.onTokenNeeded !== 'function') {
      throw new TypeError('HfyClient: onTokenNeeded is required');
    }
    var api = opts.api;
    if (api == null) {
      api = (typeof location !== 'undefined' && location.origin) ? location.origin : '';
    }
    this.#api = String(api).replace(/\/+$/, '');
    this.#onTokenNeeded = opts.onTokenNeeded;
    this.#fetchFn = opts.fetch ||
      (typeof globalThis.fetch === 'function' ? globalThis.fetch.bind(globalThis) : null);
    this.#EventSourceFn = opts.EventSource || globalThis.EventSource || null;
    this.#refreshSkewMs = opts.refreshSkewMs != null ? opts.refreshSkewMs : 30000;
  }

  get state() { return this.#state; }
  get lastEventId() { return this.#lastEventId; }
  get hasOlder() { return this.#nextCursor != null; }
  // The grant's own actor ({uuid, type, ref}, the shape of event `actor`),
  // known once the first history page has loaded.
  get viewer() { return this.#viewer; }

  onEvent(fn) {
    this.#eventListeners.add(fn);
    return () => { this.#eventListeners.delete(fn); };
  }

  onState(fn) {
    this.#stateListeners.add(fn);
    return () => { this.#stateListeners.delete(fn); };
  }

  async connect() {
    if (this.#eventSource) return;
    this.#closed = false;
    this.#setState('connecting');
    try {
      await this.#fetchToken('initial');
      var page = await this.#loadHistory(null);
      // live_after wins over the last fired id: it is the server's end mark.
      if (page && page.live_after) this.#lastEventId = page.live_after;
      this.#openStream();
      this.#scheduleRefresh();
      this.#reconnectAttempts = 0;
      this.#setState('live');
    } catch (err) {
      if (!this.#closed) this.#setState('error');
      throw err;
    }
  }

  async loadOlder() {
    if (this.#nextCursor == null) return false;
    await this.#loadHistory(this.#nextCursor);
    return this.#nextCursor != null;
  }

  async send(text, attachments) {
    var body = { text: text };
    if (attachments !== undefined) body.attachments = attachments;
    return this.#request('/v1/conversation/messages', { method: 'POST', body: body });
  }

  async upload(file) {
    var slot = await this.#request('/v1/conversation/uploads', { method: 'POST', body: { mime: file.type, size: file.size } });
    if (!this.#fetchFn) throw new Error('no fetch implementation available');
    var headers = { 'Content-Type': file.type };
    var uploadUrl = slot.upload_url;
    if (slot.direct !== true) {
      headers.Authorization = 'Bearer ' + this.#token;
      uploadUrl = this.#api + slot.upload_url;
    }
    var res = await this.#fetchFn(uploadUrl, { method: 'PUT', headers: headers, body: file });
    var status = res.status != null ? res.status : (res.ok ? 200 : 0);
    if (status < 200 || status >= 300) throw new Error('upload failed: ' + status);
    var att = { file: slot.file, mime: file.type };
    if (file.name) att.name = file.name; // a Blob has no name
    return att;
  }

  async approve(callId, approve, result) {
    var body = { call_id: callId, approve: !!approve };
    if (result !== undefined) body.result = result;
    return this.#request('/v1/conversation/approvals', { method: 'POST', body: body });
  }

  async stop() {
    return this.#request('/v1/conversation/stop', { method: 'POST' });
  }

  close() {
    this.#closed = true;
    if (this.#refreshTimer) { clearTimeout(this.#refreshTimer); this.#refreshTimer = null; }
    if (this.#reconnectTimer) { clearTimeout(this.#reconnectTimer); this.#reconnectTimer = null; }
    this.#closeStream();
    this.#setState('ended');
  }

  // -- internals -----------------------------------------------------------

  #setState(state) {
    if (this.#state === state) return;
    this.#state = state;
    for (var fn of Array.from(this.#stateListeners)) fn(state);
  }

  #fire(envelope, isHistory) {
    if (this.#closed || !envelope) return;
    if (envelope.id != null) {
      if (this.#seen.has(envelope.id)) return;
      this.#seen.add(envelope.id);
      if (!isHistory) this.#lastEventId = envelope.id;
    }
    for (var fn of Array.from(this.#eventListeners)) fn(envelope, isHistory);
  }

  async #fetchToken(reason) {
    var result = await this.#onTokenNeeded(reason);
    var token = typeof result === 'string' ? result : result && result.token;
    if (!token) throw new Error('onTokenNeeded returned no token');
    this.#token = token;
    this.#expiresAtMs = parseExpiryMs(result);
  }

  async #loadHistory(before) {
    var url = '/v1/conversation/history?limit=50';
    if (before) url += '&before=' + encodeURIComponent(before);
    var page = await this.#request(url, { method: 'GET' });
    this.#nextCursor = page && page.next != null ? page.next : null;
    if (page && page.viewer && typeof page.viewer === 'object') this.#viewer = page.viewer; // before firing: listeners may ask

    // Turns arrive oldest-first; fire as-is, the UI sorts loadOlder pages in.
    var turns = (page && page.turns) || [];
    for (var turn of turns) {
      var events = (turn && turn.events) || [];
      for (var envelope of events) this.#fire(envelope, true);
    }
    return page;
  }

  async #request(path, opts) {
    return this.#requestOnce(path, opts, true);
  }

  async #requestOnce(path, opts, retryOn401) {
    if (!this.#fetchFn) throw new Error('no fetch implementation available');
    if (!this.#token && !this.#closed) await this.#fetchToken('initial');
    var init = { method: opts.method || 'GET', headers: {} };
    if (this.#token) init.headers.Authorization = 'Bearer ' + this.#token;
    if (opts.body !== undefined) {
      init.headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(opts.body);
    }
    var res = await this.#fetchFn(this.#api + path, init);
    var parsed = await readBody(res);
    var status = res.status != null ? res.status : (res.ok ? 200 : 0);
    if (status === 401 && retryOn401 && !this.#closed) {
      await this.#fetchToken('unauthorized'); // one refresh, one retry
      this.#scheduleRefresh();
      return this.#requestOnce(path, opts, false);
    }
    var ok = res.ok != null ? res.ok : status >= 200 && status < 300;
    if (!ok) {
      var msg = (parsed && parsed.error) || ('HTTP ' + status);
      var err = new Error(msg);
      err.status = status;
      throw err;
    }
    return parsed == null ? {} : parsed;
  }

  #scheduleRefresh() {
    if (this.#refreshTimer) { clearTimeout(this.#refreshTimer); this.#refreshTimer = null; }
    if (this.#closed || this.#expiresAtMs == null) return; // no expiry: no proactive refresh
    var remaining = this.#expiresAtMs - Date.now();
    var delay = Math.max(remaining - Math.min(this.#refreshSkewMs, remaining / 2), 1000);
    this.#refreshTimer = timer(() => {
      this.#refreshTimer = null;
      this.#refreshStream();
    }, delay);
  }

  async #refreshStream() {
    if (this.#closed) return;
    this.#setState('refreshing');
    try {
      await this.#fetchToken('expired');
      if (this.#closed) return;
      this.#openStream();
      this.#scheduleRefresh();
      this.#reconnectAttempts = 0;
      this.#setState('live');
    } catch (err) {
      if (!this.#closed) this.#scheduleReconnect();
    }
  }

  #openStream() {
    if (!this.#EventSourceFn) throw new Error('no EventSource implementation available');
    this.#closeStream();
    var url = this.#api + '/v1/conversation/events?token=' + encodeURIComponent(this.#token || '');
    url += '&after=' + encodeURIComponent(this.#lastEventId || '00000000-0000-0000-0000-000000000000');
    var es = new this.#EventSourceFn(url);
    this.#eventSource = es;
    var errors = 0;
    var onData = (ev) => {
      if (this.#closed || this.#eventSource !== es) return;
      errors = 0;
      var envelope;
      try { envelope = JSON.parse(ev.data); } catch (e) { return; } // ignore unparseable frames
      this.#fire(envelope);
    };
    for (var type of EVENT_TYPES) {
      if (typeof es.addEventListener === 'function') es.addEventListener(type, onData);
    }
    es.onmessage = onData; // unnamed frames; dedupe absorbs any double delivery
    es.onerror = () => {
      if (this.#closed || this.#eventSource !== es) return;
      errors++;
      // Native EventSource retries transient drops itself; only a CLOSED
      // stream (e.g. expired token) or repeated errors need a fresh token.
      if (es.readyState === 2 || errors >= 2) this.#scheduleReconnect();
    };
    if (typeof es.addEventListener === 'function') {
      es.addEventListener('open', () => {
        if (this.#closed || this.#eventSource !== es) return;
        this.#reconnectAttempts = 0;
      });
    }
  }

  #closeStream() {
    var es = this.#eventSource;
    this.#eventSource = null;
    if (es) { try { es.close(); } catch (e) { /* already gone */ } }
  }

  #scheduleReconnect() {
    if (this.#closed || this.#reconnectTimer) return;
    this.#closeStream();
    this.#setState('reconnecting');
    var steps = [1000, 2000, 5000];
    var delay = steps[Math.min(this.#reconnectAttempts, steps.length - 1)];
    this.#reconnectAttempts++;
    this.#reconnectTimer = timer(async () => {
      this.#reconnectTimer = null;
      if (this.#closed) return;
      try {
        await this.#fetchToken('unauthorized');
        if (this.#closed) return;
        this.#openStream();
        this.#scheduleRefresh();
        this.#setState('live');
      } catch (err) {
        this.#scheduleReconnect(); // backs off again, capped at 5s
      }
    }, delay);
  }
}

async function readBody(res) {
  if (typeof res.text === 'function') {
    var text = await res.text();
    if (!text) return null;
    try { return JSON.parse(text); } catch (e) { return { error: text }; }
  }
  if (typeof res.json === 'function') {
    try { return await res.json(); } catch (e) { return null; }
  }
  return null;
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = HfyClient;
  module.exports.HfyClient = HfyClient;
}
if (typeof window !== 'undefined') window.HfyClient = HfyClient;
if (typeof globalThis !== 'undefined' && typeof window === 'undefined') {
  // Node tests reach it via require(); also expose globally for script-style loads.
  globalThis.HfyClient = globalThis.HfyClient || HfyClient;
}
