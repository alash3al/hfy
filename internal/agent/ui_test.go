package agent_test

import (
	"testing"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/provider/fakeprov"
	"github.com/alash3al/hfy/internal/types"
)

func uiServer(c *config.Config) {
	srv := c.MCP["files"]
	srv.UI = &config.UI{ImageHosts: []string{"imgs.example"}}
	c.MCP["files"] = srv
}

// A card a ui-opted-in server returns rides the tool_result event; a tool
// result with questions keeps them on the result for a later pause.
func TestUIRidesTheToolResult(t *testing.T) {
	e := setup(t, func(c *config.Config) {
		c.Agent.MCP = []string{"files"}
		uiServer(c)
	},
		fakeprov.Step{Calls: []fakeprov.Call{{ID: "u", Name: "files__ui"}}},
		fakeprov.Step{Content: "enjoy"},
	)
	turn := e.post("one margherita")
	e.waitState(turn, types.StateCompleted)
	r := e.find(turn, types.EventToolResult)
	if len(r) != 1 || r[0]["content"] != "here is your order" {
		t.Fatalf("tool result: %v\n%s", r, e.dump(turn))
	}
	card, _ := r[0]["ui"].(map[string]any)
	if card == nil || card["title"] != "Margherita" {
		t.Fatalf("card: %v", r[0]["ui"])
	}
}

// A card from a server that has no ui config never reaches a client.
func TestUIWithoutConfigIsStripped(t *testing.T) {
	e := setup(t, func(c *config.Config) { c.Agent.MCP = []string{"files"} },
		fakeprov.Step{Calls: []fakeprov.Call{{ID: "u", Name: "files__ui"}}},
		fakeprov.Step{Content: "enjoy"},
	)
	turn := e.post("one margherita")
	e.waitState(turn, types.StateCompleted)
	r := e.find(turn, types.EventToolResult)
	if len(r) != 1 || r[0]["ui"] != nil {
		t.Fatalf("ui survived the strip: %v", r)
	}
}

// A failed call for its ui: an error strips the card too.
func TestUIDiesWithTheCall(t *testing.T) {
	e := setup(t, func(c *config.Config) {
		c.Agent.MCP = []string{"files"}
		uiServer(c)
	},
		fakeprov.Step{Calls: []fakeprov.Call{{ID: "b", Name: "files__ui_boom"}}},
		fakeprov.Step{Content: "sorry"},
	)
	turn := e.post("try it")
	e.waitState(turn, types.StateCompleted)
	r := e.find(turn, types.EventToolResult)
	if len(r) != 1 || !r[0]["is_error"].(bool) || r[0]["ui"] != nil {
		t.Fatalf("error result keeps its ui: %v", r)
	}
}

// A contentless call's card rides the result like any other.
func TestUISilentCardRidesToo(t *testing.T) {
	e := setup(t, func(c *config.Config) {
		c.Agent.MCP = []string{"files"}
		uiServer(c)
	},
		fakeprov.Step{Calls: []fakeprov.Call{{ID: "s", Name: "files__ui_silent"}}},
		fakeprov.Step{Content: "quiet it is"},
	)
	turn := e.post("anything quiet")
	e.waitState(turn, types.StateCompleted)
	r := e.find(turn, types.EventToolResult)
	if len(r) != 1 || r[0]["content"] != "" {
		t.Fatalf("silent result: %v", r)
	}
	card, _ := r[0]["ui"].(map[string]any)
	if card == nil || card["title"] != "Quiet card" {
		t.Fatalf("silent card: %v", r[0]["ui"])
	}
}
