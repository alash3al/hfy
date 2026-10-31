// SSE stream parsing for hfy (PARITY.md P-NAT-002).
//
// The server's frame contract (internal/httpapi/events.go):
//   - one frame per event: `id: <uuid>\nevent: <type>\ndata: <json>\n\n`
//     (events.go:56) — `data` is always ONE json line on the wire, but the
//     parser stays a full SSE implementation (multi-line data etc.) because
//     the format, not this server's current shape, is the contract;
//   - a `: ping` COMMENT line every 15s (events.go:15,118-125) that must
//     never be dispatched but does keep the connection visibly alive;
//   - the server closes the stream when the grant ends; the client then
//     runs its reconnect ladder, and must treat >~30s WITHOUT ANY BYTES as
//     a dead connection ([HfyIdleWatchdog]) — pings at 15s mean silence
//     beyond ~2 ping intervals is an error;
//   - no matter what the server streams, the parser buffers are bounded
//     (1 MiB caps per line / per frame): a hostile stream fails the parser
//     (typed [HfyProtocolException]) instead of growing memory unboundedly,
//     and the client reconnects on that like on any stream error.
//
// Framing follows the WHATWG "server-sent events" stream interpretation
// (HTML spec, §event stream parsing): UTF-8, \n \r\n \r all end a line, a
// line leading with `:` is a comment, a `field: value` has at most ONE
// leading `value` space removed, `data` accumulates joined with \n, `id`
// persists as lastEventId across dispatches until overwritten, `retry` is
// consumed by the browser's native reconnection logic — which Dart replaces
// entirely, so [HfySseParser] ignores it (P-DIFF-005/P-DIFF-009). A blank
// line dispatches; a stream END never dispatches, so a last event without
// its trailing blank line is dropped on close, exactly as in the spec.
import 'dart:async';
import 'dart:convert' show utf8;

import '../model/event.dart' show HfyProtocolException;

/// One dispatched SSE frame.
///
/// `id` is THIS frame's own `id:` field value (null when the frame carried
/// none); `lastEventId` is the persistent per-spec value at dispatch (the
/// frame's, or inherited from an earlier frame). hfy's client resumes and
/// cursors by ENVELOPE ids (the JSON inside `data`), never by the frame id —
/// both views stay exposed for parity/debugging (P-NAT-002).
class HfySseMessage {
  final String data;
  final String? event;
  final String? id;
  final String lastEventId;

  const HfySseMessage({
    required this.data,
    this.event,
    this.id,
    required this.lastEventId,
  });

  @override
  bool operator ==(Object other) =>
      other is HfySseMessage &&
      other.data == data &&
      other.event == event &&
      other.id == id &&
      other.lastEventId == lastEventId;

  @override
  int get hashCode => Object.hash(data, event, id, lastEventId);

  @override
  String toString() =>
      'HfySseMessage(event: $event, id: $id, lastEventId: $lastEventId, '
      'data: <${data.length} chars>)';
}

/// Incremental, pure SSE parser over byte chunks.
///
/// UTF-8 may split mid-character (and a \r\n split across chunks is ONE
/// terminator); lines are buffered as BYTES and decoded whole, so chunk
/// boundaries anywhere are indistinguishable from one big chunk. Byte
/// streams never expire: with no further [push]/[close] the parser simply
/// holds the incomplete state.
class HfySseParser {
  /// Hard buffer caps, 1 MiB each: [maxLineBytes] caps ONE line as received
  /// (raw wire bytes — any line, comments included), [maxEventBytes] caps
  /// the `data` payload accumulated for one frame. The browser's
  /// EventSource handles a buggy or hostile server by dying, but a native
  /// client must die CLEANLY, before either buffer can grow unboundedly
  /// (one huge line — or an unbounded run of small data lines — would
  /// otherwise be a memory DoS in a mobile app). Breaching a cap fails the
  /// parser with [HfyProtocolException] — the client treats that like any
  /// stream error and reconnects — and leaves it UNUSABLE (a fresh parser
  /// belongs to the fresh connection; push throws a StateError afterwards).
  static const int maxLineBytes = 1 << 20;
  static const int maxEventBytes = 1 << 20;

  final List<int> _line = <int>[];
  final List<int> _bomBuf = <int>[];
  bool _pendingCr = false;
  bool _start = true; // until the possible stream-start BOM is settled
  bool _closed = false;

