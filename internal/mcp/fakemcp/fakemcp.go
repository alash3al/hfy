// Package fakemcp is a tiny stdio MCP server for tests. A test binary becomes
// the server when HFY_FAKE_MCP=1: call MaybeRun from TestMain.
package fakemcp

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type echoIn struct {
	Text string `json:"text"`
}

type noIn struct{}

// destructive, readOnly are annotation values the tests assert on.
var destructive, readOnly = true, true

// MaybeRun serves the fake MCP server and exits if HFY_FAKE_MCP is set.
func MaybeRun() {
	if os.Getenv("HFY_FAKE_MCP") != "1" {
		return
	}
	s := sdk.NewServer(&sdk.Implementation{Name: "fake", Version: "1"}, nil)
	sdk.AddTool(s, &sdk.Tool{Name: "echo", Description: "Echo the text back",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: readOnly}},
		func(ctx context.Context, _ *sdk.CallToolRequest, in echoIn) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "echo: " + in.Text}}}, nil, nil
		})
	sdk.AddTool(s, &sdk.Tool{Name: "write_file", Description: "Pretend to write a file",
		Annotations: &sdk.ToolAnnotations{DestructiveHint: &destructive}},
		func(ctx context.Context, _ *sdk.CallToolRequest, in echoIn) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "wrote " + in.Text}}}, nil, nil
		})
	sdk.AddTool(s, &sdk.Tool{Name: "whoami", Description: "Echo the request _meta; return a UI component"},
		func(ctx context.Context, req *sdk.CallToolRequest, _ noIn) (*sdk.CallToolResult, any, error) {
			meta, _ := json.Marshal(req.Params.Meta)
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: string(meta)}}},
				map[string]any{"component": "card", "title": "Margherita"}, nil
		})
	sdk.AddTool(s, &sdk.Tool{Name: "getenv", Description: "Return an environment variable of the server process"},
		func(ctx context.Context, _ *sdk.CallToolRequest, in echoIn) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: os.Getenv(in.Text)}}}, nil, nil
		})
	sdk.AddTool(s, &sdk.Tool{Name: "ui", Description: "Return ui _meta next to text content"},
		func(ctx context.Context, _ *sdk.CallToolRequest, _ noIn) (*sdk.CallToolResult, any, error) {
			// The key matches mcp.UIMetaKey (this package imports nothing).
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "here is your order"}},
				Meta: map[string]any{"hfy/ui": map[string]any{"v": 1, "type": "card", "title": "Margherita", "body": "12 EUR"}}}, nil, nil
		})
	sdk.AddTool(s, &sdk.Tool{Name: "ui_ask", Description: "Return text plus a card that carries questions"},
		func(ctx context.Context, _ *sdk.CallToolRequest, _ noIn) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "your pizza, pick a size"}},
				Meta: map[string]any{"hfy/ui": map[string]any{"v": 1, "type": "card", "title": "Order",
					"questions": map[string]any{"questions": []any{map[string]any{"id": "size",
						"text": "Which size?", "type": "single", "options": []string{"small", "large"}}}}}}}, nil, nil
		})
	sdk.AddTool(s, &sdk.Tool{Name: "ui_boom", Description: "Fails and returns ui _meta anyway"},
		func(ctx context.Context, _ *sdk.CallToolRequest, _ noIn) (*sdk.CallToolResult, any, error) {
			r := &sdk.CallToolResult{Meta: map[string]any{"hfy/ui": map[string]any{"v": 1, "type": "card", "title": "Doomed"}}}
			r.SetError(os.ErrInvalid)
			return r, nil, nil
		})
	sdk.AddTool(s, &sdk.Tool{Name: "ui_silent", Description: "Return ui _meta with no content blocks"},
		func(ctx context.Context, _ *sdk.CallToolRequest, _ noIn) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{Meta: map[string]any{"hfy/ui": map[string]any{"v": 1, "type": "card", "title": "Quiet card"}}}, nil, nil
		})
	sdk.AddTool(s, &sdk.Tool{Name: "ui_questions", Description: "Return questions with no card part"},
		func(ctx context.Context, _ *sdk.CallToolRequest, _ noIn) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "just asking"}},
				Meta: map[string]any{"hfy/ui": map[string]any{"v": 1,
					"questions": map[string]any{"questions": []any{map[string]any{"id": "pitch",
						"text": "Which?", "type": "single", "options": []string{"up", "down"}}}}}}}, nil, nil
		})
	sdk.AddTool(s, &sdk.Tool{Name: "ui_ask_stop", Description: "Ask for a stop, then return questions"},
		func(ctx context.Context, req *sdk.CallToolRequest, _ noIn) (*sdk.CallToolResult, any, error) {
			// The test plays the stop requestor: one post, awaited, so the
			// cancel_requested event is committed before the result returns.
			res, err := http.Post(os.Getenv("HFY_STOP_URL"), "text/plain", nil)
			if err != nil {
				return nil, nil, err
			}
			res.Body.Close()
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "your pizza, pick a size"}},
				Meta: map[string]any{"hfy/ui": map[string]any{"v": 1, "type": "card", "title": "Order",
					"questions": map[string]any{"questions": []any{map[string]any{"id": "size",
						"text": "Which size?", "type": "single", "options": []string{"small", "large"}}}}}}}, nil, nil
		})
	sdk.AddTool(s, &sdk.Tool{Name: "boom", Description: "Always fails"},
		func(ctx context.Context, _ *sdk.CallToolRequest, _ noIn) (*sdk.CallToolResult, any, error) {
			r := &sdk.CallToolResult{}
			r.SetError(os.ErrInvalid)
			return r, nil, nil
		})
	sdk.AddTool(s, &sdk.Tool{Name: "slow", Description: "Blocks until cancelled"},
		func(ctx context.Context, _ *sdk.CallToolRequest, _ noIn) (*sdk.CallToolResult, any, error) {
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-time.After(time.Minute):
				return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "done"}}}, nil, nil
			}
		})
	if err := s.Run(context.Background(), &sdk.StdioTransport{}); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}
