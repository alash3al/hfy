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
agent:
  model: mockprov/test
  settings: {context_window: 8192}
embeddings: {model: mockprov/emb, dimensions: 8}
`), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	rt, err := NewRuntime(cfg, nil, mcp.NewManager())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rt.agents["main"].Client.(*provider.Mock); !ok {
		t.Fatalf("main client = %T, want *provider.Mock", rt.agents["main"].Client)
	}
	if _, ok := rt.embedder.(*provider.Mock); !ok {
		t.Fatalf("embedder = %T, want *provider.Mock", rt.embedder)
	}
}
