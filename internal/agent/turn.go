package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/mcp"
	"github.com/alash3al/hfy/internal/provider"
	"github.com/alash3al/hfy/internal/svc"
	"github.com/alash3al/hfy/internal/types"
)

// RunTurn is the run_turn job: start (or resume) a turn and run the agent loop.
func (rt *Runtime) RunTurn(ctx context.Context, p svc.RunTurnPayload, job db.JobRow) error {
	turn, err := rt.Store.EventByID(ctx, rt.Store.Pool, p.TurnEventID)
	if db.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if turn.Type != types.EventTurn {
		return nil
	}

	// Register for stop requests before reading any state, so none is missed.
	tctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	rt.cancels.Store(turn.ID, cancel)
	defer rt.cancels.Delete(turn.ID)

	events, err := rt.Store.TurnEvents(ctx, rt.Store.Pool, turn.ID)
	if err != nil {
		return err
	}
	view, err := svc.ViewOf(events)
	if err != nil {
		return err
	}
	if view.State.Terminal() || (view.State == types.StateAwaitingApproval && !view.Resumable()) {
		return nil
	}
	ok, err := rt.Svc.StartTurn(ctx, turn.ConversationID, turn.ID)
	if err != nil || !ok {
		return err
	}
	if events, err = rt.Store.TurnEvents(ctx, rt.Store.Pool, turn.ID); err != nil {
		return err
	}
	if view, err = svc.ViewOf(events); err != nil {
		return err
	}

	fresh := context.WithoutCancel(ctx)
	if view.Cancelled {
		return rt.Svc.EndTurn(fresh, turn.ConversationID, turn.ID, types.TurnStateEvent{State: types.StateStopped, Reason: "stop requested"})
	}
	t, err := turn.Payload[types.Turn]()
	if err != nil {
		return err
	}
	spec, ok := rt.agents[t.AgentName()]
	if !ok { // removed from the config after the turn was posted
		return rt.Svc.EndTurn(fresh, turn.ConversationID, turn.ID, types.TurnStateEvent{State: types.StateFailed,
			Reason: "unknown agent", Error: fmt.Sprintf("agent %q is not configured", t.Agent)})
	}
	if d := spec.Limits.Timeout.D(); d > 0 {
		var stop context.CancelFunc
		tctx, stop = context.WithTimeoutCause(tctx, d, errTimeout)
		defer stop()
	}
	mainActor, err := rt.Svc.AgentActor(ctx, spec.Name)
	if err != nil {
		return err
	}
	used, calls, err := rt.Store.TurnUsage(ctx, rt.Store.Pool, turn.ID, mainActor)
	if err != nil {
		return err
	}
	conv, err := rt.Store.ConversationByID(ctx, turn.ConversationID)
	if err != nil {
		return err
	}
	env := &TurnEnv{ConversationID: turn.ConversationID, TurnID: turn.ID, Scope: turn.Scope, Vars: t.Vars,
		Total:            &usageAcc{u: used, calls: int(calls)},
		ConversationUUID: conv.UUID.String(), TurnUUID: turn.UUID.String(),
		Actor: mcp.ActorMeta{Type: turn.ActorType, Ref: turn.ActorRef}}
	sink := newTurnSink(tctx, rt, env, mainActor)
	l := newLoop(rt, spec, sink, env)
	l.steps = int(calls)

	runErr := rt.resolveDangling(tctx, l, sink, view, job.Attempts)
	var res Result
	if runErr == nil {
		var system string
		var msgs []provider.Message
		if system, msgs, runErr = rt.buildContext(tctx, turn.ConversationID, turn.ID, spec); runErr == nil {
			l.load(tctx, system, msgs)
			l.prepare = rt.summarizeHook(env)
			res, runErr = l.run(tctx)
		}
	}
	return rt.finish(ctx, tctx, sink, turn, res, runErr)
}

