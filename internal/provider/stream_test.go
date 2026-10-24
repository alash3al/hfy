package provider

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// An assistant message that only calls tools sends null content, a call
// without arguments sends {}, and a tool without parameters an empty object
// schema.
func TestRequestBodyDefaults(t *testing.T) {
	b, err := json.Marshal(buildBody(Request{Model: "m",
		Messages: []Message{{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "x__now"}}}},
		Tools:    []ToolDef{{Name: "x__now"}}}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"content":null`, `"arguments":"{}"`, `"parameters":{"type":"object","properties":{}}`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("body lacks %s: %s", want, b)
		}
	}
}

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

func TestRedactBodySignedURL(t *testing.T) {
	body := []byte(`{"messages":[{"image_url":{"url":"https://bucket.s3.amazonaws.com/k.png?X-Amz-Signature=abc&X-Amz-Expires=900"},"other":"https://cdn.example.com/a.png?size=1","design":"https://cdn.example.com/b.png?design=1","inline":"data:image/png;base64,AAAA","text":"see https://hfy.example/v1/files/k.pdf?exp=1&sig=deadbeef (application/pdf)"}]}`)
	out := string(redactBody(body))
	for _, want := range []string{`"url":"https://bucket.s3.amazonaws.com/k.png"`, `"other":"https://cdn.example.com/a.png?size=1"`, `"design":"https://cdn.example.com/b.png?design=1"`, `"inline":"[inline attachment]"`, "see https://hfy.example/v1/files/k.pdf?exp=1 (application/pdf)"} {
		if !strings.Contains(out, want) {
			t.Errorf("redacted body lacks %s: %s", want, out)
		}
	}
	if strings.Contains(out, "X-Amz-Signature") || strings.Contains(out, "sig=deadbeef") {
		t.Errorf("signed query must be stripped: %s", out)
	}
}

// A provider that fails mid-request may echo what it received, signed URLs
// included: neither the reported error nor the stored trace may keep them.
func TestAPIErrorRedactsSignedURLs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.Copy(w, r.Body)
	}))
	defer srv.Close()
	c := New("p", srv.URL, "", time.Second)
	var recs []Record
	_, err := c.Chat(t.Context(), Request{Model: "m", Messages: []Message{{Role: "user", Parts: []Part{{Type: "image_url",
		ImageURL: &ImageURL{URL: "https://bucket.s3.amazonaws.com/k.png?X-Amz-Signature=abc&X-Amz-Expires=900"}}}}}},
		Opts{MaxAttempts: 1, Record: func(r Record) int64 { recs = append(recs, r); return 0 }})
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "X-Amz-Signature") || strings.Contains(err.Error(), "sig=") {
		t.Errorf("APIError keeps the signature: %s", err)
	}
	if len(recs) != 1 || (recs[0].Error != "" && strings.Contains(recs[0].Error, "X-Amz-Signature")) {
		t.Errorf("trace error keeps the signature: %+v", recs)
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
