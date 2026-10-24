package provider_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/alash3al/hfy/internal/provider"
)

func TestMockChatStreamsReply(t *testing.T) {
	reply := "hello mock world, this is a longer reply to chunk"
	m := provider.NewMock("mock", provider.MockOptions{Reply: reply, Reasoning: "thinking"})
	var text, reasoning strings.Builder
	resp, err := m.Chat(context.Background(), provider.Request{Model: "m", Messages: []provider.Message{{Role: "user", Content: "hi there"}}}, provider.Opts{
		OnText:      func(s string) { text.WriteString(s) },
		OnReasoning: func(s string) { reasoning.WriteString(s) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if text.String() != reply || resp.Content != reply {
		t.Fatalf("streamed %q resp %q want %q", text.String(), resp.Content, reply)
	}
	if reasoning.String() != "thinking" || resp.Reasoning != "thinking" {
		t.Fatalf("reasoning %q %q", reasoning.String(), resp.Reasoning)
	}
	if resp.PromptTokens == 0 || resp.Usage.Input == 0 || resp.Usage.Output == 0 {
		t.Fatalf("usage %+v prompt %d", resp.Usage, resp.PromptTokens)
	}
	if resp.FinishReason != "stop" {
		t.Fatalf("finish %q", resp.FinishReason)
	}
}

func TestMockChatEchoDefault(t *testing.T) {
	m := provider.NewMock("mock", provider.MockOptions{})
	resp, err := m.Chat(context.Background(), provider.Request{Messages: []provider.Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "ok"},
		{Role: "user", Content: "  second question  "},
	}}, provider.Opts{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "Mock reply to: second question" {
		t.Fatalf("content %q", resp.Content)
	}
}

func TestMockChatToolCall(t *testing.T) {
	m := provider.NewMock("mock", provider.MockOptions{ToolName: "read_file", ToolArguments: `{"path":"/a"}`})
	tools := []provider.ToolDef{{Name: "read_file"}}
	resp, err := m.Chat(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: "read it"}},
		Tools:    tools,
	}, provider.Opts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "mock_call_1" || resp.ToolCalls[0].Name != "read_file" || resp.ToolCalls[0].Arguments != `{"path":"/a"}` || resp.FinishReason != "tool_calls" {
		t.Fatalf("resp %+v", resp)
	}

	resp, err = m.Chat(context.Background(), provider.Request{
		Messages: []provider.Message{
			{Role: "user", Content: "read it"},
			{Role: "tool", Content: "file contents", ToolCallID: "mock_call_1"},
		},
		Tools: tools,
	}, provider.Opts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 0 || resp.FinishReason != "stop" || resp.Content == "" {
		t.Fatalf("after tool result must reply with text: %+v", resp)
	}
}

func TestMockChatErrorStatus(t *testing.T) {
	m := provider.NewMock("mock", provider.MockOptions{ErrorStatus: 429})
	_, err := m.Chat(context.Background(), provider.Request{}, provider.Opts{})
	var apiErr *provider.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 429 {
		t.Fatalf("err %v", err)
	}
}

func TestMockEmbed(t *testing.T) {
	m := provider.NewMock("mock", provider.MockOptions{EmbedDimensions: 16})
	vecs, err := m.Embed(context.Background(), "e", []string{"hello world", "hello world", "goodbye world"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 3 || len(vecs[0]) != 16 {
		t.Fatalf("vecs %v", vecs)
	}
	if !slices.Equal(vecs[0], vecs[1]) {
		t.Fatalf("same input must give identical vector: %v vs %v", vecs[0], vecs[1])
	}
	if slices.Equal(vecs[0], vecs[2]) {
		t.Fatalf("different inputs must differ: %v", vecs[0])
	}
}

func TestMockRecordPropagatesRequestID(t *testing.T) {
	m := provider.NewMock("mock", provider.MockOptions{Reply: "hi"})
	var recs []provider.Record
	resp, err := m.Chat(context.Background(), provider.Request{Messages: []provider.Message{{Role: "user", Content: "hi"}}}, provider.Opts{
		Record: func(r provider.Record) int64 { recs = append(recs, r); return 42 },
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.RequestID != 42 || len(recs) != 1 || recs[0].URL != "mock://mock" || recs[0].Attempt != 1 {
		t.Fatalf("resp %+v recs %+v", resp, recs)
	}
}