func (rt *Runtime) finish(parent, tctx context.Context, sink *turnSink, turn db.EventRow, res Result, runErr error) error {
	fresh := context.WithoutCancel(parent) // the cancelled context cannot write
	end := func(st types.TurnStateEvent) error { return rt.Svc.EndTurn(fresh, turn.ConversationID, turn.ID, st) }
	if runErr != nil {
		cause := context.Cause(tctx)
		switch {
		case parent.Err() != nil:
			return runErr // shutting down: the lease expires and the turn resumes elsewhere
		case errors.Is(cause, errStopped), errors.Is(runErr, svc.ErrStopRequested):
			sink.FlushPartial(fresh)
			rt.markInterrupted(fresh, sink, turn.ID)
			return end(types.TurnStateEvent{State: types.StateStopped, Reason: "stop requested"})
		case errors.Is(cause, errTimeout):
			sink.FlushPartial(fresh)
			rt.markInterrupted(fresh, sink, turn.ID)
			return end(types.TurnStateEvent{State: types.StateFailed, Reason: "timeout", Error: "the turn exceeded limits.timeout"})
		}
		return runErr // infrastructure error: the job retries and resumes
	}
	switch res.Outcome {
	case Paused:
		return nil
	case Failed:
		return end(types.TurnStateEvent{State: types.StateFailed, Reason: res.Reason, Error: res.Reason})
	}
	return end(types.TurnStateEvent{State: types.StateCompleted})
}

