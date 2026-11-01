package agent_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/provider/fakeprov"
	"github.com/alash3al/hfy/internal/types"
)

// waitCalls waits until the turn has recorded n tool calls and returns their ids.
func (e *env) waitCalls(turn int64, n int) []string {
	e.t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		var ids []string
		for _, c := range e.find(turn, types.EventToolCall) {
			ids = append(ids, c["call_id"].(string))
		}
		if len(ids) == n {
			return ids
		}
	}
	e.t.Fatalf("turn did not record %d calls\n%s", n, e.dump(turn))
	return nil
}

// A model that reuses an id (every step's first call is call_0 for a provider
// that sends none) gets its calls told apart: each approval is its own, shows
// its own call, and resuming between them keeps them apart.
func TestRepeatedCallIDsEachGetTheirOwnApproval(t *testing.T) {
	write := func(file string) fakeprov.Step {
		return fakeprov.Step{Calls: []fakeprov.Call{{ID: "call_0", Name: "files__write_file", Args: `{"text":"` + file + `"}`}}}
	}
	e := setup(t, func(c *config.Config) {
		c.Main().MCP = []string{"files"}
		c.Main().Approval = &config.Approval{Tools: []string{"files.write_*"}, Timeout: config.Duration(time.Minute)}
	}, write("a.txt"), write("b.txt"), write("c.txt"), fakeprov.Step{Content: "all written"})
	turn := e.post("write them")
	ids := e.waitCalls(turn, 1)
	for i, file := range []string{"a.txt", "b.txt", "c.txt"} {
		if i > 0 {
			ids = e.waitCalls(turn, i+1)
		}
		id := ids[i]
		if i == 0 && id != "call_0" || i > 0 && strings.Contains(strings.Join(ids[:i], ","), id) {
			t.Fatalf("call %d reuses an id: %v", i, ids)
		}
		// The approval is of this call: the request names its id, and the call stored under it is this one.
		var req []map[string]any
		for deadline := time.Now().Add(10 * time.Second); len(req) <= i && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			req = e.find(turn, types.EventApprovalRequested)
		}
		if len(req) != i+1 || req[i]["call_id"] != id {
			t.Fatalf("approval %d: %v for ids %v", i, req, ids)
		}
		args, _ := json.Marshal(e.find(turn, types.EventToolCall)[i]["arguments"])
		if string(args) != `{"text":"`+file+`"}` {
			t.Fatalf("call %d stored as %s", i, args)
		}
		if err := e.rt.Svc.Approve(e.ctx, e.conv.ID, e.user, types.ApprovalDecided{CallID: id, Approve: true}); err != nil {
			t.Fatalf("approve %s: %v", id, err)
		}
	}
	e.waitState(turn, types.StateCompleted)
	r := e.find(turn, types.EventToolResult)
	if len(r) != 3 {
		t.Fatalf("results: %v\n%s", r, e.dump(turn))
	}
	for i, file := range []string{"a.txt", "b.txt", "c.txt"} {
		if r[i]["call_id"] != ids[i] || r[i]["content"] != "wrote "+file {
			t.Fatalf("result %d: %v, want %s for %s", i, r[i], "wrote "+file, ids[i])
		}
	}
	// The last prompt pairs every call with its own result, once.
	body, _ := json.Marshal(e.fake.Reqs()[3]["messages"])
	for _, id := range ids {
		if n := strings.Count(string(body), `"tool_call_id":"`+id+`"`); n != 1 {
			t.Errorf("the model was sent %d results for %s: %s", n, id, body)
		}
	}
}