  String? _eventType; // 'event:' of the frame being accumulated
  String? _frameId; // `id:` of the frame being accumulated
  String _lastEventIdBuffer = ''; // per-spec buffer; promoted at dispatch
  String _lastEventId = '';
  final List<String> _dataLines = <String>[]; // one `data:` value per line —
  // collected instead of re-concatenated into a growing String, so a frame
  // of N data lines is assembled in O(N), not the O(N²) of str += str.
  int _eventBytes = 0; // the frame-in-progress's buffered payload bytes

  /// The per-spec "last event ID": updated at every DISPATCH from the id
  /// buffer (so it persists across frames — a frame without `id:` repeats
  /// the previous value), never by unterminated trailing lines.
  String get lastEventId => _lastEventId;

  /// A cap breach fails the connection: the parser closes itself, so no
  /// further buffering happens, and the typed exception (not an untyped
  /// crash) is what the networked client reconnects on.
  Never _fail(String what) {
    _closed = true;
    throw HfyProtocolException(what);
  }

  /// Consumes a byte chunk; returns the frames completed by it.
  List<HfySseMessage> push(List<int> bytes) {
    if (_closed) {
      throw StateError('HfySseParser.push after close');
    }
    if (_start) {
      _bomBuf.addAll(bytes);
      if (_bomBuf.length < 3)
        return <HfySseMessage>[]; // wait for a full BOM-or-not
      final isBom =
          _bomBuf[0] == 0xEF && _bomBuf[1] == 0xBB && _bomBuf[2] == 0xBF;
      bytes = isBom ? _bomBuf.sublist(3) : List<int>.of(_bomBuf);
      _start = false;
    }
    final messages = <HfySseMessage>[];
    for (var i = 0; i < bytes.length; i++) {
      final b = bytes[i];
      if (_pendingCr) {
        _pendingCr = false;
        // The deferred \r terminated the pending line — ALWAYS (whether the
        // next byte turns out to be a \n joining it, or something else):
        // "a\r" + "\n" is one CRLF terminator, "a\r" + "X" is a bare-CR end.
        _processLine(_line, messages);
        _line.clear();
        if (b == 0x0A) continue; // the \n of a split \r\n: consumed
      }
      if (b == 0x0A) {
        _processLine(_line, messages);
        _line.clear();
      } else if (b == 0x0D && i == bytes.length - 1) {
        // \r at chunk end may be half of \r\n: defer until the next byte.
        _pendingCr = true;
      } else if (b == 0x0D) {
        _processLine(_line, messages);
        _line.clear();
        if (bytes[i + 1] == 0x0A) i++; // \r\n: one terminator
      } else {
        if (_line.length >= maxLineBytes) {
          // Checked BEFORE buffering the next byte: a line reaches exactly
          // maxLineBytes unharmed; one byte over never enters the buffer.
          _fail('HfySseParser: SSE line exceeds $maxLineBytes bytes');
        }
        _line.add(b);
      }
    }
    return messages;
  }

  /// Ends the stream. Trailing incomplete state is DISCARDED: an event
  /// without its trailing blank line is never dispatched (WHATWG §9.2.6:
  /// "Once the end of the file is reached, any pending data must be
  /// discarded… the incomplete event is not dispatched"), and a truncated
  /// final line is not processed either. A lone CR at the very end still
  /// terminates its (possibly empty) line first — which is why a stream
  /// ending `data: x\r\r` dispatches `x` (the empty second line IS the
  /// blank line), while `data: x\r` does not. The `id` buffer of an
  /// eof-empty last line is already unobservable: no dispatch → no
  /// promotion. Returns whatever this final step dispatched.
  List<HfySseMessage> close() {
    if (_closed) return <HfySseMessage>[];
    _closed = true;
    final out = <HfySseMessage>[];
    if (_pendingCr) {
      _pendingCr = false;
      _processLine(_line, out); // the CR was the real line terminator
    }
    _line.clear();
    _bomBuf.clear();
    _dataLines.clear();
    _eventBytes = 0;
    _eventType = null;
    _frameId = null;
    return out;
  }

  /// Pipes a chunk stream through, yielding frames as they complete. The
  /// stream's END (normal end-of-source) runs [close] and yields whatever
  /// it dispatches — e.g. an event finished by a final lone \r, whose blank
  /// line completion is deferred to close. Errors and the listener's
  /// CANCELLATION funnel through close() only to RELEASE buffered state:
  /// a half-received event is never dispatched, and the (already dead)
  /// output of close after a failure has nowhere to go.
  Stream<HfySseMessage> transform(Stream<List<int>> source) async* {
    try {
      await for (final chunk in source) {
        for (final message in push(chunk)) {
          yield message;
        }
      }
      for (final message in close()) {
        yield message; // normal end: the final close() may dispatch
      }
    } finally {
      close(); // idempotent: releases on error/cancel, no-op after a normal end
    }
  }

