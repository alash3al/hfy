// SSE parser tests (PARITY.md P-NAT-002, WHATWG event-stream interpretation,
// server framing internal/httpapi/events.go). Chunk boundaries at EVERY byte
// offset of a canonical multi-event stream, CRLF/CR/LF, split multibyte
// UTF-8 (Arabic + emoji), comments, multi-line data, space handling, empty
// data, unknown fields, BOM, trailing partial events, id persistence, and
// the idle watchdog.
import 'dart:async' show StreamController, StreamSubscription, Timer;
import 'dart:convert' show utf8;

import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/hfy_flutter.dart';

const String canonical =
    'id: 0a5f9bd4-871a-4cff-9f29-6b6e6f7a1a77\n'
    'event: turn\n'
    'data: {"id":"turn-1","type":"turn","data":{"agent":"main"}}\n'
    '\n'
    'event: message.text\n'
    'data: مرحبا 👋\n'
    '\n'
    'data: line1\n'
    'data: line2\n'
    '\n'
    ': ping\n'
    '\n';

const List<HfySseMessage> canonicalMessages = <HfySseMessage>[
  HfySseMessage(
    data: '{"id":"turn-1","type":"turn","data":{"agent":"main"}}',
    event: 'turn',
    id: '0a5f9bd4-871a-4cff-9f29-6b6e6f7a1a77',
    lastEventId: '0a5f9bd4-871a-4cff-9f29-6b6e6f7a1a77',
  ),
  HfySseMessage(
    data: 'مرحبا 👋',
    event: 'message.text',
    lastEventId: '0a5f9bd4-871a-4cff-9f29-6b6e6f7a1a77',
  ),
  HfySseMessage(
    data: 'line1\nline2',
    lastEventId: '0a5f9bd4-871a-4cff-9f29-6b6e6f7a1a77',
  ),
];

HfySseMessage msg(
  String data, {
  String? event,
  String? id,
  String lastEventId = '',
}) => HfySseMessage(data: data, event: event, id: id, lastEventId: lastEventId);

// single push + close (a lone trailing \r completing a blank line dispatches
// at the close itself — that output is included)
List<HfySseMessage> drained(String stream) {
  final parser = HfySseParser();
  final out = parser.push(utf8.encode(stream))..addAll(parser.close());
  return out;
}

// N pushes + close
List<HfySseMessage> drainedChunks(List<String> chunks) {
  final parser = HfySseParser();
  final out = <HfySseMessage>[];
  for (final chunk in chunks) {
    out.addAll(parser.push(utf8.encode(chunk)));
  }
  out.addAll(parser.close());
  return out;
}

List<HfySseMessage> drainedRaw(List<int> bytes) {
  final parser = HfySseParser();
  final out = parser.push(bytes)..addAll(parser.close());
  return out;
}

Future<List<HfySseMessage>> collect(Stream<HfySseMessage> source) =>
    source.toList();

class _FakeTimer implements Timer {
  _FakeTimer(this.callback);

  final void Function() callback;
  bool _isActive = true;

  // A real timer's callback runs at most once; "fire" on a dead timer is a
  // no-op, exactly like the event loop would do.
  void fire() {
    if (!_isActive) return;
    _isActive = false;
    callback();
  }

  @override
  void cancel() => _isActive = false;

  @override
  bool get isActive => _isActive;

  @override
  int get tick => 0;
}

