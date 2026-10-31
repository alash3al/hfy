// P-NAT-004: the app lifecycle pauses and resumes the stream through the
// controller. Paused, hidden and detached pause, resumed resumes, inactive
// changes nothing, a resume reconnects from lastEventId, and dispose detaches
// the observer. The mapping is driven directly; the registration is driven
// through the real binding.
import 'dart:io' show HttpOverrides;

import 'package:flutter/foundation.dart'
    show FlutterError, FlutterErrorDetails, FlutterExceptionHandler;
import 'package:flutter/widgets.dart' show AppLifecycleState, WidgetsBinding;
import 'package:flutter_test/flutter_test.dart';
import 'package:hfy_flutter/src/client/connection.dart' show HfyConnectionState;
import 'package:hfy_flutter/src/controller/lifecycle.dart';

import '../client/test_kit.dart';
import 'controller_kit.dart';

ChatKit? _kit;

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();
  final binding = WidgetsBinding.instance;

  late FakeHfyServer server;
  final reported = <FlutterErrorDetails>[];
  FlutterExceptionHandler? previousOnError;
  HttpOverrides? previousOverrides;

  setUp(() async {
    reported.clear();
    previousOnError = FlutterError.onError;
    FlutterError.onError = reported.add;
    // The test binding swaps in an HttpClient that answers 400 to everything;
    // these tests talk to the real fake server over a socket.
    previousOverrides = HttpOverrides.current;
    HttpOverrides.global = null;
    // The binding keeps the last state it was handed: every test starts in
    // the foreground instead of inheriting one.
    binding.handleAppLifecycleStateChanged(AppLifecycleState.resumed);
    server = await FakeHfyServer.start();
    server.historyPages[null] = historyPage(
      turns: const <Object?>[],
      liveAfter: 'la-0',
      viewer: kViewer,
    );
  });

  tearDown(() async {
    _kit?.controller.dispose();
    _kit = null;
    await server.destroy();
    HttpOverrides.global = previousOverrides;
    binding.handleAppLifecycleStateChanged(AppLifecycleState.resumed);
    FlutterError.onError = previousOnError;
    expect(
      reported,
      isEmpty,
      reason: 'nothing may be reported to the framework',
    );
  });

  test(
    'P-NAT-004: the mapping pauses on paused, hidden and detached, resumes on resumed, and inactive is ignored',
    () {
      final edges = <String>[];
      final b = HfyLifecycleBinder(
        onPause: () => edges.add('pause'),
        onResume: () => edges.add('resume'),
      );
      for (final state in [
        AppLifecycleState.resumed,
        AppLifecycleState.paused,
        AppLifecycleState.hidden,
        AppLifecycleState.detached,
        AppLifecycleState.resumed,
        AppLifecycleState.inactive,
      ]) {
        b.didChangeAppLifecycleState(state);
      }
      expect(
        edges,
        ['resume', 'pause', 'pause', 'pause', 'resume'],
        reason:
            'every edge is forwarded; the client ignores the repeats, and '
            'inactive forwards nothing',
      );
    },
  );

  test(
    'P-NAT-004: attach registers with the binding, a second attach is a no-op, detach removes the observer',
    () {
      var pauses = 0;
      final b = HfyLifecycleBinder(onPause: () => pauses++, onResume: () {});
      b.attach(binding);
      b.attach(binding);
      expect(b.attached, isTrue);
      binding.handleAppLifecycleStateChanged(AppLifecycleState.paused);
      expect(pauses, 1, reason: 'one registration, one callback');
      b.detach();
      b.detach();
      expect(b.attached, isFalse);
      binding.handleAppLifecycleStateChanged(AppLifecycleState.resumed);
      binding.handleAppLifecycleStateChanged(AppLifecycleState.paused);
      expect(pauses, 1, reason: 'a detached binder hears nothing');
    },
  );

  test(
    'P-NAT-004: attach while the app is already hidden pauses at once; attach while resumed does nothing',
    () {
      var pauses = 0;
      final b = HfyLifecycleBinder(onPause: () => pauses++, onResume: () {});
      b.attach(binding);
      expect(pauses, 0, reason: 'the app is in the foreground');
      b.detach();
      binding.handleAppLifecycleStateChanged(AppLifecycleState.hidden);
      b.attach(binding);
      expect(pauses, 1, reason: 'the state the binding already holds');
      b.detach();
    },
  );

  test(
    'P-NAT-004: paused stops delivery and resumed reconnects from lastEventId',
    () async {
      _kit = await ChatKit.open(server);
      final c = _kit!.controller;
      c.attachLifecycle(binding);
      await _kit!.connectLive();
      await _kit!.frame(wire('t1', 'turn'));
      await _kit!.frame(agentText('l2', 't1', 'before the background'));
      await until(
        () => c.turns.isNotEmpty && c.turns.single.blocks.isNotEmpty,
      );

      binding.handleAppLifecycleStateChanged(AppLifecycleState.paused);
      expect(c.connection, HfyConnectionState.paused);
      await _kit!.frame(agentText('l3', 't1', 'sent while paused'));

      binding.handleAppLifecycleStateChanged(AppLifecycleState.resumed);
      await until(
        () =>
            c.connection == HfyConnectionState.live &&
            server.sseConnections.length == 2,
      );
      final reopened = server.sseConnections.last.request;
      expect(reopened.lastEventId, 'l2');
      expect(reopened.after, 'l2');

      // The paused frame went to the dropped socket. The marker, written on
      // the resumed stream, shows the stream folds again without it.
      await _kit!.frame(agentText('l4', 't1', 'marker'));
      await until(
        () => c.turns.single.blocks.any((b) => b.text == 'marker'),
      );
      expect(
        [for (final b in c.turns.single.blocks) b.text],
        ['before the background', 'marker'],
        reason: 'the frame sent while paused never reached the controller',
      );
    },
  );

  test(
    'P-NAT-004: attachLifecycle while the app is already backgrounded pauses the stream at once',
    () async {
      _kit = await ChatKit.open(server);
      final c = _kit!.controller;
      await _kit!.connectLive();
      binding.handleAppLifecycleStateChanged(AppLifecycleState.paused);
      c.attachLifecycle(binding);
      expect(c.connection, HfyConnectionState.paused);
      expect(server.sseConnections.length, 1);
      binding.handleAppLifecycleStateChanged(AppLifecycleState.resumed);
      await until(
        () =>
            c.connection == HfyConnectionState.live &&
            server.sseConnections.length == 2,
      );
    },
  );

  test(
    'P-NAT-004: hidden and detached add nothing after a pause, and inactive is ignored',
    () async {
      _kit = await ChatKit.open(server);
      final c = _kit!.controller;
      c.attachLifecycle(binding);
      await _kit!.connectLive();

      binding.handleAppLifecycleStateChanged(AppLifecycleState.paused);
      binding.handleAppLifecycleStateChanged(AppLifecycleState.hidden);
      binding.handleAppLifecycleStateChanged(AppLifecycleState.detached);
      expect(c.connection, HfyConnectionState.paused);
      expect(server.sseConnections.length, 1);

      binding.handleAppLifecycleStateChanged(AppLifecycleState.resumed);
      await until(
        () =>
            c.connection == HfyConnectionState.live &&
            server.sseConnections.length == 2,
      );
      binding.handleAppLifecycleStateChanged(AppLifecycleState.resumed);
      binding.handleAppLifecycleStateChanged(AppLifecycleState.inactive);
      await settle();
      expect(c.connection, HfyConnectionState.live);
      expect(
        server.sseConnections.length,
        2,
        reason: 'a second resume reopens nothing',
      );
    },
  );

  test(
    'P-NAT-004: attachLifecycle registers once, and dispose detaches the observer',
    () async {
      _kit = await ChatKit.open(server);
      final c = _kit!.controller;
      c.attachLifecycle(binding);
      c.attachLifecycle(binding);
      expect(c.lifecycleAttached, isTrue);
      c.dispose();
      expect(c.lifecycleAttached, isFalse);
      binding.handleAppLifecycleStateChanged(AppLifecycleState.paused);
      binding.handleAppLifecycleStateChanged(AppLifecycleState.resumed);
      await settle();
      expect(c.connection, HfyConnectionState.ended);
    },
  );
}