// Two calls with one id in one message both run, and the model gets two results
// under two ids.
func TestSameIDTwiceInOneMessage(t *testing.T) {
	e := setup(t, func(c *config.Config) { c.Main().MCP = []string{"files"} },
		fakeprov.Step{Calls: []fakeprov.Call{
			{ID: "dup", Name: "files__echo", Args: `{"text":"one"}`},
			{ID: "dup", Name: "files__echo", Args: `{"text":"two"}`}}},
		fakeprov.Step{Content: "done"})
	turn := e.post("go")
	e.waitState(turn, types.StateCompleted)
	r := e.find(turn, types.EventToolResult)
	if len(r) != 2 || r[0]["call_id"] == r[1]["call_id"] || r[0]["content"] != "echo: one" || r[1]["content"] != "echo: two" {
		t.Fatalf("results: %v\n%s", r, e.dump(turn))
	}
	body, _ := json.Marshal(e.fake.Reqs()[1]["messages"])
	for _, c := range r {
		if n := strings.Count(string(body), `"tool_call_id":"`+c["call_id"].(string)+`"`); n != 1 {
			t.Errorf("results sent for %v: %d: %s", c["call_id"], n, body)
		}
	}
}

// An ask that reuses an earlier ask's id pauses on its own: it is not taken for
// the answered one.
func TestAskUserWithARepeatedID(t *testing.T) {
	e := setup(t, func(c *config.Config) {
		c.Main().AskUser = true
		c.Main().Approval = &config.Approval{Timeout: config.Duration(time.Minute)}
	}, askCall("q"), askCall("q"), fakeprov.Step{Content: "thanks"})
	turn := e.post("order a shirt")
	ids := e.waitCalls(turn, 1)
	answer := func(id string) {
		t.Helper()
		if err := e.rt.Svc.Approve(e.ctx, e.conv.ID, e.user, types.ApprovalDecided{CallID: id, Approve: true,
			Answers: json.RawMessage(`{"size":"m","extras":["wrap"]}`)}); err != nil {
			t.Fatalf("answer %s: %v", id, err)
		}
	}
	answer(ids[0])
	ids = e.waitCalls(turn, 2)
	if ids[0] == ids[1] {
		t.Fatalf("the second ask reuses %q", ids[0])
	}
	var asked []map[string]any
	for deadline := time.Now().Add(10 * time.Second); len(asked) < 2 && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		asked = e.find(turn, types.EventApprovalRequested)
	}
	if len(asked) != 2 || asked[1]["call_id"] != ids[1] {
		t.Fatalf("approval requests: %v", asked)
	}
	answer(ids[1])
	e.waitState(turn, types.StateCompleted)
	if r := e.find(turn, types.EventToolResult); len(r) != 2 || r[0]["call_id"] != ids[0] || r[1]["call_id"] != ids[1] {
		t.Fatalf("results: %v\n%s", r, e.dump(turn))
	}
}

// reqIDs lists the call ids a request carried: those the assistant made, then
// those its tool results answer.
func reqIDs(req map[string]any) (calls, results []string) {
	for _, m := range req["messages"].([]any) {
		m := m.(map[string]any)
		tcs, _ := m["tool_calls"].([]any)
		for _, tc := range tcs {
			calls = append(calls, tc.(map[string]any)["id"].(string))
		}
		if id, ok := m["tool_call_id"].(string); ok {
			results = append(results, id)
		}
	}
	return calls, results
}

func allUnique(t *testing.T, what string, ids []string) {
	t.Helper()
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("%s reuses %q: %v", what, id, ids)
		}
		seen[id] = true
	}
}

