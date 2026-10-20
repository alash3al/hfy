// Package harness holds the definitions of the built-in tools served by the
// in-process MCP server named "harness". The handlers live in the agent
// package, which owns the state they need.
package harness

import (
	_ "embed"
	"encoding/json"

	"github.com/alash3al/hfy/internal/mcp"
)

// Server is the name of the in-process MCP server.
const Server = "harness"

const (
	ToolSearch    = "tool_search"
	SessionSearch = "session_search"
)

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

// SubagentTool wraps a sub-agent as harness.<name>.
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
