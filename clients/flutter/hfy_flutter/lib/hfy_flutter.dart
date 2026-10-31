// hfy_flutter — native Flutter SDK of the hfy conversation embed
// (client + UI in one package; see clients/flutter/PARITY.md).
//
// Phase 1 exposes the pure, fully tested building blocks only — the wire
// event model, the SSE framing layer and the envelope dedupe. The networked
// client and the UI land in later phases, so anything NOT exported below is
// internal and may change in lockstep with PARITY.md phases.
export 'src/client/dedupe.dart' show HfyDedupe;
export 'src/client/sse.dart' show HfyIdleWatchdog, HfySseMessage, HfySseParser;
export 'src/client/token.dart' show HfyGrant;
export 'src/model/event.dart'
    show EVENT_TYPES, HfyActor, HfyEvent, HfyProtocolException;
