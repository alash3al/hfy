package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/alash3al/hfy/internal/provider"
)

// The next prompt's size is the last measurement plus what was added since;
// a provider that reports no usage gets an estimate, so compaction still runs.
func TestPromptSize(t *testing.T) {
	l := &loop{system: strings.Repeat("s", 400), msgs: []provider.Message{{Role: "user", Content: strings.Repeat("u", 400)}}}
	l.measure(0) // no usage reported
	if got := l.promptSize(); got != 200 {
		t.Fatalf("estimated size: %d", got)
	}
	l.measure(1000) // reported by the provider
	l.msgs = append(l.msgs, provider.Message{Role: "tool", Content: strings.Repeat("t", 800)})
	if got := l.promptSize(); got != 1200 {
		t.Fatalf("measured size plus growth: %d", got)
	}
}

// Optional fields sent as null mean "not given"; required ones stay for the
// tool server to refuse.
func TestDropNullOptionals(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","required":["query"],"properties":{"query":{"type":"string"},"limit":{"type":"integer"}}}`)
	if got := dropNullOptionals(schema, json.RawMessage(`{"query":"pizza","limit":null}`)); string(got) != `{"query":"pizza"}` {
		t.Fatalf("optional null: %s", got)
	}
	if got := dropNullOptionals(schema, json.RawMessage(`{"query":null}`)); string(got) != `{"query":null}` {
		t.Fatalf("a required null must stay: %s", got)
	}
	if in := json.RawMessage(`{"query": "x"}`); string(dropNullOptionals(schema, in)) != string(in) {
		t.Fatal("arguments without nulls are passed through untouched")
	}
}
