'use strict';
/* hfy embed UI: bridge + Alpine component. User content renders via x-text
   in index.html; agent markdown is built by x-markdown from createElement +
   textContent only (hfyMdParse -> tree, hfyMdBuild -> DOM). Payload fields
   follow internal/types/payloads.go: TurnState{state},
   MessageText{text,attachments}, TextDelta{text}, ToolCall{name,call_id,
   arguments}, ToolResult{call_id,content,is_error,structured},
   ApprovalRequested{call_id}, ApprovalDecided{call_id,approve,result,reason}.
   Envelope: {id,type,parent,actor:{uuid,type,ref},data,created_at}
   (internal/types/types.go). */

var HFY_STRINGS = {
  en: { title: 'Conversation', placeholder: 'Message…', send: 'Send', stop: 'Stop', attach: 'Attach files', thinking: 'Thinking', tool: 'Tool', interrupted: 'Interrupted', reconnecting: 'Reconnecting…', refreshing: 'Refreshing…', ended: 'Conversation ended', error: 'Something went wrong', retry: 'Retry', loadOlder: 'Load older messages', you: 'You', agent: 'Assistant', connecting: 'Connecting…',
    copy: 'Copy', copied: 'Copied', copyCode: 'Copy code', newMessages: 'New messages', welcomeTitle: 'How can I help?', welcomeSubtitle: 'Send a message to start the conversation.', remove: 'Remove', uploadFailed: 'Upload failed', dropFiles: 'Drop files to send', today: 'Today', yesterday: 'Yesterday', approvalNeeded: 'Approval needed', running: 'In progress', done: 'Done', failed: 'Failed', args: 'Arguments', result: 'Result', structured: 'Structured result', messages: 'Messages', typing: 'Assistant is typing…', file: 'File',
    wantsTo: 'The assistant wants to:', allow: 'Allow', deny: 'Deny', allowed: 'Allowed', denied: 'Denied', tickQueued: 'Sent', tickRunning: 'Delivered', tickDone: 'Answered', tickFailed: 'Not completed',
    actionNeeded: 'Action needed', waitingFor: 'Waiting for {name}', waitingYou: 'Waiting for your decision', open: 'Open', next: 'Next', back: 'Back', submit: 'Submit', stepOf: '{n} of {total}',
    yes: 'yes', no: 'no', photos: 'Photos', camera: 'Camera', document: 'Document', caption: 'Add a caption…', addMore: 'Add more', close: 'Close',
    discardTitle: 'Discard these attachments?', discard: 'Discard', keep: 'Keep editing', errType: 'This file type can’t be sent: {name}', errSize: 'Files must be at most {size}' },
  ar: { title: 'المحادثة', placeholder: 'اكتب رسالة…', send: 'إرسال', stop: 'إيقاف', attach: 'إرفاق ملفات', thinking: 'التفكير', tool: 'أداة', interrupted: 'انقطعت', reconnecting: 'إعادة الاتصال…', refreshing: 'جارٍ التحديث…', ended: 'انتهت المحادثة', error: 'حدث خطأ ما', retry: 'إعادة المحاولة', loadOlder: 'تحميل الرسائل الأقدم', you: 'أنت', agent: 'المساعد', connecting: 'جارٍ الاتصال…',
    copy: 'نسخ', copied: 'تم النسخ', copyCode: 'نسخ الشيفرة', newMessages: 'رسائل جديدة', welcomeTitle: 'كيف يمكنني مساعدتك؟', welcomeSubtitle: 'أرسل رسالة لبدء المحادثة.', remove: 'إزالة', uploadFailed: 'فشل الرفع', dropFiles: 'أفلت الملفات لإرسالها', today: 'اليوم', yesterday: 'أمس', approvalNeeded: 'مطلوب موافقتك', running: 'قيد التنفيذ', done: 'اكتملت', failed: 'فشلت', args: 'المُدخلات', result: 'النتيجة', structured: 'نتيجة منظّمة', messages: 'الرسائل', typing: 'المساعد يكتب…', file: 'ملف',
    wantsTo: 'يريد المساعد تنفيذ:', allow: 'سماح', deny: 'رفض', allowed: 'تم السماح', denied: 'تم الرفض', tickQueued: 'أُرسلت', tickRunning: 'وصلت', tickDone: 'تم الرد', tickFailed: 'لم تكتمل',
    actionNeeded: 'مطلوب إجراء', waitingFor: 'بانتظار {name}', waitingYou: 'بانتظار قرارك', open: 'فتح', next: 'التالي', back: 'السابق', submit: 'تأكيد', stepOf: '{n} من {total}',
    yes: 'نعم', no: 'لا', photos: 'الصور', camera: 'الكاميرا', document: 'مستند', caption: 'أضف تعليقًا…', addMore: 'إضافة المزيد', close: 'إغلاق',
    discardTitle: 'تجاهل هذه المرفقات؟', discard: 'تجاهل', keep: 'متابعة التعديل', errType: 'لا يمكن إرسال هذا النوع من الملفات: {name}', errSize: 'يجب ألا يتجاوز حجم الملف {size}' }
};
var HFY_RTL = ['ar', 'he', 'fa', 'ur', 'dv', 'ps', 'sd', 'yi'];
var HFY_DEFAULTS = { lang: '', dir: 'auto', theme: 'auto', accent: '#6366f1', density: 'comfortable', layout: 'bubbles', tool_calls: 'summary', uploads: null, parents: [] };
var HFY_TERMINAL = ['completed', 'failed', 'stopped', 'cancelled'];
var HFY_TOOL_MODES = ['hidden', 'summary', 'details'];
var HFY_ACTIVE = ['queued', 'running', 'awaiting_approval'];

function hfyResolveConfig() {
  var raw = (typeof window !== 'undefined') ? window.__HFY_EMBED__ : null;
  if (!raw || typeof raw !== 'object') return Object.assign({}, HFY_DEFAULTS);
  var c = Object.assign({}, HFY_DEFAULTS, raw);
  if (!Array.isArray(c.parents)) c.parents = [];
  if (HFY_TOOL_MODES.indexOf(c.tool_calls) === -1) c.tool_calls = HFY_DEFAULTS.tool_calls;
  // uploads: null when the server can't take files, else {types: [mime], max_bytes}
  var u = c.uploads;
  c.uploads = (u && Array.isArray(u.types) && u.types.length && u.max_bytes > 0)
    ? { types: u.types.map(function (t) { return String(t).toLowerCase(); }), max_bytes: Number(u.max_bytes) } : null;
  return c;
}
var hfyConfig = hfyResolveConfig();

function hfySendToHost(type, payload) {
  var str = JSON.stringify(Object.assign({ v: 1, type: type }, payload || {}));
  try {
    if (window.HfyBridge && window.HfyBridge.postMessage) return void window.HfyBridge.postMessage(str);
    if (window.webkit && window.webkit.messageHandlers && window.webkit.messageHandlers.hfy) return void window.webkit.messageHandlers.hfy.postMessage(str);
    if (window.chrome && window.chrome.webview) return void window.chrome.webview.postMessage(str);
    if (window.ReactNativeWebView) return void window.ReactNativeWebView.postMessage(str);
    if (window.flutter_inappwebview) return void window.flutter_inappwebview.callHandler('hfy', str);
    // Framed fallback uses '*' because the widget cannot know which allowlisted
    // origin the parent is; grant delivery is inbound-validated instead.
    if (window.parent !== window) window.parent.postMessage(str, '*');
  } catch (e) { /* host gone */ }
}

var hfyBridge = { waiters: [], stored: null, lastToken: null, component: null, pendingConfigure: null };
var hfyClient = null; // outside Alpine state: its reactive proxy breaks #private fields
var hfyDrag = null; // sheet handle swipe in progress (DOM refs stay out of Alpine)
function hfyHandleInbound(msg) {
  if (!msg || typeof msg !== 'object') return;
  if (msg.type === 'grant') {
    if (!msg.token) return;
    var grant = { token: msg.token, expires_at: msg.expires_at };
    // a grant answering a waiter always resolves it; dedupe only unsolicited stores
    if (hfyBridge.waiters.length) {
      hfyBridge.lastToken = msg.token;
      hfyBridge.waiters.splice(0).forEach(function (w) { w(grant); });
      return;
    }
    if (msg.token === hfyBridge.lastToken) return;
    hfyBridge.lastToken = msg.token;
    hfyBridge.stored = grant;
  } else if (msg.type === 'configure') {
    if (hfyBridge.component) hfyBridge.component.applyConfigure(msg);
    else hfyBridge.pendingConfigure = msg;
  }
}
function hfyRequestGrant(reason) {
  if (hfyBridge.stored) {
    var g = hfyBridge.stored, exp = g.expires_at ? Date.parse(g.expires_at) : NaN;
    hfyBridge.stored = null;
    if (isNaN(exp) || exp > Date.now()) return Promise.resolve(g);
  }
  var p = new Promise(function (res) { hfyBridge.waiters.push(res); });
  hfySendToHost('token-needed', { reason: reason || 'initial' });
  return p;
}
window.hfy = { receive: function (msg) {
  if (typeof msg === 'string') { try { msg = JSON.parse(msg); } catch (e) { return; } }
  hfyHandleInbound(msg);
} };
window.addEventListener('message', function (ev) {
  if (window.parent !== window) {
    if (ev.source !== window.parent) return;
    if (hfyConfig.parents.indexOf('*') === -1 &&
        hfyConfig.parents.indexOf(ev.origin) === -1 && ev.origin !== location.origin) return;
  } else if (!window.ReactNativeWebView) return;
  var d = ev.data;
  if (typeof d === 'string') { try { d = JSON.parse(d); } catch (e) { return; } }
  if (d && (d.type === 'grant' || d.type === 'configure')) hfyHandleInbound(d);
});

function hfyData(x) {
  if (!x) return {};
  if (typeof x === 'string') { try { return JSON.parse(x); } catch (e) { return {}; } }
  return x;
}
function hfyPretty(x) {
  if (x == null) return '';
  if (typeof x === 'string') { try { return JSON.stringify(JSON.parse(x), null, 2); } catch (e) { return x; } }
  try { return JSON.stringify(x, null, 2); } catch (e) { return String(x); }
}

