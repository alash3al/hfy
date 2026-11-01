package provider_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alash3al/hfy/internal/provider"
	"github.com/alash3al/hfy/internal/provider/fakeprov"
)

// A whole-call budget ends a slow answer that the idle timeout alone allows.
// The fake streams about 600ms of chunks, each gap 150ms: no silence reaches
// the 2s idle timeout, but the 400ms budget runs out mid-answer.
func TestCallTimeoutFiresOnSlowAnswer(t *testing.T) {
	f := fakeprov.New(t, fakeprov.Step{Content: "a slow but steady answer", ChunkDelay: 150 * time.Millisecond})
	c := provider.New("p", f.URL1(), "", 2*time.Second)
	start := time.Now()
	_, err := c.Chat(t.Context(), provider.Request{Model: "m",
		Controls: provider.Controls{CallTimeout: 400 * time.Millisecond}},
		provider.Opts{MaxAttempts: 3, Backoff: time.Millisecond})
	if !errors.Is(err, provider.ErrCallTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want ErrCallTimeout, got %v", err)
	}
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("the budget was not enforced: %s", d)
	}
}

// Without a call timeout the same answer finishes: the idle timeout is the
// only limit, and an answer that keeps streaming is never cut off. A budget
// longer than the answer does not cut it off either.
func TestNoCallTimeoutLetsSteadyAnswerFinish(t *testing.T) {
	steady := fakeprov.Step{Content: "a slow but steady answer", ChunkDelay: 150 * time.Millisecond}
	f := fakeprov.New(t, steady, steady)
	c := provider.New("p", f.URL1(), "", 400*time.Millisecond)
	for _, ctl := range []provider.Controls{{}, {CallTimeout: 5 * time.Second}} {
		resp, err := c.Chat(t.Context(), provider.Request{Model: "m", Controls: ctl}, provider.Opts{MaxAttempts: 1})
		if err != nil || resp.Content != "a slow but steady answer" {
			t.Fatalf("controls %+v: %v %+v", ctl, err, resp)
		}
	}
}

// The budget covers retries: a 429 that asks to wait longer than the budget
// ends the call at the budget, and no second attempt is made.
func TestCallTimeoutStopsRetries(t *testing.T) {
	f := fakeprov.New(t, fakeprov.Step{Status: 429, RetryAfter: "5"}, fakeprov.Step{Content: "too late"})
	c := provider.New("p", f.URL1(), "", time.Second)
	start := time.Now()
	_, err := c.Chat(t.Context(), provider.Request{Model: "m",
		Controls: provider.Controls{CallTimeout: 300 * time.Millisecond}},
		provider.Opts{MaxAttempts: 3})
	if !errors.Is(err, provider.ErrCallTimeout) {
		t.Fatalf("want ErrCallTimeout, got %v", err)
	}
	if n := len(f.Reqs()); n != 1 {
		t.Fatalf("a call past its budget was retried: %d requests", n)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the budget was not enforced: %s", d)
	}
}

// A silent provider hits the budget before the idle timeout would fire, and
// the error says it is the budget, not silence.
func TestCallTimeoutBeatsIdleTimeout(t *testing.T) {
	stall := make(chan struct{})
	t.Cleanup(func() { close(stall) })
	f := fakeprov.New(t, fakeprov.Step{Delay: stall})
	c := provider.New("p", f.URL1(), "", 5*time.Second)
	_, err := c.Chat(t.Context(), provider.Request{Model: "m",
		Controls: provider.Controls{CallTimeout: 300 * time.Millisecond}},
		provider.Opts{MaxAttempts: 3})
	if !errors.Is(err, provider.ErrCallTimeout) {
		t.Fatalf("want ErrCallTimeout, got %v", err)
	}
}
