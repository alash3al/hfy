package provider_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alash3al/hfy/internal/provider"
	"github.com/alash3al/hfy/internal/provider/fakeprov"
)

func TestChatStreamingToolCallsAndUsage(t *testing.T) {
	f := fakeprov.New(t, fakeprov.Step{
		Content: "hello world", Reasoning: "thinking",
		Calls:  []fakeprov.Call{{ID: "c1", Name: "files__read", Args: `{"path":"/a"}`}},
		Prompt: 42, Output: 7,
	})
	c := provider.New("p", f.URL1(), "key", time.Second)
	var text, reasoning strings.Builder
	var recs []provider.Record
	resp, err := c.Chat(context.Background(), provider.Request{Model: "m", Messages: []provider.Message{{Role: "user", Content: "hi"}}}, provider.Opts{
		OnText: func(s string) { text.WriteString(s) }, OnReasoning: func(s string) { reasoning.WriteString(s) },
		Record: func(r provider.Record) int64 { recs = append(recs, r); return 9 },
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "hello world" || text.String() != "hello world" || reasoning.String() != "thinking" {
		t.Fatalf("content %q %q %q", resp.Content, text.String(), reasoning.String())
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Arguments != `{"path":"/a"}` || resp.ToolCalls[0].ID != "c1" {
		t.Fatalf("calls %+v", resp.ToolCalls)
	}
	if resp.PromptTokens != 42 || resp.Usage.Output != 7 || resp.RequestID != 9 {
		t.Fatalf("usage %+v", resp)
	}
	if len(recs) != 1 || strings.Contains(string(recs[0].Request), "key") {
		t.Fatalf("records %+v", recs)
	}
}

func TestChatNonStreamAndRetry(t *testing.T) {
	f := fakeprov.New(t,
		fakeprov.Step{Status: 503},
		fakeprov.Step{Status: 429},
		fakeprov.Step{NoStream: true, Content: "finally"},
	)
	c := provider.New("p", f.URL1(), "", time.Second)
	var attempts []int
	resp, err := c.Chat(context.Background(), provider.Request{Model: "m"}, provider.Opts{
		MaxAttempts: 3, Backoff: time.Millisecond,
		Record: func(r provider.Record) int64 { attempts = append(attempts, r.Attempt); return 1 },
	})
	if err != nil || resp.Content != "finally" {
		t.Fatalf("%v %+v", err, resp)
	}
	if len(attempts) != 3 {
		t.Fatalf("each attempt must be recorded, got %v", attempts)
	}
}

func TestChatClientErrorDoesNotRetry(t *testing.T) {
	f := fakeprov.New(t, fakeprov.Step{Status: 400}, fakeprov.Step{Content: "never"})
	c := provider.New("p", f.URL1(), "", time.Second)
	_, err := c.Chat(context.Background(), provider.Request{Model: "m"}, provider.Opts{MaxAttempts: 3, Backoff: time.Millisecond})
	if err == nil || len(f.Reqs()) != 1 {
		t.Fatalf("err=%v requests=%d", err, len(f.Reqs()))
	}
}

func TestEmbed(t *testing.T) {
	f := fakeprov.New(t)
	c := provider.New("p", f.URL1(), "", time.Second)
	var recs []provider.Record
	v, err := c.Embed(t.Context(), "e", []string{"a b", "c"}, func(r provider.Record) int64 { recs = append(recs, r); return 1 })
	if err != nil || len(v) != 2 || len(v[0]) != 3 {
		t.Fatalf("%v %v", v, err)
	}
	// Same trace as chat: one row per attempt, status, and no vectors stored.
	if len(recs) != 1 || recs[0].Status != 200 || string(recs[0].Response) != `{"count":2}` || !strings.HasSuffix(recs[0].URL, "/embeddings") {
		t.Fatalf("trace %+v", recs)
	}
}

// The timeout bounds silence, not the whole answer: a long generation that
// keeps streaming must not be cut off and retried.
func TestChatTimeoutIsIdleNotTotal(t *testing.T) {
	f := fakeprov.New(t, fakeprov.Step{Content: "a slow but steady answer", ChunkDelay: 150 * time.Millisecond})
	c := provider.New("p", f.URL1(), "", 400*time.Millisecond)
	resp, err := c.Chat(context.Background(), provider.Request{Model: "m"}, provider.Opts{MaxAttempts: 1})
	if err != nil || resp.Content != "a slow but steady answer" {
		t.Fatalf("%v %+v", err, resp)
	}

	stall := make(chan struct{})
	t.Cleanup(func() { close(stall) })
	f.Push(fakeprov.Step{Delay: stall})
	start := time.Now()
	if _, err := c.Chat(context.Background(), provider.Request{Model: "m"}, provider.Opts{MaxAttempts: 1}); err == nil {
		t.Fatal("a silent provider must time out")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("timeout took %s", d)
	}
}

// A retried answer starts over: the hook tells the caller to drop what the
// failed attempt streamed, and a provider's Retry-After sets the wait.
func TestRetryStartsOver(t *testing.T) {
	f := fakeprov.New(t,
		fakeprov.Step{Content: "Hello wor", CutShort: true},
		fakeprov.Step{Status: 429, RetryAfter: "1"},
		fakeprov.Step{Content: "Hello world"},
	)
	c := provider.New("p", f.URL1(), "", time.Second)
	var streamed string
	retries := 0
	start := time.Now()
	resp, err := c.Chat(t.Context(), provider.Request{Model: "m"}, provider.Opts{
		MaxAttempts: 3, Backoff: time.Millisecond,
		OnText:  func(s string) { streamed += s },
		OnRetry: func() { retries++; streamed = "" },
	})
	if err != nil || resp.Content != "Hello world" || streamed != "Hello world" || retries != 2 {
		t.Fatalf("resp=%+v err=%v streamed=%q retries=%d", resp, err, streamed, retries)
	}
	if time.Since(start) < time.Second {
		t.Fatal("Retry-After: 1 must be honored")
	}
}
