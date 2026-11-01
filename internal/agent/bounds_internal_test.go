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

// Arguments that fit 64 KiB as sent but not once their numbers are written out
// are refused as too large, and the log keeps the marker.
func TestArgumentsTooLargeOnceWrittenOut(t *testing.T) {
	raw := `{"text":"x","n":[` + strings.Repeat("1e100,", 9999) + `1e100]}`
	if len(raw) > maxArgBytes {
		t.Fatalf("fixture is %d bytes, over the raw limit", len(raw))
	}
	c := &scripted{replies: []*provider.Response{callReply("w", "hfy__now", raw), textReply("done")}}
	env := newTestEnv()
	l := testLoop(c, env, builtins.Now)
	rec := &recSink{testSink: testSink{env}}
	l.sink = rec
	l.load(context.Background(), "sys", []provider.Message{{Role: "user", Content: "go"}})
	if res, err := l.run(context.Background()); err != nil || res.Outcome != Done {
		t.Fatalf("run: %+v %v", res, err)
	}
	if got := toolMsg(l, "w"); got != "Error: arguments too large (limit 65536 bytes); send less" {
		t.Fatalf("result: %s", got)
	}
	var marker map[string]any
	if len(rec.calls) != 1 || json.Unmarshal(rec.calls[0].Args, &marker) != nil || marker["truncated"] != true {
		t.Fatalf("stored arguments are not the marker: %+v", rec.calls)
	}
}

// Each refusal is the one the model is told about: text Postgres cannot hold,
// numbers it would expand or refuse, and JSON that does not parse.
func TestStorable(t *testing.T) {
	deep := func(n string) string { return `{"a":{"b":[1,{"c":` + n + `}]}}` }
	ones := func(n int) string { return "1" + strings.Repeat("0", n-1) }
	for raw, want := range map[string]error{
		`{"a":1}`:                  nil,
		`{"a":"x\\u0000"}`:         nil, // a literal backslash, not a NUL
		`{"a":"\u0000"}`:           errNotText,
		`"\u0000"`:                 errNotText,
		`{"\u0000":1}`:             errNotText,
		`{"a":"` + "\xff" + `"}`:   errNotText,
		`{"a":"\ud800"}`:           errNotText,
		`{"a":"\udc00"}`:           errNotText,
		`{"a":"\ud800\u0041"}`:     errNotText,
		`{"a":"\ud83d\ude00"}`:     nil, // a valid pair
		`{"a":"\\ud800"}`:          nil, // an escaped backslash, then text
		`{`:                        errNotJSON,
		`{"x":1e131072}`:           errNumber,
		`{"x":1e100000}`:           errNumber,
		`{"x":1e-16384}`:           errNumber,
		`{"x":-1e100000}`:          errNumber,
		`[[1e100000]]`:             errNumber,
		deep("1e100000"):           errNumber,
		`{"x":1E+5}`:               nil, // a small exponent is plain text
		`{"x":1.5e-3}`:             nil,
		`{"x":1e400}`:              nil,
		`{"x":1e999}`:              nil, // 1000 digits written out
		`{"x":1e1000}`:             errNumber,
		`{"x":1e-999}`:             nil,
		`{"x":1e-1000}`:            errNumber,
		`{"x":` + ones(1000) + `}`: nil,
		`{"x":` + ones(1001) + `}`: errNumber,
		`{"x":0.` + strings.Repeat("0", 999) + `1}`: errNumber, // 1000 after the point and 1 before
		`{"x":1e99999999999999999999}`:              errNumber, // an exponent that overflows
	} {
		if got := storable(raw, maxArgBytes); !errors.Is(got, want) {
			t.Errorf("storable(%q) = %v, want %v", raw, got, want)
		}
	}
}

// Numbers count at their written-out length against the same bound as the raw
// text: 1e100 is 5 bytes and 101 written out, so ten of them grow by 960.
func TestStorableCountsWrittenOutNumbers(t *testing.T) {
	raw := "[" + strings.Repeat("1e100,", 9) + "1e100]"
	if len(raw) != 61 {
		t.Fatalf("fixture is %d bytes", len(raw))
	}
	if err := storable(raw, 1020); !errors.Is(err, errTooLarge) {
		t.Errorf("1021 bytes written out must not fit 1020: %v", err)
	}
	if err := storable(raw, 1021); err != nil {
		t.Errorf("1021 bytes written out fit 1021: %v", err)
	}
	if err := storable(strings.Repeat(" ", maxArgBytes)+"1", maxArgBytes); !errors.Is(err, errTooLarge) {
		t.Errorf("raw size over the limit: %v", err)
	}
	// Raw text over the limit is refused even when its numbers shrink once written out.
	shrink := "[" + strings.Repeat("1e0,", 40) + "1]"
	if err := storable(shrink, 100); !errors.Is(err, errTooLarge) {
		t.Errorf("raw over the limit with shrinking numbers: %v", err)
	}
}

