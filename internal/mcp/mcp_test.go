package mcp_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/mcp"
	"github.com/alash3al/hfy/internal/mcp/fakemcp"
)

func TestMain(m *testing.M) {
	fakemcp.MaybeRun()
	os.Exit(m.Run())
}

func fakeServer() config.MCPServer {
	return config.MCPServer{Type: "local", Command: []string{os.Args[0]}, Environment: map[string]string{"HFY_FAKE_MCP": "1"},
		Timeout: config.Duration(time.Minute)}
}

func TestLocalClientListsAndCalls(t *testing.T) {
	ctx := t.Context()
	m := mcp.NewManager()
	c := mcp.NewClient(ctx, "files", fakeServer())
	defer c.Close()
	m.Add(c)

	tools, errs := m.Tools(ctx, nil)
	if len(errs) > 0 || len(tools) != 6 { // echo, write_file, whoami, getenv, boom, slow
		t.Fatalf("tools=%v errs=%v", tools, errs)
	}
	if tools[0].FullName() != "files.boom" || mcp.WireName(tools[0]) != "files__boom" {
		t.Fatalf("names: %+v", tools[0])
	}
	// Annotations map onto Destructive: read-only or unannotated never.
	for _, tool := range tools {
		if want := tool.Name == "write_file"; tool.Destructive != want {
			t.Fatalf("%s: Destructive=%v, want %v", tool.FullName(), tool.Destructive, want)
		}
	}
	res, err := m.Call(ctx, "files.echo", json.RawMessage(`{"text":"hi"}`), nil)
	if err != nil || res.Content != "echo: hi" || res.IsError {
		t.Fatalf("echo: %+v %v", res, err)
	}
	res, err = m.Call(ctx, "files.boom", nil, nil)
	if err != nil || !res.IsError {
		t.Fatalf("boom must be an error result, got %+v %v", res, err)
	}
}

func TestCancelStopsTool(t *testing.T) {
	ctx := context.Background()
	c := mcp.NewClient(ctx, "files", fakeServer())
	defer c.Close()
	callCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Call(callCtx, "slow", nil, nil)
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("slow call must be cancelled quickly: err=%v after %v", err, time.Since(start))
	}
	// the connection survives a cancellation
	res, err := c.Call(ctx, "echo", json.RawMessage(`{"text":"again"}`), nil)
	if err != nil || !strings.Contains(res.Content, "again") {
		t.Fatalf("after cancel: %+v %v", res, err)
	}
}

// A local server gets only what it needs from hfy's environment, never its
// secrets, plus its own configured environment.
func TestLocalServerDoesNotInheritSecrets(t *testing.T) {
	t.Setenv("HFY_SERVER_SECRET", "leak")
	c := mcp.NewClient(t.Context(), "files", fakeServer())
	defer c.Close()
	for name, want := range map[string]string{"HFY_SERVER_SECRET": "", "HFY_FAKE_MCP": "1", "PATH": os.Getenv("PATH")} {
		res, err := c.Call(t.Context(), "getenv", json.RawMessage(`{"text":"`+name+`"}`), nil)
		if err != nil || res.Content != want {
			t.Fatalf("%s = %q (%v), want %q", name, res.Content, err, want)
		}
	}
}

// A tool that does not answer within the server's timeout fails the call, not
// the turn, and the connection survives.
func TestToolCallTimeout(t *testing.T) {
	cfg := fakeServer()
	cfg.Timeout = config.Duration(300 * time.Millisecond)
	c := mcp.NewClient(t.Context(), "files", cfg)
	defer c.Close()
	start := time.Now()
	_, err := c.Call(t.Context(), "slow", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "did not answer within 300ms") || time.Since(start) > 5*time.Second {
		t.Fatalf("slow call: %v after %v", err, time.Since(start))
	}
	if res, err := c.Call(t.Context(), "echo", json.RawMessage(`{"text":"still here"}`), nil); err != nil || !strings.Contains(res.Content, "still here") {
		t.Fatalf("after a timeout: %+v %v", res, err)
	}
}
