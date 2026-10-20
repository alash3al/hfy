// Package fakemcp is a tiny stdio MCP server for tests. A test binary becomes
// the server when HFY_FAKE_MCP=1: call MaybeRun from TestMain.
package fakemcp

import (
	"context"
	"os"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type echoIn struct {
	Text string `json:"text"`
}

type noIn struct{}

// MaybeRun serves the fake MCP server and exits if HFY_FAKE_MCP is set.
func MaybeRun() {
	if os.Getenv("HFY_FAKE_MCP") != "1" {
		return
	}
	s := sdk.NewServer(&sdk.Implementation{Name: "fake", Version: "1"}, nil)
	sdk.AddTool(s, &sdk.Tool{Name: "echo", Description: "Echo the text back"},
		func(ctx context.Context, _ *sdk.CallToolRequest, in echoIn) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "echo: " + in.Text}}}, nil, nil
		})
	sdk.AddTool(s, &sdk.Tool{Name: "write_file", Description: "Pretend to write a file"},
		func(ctx context.Context, _ *sdk.CallToolRequest, in echoIn) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "wrote " + in.Text}}}, nil, nil
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
