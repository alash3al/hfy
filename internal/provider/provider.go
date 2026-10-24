// Package provider is a minimal OpenAI-compatible client: streaming chat
// completions with tool calls, and embeddings. Nothing here is specific to a
// model or vendor.
package provider

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/alash3al/hfy/internal/metrics"
	"github.com/alash3al/hfy/internal/types"
)

type Client struct {
	Name    string
	BaseURL string
	APIKey  string
	Timeout time.Duration // longest silence: until the response starts, then between chunks
	HTTP    *http.Client
}

func New(name, baseURL, apiKey string, timeout time.Duration) *Client {
	return &Client{Name: name, BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey, Timeout: timeout, HTTP: &http.Client{}}
}

// Adapter is a model backend: the OpenAI-compatible Client, or the
// in-process Mock. The agent picks one per configured provider.
type Adapter interface {
	Chat(ctx context.Context, req Request, o Opts) (*Response, error)
	Embed(ctx context.Context, model string, inputs []string, rec func(Record) int64) ([][]float32, error)
}

var _ Adapter = (*Client)(nil)

type Part struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

type ImageURL struct {
	URL string `json:"url"`
}

type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

type Message struct {
	Role       string // system | user | assistant | tool
	Content    string
	Parts      []Part // user messages with images; replaces Content
	ToolCalls  []ToolCall
	ToolCallID string
}

type ToolDef struct {
	Name        string
	Description string
	Parameters  json.RawMessage // JSON Schema object
}

type Request struct {
	Model       string
	Messages    []Message
	Tools       []ToolDef
	Temperature *float64
	TopP        *float64
}

type Response struct {
	Content      string
	Reasoning    string
	ToolCalls    []ToolCall
	Usage        types.Usage
	PromptTokens int64
	FinishReason string
	RequestID    int64 // outbound_requests row of the successful attempt
}

// Record is one outbound HTTP attempt, for outbound_requests.
type Record struct {
	URL       string
	Request   json.RawMessage
	Response  json.RawMessage
	Status    int
	LatencyMS int
	Attempt   int
	Error     string
}

// Opts carries per-call hooks.
type Opts struct {
	MaxAttempts int
	Backoff     time.Duration // first retry delay, doubled each time (default 1s)
	OnText      func(string)
	OnReasoning func(string)
	// OnRetry runs before an attempt that follows a failed one: what the
	// failed attempt streamed through OnText/OnReasoning is void, since the
	// answer starts over.
	OnRetry func()
	Record  func(Record) int64
}

// APIError is a non-2xx answer from the provider.
type APIError struct {
	Status     int
	Body       string
	RetryAfter time.Duration // the provider's Retry-After, if any
}

func (e *APIError) Error() string { return fmt.Sprintf("provider returned %d: %s", e.Status, e.Body) }

func (e *APIError) retryable() bool { return e.Status == 429 || e.Status >= 500 }

// Chat runs one chat completion, retrying transient failures.
func (c *Client) Chat(ctx context.Context, req Request, o Opts) (*Response, error) {
	attempts := max(o.MaxAttempts, 1)
	backoff := o.Backoff
	if backoff == 0 {
		backoff = time.Second
	}
	body, err := json.Marshal(buildBody(req))
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		resp, err := c.chatOnce(ctx, body, attempt, o)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if apiErr, ok := errors.AsType[*APIError](err); ok && !apiErr.retryable() {
			return nil, err
		}
		if attempt < attempts {
			wait := backoff << (attempt - 1)
			if apiErr, ok := errors.AsType[*APIError](err); ok && apiErr.RetryAfter > 0 {
				wait = apiErr.RetryAfter // the provider knows when it will take more
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
			if o.OnRetry != nil {
				o.OnRetry()
			}
		}
	}
	return nil, fmt.Errorf("after %d attempts: %w", attempts, lastErr)
}

// errIdle ends a request whose provider went silent for longer than Timeout.
var errIdle = errors.New("provider idle timeout")

// idleReader re-arms the idle timer whenever data arrives.
type idleReader struct {
	r io.Reader
	t *time.Timer
	d time.Duration
}

func (r idleReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.t.Reset(r.d)
	}
	return n, err
}

