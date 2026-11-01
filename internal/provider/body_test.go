package provider

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

// goldenRequest exercises every core field a request body carries today.
func goldenRequest() Request {
	temp, topP := 0.2, 0.9
	return Request{Model: "m", Temperature: &temp, TopP: &topP,
		Messages: []Message{
			{Role: "system", Content: "Be brief."},
			{Role: "user", Content: "hi"},
			{Role: "user", Parts: []Part{{Type: "text", Text: "see"}, {Type: "image_url", ImageURL: &ImageURL{URL: "https://cdn.example.com/a.png"}}}},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "files__read", Arguments: `{"path":"/a"}`}}},
			{Role: "tool", ToolCallID: "c1", Content: "ok"},
		},
		Tools: []ToolDef{{Name: "files__read", Description: "Read a file",
			Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)}}}
}

// Requests that set no model controls keep their exact bytes: the golden
// line was captured before the controls existed.
func TestRequestBodyGoldenWithoutControls(t *testing.T) {
	const want = `{"model":"m","messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"hi"},` +
		`{"role":"user","content":[{"type":"text","text":"see"},{"type":"image_url","image_url":{"url":"https://cdn.example.com/a.png"}}]},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"files__read","arguments":"{\"path\":\"/a\"}"}}]},` +
		`{"role":"tool","content":"ok","tool_call_id":"c1"}],"stream":true,"stream_options":{"include_usage":true},` +
		`"tools":[{"type":"function","function":{"name":"files__read","description":"Read a file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}],` +
		`"temperature":0.2,"top_p":0.9}`
	got, err := requestBody(goldenRequest(), "")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("body changed:\n got %s\nwant %s", got, want)
	}
}

