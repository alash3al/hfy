// HTTP errors, exactly the client.js error contract (client.js:214,220-226,
// PARITY.md P-CLIENT-022) plus the typed shape the Dart SDK owes its callers:
// message = the server's `{"error": "..."}` text, else 'HTTP `<status>`'; the
// numeric status rides along. reference.md maps: 400 bad input, 401 no valid
// token, 403 not your turn (or a grant whose agent is gone), 404 unknown
// conversation, 409 no pending approval, 413 a body over 2 MiB.
class HfyHttpException implements Exception {
  final int status;

  /// The server's error text, or 'HTTP `<status>`' when it sent none.
  final String message;

  /// The parsed JSON body when the response carried a decodable one.
  /// Diagnostics only; never contains the token.
  final Object? body;

  const HfyHttpException(this.status, this.message, {this.body});

  /// Maps one response by status: 401/403 are [HfyAuthException], everything
  /// else a plain [HfyHttpException]. `[parsed]` is the decoded body (client.js
  /// readBody shape: `{error: text}` when JSON parsing failed).
  factory HfyHttpException.fromStatus(int status, Object? parsed,
      {required String fallback}) {
    final message = _message(parsed, status, fallback);
    final body =
        parsed == null ? const <String, Object?>{} : Map<String, Object?>.from(
            parsed is Map ? parsed : <String, Object?>{});
    if (status == 401 || status == 403) {
      return HfyAuthException(status, message, body: body);
    }
    return HfyHttpException(status, message, body: body);
  }

  // client.js:222 — `(parsed && parsed.error) || ('HTTP ' + status)`: a
  // non-empty string wins; everything else (null, empty) falls back.
  static String _message(Object? parsed, int status, String fallback) {
    final error = parsed is Map ? parsed['error'] : null;
    if (error is String && error.isNotEmpty) return error;
    return fallback;
  }

  @override
  String toString() => 'HfyHttpException $status: $message';
}

/// 401/403: the token is missing, unknown, expired or revoked, or the grant
/// answers for an agent the config no longer has (docs/reference.md).
class HfyAuthException extends HfyHttpException {
  const HfyAuthException(super.status, super.message, {super.body});

  @override
  String toString() => 'HfyAuthException $status: $message';
}
