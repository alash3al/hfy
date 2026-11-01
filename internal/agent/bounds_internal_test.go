package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/mcp"
	"github.com/alash3al/hfy/internal/mcp/builtins"
	"github.com/alash3al/hfy/internal/provider"
)

// scripted answers each model request with the next reply and keeps the requests.
type scripted struct {
	replies []*provider.Response
	reqs    []provider.Request
}

func (s *scripted) Chat(_ context.Context, req provider.Request, _ provider.Opts) (*provider.Response, error) {
	s.reqs = append(s.reqs, req)
	if len(s.replies) == 0 {
		return nil, errors.New("no scripted reply left")
	}
	r := s.replies[0]
	s.replies = s.replies[1:]
	return r, nil
}

func (s *scripted) Embed(context.Context, string, []string, func(provider.Record) int64) ([][]float32, error) {
	return nil, errors.New("embeddings are not scripted")
}

// testSink is a quiet sink (as sub-agents run with): it keeps nothing, but
// counts its durable events the way a turn's sink does.
type testSink struct{ env *TurnEnv }

func (s testSink) TextDelta(string)      {}
func (s testSink) ReasoningDelta(string) {}
func (s testSink) Restart()              {}
func (s testSink) Step(context.Context, StepOut) error {
	s.env.events.Add(1)
	return nil
}
func (s testSink) CallStarted(context.Context, CallInfo) error {
	s.env.events.Add(1)
	return nil
}
func (s testSink) CallFinished(context.Context, CallInfo, mcp.Result) error {
	s.env.events.Add(1)
	return nil
}

// testLoop is a sub-agent's loop over client, with the named built-ins offered.
func testLoop(client provider.Adapter, env *TurnEnv, tools ...string) *loop {
	rt := &Runtime{Cfg: &config.Config{}, mcp: mcp.NewManager(), subs: map[string]*Spec{}}
	rt.registerBuiltins()
	spec := &Spec{Name: "helper", Client: client, Model: "m", Builtins: map[string]bool{}}
	for _, t := range tools {
		spec.Builtins[t] = true
	}
	return newLoop(rt, spec, testSink{env}, env)
}

func newTestEnv() *TurnEnv { return &TurnEnv{Total: &usageAcc{}} }

// padArgs is a JSON object of exactly n bytes.
func padArgs(n int) string { return `{"pad":"` + strings.Repeat("a", n-len(`{"pad":""}`)) + `"}` }

func callReply(id, name, args string) *provider.Response {
	return &provider.Response{ToolCalls: []provider.ToolCall{{ID: id, Name: name, Arguments: args}}}
}

func textReply(s string) *provider.Response { return &provider.Response{Content: s} }

// toolMsg is the content the model was sent for a call ("" if none).
func toolMsg(l *loop, id string) string {
	for _, m := range l.msgs {
		if m.Role == "tool" && m.ToolCallID == id {
			return m.Content
		}
	}
	return ""
}

// storedArgs is the arguments the call's assistant message carries.
func storedArgs(l *loop, id string) string {
	for _, m := range l.msgs {
		for _, tc := range m.ToolCalls {
			if tc.ID == id {
				return tc.Arguments
			}
		}
	}
	return ""
}

func TestArgsLimitIsExact(t *testing.T) {
	c := &scripted{replies: []*provider.Response{
		callReply("edge", "hfy__now", padArgs(maxArgBytes)),
		callReply("big", "hfy__now", padArgs(maxArgBytes+1)),
		textReply("done"),
	}}
	l := testLoop(c, newTestEnv(), builtins.Now)
	l.load(context.Background(), "sys", []provider.Message{{Role: "user", Content: "go"}})
	res, err := l.run(context.Background())
	if err != nil || res.Outcome != Done {
		t.Fatalf("run: %+v %v", res, err)
	}
	if edge := toolMsg(l, "edge"); strings.Contains(edge, "too large") || !strings.Contains(edge, `"utc"`) {
		t.Fatalf("a call at the limit must run: %s", edge)
	}
	if big := toolMsg(l, "big"); big != "Error: arguments too large (limit 65536 bytes); send less" {
		t.Fatalf("over the limit: %s", big)
	}
	var marker map[string]any
	if err := json.Unmarshal([]byte(storedArgs(l, "big")), &marker); err != nil ||
		marker["truncated"] != true || marker["bytes"] != float64(maxArgBytes+1) {
		t.Fatalf("stored arguments are not the marker: %s", storedArgs(l, "big"))
	}
}

