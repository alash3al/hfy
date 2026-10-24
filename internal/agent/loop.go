package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/alash3al/hfy/internal/mcp"
	"github.com/alash3al/hfy/internal/mcp/builtins"
	"github.com/alash3al/hfy/internal/metrics"
	"github.com/alash3al/hfy/internal/provider"
	"github.com/alash3al/hfy/internal/svc"
	"github.com/alash3al/hfy/internal/types"
)

var (
	errStopped = errors.New("stopped by request")
	errTimeout = errors.New("turn timeout")
)

// toolSearchName is the full name of the built-in tool search.
const toolSearchName = builtins.Server + "." + builtins.ToolSearch

// askUserTimeout is how long hfy.ask_user waits for answers when the agent
// has no approval policy; with one, the agent's approval.timeout applies.
const askUserTimeout = 30 * time.Minute

// approvalTimeout is how long an approval request waits when the agent has no
// approval policy; with one, the agent's approval.timeout applies.
const approvalTimeout = 10 * time.Minute

// coreNoApproval are the built-in tool names that never wait for approval:
// they are pure or harness internals, so patterns like "hfy.*" never pause
// them. Sub-agent tools (hfy.<name>) stay guardable.
var coreNoApproval = map[string]bool{
	builtins.Now: true, builtins.MathCalc: true,
	builtins.ToolSearch: true, builtins.ConversationSearch: true,
}

const (
	repeatWarn  = 3       // the third identical call is refused with advice
	repeatAbort = 6       // the sixth ends the turn
	maxResult   = 100_000 // bytes of a tool result the model sees
	// maxStructured bounds a tool's structuredContent, stored with its result
	// for clients.
	maxStructured = 1 << 20
)

// Outcome is how a loop run ended (cancellation is returned as an error).
type Outcome int

const (
	Done Outcome = iota
	Paused
	Failed
)

type Result struct {
	Outcome Outcome
	Final   string
	Reason  string
}

// usageAcc is a mutex-guarded usage total shared with sub-agents.
type usageAcc struct {
	mu    sync.Mutex
	u     types.Usage
	calls int
}

func (a *usageAcc) add(u types.Usage) {
	a.mu.Lock()
	a.u.Add(u)
	a.calls++
	a.mu.Unlock()
}

func (a *usageAcc) snapshot() (types.Usage, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.u, a.calls
}

// TurnEnv is the per-turn context shared by the main loop and its sub-agents.
type TurnEnv struct {
	ConversationID int64
	TurnID         int64
	Scope          string
	Vars           map[string]string
	Total          *usageAcc // the whole turn, sub-agents included
	// Sent to tool servers on every call (mcp.MetaKey): public ids and who
	// started the turn, so tools can authorize the call themselves.
	ConversationUUID, TurnUUID string
	Actor                      mcp.ActorMeta
}

type loop struct {
	rt    *Runtime
	spec  *Spec
	sink  Sink
	env   *TurnEnv
	total *usageAcc // this loop's own usage (== env.Total for the main agent)

	system    string
	msgs      []provider.Message
	activated map[string]bool // full names unlocked by tool_search
	counts    map[string]int
	steps     int // steps already taken (includes earlier runs of a resumed turn)

	// The prompt's size in tokens when last measured, and len(msgs) then:
	// what was added since is estimated on top (see promptSize).
	lastPrompt int64
	measuredAt int
	// prepare may rebuild msgs before a model call (summarization).
	prepare func(ctx context.Context, l *loop) error
}

func newLoop(rt *Runtime, spec *Spec, sink Sink, env *TurnEnv) *loop {
	l := &loop{rt: rt, spec: spec, sink: sink, env: env,
		activated: map[string]bool{}, counts: map[string]int{}}
	if spec.Top {
		l.total = env.Total
	} else {
		l.total = &usageAcc{}
	}
	return l
}

// load sets the system prompt and messages the loop sends. It measures them
// at once (promptSize reads msgs from the last measurement on) and unlocks
// the tools that earlier tool_search results found.
func (l *loop) load(ctx context.Context, system string, msgs []provider.Message) {
	l.system, l.msgs = system, msgs
	l.measure(0)
	l.deriveActivated(ctx)
}

func (l *loop) addUsage(u types.Usage) {
	l.total.add(u)
	if l.total != l.env.Total {
		l.env.Total.add(u) // sub-agent usage counts toward the parent's turn
	}
}

// measure records the size of the prompt just sent: what the provider
// reported, or an estimate when it reports none (0).
func (l *loop) measure(reported int64) {
	l.lastPrompt, l.measuredAt = reported, len(l.msgs)
	if reported == 0 {
		l.lastPrompt = estimateTokens(l.system, l.msgs)
	}
}

