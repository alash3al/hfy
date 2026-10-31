// Review item 2, both flavor bases at once: one fake server, one client
// WITHOUT a path prefix (the whole suite's default) and one WITH
// http://host:port/prefix. Every endpoint must carry the prefix into the
// wire exactly like client.js's string joining (`api + path`), the SSE open
// must keep it (it always did — through pathSegments), the upload PUT joins
// the prefix onto the slot's server-root path, and NO doubled slashes ever
// leak into a request path. A never-authorization leak: an ABSOLUTE
// upload_url in a NON-direct slot (any host) is rejected before any PUT —
// the bearer token cannot hop to a foreign host.
import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/src/client/connection.dart'
    show HfyConnectionState;
import 'package:hfy_flutter/src/model/event.dart' show HfyProtocolException;

import 'test_kit.dart';

void main() {
  late FakeHfyServer server;
  late ClientKit plain; // no prefix — the suite-wide default wiring
  late ClientKit prefixed; // .../prefix base
  late Uri prefixBase;

  setUp(() async {
    server = await FakeHfyServer.start();
    server.pathPrefix = '/prefix';
    prefixBase = server.base.replace(path: '/prefix');
    plain = await ClientKit.open(server);
    prefixed = await ClientKit.open(server, api: prefixBase);
  });

  tearDown(() => server.destroy());

  test('both bases agree on EVERY endpoint (review item 2): history, SSE '
      'open, send, approvals, stop, config, slot POST and the hfy-path '
      'upload PUT all join', () async {
    final cases = <(String, ClientKit)>[
      ('no-prefix base', plain),
      ('/prefix base', prefixed),
    ];
    for (final (label, kit) in cases) {
      server.historyPages[null] = historyPage(liveAfter: 'la-0');
      server.uploadSlot = <String, Object?>{
        'file': 'uploads/c1/k.txt',
        // uploads.go:100's server-root path (prefix-UNaware):
        'upload_url':
            '/v1/conversation/uploads/uploads%2Fc1%2Fk.txt?exp=1&sig=S',
        'expires_at': '2026-01-01T00:05:00Z',
        'direct': false,
      };
      await kit.client.connect();
      await kit.waitLive();
      await settle();

      await kit.client.send('hi');
      await kit.client.approve('c1', true);
      await kit.client.stop();
      await kit.client.config();
      await kit.client.upload(
          mime: 'text/plain',
          size: 2,
          name: 'a.txt',
          bytes: Stream.value('ab'.codeUnits));

      final paths = server.requests
          .map((r) => Uri.parse(r.path).path)
          .where((p) => p.contains('/v1/'))
          .toList();
      // Exactly the eight calls this iteration drove:
      expect(paths.length, 8, reason: 'label: $label');
      final expectedSuffixes = <String>[
        '/v1/conversation/history',
        '/v1/conversation/events',
        '/v1/conversation/messages',
        '/v1/conversation/approvals',
        '/v1/conversation/stop',
        '/v1/conversation/config',
        '/v1/conversation/uploads',
        '/v1/conversation/uploads/uploads%2Fc1%2Fk.txt',
      ];
      for (final suffix in expectedSuffixes) {
        final hits = paths.where((p) => p.endsWith(suffix)).length;
        expect(hits, 1,
            reason: 'one $suffix call recorded (label: $label)');
      }
      // The prefix rides on EVERY call (or nothing does, per the label):
      final withPrefix = paths.where((p) => p.startsWith('/prefix')).length;
      final withoutPrefix = paths.length - withPrefix;
      expect(label == '/prefix base' ? withPrefix : withoutPrefix, 8,
          reason: 'the api base path prefix reaches EVERY endpoint '
              '(base: $prefixBase) exactly like client.js string joining');
      // No doubled slash anywhere (a stripped base or junction never
      // degenerates into "//v1/..."):
      expect(paths,
          everyElement(predicate<String>((p) => !p.contains('//'))),
          reason: 'no doubled slash on any joined request path');
      server.requests.clear();
    }
  });

  test('the SSE open keeps the prefix AND its query intact '
      '(?after= on the first open)', () async {
    server.historyPages[null] = historyPage(liveAfter: 'la-0');
    await prefixed.client.connect();
    await server.nextSseConnection();
    await until(() => prefixed.client.state.value == HfyConnectionState.live);
    final req = server.sseConnections.last.request;
    expect(Uri.parse(req.path).path, '/prefix/v1/conversation/events');
    expect(req.after, 'la-0',
        reason: 'the query survives the join byte-exact');
    expect(req.authorization, 'Bearer tok-1');
    expect(server.historyPages[null], isNotNull,
        reason: 'the history join itself is asserted above');
  });

  test('security (review item 2): an absolute upload_url in a NON-direct '
      'slot is rejected — no PUT, no bearer on a foreign host', () async {
    server.uploadSlot = <String, Object?>{
      'file': 'uploads/c1/k.png',
      'upload_url': 'http://evil.example.com/steal',
      'expires_at': '2026-01-01T00:05:00Z',
      'direct': false,
    };
    await expectLater(
        prefixed.client.upload(
            mime: 'image/png',
            size: 1,
            name: 'a.png',
            bytes: Stream.value('A'.codeUnits)),
        throwsA(isA<HfyProtocolException>().having(
            (e) => e.message,
            'message',
            contains('non-direct upload_url must be a server path'))));
    // Server-relative-but-not-a-path is equally rejected:
    server.uploadSlot = <String, Object?>{
      'file': 'uploads/c1/k2.png',
      'upload_url': 'uploads/c1/relative.png',
      'expires_at': '2026-01-01T00:05:00Z',
      'direct': false,
    };
    await expectLater(
        prefixed.client.upload(
            mime: 'image/png',
            size: 1,
            name: 'b.png',
            bytes: Stream.value('B'.codeUnits)),
        throwsA(isA<HfyProtocolException>()));
    // NOTHING was PUT anywhere:
    expect(server.uploadPuts, isEmpty);
    expect(server.uploadPutBodies, isEmpty);
    // The slot request itself still went through (that endpoint is fine):
    expect(
        server.requests.where((r) =>
            Uri.parse(r.path).path.endsWith('/v1/conversation/uploads')),
        isNotEmpty);
  });

  test('a DIRECT (presigned) slot PUTs the ABSOLUTE URL as received, with '
      'no auth header (P-CLIENT-011, security: the direct slot carries no '
      'Authorization on any host)', () async {
    server.uploadSlot = <String, Object?>{
      'file': 'uploads/c1/d.png',
      'upload_url': '${server.base}/v1/files/serve-direct',
      'expires_at': '2026-01-01T00:05:00Z',
      'direct': true,
    };
    final ref = await prefixed.client.upload(
        mime: 'image/png',
        size: 2,
        name: 'd.png',
        bytes: Stream.value('DD'.codeUnits));
    expect(ref.file, 'uploads/c1/d.png');
    final put = server.uploadPuts.single;
    expect(put.path, '/v1/files/serve-direct',
        reason: 'the absolute presigned URL is used verbatim (no prefix '
            'prepending onto the foreign path)');
    expect(put.authorization, '');
  });
}
