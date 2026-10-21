package provider

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestStreamToolCallIndicesNeedNotBeContiguous(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"a","function":{"name":"x__one","arguments":"{}"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":3,"id":"b","function":{"name":"x__two","arguments":"{}"}}]}}]}

data: [DONE]
`
	resp, err := readStream(strings.NewReader(sse), Opts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 2 || resp.ToolCalls[0].ID != "a" || resp.ToolCalls[1].ID != "b" {
		t.Fatalf("calls: %+v", resp.ToolCalls)
	}
}

// Servers name reasoning differently (DeepSeek: reasoning_content; Ollama and
// vLLM: reasoning), streamed or not.
func TestNonStreamReadsEitherReasoningField(t *testing.T) {
	for _, field := range []string{"reasoning_content", "reasoning"} {
		body := `{"choices":[{"message":{"content":"hi","` + field + `":"thought"},"finish_reason":"stop"}]}`
		resp, err := readJSON(strings.NewReader(body), Opts{})
		if err != nil || resp.Content != "hi" || resp.Reasoning != "thought" {
			t.Fatalf("%s: %+v %v", field, resp, err)
		}
	}
}

// A stream that stops before [DONE] or a finish_reason is not an answer, and
// an error a server sends mid-stream is an error, not an empty reply.
func TestStreamMustFinish(t *testing.T) {
	text := `data: {"choices":[{"delta":{"content":"Half an ans"}}]}` + "\n\n"
	if _, err := readStream(strings.NewReader(text), Opts{}); !errors.Is(err, errCutShort) {
		t.Fatalf("cut short: %v", err)
	}
	finished := text + `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" // no [DONE]: still whole
	if resp, err := readStream(strings.NewReader(finished), Opts{}); err != nil || resp.Content != "Half an ans" {
		t.Fatalf("finished without [DONE]: %+v %v", resp, err)
	}
	for _, e := range []string{`{"message":"overloaded"}`, `"overloaded"`} {
		failed := text + `data: {"error":` + e + `}` + "\n\ndata: [DONE]\n\n"
		if _, err := readStream(strings.NewReader(failed), Opts{}); err == nil || !strings.Contains(err.Error(), "overloaded") {
			t.Fatalf("error chunk %s: %v", e, err)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	for h, want := range map[string]time.Duration{
		"":                              0,
		"7":                             7 * time.Second,
		"Wed, 21 Oct 2026 12:00:30 GMT": 30 * time.Second,
		"3600":                          maxRetryAfter, // capped
		"-5":                            0,
		"soon":                          0,
	} {
		if got := retryAfter(h, now); got != want {
			t.Errorf("Retry-After %q = %v, want %v", h, got, want)
		}
	}
}
