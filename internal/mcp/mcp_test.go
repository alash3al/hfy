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
	return config.MCPServer{Type: "local", Command: []string{os.Args[0]}, Environment: map[string]string{"HFY_FAKE_MCP": "1"}}
}

func TestLocalClientListsAndCalls(t *testing.T) {
	ctx := t.Context()
	m := mcp.NewManager()
	c := mcp.NewClient(ctx, "files", fakeServer())
	defer c.Close()
	m.Add(c)

	tools, errs := m.Tools(ctx, nil)
	if len(errs) > 0 || len(tools) != 5 { // echo, write_file, whoami, boom, slow
		t.Fatalf("tools=%v errs=%v", tools, errs)
	}
	if tools[0].FullName() != "files.boom" || mcp.WireName(tools[0]) != "files__boom" {
		t.Fatalf("names: %+v", tools[0])
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

func TestLocalSource(t *testing.T) {
	l := mcp.NewLocal("harness")
	l.Add(mcp.Tool{Name: "ping"}, func(context.Context, json.RawMessage) (mcp.Result, error) {
		return mcp.Result{Content: "pong"}, nil
	})
	m := mcp.NewManager()
	m.Add(l)
	r, err := m.Call(context.Background(), "harness.ping", nil, nil)
	if err != nil || r.Content != "pong" {
		t.Fatal(r, err)
	}
	if _, err := m.Call(context.Background(), "harness.nope", nil, nil); err == nil {
		t.Fatal("unknown tool must error")
	}
}
