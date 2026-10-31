// Connection lifecycle of the hfy client: the exact state machine and
// transitions of the embedded web client (client.js #setState, PARITY.md
// P-CLIENT-031), plus the native-only `paused` state for the app-lifecycle
// pause()/resume() pair (P-NAT-004 analog; the web has no equivalent).
import 'dart:async' show Timer;

enum HfyConnectionState {
  /// Never connected. A pause() before the first connect() is recorded and the
  /// state stays idle until connect().
  idle,

  /// connect() is in flight (token + first history page).
  connecting,

  /// The SSE stream is open and delivering.
  live,

  /// A proactive token refresh is running (stream reopens on success).
  refreshing,

  /// The stream died; the backoff ladder is waiting to reopen.
  reconnecting,

  /// connect() failed (retry re-enters [connecting] via ui.js retry()).
  error,

  /// close() ended the client. Terminal (P-CLIENT-014).
  ended,

  /// NEW, native-only (P-NAT-004 analog): pause() closed the stream and
  /// cancelled the timers without ending the client; resume() returns via
  /// the reconnect path. A connect() made while paused loads history and
  /// stops here. Never produced by the web client.
  paused,
}

/// Wall clock in epoch milliseconds (client.js `Date.now()`); injectable for
/// tests (PARITY.md §11-2: fake clocks instead of real waits).
typedef HfyClock = int Function();

/// One delayed timer handle (client.js `timer(fn, ms)`); injectable so tests
/// can fire the ladder/refresh/reconnect timers by hand instead of sleeping.
typedef HfyTimerFactory = Timer Function(Duration, void Function());
