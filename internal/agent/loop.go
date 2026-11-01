package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	builtins.ToolSearch: true,
}

const (
	repeatWarn  = 3       // the third identical call is refused with advice
	repeatAbort = 6       // the sixth ends the turn
	maxResult   = 100_000 // bytes of a tool result the model sees
	// maxStructured bounds a tool's structuredContent, card and questions, which
	// are stored with its result for clients.
	maxStructured = 1 << 20
	// The bounds below are fixed, not configured: they stop a runaway or
	// hostile model from growing one turn without limit.
	// maxArgBytes bounds one call's arguments. Every stored byte is re-sent in
	// later prompts, and no real tool call needs more than this.
	maxArgBytes = 64 << 10
	// maxTurnEvents bounds the durable events of one turn. A turn of 50 steps
	// with 10 calls each is about 1,200 events.
	maxTurnEvents = 5000
	// maxPromptBytes bounds the text of one model request after compaction:
	// about 2M tokens, far past the context windows in use.
	maxPromptBytes = 8 << 20
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
	// The turn's durable events: seeded from its log, then counted as the
	// agent appends them (maxTurnEvents).
	events atomic.Int64
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
	used      map[string]bool // call ids of the conversation so far (history and this turn): a call is keyed by id alone
	steps     int             // steps already taken (includes earlier runs of a resumed turn)

	// The prompt's size in tokens when last measured, and len(msgs) then:
	// what was added since is estimated on top (see promptSize).
	lastPrompt int64
	measuredAt int
	// prepare may rebuild msgs before a model call (summarization). force asks
	// for compaction whatever the token count says.
	prepare func(ctx context.Context, l *loop, force bool) error
	// compactions counts the summaries prepare has made, so run can tell an
	// oversize prompt that compaction failed to shrink from one it never tried.
	compactions int
}

func newLoop(rt *Runtime, spec *Spec, sink Sink, env *TurnEnv) *loop {
	l := &loop{rt: rt, spec: spec, sink: sink, env: env,
		activated: map[string]bool{}, counts: map[string]int{}, used: map[string]bool{}}
	if spec.Top {
		l.total = env.Total
	} else {
		l.total = &usageAcc{}
	}
	return l
}

// load sets the system prompt and messages the loop sends. It marks the call
// ids already in them used (a provider may reject an id repeated in one
// conversation), measures them at once (promptSize reads msgs from the last
// measurement on) and unlocks the tools that earlier tool_search results found.
func (l *loop) load(ctx context.Context, system string, msgs []provider.Message) {
	l.system, l.msgs = system, msgs
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			l.used[tc.ID] = true
		}
		if m.ToolCallID != "" {
			l.used[m.ToolCallID] = true
		}
	}
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
	return l.eventsLimit()
}

// eventsLimit is the reason to end a turn that holds maxTurnEvents events.
func (l *loop) eventsLimit() string {
	if l.env.events.Load() >= maxTurnEvents {
		return fmt.Sprintf("limit reached: %d events in one turn", maxTurnEvents)
	}
	return ""
}

