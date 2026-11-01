package agent_test

import (
	"strings"
	"testing"
	"time"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/provider/fakeprov"
	"github.com/alash3al/hfy/internal/types"
)

// Each agent's request controls reach its own calls and no one else's: the
// main agent, a named agent, a sub-agent and the summarizer, with the
// provider's max_tokens_param naming the output cap.
func TestRequestControlsReachMainAndNamedAgents(t *testing.T) {
	e := setup(t, func(c *config.Config) {
		p := c.Providers["fake"]
		p.MaxTokensParam = "max_completion_tokens"
		c.Providers["fake"] = p
		c.Main().Settings.ReasoningEffort = "low"
		c.Main().Settings.MaxOutputTokens = 100
		c.Main().Settings.CallTimeout = config.Duration(time.Minute)
		c.Main().Settings.Extra = map[string]any{"cache_control": map[string]any{"type": "ephemeral"}}
		c.Agents["store"] = &config.Agent{Model: "fake/m", System: "You answer for the store.",
			Settings: config.Settings{ContextWindow: 100000, ReasoningEffort: "high", MaxOutputTokens: 200}}
	}, fakeprov.Step{Content: "main reply"}, fakeprov.Step{Content: "store reply"})
	e.waitState(e.post("hi"), types.StateCompleted)
	e.waitState(e.postAs("u2", "store", "hi"), types.StateCompleted)

	reqs := e.fake.Reqs()
	if len(reqs) != 2 {
		t.Fatalf("model calls: %d", len(reqs))
	}
	main, store := reqs[0], reqs[1]
	if main["reasoning_effort"] != "low" || main["max_completion_tokens"] != 100.0 || main["cache_control"] == nil {
		t.Errorf("main agent's controls not sent: %v", main)
	}
	if store["reasoning_effort"] != "high" || store["max_completion_tokens"] != 200.0 || store["cache_control"] != nil {
		t.Errorf("named agent's own controls not sent alone: %v", store)
	}
	if _, ok := main["max_tokens"]; ok {
		t.Error("max_tokens sent though the provider names max_completion_tokens")
	}
}

// A sub-agent gets its own controls and not its parent's cap; the summarizer
// gets its own, and the default max_tokens name.
func TestRequestControlsReachSubagentAndSummarizer(t *testing.T) {
	long := strings.Repeat("word ", 80)
	e := setup(t, func(c *config.Config) {
		c.Main().Settings.ReasoningEffort = "low"
		c.Main().Settings.MaxOutputTokens = 100
		c.Subagents = map[string]config.Subagent{"refunds": {
			Description: "Decides refund eligibility", Model: "fake/m", System: "You are a refunds specialist.",
			Settings: config.Settings{ReasoningEffort: "minimal"},
		}}
	},
		fakeprov.Step{Calls: []fakeprov.Call{{ID: "r", Name: "hfy__refunds", Args: `{"task":"order 42"}`}}},
		fakeprov.Step{Content: "eligible"},
		fakeprov.Step{Content: "done"},
	)
	e.waitState(e.post("refund order 42"), types.StateCompleted)
	reqs := e.fake.Reqs()
	if len(reqs) != 3 || reqs[1]["reasoning_effort"] != "minimal" || reqs[1]["max_tokens"] != nil {
		t.Fatalf("sub-agent request: %v", reqs)
	}
	if reqs[0]["reasoning_effort"] != "low" || reqs[0]["max_tokens"] != 100.0 {
		t.Fatalf("main agent request: %v", reqs[0])
	}

	e = setup(t, func(c *config.Config) {
		c.Main().Settings.ContextWindow = 150
		c.Summarizer = &config.Summarizer{Model: "fake/m", KeepRecent: 1,
			Settings: config.Settings{ContextWindow: 4000, ReasoningEffort: "none", MaxOutputTokens: 300}}
	},
		fakeprov.Step{Content: "reply one"},
		fakeprov.Step{Content: "reply two"},
		fakeprov.Step{Content: "THE SUMMARY"},
		fakeprov.Step{Content: "reply three"},
	)
	for _, text := range []string{"first ", "second ", "third "} {
		e.waitState(e.post(text+long), types.StateCompleted)
	}
	var summarizer map[string]any
	for _, r := range e.fake.Reqs() {
		if msgs, ok := r["messages"].([]any); ok && len(msgs) > 1 &&
			strings.Contains(msgs[1].(map[string]any)["content"].(string), "New conversation to fold in") {
			summarizer = r
		}
	}
	if summarizer == nil {
		t.Fatal("the summarizer was never called")
	}
	if summarizer["reasoning_effort"] != "none" || summarizer["max_tokens"] != 300.0 {
		t.Fatalf("summarizer request: %v", summarizer)
	}
}

// call_timeout bounds the whole model call: a call that never answers ends
// the turn at the budget, without a second attempt.
func TestCallTimeoutEndsTheTurn(t *testing.T) {
	stall := make(chan struct{})
	t.Cleanup(func() { close(stall) })
	e := setup(t, func(c *config.Config) {
		c.Main().Settings.CallTimeout = config.Duration(300 * time.Millisecond)
	}, fakeprov.Step{Delay: stall})
	turn := e.post("hi")
	e.waitState(turn, types.StateFailed)
	if !strings.Contains(e.dump(turn), "exceeded call_timeout") {
		t.Fatalf("the failure does not name the budget:\n%s", e.dump(turn))
	}
	if n := len(e.fake.Reqs()); n != 1 {
		t.Fatalf("model calls: %d", n)
	}
}
