// Package harness holds the built-in tools, served by the in-process MCP
// server named "hfy" as hfy.<tool>. The core tools are pure and served from
// here; the handlers of those that need runtime state (searches, sub-agents)
// live in the agent package.
package harness

import (
	"context"
	_ "embed"
	"encoding/json"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/mcp"
)

// Server is the name of the in-process MCP server (reserved in the config).
const Server = config.BuiltinServer

const (
	ToolSearch    = "tool_search"
	SessionSearch = "session_search"
	Now           = "now"
	MathCalc      = "math_calc"
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
		panic("harness/tools.json: " + err.Error())
	}
	return m
}()

func ToolSearchTool() mcp.Tool    { return build(ToolSearch, "") }
func SessionSearchTool() mcp.Tool { return build(SessionSearch, "") }

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
