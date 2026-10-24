package provider

import (
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"slices"
	"strings"
	"time"

	"github.com/alash3al/hfy/internal/metrics"
	"github.com/alash3al/hfy/internal/types"
)

type MockOptions struct {
	Reply, Reasoning string
	Delay            time.Duration
	ErrorStatus      int
	EmbedDimensions  int
	ToolName         string
	ToolArguments    string
}

type Mock struct {
	Name string
	Opts MockOptions
}

func NewMock(name string, o MockOptions) *Mock { return &Mock{Name: name, Opts: o} }

var _ Adapter = (*Mock)(nil)

func mockUsage(req Request, content string) (types.Usage, int64) {
	chars := 0
	for _, msg := range req.Messages {
		chars += len(msg.Content)
	}
	prompt := int64(max(1, chars/4))
	output := int64(max(1, len([]rune(content))/4))
	return types.Usage{Input: prompt, Output: output}, prompt
}

func (m *Mock) Chat(ctx context.Context, req Request, o Opts) (*Response, error) {
	start := time.Now()
	body, _ := json.Marshal(buildBody(req))
	fail := func(err error) (*Response, error) {
		metrics.ProviderRequests.WithLabelValues(m.Name, "chat", metrics.Outcome(true)).Inc()
		metrics.Since(metrics.ProviderDuration.WithLabelValues(m.Name, "chat"), start)
		if o.Record != nil {
			rec := Record{URL: "mock://" + m.Name, Request: redactBody(body), Attempt: 1, Error: err.Error()}
			if apiErr, ok := errors.AsType[*APIError](err); ok {
				rec.Status = apiErr.Status
			}
			o.Record(rec)
		}
		return nil, err
	}
	if m.Opts.ErrorStatus != 0 {
		return fail(&APIError{Status: m.Opts.ErrorStatus, Body: "mock error"})
	}
	if m.Opts.Delay > 0 {
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-time.After(m.Opts.Delay):
		}
	}

	var resp *Response
	toolOffered := false
	if m.Opts.ToolName != "" {
		for _, t := range req.Tools {
			if t.Name == m.Opts.ToolName {
				toolOffered = true
				break
			}
		}
	}
	hasToolMsg := false
	for _, msg := range req.Messages {
		if msg.Role == "tool" {
			hasToolMsg = true
			break
		}
	}
	if toolOffered && !hasToolMsg {
		usage, prompt := mockUsage(req, "")
		args := m.Opts.ToolArguments
		if args == "" {
			args = "{}"
		}
		resp = &Response{
			ToolCalls:    []ToolCall{{ID: "mock_call_1", Name: m.Opts.ToolName, Arguments: args}},
			Usage:        usage,
			PromptTokens: prompt,
			FinishReason: "tool_calls",
		}
	} else {
		content := m.Opts.Reply
		if content == "" {
			last := ""
			for _, msg := range slices.Backward(req.Messages) {
				if msg.Role == "user" {
					last = strings.TrimSpace(msg.Content)
					break
				}
			}
			if last == "" {
				content = "Mock reply"
			} else {
				content = "Mock reply to: " + last
			}
		}
		if m.Opts.Reasoning != "" && o.OnReasoning != nil {
			o.OnReasoning(m.Opts.Reasoning)
		}
		if o.OnText != nil {
			runes := []rune(content)
			for i := 0; i < len(runes); i += 24 {
				end := min(i+24, len(runes))
				o.OnText(string(runes[i:end]))
			}
		}
		usage, prompt := mockUsage(req, content)
		resp = &Response{
			Content:      content,
			Reasoning:    m.Opts.Reasoning,
			Usage:        usage,
			PromptTokens: prompt,
			FinishReason: "stop",
		}
	}

	metrics.ProviderRequests.WithLabelValues(m.Name, "chat", metrics.Outcome(false)).Inc()
	metrics.Since(metrics.ProviderDuration.WithLabelValues(m.Name, "chat"), start)
	if o.Record != nil {
		respJSON, _ := json.Marshal(map[string]any{
			"content": resp.Content, "tool_calls": resp.ToolCalls, "usage": resp.Usage,
		})
		resp.RequestID = o.Record(Record{
			URL: "mock://" + m.Name, Request: redactBody(body), Response: respJSON, Attempt: 1,
		})
	}
	return resp, nil
}

func (m *Mock) Embed(ctx context.Context, model string, inputs []string, rec func(Record) int64) ([][]float32, error) {
	start := time.Now()
	reqJSON, _ := json.Marshal(map[string]any{"model": model, "inputs": len(inputs)})
	if m.Opts.ErrorStatus != 0 {
		err := &APIError{Status: m.Opts.ErrorStatus, Body: "mock error"}
		metrics.ProviderRequests.WithLabelValues(m.Name, "embedding", metrics.Outcome(true)).Inc()
		metrics.Since(metrics.ProviderDuration.WithLabelValues(m.Name, "embedding"), start)
		if rec != nil {
			rec(Record{URL: "mock://" + m.Name, Request: reqJSON, Attempt: 1, Status: m.Opts.ErrorStatus, Error: err.Error()})
		}
		return nil, err
	}
	dims := m.Opts.EmbedDimensions
	if dims == 0 {
		dims = 384
	}
	vecs := make([][]float32, len(inputs))
	for i, input := range inputs {
		vec := make([]float32, dims)
		for word := range strings.FieldsSeq(strings.ToLower(input)) {
			h := fnv.New32a()
			_, _ = h.Write([]byte(word))
			vec[int(h.Sum32()%uint32(dims))]++
		}
		vecs[i] = vec
	}
	metrics.ProviderRequests.WithLabelValues(m.Name, "embedding", metrics.Outcome(false)).Inc()
	metrics.Since(metrics.ProviderDuration.WithLabelValues(m.Name, "embedding"), start)
	if rec != nil {
		respJSON, _ := json.Marshal(map[string]any{"count": len(inputs)})
		rec(Record{URL: "mock://" + m.Name, Request: reqJSON, Response: respJSON, Attempt: 1})
	}
	return vecs, nil
}