// promptSize is the size the next request will have: the last measurement
// plus an estimate of the messages added since (replies, tool results).
func (l *loop) promptSize() int64 {
	return l.lastPrompt + estimateTokens("", l.msgs[l.measuredAt:])
}

func (l *loop) limitHit() string {
	lim := l.spec.Limits
	if lim.Steps > 0 && l.steps >= lim.Steps {
		return fmt.Sprintf("limit reached: %d steps", lim.Steps)
	}
	if lim.TotalTokens > 0 {
		u, _ := l.total.snapshot()
		if u.Total() >= lim.TotalTokens {
			return fmt.Sprintf("limit reached: %d tokens", lim.TotalTokens)
		}
	}
	return ""
}

func (l *loop) run(ctx context.Context) (Result, error) {
	for {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if why := l.limitHit(); why != "" {
			return Result{Outcome: Failed, Reason: why}, nil
		}
		if l.prepare != nil {
			if err := l.prepare(ctx, l); err != nil {
				return Result{}, err
			}
		}
		defs, lookup := l.tools(ctx)
		req := provider.Request{
			Model:       l.spec.Model,
			Messages:    append([]provider.Message{{Role: "system", Content: l.system}}, l.msgs...),
			Tools:       defs,
			Temperature: l.spec.Settings.Temperature, TopP: l.spec.Settings.TopP,
		}
		kind := types.OutboundModel
		if l.spec.Name == types.AgentSummarizer {
			kind = types.OutboundSummarizer
		}
		resp, err := l.spec.Client.Chat(ctx, req, provider.Opts{
			MaxAttempts: l.rt.Cfg.Queue.MaxAttempts,
			OnText:      l.sink.TextDelta, OnReasoning: l.sink.ReasoningDelta, OnRetry: l.sink.Restart,
			Record: l.rt.recorder(&l.env.TurnID, kind),
		})
		if err != nil {
			if ctx.Err() != nil {
				return Result{}, ctx.Err()
			}
			return Result{Outcome: Failed, Reason: "model call failed: " + err.Error()}, nil
		}
		l.steps++
		l.addUsage(resp.Usage)
		for kind, n := range map[string]int64{"input": resp.Usage.Input, "output": resp.Usage.Output, "cached": resp.Usage.Cached} {
			metrics.Tokens.WithLabelValues(l.spec.Name, kind).Add(float64(n))
		}
		l.measure(resp.PromptTokens)
		text := types.Clean(resp.Content)
		if err := l.sink.Step(ctx, StepOut{Reasoning: types.Clean(resp.Reasoning), Text: text,
			Usage: resp.Usage, Model: l.spec.Model, RequestID: resp.RequestID}); err != nil {
			return Result{}, err
		}
		asst := provider.Message{Role: "assistant", Content: text}
		calls := make([]provider.ToolCall, 0, len(resp.ToolCalls))
		for i, tc := range resp.ToolCalls {
			if tc.ID == "" {
				tc.ID = fmt.Sprintf("call_%d_%d", l.steps, i)
			}
			calls = append(calls, tc)
		}
		asst.ToolCalls = calls
		l.msgs = append(l.msgs, asst)
		if len(calls) == 0 {
			return Result{Outcome: Done, Final: text}, nil
		}
		for _, tc := range calls {
			out, err := l.handleCall(ctx, tc, lookup)
			if err != nil {
				return Result{}, err
			}
			if out.paused {
				return Result{Outcome: Paused}, nil
			}
			if out.abort != "" {
				return Result{Outcome: Failed, Reason: out.abort}, nil
			}
		}
	}
}

type callOutcome struct {
	paused bool
	abort  string
}

