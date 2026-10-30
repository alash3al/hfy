// Package mcp calls the tools of the configured MCP servers. Every tool, theirs
// or one of hfy's built-ins (which the agent loop runs itself), is a Tool
// addressed as <server>.<tool>.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
)

type Tool struct {
	Server      string
	Name        string
	Description string
	Schema      json.RawMessage // JSON Schema for the arguments
	// Destructive is what the server's annotations say: readOnlyHint is not
	// true AND destructiveHint is explicitly true (the spec's implicit
	// default counts as false, so unannotated tools are never destructive).
	Destructive bool
}

// FullName is the <server>.<tool> name used in config patterns and events.
func (t Tool) FullName() string { return t.Server + "." + t.Name }

type Result struct {
	Content    string          // what the model sees
	Structured json.RawMessage // the tool's structuredContent, for clients (e.g. UI components)
	// UI is the tool's _meta "hfy/ui" block: the raw block as returned (in
	// Call) and the sanitized card/questions once the agent loop gates it.
	// The model never sees either.
	UI        json.RawMessage
	Questions json.RawMessage
	IsError   bool
}

// MetaKey and UIMetaKey are the _meta keys a tool call carries: MetaKey names
// who is acting, in which conversation and turn, so a tool server can authorize
// the call itself (its value is a TurnMeta); UIMetaKey names a UI block
// (a card for the chat timeline) the tool may return.
const (
	MetaKey   = "hfy/turn"
	UIMetaKey = "hfy/ui"
)

// TurnMeta is the per-call context sent under MetaKey.
type TurnMeta struct {
	Conversation string            `json:"conversation"` // conversation uuid
	Turn         string            `json:"turn"`         // turn uuid
	CallID       string            `json:"call_id"`
	Agent        string            `json:"agent"` // the agent (or sub-agent) making the call
	Actor        ActorMeta         `json:"actor"` // who started the turn
	Scope        string            `json:"scope"`
	Vars         map[string]string `json:"vars,omitempty"`
}

type ActorMeta struct {
	Type string `json:"type"`
	Ref  string `json:"ref"`
}

var wireUnsafe = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// WireName is the provider-safe function name for a tool ("files.read" -> "files__read").
func WireName(t Tool) string {
	return wireUnsafe.ReplaceAllString(t.Server, "_") + "__" + wireUnsafe.ReplaceAllString(t.Name, "_")
}

// WireNameOf is WireName for a stored full name ("files.read").
func WireNameOf(full string) string {
	server, tool, ok := strings.Cut(full, ".")
	if !ok {
		return full
	}
	return WireName(Tool{Server: server, Name: tool})
}

// Manager holds the configured servers and resolves full names to calls.
type Manager struct {
	mu      sync.RWMutex
	sources map[string]*Client
}

func NewManager() *Manager { return &Manager{sources: map[string]*Client{}} }

func (m *Manager) Add(c *Client) {
	m.mu.Lock()
	m.sources[c.Name()] = c
	m.mu.Unlock()
}

func (m *Manager) Source(name string) (*Client, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sources[name]
	return s, ok
}

// Tools lists the tools of the given servers (nil = all), sorted by full name.
// A server that fails to list is skipped and reported in errs.
func (m *Manager) Tools(ctx context.Context, servers []string) (tools []Tool, errs map[string]error) {
	m.mu.RLock()
	var srcs []*Client
	if servers == nil {
		for _, s := range m.sources {
			srcs = append(srcs, s)
		}
	} else {
		for _, n := range servers {
			if s, ok := m.sources[n]; ok {
				srcs = append(srcs, s)
			}
		}
	}
	m.mu.RUnlock()
	for _, s := range srcs {
		ts, err := s.Tools(ctx)
		if err != nil {
			if errs == nil {
				errs = map[string]error{}
			}
			errs[s.Name()] = err
			continue
		}
		tools = append(tools, ts...)
	}
	slices.SortFunc(tools, func(a, b Tool) int { return strings.Compare(a.FullName(), b.FullName()) })
	return tools, errs
}

// Call runs <server>.<tool>.
func (m *Manager) Call(ctx context.Context, full string, args json.RawMessage, meta *TurnMeta) (Result, error) {
	server, tool, ok := strings.Cut(full, ".")
	if !ok {
		return Result{}, fmt.Errorf("tool name %q must look like <server>.<tool>", full)
	}
	s, found := m.Source(server)
	if !found {
		return Result{}, fmt.Errorf("unknown server %q", server)
	}
	return s.Call(ctx, tool, args, meta)
}