// request is the model request for the loop's current messages.
func (l *loop) request(defs []provider.ToolDef) provider.Request {
	return provider.Request{
		Model:       l.spec.Model,
		Messages:    append([]provider.Message{{Role: "system", Content: l.system}}, l.msgs...),
		Tools:       defs,
		Temperature: l.spec.Settings.Temperature, TopP: l.spec.Settings.TopP,
	}
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
			if err := l.prepare(ctx, l, false); err != nil {
				return Result{}, err
			}
		}
		defs, lookup := l.tools(ctx)
		req := l.request(defs)
		compacted := false
		if requestBytes(req) > maxPromptBytes && l.prepare != nil {
			// Compaction gets its chance whatever the token count says.
			before := l.compactions
			if err := l.prepare(ctx, l, true); err != nil {
				return Result{}, err
			}
			compacted = l.compactions > before
			defs, lookup = l.tools(ctx)
			req = l.request(defs)
		}
		if n := requestBytes(req); n > maxPromptBytes {
			how := "compaction unavailable"
			if compacted {
				how = "after compaction"
			}
			return Result{Outcome: Failed, Reason: fmt.Sprintf("limit reached: prompt of %d bytes, %s (limit %d)", n, how, maxPromptBytes)}, nil
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
			return Result{Outcome: Failed, Reason: "model call failed: " + types.ErrorText(err)}, nil
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
		tooLarge := make([]bool, 0, len(resp.ToolCalls))
		for i, tc := range resp.ToolCalls {
			// An id the event log cannot hold (empty, NUL, too long) or that the
			// conversation already used is replaced: approvals, results and resume
			// all find a call by its id alone. The suffix is a number, so the id
			// stays short however often a model collides.
			if tc.ID == "" || !storableText(tc.ID) || l.used[tc.ID] {
				base := fmt.Sprintf("call_%d_%d", l.steps, i)
				tc.ID = base
				for n := 1; l.used[tc.ID]; n++ {
					tc.ID = fmt.Sprintf("%s_%d", base, n)
				}
			}
			l.used[tc.ID] = true
			big := len(tc.Arguments) > maxArgBytes
			if big {
				tc.Arguments = argsMarker(tc.Arguments)
			}
			calls = append(calls, tc)
			tooLarge = append(tooLarge, big)
		}
		asst.ToolCalls = calls
		l.msgs = append(l.msgs, asst)
		if len(calls) == 0 {
			return Result{Outcome: Done, Final: text}, nil
		}
		for i, tc := range calls {
			out, err := l.handleCall(ctx, tc, tooLarge[i], lookup)
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

func (l *loop) handleCall(ctx context.Context, tc provider.ToolCall, tooLarge bool, lookup map[string]mcp.Tool) (callOutcome, error) {
	if why := l.eventsLimit(); why != "" {
		return callOutcome{abort: why}, nil
	}
	tool, known := lookup[tc.Name]
	known = known && storableText(tc.Name) // a name the log cannot hold is an unknown tool
	name := clipName(tc.Name)
	if known {
		name = clipName(tool.FullName())
	}
	args, argsErr := normalizeArgs(tc.Arguments)
	if known && argsErr == nil {
		args = dropNullOptionals(tool.Schema, args)
	}
	if name == svc.AskUserTool && argsErr == nil {
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

	// Every recorded call counts, refused ones too. seedCounts restores the
	// same counts when the turn resumes, so the guard survives a resume.
	key := callKey(name, args)
	l.counts[key]++
	n := l.counts[key]

	var res mcp.Result
	var out callOutcome
	v := repeatVerdict(n)
	switch {
	case v == verdictAbort:
		res = errResult(promptRepeated)
		out.abort = fmt.Sprintf("loop detected: %s called %d times with the same arguments", name, n)
	case !known:
		res = errResult(fmt.Sprintf("unknown tool %q. Valid tools: %s. Use the exact name.", name, l.validNames(lookup)))
	case tooLarge || errors.Is(argsErr, errTooLarge):
		res = errResult(fmt.Sprintf("arguments too large (limit %d bytes); send less", maxArgBytes))
	case argsErr != nil:
		res = errResult("invalid arguments: " + argsErr.Error() + ". Send a JSON object. Schema: " + string(tool.Schema))
	case v == verdictWarn:
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
		if err := l.rt.Svc.RequestApproval(ctx, l.env.ConversationID, l.env.TurnID, types.ApprovalRequested{CallID: info.ID}, timeout); err != nil {
			return callOutcome{}, err
		}
		return callOutcome{paused: true}, nil
	case l.needsApproval(name, tool):
		timeout := approvalTimeout
		if l.spec.Approval != nil {
			timeout = l.spec.Approval.Timeout.D()
		}
		if err := l.rt.Svc.RequestApproval(ctx, l.env.ConversationID, l.env.TurnID, types.ApprovalRequested{CallID: info.ID}, timeout); err != nil {
			return callOutcome{}, err
		}
		return callOutcome{paused: true}, nil
	default: // the tool server validates the arguments against its schema
		res = l.exec(ctx, tool, args, info.ID)
		if err := ctx.Err(); err != nil {
			return callOutcome{}, err // cancelled mid-call: leave it to be marked interrupted
		}
		if out2, err := l.holdForQuestions(ctx, info, res); out2.paused || err != nil {
			return out2, err
		}
	}
	if err := l.finishCall(ctx, info, res); err != nil {
		return callOutcome{}, err
	}
	return out, nil
}

// holdForQuestions pauses on a tool's own questions (design B: they ride the
// tool's real call). The result is already computed, so the request holds it:
// on resume it becomes the call's tool_result next to the user's answers.
func (l *loop) holdForQuestions(ctx context.Context, info CallInfo, res mcp.Result) (callOutcome, error) {
	if len(res.Questions) == 0 {
		return callOutcome{}, nil
	}
	timeout := askUserTimeout
	if l.spec.Approval != nil {
		timeout = l.spec.Approval.Timeout.D()
	}
	req := types.ApprovalRequested{CallID: info.ID, Questions: res.Questions, UI: res.UI,
		Held: &types.HeldResult{Content: res.Content, Structured: res.Structured}}
	if err := l.rt.Svc.RequestApproval(ctx, l.env.ConversationID, l.env.TurnID, req, timeout); err != nil {
		if errors.Is(err, svc.ErrStopRequested) {
			// A stop that cancels the turn context before the request is
			// stored falls under the same "cancelled mid-call" policy: the
			// call is marked interrupted. Even so, the stop won the race but
			// the tool ran: its result must land as the one tool_result, or
			// the model may re-run it.
			if ferr := l.finishCall(context.WithoutCancel(ctx), info, mcp.Result{
				Content:    svc.HeldResultText(req, types.ApprovalDecided{}, false),
				Structured: res.Structured, UI: res.UI}); ferr != nil {
				return callOutcome{}, ferr
			}
		}
		return callOutcome{}, err
	}
	return callOutcome{paused: true}, nil
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
		res = errResult(types.ErrorText(err))
	}
	metrics.ToolCalls.WithLabelValues(tool.Server, metrics.Outcome(res.IsError)).Inc()
	metrics.Since(metrics.ToolDuration.WithLabelValues(tool.Server), start)
	if src, ok := l.rt.mcp.Source(tool.Server); ok && src.Endpoint() != "" {
		req, _ := json.Marshal(map[string]any{"tool": tool.Name, "arguments": json.RawMessage(args)})
		rec := provider.Record{URL: src.Endpoint(), Request: req, Attempt: 1, LatencyMS: int(time.Since(start).Milliseconds())}
		if err != nil {
			rec.Error = types.ErrorText(err)
		} else {
			rec.Response, _ = json.Marshal(map[string]any{"is_error": res.IsError, "bytes": len(res.Content)})
		}
		l.rt.recorder(&l.env.TurnID, types.OutboundMCPRemote)(rec)
	}
	res.Content = types.Clean(res.Content)
	if len(res.Content) > maxResult {
		res.Content = truncate(res.Content, maxResult) + "\n[truncated]"
	}
	res.Structured = keepStored(res.Structured, maxStructured, "structured result", tool.FullName())
	// A tool's ui block reaches clients only when its server's config opted
	// in; error results lose the whole block. Sub-agents and ask-less agents
	// lose only the questions here (the card survives); a sub-agent's card
	// is dropped later because its result reaches the parent as text.
	// Questions ride the result separately and pause the turn later (handleCall).
	if cfg := l.rt.Cfg.MCP[tool.Server]; cfg.UI != nil && !res.IsError {
		card, qs, err := svc.SanitizeUI(res.UI, cfg.UI.ImageHosts, l.spec.Top && l.spec.Builtins[builtins.AskUser])
		if err != nil {
			slog.Warn("tool's ui block dropped", "tool", tool.FullName(), "err", err)
		}
		res.UI = keepStored(card, maxStructured, "ui block", tool.FullName())
		res.Questions = keepStored(qs, maxStructured, "questions", tool.FullName())
	} else {
		res.UI, res.Questions = nil, nil
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

// normalizeArgs returns the arguments the event log stores, and why they are
// refused (nil if they are fine). Text that is not storable JSON is kept as a
// JSON string of what the model said; arguments too large once their numbers
// are written out are stored as their marker.
func normalizeArgs(raw string) (json.RawMessage, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return json.RawMessage(`{}`), nil
	}
	err := storable(raw, maxArgBytes)
	switch {
	case err == nil:
		return json.RawMessage(raw), nil
	case errors.Is(err, errTooLarge):
		return json.RawMessage(argsMarker(raw)), err
	}
	b, _ := json.Marshal(types.Clean(raw)) // keep what the model said, as a JSON string
	if len(b) > maxReplayArgs {            // escapes can inflate it; replay would clamp it anyway
		return json.RawMessage(argsMarker(raw)), err
	}
	return b, err
}

// seedCounts restores the repeat counts of a resumed turn from the calls it
// already recorded: each tool_call event was counted when it was recorded. An
// ask the viewer answered is not counted: answered asks never repeat-count.
// The recorded call ids are marked used too, so a resumed turn never reuses one.
func (l *loop) seedCounts(v svc.TurnView) {
	for _, c := range v.Calls {
		l.used[c.CallID] = true
		if d, ok := v.Decided[c.CallID]; ok && d.Approve && c.Name == svc.AskUserTool {
			continue
		}
		l.counts[callKey(c.Name, c.Arguments)]++
	}
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
