// Package mcp gives the agent loop one tool path: every tool, external or
// built in, is a Tool served by a Source and addressed as <server>.<tool>.
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
}

// FullName is the <server>.<tool> name used in config patterns and events.
func (t Tool) FullName() string { return t.Server + "." + t.Name }

type Result struct {
	Content    string          // what the model sees
	Structured json.RawMessage // the tool's structuredContent, for clients (e.g. UI components)
	IsError    bool
}

// MetaKey is the _meta key every tool call carries (reverse DNS, as the MCP
// spec asks): who is acting, in which session and turn, so a tool server can
// authorize the call itself. Its value is a TurnMeta.
const MetaKey = "io.github.alash3al.hfy/turn"

// TurnMeta is the per-call context sent under MetaKey.
type TurnMeta struct {
	Session string            `json:"session"` // session uuid
	Turn    string            `json:"turn"`    // turn uuid
	CallID  string            `json:"call_id"`
	Agent   string            `json:"agent"` // the agent (or sub-agent) making the call
	Actor   ActorMeta         `json:"actor"` // who started the turn
	Scope   string            `json:"scope"`
	Vars    map[string]string `json:"vars,omitempty"`
}

type ActorMeta struct {
	Type string `json:"type"`
	Ref  string `json:"ref"`
}

// Source serves the tools of one MCP server (external) or the in-process built-in server (hfy).
type Source interface {
	Name() string
	// Endpoint is the remote URL for remote servers, "" otherwise.
	Endpoint() string
	Tools(ctx context.Context) ([]Tool, error)
	Call(ctx context.Context, tool string, args json.RawMessage, meta *TurnMeta) (Result, error)
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

// Manager holds all sources and resolves full names to calls.
type Manager struct {
	mu      sync.RWMutex
	sources map[string]Source
}

func NewManager() *Manager { return &Manager{sources: map[string]Source{}} }

func (m *Manager) Add(s Source) {
	m.mu.Lock()
	m.sources[s.Name()] = s
	m.mu.Unlock()
}

func (m *Manager) Source(name string) (Source, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sources[name]
	return s, ok
}

// Tools lists the tools of the given servers (nil = all), sorted by full name.
// A server that fails to list is skipped and reported in errs.
func (m *Manager) Tools(ctx context.Context, servers []string) (tools []Tool, errs map[string]error) {
	m.mu.RLock()
	var srcs []Source
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

// Local is an in-process Source; the built-in server (hfy) is one.
type Local struct {
	name     string
	mu       sync.RWMutex
	tools    []Tool
	handlers map[string]func(context.Context, json.RawMessage) (Result, error)
}

func NewLocal(name string) *Local {
	return &Local{name: name, handlers: map[string]func(context.Context, json.RawMessage) (Result, error){}}
}

func (l *Local) Add(t Tool, h func(context.Context, json.RawMessage) (Result, error)) {
	t.Server = l.name
	l.mu.Lock()
	l.tools = append(l.tools, t)
	l.handlers[t.Name] = h
	l.mu.Unlock()
}

func (l *Local) Name() string     { return l.name }
func (l *Local) Endpoint() string { return "" }

func (l *Local) Tools(context.Context) ([]Tool, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return append([]Tool(nil), l.tools...), nil
}

// Call runs a built-in; they read the turn from ctx, not from meta.
func (l *Local) Call(ctx context.Context, tool string, args json.RawMessage, _ *TurnMeta) (Result, error) {
	l.mu.RLock()
	h, ok := l.handlers[tool]
	l.mu.RUnlock()
	if !ok {
		return Result{}, fmt.Errorf("unknown tool %q on %s", tool, l.name)
	}
	return h(ctx, args)
}