func bodyMap(t *testing.T, r Request, maxParam string) map[string]any {
	t.Helper()
	b, err := requestBody(r, maxParam)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// Each control is sent only when set, under the field name its provider
// reads; call_timeout is hfy's own and never sent.
func TestRequestControlsEncoding(t *testing.T) {
	for _, c := range []struct {
		name     string
		ctl      Controls
		maxParam string
		want     map[string]any // present with this value
		absent   []string
	}{
		{"unset", Controls{}, "", nil, []string{"reasoning_effort", "max_tokens", "max_completion_tokens"}},
		{"unset, completion param", Controls{}, "max_completion_tokens", nil, []string{"reasoning_effort", "max_tokens", "max_completion_tokens"}},
		{"effort", Controls{ReasoningEffort: "none"}, "", map[string]any{"reasoning_effort": "none"}, []string{"max_tokens", "max_completion_tokens"}},
		{"cap, default param", Controls{MaxOutputTokens: 100}, "", map[string]any{"max_tokens": 100.0}, []string{"reasoning_effort", "max_completion_tokens"}},
		{"cap, max_tokens", Controls{MaxOutputTokens: 100}, "max_tokens", map[string]any{"max_tokens": 100.0}, []string{"max_completion_tokens"}},
		{"cap, max_completion_tokens", Controls{MaxOutputTokens: 100}, "max_completion_tokens", map[string]any{"max_completion_tokens": 100.0}, []string{"max_tokens"}},
		{"effort and cap", Controls{ReasoningEffort: "high", MaxOutputTokens: 50}, "max_completion_tokens",
			map[string]any{"reasoning_effort": "high", "max_completion_tokens": 50.0}, []string{"max_tokens"}},
		{"call timeout is not sent", Controls{CallTimeout: 30 * time.Second}, "", nil, []string{"call_timeout", "max_tokens", "reasoning_effort"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := goldenRequest()
			r.Controls = c.ctl
			m := bodyMap(t, r, c.maxParam)
			for k, want := range c.want {
				if m[k] != want {
					t.Errorf("%s = %v, want %v", k, m[k], want)
				}
			}
			for _, k := range c.absent {
				if _, ok := m[k]; ok {
					t.Errorf("%s must not be sent: %v", k, m[k])
				}
			}
		})
	}
}

func TestExtraMergesAfterCoreFields(t *testing.T) {
	r := goldenRequest()
	r.Controls.Extra = map[string]any{
		"cache_control": map[string]any{"type": "ephemeral"},
		"reasoning":     map[string]any{"effort": "high", "exclude": true},
	}
	m := bodyMap(t, r, "")
	if got := m["cache_control"]; got == nil || got.(map[string]any)["type"] != "ephemeral" {
		t.Errorf("cache_control = %v", got)
	}
	if got := m["reasoning"]; got == nil || got.(map[string]any)["effort"] != "high" {
		t.Errorf("reasoning = %v", got)
	}
	if m["model"] != "m" || len(m["messages"].([]any)) != 5 || m["temperature"] != 0.2 {
		t.Errorf("core fields changed: %v", m)
	}
}

// extra can never replace a field hfy sends, nor set one of its envelope keys
// even when the request does not carry it (tools with none offered).
func TestExtraCannotReplaceCoreFields(t *testing.T) {
	for _, c := range []struct {
		name  string
		r     Request
		param string
	}{
		{"model", withExtra(goldenRequest(), map[string]any{"model": "x"}), ""},
		{"messages", withExtra(goldenRequest(), map[string]any{"messages": []any{}}), ""},
		{"stream", withExtra(goldenRequest(), map[string]any{"stream": false}), ""},
		{"stream_options", withExtra(goldenRequest(), map[string]any{"stream_options": map[string]any{}}), ""},
		{"n", withExtra(goldenRequest(), map[string]any{"n": 2}), ""},
		{"Tools", withExtra(goldenRequest(), map[string]any{"Tools": []any{}}), ""},
		{"MODEL", withExtra(goldenRequest(), map[string]any{"MODEL": "x"}), ""},
		{"Stream", withExtra(goldenRequest(), map[string]any{"Stream": true}), ""},
		{"tools without tools", withExtra(Request{Model: "m", Messages: goldenRequest().Messages}, map[string]any{"tools": []any{}}), ""},
		{"temperature", withExtra(goldenRequest(), map[string]any{"temperature": 1}), ""},
		{"reasoning_effort", withEffort(goldenRequest(), "low", map[string]any{"reasoning_effort": "high"}), ""},
		{"max_tokens", withCap(goldenRequest(), 10, map[string]any{"max_tokens": 5}), "max_tokens"},
		{"max_completion_tokens", withCap(goldenRequest(), 10, map[string]any{"max_completion_tokens": 5}), "max_completion_tokens"},
		{"MAX_TOKENS", withCap(goldenRequest(), 10, map[string]any{"MAX_TOKENS": 5}), "max_tokens"},
	} {
		t.Run(c.name, func(t *testing.T) {
			key := strings.TrimPrefix(c.name, "tools without ")
			if _, err := requestBody(c.r, c.param); err == nil || !strings.Contains(err.Error(), "extra.") || !strings.Contains(err.Error(), key) {
				t.Fatalf("want an error naming extra.%s, got %v", key, err)
			}
		})
	}
}

func TestExtraKeysDifferingOnlyInCaseCollide(t *testing.T) {
	if _, err := requestBody(withExtra(goldenRequest(), map[string]any{"Foo": 1, "foo": 2}), ""); err == nil || !strings.Contains(err.Error(), "extra.foo") {
		t.Fatalf("want an error naming extra.foo, got %v", err)
	}
}

func withExtra(r Request, extra map[string]any) Request {
	r.Controls.Extra = extra
	return r
}

func withEffort(r Request, effort string, extra map[string]any) Request {
	r.Controls = Controls{ReasoningEffort: effort, Extra: extra}
	return r
}

func withCap(r Request, n int, extra map[string]any) Request {
	r.Controls = Controls{MaxOutputTokens: n, Extra: extra}
	return r
}

func TestExtraValueMustSerialise(t *testing.T) {
	r := goldenRequest()
	r.Controls.Extra = map[string]any{"bad": math.Inf(1)}
	if _, err := requestBody(r, ""); err == nil || !strings.Contains(err.Error(), "extra.bad") {
		t.Fatalf("want an error naming extra.bad, got %v", err)
	}
}
