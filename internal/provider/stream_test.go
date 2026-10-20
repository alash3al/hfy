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
