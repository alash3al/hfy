package agent

import (
	"testing"

	"github.com/alash3al/hfy/internal/svc"
	"github.com/alash3al/hfy/internal/types"
)

func TestRepeatVerdict(t *testing.T) {
	for n, want := range map[int]verdict{0: verdictOK, 1: verdictOK, 2: verdictOK,
		3: verdictWarn, 4: verdictWarn, 5: verdictWarn, 6: verdictAbort, 7: verdictAbort} {
		if got := repeatVerdict(n); got != want {
			t.Errorf("repeatVerdict(%d) = %v, want %v", n, got, want)
		}
	}
}

// The key is the same however the store writes the arguments back, and
// different calls keep different keys.
func TestCallKeyIgnoresByteForm(t *testing.T) {
	a := callKey("files.echo", []byte(`{"b":1,"a":"x"}`))
	if b := callKey("files.echo", []byte(`{"a": "x", "b": 1}`)); a != b {
		t.Errorf("reordered keys: %q != %q", a, b)
	}
	for _, other := range []string{
		callKey("files.echo", []byte(`{"a":"y","b":1}`)),
		callKey("files.write", []byte(`{"b":1,"a":"x"}`)),
	} {
		if other == a {
			t.Errorf("different call shares a key: %q", a)
		}
	}
	// Array order is part of the call: sorting elements would merge these.
	for _, pair := range [][2]string{
		{`[1,2]`, `[2,1]`},
		{`{"a":[1,2]}`, `{"a":[2,1]}`},
		{`{"a":[[1,2],[3]]}`, `{"a":[[3],[1,2]]}`},
		{`{"a":[[1,2],[3]]}`, `{"a":[[2,1],[3]]}`},
		{`{"a":[1,[2,3]]}`, `{"a":[1,[3,2]]}`},
	} {
		if callKey("files.echo", []byte(pair[0])) == callKey("files.echo", []byte(pair[1])) {
			t.Errorf("reordered array shares a key: %s and %s", pair[0], pair[1])
		}
	}
}

// Seeding a resumed turn counts its recorded calls exactly as the live path
// counted them, whatever order the stored arguments come back in.
func TestSeedCountsMatchTheLiveKeys(t *testing.T) {
	l := &loop{counts: map[string]int{}}
	l.seedCounts(svc.TurnView{Calls: []types.ToolCall{
		{Name: "hfy.now", Arguments: []byte(`{"b":1,"a":2}`)},
		{Name: "hfy.now", Arguments: []byte(`{"a": 2, "b": 1}`)},
	}})
	if n := l.counts[callKey("hfy.now", []byte(`{"a":2,"b":1}`))]; n != 2 {
		t.Fatalf("seeded count = %d, want 2", n)
	}
}

// An ask the viewer answered is not counted; a declined, timed-out or pending
// ask is, and so is any other approved call.
func TestSeedCountsSkipsAnsweredAsks(t *testing.T) {
	args := []byte(`{"questions":[]}`)
	ask := func(id string) types.ToolCall {
		return types.ToolCall{Name: svc.AskUserTool, CallID: id, Arguments: args}
	}
	l := &loop{counts: map[string]int{}}
	l.seedCounts(svc.TurnView{
		Calls: []types.ToolCall{ask("answered"), ask("declined"), ask("timed_out"), ask("pending"),
			{Name: "files.write", CallID: "held", Arguments: []byte(`{}`)}},
		Decided: map[string]types.ApprovalDecided{
			"answered":  {CallID: "answered", Approve: true},
			"declined":  {CallID: "declined", Approve: false},
			"timed_out": {CallID: "timed_out", Approve: false, Reason: "timeout"},
			"held":      {CallID: "held", Approve: true},
		},
	})
	if n := l.counts[callKey(svc.AskUserTool, args)]; n != 3 {
		t.Errorf("asks counted = %d, want 3 (declined, timed out, pending)", n)
	}
	if n := l.counts[callKey("files.write", []byte(`{}`))]; n != 1 {
		t.Errorf("approved non-ask counted = %d, want 1", n)
	}
}
