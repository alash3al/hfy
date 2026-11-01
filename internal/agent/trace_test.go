package agent_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/provider/fakeprov"
	"github.com/alash3al/hfy/internal/types"
)

// hostileText is what a server can put in an error: NUL, a URL with a secret
// in it, an echoed key, and far more bytes than a row should keep.
func hostileText(key string) string {
	return "bad\x00 call https://x.example/p?token=SECRET123 key=" + key + " " + strings.Repeat("A", 200<<10)
}

// outboundError reads the error of the one outbound_requests row of kind.
func (e *env) outboundError(kind string) string {
	e.t.Helper()
	var msg *string
	if err := e.store.Pool.QueryRow(e.ctx, "SELECT error FROM outbound_requests WHERE kind=$1", kind).Scan(&msg); err != nil || msg == nil {
		e.t.Fatalf("outbound row of %s: %v %v", kind, msg, err)
	}
	return *msg
}

func checkTraceText(t *testing.T, text string, secrets ...string) {
	t.Helper()
	if len(text) > types.MaxErrorBytes || !utf8.ValidString(text) || strings.Contains(text, "\x00") {
		t.Errorf("trace error not clean and bounded: %d bytes", len(text))
	}
	for _, s := range append(secrets, "SECRET123", "x.example") {
		if strings.Contains(text, s) {
			t.Errorf("trace error keeps %q: %.200s", s, text)
		}
	}
	if !strings.Contains(text, "<url>") {
		t.Errorf("trace error has no <url>: %.200s", text)
	}
}

// An MCP server's failure reaches the trace row as clean, bounded, URL-scrubbed
// text, and the turn goes on.
func TestMCPCallErrorTraceIsClean(t *testing.T) {
	srv := sdk.NewServer(&sdk.Implementation{Name: "r", Version: "1"}, nil)
	srv.AddTool(&sdk.Tool{Name: "boom", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) { // a protocol error, not an isError result
			return nil, errors.New(hostileText("KEY"))
		})
	mcpSrv := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, nil))
	t.Cleanup(func() { mcpSrv.CloseClientConnections(); mcpSrv.Close() }) // the client keeps an event stream open
	e := setup(t, func(c *config.Config) {
		c.MCP = map[string]config.MCPServer{"rem": {Type: "remote", URL: mcpSrv.URL, Timeout: config.Duration(10 * time.Second)}}
		c.Main().MCP = []string{"rem"}
	}, fakeprov.Step{Calls: []fakeprov.Call{{ID: "c1", Name: "rem__boom", Args: `{}`}}}, fakeprov.Step{Content: "done"})
	turn := e.post("go")
	e.waitState(turn, types.StateCompleted)
	checkTraceText(t, e.outboundError("mcp_remote"))
}

// A provider's error body reaches the trace row and the turn's reason as
// clean, bounded text with no URL and no echoed API key.
func TestProviderErrorTraceIsClean(t *testing.T) {
	const key = "sk-live-0123456789"
	provSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, hostileText(key), http.StatusBadRequest)
	}))
	defer provSrv.Close()
	e := setup(t, func(c *config.Config) {
		c.Providers = map[string]config.Provider{"fake": {URL: provSrv.URL + "/v1", APIKey: key}}
	})
	turn := e.post("go")
	e.waitState(turn, types.StateFailed)
	checkTraceText(t, e.outboundError("model"), key)
}
