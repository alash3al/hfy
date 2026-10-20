// Package svc holds the session mutations shared by the HTTP API and the
// workers: posting, stopping, approvals and ending turns. All of them take the
// session row lock, so they serialize per session.
package svc

import (
	"context"
	"errors"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/metrics"
	"github.com/alash3al/hfy/internal/types"
)

var (
	ErrNoActiveTurn      = errors.New("no active turn")
	ErrNoPendingApproval = errors.New("no pending approval for that call")
	// ErrStopRequested: a stop arrived before the turn could pause.
	ErrStopRequested = errors.New("stop requested")
)

type RunTurnPayload struct {
	TurnEventID int64 `json:"turn_event_id"`
}

type ApprovalTimeoutPayload struct {
	TurnEventID int64  `json:"turn_event_id"`
	CallID      string `json:"call_id"`
}

type Service struct {
	Store *db.Store
	Cfg   *config.Config
}

func New(store *db.Store, cfg *config.Config) *Service { return &Service{Store: store, Cfg: cfg} }

func (s *Service) AgentActor(ctx context.Context, ref string) (int64, error) {
	return s.Store.FindOrCreateActor(ctx, types.ActorAgent, ref)
}

type PostInput struct {
	SessionID   int64
	Actor       int64
	Scope       string
	Vars        map[string]string
	Text        string
	Attachments []types.Attachment
}

// Post appends a turn with its user message and a queued state, and enqueues
// run_turn unless the session already has an active (or queued) turn.
func (s *Service) Post(ctx context.Context, in PostInput) (uuid.UUID, error) {
	main, err := s.AgentActor(ctx, types.AgentMain)
	if err != nil {
		return uuid.UUID{}, err
	}
	var turnUUID uuid.UUID
	err = s.Store.Tx(ctx, func(tx pgx.Tx) error {
		sess, err := s.Store.LockSession(ctx, tx, in.SessionID)
		if err != nil {
			return err
		}
		turn, err := s.Store.AppendEvent(ctx, tx, db.NewEvent{SessionID: in.SessionID, ActorID: in.Actor,
			Scope: in.Scope, Payload: types.Turn{Vars: in.Vars, Scope: in.Scope}})
		if err != nil {
			return err
		}
		turnUUID = turn.UUID
		msg, err := s.Store.AppendEvent(ctx, tx, db.NewEvent{SessionID: in.SessionID, ParentID: &turn.ID, ActorID: in.Actor,
			Scope: in.Scope, Payload: types.MessageText{Text: in.Text, Attachments: in.Attachments}})
		if err != nil {
			return err
		}
		if _, err := s.Store.AppendEvent(ctx, tx, db.NewEvent{SessionID: in.SessionID, ParentID: &turn.ID, ActorID: main,
			Scope: in.Scope, Payload: types.TurnStateEvent{State: types.StateQueued}}); err != nil {
			return err
		}
		if in.Text != "" && s.Cfg.Embeddings != nil {
			if err := s.EnqueueEmbed(ctx, tx, msg.ID); err != nil {
				return err
			}
		}
		if sess.ActiveTurnID != nil {
			return nil // the running (or paused) turn chains to this one when it ends
		}
		queued, err := s.Store.PendingTurnJob(ctx, tx, in.SessionID, types.JobQueued)
		if err != nil || queued {
			return err
		}
		return s.enqueueRun(ctx, tx, in.SessionID, turn.ID)
	})
	return turnUUID, err
}

func (s *Service) enqueueRun(ctx context.Context, q db.DBTX, session, turn int64) error {
	_, err := s.Store.Enqueue(ctx, q, db.NewJob{
		Type: types.JobRunTurn, Queue: types.QueueTurns, Payload: RunTurnPayload{TurnEventID: turn},
		MaxAttempts: s.Cfg.Queue.MaxAttempts, Key: db.SessionKey(session),
	})
	return err
}

// EmbedPayload names the message.text events to embed; empty means a backfill
// of everything that has no vector for the configured model.
type EmbedPayload struct {
	EventIDs []int64 `json:"event_ids,omitempty"`
}

// EnqueueEmbed schedules embed_events for the given events (none = backfill),
// in the caller's transaction, so it can never miss what that transaction wrote.
// Targeted jobs run in parallel (the upsert is idempotent); backfills, which
// every booting process enqueues, run one at a time.
func (s *Service) EnqueueEmbed(ctx context.Context, q db.DBTX, eventIDs ...int64) error {
	key := ""
	if len(eventIDs) == 0 {
		key = "embed_backfill"
	}
	_, err := s.Store.Enqueue(ctx, q, db.NewJob{
		Type: types.JobEmbedEvents, Queue: types.QueueEmbeddings, Payload: EmbedPayload{EventIDs: eventIDs},
		MaxAttempts: s.Cfg.Queue.MaxAttempts, Key: key,
	})
	return err
}