// resolveDangling settles tool calls that have no result yet. Calls are appended
// right before they run, so a call without a result was either in flight when a
// worker died (never re-run blindly: it is marked interrupted) or is waiting on
// an approval decision.
func (rt *Runtime) resolveDangling(ctx context.Context, l *loop, sink *turnSink, v svc.TurnView, attempts int) error {
	for _, c := range v.Calls {
		if v.Done[c.CallID] {
			continue
		}
		info := CallInfo{ID: c.CallID, Name: c.Name, Args: c.Arguments}
		d, decided := v.Decided[c.CallID]
		switch {
		case decided && v.RequestData[c.CallID].Held != nil:
			// A held call already ran; its stored result is the tool_result,
			// joined with the answers (a decline or a timeout just tells the
			// model the viewer passed). The client's d.Result is ignored.
			req := v.RequestData[c.CallID]
			var text string
			switch {
			case d.Approve:
				text = answersText(d.Answers)
				if text == "" {
					text = strings.TrimSpace(promptDeclined)
				}
			case d.Reason == "timeout":
				text = strings.TrimSpace(promptTimeout)
			default:
				text = strings.TrimSpace(promptDeclined)
			}
			content := req.Held.Content
			if content != "" && text != "" {
				content += "\n\n"
			}
			content += text
			if err := sink.CallFinished(ctx, info, mcp.Result{Content: content, Structured: req.Held.Structured, UI: req.UI}); err != nil {
				return err
			}
		case decided && d.Approve && c.Name == svc.AskUserTool:
			// Never re-run an answered ask: the decision's answers ARE the
			// tool result, exactly this JSON (strictly; an old decision without
			// answers falls back to the declined text).
			text := answersText(d.Answers)
			if text == "" {
				text = strings.TrimSpace(promptDeclined)
			}
			if err := sink.CallFinished(ctx, info, mcp.Result{Content: text}); err != nil {
				return err
			}
		case decided && !d.Approve:
			text := d.Result
			if text == "" {
				if c.Name == svc.AskUserTool { // declined or timed out: the model continues gracefully
					if d.Reason == "timeout" {
						text = strings.TrimSpace(promptTimeout)
					} else {
						text = strings.TrimSpace(promptDeclined)
					}
				} else {
					text = strings.TrimSpace(promptRejected)
					if d.Reason != "" {
						text += " Reason: " + d.Reason
					}
				}
			}
			if err := sink.CallFinished(ctx, info, mcp.Result{Content: text}); err != nil {
				return err
			}
		case decided && attempts <= 1:
			tool, ok := rt.findTool(ctx, c.Name)
			res := errResult(fmt.Sprintf("tool %q is no longer available", c.Name))
			if ok {
				res = l.exec(ctx, tool, c.Arguments, c.CallID)
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			if len(res.Questions) > 0 { // approved calls cannot pause again
				slog.Warn("approved tool asked questions; dropped", "tool", c.Name)
			}
			res.Questions = nil
			if err := sink.CallFinished(ctx, info, res); err != nil {
				return err
			}
		case v.Requested[c.CallID] && !decided:
			continue
		default:
			if err := sink.append(ctx, types.ToolCallInterrupted{CallID: c.CallID}, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

// answersText renders a decision's answers as the tool_result text, an object
// re-marshaled from what the stored event decoded to (Postgres re-orders jsonb
// object keys, so only decoding yields a stable order). Empty when the
// decision's answers are unusable.
func answersText(raw json.RawMessage) string {
	var answ map[string]any
	if json.Unmarshal(raw, &answ) != nil || answ == nil {
		return ""
	}
	b, err := json.Marshal(map[string]any{"answers": answ})
	if err != nil {
		return ""
	}
	return string(b)
}

func (rt *Runtime) findTool(ctx context.Context, full string) (mcp.Tool, bool) {
	if b, ok := rt.findBuiltin(full); ok {
		return b.tool, true
	}
	all, _ := rt.mcp.Tools(ctx, nil)
	for _, t := range all {
		if t.FullName() == full {
			return t, true
		}
	}
	return mcp.Tool{}, false
}

// markInterrupted flags calls that were running when a turn was stopped.
func (rt *Runtime) markInterrupted(ctx context.Context, sink *turnSink, turnID int64) {
	events, err := rt.Store.TurnEvents(ctx, rt.Store.Pool, turnID)
	if err != nil {
		slog.Error("mark interrupted", "turn", turnID, "err", err)
		return
	}
	v, err := svc.ViewOf(events)
	if err != nil {
		slog.Error("mark interrupted", "turn", turnID, "err", err)
		return
	}
	for _, c := range v.Calls {
		if v.Done[c.CallID] {
			continue
		}
		if err := sink.append(ctx, types.ToolCallInterrupted{CallID: c.CallID}, nil); err != nil {
			slog.Error("mark interrupted", "turn", turnID, "call", c.CallID, "err", err)
		}
	}
}

// onTurnExhausted ends a turn whose job ran out of attempts.
func (rt *Runtime) onTurnExhausted(ctx context.Context, job db.JobRow, err error) {
	var p svc.RunTurnPayload
	if jerr := json.Unmarshal(job.Payload, &p); jerr != nil {
		slog.Error("end exhausted turn: bad payload", "job", job.ID, "err", jerr)
		return
	}
	turn, gerr := rt.Store.EventByID(ctx, rt.Store.Pool, p.TurnEventID)
	if gerr != nil {
		slog.Error("end exhausted turn", "turn", p.TurnEventID, "err", gerr)
		return
	}
	msg := fmt.Sprintf("worker gave up after %d attempts: %v", job.Attempts, err)
	if e := rt.Svc.EndTurn(ctx, turn.ConversationID, turn.ID, types.TurnStateEvent{State: types.StateFailed, Reason: "worker failed", Error: msg}); e != nil {
		slog.Error("end exhausted turn", "turn", turn.ID, "err", e)
	}
}

// estimateTokens is a rough 4-chars-per-token guess, used only until the
// provider reports a real prompt size.
func estimateTokens(system string, msgs []provider.Message) int64 {
	n := len(system)
	for _, m := range msgs {
		n += len(m.Content)
		for _, tc := range m.ToolCalls {
			n += len(tc.Name) + len(tc.Arguments)
		}
	}
	return int64(n / 4)
}
