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
		c.Agent.MCP = []string{"files"}
		c.Agent.Approval = &config.Approval{Tools: []string{"files.write_*"}, Timeout: config.Duration(time.Minute)}
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
	e := setup(t, func(c *config.Config) { c.Agent.MCP = []string{"files"} },
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
		c.Agent.AskUser = true
		c.Agent.Approval = &config.Approval{Timeout: config.Duration(time.Minute)}
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