func TestStoredNamesAndIDs(t *testing.T) {
	long := strings.Repeat("n", maxName)
	for s, want := range map[string]bool{
		"files__echo": true,
		long:          true,
		long + "n":    false,
		"a\x00b":      false,
		"\xff":        false,
	} {
		if got := storableText(s); got != want {
			t.Errorf("storableText(%q) = %v, want %v", s, got, want)
		}
	}
	for in, want := range map[string]string{
		"nope\x00x":              "nopex",
		"":                       "(unnamed)",
		"\x00":                   "(unnamed)",
		strings.Repeat("é", 200): strings.Repeat("é", 128),
	} {
		if got := clipName(in); got != want {
			t.Errorf("clipName(%q) = %q, want %q", in, got, want)
		}
	}
}

// Recording sink for the loop's persisted calls and results.
type recSink struct {
	testSink
	calls   []CallInfo
	results []mcp.Result
}

func (s *recSink) CallStarted(ctx context.Context, c CallInfo) error {
	s.calls = append(s.calls, c)
	return s.testSink.CallStarted(ctx, c)
}

func (s *recSink) CallFinished(ctx context.Context, c CallInfo, r mcp.Result) error {
	s.results = append(s.results, r)
	return s.testSink.CallFinished(ctx, c, r)
}

// A tool name the log cannot hold is an unknown tool, stored cleaned and cut;
// an id it cannot hold gets the generated one. Either way the turn goes on.
func TestUnstorableNamesAndIDsAreAnswered(t *testing.T) {
	long := strings.Repeat("n", 300)
	for _, c := range []struct {
		name, id, wantName, wantID string
		unknown                    bool
	}{
		{name: "nope\x00x", id: "c", wantName: "nopex", wantID: "c", unknown: true},
		{name: long, id: "c", wantName: long[:maxName], wantID: "c", unknown: true},
		{name: "", id: "c", wantName: "(unnamed)", wantID: "c", unknown: true},
		{name: "hfy__now", id: "bad\x00id", wantName: "hfy.now", wantID: "call_1_0"},
		{name: "hfy__now", id: long, wantName: "hfy.now", wantID: "call_1_0"},
	} {
		t.Run(c.wantName+"/"+c.wantID, func(t *testing.T) {
			client := &scripted{replies: []*provider.Response{callReply(c.id, c.name, "{}"), textReply("done")}}
			env := newTestEnv()
			l := testLoop(client, env, builtins.Now)
			rec := &recSink{testSink: testSink{env}}
			l.sink = rec
			l.load(context.Background(), "sys", []provider.Message{{Role: "user", Content: "go"}})
			if res, err := l.run(context.Background()); err != nil || res.Outcome != Done {
				t.Fatalf("run: %+v %v", res, err)
			}
			if len(rec.calls) != 1 || rec.calls[0].Name != c.wantName || rec.calls[0].ID != c.wantID {
				t.Fatalf("recorded call: %+v", rec.calls)
			}
			if c.unknown && (!rec.results[0].IsError || !strings.Contains(rec.results[0].Content, "unknown tool")) {
				t.Fatalf("unknown name result: %+v", rec.results[0])
			}
		})
	}
}

// A name the log cannot hold is an unknown tool even when the lookup has an
// entry under it: the entry must not run.
func TestUnstorableNameIsUnknownEvenInLookup(t *testing.T) {
	for _, name := range []string{"hfy.now\x00", strings.Repeat("n", 300)} {
		env := newTestEnv()
		l := testLoop(&scripted{}, env, builtins.Now)
		rec := &recSink{testSink: testSink{env}}
		l.sink = rec
		now, ok := l.rt.findBuiltin("hfy.now")
		if !ok {
			t.Fatal("no hfy.now built-in")
		}
		out, err := l.handleCall(context.Background(), provider.ToolCall{ID: "c", Name: name, Arguments: "{}"},
			false, map[string]mcp.Tool{name: now.tool})
		if err != nil || out.abort != "" || out.paused {
			t.Fatalf("handleCall: %+v %v", out, err)
		}
		if len(rec.results) != 1 || !rec.results[0].IsError || !strings.Contains(rec.results[0].Content, "unknown tool") {
			t.Fatalf("result for %q: %+v", name[:min(len(name), 12)], rec.results)
		}
	}
}