// post makes one POST attempt: idle timeout, headers, status check, metrics
// and the trace row (rec arrives with URL, Request and Attempt set). read
// consumes a 2xx body and returns what the trace keeps of it. It returns the
// trace row id (0 without a recorder).
func (c *Client) post(ctx context.Context, kind string, rec Record, body []byte, record func(Record) int64,
	read func(http.Header, io.Reader) (json.RawMessage, error)) (id int64, err error) {
	// The timeout bounds silence, not length: an answer that keeps streaming
	// is never cut off.
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var idle *time.Timer
	if c.Timeout > 0 {
		idle = time.AfterFunc(c.Timeout, func() { cancel(errIdle) })
		defer idle.Stop()
	}
	start := time.Now()
	defer func() {
		if err != nil && errors.Is(context.Cause(ctx), errIdle) {
			err = fmt.Errorf("no data from the provider for %s: %w", c.Timeout, err)
		}
		if err != nil {
			rec.Error = redactSigned(err.Error()) // provider errors may echo signed URLs
		}
		rec.LatencyMS = int(time.Since(start).Milliseconds())
		metrics.ProviderRequests.WithLabelValues(c.Name, kind, metrics.Outcome(err != nil)).Inc()
		metrics.Since(metrics.ProviderDuration.WithLabelValues(c.Name, kind), start)
		if record != nil {
			id = record(rec)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rec.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	rec.Status = resp.StatusCode
	var r io.Reader = resp.Body
	if idle != nil {
		r = idleReader{r: resp.Body, t: idle, d: c.Timeout}
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(r, 8<<10))
		return 0, &APIError{Status: resp.StatusCode, Body: redactSigned(strings.TrimSpace(string(b))),
			RetryAfter: retryAfter(resp.Header.Get("Retry-After"), time.Now())}
	}
	rec.Response, err = read(resp.Header, r)
	return 0, err
}

// maxRetryAfter caps how long a provider's Retry-After may hold a turn.
const maxRetryAfter = time.Minute

// retryAfter reads a Retry-After header: seconds or an HTTP date (RFC 9110).
func retryAfter(h string, now time.Time) time.Duration {
	var d time.Duration
	if secs, err := strconv.Atoi(strings.TrimSpace(h)); err == nil {
		d = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(h); err == nil {
		d = t.Sub(now)
	}
	return min(max(d, 0), maxRetryAfter)
}

func (c *Client) chatOnce(ctx context.Context, body []byte, attempt int, o Opts) (*Response, error) {
	var resp *Response
	rec := Record{URL: c.BaseURL + "/chat/completions", Request: redactBody(body), Attempt: attempt}
	id, err := c.post(ctx, "chat", rec, body, o.Record, func(h http.Header, r io.Reader) (json.RawMessage, error) {
		var err error
		if strings.HasPrefix(h.Get("Content-Type"), "text/event-stream") {
			resp, err = readStream(r, o)
		} else {
			resp, err = readJSON(r, o)
		}
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{ // the assembled answer, not the raw stream
			"content": resp.Content, "reasoning": resp.Reasoning, "tool_calls": resp.ToolCalls,
			"usage": resp.Usage, "prompt_tokens": resp.PromptTokens, "finish_reason": resp.FinishReason,
		})
	})
	if err != nil {
		return nil, err
	}
	resp.RequestID = id
	return resp, nil
}

// The wire* types are the request body as OpenAI-compatible servers read it
// (answers name a call's function the same way: wireFunc).
type wireRequest struct {
	Model         string        `json:"model"`
	Messages      []wireMessage `json:"messages"`
	Stream        bool          `json:"stream"`
	StreamOptions struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	Tools       []wireTool `json:"tools,omitempty"`
	Temperature *float64   `json:"temperature,omitempty"`
	TopP        *float64   `json:"top_p,omitempty"`
}

type wireMessage struct {
	Role       string     `json:"role"`
	Content    any        `json:"content"` // the text, the []Part, or null
	ToolCalls  []wireCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type wireCall struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Function wireFunc `json:"function"`
}

type wireFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireTool struct {
	Type     string       `json:"type"`
	Function wireToolFunc `json:"function"`
}

type wireToolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

func buildBody(r Request) wireRequest {
	b := wireRequest{Model: r.Model, Messages: make([]wireMessage, len(r.Messages)), Stream: true,
		Temperature: r.Temperature, TopP: r.TopP}
	b.StreamOptions.IncludeUsage = true
	for i, m := range r.Messages {
		wm := wireMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		switch {
		case len(m.Parts) > 0:
			wm.Content = m.Parts
		case m.Role == "assistant" && len(m.ToolCalls) > 0 && m.Content == "":
			wm.Content = nil
		}
		for _, tc := range m.ToolCalls {
			wm.ToolCalls = append(wm.ToolCalls, wireCall{ID: tc.ID, Type: "function",
				Function: wireFunc{Name: tc.Name, Arguments: cmp.Or(tc.Arguments, "{}")}})
		}
		b.Messages[i] = wm
	}
	for _, t := range r.Tools {
		params := t.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		b.Tools = append(b.Tools, wireTool{Type: "function",
			Function: wireToolFunc{Name: t.Name, Description: t.Description, Parameters: params}})
	}
	return b
}

// signedURLRe finds a signature query parameter wherever it appears in a
// string: from it to the end of its token is the credential, and redactSigned
// drops it. Only real query params match, so ?design=1 stays.
var signedURLRe = regexp.MustCompile(`[?&](X-Amz-[A-Za-z-]+|X-Goog-[A-Za-z-]+|Signature|sig)=`)

// redactSigned removes the credentials of any signed URL in s: everything
// from a signature query parameter to the end of its token.
func redactSigned(s string) string {
	for {
		loc := signedURLRe.FindStringIndex(s)
		if loc == nil {
			return s
		}
		rest := s[loc[0]:]
		s = s[:loc[0]]
		if e := strings.IndexAny(rest, " \t\n\"'"); e > 0 {
			s += rest[e:] // the token ends at the next space or quote
		}
	}
}

// redactBody replaces inline base64 images and the credentials of signed
// URLs with placeholders before the request is stored in outbound_requests.
// Headers are never part of it.
func redactBody(body []byte) json.RawMessage {
	var v any
	if json.Unmarshal(body, &v) != nil {
		return nil
	}
	var walk func(any) any
	walk = func(x any) any {
		switch t := x.(type) {
		case map[string]any:
			for k, e := range t {
				t[k] = walk(e)
			}
		case []any:
			for i, e := range t {
				t[i] = walk(e)
			}
		case string:
			if strings.HasPrefix(t, "data:") && strings.Contains(t, ";base64,") {
				return "[inline attachment]"
			}
			return redactSigned(t)
		}
		return x
	}
	out, _ := json.Marshal(walk(v))
	return out
}

// delta is what a choice carries: a streamed delta, or the whole message of
// an answer that is not streamed.
type delta struct {
	Content          *string `json:"content"`
	ReasoningContent *string `json:"reasoning_content"`
	Reasoning        *string `json:"reasoning"`
	ToolCalls        []struct {
		Index    int      `json:"index"` // streamed calls only
		ID       string   `json:"id"`
		Function wireFunc `json:"function"`
	} `json:"tool_calls"`
}

type chunk struct {
	Error   json.RawMessage `json:"error"` // some servers report a failure mid-stream
	Choices []struct {
		Delta        delta   `json:"delta"`
		Message      *delta  `json:"message"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		Details          *struct {
			Cached int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

type assembler struct {
	resp  Response
	calls map[int]*ToolCall
	o     Opts
}

func (a *assembler) add(c chunk) {
	if c.Usage != nil {
		a.resp.PromptTokens = c.Usage.PromptTokens
		a.resp.Usage = types.Usage{Input: c.Usage.PromptTokens, Output: c.Usage.CompletionTokens}
		if c.Usage.Details != nil {
			a.resp.Usage.Cached = c.Usage.Details.Cached
		}
	}
	for _, ch := range c.Choices {
		d := ch.Delta
		if d.Content != nil && *d.Content != "" {
			a.resp.Content += *d.Content
			if a.o.OnText != nil {
				a.o.OnText(*d.Content)
			}
		}
		for _, r := range []*string{d.ReasoningContent, d.Reasoning} {
			if r != nil && *r != "" {
				a.resp.Reasoning += *r
				if a.o.OnReasoning != nil {
					a.o.OnReasoning(*r)
				}
			}
		}
		for _, tc := range d.ToolCalls {
			cur := a.calls[tc.Index]
			if cur == nil {
				cur = &ToolCall{}
				a.calls[tc.Index] = cur
			}
			if tc.ID != "" {
				cur.ID = tc.ID
			}
			cur.Name += tc.Function.Name
			cur.Arguments += tc.Function.Arguments
		}
		if m := ch.Message; m != nil { // non-streaming answer
			if m.Content != nil {
				a.resp.Content += *m.Content
				if a.o.OnText != nil && *m.Content != "" {
					a.o.OnText(*m.Content)
				}
			}
			for _, r := range []*string{m.ReasoningContent, m.Reasoning} { // servers differ, as in deltas
				if r != nil {
					a.resp.Reasoning += *r
				}
			}
			for i, tc := range m.ToolCalls {
				a.calls[i] = &ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments}
			}
		}
		if ch.FinishReason != nil {
			a.resp.FinishReason = *ch.FinishReason
		}
	}
}

func (a *assembler) done() *Response {
	idx := make([]int, 0, len(a.calls))
	for i := range a.calls {
		idx = append(idx, i)
	}
	slices.Sort(idx) // indices need not start at 0 or be contiguous
	for _, i := range idx {
		if tc := a.calls[i]; tc != nil && tc.Name != "" {
			if tc.ID == "" {
				tc.ID = fmt.Sprintf("call_%d", i)
			}
			a.resp.ToolCalls = append(a.resp.ToolCalls, *tc)
		}
	}
	return &a.resp
}

// errCutShort is a stream that ended before the answer did: neither [DONE]
// nor a finish_reason arrived, so what came is not the whole answer.
var errCutShort = errors.New("the stream ended before the answer finished")

func readStream(r io.Reader, o Opts) (*Response, error) {
	a := &assembler{calls: map[int]*ToolCall{}, o: o}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	done := false
	for sc.Scan() {
		line := sc.Text()
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			done = true
			break
		}
		var c chunk
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			return nil, fmt.Errorf("bad stream chunk: %w", err)
		}
		if err := streamError(c.Error); err != nil {
			return nil, err
		}
		a.add(c)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !done && a.resp.FinishReason == "" {
		return nil, errCutShort
	}
	return a.done(), nil
}

func streamError(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Message != "" {
		return fmt.Errorf("provider error in stream: %s", e.Message)
	}
	return fmt.Errorf("provider error in stream: %s", raw)
}

func readJSON(r io.Reader, o Opts) (*Response, error) {
	var c chunk
	if err := json.NewDecoder(r).Decode(&c); err != nil {
		return nil, fmt.Errorf("bad response: %w", err)
	}
	a := &assembler{calls: map[int]*ToolCall{}, o: o}
	a.add(c)
	return a.done(), nil
}

// Embed returns one vector per input. One attempt: its callers retry (the
// embed job) or degrade (conversation_search).
func (c *Client) Embed(ctx context.Context, model string, inputs []string, rec func(Record) int64) ([][]float32, error) {
	body, err := json.Marshal(map[string]any{"model": model, "input": inputs})
	if err != nil {
		return nil, err
	}
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	trace := Record{URL: c.BaseURL + "/embeddings", Request: body, Attempt: 1}
	_, err = c.post(ctx, "embedding", trace, body, rec, func(_ http.Header, r io.Reader) (json.RawMessage, error) {
		if err := json.NewDecoder(io.LimitReader(r, 64<<20)).Decode(&out); err != nil {
			return nil, fmt.Errorf("bad response: %w", err)
		}
		return fmt.Appendf(nil, `{"count":%d}`, len(out.Data)), nil // vectors are not stored in the trace
	})
	if err != nil {
		return nil, err
	}
	if len(out.Data) != len(inputs) {
		return nil, fmt.Errorf("embeddings: got %d vectors for %d inputs", len(out.Data), len(inputs))
	}
	vecs := make([][]float32, len(inputs))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(vecs) {
			return nil, errors.New("embeddings: bad index")
		}
		vecs[d.Index] = d.Embedding
	}
	return vecs, nil
}