func TestPromptLimitAndCompactionFirst(t *testing.T) {
	ctx := context.Background()
	const sys = "sys"
	fit := strings.Repeat("x", maxPromptBytes-len(sys))

	c := &scripted{replies: []*provider.Response{textReply("ok")}}
	l := testLoop(c, newTestEnv())
	l.load(ctx, sys, []provider.Message{{Role: "user", Content: fit}})
	if res, err := l.run(ctx); err != nil || res.Outcome != Done || len(c.reqs) != 1 {
		t.Fatalf("a prompt at the limit is sent: %+v %v", res, err)
	}

	c = &scripted{replies: []*provider.Response{textReply("ok")}}
	l = testLoop(c, newTestEnv())
	var forced []bool
	l.prepare = func(_ context.Context, l *loop, force bool) error {
		forced = append(forced, force)
		if force {
			l.msgs = []provider.Message{{Role: "user", Content: "short"}}
		}
		return nil
	}
	l.load(ctx, sys, []provider.Message{{Role: "user", Content: fit + "x"}})
	if res, err := l.run(ctx); err != nil || res.Outcome != Done || len(c.reqs) != 1 {
		t.Fatalf("compaction must bring the prompt under the limit: %+v %v", res, err)
	}
	if len(forced) != 2 || forced[0] || !forced[1] {
		t.Fatalf("compaction calls: %v, want [false true]", forced)
	}

	c = &scripted{replies: []*provider.Response{textReply("never")}}
	l = testLoop(c, newTestEnv())
	l.load(ctx, sys, []provider.Message{{Role: "user", Content: fit + "x"}})
	res, err := l.run(ctx)
	if err != nil || res.Outcome != Failed || !strings.Contains(res.Reason, "prompt of") || len(c.reqs) != 0 {
		t.Fatalf("without compaction the turn fails before sending: %+v %v, requests %d", res, err, len(c.reqs))
	}
}

// The turn's events cap ends the loop before a call that would go past it.
func TestEventsLimitEndsTheLoop(t *testing.T) {
	env := newTestEnv()
	env.events.Store(maxTurnEvents - 4) // the step adds one, each call two
	c := &scripted{replies: []*provider.Response{
		{ToolCalls: []provider.ToolCall{
			{ID: "a", Name: "nope__a", Arguments: "{}"},
			{ID: "b", Name: "nope__b", Arguments: "{}"},
			{ID: "c", Name: "nope__c", Arguments: "{}"},
		}},
		textReply("never"),
	}}
	l := testLoop(c, env)
	l.load(context.Background(), "sys", []provider.Message{{Role: "user", Content: "go"}})
	res, err := l.run(context.Background())
	if err != nil || res.Outcome != Failed || !strings.Contains(res.Reason, "events in one turn") {
		t.Fatalf("run: %+v %v", res, err)
	}
	if toolMsg(l, "a") == "" || toolMsg(l, "b") == "" || toolMsg(l, "c") != "" {
		t.Fatalf("calls before the cap run, the one at it does not")
	}
}

// Identical unknown calls get the valid-tools hint until the abort: the guard
// counts them, and the hint stays visible.
func TestIdenticalUnknownCallsEndTheLoop(t *testing.T) {
	var replies []*provider.Response
	for i := range repeatAbort {
		replies = append(replies, callReply(string(rune('a'+i)), "nope__x", "{}"))
	}
	c := &scripted{replies: replies}
	l := testLoop(c, newTestEnv())
	l.load(context.Background(), "sys", []provider.Message{{Role: "user", Content: "go"}})
	res, err := l.run(context.Background())
	if err != nil || res.Outcome != Failed || !strings.Contains(res.Reason, "loop detected") {
		t.Fatalf("run: %+v %v", res, err)
	}
	if got := toolMsg(l, "c"); !strings.Contains(got, "Valid tools") {
		t.Fatalf("unknown tool hint lost: %s", got)
	}
}

// A NUL in the arguments would fail the event insert (jsonb refuses it), so
// the call is answered as invalid instead.
func TestNULInArgumentsIsInvalid(t *testing.T) {
	c := &scripted{replies: []*provider.Response{
		callReply("n", "hfy__now", `{"timezone":"\u0000"}`),
		textReply("done"),
	}}
	l := testLoop(c, newTestEnv(), builtins.Now)
	l.load(context.Background(), "sys", []provider.Message{{Role: "user", Content: "go"}})
	if res, err := l.run(context.Background()); err != nil || res.Outcome != Done {
		t.Fatalf("run: %+v %v", res, err)
	}
	if got := toolMsg(l, "n"); !strings.Contains(got, "invalid arguments") {
		t.Fatalf("NUL argument result: %s", got)
	}
}

func TestStorableJSON(t *testing.T) {
	for raw, want := range map[string]bool{
		`{"a":1}`:                true,
		`{"a":1e400}`:            true,
		`{"a":"x\\u0000"}`:       true, // a literal backslash, not a NUL
		`{"a":"\u0000"}`:         false,
		`"\u0000"`:               false,
		`{"\u0000":1}`:           false,
		`{"a":"` + "\xff" + `"}`: false,
		`{`:                      false,
	} {
		if got := storableJSON(raw); got != want {
			t.Errorf("storableJSON(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestArgsMarkerDescribesTheCall(t *testing.T) {
	raw := padArgs(maxArgBytes + 1)
	a, b := argsMarker(raw), argsMarker(strings.Replace(raw, "aaa", "bbb", 1))
	var m map[string]any
	if err := json.Unmarshal([]byte(a), &m); err != nil || m["truncated"] != true || m["bytes"] != float64(maxArgBytes+1) {
		t.Fatalf("marker: %s", a)
	}
	if len(a) > 1024 {
		t.Fatalf("marker is %d bytes", len(a))
	}
	if a == b {
		t.Fatal("different oversize payloads share a marker")
	}
}
