// HTTP errors, exactly the client.js error contract (client.js:214,220-226,
// PARITY.md P-CLIENT-022) plus the typed shape the Dart SDK owes its callers:
// message = the server's `{"error": "..."}` text, else 'HTTP `<status>`'; the
// numeric status rides along. reference.md maps: 400 bad input, 401 no valid
// token, 403 not your turn (or a grant whose agent is gone), 404 unknown
// conversation, 409 no pending approval, 413 a body over 2 MiB.
//
// The AUTH subtype is 401 ONLY (review item 6): a 403 "not your turn" is a
// plain HfyHttpException — the token is valid, the grant just may not act in
// this moment, so callers must not read it as "authenticate again".
//
// Server-provided error text is sanitized before it becomes [message]: control
// characters are stripped and the text is clamped at 512 chars (review item
// 10), so a proxy's HTML error page can never become a giant message.
//
// Transport-level failures (socket/refused/timeout — anything dio raises
// before a HTTP status exists) never carry the bearer request headers: they
// surface as [HfyNetworkException] (review item 5), whose message/toString
// contain neither headers nor URL query.
import 'package:dio/dio.dart' show DioException;

const int _maxErrorMessageChars = 512;

/// Strips control characters and clamps [text] at
/// [_maxErrorMessageChars] chars; used for server-supplied error bodies.
String sanitizeServerError(String text) {
  final cleaned = text.replaceAll(RegExp(r'[\x00-\x1F\x7F]'), '');
  if (cleaned.length <= _maxErrorMessageChars) return cleaned;
  return cleaned.substring(0, _maxErrorMessageChars);
}

class HfyHttpException implements Exception {
  final int status;

  /// The server's error text (sanitized, at most 512 chars), or
  /// 'HTTP `<status>`' when it sent none.
  final String message;

  /// The parsed JSON body when the response carried a decodable one.
  /// Diagnostics only; never contains the token.
  final Object? body;

  const HfyHttpException(this.status, this.message, {this.body});

  /// Maps one response by status: 401 is [HfyAuthException], everything else
  /// (403 "not your turn" included) a plain [HfyHttpException]. `[parsed]` is
  /// the decoded body (client.js readBody shape: `{error: text}` when JSON
  /// parsing failed).
  factory HfyHttpException.fromStatus(int status, Object? parsed,
      {required String fallback}) {
    final message = _message(parsed, status, fallback);
    final body = parsed == null
        ? const <String, Object?>{}
        : Map<String, Object?>.from(parsed is Map ? parsed : <String, Object?>{});
    if (status == 401) {
      return HfyAuthException(status, message, body: body);
    }
    return HfyHttpException(status, message, body: body);
  }

  // client.js:222 — `(parsed && parsed.error) || ('HTTP ' + status)`: a
  // non-empty string wins; everything else (null, empty) falls back. The
  // server text is sanitized/clamped before use (review item 10).
  static String _message(Object? parsed, int status, String fallback) {
    final error = parsed is Map ? parsed['error'] : null;
    if (error is String && error.isNotEmpty) return sanitizeServerError(error);
    return fallback;
  }

  @override
  String toString() => 'HfyHttpException $status: $message';
}

/// 401 only: the token is missing, unknown, expired or revoked (the 403
/// "not your turn" family is an ordinary [HfyHttpException] now — see the
/// file docs and review item 6).
class HfyAuthException extends HfyHttpException {
  const HfyAuthException(super.status, super.message, {super.body});

  @override
  String toString() => 'HfyAuthException $status: $message';
}

/// A transport failure (connect refused/timeout, socket death, dio-level
/// cancellation) — no HTTP status exists. The wrapped [DioException] carries
/// the request's headers (the bearer token) and absolute URL, so it is NEVER
/// rethrown as-is: [message] is built only from dio's error TYPE and its own
/// short text, and [toString] exposes nothing else (review item 5 — a
/// connection failure must be printable without leaking credentials).
class HfyNetworkException implements Exception {
  final String message;

  const HfyNetworkException(this.message);

  /// Maps one raw dio failure to the safe shape. `DioException.message` is
  /// dio's own short human text ("Connection timed out", …) and `type` the
  /// failure class; neither embeds the request URL or headers.
  factory HfyNetworkException.fromDio(DioException error) {
    final detail = error.message ?? (error.error?.toString()) ?? 'no detail';
    return HfyNetworkException('${error.type.name}: ${sanitizeServerError(detail)}');
  }

  @override
  String toString() => 'HfyNetworkException: $message';
}
