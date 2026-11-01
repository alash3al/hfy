package agent

import (
	"testing"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/mcp"
	"github.com/alash3al/hfy/internal/provider"
)

func TestMockProviderWiring(t *testing.T) {
	cfg, err := config.Parse([]byte(`
server: {secret: test-secret}
database: {url: "postgres://localhost:5432/hfy_test"}
providers:
  mockprov:
    type: mock
    mock: {reply: "hi"}
agents:
  main:
    model: mockprov/test
    settings: {context_window: 8192}
`), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	rt, err := NewRuntime(cfg, nil, mcp.NewManager())
	if err != nil {
		t.Fatal(err)
	}
	main, ok := rt.agents["main"].Client.(controlled)
	if !ok {
		t.Fatalf("main client = %T, want controlled", rt.agents["main"].Client)
	}
	if _, ok := main.Adapter.(*provider.Mock); !ok {
		t.Fatalf("main adapter = %T, want *provider.Mock", main.Adapter)
	}
}