func (l *loop) handleCall(ctx context.Context, tc provider.ToolCall, lookup map[string]mcp.Tool) (callOutcome, error) {
	tool, known := lookup[tc.Name]
	name := tc.Name
	if known {
		name = tool.FullName()
	}
	args, argsOK := normalizeArgs(tc.Arguments)
	if known && argsOK {
		args = dropNullOptionals(tool.Schema, args)
	}
	if name == svc.AskUserTool && argsOK {
		// Store the repaired questions: the UI renders and Approve checks
		// answers against exactly what the viewer will be asked.
		if qs, err := svc.ParseQuestions(args); err == nil {
			args = svc.MarshalQuestions(qs)
		}
	}
	info := CallInfo{ID: tc.ID, Name: name, Args: args}
	if err := l.sink.CallStarted(ctx, info); err != nil {
		return callOutcome{}, err
	}

	var res mcp.Result
	var out callOutcome
	switch {
	case !known:
		res = errResult(fmt.Sprintf("unknown tool %q. Valid tools: %s. Use the exact name.", tc.Name, l.validNames(lookup)))
	case !argsOK:
		res = errResult("invalid arguments: they are not valid JSON. Send a JSON object. Schema: " + string(tool.Schema))
	default: // the tool server validates the arguments against its schema
		key := name + "\x00" + compact(args)
		l.counts[key]++
		switch n := l.counts[key]; {
		case n >= repeatAbort:
			res = errResult(promptRepeated)
			out.abort = fmt.Sprintf("loop detected: %s called %d times with the same arguments", name, n)
		case n >= repeatWarn:
			res = errResult(promptRepeated)
		case name == svc.AskUserTool:
			// Validate before pausing: an invalid call comes back as an
			// error result and never stalls the conversation. ask_user is
			// never an approval match: it pauses here, exactly once.
			if _, err := svc.ParseQuestions(args); err != nil {
				res = errResult("invalid questions: " + err.Error())
				break
			}
			timeout := askUserTimeout
			if l.spec.Approval != nil {
				timeout = l.spec.Approval.Timeout.D()
			}
			if err := l.rt.Svc.RequestApproval(ctx, l.env.ConversationID, l.env.TurnID, info.ID, timeout); err != nil {
				return callOutcome{}, err
			}
			return callOutcome{paused: true}, nil
		case l.needsApproval(name, tool):
			timeout := approvalTimeout
			if l.spec.Approval != nil {
				timeout = l.spec.Approval.Timeout.D()
			}
			if err := l.rt.Svc.RequestApproval(ctx, l.env.ConversationID, l.env.TurnID, info.ID, timeout); err != nil {
				return callOutcome{}, err
			}
			return callOutcome{paused: true}, nil
		default:
			res = l.exec(ctx, tool, args, info.ID)
			if err := ctx.Err(); err != nil {
				return callOutcome{}, err // cancelled mid-call: leave it to be marked interrupted
			}
		}
	}
	if err := l.finishCall(ctx, info, res); err != nil {
		return callOutcome{}, err
	}
	return out, nil
}

// needsApproval is the one decision for "does this call wait for approval?".
// The call's server owns policy: its approval patterns (WITHOUT the server
// prefix) match the tool name, or it wants approve_destructive and the
// tool's annotations say destructive. The agent's own patterns only add to
// that. Core built-ins never wait, whatever a pattern like "hfy.*" says.
func (l *loop) needsApproval(name string, tool mcp.Tool) bool {
	if tool.Server == builtins.Server && coreNoApproval[tool.Name] {
		return false
	}
	if srv, ok := l.rt.Cfg.MCP[tool.Server]; ok {
		for _, p := range srv.Approval {
			if ok, _ := path.Match(p, tool.Name); ok {
				return true
			}
		}
		if srv.ApproveDestructive && tool.Destructive {
			return true
		}
	}
	return l.spec.Approval.Requires(name)
}

// finishCall persists the result and adds it to the model's messages.
func (l *loop) finishCall(ctx context.Context, info CallInfo, res mcp.Result) error {
	if err := l.sink.CallFinished(ctx, info, res); err != nil {
		return err
	}
	l.msgs = append(l.msgs, provider.Message{Role: "tool", ToolCallID: info.ID, Content: toolText(res)})
	if info.Name == toolSearchName && !res.IsError {
		l.activate(ctx, res.Content)
	}
	return nil
}

func toolText(r mcp.Result) string {
	if r.IsError && !strings.HasPrefix(r.Content, "Error") {
		return "Error: " + r.Content
	}
	return r.Content
}

func errResult(msg string) mcp.Result { return mcp.Result{Content: msg, IsError: true} }

// exec runs a tool, a built-in here and any other through MCP, and records
// remote calls.
func (l *loop) exec(ctx context.Context, tool mcp.Tool, args json.RawMessage, callID string) mcp.Result {
	start := time.Now()
	var res mcp.Result
	var err error
	if b, ok := l.rt.findBuiltin(tool.FullName()); ok {
		res, err = b.run(ctx, l, args)
	} else {
		res, err = l.rt.mcp.Call(ctx, tool.FullName(), args, &mcp.TurnMeta{
			Conversation: l.env.ConversationUUID, Turn: l.env.TurnUUID, CallID: callID, Agent: l.spec.Name,
			Actor: l.env.Actor, Scope: l.env.Scope, Vars: l.env.Vars,
		})
	}
	if err != nil {
		res = errResult(err.Error())
	}
	metrics.ToolCalls.WithLabelValues(tool.Server, metrics.Outcome(res.IsError)).Inc()
	metrics.Since(metrics.ToolDuration.WithLabelValues(tool.Server), start)
	if src, ok := l.rt.mcp.Source(tool.Server); ok && src.Endpoint() != "" {
		req, _ := json.Marshal(map[string]any{"tool": tool.Name, "arguments": json.RawMessage(args)})
		rec := provider.Record{URL: src.Endpoint(), Request: req, Attempt: 1, LatencyMS: int(time.Since(start).Milliseconds())}
		if err != nil {
			rec.Error = err.Error()
		} else {
			rec.Response, _ = json.Marshal(map[string]any{"is_error": res.IsError, "bytes": len(res.Content)})
		}
		l.rt.recorder(&l.env.TurnID, types.OutboundMCPRemote)(rec)
	}
	res.Content = types.Clean(res.Content)
	if len(res.Content) > maxResult {
		res.Content = truncate(res.Content, maxResult) + "\n[truncated]"
	}
	if len(res.Structured) > maxStructured {
		slog.Warn("tool's structured result dropped: too large", "tool", tool.FullName(), "bytes", len(res.Structured))
		res.Structured = nil
	}
	return res
}