// appendTo appends an event under a turn, copying its scope. Inside a
// mutation every read and write goes through tx: a second pool connection
// taken while the transaction holds one deadlocks once the pool is exhausted.
func (s *Service) appendTo(ctx context.Context, tx pgx.Tx, turn db.EventRow, actor int64, p types.Payload) error {
	_, err := s.Store.AppendEvent(ctx, tx, db.NewEvent{SessionID: turn.SessionID, ParentID: &turn.ID,
		ActorID: actor, Scope: turn.Scope, Payload: p})
	return err
}

// EndTurn appends the terminal state of a turn, books its usage on the
// session, clears the active pointer and chains to the next queued turn.
// It is idempotent: a turn that already ended is left alone.
func (s *Service) EndTurn(ctx context.Context, sessionID, turnID int64, st types.TurnStateEvent) error {
	var ended bool
	err := s.Store.Tx(ctx, func(tx pgx.Tx) (err error) {
		ended, err = s.endTurnTx(ctx, tx, sessionID, turnID, st)
		return err
	})
	if err == nil && ended {
		metrics.Turns.WithLabelValues(string(st.State)).Inc()
	}
	return err
}

// endTurnTx reports whether it ended the turn (false: it had already ended).
func (s *Service) endTurnTx(ctx context.Context, tx pgx.Tx, sessionID, turnID int64, st types.TurnStateEvent) (bool, error) {
	sess, err := s.Store.LockSession(ctx, tx, sessionID)
	if err != nil {
		return false, err
	}
	if cur, err := s.Store.TurnStateOf(ctx, tx, turnID); err != nil {
		return false, err
	} else if cur != nil && cur.State.Terminal() {
		return false, nil
	}
	turn, err := s.Store.EventByID(ctx, tx, turnID)
	if err != nil {
		return false, err
	}
	main, err := s.AgentActor(ctx, types.AgentMain)
	if err != nil {
		return false, err
	}
	usage, _, err := s.Store.TurnUsage(ctx, tx, turnID, main)
	if err != nil {
		return false, err
	}
	st.Usage = &usage
	if err := s.appendTo(ctx, tx, turn, main, st); err != nil {
		return false, err
	}
	if err := s.Store.AddSessionUsage(ctx, tx, sessionID, usage.Input, usage.Output, usage.Cached); err != nil {
		return false, err
	}
	if sess.ActiveTurnID != nil && *sess.ActiveTurnID == turnID {
		if err := s.Store.SetActiveTurn(ctx, tx, sessionID, nil); err != nil {
			return false, err
		}
	} else if sess.ActiveTurnID != nil {
		return true, nil // another turn is active; it will chain on its own
	}
	return true, s.chainNext(ctx, tx, sessionID, turnID)
}

// chainNext enqueues run_turn for the oldest queued turn after the one that
// ended (turns run oldest first, so none is older), if no job waits.
func (s *Service) chainNext(ctx context.Context, tx pgx.Tx, sessionID, after int64) error {
	queued, err := s.Store.QueuedTurns(ctx, tx, sessionID, after)
	if err != nil || len(queued) == 0 {
		return err
	}
	waiting, err := s.Store.PendingTurnJob(ctx, tx, sessionID, types.JobQueued)
	if err != nil || waiting {
		return err
	}
	return s.enqueueRun(ctx, tx, sessionID, queued[0])
}

// StartTurn marks the turn running and active. It returns false when the turn
// must not run now (finished, or an older turn is still open).
func (s *Service) StartTurn(ctx context.Context, sessionID, turnID int64) (bool, error) {
	main, err := s.AgentActor(ctx, types.AgentMain)
	if err != nil {
		return false, err
	}
	var ok bool
	err = s.Store.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := s.Store.LockSession(ctx, tx, sessionID); err != nil {
			return err
		}
		st, err := s.Store.TurnStateOf(ctx, tx, turnID)
		if err != nil || st == nil || st.State.Terminal() {
			return err
		}
		open, err := s.Store.OpenTurns(ctx, tx, sessionID, 1)
		if err != nil || len(open) == 0 || open[0] != turnID {
			return err // an older turn goes first; its end chains to this one
		}
		turn, err := s.Store.EventByID(ctx, tx, turnID)
		if err != nil {
			return err
		}
		if err := s.Store.SetActiveTurn(ctx, tx, sessionID, &turnID); err != nil {
			return err
		}
		ok = true
		if st.State == types.StateRunning {
			return nil // resuming after a crash
		}
		return s.appendTo(ctx, tx, turn, main, types.TurnStateEvent{State: types.StateRunning})
	})
	return ok, err
}

