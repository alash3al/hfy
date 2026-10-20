INSERT INTO outbound_requests (turn_event_id, kind, url, request, response, status_code, latency_ms, attempt, error)
VALUES (@turn_event_id, @kind, @url, @request, @response, @status_code, @latency_ms, @attempt, @error)
RETURNING id
