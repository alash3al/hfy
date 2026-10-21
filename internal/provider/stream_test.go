package provider

import (
	"strings"
	"testing"
)

func TestStreamToolCallIndicesNeedNotBeContiguous(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"a","function":{"name":"x__one","arguments":"{}"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":3,"id":"b","function":{"name":"x__two","arguments":"{}"}}]}}]}

data: [DONE]
`
	resp, err := readStream(strings.NewReader(sse), Opts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 2 || resp.ToolCalls[0].ID != "a" || resp.ToolCalls[1].ID != "b" {
		t.Fatalf("calls: %+v", resp.ToolCalls)
	}
}

// Servers name reasoning differently (DeepSeek: reasoning_content; Ollama and
// vLLM: reasoning), streamed or not.
func TestNonStreamReadsEitherReasoningField(t *testing.T) {
	for _, field := range []string{"reasoning_content", "reasoning"} {
		body := `{"choices":[{"message":{"content":"hi","` + field + `":"thought"},"finish_reason":"stop"}]}`
		resp, err := readJSON(strings.NewReader(body), Opts{})
		if err != nil || resp.Content != "hi" || resp.Reasoning != "thought" {
			t.Fatalf("%s: %+v %v", field, resp, err)
		}
	}
}