// probeLoop is a loop whose one built-in, a card-server tool, returns out.
func probeLoop(out mcp.Result) (*loop, *recSink) {
	client := &scripted{replies: []*provider.Response{callReply("p", "probe__card", "{}"), textReply("done")}}
	env := newTestEnv()
	l := testLoop(client, env, "card")
	l.spec.Top = true
	l.spec.Builtins[builtins.AskUser] = true
	l.rt.builtins = append(l.rt.builtins, builtin{tool: mcp.Tool{Server: "probe", Name: "card"},
		run: func(context.Context, *loop, json.RawMessage) (mcp.Result, error) { return out, nil }})
	l.rt.Cfg.MCP = map[string]config.MCPServer{"probe": {UI: &config.UI{ImageHosts: []string{"imgs.example"}}}}
	rec := &recSink{testSink: testSink{env}}
	l.sink = rec
	l.load(context.Background(), "sys", []provider.Message{{Role: "user", Content: "go"}})
	return l, rec
}

// Output the event log cannot hold never reaches it: structured content, the
// card and its questions are dropped one by one, and the result text is cleaned.
func TestToolOutputThatCannotBeStoredIsDropped(t *testing.T) {
	card := func(extra string) json.RawMessage {
		return json.RawMessage(`{"v":1,"type":"card","title":"T"` + extra + `}`)
	}
	for name, c := range map[string]struct {
		out                    mcp.Result
		wantStructured, wantUI bool
	}{
		"structured with a NUL":       {out: mcp.Result{Content: "a\x00b", Structured: json.RawMessage(`{"a":"x\u0000"}`)}},
		"structured with an exponent": {out: mcp.Result{Content: "a\x00b", Structured: json.RawMessage(`{"n":1e100000}`)}},
		"structured that stores":      {out: mcp.Result{Content: "a\x00b", Structured: json.RawMessage(`{"n":1.5e3}`)}, wantStructured: true},
		"ui payload with a NUL":       {out: mcp.Result{Content: "a\x00b", UI: card(`,"buttons":[{"host":{"label":"Go","name":"svc","payload":{"x":"a\u0000"}}}]`)}},
		"ui payload with an exponent": {out: mcp.Result{Content: "a\x00b", UI: card(`,"buttons":[{"host":{"label":"Go","name":"svc","payload":{"x":1e100000}}}]`)}},
		"questions with a NUL":        {out: mcp.Result{Content: "a\x00b", UI: card(`,"questions":{"questions":[{"id":"a","text":"b\u0000c","type":"text"}]}`)}, wantUI: true},
		"a card that stores":          {out: mcp.Result{Content: "a\x00b", UI: card("")}, wantUI: true},
	} {
		t.Run(name, func(t *testing.T) {
			l, rec := probeLoop(c.out)
			if res, err := l.run(context.Background()); err != nil || res.Outcome != Done {
				t.Fatalf("run: %+v %v", res, err)
			}
			if len(rec.results) != 1 {
				t.Fatalf("results: %d", len(rec.results))
			}
			got := rec.results[0]
			if got.Content != "ab" {
				t.Errorf("content %q, want the cleaned text", got.Content)
			}
			if (got.Structured != nil) != c.wantStructured || (got.UI != nil) != c.wantUI || got.Questions != nil {
				t.Errorf("structured %q ui %q questions %q", got.Structured, got.UI, got.Questions)
			}
		})
	}
}

// A provider error can carry raw response bytes, and the failure reason is stored.
type failing struct{ scripted }

func (failing) Chat(context.Context, provider.Request, provider.Opts) (*provider.Response, error) {
	return nil, errors.New("provider error in stream: x\x00y")
}

func TestModelErrorReasonIsStoredClean(t *testing.T) {
	l := testLoop(&failing{}, newTestEnv())
	l.load(context.Background(), "sys", []provider.Message{{Role: "user", Content: "go"}})
	res, err := l.run(context.Background())
	if err != nil || res.Outcome != Failed || res.Reason != "model call failed: provider error in stream: xy" {
		t.Fatalf("run: %+v %v", res, err)
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
