// Integration test against a REAL hfy server. GATED: it runs only when both
// HFY_E2E_URL and HFY_E2E_SECRET are set (the phase runner starts a mock-
// provider hfy and exports them; otherwise every test below is skipped).
//
// Flow (docs/reference.md API + the phase task):
//   backend (server secret via plain dio): create a conversation + a grant
//   SDK (HfyClient): connect -> live, viewer from history, send('...') ->
//     the user message.text event + the mock provider's reply arrive on the
//     stream, a fresh client's connect() serves both from history,
//   typed errors for approve() of nothing (409 'no pending approval'),
//   stop() answers 202, config() has uploads null (no filesystem configured).
import 'dart:io' show Platform;

import 'package:dio/dio.dart' as d;
import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/src/client/client.dart';
import 'package:hfy_flutter/src/client/connection.dart';
import 'package:hfy_flutter/src/client/errors.dart';
import 'package:hfy_flutter/src/client/token.dart';
import 'package:hfy_flutter/src/model/event.dart' show HfyEvent;

final String? _e2eUrl = Platform.environment['HFY_E2E_URL'];
final String? _e2eSecret = Platform.environment['HFY_E2E_SECRET'];
final bool hasE2E = _e2eUrl != null && _e2eSecret != null;
Uri get base => Uri.parse(_e2eUrl!);
String get secret => _e2eSecret!;

/// Polls until [cond] holds (the same helper shape the fake-server suite
/// uses, kept local so this file owns none of the fake server imports).
Future<void> until(bool Function() cond, {int timeoutMs = 2000}) async {
  final deadline = DateTime.now().add(Duration(milliseconds: timeoutMs));
  while (!cond()) {
    if (DateTime.now().isAfter(deadline)) {
      throw StateError('until: condition never held within ${timeoutMs}ms');
    }
    await Future<void>.delayed(const Duration(milliseconds: 5));
  }
}

void main() {
  test('E2E: connect/read/send/approve/stop/config against a real '
      'mock-provider hfy', () async {
    d.Dio? httpForCleanup;
    HfyClient? live;
    try {
      final b = base;
      final http = httpForCleanup = d.Dio();

      // -- backend: one conversation, plain dio + the server secret --------
      final res = await http.post<Map<String, Object?>>('$b/v1/conversations',
          options: d.Options(headers: {'authorization': 'Bearer $secret'}),
          data: <String, Object?>{});
      final conversation = res.data!['id'] as String;
      // The SDK's token provider re-mints against the same secret.
      Future<Object?> mint(String reason) async {
        // ignore: avoid_print
        print('E2E minting grant (reason: $reason)');
        final g = await http.post<Map<String, Object?>>('$b/v1/grants',
            options: d.Options(headers: {'authorization': 'Bearer $secret'}),
            data: <String, Object?>{
              'conversation': conversation,
              'actor': 'u1',
              'expires_in': 600,
            });
        final data = g.data!;
        final expiresAt = DateTime.parse(data['expires_at'] as String);
        return HfyGrant(token: data['token'] as String, expiresAt: expiresAt);
      }

      live = HfyClient(api: base, tokenProvider: mint);
      final events = <HfyEvent>[];
      live.events.listen(events.add);

      await live.connect();
      await until(() => live!.state.value == HfyConnectionState.live);
      // The wire actor: ref is the grant's actor; uuid is hfy's internal id;
      // a granted viewer's type is 'external' (hfy's ActorType).
      expect(live.viewer!.ref, 'u1');
      expect(live.viewer!.type, 'external');
      expect(live.viewer!.uuid, isA<String>().having((u) => u.length, '', 36));

      // -- send and watch both texts arrive ---------------------------------
      final turn = await live.send('Hello flutter');
      expect(turn, isNotNull);
      // The user's own message.text, then the mock reply, then turn state
      // completed — poll until both texts are on the stream (the mock
      // provider answers immediately, but the turn runs over the queue).
      await until(() {
        final texts = events
            .where((e) => e.type == 'message.text')
            .map((e) => e.data['text'])
            .toSet();
        return texts.containsAll(const ['Hello flutter', 'Hello from mock']);
      }, timeoutMs: 20000);
      final userText = events.singleWhere(
          (e) => e.type == 'message.text' && e.actor?.type == 'external');
      expect(userText.data['text'], 'Hello flutter');
      final reply = events.singleWhere(
          (e) => e.type == 'message.text' && e.actor?.type == 'agent');
      expect(reply.data['text'], 'Hello from mock');
      // The turn completes visibly; assertions per docs/reference.md.
      await until(() => events.any(
          (e) => e.type == 'turn.state' && e.data['state'] == 'completed'),
          timeoutMs: 20000);
      final completed = events
          .where((e) =>
              e.type == 'turn.state' &&
              e.data['state'] == 'completed')
          .toList();
      expect(completed, isNotEmpty, reason: 'the turn finished');
      // The cursor advanced past the reply through later events (the mock
      // provider's turn state / usage ride after the text).
      expect(live.lastEventId!.length, 36);

      // -- a fresh client reloads everything from history --------------------
      final reloaded = HfyClient(api: base, tokenProvider: mint);
      final historyTexts = <String>[];
      reloaded.events
          .listen((event) => historyTexts.add('${event.type}:${event.data['text']}'));
      await reloaded.connect();
      await until(() =>
          historyTexts.where((t) => t.startsWith('message.text:')).length >= 2);
      expect(
          historyTexts.where((t) => t.contains('Hello flutter')), isNotEmpty);
      expect(historyTexts.where((t) => t.contains('Hello from mock')),
          isNotEmpty);
      expect(reloaded.viewer!.ref, 'u1');
      expect(reloaded.viewer!.type, 'external');
      expect(reloaded.hasOlder, isFalse);
      reloaded.close();

      // -- typed errors + the remaining endpoints ----------------------------
      try {
        await live.approve('no-such-call', true);
        fail('approve of nothing must not succeed silently');
      } on HfyHttpException catch (e) {
        // svc.ErrNoPendingApproval maps to 409 with its exact text.
        expect(e.status, 409);
        expect(e.message, 'no pending approval for that call');
      }
      await live.stop(); // 202 {} — nothing on the wire to throw

      final config = await live.config();
      expect(config.uploads, isNull,
          reason: 'the e2e hfy has no filesystem configured');
      live.close();
      await until(() => live!.state.value == HfyConnectionState.ended);
    } finally {
      live?.close();
      httpForCleanup?.close();
    }
  }, skip: !hasE2E ? 'HFY_E2E_URL / HFY_E2E_SECRET not set' : null,
      timeout: const Timeout(Duration(minutes: 3)));
}
