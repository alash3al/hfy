package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/mcp"
	"github.com/alash3al/hfy/internal/mcp/builtins"
	"github.com/alash3al/hfy/internal/provider"
	"github.com/alash3al/hfy/internal/svc"
	"github.com/alash3al/hfy/internal/types"
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
		callReply("edge", "hfy__now", padArgs(maxArgBytes-1)), // reads back at exactly the limit: one ":" gains a space
		callReply("wide", "hfy__now", padArgs(maxArgBytes)),   // fits raw, reads back one byte over
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
	if wide := toolMsg(l, "wide"); wide != "Error: arguments too large (limit 65536 bytes); send less" {
		t.Fatalf("raw at the limit reads back over it: %s", wide)
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
// text: 1e100 is 5 bytes and 101 written out, so ten of them grow by 960, and
// the nine commas between them grow by a space each on read.
func TestStorableCountsWrittenOutNumbers(t *testing.T) {
	raw := "[" + strings.Repeat("1e100,", 9) + "1e100]"
	if len(raw) != 61 {
		t.Fatalf("fixture is %d bytes", len(raw))
	}
	if err := storable(raw, 1029); !errors.Is(err, errTooLarge) {
		t.Errorf("1030 bytes read back must not fit 1029: %v", err)
	}
	if err := storable(raw, 1030); err != nil {
		t.Errorf("1030 bytes read back fit 1030: %v", err)
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
type failing struct{}

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

// An oversize prompt names what compaction did: it was unavailable, or it ran
// and was not enough.
func TestOversizePromptSaysWhatCompactionDid(t *testing.T) {
	ctx := context.Background()
	huge := strings.Repeat("x", maxPromptBytes+1)
	for name, c := range map[string]struct {
		prepare func(context.Context, *loop, bool) error
		want    string
	}{
		"no summarizer":   {nil, "compaction unavailable"},
		"nothing to fold": {func(context.Context, *loop, bool) error { return nil }, "compaction unavailable"},
		"ran, not enough": {func(_ context.Context, l *loop, force bool) error {
			if force {
				l.compactions++
			}
			return nil
		}, "after compaction"},
	} {
		t.Run(name, func(t *testing.T) {
			l := testLoop(&scripted{}, newTestEnv())
			l.prepare = c.prepare
			l.load(ctx, "sys", []provider.Message{{Role: "user", Content: huge}})
			res, err := l.run(ctx)
			if err != nil || res.Outcome != Failed || !strings.Contains(res.Reason, c.want) {
				t.Fatalf("run: %+v %v, want %q", res, err, c.want)
			}
		})
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

// Every token is checked, so a value under a repeated key is too, and nesting
// is bounded at maxDepth however it is written.
func TestStorableChecksEveryTokenAndDepth(t *testing.T) {
	for raw, want := range map[string]error{
		`{"a":"\u0000","a":1}`:                   errNotText,
		`{"a":1,"a":"\u0000"}`:                   errNotText,
		`{"a":{"b":"\u0000"},"a":[]}`:            errNotText,
		`{"a":"\ud800","a":1}`:                   errNotText,
		`{"a":1e999999,"a":1}`:                   errNumber,
		`{"a":1e100000,"a":1}`:                   errNumber,
		`{"a":[1e100000],"a":1}`:                 errNumber,
		`{"\u0000":1,"b":2}`:                     errNotText,
		`{"a":1,"\u0000":2}`:                     errNotText,
		`{"\ud800":1}`:                           errNotText,
		"\ufeff{}":                               errNotJSON,
		"{\"a\":\"\xff\"}":                       errNotText,
		`{"a":1,"a":2}`:                          nil,
		nested(maxDepth-1, false):                nil,
		nested(maxDepth, false):                  nil,
		nested(maxDepth+1, false):                errDepth,
		nested(maxDepth-1, true):                 nil,
		nested(maxDepth, true):                   nil,
		nested(maxDepth+1, true):                 errDepth,
		nested(10000, false):                     errDepth, // valid JSON, and what Go reads
		nested(10001, false):                     errNotJSON,
		`[` + nested(maxDepth, false) + `]`:      errDepth,
		`[` + strings.Repeat(`[],`, 100) + `[]]`: nil, // wide is not deep
	} {
		if got := storable(raw, maxArgBytes); !errors.Is(got, want) {
			t.Errorf("storable(%.50q) = %v, want %v", raw, got, want)
		}
	}
}

// The size counted is what Postgres reads back: a space after every "," and
// ":" outside strings, none for those inside, and each number written out.
func TestStorableCountsSeparatorsAsReadBack(t *testing.T) {
	for _, c := range []struct {
		raw  string
		back int // Postgres' read-back length
	}{
		{`[1,1,1]`, len(`[1, 1, 1]`)},
		{`{"a":1,"b":2}`, len(`{"a": 1, "b": 2}`)},
		{`["a,b:c",":",","]`, len(`["a,b:c", ":", ","]`)},
		{`["\",:"]`, len(`["\",:"]`)}, // an escaped quote does not end the string
		{`["\\",1]`, len(`["\\", 1]`)},
		{`{"a":[1e10,2]}`, len(`{"a": [10000000000, 2]}`)},
	} {
		if err := storable(c.raw, c.back); err != nil {
			t.Errorf("storable(%s, %d) = %v: it reads back as %d bytes", c.raw, c.back, err, c.back)
		}
		if err := storable(c.raw, c.back-1); !errors.Is(err, errTooLarge) {
			t.Errorf("storable(%s, %d) = %v, want too large", c.raw, c.back-1, err)
		}
	}
}

// Empty, null and blank arguments are {} before any check; ordinary values
// are kept byte for byte.
func TestNormalizeArgsKeepsCommonValues(t *testing.T) {
	for _, raw := range []string{"", "  \n\t ", "null", " null "} {
		if got, err := normalizeArgs(raw); err != nil || string(got) != "{}" {
			t.Errorf("normalizeArgs(%q) = %s, %v, want {}", raw, got, err)
		}
	}
	for _, raw := range []string{
		`{"pi":3.14159,"e":1e10,"big":12345678901234567890,"neg":-0.5E-3}`,
		`{"max":1.7976931348623157e308,"min":-1.7976931348623157e308,"tiny":5e-324}`,
		`{"text":"héllo 日本語 \ud83d\ude00 \u00e9"}`,
		`{"a":[1,2,{"b":null}],"c":{}}`,
		nested(maxDepth, true),
	} {
		if got, err := normalizeArgs(raw); err != nil || string(got) != raw {
			t.Errorf("normalizeArgs(%.50q) = %.50s, %v, want it unchanged", raw, got, err)
		}
	}
	// A refusal keeps what the model said as a JSON string, within the replay bound.
	got, err := normalizeArgs(`{"a":"\u0000","a":1}`)
	if !errors.Is(err, errNotText) || string(got) != `"{\"a\":\"\\u0000\",\"a\":1}"` {
		t.Errorf("duplicate key refusal: %s, %v", got, err)
	}
	if got, err = normalizeArgs(nested(maxDepth+1, false)); !errors.Is(err, errDepth) || !json.Valid(got) {
		t.Errorf("too deep: %s, %v", got, err)
	}
}

// What a tool server sends back is checked the same way: structured content
// with a repeated key or too deep is dropped, not stored.
func TestKeepStoredChecksStructuredContent(t *testing.T) {
	for raw, kept := range map[string]bool{
		`{"a":"\u0000","a":1}`:   false,
		`{"a":1e999999,"a":1}`:   false,
		nested(maxDepth+1, true): false,
		nested(maxDepth, true):   true,
		`{"a":1,"a":2}`:          true,
		`{"rows":[{"id":1}]}`:    true,
	} {
		if got := keepStored(json.RawMessage(raw), maxStructured, "structured result", "files.x") != nil; got != kept {
			t.Errorf("keepStored(%.50q) kept = %v, want %v", raw, got, kept)
		}
	}
}

// msgIDs are the call ids of the assistant messages, and of the tool messages.
func msgIDs(l *loop) (calls, results []string) {
	for _, m := range l.msgs {
		for _, tc := range m.ToolCalls {
			calls = append(calls, tc.ID)
		}
		if m.Role == "tool" {
			results = append(results, m.ToolCallID)
		}
	}
	return calls, results
}

// A turn finds its calls by id alone, so an id is used once: a repeat (in one
// message or across steps), an empty or unstorable id, and a model that sends
// the id the loop would generate all end up with distinct ids, and every
// call gets exactly one result.
func TestCallIDsAreUniqueWithinALoop(t *testing.T) {
	n := 0 // distinct arguments, so the repeat guard stays out of it
	now := func(id string) provider.ToolCall {
		n++
		return provider.ToolCall{ID: id, Name: "hfy__now", Arguments: `{"n":` + strconv.Itoa(n) + `}`}
	}
	c := &scripted{replies: []*provider.Response{
		{ToolCalls: []provider.ToolCall{now("a"), now("a"), now("\x00"), now("call_2_0")}},
		{ToolCalls: []provider.ToolCall{now("a"), now("call_2_0"), now("")}},
		textReply("done"),
	}}
	l := testLoop(c, newTestEnv(), builtins.Now)
	l.load(context.Background(), "sys", []provider.Message{{Role: "user", Content: "go"}})
	if res, err := l.run(context.Background()); err != nil || res.Outcome != Done {
		t.Fatalf("run: %+v %v", res, err)
	}
	calls, results := msgIDs(l)
	seen := map[string]bool{}
	for _, id := range calls {
		if seen[id] {
			t.Fatalf("id %q used twice: %v", id, calls)
		}
		seen[id] = true
	}
	// step 1: the repeat and the NUL id are renamed, a model's own call_2_0 is kept;
	// step 2: the repeats are renamed, and the generated name that is taken gets a suffix
	if got := strings.Join(calls, ","); got != "a,call_1_1,call_1_2,call_2_0,call_2_0_1,call_2_1,call_2_2" {
		t.Fatalf("call ids: %s", got)
	}
	if strings.Join(results, ",") != strings.Join(calls, ",") {
		t.Fatalf("results %v do not answer the calls %v one by one", results, calls)
	}
}

// A resumed turn starts from the ids it already recorded.
func TestSeedCountsMarksRecordedIDsUsed(t *testing.T) {
	l := &loop{counts: map[string]int{}, used: map[string]bool{}}
	l.seedCounts(svc.TurnView{Calls: []types.ToolCall{
		{Name: "hfy.now", CallID: "x", Arguments: []byte(`{}`)},
		{Name: svc.AskUserTool, CallID: "q", Arguments: []byte(`{}`)},
	}, Decided: map[string]types.ApprovalDecided{"q": {CallID: "q", Approve: true}}})
	if !l.used["x"] || !l.used["q"] || len(l.used) != 2 {
		t.Fatalf("used: %v", l.used)
	}
}

// Rows written before the bounds, or by another version, may hold arguments
// far over maxArgBytes: replaying them sends a marker, never the megabytes. A
// row this version could have stored is replayed whole.
func TestReplayClampsOversizeArguments(t *testing.T) {
	huge := `{"pad":"` + strings.Repeat("a", 3<<20) + `"}`
	fits := padArgs(maxReplayArgs)                      // the most a stored row can be: 2x what storable lets in
	garbage, _ := json.Marshal(strings.Repeat("<", 40)) // invalid JSON the model sent, kept as a string
	call := func(id, args string) db.EventRow {
		data, _ := json.Marshal(types.ToolCall{Name: "hfy.now", CallID: id, Arguments: json.RawMessage(args)})
		return db.EventRow{Type: types.EventToolCall, V: 1, Data: data, ActorType: string(types.ActorAgent), ActorRef: "helper"}
	}
	rt := &Runtime{}
	msgs, err := rt.messagesFrom(context.Background(), []db.EventRow{
		call("huge", huge), call("fits", fits), call("text", string(garbage))}, 0, &Spec{Name: "helper"}, false)
	if err != nil || len(msgs) == 0 {
		t.Fatalf("messages: %v %v", msgs, err)
	}
	got := map[string]string{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			got[tc.ID] = tc.Arguments
		}
	}
	var marker map[string]any
	if json.Unmarshal([]byte(got["huge"]), &marker) != nil || marker["truncated"] != true ||
		marker["bytes"] != float64(len(huge)) || len(got["huge"]) > 1024 {
		t.Fatalf("oversize row replayed as %.200s (%d bytes)", got["huge"], len(got["huge"]))
	}
	if got["fits"] != fits || got["text"] != strings.Repeat("<", 40) {
		t.Fatalf("a row within the bound changed: %.80s / %q", got["fits"], got["text"])
	}
}

// What a refused text is stored as, a JSON string, stays within the replay
// bound even when escaping inflates it six-fold.
func TestRefusedTextStoredWithinTheReplayBound(t *testing.T) {
	raw := `{"a":` + strings.Repeat("\u0001", maxArgBytes-8) // not JSON; each control byte escapes to 6
	got, err := normalizeArgs(raw)
	if err == nil || len(got) > maxReplayArgs || !json.Valid(got) {
		t.Fatalf("stored %d bytes, %v", len(got), err)
	}
	var marker map[string]any
	if json.Unmarshal(got, &marker) != nil || marker["truncated"] != true {
		t.Fatalf("inflated text must be a marker: %.100s", got)
	}
	if text := argsText(got); text != string(got) { // a marker replays as it is
		t.Fatalf("replayed as %.100s", text)
	}
	small, err := normalizeArgs(`{"a":`)
	if err == nil || string(small) != `"{\"a\":"` {
		t.Fatalf("a short refused text: %s %v", small, err)
	}
}

// Calls that never got a result are answered in the order they were made, so
// the same events always give the same prompt (provider caching depends on it).
func TestDanglingCallsKeepTheirOrder(t *testing.T) {
	var events []db.EventRow
	for i := range 8 {
		data, _ := json.Marshal(types.ToolCall{Name: "hfy.now", CallID: "call-" + strconv.Itoa(i), Arguments: json.RawMessage(`{}`)})
		events = append(events, db.EventRow{Type: types.EventToolCall, V: 1, Data: data,
			ActorType: string(types.ActorAgent), ActorRef: "helper"})
	}
	build := func() string {
		msgs, err := (&Runtime{}).messagesFrom(context.Background(), events, 0, &Spec{Name: "helper"}, false)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(msgs)
		return string(b)
	}
	want := build()
	for range 100 {
		if got := build(); got != want {
			t.Fatalf("prompt changed between builds:\n%s\n%s", want, got)
		}
	}
	if !strings.Contains(want, `"call-0"`) || strings.Index(want, `"ToolCallID":"call-0"`) > strings.Index(want, `"ToolCallID":"call-7"`) {
		t.Fatalf("not in call order: %s", want)
	}
}

func callEvent(id string) db.EventRow {
	data, _ := json.Marshal(types.ToolCall{Name: "hfy.now", CallID: id, Arguments: json.RawMessage(`{}`)})
	return db.EventRow{Type: types.EventToolCall, V: 1, Data: data, ActorType: string(types.ActorAgent), ActorRef: "helper"}
}

func resultEvent(id string) db.EventRow {
	data, _ := json.Marshal(types.ToolResult{CallID: id, Content: "ok"})
	return db.EventRow{Type: types.EventToolResult, V: 1, Data: data, ActorType: string(types.ActorAgent), ActorRef: "helper"}
}

func userEvent(text string) db.EventRow {
	data, _ := json.Marshal(types.MessageText{Text: text})
	return db.EventRow{Type: types.EventMessageText, V: 1, Data: data, ActorType: string(types.ActorExternal), ActorRef: "u1"}
}

// shape lists messages as "role" or "role:id" (a tool answer) for ordering checks.
func shape(msgs []provider.Message) string {
	var parts []string
	for _, m := range msgs {
		switch m.Role {
		case "tool":
			parts = append(parts, "tool:"+m.ToolCallID)
		case "user":
			parts = append(parts, "user:"+m.Content)
		default:
			ids := ""
			for _, tc := range m.ToolCalls {
				ids += tc.ID
			}
			parts = append(parts, "assistant:"+ids)
		}
	}
	return strings.Join(parts, " ")
}

// A call id reused after it was answered is answered at its last use: the
// dangling answers follow the order of the calls they belong to.
func TestReusedCallIDIsAnsweredAtItsLastUse(t *testing.T) {
	events := []db.EventRow{callEvent("x"), resultEvent("x"), callEvent("y"), callEvent("x")}
	var want string
	for range 50 {
		msgs, err := (&Runtime{}).messagesFrom(context.Background(), events, 0, &Spec{Name: "helper"}, false)
		if err != nil {
			t.Fatal(err)
		}
		got := shape(msgs)
		if want == "" {
			want = got
		}
		if got != want || got != "assistant:x tool:x assistant:yx tool:y tool:x" {
			t.Fatalf("got %q", got)
		}
	}
}

// A call left unanswered in an earlier turn is answered right after the
// assistant message that made it, never after the user messages that follow:
// providers reject a tool answer that is not next to its call.
func TestDanglingCallOfAnEarlierTurnIsAnsweredInPlace(t *testing.T) {
	events := []db.EventRow{userEvent("one"), callEvent("a"), callEvent("b"), resultEvent("a"),
		userEvent("two"), callEvent("c"), userEvent("three")}
	msgs, err := (&Runtime{}).messagesFrom(context.Background(), events, 0, &Spec{Name: "helper"}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := "user:one assistant:ab tool:a tool:b user:two assistant:c tool:c user:three"
	if got := shape(msgs); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}