// A model that repeats an id from an earlier turn (alone, or twice in one
// message beside a fresh one) gets new ids that are unique across the whole
// conversation, and the ids stored, sent live and replayed next turn agree.
func TestCallIDsAreUniqueAcrossTurns(t *testing.T) {
	echo := func(id, text string) fakeprov.Call {
		return fakeprov.Call{ID: id, Name: "files__echo", Args: `{"text":"` + text + `"}`}
	}
	e := setup(t, func(c *config.Config) { c.Main().MCP = []string{"files"} },
		fakeprov.Step{Calls: []fakeprov.Call{echo("dup", "t1")}}, fakeprov.Step{Content: "one"},
		fakeprov.Step{Calls: []fakeprov.Call{echo("dup", "t2")}}, fakeprov.Step{Content: "two"},
		fakeprov.Step{Calls: []fakeprov.Call{echo("dup", "t3a"), echo("dup", "t3b"), echo("fresh", "t3c")}}, fakeprov.Step{Content: "three"},
		fakeprov.Step{Content: "four"})
	var turns []int64
	for _, text := range []string{"one", "two", "three", "four"} {
		turn := e.post(text)
		e.waitState(turn, types.StateCompleted)
		turns = append(turns, turn)
	}
	var stored, storedResults []string
	for _, turn := range turns {
		for _, c := range e.find(turn, types.EventToolCall) {
			stored = append(stored, c["call_id"].(string))
		}
		for _, r := range e.find(turn, types.EventToolResult) {
			storedResults = append(storedResults, r["call_id"].(string))
		}
	}
	allUnique(t, "stored calls", stored)
	if len(stored) != 5 || stored[0] != "dup" || stored[4] != "fresh" || strings.Join(storedResults, ",") != strings.Join(stored, ",") {
		t.Fatalf("stored %v, results %v", stored, storedResults)
	}
	reqs := e.fake.Reqs()
	// Live: the prompt that followed each turn's calls carried the stored ids.
	for turn, want := range map[int][]string{1: stored[:1], 3: stored[1:2], 5: stored[2:]} {
		calls, results := reqIDs(reqs[turn])
		_ = calls
		if got := results[len(results)-len(want):]; strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("request %d answered %v, stored %v", turn, got, want)
		}
	}
	// Replayed: the next turn's prompt holds exactly the stored ids, once each.
	calls, results := reqIDs(reqs[6])
	allUnique(t, "replayed calls", calls)
	if strings.Join(calls, ",") != strings.Join(stored, ",") || strings.Join(results, ",") != strings.Join(stored, ",") {
		t.Fatalf("replayed calls %v results %v, stored %v", calls, results, stored)
	}
}

// A paused turn that resumes after a model repeated an id from an earlier turn
// keeps the id it was stored under, and what it asks next stays unique.
func TestCallIDsUniqueAcrossTurnsOnResume(t *testing.T) {
	write := func(file string) fakeprov.Step {
		return fakeprov.Step{Calls: []fakeprov.Call{{ID: "dup", Name: "files__write_file", Args: `{"text":"` + file + `"}`}}}
	}
	e := setup(t, func(c *config.Config) {
		c.Main().MCP = []string{"files"}
		c.Main().Approval = &config.Approval{Tools: []string{"files.write_*"}, Timeout: config.Duration(time.Minute)}
	}, write("a.txt"), fakeprov.Step{Content: "one"}, // turn 1 asks (dup), then answers
		write("b.txt"), write("c.txt"), fakeprov.Step{Content: "two"}) // turn 2: dup collides, pauses; resumed, dup again
	t1 := e.post("one")
	id1 := e.waitCalls(t1, 1)[0]
	if err := e.rt.Svc.Approve(e.ctx, e.conv.ID, e.user, types.ApprovalDecided{CallID: id1, Approve: true}); err != nil {
		t.Fatal(err)
	}
	e.waitState(t1, types.StateCompleted)
	t2 := e.post("two")
	id2 := e.waitCalls(t2, 1)[0]
	if id1 != "dup" || id2 == "dup" {
		t.Fatalf("ids %q then %q", id1, id2)
	}
	if err := e.rt.Svc.Approve(e.ctx, e.conv.ID, e.user, types.ApprovalDecided{CallID: id2, Approve: true}); err != nil {
		t.Fatal(err)
	}
	ids := e.waitCalls(t2, 2)
	if ids[0] != id2 {
		t.Fatalf("the resumed turn renamed its recorded call: %v", ids)
	}
	allUnique(t, "conversation", []string{id1, ids[0], ids[1]})
	if err := e.rt.Svc.Approve(e.ctx, e.conv.ID, e.user, types.ApprovalDecided{CallID: ids[1], Approve: true}); err != nil {
		t.Fatal(err)
	}
	e.waitState(t2, types.StateCompleted)
	// The resumed turn's prompts show the stored ids.
	reqs := e.fake.Reqs()
	calls, results := reqIDs(reqs[len(reqs)-1])
	want := strings.Join([]string{id1, ids[0], ids[1]}, ",")
	if strings.Join(calls, ",") != want || strings.Join(results, ",") != want {
		t.Fatalf("last prompt calls %v results %v, want %s", calls, results, want)
	}
}
