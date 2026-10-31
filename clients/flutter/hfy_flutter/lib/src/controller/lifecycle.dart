// HfyLifecycleBinder: the app-lifecycle half of P-NAT-004. Going to the
// background closes the SSE stream (pause); coming back resumes it through the
// reconnect path from lastEventId (resume). The web client has no equivalent.
//
// Mapping: paused, hidden and detached pause; resumed resumes; inactive (a
// transient overlay, a call prompt) changes nothing. Each edge fires once: a
// second pause while paused, or a resume while not paused, is a no-op.
import 'package:flutter/widgets.dart'
    show AppLifecycleState, WidgetsBinding, WidgetsBindingObserver;

class HfyLifecycleBinder with WidgetsBindingObserver {
  HfyLifecycleBinder({required this.onPause, required this.onResume});

  final void Function() onPause;
  final void Function() onResume;

  WidgetsBinding? _binding;
  bool _paused = false;

  bool get attached => _binding != null;

  /// Registers this observer with [binding]; a second attach is a no-op.
  void attach(WidgetsBinding binding) {
    if (_binding != null) return;
    _binding = binding;
    binding.addObserver(this);
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
    switch (state) {
      case AppLifecycleState.paused:
      case AppLifecycleState.hidden:
      case AppLifecycleState.detached:
        if (_paused) return;
        _paused = true;
        onPause();
      case AppLifecycleState.resumed:
        if (!_paused) return;
        _paused = false;
        onResume();
      case AppLifecycleState.inactive:
        return;
    }
  }
}
