package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"uuid"

	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/mcp"
	"github.com/alash3al/hfy/internal/svc"
	"github.com/alash3al/hfy/internal/types"
)

func mustUUID(s string) uuid.UUID {
	u, _ := uuid.Parse(s)
	return u
}

func wireJoin(server, tool string) string {
	return mcp.WireName(mcp.Tool{Server: server, Name: tool})
}

// RunTurn is the run_turn job: start (or resume) a turn and run the agent loop.
func (rt *Runtime) RunTurn(ctx context.Context, p svc.RunTurnPayload, job db.JobRow) error {
	turn, err := rt.Store.EventByID(ctx, p.TurnEventID)
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
	view := svc.ViewOf(events)
	if view.State.Terminal() || (view.State == types.StateAwaitingApproval && !view.Resumable()) {
		return nil
	}
	ok, err := rt.Svc.StartTurn(ctx, turn.SessionID, turn.ID)
	if err != nil || !ok {
		return err
	}
	if events, err = rt.Store.TurnEvents(ctx, rt.Store.Pool, turn.ID); err != nil {
		return err
	}
	view = svc.ViewOf(events)

	fresh := context.WithoutCancel(ctx)
	if view.Cancelled {
		return rt.Svc.EndTurn(fresh, turn.SessionID, turn.ID, types.TurnStateEvent{State: types.StateStopped, Reason: "stop requested"})
	}
	if d := rt.Main.Limits.Timeout.D(); d > 0 {
		var stop context.CancelFunc
		tctx, stop = context.WithTimeoutCause(tctx, d, errTimeout)
		defer stop()
	}

	var t types.Turn
	_ = json.Unmarshal(turn.Data, &t)
	mainActor, err := rt.Svc.AgentActor(ctx, types.AgentMain)
	if err != nil {
		return err
	}
	used, calls, err := rt.Store.TurnUsage(ctx, turn.ID, mainActor)
	if err != nil {
		return err
	}
	env := &TurnEnv{SessionID: turn.SessionID, TurnID: turn.ID, Scope: turn.Scope, Vars: t.Vars,
		Total: &usageAcc{u: used, calls: int(calls)}}
	sink := newTurnSink(tctx, rt, env, mainActor)
	l := newLoop(rt, rt.Main, sink, env, "")
	l.steps = int(calls)

	runErr := rt.resolveDangling(tctx, l, sink, view, job.Attempts)
	var res Result
	if runErr == nil {
		var bc *BuiltContext
		if bc, runErr = rt.BuildContext(tctx, turn.SessionID, turn.ID, rt.Main); runErr == nil {
			l.system, l.msgs, l.lastPrompt = bc.System, bc.Msgs, estimateTokens(bc.System, bc.Msgs)
			l.deriveActivated()
			l.prepare = rt.summarizeHook(env)
			res, runErr = l.run(tctx)
		}
	}
	return rt.finish(ctx, tctx, sink, turn, res, runErr)
}

func (rt *Runtime) finish(parent, tctx context.Context, sink *turnSink, turn db.EventRow, res Result, runErr error) error {
	fresh := context.WithoutCancel(parent) // the cancelled context cannot write
	end := func(st types.TurnStateEvent) error { return rt.Svc.EndTurn(fresh, turn.SessionID, turn.ID, st) }
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
		case decided && !d.Approve:
			text := d.Result
			if text == "" {
				text = strings.TrimSpace(promptRejected)
				if d.Reason != "" {
					text += " Reason: " + d.Reason
				}
			}
			if err := sink.CallFinished(ctx, info, mcp.Result{Content: text}); err != nil {
				return err
			}
		case decided && attempts <= 1:
			tool, ok := rt.findTool(ctx, c.Name)
			res := errResult(fmt.Sprintf("tool %q is no longer available", c.Name))
			if ok {
				res = l.exec(ctx, tool, c.Arguments)
				if err := ctx.Err(); err != nil {
					return err
				}
			}
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

func (rt *Runtime) findTool(ctx context.Context, full string) (mcp.Tool, bool) {
	all, _ := rt.MCP.Tools(ctx, nil)
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
	v := svc.ViewOf(events)
	for _, c := range v.Calls {
		if !v.Done[c.CallID] {
			sink.append(ctx, types.ToolCallInterrupted{CallID: c.CallID}, nil)
		}
	}
}

// OnTurnExhausted ends a turn whose job ran out of attempts.
func (rt *Runtime) OnTurnExhausted(ctx context.Context, job db.JobRow, err error) {
	var p svc.RunTurnPayload
	if json.Unmarshal(job.Payload, &p) != nil {
		return
	}
	turn, gerr := rt.Store.EventByID(ctx, p.TurnEventID)
	if gerr != nil {
		return
	}
	msg := fmt.Sprintf("worker gave up after %d attempts: %v", job.Attempts, err)
	if e := rt.Svc.EndTurn(ctx, turn.SessionID, turn.ID, types.TurnStateEvent{State: types.StateFailed, Reason: "worker failed", Error: msg}); e != nil {
		slog.Error("end exhausted turn", "turn", turn.ID, "err", e)
	}
}

// estimateTokens is a rough 4-chars-per-token guess, used only until the
// provider reports a real prompt size.
func estimateTokens(system string, msgs []providerMessage) int64 {
	n := len(system)
	for _, m := range msgs {
		n += len(m.Content)
		for _, tc := range m.ToolCalls {
			n += len(tc.Name) + len(tc.Arguments)
		}
	}
	return int64(n / 4)
}