  void _processLine(List<int> line, List<HfySseMessage> out) {
    if (line.isEmpty) {
      _dispatch(out);
      return;
    }
    if (line.first == 0x3A) return; // ':' at start = comment (server pings)
    final decoded = utf8.decode(line, allowMalformed: true);
    final colon = decoded.indexOf(':');
    final field = colon == -1 ? decoded : decoded.substring(0, colon);
    var value = colon == -1 ? '' : decoded.substring(colon + 1);
    // The value's WIRE byte count, O(1): for the `data` path the field name
    // + colon are pure ASCII, so decoded code units before the value ARE
    // wire bytes, and line.length − that prefix (minus a stripped space) is
    // exactly the value as received.
    final valueBytes =
        line.length - (colon + 1) - (value.startsWith(' ') ? 1 : 0);
    if (value.startsWith(' ')) value = value.substring(1); // exactly one
    switch (field) {
      case 'data':
        // WHATWG field rule: append the value, then ONE LF; dispatch
        // removes a single trailing LF (multi-line data joins with \n).
        // The (=maxEventBytes) cap counts one LF per data line — one more
        // than the joined payload's real separators — so the check errs shut.
        _eventBytes += valueBytes + 1;
        if (_eventBytes > maxEventBytes) {
          _fail('HfySseParser: SSE event exceeds $maxEventBytes bytes of data');
        }
        _dataLines.add(value);
        break;
      case 'event':
        _eventType = value;
        break;
      case 'id':
        // A NUL in an id is hostile: the field is ignored per spec.
        if (!value.contains('\u0000')) {
          _frameId = value;
          _lastEventIdBuffer = value;
        }
        break;
      case 'retry':
        break; // EventSource's reconnect hint; Dart owns its ladder
      default:
        break; // unknown fields ignored
    }
  }

  void _dispatch(List<HfySseMessage> out) {
    // WHATWG dispatch step order: promote the id buffer FIRST — on ANY
    // blank line, even one that turns out to carry no event ("The buffer
    // does not get reset"). An `id:` with no value resets it to ''.
    _lastEventId = _lastEventIdBuffer;
    // …then the empty-data early return also clears the event type buffer.
    if (_dataLines.isEmpty) {
      _eventType = null;
      return;
    }
    // Each queued value + ONE LF; the join IS the LF-append accumulator,
    // and stripping one final LF matches the field rule exactly.
    final buffer = StringBuffer();
    for (final value in _dataLines) {
      buffer
        ..write(value)
        ..write('\n');
    }
    final payload = buffer.toString().substring(
      0,
      buffer.length - 1,
    ); // the single trailing LF
    out.add(
      HfySseMessage(
        data: payload,
        event: _eventType,
        id: _frameId,
        lastEventId: _lastEventId,
      ),
    );
    _dataLines.clear();
    _eventBytes = 0;
    _eventType = null;
    _frameId = null;
  }
}

/// The P-NAT-002 native rule the browser gets for free: a silent stream is
/// a dead stream. Arms `idle` on [feed]; if NO bytes arrive during the
/// window (feed on every chunk — frames or pings alike), [onDead] fires
/// once and the watchdog disarms until the next feed (new stream). Tests
/// inject the factory below, so no real 30s waits.
///
/// CLIENT CONTRACT: the networked client MUST call [feed] at connect time —
/// before the first byte — so that a stream already hung before its first
/// byte is detected by the same window, not just one that hangs later.
class HfyIdleWatchdog {
  final Duration idle;
  final void Function() onDead;

  /// Test-only knob; production never passes it. Kept private (`startTimer`
  /// used to be a visible field that also invited misuse as a public API).
  final Timer Function(Duration delay, void Function() callback) _startTimer;

  Timer? _timer;

  HfyIdleWatchdog({
    required this.idle,
    required this.onDead,
    Timer Function(Duration delay, void Function() callback)? timerFactory,
  }) : _startTimer =
           timerFactory ?? ((delay, callback) => Timer(delay, callback));

  /// Activity resets the window and (re)arms the watchdog.
  void feed() {
    _timer?.cancel();
    _timer = _startTimer(idle, _fire);
  }

  /// Disarms; nothing fires on later silence.
  void cancel() {
    _timer?.cancel();
    _timer = null;
  }

  void _fire() {
    _timer = null;
    onDead();
  }
}
