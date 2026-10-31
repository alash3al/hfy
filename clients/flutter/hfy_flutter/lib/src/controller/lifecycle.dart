// HfyLifecycleBinder: the app-lifecycle half of P-NAT-004. Going to the
// background closes the SSE stream (pause); coming back resumes it through the
// reconnect path from lastEventId (resume). The web client has no equivalent.
//
// Mapping: paused, hidden and detached pause; resumed resumes; inactive (a
// transient overlay, a call prompt) changes nothing. Repeated edges are passed
// on as they come: HfyClient.pause() and resume() already ignore a repeat.
import 'package:flutter/widgets.dart'
    show AppLifecycleState, WidgetsBinding, WidgetsBindingObserver;

class HfyLifecycleBinder with WidgetsBindingObserver {
  HfyLifecycleBinder({required this.onPause, required this.onResume});

  final void Function() onPause;
  final void Function() onResume;

  WidgetsBinding? _binding;

  bool get attached => _binding != null;

  /// Registers this observer with [binding]; a second attach is a no-op. An
  /// app already in the background pauses at once, since the observer only
  /// hears edges from here on.
  void attach(WidgetsBinding binding) {
    if (_binding != null) return;
    _binding = binding;
    binding.addObserver(this);
    if (_backgrounded(binding.lifecycleState)) onPause();
  }

  /// Unregisters from the binding it attached to; idempotent.
  void detach() {
    final binding = _binding;
    if (binding == null) return;
    _binding = null;
    binding.removeObserver(this);
  }

  @override
  void didChangeAppLifecycleState(AppLifecycleState state) {
    if (_backgrounded(state)) {
      onPause();
    } else if (state == AppLifecycleState.resumed) {
      onResume();
    }
  }

  static bool _backgrounded(AppLifecycleState? state) =>
      state == AppLifecycleState.paused ||
      state == AppLifecycleState.hidden ||
      state == AppLifecycleState.detached;
}
