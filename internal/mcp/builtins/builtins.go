// Package builtins holds the built-in tools, named hfy.<tool> as if served by
// an MCP server named "hfy"; the agent loop runs them itself. The core tools
// are pure and run from here; the handlers of those that need runtime state
// (searches, sub-agents) live in the agent package.
package builtins

import (
	"context"
	_ "embed"
	"encoding/json"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/mcp"
)

// Server is the server part of a built-in's name (reserved in the config).
const Server = config.BuiltinServer

const (
	ToolSearch         = "tool_search"
	ConversationSearch = "conversation_search"
	Now                = "now"
	MathCalc           = "math_calc"
	AskUser            = "ask_user"
)

// Core is a built-in every agent gets. A tool is core only if it is pure (no
// I/O, no app data, no permissions, so safe for anyone) and does what models
// get wrong on their own. Anything touching data belongs to the app's MCP
// servers.
type Core struct {
	Tool mcp.Tool
	Run  func(context.Context, json.RawMessage) (mcp.Result, error)
}

func CoreTools() []Core {
	return []Core{{build(Now, ""), now}, {build(MathCalc, ""), mathCalc}}
}

//go:embed tools.json
var raw []byte

type def struct {
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

var defs = func() map[string]def {
	m := map[string]def{}
	if err := json.Unmarshal(raw, &m); err != nil {
		panic("builtins/tools.json: " + err.Error())
	}
	return m
}()

func ToolSearchTool() mcp.Tool         { return build(ToolSearch, "") }
func ConversationSearchTool() mcp.Tool { return build(ConversationSearch, "") }

// AskUserTool is hfy.ask_user, the harness interaction primitive: calling it
// pauses the turn for the viewer's answers instead of running anything.
func AskUserTool() mcp.Tool { return build(AskUser, "") }

// SubagentTool wraps a sub-agent as hfy.<name>.
func SubagentTool(name, description string) mcp.Tool {
	t := build("subagent", description)
	t.Name = name
	return t
}

func build(key, desc string) mcp.Tool {
	d := defs[key]
	if desc == "" {
		desc = d.Description
	}
	return mcp.Tool{Server: Server, Name: key, Description: desc, Schema: d.Schema}
}