var hfyFmtCache = {};
function hfyFmt(lang, opts) {
  var key = lang + '|' + JSON.stringify(opts);
  if (!hfyFmtCache[key]) {
    try { hfyFmtCache[key] = new Intl.DateTimeFormat(lang, opts); }
    catch (e) { delete opts.timeZone; hfyFmtCache[key] = new Intl.DateTimeFormat('en', opts); }
  }
  return hfyFmtCache[key];
}
function hfySize(n, lang) {
  if (!(n >= 0)) return '';
  var units = ['byte', 'kilobyte', 'megabyte', 'gigabyte'], i = 0;
  // binary steps, as OS file managers and upload limits (10 MiB -> "10 MB") read
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  // 'short' bytes reads "13 byte" in English; 'narrow' gives "13B"
  try { return new Intl.NumberFormat(lang, { style: 'unit', unit: units[i], unitDisplay: i ? 'short' : 'narrow', maximumFractionDigits: i ? 1 : 0 }).format(n); }
  catch (e) { return (i ? n.toFixed(1) : n) + ' ' + ['B', 'KB', 'MB', 'GB'][i]; }
}
function hfyInitials(name) {
  return String(name || '').trim().split(/[\s._@-]+/).filter(Boolean).slice(0, 2)
    .map(function (p) { return Array.from(p)[0].toUpperCase(); }).join('');
}
// Text color for the accent fill: white unless it would fall under ~4:1.
function hfyOnColor(hex) {
  var m = /^#?([0-9a-f]{3}|[0-9a-f]{6})$/i.exec(String(hex || '').trim());
  if (!m) return '#fff';
  var h = m[1].length === 3 ? m[1].replace(/./g, '$&$&') : m[1];
  var lin = [0, 2, 4].map(function (i) {
    var c = parseInt(h.substr(i, 2), 16) / 255;
    return c <= 0.03928 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4);
  });
  var lum = 0.2126 * lin[0] + 0.7152 * lin[1] + 0.0722 * lin[2];
  return 1.05 / (lum + 0.05) >= 4 ? '#fff' : '#111827';
}
// "read_file" / "readFile" -> "Read file"
function hfyHumanize(s) {
  s = String(s || '').replace(/([a-z0-9])([A-Z])/g, '$1 $2').replace(/[_\-\s]+/g, ' ').trim().toLowerCase();
  return s ? Array.from(s)[0].toUpperCase() + s.slice(Array.from(s)[0].length) : '';
}
// "files.read_file" -> "Read file": the part after the last server separator
function hfyToolName(name) { return hfyHumanize(String(name || '').split(/\.|__|\//).pop()); }
function hfyClip(s, n) {
  var a = Array.from(String(s).replace(/\s+/g, ' ').trim());
  return a.length > n ? a.slice(0, n - 1).join('') + '…' : a.join('');
}
// A JSON value as plain words, never JSON: booleans yes/no, arrays
// comma-joined, objects "Key: value · Key: value", two levels deep.
function hfyArgText(x, L, depth) {
  if (x == null) return '—';
  if (typeof x === 'boolean') return x ? L.yes : L.no;
  if (typeof x !== 'object') return String(x);
  if (depth >= 2) return '…';
  if (Array.isArray(x)) return x.map(function (v) { return hfyArgText(v, L, depth + 1); }).join(', ');
  return Object.keys(x).map(function (k) { return (hfyHumanize(k) || k) + ': ' + hfyArgText(x[k], L, depth + 1); }).join(' · ');
}
// Tool arguments as a short key: value list for approvals. L: {yes, no}.
function hfyArgList(raw, L) {
  var v = raw, max = 12;
  L = L || { yes: 'yes', no: 'no' };
  if (typeof v === 'string') {
    try { v = JSON.parse(v); } catch (e) { return v.trim() ? [{ k: 0, key: '', hasKey: false, value: hfyClip(v, 120) }] : []; }
  }
  if (v == null) return [];
  if (typeof v !== 'object' || Array.isArray(v)) return [{ k: 0, key: '', hasKey: false, value: hfyClip(hfyArgText(v, L, 0), 120) }];
  var keys = Object.keys(v), out = keys.slice(0, max).map(function (key, i) {
    return { k: i, key: hfyHumanize(key) || key, hasKey: true, value: hfyClip(hfyArgText(v[key], L, 0), 120) };
  });
  if (keys.length > max) out.push({ k: max, key: '', hasKey: false, value: '…' });
  return out;
}
function hfyIsImageType(t) { return /^image\//i.test(String(t || '')); }
// Document icon tint in the document preview.
function hfyDocCls(mime) {
  var m = String(mime || '').toLowerCase();
  if (m === 'application/pdf') return 'is-pdf';
  if (/sheet|excel|csv/.test(m)) return 'is-sheet';
  if (/word|document|rtf/.test(m)) return 'is-word';
  if (/zip|compressed|tar|gzip/.test(m)) return 'is-zip';
  if (/^text\//.test(m)) return 'is-text';
  return 'is-file';
}
// Focus after Alpine's next tick has revealed the target.
function hfyFocusSoon(sel) {
  setTimeout(function () { var el = document.querySelector(sel); if (el && el.focus) el.focus(); }, 40);
}
// Keep Tab inside a modal (ev.currentTarget); the page behind is inert too.
function hfyTrap(ev) {
  if (ev.key !== 'Tab') return;
  var box = ev.currentTarget, f = Array.from(box.querySelectorAll('button:not([disabled]), summary, textarea'))
    .filter(function (el) { return el.offsetParent !== null; });
  if (!f.length) return;
  var a = document.activeElement;
  if (ev.shiftKey && (a === f[0] || a === box)) { ev.preventDefault(); f[f.length - 1].focus(); }
  else if (!ev.shiftKey && a === f[f.length - 1]) { ev.preventDefault(); f[0].focus(); }
}
// Stable 0..11 bucket per sender, mapped to readable hues in ui.css.
function hfyHue(s) {
  var h = 2166136261; s = String(s); // FNV-1a
  for (var i = 0; i < s.length; i++) h = Math.imul(h ^ s.charCodeAt(i), 16777619);
  return (h >>> 0) % 12;
}
// Delivery ticks on own messages follow their turn's state.
function hfyTick(state) {
  if (state === 'completed') return { cls: 'is-read', label: 'tickDone' };
  if (state === 'running' || state === 'awaiting_approval') return { cls: 'is-delivered', label: 'tickRunning' };
  if (HFY_TERMINAL.indexOf(state) !== -1) return { cls: 'is-failed', label: 'tickFailed' };
  return { cls: 'is-sent', label: 'tickQueued' };
}

// Links in markdown: absolute http(s)/mailto only; anything else stays text.
function hfySafeHref(raw) {
  if (typeof raw !== 'string' || !/^(?:https?:\/\/|mailto:)/i.test(raw)) return null;
  var u; try { u = new URL(raw); } catch (e) { return null; }
  return /^(?:https?|mailto):$/.test(u.protocol) ? u.href : null;
}
// Attachment URLs come from the server (signed, possibly same-origin relative).
function hfySafeUrl(raw) {
  if (typeof raw !== 'string' || !raw) return '';
  try { var u = new URL(raw, location.href); return /^https?:$/.test(u.protocol) ? u.href : ''; } catch (e) { return ''; }
}
function hfyCopy(text) {
  if (navigator.clipboard && navigator.clipboard.writeText) {
    return navigator.clipboard.writeText(text).catch(function () { hfyCopyFallback(text); });
  }
  return Promise.resolve().then(function () { hfyCopyFallback(text); });
}
function hfyCopyFallback(text) {
  var prev = document.activeElement, ta = document.createElement('textarea'), ok = false;
  ta.value = text; ta.setAttribute('readonly', ''); ta.className = 'hfy-offscreen';
  document.body.appendChild(ta); ta.select();
  try { ok = document.execCommand('copy'); } catch (e) { ok = false; }
  ta.remove();
  if (prev && prev.focus) prev.focus();
  if (!ok) throw new Error('copy failed');
}

/* ---- markdown: parse(text) -> tree (pure), build(tree) -> DOM ---- */
var HFY_MD_DEPTH = 6;
var HFY_MD_FENCE = /^ {0,3}(`{3,}|~{3,})[ \t]*([^\s`]*)[^`]*$/;
var HFY_MD_HEAD = /^ {0,3}(#{1,6})[ \t]+(.*?)(?:[ \t]+#+)?[ \t]*$/;
var HFY_MD_HR = /^ {0,3}([-*_])(?:[ \t]*\1){2,}[ \t]*$/;
var HFY_MD_QUOTE = /^ {0,3}> ?/;
var HFY_MD_ITEM = /^( {0,3})([-*+]|\d{1,9}[.)])(?:([ \t]+)(.*))?$/;
var HFY_MD_PUNCT = /[!-\/:-@\[-`{-~]/;
var HFY_MD_WORD = /[\p{L}\p{N}]/u;
var HFY_MD_AUTO = /<((?:https?:\/\/|mailto:)[^\s<>]+)>/iy;
var HFY_MD_BARE = /https?:\/\/[^\s<>"'`]+/iy;

function hfyIndent(line) {
  var n = 0;
  for (var i = 0; i < line.length; i++) {
    if (line[i] === ' ') n++; else if (line[i] === '\t') n += 4 - (n % 4); else break;
  }
  return n;
}
function hfyDedent(line, cols) {
  var n = 0, i = 0;
  while (i < line.length && n < cols && (line[i] === ' ' || line[i] === '\t')) { n += line[i] === '\t' ? 4 - (n % 4) : 1; i++; }
  return line.slice(i);
}
function hfyRun(s, i, ch) { var n = 0; while (s[i + n] === ch) n++; return n; }
function hfyFindRun(s, from, ch, len) {
  for (var j = from; j < s.length;) {
    if (s[j] !== ch) { j++; continue; }
    var r = hfyRun(s, j, ch);
    if (r === len) return j;
    j += r;
  }
  return -1;
}
function hfyMdStarts(line, inPara) {
  if (HFY_MD_FENCE.test(line) || HFY_MD_HEAD.test(line) || HFY_MD_HR.test(line) || HFY_MD_QUOTE.test(line)) return true;
  var m = HFY_MD_ITEM.exec(line);
  if (!m) return false;
  if (!inPara) return true;
  // inside a paragraph only a non-empty bullet or a list starting at 1 interrupts
  return !!m[4] && (/^[-*+]$/.test(m[2]) || /^1[.)]$/.test(m[2]));
}
function hfyMdParse(src) {
  return hfyMdBlocks(String(src == null ? '' : src).replace(/\r\n?/g, '\n').split('\n'), 0);
}
function hfyMdBlocks(lines, depth) {
  var out = [], para = [], i = 0, m;
  function flush() { if (para.length) { out.push({ t: 'p', c: hfyMdInline(para.join('\n'), 0, false) }); para = []; } }
  while (i < lines.length) {
    var line = lines[i];
    if (!line.trim()) { flush(); i++; continue; }
    if (para.length && !hfyMdStarts(line, true)) { para.push(line.trim()); i++; continue; }
    flush();
    if ((m = HFY_MD_FENCE.exec(line))) {
      var close = new RegExp('^ {0,3}' + (m[1][0] === '`' ? '`' : '~') + '{' + m[1].length + ',}[ \\t]*$'), body = [], ind = hfyIndent(line);
      for (i++; i < lines.length && !close.test(lines[i]); i++) body.push(hfyDedent(lines[i], ind));
      i++; // closing fence; an unterminated fence (still streaming) runs to the end
      out.push({ t: 'pre', lang: m[2], text: body.join('\n') });
      continue;
    }
    if ((m = HFY_MD_HEAD.exec(line))) { out.push({ t: 'h', level: Math.min(m[1].length, 3), c: hfyMdInline(m[2], 0, false) }); i++; continue; }
    if (HFY_MD_HR.test(line)) { out.push({ t: 'hr' }); i++; continue; }
    if (HFY_MD_QUOTE.test(line)) {
      var q = [];
      while (i < lines.length && HFY_MD_QUOTE.test(lines[i])) { q.push(lines[i].replace(HFY_MD_QUOTE, '')); i++; }
      out.push(depth < HFY_MD_DEPTH ? { t: 'quote', c: hfyMdBlocks(q, depth + 1) } : { t: 'p', c: hfyMdInline(q.join('\n'), 0, false) });
      continue;
    }
    if (HFY_MD_ITEM.test(line)) { i = hfyMdList(lines, i, depth, out); continue; }
    para.push(line.trim()); i++;
  }
  flush();
  return out;
}
function hfyMdList(lines, i, depth, out) {
  var first = HFY_MD_ITEM.exec(lines[i]), ordered = /\d/.test(first[2]), items = [], cur = null;
  var indent = 0, nest = 0, blank = false, m;
  while (i < lines.length) {
    var line = lines[i], ind = hfyIndent(line);
    if (cur && line.trim() && ind >= nest) { cur.push(hfyDedent(line, Math.min(ind, indent))); blank = false; i++; continue; }
    m = HFY_MD_ITEM.exec(line);
    if (m && /\d/.test(m[2]) === ordered && !HFY_MD_HR.test(line)) {
      if (cur) items.push(cur);
      var gap = m[3] ? (m[3].length > 4 ? 1 : m[3].length) : 1;
      cur = [m[4] || ''];
      indent = m[1].length + m[2].length + gap;
      nest = m[1].length + Math.min(m[2].length + 1, 2);
      blank = false; i++; continue;
    }
    if (!line.trim()) {
      var j = i + 1, m2;
      while (j < lines.length && !lines[j].trim()) j++;
      if (j < lines.length && (hfyIndent(lines[j]) >= nest || ((m2 = HFY_MD_ITEM.exec(lines[j])) && /\d/.test(m2[2]) === ordered))) { cur.push(''); blank = true; i++; continue; }
      break;
    }
    if (!blank && !hfyMdStarts(line, true)) { cur.push(line.trim()); i++; continue; } // lazy continuation
    break;
  }
  if (cur) items.push(cur);
  out.push({ t: ordered ? 'ol' : 'ul', start: ordered ? parseInt(first[2], 10) : 1,
    items: items.map(function (it) { return depth < HFY_MD_DEPTH ? hfyMdBlocks(it, depth + 1) : [{ t: 'p', c: hfyMdInline(it.join('\n'), 0, false) }]; }) });
  return i;
}
function hfyTrimUrl(url) {
  url = url.replace(/[.,;:!?'"*_~]+$/, '');
  while (url.slice(-1) === ')' && url.split('(').length < url.split(')').length) url = url.slice(0, -1).replace(/[.,;:!?'"*_~]+$/, '');
  return url;
}
function hfyMdInline(s, depth, noLink) {
  if (depth > HFY_MD_DEPTH) return [{ t: 'text', v: s }];
  var out = [], buf = '', i = 0, n = s.length, m, h, r;
  function emit(node) { if (buf) { out.push({ t: 'text', v: buf }); buf = ''; } out.push(node); }
  while (i < n) {
    var c = s[i];
    if (c === '\\' && i + 1 < n && HFY_MD_PUNCT.test(s[i + 1])) { buf += s[i + 1]; i += 2; continue; }
    if (c === '\n') { emit({ t: 'br' }); i++; continue; }
    if (c === '`') {
      r = hfyRun(s, i, '`');
      var end = hfyFindRun(s, i + r, '`', r);
      if (end === -1) { buf += s.slice(i, i + r); i += r; continue; }
      var code = s.slice(i + r, end).replace(/\n/g, ' ');
      if (code.length > 2 && code[0] === ' ' && code[code.length - 1] === ' ' && code.trim()) code = code.slice(1, -1);
      emit({ t: 'code', v: code }); i = end + r; continue;
    }
    if (!noLink && c === '[') {
      var lk = hfyMdLink(s, i, depth);
      if (lk) { emit(lk.node); i = lk.end; continue; }
    }
    if (!noLink && c === '<') {
      HFY_MD_AUTO.lastIndex = i;
      if ((m = HFY_MD_AUTO.exec(s)) && (h = hfySafeHref(m[1]))) { emit({ t: 'a', href: h, c: [{ t: 'text', v: m[1] }] }); i += m[0].length; continue; }
    }
    if (!noLink && (c === 'h' || c === 'H') && (i === 0 || !/[\w\/]/.test(s[i - 1]))) {
      HFY_MD_BARE.lastIndex = i;
      if ((m = HFY_MD_BARE.exec(s))) {
        var url = hfyTrimUrl(m[0]);
        if ((h = hfySafeHref(url))) { emit({ t: 'a', href: h, c: [{ t: 'text', v: url }] }); i += url.length; continue; }
      }
    }
    if (c === '*' || c === '_') {
      var em = hfyMdEmph(s, i, depth, noLink);
      if (em) { emit(em.node); i = em.end; continue; }
      r = hfyRun(s, i, c); buf += s.slice(i, i + r); i += r; continue;
    }
    buf += c; i++;
  }
  if (buf) out.push({ t: 'text', v: buf });
  return out;
}
function hfyMdEmph(s, i, depth, noLink) {
  var ch = s[i], run = hfyRun(s, i, ch), after = s[i + run];
  if (run > 3 || after === undefined || /\s/.test(after)) return null;
  if (ch === '_' && i > 0 && HFY_MD_WORD.test(s[i - 1])) return null;
  var close = hfyMdCloser(s, i + run, ch, run, depth);
  if (!close || close.at <= i + run) return null;
  var kids = hfyMdInline(s.slice(i + run, close.at), depth + 1, noLink);
  var node = { t: run === 2 ? 'b' : 'i', c: kids };
  if (run === 3) node = { t: 'b', c: [node] };
  return { node: node, end: close.end };
}
// shortcut: emphasis matching is a forward scan, not CommonMark's delimiter
// stack; odd nestings degrade to literal asterisks, never to markup.
function hfyMdCloser(s, from, ch, k, depth) {
  for (var j = from; j < s.length;) {
    var c = s[j];
    if (c === '\\') { j += 2; continue; }
    if (c === '`') { var r = hfyRun(s, j, '`'), e = hfyFindRun(s, j + r, '`', r); j = e === -1 ? j + r : e + r; continue; }
    if (c !== ch) { j++; continue; }
    var m = hfyRun(s, j, ch), prev = s[j - 1], next = s[j + m];
    var canClose = !/\s/.test(prev) && (ch !== '_' || next === undefined || !HFY_MD_WORD.test(next));
    if (canClose && m === k) return { at: j, end: j + m };
    if (canClose && m === 3 && k < 3) return { at: j + m - k, end: j + m };
    var canOpen = next !== undefined && !/\s/.test(next) && m <= 3 && (ch !== '_' || !HFY_MD_WORD.test(prev));
    if (canOpen && depth < HFY_MD_DEPTH) {
      var sub = hfyMdCloser(s, j + m, ch, m, depth + 1);
      if (sub) { j = sub.end; continue; }
    }
    j += m;
  }
  return null;
}
function hfyMdLink(s, i, depth) {
  var d = 0, j = i, lim = Math.min(s.length, i + 1000);
  for (; j < lim; j++) {
    if (s[j] === '\\') { j++; continue; }
    if (s[j] === '[') d++;
    else if (s[j] === ']' && --d === 0) break;
  }
  if (j >= lim || s[j + 1] !== '(') return null;
  var k = j + 2, p = 1, klim = Math.min(s.length, k + 2048);
  for (; k < klim; k++) {
    if (s[k] === '\\') { k++; continue; }
    if (s[k] === '\n') return null;
    if (s[k] === '(') p++;
    else if (s[k] === ')' && --p === 0) break;
  }
  if (p !== 0 || k >= klim) return null;
  var dest = s.slice(j + 2, k).trim();
  var mm = /^<([^<>\n]*)>(?:\s+(?:"[^"]*"|'[^']*'))?$/.exec(dest) || /^(\S*)(?:\s+(?:"[^"]*"|'[^']*'|\([^)]*\)))?$/.exec(dest);
  var href = mm ? hfySafeHref(mm[1]) : null;
  if (!href) return { node: { t: 'text', v: s.slice(i, k + 1) }, end: k + 1 };
  return { node: { t: 'a', href: href, c: hfyMdInline(s.slice(i + 1, j), depth + 1, true) }, end: k + 1 };
}
// The block a message visually ends in (descending into lists and quotes).
function hfyMdLastLeaf(tree) {
  var n = tree[tree.length - 1];
  while (n) {
    if (n.t === 'quote') n = n.c[n.c.length - 1];
    else if (n.t === 'ul' || n.t === 'ol') { var it = n.items[n.items.length - 1]; n = it && it[it.length - 1]; }
    else return n;
  }
  return null;
}
// The final paragraph/heading gets .hfy-end: the stream caret and the
// in-bubble timestamp spacer hang off it.
function hfyMdBuild(tree, doc, labels) {
  var leaf = hfyMdLastLeaf(tree);
  if (leaf && (leaf.t === 'p' || leaf.t === 'h')) leaf.end = true;
  return hfyMdFrag(tree, doc, labels || {});
}
function hfyMdFrag(tree, doc, L) {
  var frag = doc.createDocumentFragment();
  for (var i = 0; i < tree.length; i++) frag.appendChild(hfyMdEl(tree[i], doc, L));
  return frag;
}
function hfyMdEl(n, doc, L) {
  var el;
  switch (n.t) {
    case 'text': return doc.createTextNode(n.v);
    case 'br': return doc.createElement('br');
    case 'hr': return doc.createElement('hr');
    case 'code': el = doc.createElement('code'); el.textContent = n.v; return el;
    case 'pre': return hfyMdCodeBlock(n, doc, L);
    case 'b': el = doc.createElement('strong'); break;
    case 'i': el = doc.createElement('em'); break;
    case 'a':
      el = doc.createElement('a');
      el.setAttribute('href', n.href); el.setAttribute('target', '_blank'); el.setAttribute('rel', 'noopener noreferrer');
      break;
    case 'p': case 'h':
      el = doc.createElement(n.t === 'p' ? 'p' : 'h' + (n.level + 2)); el.setAttribute('dir', 'auto');
      if (n.end) el.className = 'hfy-end';
      break;
    case 'quote': el = doc.createElement('blockquote'); el.setAttribute('dir', 'auto'); break;
    case 'ul': case 'ol':
      el = doc.createElement(n.t); el.setAttribute('dir', 'auto');
      if (n.t === 'ol' && n.start !== 1) el.setAttribute('start', String(n.start));
      n.items.forEach(function (item) { var li = doc.createElement('li'); li.appendChild(hfyMdFrag(item, doc, L)); el.appendChild(li); });
      return el;
    default: return doc.createTextNode('');
  }
  for (var i = 0; i < n.c.length; i++) el.appendChild(hfyMdEl(n.c[i], doc, L));
  return el;
}
function hfyMdCodeBlock(n, doc, L) {
  var wrap = doc.createElement('div'), head = doc.createElement('div'), lang = doc.createElement('span');
  var btn = doc.createElement('button'), pre = doc.createElement('pre'), code = doc.createElement('code');
  wrap.className = 'hfy-code'; head.className = 'hfy-code-head'; lang.className = 'hfy-code-lang';
  lang.textContent = n.lang || '';
  btn.setAttribute('type', 'button'); btn.className = 'hfy-code-copy';
  btn.textContent = L.copy || 'Copy'; btn.setAttribute('aria-label', L.copyCode || 'Copy code');
  btn.addEventListener('click', function () {
    var cur = (hfyBridge.component && hfyBridge.component.L) || L;
    hfyCopy(n.text).then(function () {
      btn.textContent = cur.copied || 'Copied';
      setTimeout(function () { btn.textContent = cur.copy || 'Copy'; }, 1500);
    }, function () { /* clipboard denied */ });
  });
  code.textContent = n.text;
  pre.appendChild(code); head.appendChild(lang); head.appendChild(btn);
  wrap.appendChild(head); wrap.appendChild(pre);
  return wrap;
}
function hfyMdRender(el, text) {
  var c = hfyBridge.component;
  el.replaceChildren(hfyMdBuild(hfyMdParse(text), document, c ? c.L : HFY_STRINGS.en));
  if (c) c.keepBottom();
}

document.addEventListener('alpine:init', function () {
  // x-markdown="path": first paint is synchronous (stable heights for
  // loadOlder); streaming updates coalesce to one rebuild per frame.
  window.Alpine.directive('markdown', function (el, dir, util) {
    var get = util.evaluateLater(dir.expression), last = null, raf = 0, first = true;
    util.effect(function () {
      get(function (text) {
        text = text == null ? '' : String(text);
        if (text === last) return;
        last = text;
        if (first) { first = false; hfyMdRender(el, text); return; }
        if (!raf) raf = requestAnimationFrame(function () { raf = 0; hfyMdRender(el, last); });
      });
    });
    util.cleanup(function () { if (raf) cancelAnimationFrame(raf); });
  });
  // x-progress="path": a 0..1 value as the --hfy-p custom property (send ring)
  window.Alpine.directive('progress', function (el, dir, util) {
    var get = util.evaluateLater(dir.expression);
    util.effect(function () {
      get(function (v) { el.style.setProperty('--hfy-p', String(Math.max(0, Math.min(1, Number(v) || 0)))); });
    });
  });
  window.Alpine.data('hfyEmbed', function () { return window.hfyEmbed(); });
});

var hfySeq = 0;
window.hfyEmbed = function () {
  return {
    config: hfyResolveConfig(), draft: '', connState: 'idle', turns: [], L: Object.assign({}, HFY_STRINGS.en),
    hasOlder: false, loadingOlder: false, tz: '', customStrings: {},
    statusShow: false, statusText: '', statusCls: '', showRetry: false,
    composerError: '', showWelcome: false, showNewPill: false, dragOver: false,
    sendDisabled: true, coarse: false, enterHint: 'send',
    // attachments: 📎/📷 in the pill, an attach menu, then a photo or document preview
    showAttach: false, showCamera: false, attachDisabled: true, photoAccept: '', docAccept: '',
    attachMenuOpen: false, attachOpts: { photos: false, camera: false, document: false }, modalOpen: false,
    pvOpen: false, pvKind: 'photo', pvItems: [], pvSel: '', pvCaption: '', pvSending: false, pvError: '', pvConfirm: false,
    preview: { cls: 'is-photo', isPhoto: true, isDoc: false, images: [], docs: [], hasDocs: false, mainUrl: '', mainName: '', title: '',
      progress: 1, sendCls: '', sendDisabled: true },
    _pvPosting: false,
    // one round button: Send, or Stop while the viewer's own turn is active
    ownActive: false, roundCls: 'is-send', roundLabel: '', roundDisabled: true,
    // approval bottom sheet over the viewer's own pending approvals
    pendingOwn: [], sheetOpen: false, showActionPill: false, pendingCount: '', sheetStep: 0, sheetError: '', sheetBusy: false,
    sheet: { title: '', argList: [], hasArgList: false, showRawArgs: false, argsText: '', isMulti: false, stepText: '', dots: [],
      allowPressed: null, denyPressed: null, showBack: false, showNext: false, showSubmit: false, submitDisabled: true, showDecide: true },
    _seenPending: {}, _sheetMin: false, _awaitTurn: '',
    _stick: true, _collecting: null, _loaded: false, _dragDepth: 0,

    t: function (k) {
      if (this.customStrings && this.customStrings[k] != null) return String(this.customStrings[k]);
      var base = (this.config.lang || 'en').split('-')[0];
      var s = HFY_STRINGS[base] && HFY_STRINGS[base][k];
      return s != null ? s : (HFY_STRINGS.en[k] != null ? HFY_STRINGS.en[k] : k);
    },
    resolvedLang: function () { return this.config.lang || navigator.language || 'en'; },
    resolvedDir: function () {
      if (this.config.dir && this.config.dir !== 'auto') return this.config.dir;
      return HFY_RTL.indexOf(this.resolvedLang().split('-')[0]) !== -1 ? 'rtl' : 'ltr';
    },
    resolvedTheme: function () {
      if (this.config.theme && this.config.theme !== 'auto') return this.config.theme;
      return (window.matchMedia && matchMedia('(prefers-color-scheme: dark)').matches) ? 'dark' : 'light';
    },
    applyDom: function () {
      this.config.lang = this.resolvedLang(); hfyConfig = this.config;
      var el = document.documentElement, self = this, accent = this.config.accent || HFY_DEFAULTS.accent, L = {};
      el.lang = this.config.lang; el.dir = this.resolvedDir();
      el.dataset.theme = this.resolvedTheme();
      el.dataset.density = this.config.density || 'comfortable';
      el.dataset.layout = this.config.layout || 'bubbles';
      el.style.setProperty('--hfy-accent', accent);
      el.style.setProperty('--hfy-on-accent', hfyOnColor(accent));
      Object.keys(HFY_STRINGS.en).forEach(function (k) { L[k] = self.t(k); });
      this.L = L;
      document.title = L.title;
      document.querySelectorAll('.hfy-code-copy').forEach(function (b) { b.textContent = L.copy; b.setAttribute('aria-label', L.copyCode); });
      this.relayout();
      this.syncDerived();
    },
    syncDerived: function () {
      var s = this.connState;
      // thin bar only while not live; errors add Retry
      this.statusShow = s !== 'live' && s !== 'idle';
      this.statusText = this.statusShow ? this.t(s) : '';
      this.statusCls = 'is-' + s;
      this.showRetry = s === 'error';
      this.showWelcome = this._loaded && !this.turns.length && !this.hasOlder;
      // turn.own / turn.state come from folded events, so a reload mid-turn
      // lands back in exactly this state
      // _awaitTurn bridges send() resolving and that turn's events arriving
      var wait = this._awaitTurn;
      if (wait && this.turns.some(function (t) { return t.id === wait && t.blocks.some(function (b) { return b.isText && b.isUser; }); })) this._awaitTurn = '';
      this.ownActive = !!this._awaitTurn || this.turns.some(function (t) { return t.own && HFY_ACTIVE.indexOf(t.state) !== -1; });
      this.updateSend();
      this.syncSheet();
      this.syncAttach();
    },
    syncAttach: function () {
      var u = this.config.uploads, imgs = u ? u.types.filter(hfyIsImageType) : [], docs = u ? u.types.filter(function (t) { return !hfyIsImageType(t); }) : [];
      this.showAttach = !!u;
      this.showCamera = !!u && this.coarse && imgs.length > 0;
      this.enterHint = this.coarse ? 'enter' : 'send';
      this.attachDisabled = !u || !hfyClient || this.ownActive; // blocked like sending while the own turn runs
      this.photoAccept = imgs.join(',');
      this.docAccept = docs.join(',');
      this.attachOpts = { photos: imgs.length > 0, camera: this.coarse && imgs.length > 0, document: docs.length > 0 };
      if (this.attachDisabled && this.attachMenuOpen) this.attachMenuOpen = false;
      this.syncModal();
    },
    applyConfigure: function (cfg) {
      var self = this;
      ['lang', 'dir', 'theme', 'accent', 'density', 'layout'].forEach(function (k) {
        if (cfg[k] != null) self.config[k] = cfg[k];
      });
      if (HFY_TOOL_MODES.indexOf(cfg.tool_calls) !== -1) this.config.tool_calls = cfg.tool_calls;
      if (cfg.strings && typeof cfg.strings === 'object') this.customStrings = Object.assign({}, this.customStrings, cfg.strings);
      this.applyDom();
    },
    requestGrant: function (reason) { return hfyRequestGrant(reason); },

    init: async function () {
      this.config = hfyResolveConfig(); hfyConfig = this.config;
      try { this.tz = Intl.DateTimeFormat().resolvedOptions().timeZone || ''; } catch (e) { this.tz = ''; }
      this.applyDom();
      hfyBridge.component = this;
      if (hfyBridge.pendingConfigure) { this.applyConfigure(hfyBridge.pendingConfigure); hfyBridge.pendingConfigure = null; }
      var self = this;
      if (window.matchMedia) {
        var mq = matchMedia('(prefers-color-scheme: dark)'), onScheme = function () { if (!self.config.theme || self.config.theme === 'auto') self.applyDom(); };
        if (mq.addEventListener) mq.addEventListener('change', onScheme); else if (mq.addListener) mq.addListener(onScheme);
        // touch-first devices: Enter is a newline, and the camera shortcut appears
        var pq = matchMedia('(pointer: coarse)'), onPointer = function () { self.coarse = pq.matches; self.syncAttach(); };
        if (pq.addEventListener) pq.addEventListener('change', onPointer); else if (pq.addListener) pq.addListener(onPointer);
        onPointer();
      }
      // the pane shrinks when chips, a taller draft or a mobile keyboard appear
      if (window.ResizeObserver && this.scroller()) new ResizeObserver(function () { self.keepBottom(); }).observe(this.scroller());
      hfySendToHost('ready', {});
      hfyClient = new HfyClient({ api: location.origin, onTokenNeeded: function (r) { return self.requestGrant(r); } });
      hfyClient.onEvent(function (env, isHistory) { self.fold(env, isHistory); });
      hfyClient.onState(function (s) { self.connState = s; self.syncDerived(); });
      try { await hfyClient.connect(); this._loaded = true; } catch (e) { this.connState = 'error'; }
      this.hasOlder = hfyClient.hasOlder;
      this.syncDerived(); this.updateSend();
      this.scrollToBottom(false);
    },
    retry: async function () {
      if (hfyClient && hfyClient.state === 'live') { this.connState = hfyClient.state; this.syncDerived(); return; }
      this.connState = 'connecting'; this.syncDerived();
      try { await hfyClient.connect(); this._loaded = true; this.hasOlder = hfyClient.hasOlder; } catch (e) { this.connState = 'error'; }
      this.syncDerived();
    },

    ensureTurn: function (list, id) {
      for (var i = 0; i < list.length; i++) if (list[i].id === id) return list[i];
      var turn = { id: id, state: 'queued', stateSeen: false, agent: '', error: '', blocks: [], showTyping: false, typingCls: '', own: false, userLabel: '' };
      list.push(turn); return turn;
    },
    findTool: function (turn, callId) {
      for (var i = 0; i < turn.blocks.length; i++)
        if (turn.blocks[i].kind === 'tool' && turn.blocks[i].callId === callId) return turn.blocks[i];
      return null;
    },
    streamingBlock: function (turn, kind) {
      for (var i = turn.blocks.length - 1; i >= 0; i--) {
        var b = turn.blocks[i];
        if (b.kind === kind && b.streaming) return b;
      }
      return null;
    },

    // Every block carries every field the template reads: the Alpine CSP
    // build refuses (and logs) any property path that resolves to undefined.
    // Display strings are (re)computed in relayout().
    baseBlock: function (env, kind, isAgent) {
      var a = env.actor || {}, ts = Date.parse(env.created_at);
      if (isNaN(ts)) ts = Date.now();
      return { id: env.id, kind: kind, isText: kind === 'text', isReasoning: kind === 'reasoning', isTool: kind === 'tool',
        isAgent: isAgent, isUser: !isAgent, ref: a.ref || '', actorUuid: a.uuid || '', actorKey: (isAgent ? 'a:' + (a.ref || 'main') : 'u:' + (a.ref || a.uuid || '')),
        actorLabel: '', avatarText: '', avatarCls: '', ts: ts, iso: new Date(ts).toISOString(), dayKey: this.dayKey(ts),
        timeText: '', fullTime: '', showDay: false, dayText: '', showHead: true, rowCls: '', bubbleCls: '',
        text: '', images: [], files: [], hasImages: false, hasFiles: false, streaming: false, copied: false,
        isOwn: false, showBubbleName: false, nameCls: '', tickCls: '', tickLabel: '', endsInText: true, endsFor: null,
        callId: '', name: '', niceName: '', toolLabel: '', argsText: '', argsRaw: null, argLang: '', argList: [], hasArgList: false, showRawArgs: false,
        status: '', statusCls: '', statusText: '', isRunning: false,
        content: '', structuredText: '', showApproval: false, showSummary: false, showDetails: false, showDecided: false, showBadge: false, decision: '',
        decidedText: '', decidedCls: '', busy: false, hasDetails: false,
        showPendingOwn: false, showPendingOther: false, waitingText: '', choice: '', submitted: false };
    },
    setAtts: function (b, list) {
      var atts = this.mapAtts(list);
      b.images = atts.filter(function (a) { return a.isImage; });
      b.files = atts.filter(function (a) { return !a.isImage; });
      b.hasImages = b.images.length > 0; b.hasFiles = b.files.length > 0;
    },
    newTextBlock: function (env, isAgent, d) {
      var b = this.baseBlock(env, 'text', isAgent);
      b.text = d.text || ''; this.setAtts(b, d.attachments);
      return b;
    },
    newToolBlock: function (env, d) {
      var b = this.baseBlock(env, 'tool', true);
      b.callId = d.call_id || ''; b.name = d.name || ''; b.argsText = hfyPretty(d.arguments);
      b.argsRaw = d.arguments === undefined ? null : d.arguments; // argList is built (localized) in relayout
      this.setToolStatus(b, 'running');
      return b;
    },
    setToolStatus: function (b, status) { b.status = status; b.statusCls = 'is-' + status; b.isRunning = status === 'running'; },
    mapAtts: function (list) {
      var self = this;
      return (Array.isArray(list) ? list : []).map(function (a, i) {
        a = a || {};
        var url = hfySafeUrl(a.url), mime = String(a.mime || ''), img = !!url && /^image\//i.test(mime);
        return { k: (a.file || a.url || mime) + ':' + i, url: url, label: String(a.name || mime || self.t('file')),
          isImage: img, isLink: !!url && !img, isPlain: !url };
      });
    },
    actorLabelFor: function (b) {
      if (b.isAgent) return (!b.ref || b.ref === 'main') ? this.t('agent') : b.ref;
      return b.ref || this.t('you');
    },

    fold: function (env, isHistory) {
      if (!env || !env.type) return;
      var list = (isHistory && this._collecting) || this.turns, d = hfyData(env.data);
      var turnId = env.type === 'turn' ? env.id : (env.parent || env.id);
      var turn, b, layout = true;
      switch (env.type) {
        case 'turn':
          turn = this.ensureTurn(list, turnId); turn.agent = d.agent || turn.agent; break;
        case 'turn.state':
          turn = this.ensureTurn(list, turnId); turn.stateSeen = true; turn.state = d.state || turn.state;
          if (d.error) turn.error = d.error; break;
        case 'message.text': {
          turn = this.ensureTurn(list, turnId);
          var isAgent = !!(env.actor && env.actor.type === 'agent');
          b = isAgent ? this.streamingBlock(turn, 'text') : null;
          if (b) {
            var nb = this.newTextBlock(env, true, d);
            b.text = nb.text; b.images = nb.images; b.files = nb.files; b.hasImages = nb.hasImages; b.hasFiles = nb.hasFiles; b.streaming = false;
            b.id = env.id; b.ts = nb.ts; b.iso = nb.iso; b.dayKey = nb.dayKey;
          } else turn.blocks.push(this.newTextBlock(env, isAgent, d));
          break;
        }
        case 'text.delta':
          turn = this.ensureTurn(list, turnId);
          b = this.streamingBlock(turn, 'text');
          if (b) layout = false;
          else { b = this.newTextBlock(env, true, {}); b.id = turnId + ':stream'; b.streaming = true; turn.blocks.push(b); }
          b.text += d.text || ''; break;
        case 'reasoning.delta':
          turn = this.ensureTurn(list, turnId);
          b = this.streamingBlock(turn, 'reasoning');
          if (b) layout = false;
          else { b = this.baseBlock(env, 'reasoning', true); b.id = turnId + ':rstream'; b.streaming = true; turn.blocks.push(b); }
          b.text += d.text || ''; break;
        case 'reasoning':
          turn = this.ensureTurn(list, turnId);
          b = this.streamingBlock(turn, 'reasoning');
          if (b) { b.text = d.text || ''; b.streaming = false; b.id = env.id; }
          else { b = this.baseBlock(env, 'reasoning', true); b.text = d.text || ''; turn.blocks.push(b); }
          break;
        case 'tool_call':
          turn = this.ensureTurn(list, turnId);
          turn.blocks.push(this.newToolBlock(env, d)); break;
        case 'tool_result':
          turn = this.ensureTurn(list, turnId);
          b = this.findTool(turn, d.call_id);
          if (!b) { b = this.newToolBlock(env, { call_id: d.call_id }); turn.blocks.push(b); }
          this.setToolStatus(b, d.is_error ? 'error' : 'done');
          b.content = d.content || '';
          if (d.structured != null) b.structuredText = hfyPretty(d.structured); break;
        case 'tool_call.interrupted':
          turn = this.ensureTurn(list, turnId);
          b = this.findTool(turn, d.call_id); if (b) this.setToolStatus(b, 'interrupted'); break;
        case 'approval.requested':
          turn = this.ensureTurn(list, turnId);
          b = this.findTool(turn, d.call_id);
          if (!b) { b = this.newToolBlock(env, { call_id: d.call_id }); turn.blocks.push(b); }
          if (!b.showDecided) b.showApproval = true; break;
        case 'approval.decided':
          turn = this.ensureTurn(list, turnId);
          b = this.findTool(turn, d.call_id);
          if (b) { b.showApproval = false; b.showDecided = true; b.busy = false; b.decision = d.approve ? 'allowed' : 'denied'; }
          break;
        default: return;
      }
      if (this._collecting && isHistory) return; // loadOlder merges and lays out once
      if (layout) this.relayout();
      this.syncDerived();
      if (this._stick) this.scrollToBottom(false);
      else if (!isHistory) this.showNewPill = true;
    },

    dayKey: function (ts) {
      return hfyFmt('en-CA', { timeZone: this.tz || undefined, year: 'numeric', month: '2-digit', day: '2-digit' }).format(ts);
    },
    fmtTime: function (ts) {
      if (ts == null || ts === '') return '';
      try { return hfyFmt(this.resolvedLang(), { hour: 'numeric', minute: '2-digit', timeZone: this.tz || undefined }).format(new Date(ts)); }
      catch (e) { return ''; }
    },
    fmtDay: function (ts) {
      var opts = { weekday: 'long', month: 'long', day: 'numeric', timeZone: this.tz || undefined };
      if (new Date(ts).getFullYear() !== new Date().getFullYear()) opts.year = 'numeric';
      try { return hfyFmt(this.resolvedLang(), opts).format(new Date(ts)); } catch (e) { return ''; }
    },
    // Presentation pass: grouping, day separators, typing and every
    // locale-dependent label. Cheap enough to run on each structural event.
    relayout: function () {
      var self = this, now = Date.now(), today = this.dayKey(now), yest = this.dayKey(now - 864e5);
      var prevKey = '', prevDay = '', prevTs = 0, firstActive = true, lang = this.resolvedLang();
      var fullFmt = hfyFmt(lang, { dateStyle: 'medium', timeStyle: 'short', timeZone: this.tz || undefined });
      var mode = this.config.tool_calls, viewer = hfyClient && hfyClient.viewer, people = {}, pendingOwn = [];
      var yesNo = { yes: this.t('yes'), no: this.t('no') }, argKey = lang + '|' + yesNo.yes + '|' + yesNo.no;
      this.turns.forEach(function (t) { t.blocks.forEach(function (b) { if (b.isText && b.isUser) people[b.actorKey] = 1; }); });
      var multi = Object.keys(people).length > 1;
      this.turns.forEach(function (turn) {
        var terminal = HFY_TERMINAL.indexOf(turn.state) !== -1, tick = hfyTick(turn.state);
        // a turn is the viewer's when its user message is
        var starter = turn.blocks.find(function (b) { return b.isText && b.isUser; });
        turn.own = !!starter && !!viewer && !!viewer.uuid && starter.actorUuid === viewer.uuid;
        turn.userLabel = starter ? self.actorLabelFor(starter) : '';
        turn.blocks.forEach(function (b) {
          if (terminal && b.streaming) b.streaming = false;
          var hidden = false;
          if (b.isTool) {
            // pending approvals stay visible in every mode: the viewer's own
            // open the sheet; anyone else's are a read-only "waiting" note
            b.showPendingOwn = b.showApproval && turn.own;
            b.showPendingOther = b.showApproval && !turn.own;
            b.waitingText = turn.own ? self.t('waitingYou') : turn.userLabel ? self.t('waitingFor').replace('{name}', turn.userLabel) : self.t('approvalNeeded');
            if (b.showPendingOwn && !b.submitted) pendingOwn.push(b);
            b.niceName = hfyToolName(b.name) || self.t('tool');
            if (b.argLang !== argKey) { b.argList = hfyArgList(b.argsRaw, yesNo); b.hasArgList = b.argList.length > 0; b.argLang = argKey; }
            b.toolLabel = b.name || self.t('tool');
            // a denied call still gets a tool_result; "Denied" is the status that matters
            var denied = b.decision === 'denied';
            b.statusText = denied ? self.t('denied') : self.t(b.status === 'error' ? 'failed' : b.status);
            b.statusCls = 'is-' + (denied ? 'denied' : b.status);
            b.showBadge = b.decision === 'allowed';
            b.decidedText = b.decision ? self.t(b.decision) : '';
            b.decidedCls = b.decision ? 'is-' + b.decision : '';
            b.hasDetails = !!(b.argsText || b.content || b.structuredText);
            b.showRawArgs = mode === 'details' && !!b.argsText;
            // hidden mode still keeps decided approvals: the viewer made that call
            b.showSummary = !b.showApproval && (mode === 'summary' || (mode === 'hidden' && !!b.decision));
            b.showDetails = !b.showApproval && mode === 'details';
            hidden = !b.showApproval && !b.showSummary && !b.showDetails;
          }
          if (b.isText && b.endsFor !== b.text) {
            var leaf = b.isAgent ? hfyMdLastLeaf(hfyMdParse(b.text)) : null;
            b.endsFor = b.text;
            b.endsInText = !!b.text.trim() && (!b.isAgent || (!!leaf && (leaf.t === 'p' || leaf.t === 'h')));
          }
          var own = !!viewer && !!viewer.uuid && b.actorUuid === viewer.uuid;
          var showDay = !hidden && b.dayKey !== prevDay;
          var head = !hidden && (showDay || b.actorKey !== prevKey || (b.isText && b.ts - prevTs > 5 * 60000));
          if (!hidden) { prevDay = b.dayKey; prevKey = b.actorKey; prevTs = b.ts; }
          b.isOwn = own;
          b.showDay = showDay;
          b.dayText = !showDay ? '' : b.dayKey === today ? self.t('today') : b.dayKey === yest ? self.t('yesterday') : self.fmtDay(b.ts);
          b.showHead = head;
          b.rowCls = (b.isAgent ? 'is-agent' : 'is-user') + (own ? ' is-own' : ' is-other') + ' is-' + b.kind +
            (head ? ' is-head' : ' is-grouped') + (hidden ? ' is-hidden' : '');
          b.showBubbleName = head && b.isText && !own && (multi || (b.isAgent && !!b.ref && b.ref !== 'main'));
          b.bubbleCls = (b.streaming ? 'is-streaming' : '') + (b.endsInText && !b.hasFiles ? '' : ' is-padfoot') +
            (b.hasImages ? ' has-images' : '') + (b.showBubbleName ? ' has-name' : '');
          b.actorLabel = self.actorLabelFor(b);
          b.nameCls = 'hfy-hue-' + hfyHue(b.actorKey);
          b.tickCls = own ? tick.cls : '';
          b.tickLabel = own ? self.t(tick.label) : '';
          var bot = b.isAgent && (!b.ref || b.ref === 'main');
          b.avatarCls = bot ? 'is-bot' : (b.isUser && !b.ref) ? 'is-anon' : 'is-named';
          b.avatarText = b.avatarCls === 'is-named' ? hfyInitials(b.actorLabel) : '';
          b.timeText = self.fmtTime(b.ts);
          try { b.fullTime = fullFmt.format(new Date(b.ts)); } catch (e) { b.fullTime = ''; }
        });
        var active = turn.state === 'queued' || turn.state === 'running', last = turn.blocks[turn.blocks.length - 1];
        // a hidden tool shows no spinner, so the typing dots stand in for it
        var busy = !!last && ((last.isText && last.isAgent) || (last.isTool && (last.showApproval || (last.isRunning && mode !== 'hidden'))) || (last.isReasoning && last.streaming));
        turn.showTyping = active && firstActive && !busy;
        turn.typingCls = prevKey.indexOf('a:') === 0 ? 'is-grouped' : 'is-head';
        if (active || turn.state === 'awaiting_approval') firstActive = false;
      });
      this.pendingOwn = pendingOwn;
    },

    scroller: function () { return document.querySelector('.hfy-messages'); },
    keepBottom: function () {
      if (!this._stick || this._collecting) return;
      var m = this.scroller(); if (m) m.scrollTop = m.scrollHeight;
    },
    scrollToBottom: function (smooth) {
      var self = this;
      requestAnimationFrame(function () {
        var m = self.scroller(); if (!m) return;
        var reduce = window.matchMedia && matchMedia('(prefers-reduced-motion: reduce)').matches;
        if (smooth && !reduce && m.scrollTo) m.scrollTo({ top: m.scrollHeight, behavior: 'smooth' });
        else m.scrollTop = m.scrollHeight;
      });
    },
    jumpToBottom: function () { this._stick = true; this.showNewPill = false; this.scrollToBottom(true); },
    onScroll: function (ev) {
      var m = ev.target;
      this._stick = m.scrollHeight - m.scrollTop - m.clientHeight < 80;
      if (this._stick && this.showNewPill) this.showNewPill = false;
    },

    loadOlder: async function () {
      if (!hfyClient || this.loadingOlder) return;
      // anchor on the first visible message row: the old first block may lose
      // its day separator, so a plain scrollHeight delta drifts
      var m = this.scroller(), anchor = null, anchorTop = 0;
      if (m) {
        var top = m.getBoundingClientRect().top;
        anchor = Array.from(m.querySelectorAll('.hfy-row')).find(function (r) { return r.getBoundingClientRect().bottom > top; }) || null;
        if (anchor) anchorTop = anchor.getBoundingClientRect().top;
      }
      this.loadingOlder = true; this._collecting = [];
      try {
        await hfyClient.loadOlder();
        var older = this._collecting; this._collecting = null;
        var byId = {}; this.turns.forEach(function (t) { byId[t.id] = t; });
        var prepend = [];
        older.forEach(function (ct) {
          var existing = byId[ct.id];
          if (!existing) { prepend.push(ct); return; }
          var seenBlocks = {};
          existing.blocks.forEach(function (b) { seenBlocks[b.id] = 1; });
          ct.blocks.forEach(function (b) { if (!seenBlocks[b.id]) { existing.blocks.push(b); seenBlocks[b.id] = 1; } });
          // only a collected turn that actually saw a turn.state event may overwrite state
          if (ct.stateSeen && ct.state) existing.state = ct.state;
          if (ct.error) existing.error = ct.error;
        });
        this.turns = prepend.concat(this.turns);
        this.hasOlder = hfyClient.hasOlder;
        this.relayout(); this.syncDerived();
        requestAnimationFrame(function () {
          if (anchor && anchor.isConnected) m.scrollTop += anchor.getBoundingClientRect().top - anchorTop;
        });
      } catch (e) { this._collecting = null; }
      this.loadingOlder = false;
    },

    inputEl: function () { return document.querySelector('.hfy-input'); },
    autosize: function (el) {
      el = el || this.inputEl(); if (!el) return;
      el.style.height = 'auto';
      el.style.height = el.scrollHeight + 'px'; // CSS max-height caps it at ~8 lines
    },
    syncInput: function () { var self = this; requestAnimationFrame(function () { self.autosize(); }); },
    updateSend: function () {
      this.sendDisabled = !hfyClient || this.ownActive || !this.draft.trim();
      this.roundCls = this.ownActive ? 'is-stop' : this.sendDisabled ? 'is-send is-empty' : 'is-send';
      this.roundLabel = this.t(this.ownActive ? 'stop' : 'send');
      this.roundDisabled = this.ownActive ? !hfyClient : this.sendDisabled;
    },
    onRound: function () { if (this.ownActive) this.doStop(); else this.sendMsg(); },
    // Fine pointer: Enter sends, Shift+Enter breaks the line. Touch: Enter is
    // always a newline (send with the button, as on WhatsApp mobile). Never
    // while an IME is composing.
    enterSends: function (ev) { return !(ev.isComposing || ev.keyCode === 229 || ev.shiftKey || this.coarse); },
    onEnter: function (ev) { if (!this.enterSends(ev)) return; ev.preventDefault(); this.sendMsg(); },
    draftChanged: function (ev) { this.draft = ev.target.value; this.autosize(ev.target); this.composerError = ''; this.updateSend(); },
    sendMsg: async function () {
      if (!hfyClient || this.ownActive) return;
      var text = this.draft.trim();
      if (!text) return;
      this.draft = ''; this.updateSend(); this.syncInput();
      var err = await this.post(text, undefined);
      if (!err) return;
      if (!this.draft) this.draft = text;
      this.updateSend(); this.syncInput();
      this.composerError = err;
    },
    // one message out; '' on success, else the error to show
    post: async function (text, atts) {
      this._stick = true; this.showNewPill = false;
      try {
        var res = await hfyClient.send(text, atts);
        this.composerError = '';
        if (res && res.turn) { this._awaitTurn = res.turn; this.syncDerived(); }
        return '';
      } catch (e) { return (e && e.message) || this.t('error'); }
    },

    // ---- attach menu (bottom sheet on touch / narrow, popover above 📎 otherwise)
    syncModal: function () { this.modalOpen = this.sheetOpen || this.attachMenuOpen || this.pvOpen; },
    toggleAttachMenu: function () {
      if (this.attachMenuOpen) { this.closeAttachMenu(true); return; }
      if (this.attachDisabled) return;
      this.attachMenuOpen = true; this.syncModal();
      hfyFocusSoon('.hfy-opt');
    },
    closeAttachMenu: function (refocus) {
      if (!this.attachMenuOpen) return;
      this.attachMenuOpen = false; this.syncModal();
      if (refocus) hfyFocusSoon('.hfy-attach');
    },
    dismissAttachMenu: function () { this.closeAttachMenu(true); },
    onMenuKey: function (ev) {
      if (ev.key === 'Escape') { ev.preventDefault(); this.closeAttachMenu(true); return; }
      hfyTrap(ev);
    },
    pickPhotos: function () { this.pick('.hfy-in-photos'); },
    pickCamera: function () { this.pick('.hfy-in-camera'); },
    pickDocs: function () { this.pick('.hfy-in-docs'); },
    pickMore: function () { this.pick(this.pvKind === 'photo' ? '.hfy-in-photos' : '.hfy-in-docs'); },
    pick: function (sel) {
      if (this.attachDisabled) return;
      this.closeAttachMenu(false);
      var f = document.querySelector(sel);
      if (f) f.click(); // still inside the click's user gesture, so the picker opens
    },
    onPicked: function (ev) {
      var files = Array.from(ev.target.files || []);
      ev.target.value = '';
      this.openFiles(files);
    },

    // ---- pre-checks, then the photo / document preview
    checkFile: function (file) {
      var u = this.config.uploads, mime = String(file.type || '').toLowerCase();
      if (!u || u.types.indexOf(mime) === -1) return this.t('errType').replace('{name}', file.name || mime || this.t('file'));
      if (file.size > u.max_bytes) return this.t('errSize').replace('{size}', hfySize(u.max_bytes, this.resolvedLang()));
      return '';
    },
    // Rejected files never upload. Images open (or join) the photo preview,
    // documents the document preview; mixed drops go to the photo preview.
    openFiles: function (files) {
      if (this.attachDisabled || !files.length) return;
      var self = this, ok = [], errs = [];
      files.forEach(function (f) { var e = self.checkFile(f); if (!e) ok.push(f); else if (errs.indexOf(e) === -1) errs.push(e); });
      var err = errs.join('\n');
      if (!ok.length) {
        if (this.pvOpen) { this.pvError = err; this.syncPreview(); } else this.composerError = err;
        return;
      }
      var hasImg = ok.some(function (f) { return hfyIsImageType(f.type); }), wasOpen = this.pvOpen;
      if (!wasOpen) { this.pvItems = []; this.pvSel = ''; this.pvCaption = ''; this.pvSending = false; this.pvConfirm = false; this.pvKind = hasImg ? 'photo' : 'doc'; }
      else if (hasImg) this.pvKind = 'photo';
      this.pvError = err; this.composerError = '';
      ok.forEach(function (f) { self.addItem(f); });
      this.pvOpen = true; this.syncModal(); this.syncPreview();
      if (!wasOpen) hfyFocusSoon(this.coarse ? '.hfy-preview' : '.hfy-pv-caption');
    },
    // Uploads start at once; the preview's Send waits for them.
    addItem: function (file) {
      var self = this, img = hfyIsImageType(file.type), name = file.name || this.t('file');
      var ext = (/\.([a-z0-9]{1,5})$/i.exec(name) || [])[1] || String(file.type || '').split('/').pop().slice(0, 4);
      this.pvItems.push({ k: 'p' + (++hfySeq), name: name, sizeText: hfySize(file.size, this.resolvedLang()), isImage: img, url: '',
        ext: String(ext).toUpperCase(), docCls: hfyDocCls(file.type), uploading: true, ready: false, failed: false, error: '', att: null,
        cls: 'is-uploading', selected: false, removeLabel: this.t('remove') + ': ' + name });
      var it = this.pvItems[this.pvItems.length - 1]; // the reactive proxy, so later updates render
      if (img && window.FileReader) {
        // a data: URL, because the page CSP allows data: images but not blob:
        var r = new FileReader();
        r.onload = function () { it.url = String(r.result || ''); self.syncPreview(); };
        r.readAsDataURL(file);
      }
      hfyClient.upload(file).then(function (att) { it.att = att; it.ready = true; },
        function (e) { it.failed = true; it.error = self.t('uploadFailed') + ': ' + name + ((e && e.message) ? ' (' + e.message + ')' : ''); })
        .then(function () { it.uploading = false; self.syncPreview(); self.flushPreview(); });
    },
    syncPreview: function () {
      var self = this, items = this.pvItems, sel = null, settled = 0, busy = false;
      var imgs = items.filter(function (i) { return i.isImage; }), docs = items.filter(function (i) { return !i.isImage; });
      if (!imgs.some(function (i) { return i.k === self.pvSel; })) this.pvSel = imgs.length ? imgs[0].k : '';
      items.forEach(function (i) {
        i.selected = i.k === self.pvSel;
        if (i.selected) sel = i;
        if (i.uploading) busy = true; else settled++;
        i.cls = (i.uploading ? 'is-uploading' : i.failed ? 'is-failed' : 'is-ready') + (i.selected ? ' is-selected' : '');
      });
      var photo = this.pvKind === 'photo';
      this.preview = { cls: 'is-' + this.pvKind, isPhoto: photo, isDoc: !photo, images: imgs, docs: docs, hasDocs: docs.length > 0,
        mainUrl: sel ? sel.url : '', mainName: sel ? sel.name : '', title: this.t(photo ? 'photos' : 'document'),
        progress: items.length ? settled / items.length : 1,
        sendCls: (busy ? 'has-progress' : '') + (this.pvSending ? ' is-busy' : ''),
        sendDisabled: this.pvSending || !hfyClient || this.ownActive ||
          (!items.some(function (i) { return !i.failed; }) && !this.pvCaption.trim()) };
    },
    selectItem: function () { if (this.it) { this.pvSel = this.it.k; this.syncPreview(); } },
    removeItem: function () {
      var k = this.it && this.it.k;
      this.pvItems = this.pvItems.filter(function (i) { return i.k !== k; });
      if (!this.pvItems.length) { this.discardPreview(); return; }
      this.pvError = ''; this.syncPreview();
    },
    captionChanged: function (ev) { this.pvCaption = ev.target.value; this.autosize(ev.target); this.syncPreview(); },
    onCaptionEnter: function (ev) { if (!this.enterSends(ev)) return; ev.preventDefault(); this.sendPreview(); },
    sendPreview: function () {
      if (this.preview.sendDisabled) return;
      this.pvSending = true; this.pvError = ''; this.syncPreview();
      this.flushPreview();
    },
    // Runs on Send and after every upload settles; posts once nothing is in
    // flight. A failed upload stops the send: never post a partial set silently.
    flushPreview: async function () {
      if (!this.pvSending || this._pvPosting || this.pvItems.some(function (i) { return i.uploading; })) return;
      var failed = this.pvItems.filter(function (i) { return i.failed; });
      if (failed.length) { this.pvSending = false; this.pvError = failed[0].error; this.syncPreview(); return; }
      var atts = this.pvItems.filter(function (i) { return i.ready; }).map(function (i) { return i.att; });
      this._pvPosting = true;
      var err = await this.post(this.pvCaption.trim(), atts.length ? atts : undefined);
      this._pvPosting = false;
      if (!err) { this.discardPreview(); return; }
      this.pvSending = false; this.pvError = err; this.syncPreview();
    },
    closePreview: function () {
      if (this.pvSending) return;
      if (this.pvCaption.trim() && !this.pvConfirm) { this.pvConfirm = true; hfyFocusSoon('.hfy-pv-keep'); return; }
      this.discardPreview();
    },
    keepPreview: function () { this.pvConfirm = false; hfyFocusSoon('.hfy-pv-caption'); },
    // closing discards: the uploaded objects are left for the server's cleanup
    discardPreview: function () {
      this.pvOpen = false; this.pvItems = []; this.pvSel = ''; this.pvCaption = ''; this.pvSending = false; this.pvError = ''; this.pvConfirm = false;
      var cap = document.querySelector('.hfy-pv-caption');
      if (cap) cap.style.height = '';
      this.syncModal(); this.syncPreview();
      hfyFocusSoon('.hfy-input');
    },
    onPreviewKey: function (ev) {
      if (ev.key === 'Escape') { ev.preventDefault(); if (this.pvConfirm) this.keepPreview(); else this.closePreview(); return; }
      hfyTrap(ev);
    },

    // ---- drop & paste feed the same previews
    hasFiles: function (ev) {
      var dt = ev.dataTransfer;
      return !!dt && Array.from(dt.types || []).indexOf('Files') !== -1;
    },
    canDrop: function (ev) { return this.hasFiles(ev) && !this.attachDisabled && !this.sheetOpen && !this.attachMenuOpen; },
    onDragEnter: function (ev) { if (!this.canDrop(ev)) return; ev.preventDefault(); this._dragDepth++; this.dragOver = true; },
    onDragOver: function (ev) { if (!this.canDrop(ev)) return; ev.preventDefault(); ev.dataTransfer.dropEffect = 'copy'; },
    onDragLeave: function (ev) {
      if (!this.hasFiles(ev)) return;
      if (--this._dragDepth <= 0) { this._dragDepth = 0; this.dragOver = false; }
    },
    onDrop: function (ev) {
      if (!this.hasFiles(ev)) return;
      ev.preventDefault(); this._dragDepth = 0; this.dragOver = false;
      if (this.canDrop(ev)) this.openFiles(Array.from(ev.dataTransfer.files || []));
    },
    onPaste: function (ev) {
      var cd = ev.clipboardData;
      // rich-text pastes may carry an image rendition too; let text win
      if (this.attachDisabled || !cd || !cd.files || !cd.files.length || Array.from(cd.types || []).indexOf('text/plain') !== -1) return;
      ev.preventDefault(); this.openFiles(Array.from(cd.files));
    },
    doStop: async function () {
      if (!hfyClient) return;
      if (this._awaitTurn) { this._awaitTurn = ''; this.syncDerived(); }
      try { await hfyClient.stop(); this.composerError = ''; } catch (e) { this.composerError = (e && e.message) || this.t('error'); }
    },
    copyBlock: function () {
      var b = this.block;
      if (!b) return;
      hfyCopy(b.text).then(function () {
        b.copied = true;
        setTimeout(function () { b.copied = false; }, 1500);
      }, function () { /* clipboard denied */ });
    },

    // ---- approval sheet: a stepper over the viewer's own pending approvals.
    // Choices live on the blocks; nothing reaches the server before Allow/Deny
    // (single) or Submit (several). Dismissing only minimizes.
    syncSheet: function () {
      var self = this, list = this.pendingOwn, n = list.length, fresh = false;
      list.forEach(function (b) { if (!self._seenPending[b.callId]) { self._seenPending[b.callId] = 1; fresh = true; } });
      if (fresh) this._sheetMin = false; // a new request (or a reload) surfaces the sheet
      if (!n) this.sheetError = '';
      if (this.sheetStep > n - 1) this.sheetStep = Math.max(0, n - 1);
      var step = this.sheetStep, b = list[step], multi = n > 1, last = step === n - 1;
      this.pendingCount = String(n);
      this.showActionPill = n > 0 && this._sheetMin;
      this.sheet = { title: b ? b.niceName : '', argList: b ? b.argList : [], hasArgList: !!b && b.hasArgList,
        showRawArgs: !!b && this.config.tool_calls === 'details' && !!b.argsText, argsText: b ? b.argsText : '',
        isMulti: multi, stepText: multi ? this.t('stepOf').replace('{n}', step + 1).replace('{total}', n) : '',
        dots: multi ? list.map(function (x, i) { return { k: x.callId || String(i), cls: (i === step ? 'is-current' : '') + (x.choice ? ' is-' + x.choice : '') }; }) : [],
        allowPressed: multi ? (!!b && b.choice === 'allowed') : null, denyPressed: multi ? (!!b && b.choice === 'denied') : null,
        showBack: multi && step > 0, showNext: multi && !last, showSubmit: multi && last,
        submitDisabled: this.sheetBusy || list.some(function (x) { return !x.choice; }), showDecide: !!b };
      var open = n > 0 && !this._sheetMin;
      if (open !== this.sheetOpen) { this.sheetOpen = open; this.syncModal(); this.sheetFocus(open, n > 0); }
    },
    sheetFocus: function (open, pending) {
      var self = this;
      // after x-show (applied on Alpine's next tick) has revealed the target
      setTimeout(function () {
        var a = document.activeElement, sheet = document.querySelector('.hfy-sheet');
        if (open) { if (sheet) sheet.focus(); return; }
        var pill = document.querySelector('.hfy-action-pill');
        if (pending && pill) { pill.focus(); return; }
        if (!a || a === document.body || (sheet && sheet.contains(a))) { var i = self.inputEl(); if (i) i.focus(); }
      }, 30);
    },
    minimizeSheet: function () { this._sheetMin = true; this.syncSheet(); },
    openSheet: function () { this._sheetMin = false; this.syncSheet(); },
    openApproval: function () {
      var id = this.block && this.block.callId;
      var i = this.pendingOwn.findIndex(function (b) { return b.callId === id; });
      if (i >= 0) this.sheetStep = i;
      this.openSheet();
    },
    sheetBack: function () { if (this.sheetStep > 0) this.sheetStep--; this.syncSheet(); },
    sheetNext: function () { if (this.sheetStep < this.pendingOwn.length - 1) this.sheetStep++; this.syncSheet(); },
    sheetAllow: function () { this.sheetChoose('allowed'); },
    sheetDeny: function () { this.sheetChoose('denied'); },
    sheetChoose: function (choice) {
      var list = this.pendingOwn, b = list[this.sheetStep];
      if (!b || this.sheetBusy) return;
      b.choice = choice;
      if (list.length === 1) { this.sendDecisions([b]); return; }
      if (this.sheetStep < list.length - 1) this.sheetStep++;
      this.syncSheet();
    },
    sheetSubmit: function () {
      var list = this.pendingOwn.slice();
      if (list.length && !list.some(function (b) { return !b.choice; })) this.sendDecisions(list);
    },
    // in order; the first failure stops the run and stays in the sheet
    sendDecisions: async function (list) {
      if (!hfyClient || this.sheetBusy) return;
      this.sheetBusy = true; this.sheetError = ''; this.syncSheet();
      for (var i = 0; i < list.length; i++) {
        try { await hfyClient.approve(list[i].callId, list[i].choice === 'allowed'); list[i].submitted = true; }
        catch (e) { this.sheetError = (e && e.message) || this.t('error'); break; }
      }
      this.sheetBusy = false; this.sheetStep = 0;
      this.relayout(); this.syncSheet();
    },
    onSheetKey: function (ev) {
      if (ev.key === 'Escape') { ev.preventDefault(); this.minimizeSheet(); return; }
      hfyTrap(ev);
    },
    onHandleDown: function (ev) {
      hfyDrag = { y: ev.clientY, dy: 0, sheet: ev.currentTarget.closest('.hfy-sheet') };
      try { ev.currentTarget.setPointerCapture(ev.pointerId); } catch (e) { /* synthetic event */ }
    },
    onHandleMove: function (ev) {
      if (!hfyDrag) return;
      hfyDrag.dy = Math.max(0, ev.clientY - hfyDrag.y);
      hfyDrag.sheet.style.transform = 'translateY(' + hfyDrag.dy + 'px)';
    },
    onHandleUp: function () {
      var d = hfyDrag; hfyDrag = null;
      if (!d) return;
      d.sheet.style.transform = '';
      if (d.dy > 60) this.minimizeSheet();
    }
  };
};

if (typeof module !== 'undefined' && module.exports) {
  module.exports = { hfyMdParse: hfyMdParse, hfyMdBuild: hfyMdBuild, hfySafeHref: hfySafeHref, hfySafeUrl: hfySafeUrl, hfyOnColor: hfyOnColor,
    hfyToolName: hfyToolName, hfyArgList: hfyArgList, hfyTick: hfyTick, hfyHue: hfyHue };
}
