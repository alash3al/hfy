package agent

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/alash3al/hfy/internal/mcp/builtins"
	"github.com/alash3al/hfy/internal/provider"
)

// load marks the ids of the history used, whatever side carries them, and
// renames a new call only when it collides.
func TestLoadMarksHistoryIDsUsed(t *testing.T) {
	n := 0 // distinct arguments, so the repeat guard stays out of it
	now := func(id string) provider.ToolCall {
		n++
		return provider.ToolCall{ID: id, Name: "hfy__now", Arguments: `{"n":` + strconv.Itoa(n) + `}`}
	}
	c := &scripted{replies: []*provider.Response{
		{ToolCalls: []provider.ToolCall{now("h"), now("R"), now("r"), now("h"), now("call_0_1"), now("new")}},
		textReply("done"),
	}}
	l := testLoop(c, newTestEnv(), builtins.Now)
	l.load(context.Background(), "sys", []provider.Message{{Role: "user", Content: "go"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{now("h"), now("call_1_0")}},
		{Role: "tool", ToolCallID: "h", Content: "x"}, {Role: "tool", ToolCallID: "call_1_0", Content: "x"},
		{Role: "tool", ToolCallID: "orphan", Content: "x"}})
	if res, err := l.run(context.Background()); err != nil || res.Outcome != Done {
		t.Fatalf("run: %+v %v", res, err)
	}
	var got []string
	for _, m := range l.msgs[5:] {
		for _, tc := range m.ToolCalls {
			got = append(got, tc.ID)
		}
	}
	// "h" collides with history and with itself; ids are case-sensitive; call_0_1 is free in history.
	if s := strings.Join(got, ","); s != "call_1_0_1,R,r,call_1_3,call_0_1,new" {
		t.Fatalf("ids: %s", s)
	}
	if !l.used["orphan"] {
		t.Fatal("a result's id is history too")
	}
}