void main() {
  group('P-NAT-002 canonical stream', () {
    test(
      'the whole stream in one push: 3 events; the ping comment vanishes',
      () {
        expect(drained(canonical), canonicalMessages);
      },
    );

    test('server ping (`: ping`, events.go:120) never dispatches', () {
      expect(drained(': ping\n\n'), isEmpty);
      expect(drained(':ping\n\n'), isEmpty); // no space needed after ':'
    });
  });

  group('P-NAT-002 chunk boundaries — every byte offset', () {
    final bytes = utf8.encode(canonical);
    for (var split = 0; split <= bytes.length; split++) {
      test('split at byte offset $split', () {
        final parser = HfySseParser();
        final out = [
          ...parser.push(bytes.sublist(0, split)),
          ...parser.push(bytes.sublist(split)),
        ];
        expect(out, canonicalMessages, reason: 'split $split/${bytes.length}');
      });
    }
  });

  group('P-NAT-002 UTF-8 split mid-character', () {
    const text = 'أهلاً 👍 ۩'; // 2-byte runes + a 4-byte emoji
    final stream = utf8.encode('data: $text\n\n');
    final expected = [msg(text)];

    test('every byte offset between two chunks keeps the text whole', () {
      for (var split = 0; split <= stream.length; split++) {
        final parser = HfySseParser();
        final out = [
          ...parser.push(stream.sublist(0, split)),
          ...parser.push(stream.sublist(split)),
        ];
        expect(out, expected, reason: 'split $split/${stream.length}');
      }
    });

    test('mid-rune Arabic splits complete correctly across resumption', () {
      const word = 'مرحبا'; // 5 runes × 2 bytes
      final runes = utf8.encode(word);
      expect(runes.length, 10);
      for (var split = 1; split < runes.length; split++) {
        final parser = HfySseParser();
        final out = [
          ...parser.push(utf8.encode('data: ')),
          ...parser.push(runes.sublist(0, split)),
          ...parser.push(runes.sublist(split)),
          ...parser.push(utf8.encode('\n\n')),
        ];
        expect(out, [msg(word)], reason: 'mid-rune split $split');
      }
    });

    test('mid-rune split of a 4-byte emoji then completion decodes intact', () {
      final emoji = utf8.encode('👍');
      expect(emoji.length, 4);
      for (var split = 1; split < emoji.length; split++) {
        final parser = HfySseParser();
        final out = [
          ...parser.push(utf8.encode('data: ')),
          ...parser.push(emoji.sublist(0, split)),
          ...parser.push(emoji.sublist(split)),
          ...parser.push(utf8.encode('\n\n')),
        ];
        expect(out, [msg('👍')], reason: 'emoji split $split');
      }
    });

    test('three-way splits at sample offsets equal the one-chunk result', () {
      final bytes = utf8.encode(canonical);
      for (final split1 in [1, 17, 50]) {
        for (var split2 = split1 + 40; split2 < bytes.length; split2 += 97) {
          final parser = HfySseParser();
          final out = [
            ...parser.push(bytes.sublist(0, split1)),
            ...parser.push(bytes.sublist(split1, split2)),
            ...parser.push(bytes.sublist(split2)),
          ];
          expect(out, canonicalMessages, reason: 'splits $split1/$split2');
        }
      }
    });
  });

  group('P-NAT-002 line endings', () {
    test('LF lines + blank line dispatches', () {
      expect(drained('data: a\ndata: b\n\n'), [msg('a\nb')]);
    });

    test('CRLF frames', () {
      expect(drained('data: a\r\n\r\n'), [msg('a')]);
      expect(drained('data: a\r\ndata: b\r\n\r\n'), [msg('a\nb')]);
    });

    test('bare CR frames', () {
      expect(drained('data: a\r\r'), [msg('a')]);
    });

    test('mixed endings in one stream', () {
      expect(drained('data: a\r\ndata: b\r\n\ndata: c\r\r'), [
        msg('a\nb'),
        msg('c'),
      ]);
    });

    test('CRLF split across the chunk boundary is ONE terminator', () {
      // the \n completes the line pending its deferred \r
      expect(drainedChunks(['data: a\r', '\n']), isEmpty); // no blank yet!
      // a second data line still joins the FIRST frame (no blank between)
      expect(drainedChunks(['data: a\r', '\ndata: b\n\n']), [msg('a\nb')]);
      // an extra \n after the split-CR: line + blank → first dispatch
      expect(drainedChunks(['data: a\r', '\n\ndata: b\n\n']), [
        msg('a'),
        msg('b'),
      ]);
    });

    test('CR CR LF across three chunks: the resumed empty line dispatches', () {
      final parser = HfySseParser();
      parser.push(utf8.encode('data: a\r'));
      parser.push(const [0x0D]);
      expect(parser.push(const [0x0A]), [msg('a')]); // CR#2+LF = blank line
      expect(parser.push(utf8.encode('\n')), isEmpty); // LF alone: no-op
    });

    test(
      'deferred CR resolved by a non-LF byte keeps further bytes flowing',
      () {
        // chunk cut between the CR and a new field line ('X' is unknown)
        expect(drainedChunks(['data: a\r', 'X\n\n']), [msg('a')]);
      },
    );
  });

  group('P-NAT-002 field rules', () {
    test('multi-line data joins with \\n', () {
      expect(drained('data: x\ndata: y\ndata: z\n\n'), [msg('x\ny\nz')]);
    });

    test('missing space after colon is kept ("data:x" → "x")', () {
      expect(drained('data:x\n\n'), [msg('x')]);
    });

    test('at most ONE leading space removed ("data:  x" → " x")', () {
      expect(drained('data:  x\n\n'), [msg(' x')]);
      expect(drained('data:   x\n\n'), [msg('  x')]);
    });

    test(
      'empty first data line creates its LF separator (spec append rule)',
      () {
        // WHATWG: every data field appends value + LF; dispatch strips ONE
        // trailing LF — an empty first line therefore yields a leading \n.
        expect(drained('data:\ndata:y\n\n'), [msg('\ny')]);
      },
    );

    test('no data at all → nothing dispatches; bare `data:` fires EMPTY', () {
      expect(drained('data: \n\n'), [msg('')]); // lone value-free data line
      expect(drained('data:\n\n'), [msg('')]);
      expect(drained('\n\n'), isEmpty);
      expect(drained('\n\n\n'), isEmpty);
    });

    test('unknown fields are ignored', () {
      expect(drained('x-custom: hello\n\ndata: ok\n\n'), [msg('ok')]);
      // a space before the colon puts the field name "data" (unknown!)
      expect(drained('data : x\n\ndata: y\n\n'), [msg('y')]);
    });

    test('empty event: value stays raw ("") — unnamed semantics upstream', () {
      final parser = HfySseParser();
      final out = parser.push(utf8.encode('event:\ndata: x\n\n'));
      expect(out.single.data, 'x');
      expect(out.single.event, '');
    });

    test('retry is ignored — Dart owns the reconnect ladder (P-DIFF-009)', () {
      expect(drained('retry: 500\ndata: x\n\n'), [msg('x')]);
      expect(drained('retry: 500\n\n'), isEmpty);
    });

    test('a field line without a colon keeps its field semantics (spec)', () {
      // 'data' naked = a data field with the empty value → fires ''
      expect(drained('data\n\n'), [msg('')]);
      expect(drained('blah\n\n'), isEmpty);
    });

    test('comment lines inside a frame never split the frame', () {
      expect(drained('data: a\n: meow\ndata: b\n\n'), [msg('a\nb')]);
    });
  });

  group('P-NAT-002 id persistence — both views exposed', () {
    test('frame id vs persistent lastEventId across frames', () {
      final parser = HfySseParser();
      final out = parser.push(
        utf8.encode('id: a\ndata: 1\n\ndata: 2\n\nid: b\ndata: 3\n\n'),
      );
      expect(out[0].id, 'a');
      expect(out[0].lastEventId, 'a');
      expect(out[1].id, isNull); // frame 2 carries no id:
      expect(out[1].lastEventId, 'a'); // ...but inherits it (persists)
      expect(out[2].id, 'b');
      expect(out[2].lastEventId, 'b');
      expect(parser.lastEventId, 'b');
    });

    test('NUL in an id: field is ignored per spec (older id survives)', () {
      final parser = HfySseParser();
      final out = parser.push(
        utf8.encode('id: a\ndata: 1\n\nid: b\u0000c\ndata: 2\n\n'),
      );
      expect(out[0].id, 'a');
      expect(out[1].id, isNull);
      expect(out[1].lastEventId, 'a');
      expect(parser.lastEventId, 'a');
    });

    test('parser.lastEventId untouched by an unterminated trailing id', () {
      final parser = HfySseParser();
      parser.push(utf8.encode('id: z\ndata: orphan\n')); // no blank line
      parser.close();
      expect(parser.lastEventId, '');
    });

    test(
      'an EMPTY frame still promotes the id buffer (spec dispatch order)',
      () {
        final parser = HfySseParser();
        expect(parser.push(utf8.encode('id: a\ndata: 1\n\n')), [
          msg('1', id: 'a', lastEventId: 'a'),
        ]);
        // 'id: b' + blank line + no data → no event, but the id IS promoted
        // before the empty-data early return; `id:` with no value resets to ''.
        expect(parser.push(utf8.encode('id: b\n\n')), isEmpty);
        expect(parser.lastEventId, 'b');
        expect(
          parser.push(utf8.encode('id:\ndata: 2\n\n')).single.lastEventId,
          '',
        );
      },
    );

    test('the WHATWG spec example stream (~9.2.6) fires all three events', () {
      final out = drained(
        ': test stream\n'
        '\n'
        'data: first event\n'
        'id: 1\n'
        '\n'
        'data:second event\n'
        'id\n'
        '\n'
        'data:  third event\n'
        '\n',
      );
      expect(out, [
        msg('first event', id: '1', lastEventId: '1'),
        msg('second event', id: '', lastEventId: ''),
        msg(' third event', id: null, lastEventId: ''),
      ]);
    });
  });

  group('P-NAT-002 BOM', () {
    final body = utf8.encode('data: x\n\n');
    final all = <int>[0xEF, 0xBB, 0xBF, ...body];

    test('UTF-8 BOM at stream start is stripped', () {
      expect(drainedRaw(all), [msg('x')]);
    });

    test('a first chunk shorter than a BOM waits for the rest', () {
      for (var split = 0; split <= 4; split++) {
        final parser = HfySseParser();
        final out = [
          ...parser.push(all.sublist(0, split)),
          ...parser.push(all.sublist(split)),
        ];
        expect(out, [msg('x')], reason: 'BOM split $split');
      }
    });

    test('BOM after the stream start is content (U+FEFF character)', () {
      final midBom = <int>[
        ...utf8.encode('data: '),
        0xEF,
        0xBB,
        0xBF,
        ...utf8.encode('x\n\n'),
      ];
      expect(drainedRaw(midBom).single.data, '\uFEFFx');
    });

    test('first bytes that are NOT a BOM are processed as data', () {
      // 'deaf' has no colon → unknown field; the blank line never dispatches
      expect(drainedRaw([0x64, 0x65, 0x61, 0x66, 0x0A, 0x0A]), isEmpty);
      expect(drained('data: x\n\n'), [msg('x')]);
    });
  });

  group('P-NAT-002 close / trailing partial event', () {
    test('last event WITHOUT the trailing blank line is NOT dispatched', () {
      final parser = HfySseParser();
      expect(parser.push(utf8.encode('id: z\n')), isEmpty);
      expect(parser.push(utf8.encode('event: turn\n')), isEmpty);
      expect(parser.push(utf8.encode('data: partial\n')), isEmpty);
      parser.close();
      expect(parser.lastEventId, '', reason: 'no dispatch → no id promotion');
    });

    test('truncated final lines are never processed (EOF is not a blank)', () {
      expect(drained('data: par'), isEmpty);
      expect(drained('data: par\r'), isEmpty);
      expect(drained('data: par\n'), isEmpty);
    });

    test('a complete dispatched event keeps its id after close', () {
      final parser = HfySseParser();
      expect(parser.push(utf8.encode('id: q\ndata: 1\n\n')), [
        msg('1', id: 'q', lastEventId: 'q'),
      ]);
      parser.close();
      expect(parser.lastEventId, 'q');
    });

    test('close() is idempotent; push after close throws StateError', () {
      final parser = HfySseParser();
      parser.close();
      parser.close();
      expect(() => parser.push(const [0x61]), throwsStateError);
    });

    test('an empty stream + close is a no-op', () {
      final parser = HfySseParser();
      parser.close();
      expect(parser.lastEventId, '');
    });
  });

  group('P-NAT-002 transform()', () {
    test(
      'chunks stream into frames; EOF never flushes trailing state',
      () async {
        final source = Stream<List<int>>.fromIterable(<List<int>>[
          utf8.encode('id: a\nevent: turn\n'),
          utf8.encode('data: hello\n\n'),
          utf8.encode('data: trailing'),
        ]);
        final out = await collect(HfySseParser().transform(source));
        expect(out, [msg('hello', event: 'turn', id: 'a', lastEventId: 'a')]);
      },
    );

    test(
      'what close() dispatches on normal end SURVIVES transform '
      '(a final lone \\r — its blank-line completion is a close affair)',
      () async {
        // 'data: x\r\r': the second \r terminates the EMPTY line and closes x,
        // but sitting at the very end of the stream it is deferred to close().
        final source = Stream<List<int>>.fromIterable(<List<int>>[
          utf8.encode('data: x\r\r'),
        ]);
        final out = await collect(HfySseParser().transform(source));
        expect(out, [msg('x')]);
      },
    );

    test(
      'source errors propagate AND release the parser (close() ran)',
      () async {
        final parser = HfySseParser();
        Stream<List<int>> source() async* {
          yield utf8.encode('data: half');
          throw StateError('boom');
        }

        await expectLater(
          collect(parser.transform(source())),
          throwsStateError,
        );
        // The error path already close()d the parser (release — nothing
        // half-received is ever dispatched): it is now dead like any closed one.
        expect(() => parser.push(const <int>[]), throwsStateError);
        expect(parser.close(), isEmpty);
      },
    );

    test(
      'cancelling the transform releases the parser (close() ran)',
      () async {
        final parser = HfySseParser();
        final controller = StreamController<List<int>>();
        var received = 0;
        Future<void>? cancelled;
        late final StreamSubscription<HfySseMessage> done;
        done = parser.transform(controller.stream).listen((_) {
          // Cancel while the generator is suspended AT its yield — the one
          // cancellation point whose teardown (the finally → close()) is
          // guaranteed to run and to complete(done.cancel()'s future).
          received++;
          cancelled = done.cancel();
        });
        // One whole frame, then a half-received one: the listener above sees
        // the first message and cancels mid-frame.
        controller.add(utf8.encode('id: a\ndata: x\n\ndata: half'));
        await controller
            .close(); // settled delivery: the callback ran, cancel went in
        await cancelled;
        expect(received, 1);
        expect(
          () => parser.push(const <int>[]),
          throwsStateError,
          reason: 'a cancelled transform close()d (released) its parser',
        );
      },
    );
  });

  group('P-NAT-002 buffer limits (1 MiB line / 1 MiB event)', () {
    const oneMiB = 1 << 20;

    test('a 50 MB single line is refused mid-stream, not buffered', () {
      final parser = HfySseParser();
      final chunk = List<int>.filled(
        oneMiB,
        0x78,
      ); // same 'x' MiB, 50× streamed
      // The first MiB is exactly at the line cap (fine); the first byte of
      // the second chunk breaches it — the parser never holds more than the
      // approved 1 MiB, making a 50 MB hostile line a 2 MiB event loop.
      expect(() {
        for (var i = 0; i < 50; i++) {
          parser.push(chunk);
        }
      }, throwsHfyProtocol());
    });

    test('exactly the line cap passes; one byte over throws', () {
      // 'data: ' is 6 wire bytes → a value of (1 MiB − 6) 'x' makes the line
      // EXACTLY maxLineBytes before the terminator: must dispatch whole.
      final stream = 'data: ${'x' * (oneMiB - 6)}\n\n';
      expect(drained(stream), [msg('x' * (oneMiB - 6))]);
      expect(
        () => drained('data: ${'x' * (oneMiB - 5)}\n\n'),
        throwsHfyProtocol(),
      );
    });

    test('many small data lines still make one event in O(n)', () {
      // Horizontal growth is not the line cap's business: 100 000 one-char
      // `data:` lines join into ONE frame (the collected-lines assembly is
      // linear; the naive `+=` build this test guards against would crawl).
      final parser = HfySseParser();
      final out = <HfySseMessage>[];
      for (var i = 0; i < 100000; i++) {
        out.addAll(parser.push(utf8.encode('data: x')));
        out.addAll(parser.push(const [0x0A])); // end each line separately
      }
      out.addAll(parser.push(const [0x0A])); // the dispatching blank line
      final single = out.single;
      expect(single.data.length, 199999); // join = 'x\n' × 99 999 + 'x'
      expect(single.data.startsWith('x\nx'), isTrue);
      expect(single.data.endsWith('\nx'), isTrue);
    });

    test('the event cap covers the whole frame, not each line', () {
      // 7-char values + 1 LF = 8 payload bytes per data line:
      // 131 072 lines = EXACTLY 1 MiB of payload (passes), one more breaches.
      // (The final blank line DISPATCHES the at-limit frame.)
      final line = 'data: ${'x' * 7}\n';
      final atLimit = '${line * 131072}\n';
      final messages = drained(atLimit);
      expect(messages.single.data.length, oneMiB - 1); // joining strips 1 LF
      expect(() => drained('${line * 131073}\n'), throwsHfyProtocol());
    });

    test(
      'a breach leaves the parser UNUSABLE (reconnect = a fresh parser)',
      () {
        final parser = HfySseParser();
        expect(
          () => parser.push(utf8.encode('data: ${'x' * (oneMiB + 1)}\n\n')),
          throwsHfyProtocol(),
        );
        expect(
          () => parser.push(utf8.encode('data: again\n\n')),
          throwsStateError,
        );
        expect(parser.close(), isEmpty); // releasing is all that is left
      },
    );

    test('a boundless run of small data lines breaches the EVENT cap', () {
      // 2 payload bytes per one-char line: the 524 288th data line is EXACTLY
      // 1 MiB, the next one breaches. No single line is long, yet the frame
      // as a whole still cannot unbound a cap.
      final lineBytes = utf8.encode('data: x\n');
      final parser = HfySseParser();
      expect(() {
        for (var i = 0; i < 524300; i++) {
          parser.push(lineBytes);
        }
      }, throwsHfyProtocol());
    });
  });

  group('P-NAT-002 one byte at a time', () {
    test('the canonical stream fed byte-by-byte yields the same frames', () {
      final parser = HfySseParser();
      final out = <HfySseMessage>[];
      for (final byte in utf8.encode(canonical)) {
        out.addAll(parser.push([byte]));
      }
      out.addAll(parser.close());
      expect(out, canonicalMessages);
    });

    test('a CRLF stream fed one byte at a time parses identically', () {
      const stream =
          'id: a\r\nevent: turn\r\ndata: one\r\ndata: two\r\n\r\nevent: x\r\ndata: t\r\r';
      final parser = HfySseParser();
      final out = <HfySseMessage>[];
      for (final byte in utf8.encode(stream)) {
        out.addAll(parser.push([byte]));
      }
      out.addAll(parser.close());
      expect(out, [
        msg('one\ntwo', event: 'turn', id: 'a', lastEventId: 'a'),
        msg('t', event: 'x', lastEventId: 'a'),
      ]);
    });
  });

  group('HfyIdleWatchdog (P-NAT-002:no bytes for >30s = dead)', () {
    test('fires once after the idle window; silence alone never re-fires', () {
      var fired = 0;
      final timers = <_FakeTimer>[];
      final watchdog = HfyIdleWatchdog(
        idle: const Duration(seconds: 30),
        onDead: () => fired++,
        timerFactory: (delay, callback) {
          expect(delay, const Duration(seconds: 30), reason: 'armed with idle');
          return (timers..add(_FakeTimer(callback))).last;
        },
      );
      watchdog.feed();
      timers.single.fire();
      expect(fired, 1);
      timers.single.fire(); // no feed() since → disarmed: must NOT re-fire
      expect(fired, 1);
    });

    test('activity resets the window (feed re-arms before the deadline)', () {
      var fired = 0;
      final timers = <_FakeTimer>[];
      final watchdog = HfyIdleWatchdog(
        idle: const Duration(seconds: 30),
        onDead: () => fired++,
        timerFactory: (delay, callback) {
          expect(delay, const Duration(seconds: 30));
          return (timers..add(_FakeTimer(callback))).last;
        },
      );
      watchdog.feed();
      watchdog.feed(); // reset: window restarted
      expect(timers[0].isActive, isFalse, reason: 'old arm cancelled');
      expect(timers[1].isActive, isTrue);
      timers[1].fire();
      expect(fired, 1);
      watchdog.feed(); // feed restarts the NEXT deadness watch
      timers.last.fire();
      expect(fired, 2);
    });

    test('cancel(): an armed watchdog never fires after cancel', () {
      var fired = 0;
      final timers = <_FakeTimer>[];
      final watchdog = HfyIdleWatchdog(
        idle: const Duration(seconds: 30),
        onDead: () => fired++,
        timerFactory: (delay, callback) {
          expect(delay, const Duration(seconds: 30));
          return (timers..add(_FakeTimer(callback))).last;
        },
      );
      watchdog.feed();
      watchdog.feed();
      watchdog.cancel();
      expect(timers.last.isActive, isFalse, reason: 'disarmed on cancel');
      timers.last.fire(); // a cancelled timer's callback never runs in prod
      expect(fired, 0);
      watchdog.feed();
      watchdog.cancel();
      expect(fired, 0);
    });

    // No real-timer test: a 10ms wall-clock race is flaky by nature. The
    // default factory (a plain Timer) is the one line `?? ((delay, callback)
    // => Timer(delay, callback))` — every behavior above is verified on the
    // injected factory with the exact idle duration asserted at arm time.
  });
}

Matcher throwsHfyProtocol() => throwsA(isA<HfyProtocolException>());
