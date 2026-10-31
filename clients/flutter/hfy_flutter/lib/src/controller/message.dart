// Values the chat controller hands to the UI: one user-facing message and one
// approval/answer decision (PARITY.md P-CMP-015, P-FOLD-055, P-SH-014).

/// One message for the UI: a localizable [key] (resolved through HfyL10n when
/// shown) or server [text] shown exactly as the server sent it (P-CMP-015).
class HfyMessage {
  const HfyMessage.key(this.key) : text = null;

  const HfyMessage.text(this.text) : key = null;

  final String? key;
  final String? text;

  @override
  bool operator ==(Object other) =>
      other is HfyMessage && other.key == key && other.text == text;

  @override
  int get hashCode => Object.hash(key, text);

  @override
  String toString() =>
      text != null ? 'HfyMessage.text($text)' : 'HfyMessage.key($key)';
}

/// One approval or answer set to submit (P-FOLD-055). [answers] is set for an
/// ask and is sent with `approve: true`; [questionIds] lists the set's
/// questions in order, so a failure can land on the one the server names. An
/// ask sent without them takes them from its pending block.
class HfyDecision {
  const HfyDecision({
    required this.callId,
    required this.approve,
    this.answers,
    this.questionIds = const <String>[],
  });

  final String callId;
  final bool approve;
  final Map<String, Object?>? answers;
  final List<String> questionIds;
}
