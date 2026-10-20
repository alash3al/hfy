package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alash3al/hfy/internal/config"
)

// Client is an external MCP server (local stdio process or remote HTTP). It
// connects lazily and reuses the connection; the tool list is cached until the
// server reports a change.
type Client struct {
	name string
	cfg  config.MCPServer
	base context.Context // process lifetime; connections outlive any single request

	mu       sync.Mutex
	failedAt time.Time // last failed connect: retry no sooner than retryAfter
	lastErr  error
	session  *sdk.ClientSession
	tools    []Tool
	fresh    bool
}

func NewClient(base context.Context, name string, cfg config.MCPServer) *Client {
	return &Client{name: name, cfg: cfg, base: base}
}

// retryAfter keeps an unreachable server from adding a connect attempt to every step.
const retryAfter = 10 * time.Second

func (c *Client) Name() string { return c.name }

func (c *Client) Endpoint() string {
	if c.cfg.Type == "remote" {
		return c.cfg.URL
	}
	return ""
}

func (c *Client) connect() (*sdk.ClientSession, error) {
	if c.session != nil {
		return c.session, nil
	}
	if time.Since(c.failedAt) < retryAfter {
		return nil, c.lastErr
	}
	cl := sdk.NewClient(&sdk.Implementation{Name: "hfy", Version: "1"}, &sdk.ClientOptions{
		ToolListChangedHandler: func(context.Context, *sdk.ToolListChangedRequest) {
			c.mu.Lock()
			c.fresh = false
			c.mu.Unlock()
		},
	})
	var tr sdk.Transport
	switch c.cfg.Type {
	case "local":
		cmd := exec.Command(c.cfg.Command[0], c.cfg.Command[1:]...)
		cmd.Env = os.Environ()
		for k, v := range c.cfg.Environment {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		cmd.Stderr = os.Stderr
		tr = &sdk.CommandTransport{Command: cmd}
	default:
		tr = &sdk.StreamableClientTransport{
			Endpoint:   c.cfg.URL,
			HTTPClient: &http.Client{Transport: headerTransport{h: c.cfg.Headers, next: http.DefaultTransport}},
		}
	}
	sess, err := cl.Connect(c.base, tr, nil)
	if err != nil {
		c.failedAt, c.lastErr = time.Now(), fmt.Errorf("connect mcp %s: %w", c.name, err)
		return nil, c.lastErr
	}
	c.session = sess
	return sess, nil
}

// drop forgets a broken session so the next call reconnects.
func (c *Client) drop() {
	if c.session != nil {
		c.session.Close()
		c.session = nil
		c.fresh = false
	}
}

func (c *Client) Tools(ctx context.Context) ([]Tool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fresh {
		return c.tools, nil
	}
	sess, err := c.connect()
	if err != nil {
		return nil, err
	}
	var out []Tool
	params := &sdk.ListToolsParams{}
	for {
		res, err := sess.ListTools(ctx, params)
		if err != nil {
			c.drop()
			return nil, fmt.Errorf("list tools of %s: %w", c.name, err)
		}
		for _, t := range res.Tools {
			schema, _ := json.Marshal(t.InputSchema)
			out = append(out, Tool{Server: c.name, Name: t.Name, Description: t.Description, Schema: schema})
		}
		if res.NextCursor == "" {
			break
		}
		params.Cursor = res.NextCursor
	}
	c.tools, c.fresh = out, true
	return out, nil
}

func (c *Client) Call(ctx context.Context, tool string, args json.RawMessage, meta *TurnMeta) (Result, error) {
	c.mu.Lock()
	sess, err := c.connect()
	c.mu.Unlock()
	if err != nil {
		return Result{}, err
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	// Cancelling ctx makes the SDK send notifications/cancelled to the server.
	params := &sdk.CallToolParams{Name: tool, Arguments: args}
	if meta != nil {
		params.Meta = sdk.Meta{MetaKey: meta}
	}
	res, err := sess.CallTool(ctx, params)
	if err != nil {
		if ctx.Err() == nil { // a transport failure, not a cancellation
			c.mu.Lock()
			if c.session == sess {
				c.drop()
			}
			c.mu.Unlock()
		}
		return Result{}, err
	}
	out := Result{Content: flatten(res), IsError: res.IsError}
	if res.StructuredContent != nil {
		out.Structured, _ = json.Marshal(res.StructuredContent)
	}
	return out, nil
}

// Close ends the connection (and the child process for local servers).
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.drop()
}

// flatten turns MCP content into text for the model. Images are not forwarded
// in v1: they become a placeholder.
func flatten(res *sdk.CallToolResult) string {
	var parts []string
	for _, ct := range res.Content {
		switch v := ct.(type) {
		case *sdk.TextContent:
			parts = append(parts, v.Text)
		case *sdk.ImageContent:
			parts = append(parts, "[image: "+v.MIMEType+"]")
		case *sdk.AudioContent:
			parts = append(parts, "[audio: "+v.MIMEType+"]")
		case *sdk.EmbeddedResource:
			if v.Resource != nil && v.Resource.Text != "" {
				parts = append(parts, v.Resource.Text)
			} else if v.Resource != nil {
				parts = append(parts, "[resource: "+v.Resource.URI+"]")
			}
		case *sdk.ResourceLink:
			parts = append(parts, "[resource: "+v.URI+"]")
		default:
			if b, err := json.Marshal(ct); err == nil {
				parts = append(parts, string(b))
			}
		}
	}
	if len(parts) == 0 && res.StructuredContent != nil {
		if b, err := json.Marshal(res.StructuredContent); err == nil {
			return string(b)
		}
	}
	return strings.Join(parts, "\n")
}

type headerTransport struct {
	h    map[string]string
	next http.RoundTripper
}

func (t headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range t.h {
		r.Header.Set(k, v)
	}
	return t.next.RoundTrip(r)
}

// StartAll registers a Client per configured server. Connections are lazy, so
// a server that is down at boot does not stop hfy; each is warmed up in the
// background and failures are logged.
func StartAll(base context.Context, m *Manager, servers map[string]config.MCPServer) []*Client {
	var clients []*Client
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		c := NewClient(base, name, servers[name])
		m.Add(c)
		clients = append(clients, c)
		go func() {
			if _, err := c.Tools(base); err != nil && base.Err() == nil {
				slog.Warn("mcp server unavailable at boot; will retry on use", "server", name, "err", err)
			}
		}()
	}
	return clients
}
