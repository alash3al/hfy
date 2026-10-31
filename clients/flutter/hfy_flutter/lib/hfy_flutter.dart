// hfy_flutter — native Flutter SDK of the hfy conversation embed
// (client + conversation fold; see clients/flutter/PARITY.md).
//
// This file is the ENTIRE public API. Only the names listed in the `show`
// clauses below are exported — everything else is internal and may change in
// lockstep with PARITY.md phases. In particular these stay PRIVATE:
//
//   - the SSE framing internals (HfySseParser, HfySseMessage, HfyIdleWatchdog
//     — ids of the SSE protocol are exposed through the wire envelope
//     contract; parsers are an implementation detail),
//   - the envelope dedupe (HfyDedupe is already wired inside HfyClient),
//   - the REST layer (HfyApi, HfyEventResponse, HfyHistoryPage, HfyTokenGate
//     — apps talk to the server through [HfyClient] only),
//   - the fold's pure helpers (hfySafeUrl, hfyMapAttachments, the state
//     constants) — their results surface on [HfyBlock] and [HfyTurn],
//   - the clock/timer typedefs ([HfyClientOptions.clock] / timerFactory
//     accept plain lambdas without naming them),
//   - test helpers and fixtures (test/).
//
// HfyClientConfig/HfyUploadsConfig/HfyAttachmentRef are the uploads/config
// types of P-CFG-004/P-CFG-026; EVENT_TYPES the wire event kinds.
//
// HfyApi is deliberately NOT exported: every HTTP path goes through
// HfyClient, so callers never build one by hand.
//
// NOTE the two "HfyClock" spellings in src/ deliberately do not collide
// here: the client's (epoch-ms typedef, connection.dart) and the fold's
// (state.dart abstract class) both stay unexported for that reason.

// The networked client (client.js port).
export 'src/client/client.dart'
    show HfyAttachmentRef, HfyClient, HfyClientConfig, HfyClientOptions,
    HfyUploadsConfig;
export 'src/client/connection.dart' show HfyConnectionState;

// The bearer token grant and its expiry parsing.
export 'src/client/token.dart' show HfyGrant;

// Typed HTTP exceptions: 401 is the AUTH subtype, everything else (403 "not
// your turn" included) a plain HfyHttpException.
export 'src/client/errors.dart' show HfyAuthException, HfyHttpException,
    HfyNetworkException;

// The wire event model (SSE + history envelopes).
export 'src/model/event.dart' show EVENT_TYPES, HfyActor, HfyEvent,
    HfyProtocolException;

// The conversation fold's read types (what an app renders).
export 'src/model/ask.dart'
    show HfyAskOption, HfyAskQuestion, HfyAskState;
export 'src/model/fold.dart' show HfyConversation;
export 'src/model/state.dart'
    show HfyAttachment, HfyBlock, HfyToolCalls, HfyTurn, HfyTz, HfyViewer;