// tools returns the function definitions shown to the model this step and the
// wire-name lookup used to resolve its calls.
func (l *loop) tools(ctx context.Context) ([]provider.ToolDef, map[string]mcp.Tool) {
	var shown, all []mcp.Tool
	for _, b := range l.rt.builtins {
		if l.spec.Builtins[b.tool.Name] {
			shown = append(shown, b.tool)
			all = append(all, b.tool)
		}
	}
	if l.spec.MCP == nil {
		ext, _ := l.rt.mcp.Tools(ctx, nil)
		for _, t := range ext {
			all = append(all, t) // callable if the model names it, shown once found
			if l.activated[t.FullName()] {
				shown = append(shown, t)
			}
		}
	} else {
		ext, _ := l.rt.mcp.Tools(ctx, l.spec.MCP)
		shown = append(shown, ext...)
		all = append(all, ext...)
	}
	defs := make([]provider.ToolDef, len(shown))
	for i, t := range shown {
		defs[i] = provider.ToolDef{Name: mcp.WireName(t), Description: t.Description, Parameters: t.Schema}
	}
	lookup := make(map[string]mcp.Tool, len(all))
	for _, t := range all {
		lookup[mcp.WireName(t)] = t
	}
	return defs, lookup
}

func (l *loop) validNames(lookup map[string]mcp.Tool) string {
	var names []string
	for w := range lookup {
		names = append(names, w)
	}
	slices.Sort(names)
	if len(names) > 40 {
		names = append(names[:40], "...")
	}
	if l.spec.Builtins[builtins.ToolSearch] {
		names = append(names, "(use "+mcp.WireNameOf(toolSearchName)+" to find more)")
	}
	return strings.Join(names, ", ")
}

// activate unlocks the tools named in a tool_search result.
func (l *loop) activate(ctx context.Context, content string) {
	var hits []struct {
		Name string `json:"name"` // wire name
	}
	if json.Unmarshal([]byte(content), &hits) != nil {
		return
	}
	ext, _ := l.rt.mcp.Tools(ctx, nil)
	byWire := map[string]string{}
	for _, t := range ext {
		byWire[mcp.WireName(t)] = t.FullName()
	}
	for _, h := range hits {
		if full, ok := byWire[h.Name]; ok {
			l.activated[full] = true
		}
	}
}

// deriveActivated rebuilds the unlocked set from earlier tool_search results,
// so a resumed turn shows the same tools.
func (l *loop) deriveActivated(ctx context.Context) {
	searchCalls := map[string]bool{}
	for _, m := range l.msgs {
		for _, tc := range m.ToolCalls {
			if tc.Name == mcp.WireNameOf(toolSearchName) {
				searchCalls[tc.ID] = true
			}
		}
		if m.Role == "tool" && searchCalls[m.ToolCallID] {
			l.activate(ctx, m.Content)
		}
	}
}

// normalizeArgs returns storable JSON arguments and whether the model's text was valid JSON.
func normalizeArgs(raw string) (json.RawMessage, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return json.RawMessage(`{}`), true
	}
	if json.Valid([]byte(raw)) {
		return json.RawMessage(raw), true
	}
	b, _ := json.Marshal(types.Clean(raw)) // keep what the model said, as a JSON string
	return b, false
}

// dropNullOptionals removes optional fields the model set to null: models
// (strict modes especially) send null for "not given", which a schema typed
// "integer" would reject, here and in the tool server.
func dropNullOptionals(schema, args json.RawMessage) json.RawMessage {
	var obj map[string]json.RawMessage
	var s struct {
		Required []string `json:"required"`
	}
	if json.Unmarshal(args, &obj) != nil || json.Unmarshal(schema, &s) != nil {
		return args
	}
	dropped := false
	for k, v := range obj {
		if string(v) == "null" && !slices.Contains(s.Required, k) {
			delete(obj, k)
			dropped = true
		}
	}
	if !dropped {
		return args
	}
	b, _ := json.Marshal(obj)
	return b
}

func compact(b json.RawMessage) string {
	var buf bytes.Buffer
	if json.Compact(&buf, b) != nil {
		return string(b)
	}
	return buf.String()
}
