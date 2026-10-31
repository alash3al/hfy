// Shared wiring for the controller suite: one HfyChatController over a real
// HfyClient on the fake server (the client test kit's fake clock and timers),
// plus wire-envelope builders for scripted conversations.
import 'package:hfy_flutter/src/controller/chat_controller.dart';

import '../client/test_kit.dart';

const String kViewerUuid = 'u-viewer';

/// The grant's actor in every history page this suite serves.
const Map<String, Object?> kViewer = <String, Object?>{
  'uuid': kViewerUuid,
  'type': 'user',
  'ref': 'me',
};

/// One wire envelope. The agent is the default actor, like the server's.
Map<String, Object?> wire(
  String id,
  String type, {
  String? parent,
  Map<String, Object?>? data,
  String uuid = 'u-agent',
  String actorType = 'agent',
  String ref = 'main',
}) => <String, Object?>{
  'id': id,
  'type': type,
  if (parent != null) 'parent': parent,
  'actor': <String, Object?>{'uuid': uuid, 'type': actorType, 'ref': ref},
  'v': 1,
  'data': data ?? <String, Object?>{},
  'created_at': '2026-01-01T00:00:00Z',
};

Map<String, Object?> userText(
  String id,
  String turn,
  String text, {
  String uuid = kViewerUuid,
}) => wire(
  id,
  'message.text',
  parent: turn,
  data: <String, Object?>{'text': text},
  uuid: uuid,
  actorType: 'user',
  ref: 'me',
);

Map<String, Object?> agentText(String id, String turn, String text) => wire(
  id,
  'message.text',
  parent: turn,
  data: <String, Object?>{'text': text},
);

Map<String, Object?> turnState(
  String id,
  String turn,
  String state, {
  String? error,
}) => wire(
  id,
  'turn.state',
  parent: turn,
  data: <String, Object?>{'state': state, 'error': ?error},
);

Map<String, Object?> toolCall(
  String id,
  String turn,
  String callId,
  String name, {
  Object? arguments,
}) => wire(
  id,
  'tool_call',
  parent: turn,
  data: <String, Object?>{
    'call_id': callId,
    'name': name,
    'arguments': ?arguments,
  },
);

Map<String, Object?> approvalRequested(String id, String turn, String callId) =>
    wire(
      id,
      'approval.requested',
      parent: turn,
      data: <String, Object?>{'call_id': callId},
    );

Map<String, Object?> approvalDecided(
  String id,
  String turn,
  String callId, {
  required bool approve,
}) => wire(
  id,
  'approval.decided',
  parent: turn,
  data: <String, Object?>{'call_id': callId, 'approve': approve},
);

/// A `hfy.ask_user` tool's arguments: `{questions: [...]}`.
Map<String, Object?> askArgs(List<Map<String, Object?>> questions) =>
    <String, Object?>{'questions': questions};

Map<String, Object?> textQuestion(String id) => <String, Object?>{
  'id': id,
  'type': 'text',
  'text': 'Question $id',
};

Map<String, Object?> singleQuestion(String id, List<String> values) =>
    <String, Object?>{
      'id': id,
      'type': 'single',
      'text': 'Question $id',
      'options': <Map<String, Object?>>[
        for (final v in values) <String, Object?>{'value': v, 'label': v},
      ],
    };

/// One history turn: `{events: [...]}`, oldest-first inside a page.
Map<String, Object?> pageTurn(List<Map<String, Object?>> events) =>
    <String, Object?>{'events': events};

/// The controller wired to one real client on [server].
class ChatKit {
  ChatKit._(this.server, this.kit, this.controller);

  final FakeHfyServer server;
  final ClientKit kit;
  final HfyChatController controller;

  static Future<ChatKit> open(
    FakeHfyServer server, {
    HfyChatOptions options = const HfyChatOptions(),
    HfyTokenProvider? provider,
  }) async {
    final kit = await ClientKit.open(server, provider: provider);
    return ChatKit._(
      server,
      kit,
      HfyChatController(kit.client, options: options),
    );
  }

  /// The SSE stream of the most recent open.
  FakeSseConnection get sse => server.sseConnections.last;

  /// connect(), then wait until the background stream is open.
  Future<void> connectLive() async {
    await controller.connect();
    await kit.waitLive();
  }

  Future<void> frame(Map<String, Object?> envelope) => sse.addFrame(envelope);

  /// Recorded requests whose path (without the query) is [path].
  List<RecordedRequest> requestsTo(String path) =>
      server.requests.where((r) => Uri.parse(r.path).path == path).toList();
}