// Stop cancels queued turns and asks the active one to stop.
func (s *Service) Stop(ctx context.Context, sessionID, actor int64) error {
	main, err := s.AgentActor(ctx, types.AgentMain)
	if err != nil {
		return err
	}
	var ended []types.TurnState // counted once committed
	err = s.Store.Tx(ctx, func(tx pgx.Tx) error {
		ended = ended[:0]
		sess, err := s.Store.LockSession(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		queued, err := s.Store.QueuedTurns(ctx, tx, sessionID, 0)
		if err != nil {
			return err
		}
		for _, q := range queued {
			if sess.ActiveTurnID != nil && *sess.ActiveTurnID == q {
				continue
			}
			turn, err := s.Store.EventByID(ctx, tx, q)
			if err != nil {
				return err
			}
			if err := s.appendTo(ctx, tx, turn, main,
				types.TurnStateEvent{State: types.StateCancelled, Reason: "stop requested"}); err != nil {
				return err
			}
			ended = append(ended, types.StateCancelled)
		}
		if err := s.Store.CancelQueuedTurnJobs(ctx, tx, sessionID); err != nil {
			return err
		}
		if sess.ActiveTurnID == nil {
			return nil
		}
		active, err := s.Store.EventByID(ctx, tx, *sess.ActiveTurnID)
		if err != nil {
			return err
		}
		if err := s.appendTo(ctx, tx, active, actor, types.TurnCancelRequested{}); err != nil {
			return err
		}
		st, err := s.Store.TurnStateOf(ctx, tx, active.ID)
		if err != nil {
			return err
		}
		if st != nil && st.State == types.StateAwaitingApproval { // no worker runs it
			stopped, err := s.endTurnTx(ctx, tx, sessionID, active.ID, types.TurnStateEvent{State: types.StateStopped, Reason: "stop requested"})
			if stopped {
				ended = append(ended, types.StateStopped)
			}
			return err
		}
		return s.Store.NotifyCancel(ctx, tx, active.ID)
	})
	if err == nil {
		for _, st := range ended {
			metrics.Turns.WithLabelValues(string(st)).Inc()
		}
	}
	return err
}

// Approve records a client's decision on a pending tool call and resumes the turn.
func (s *Service) Approve(ctx context.Context, sessionID, actor int64, d types.ApprovalDecided) error {
	return s.Store.Tx(ctx, func(tx pgx.Tx) error {
		return s.decide(ctx, tx, sessionID, actor, d)
	})
}

// ApprovalTimeout rejects a still-undecided call; it does nothing otherwise.
func (s *Service) ApprovalTimeout(ctx context.Context, p ApprovalTimeoutPayload) error {
	turn, err := s.Store.EventByID(ctx, s.Store.Pool, p.TurnEventID)
	if err != nil {
		return err
	}
	main, err := s.AgentActor(ctx, types.AgentMain)
	if err != nil {
		return err
	}
	err = s.Store.Tx(ctx, func(tx pgx.Tx) error {
		return s.decide(ctx, tx, turn.SessionID, main, types.ApprovalDecided{
			CallID: p.CallID, Approve: false, Reason: "timeout"})
	})
	if errors.Is(err, ErrNoPendingApproval) {
		return nil
	}
	return err
}

func (s *Service) decide(ctx context.Context, tx pgx.Tx, sessionID, actor int64, d types.ApprovalDecided) error {
	sess, err := s.Store.LockSession(ctx, tx, sessionID)
	if err != nil {
		return err
	}
	if sess.ActiveTurnID == nil {
		return ErrNoPendingApproval
	}
	events, err := s.Store.TurnEvents(ctx, tx, *sess.ActiveTurnID)
	if err != nil {
		return err
	}
	v, err := ViewOf(events)
	if err != nil {
		return err
	}
	if _, decided := v.Decided[d.CallID]; v.State != types.StateAwaitingApproval || !v.Requested[d.CallID] || decided {
		return ErrNoPendingApproval
	}
	if err := s.appendTo(ctx, tx, events[0], actor, d); err != nil { // events[0] is the turn
		return err
	}
	return s.enqueueRun(ctx, tx, sessionID, events[0].ID)
}

// RequestApproval pauses the running turn on a tool call. The request, the
// awaiting_approval state and the timeout job commit together under the
// session lock, so a stop that committed first wins (ErrStopRequested) and a
// crash cannot leave a pause without its timeout.
func (s *Service) RequestApproval(ctx context.Context, sessionID, turnID int64, callID string) error {
	main, err := s.AgentActor(ctx, types.AgentMain)
	if err != nil {
		return err
	}
	return s.Store.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := s.Store.LockSession(ctx, tx, sessionID); err != nil {
			return err
		}
		events, err := s.Store.TurnEvents(ctx, tx, turnID)
		if err != nil {
			return err
		}
		v, err := ViewOf(events)
		if err != nil {
			return err
		}
		if v.Cancelled {
			return ErrStopRequested
		}
		turn := events[0]
		if err := s.appendTo(ctx, tx, turn, main, types.ApprovalRequested{CallID: callID}); err != nil {
			return err
		}
		if err := s.appendTo(ctx, tx, turn, main, types.TurnStateEvent{State: types.StateAwaitingApproval}); err != nil {
			return err
		}
		at := time.Now().Add(s.Cfg.Agent.Approval.Timeout.D())
		_, err = s.Store.Enqueue(ctx, tx, db.NewJob{
			Type: types.JobApprovalTimeout, Queue: types.QueueTurns,
			Payload: ApprovalTimeoutPayload{TurnEventID: turnID, CallID: callID}, RunAt: &at,
			MaxAttempts: s.Cfg.Queue.MaxAttempts,
		})
		return err
	})
}
