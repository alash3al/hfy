// Package fakeprov is a scriptable OpenAI-compatible server for tests.
package fakeprov

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Step is one scripted chat answer.
type Step struct {
	Status    int // default 200
	Content   string
	Reasoning string
	Calls     []Call
	Prompt    int64 // reported prompt tokens (default 10)
	Output    int64 // default 5
	NoStream  bool  // answer with a plain JSON body
	Delay     chan struct{}
}

type Call struct {
	ID, Name, Args string
}

// Server records every chat request body and answers from the script; when
// the script runs out it answers with Default.
type Server struct {
	*httptest.Server
	mu       sync.Mutex
	script   []Step
	Requests []map[string]any
	Default  Step
	EmbedDim int
}

func New(t testing.TB, script ...Step) *Server {
	f := &Server{script: script, Default: Step{Content: "ok"}, EmbedDim: 3}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *Server) URL1() string { return f.URL + "/v1" }

func (f *Server) Push(s ...Step) {
	f.mu.Lock()
	f.script = append(f.script, s...)
	f.mu.Unlock()
}

func (f *Server) Reqs() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.Requests...)
}

func (f *Server) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/embeddings"):
		f.embeddings(w, r)
	case strings.HasSuffix(r.URL.Path, "/chat/completions"):
		f.chat(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *Server) chat(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.Requests = append(f.Requests, body)
	step := f.Default
	if len(f.script) > 0 {
		step, f.script = f.script[0], f.script[1:]
	}
	f.mu.Unlock()

	if step.Delay != nil {
		select {
		case <-step.Delay:
		case <-r.Context().Done():
			return
		}
	}
	if step.Status != 0 && step.Status != 200 {
		http.Error(w, "scripted failure", step.Status)
		return
	}
	prompt, out := step.Prompt, step.Output
	if prompt == 0 {
		prompt = 10
	}
	if out == 0 {
		out = 5
	}
	usage := map[string]any{"prompt_tokens": prompt, "completion_tokens": out}

	if step.NoStream {
		msg := map[string]any{"role": "assistant", "content": step.Content}
		var calls []any
		for _, c := range step.Calls {
			calls = append(calls, map[string]any{"id": c.ID, "type": "function",
				"function": map[string]any{"name": c.Name, "arguments": c.Args}})
		}
		if calls != nil {
			msg["tool_calls"] = calls
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": msg, "finish_reason": "stop"}}, "usage": usage})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	fl := w.(http.Flusher)
	send := func(delta map[string]any, finish any) {
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": delta, "finish_reason": finish}}})
		fmt.Fprintf(w, "data: %s\n\n", b)
		fl.Flush()
	}
	if step.Reasoning != "" {
		send(map[string]any{"reasoning_content": step.Reasoning}, nil)
	}
	for _, part := range split(step.Content, 3) {
		send(map[string]any{"content": part}, nil)
	}
	for i, c := range step.Calls {
		half := len(c.Args) / 2
		send(map[string]any{"tool_calls": []any{map[string]any{"index": i, "id": c.ID, "type": "function",
			"function": map[string]any{"name": c.Name, "arguments": c.Args[:half]}}}}, nil)
		send(map[string]any{"tool_calls": []any{map[string]any{"index": i,
			"function": map[string]any{"arguments": c.Args[half:]}}}}, nil)
	}
	send(map[string]any{}, "stop")
	ub, _ := json.Marshal(map[string]any{"choices": []any{}, "usage": usage})
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", ub)
	fl.Flush()
}

func split(s string, n int) []string {
	if s == "" {
		return nil
	}
	size := max(len(s)/n, 1)
	var out []string
	for len(s) > size {
		out = append(out, s[:size])
		s = s[size:]
	}
	return append(out, s)
}

// embeddings returns a deterministic bag-of-words vector per input, so similar
// texts are close.
func (f *Server) embeddings(w http.ResponseWriter, r *http.Request) {
	var req struct{ Input []string }
	json.NewDecoder(r.Body).Decode(&req)
	data := make([]any, len(req.Input))
	for i, in := range req.Input {
		v := make([]float32, f.EmbedDim)
		for _, word := range strings.Fields(strings.ToLower(in)) {
			h := fnv.New32a()
			h.Write([]byte(word))
			v[int(h.Sum32())%f.EmbedDim]++
		}
		data[i] = map[string]any{"index": i, "embedding": v}
	}
	json.NewEncoder(w).Encode(map[string]any{"data": data})
}
