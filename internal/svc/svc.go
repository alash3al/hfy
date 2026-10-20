// Package svc holds the session mutations shared by the HTTP API and the
// workers: posting, stopping, approvals and ending turns. All of them take the
// session row lock, so they serialize per session.
package svc

import (
	"context"
	"encoding/json"
	"errors"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/types"
)

var (
	ErrNoActiveTurn      = errors.New("no active turn")
	ErrNoPendingApproval = errors.New("no pending approval for that call")
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
		if _, err := s.Store.AppendEvent(ctx, tx, db.NewEvent{SessionID: in.SessionID, ParentID: &turn.ID, ActorID: in.Actor,
			Scope: in.Scope, Payload: types.MessageText{Text: in.Text, Attachments: in.Attachments}}); err != nil {
			return err
		}
		if _, err := s.Store.AppendEvent(ctx, tx, db.NewEvent{SessionID: in.SessionID, ParentID: &turn.ID, ActorID: main,
			Scope: in.Scope, Payload: types.TurnStateEvent{State: types.StateQueued}}); err != nil {
			return err
		}
		if in.Text != "" && s.Cfg.Embeddings != nil {
			if err := s.EnqueueEmbed(ctx, tx); err != nil {
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

// EnqueueEmbed schedules an embed_events job unless one is already waiting.
func (s *Service) EnqueueEmbed(ctx context.Context, q db.DBTX) error {
	_, err := s.Store.Enqueue(ctx, q, db.NewJob{
		Type: types.JobEmbedEvents, Queue: types.QueueEmbeddings, Payload: struct{}{},
		MaxAttempts: s.Cfg.Queue.MaxAttempts, Key: "embed_events",
		DedupeStates: []types.JobState{types.JobQueued},
	})
	return err
}

// EndTurn appends the terminal state of a turn, books its usage on the
// session, clears the active pointer and chains to the next queued turn.
// It is idempotent: a turn that already ended is left alone.
func (s *Service) EndTurn(ctx context.Context, sessionID, turnID int64, st types.TurnStateEvent) error {
	return s.Store.Tx(ctx, func(tx pgx.Tx) error { return s.endTurnTx(ctx, tx, sessionID, turnID, st) })
}

func (s *Service) endTurnTx(ctx context.Context, tx pgx.Tx, sessionID, turnID int64, st types.TurnStateEvent) error {
	sess, err := s.Store.LockSession(ctx, tx, sessionID)
	if err != nil {
		return err
	}
	if cur, err := s.Store.TurnStateOf(ctx, tx, turnID); err != nil {
		return err
	} else if cur != nil && cur.State.Terminal() {
		return nil
	}
	turn, err := s.Store.EventByID(ctx, turnID)
	if err != nil {
		return err
	}
	main, err := s.AgentActor(ctx, types.AgentMain)
	if err != nil {
		return err
	}
	usage, _, err := s.Store.TurnUsage(ctx, turnID)
	if err != nil {
		return err
	}
	st.Usage = &usage
	if _, err := s.Store.AppendEvent(ctx, tx, db.NewEvent{SessionID: sessionID, ParentID: &turnID, ActorID: main,
		Scope: turn.Scope, Payload: st}); err != nil {
		return err
	}
	if err := s.Store.AddSessionUsage(ctx, tx, sessionID, usage.Input, usage.Output, usage.Cached); err != nil {
		return err
	}
	if sess.ActiveTurnID != nil && *sess.ActiveTurnID == turnID {
		if err := s.Store.SetActiveTurn(ctx, tx, sessionID, nil); err != nil {
			return err
		}
	} else if sess.ActiveTurnID != nil {
		return nil // another turn is active; it will chain on its own
	}
	return s.chainNext(ctx, tx, sessionID)
}

// chainNext enqueues run_turn for the oldest queued turn, if no job waits.
func (s *Service) chainNext(ctx context.Context, tx pgx.Tx, sessionID int64) error {
	queued, err := s.Store.QueuedTurns(ctx, tx, sessionID, 0)
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
		turn, err := s.Store.EventByID(ctx, turnID)
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
		_, err = s.Store.AppendEvent(ctx, tx, db.NewEvent{SessionID: sessionID, ParentID: &turnID, ActorID: main,
			Scope: turn.Scope, Payload: types.TurnStateEvent{State: types.StateRunning}})
		return err
	})
	return ok, err
}

// Stop cancels queued turns and asks the active one to stop.
func (s *Service) Stop(ctx context.Context, sessionID, actor int64) error {
	main, err := s.AgentActor(ctx, types.AgentMain)
	if err != nil {
		return err
	}
	return s.Store.Tx(ctx, func(tx pgx.Tx) error {
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
			row, err := s.Store.EventByID(ctx, q)
			if err != nil {
				return err
			}
			if _, err := s.Store.AppendEvent(ctx, tx, db.NewEvent{SessionID: sessionID, ParentID: &q, ActorID: main,
				Scope: row.Scope, Payload: types.TurnStateEvent{State: types.StateCancelled, Reason: "stop requested"}}); err != nil {
				return err
			}
		}
		if err := s.Store.CancelQueuedTurnJobs(ctx, tx, sessionID); err != nil {
			return err
		}
		if sess.ActiveTurnID == nil {
			return nil
		}
		active := *sess.ActiveTurnID
		row, err := s.Store.EventByID(ctx, active)
		if err != nil {
			return err
		}
		if _, err := s.Store.AppendEvent(ctx, tx, db.NewEvent{SessionID: sessionID, ParentID: &active, ActorID: actor,
			Scope: row.Scope, Payload: types.TurnCancelRequested{}}); err != nil {
			return err
		}
		st, err := s.Store.TurnStateOf(ctx, tx, active)
		if err != nil {
			return err
		}
		if st != nil && st.State == types.StateAwaitingApproval { // no worker runs it
			return s.endTurnTx(ctx, tx, sessionID, active, types.TurnStateEvent{State: types.StateStopped, Reason: "stop requested"})
		}
		return s.Store.NotifyCancel(ctx, tx, active)
	})
}

// Approve records a client's decision on a pending tool call and resumes the turn.
func (s *Service) Approve(ctx context.Context, sessionID, actor int64, d types.ApprovalDecided) error {
	return s.Store.Tx(ctx, func(tx pgx.Tx) error {
		return s.decide(ctx, tx, sessionID, actor, d)
	})
}

// ApprovalTimeout rejects a still-undecided call; it does nothing otherwise.
func (s *Service) ApprovalTimeout(ctx context.Context, p ApprovalTimeoutPayload) error {
	turn, err := s.Store.EventByID(ctx, p.TurnEventID)
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
	turnID := *sess.ActiveTurnID
	st, err := s.Store.TurnStateOf(ctx, tx, turnID)
	if err != nil {
		return err
	}
	if st == nil || st.State != types.StateAwaitingApproval {
		return ErrNoPendingApproval
	}
	events, err := s.Store.TurnEvents(ctx, tx, turnID)
	if err != nil {
		return err
	}
	requested, decided := false, false
	for _, e := range events {
		var p struct {
			CallID string `json:"call_id"`
		}
		switch e.Type {
		case types.EventApprovalRequested, types.EventApprovalDecided:
			if json.Unmarshal(e.Data, &p) == nil && p.CallID == d.CallID {
				if e.Type == types.EventApprovalRequested {
					requested = true
				} else {
					decided = true
				}
			}
		}
	}
	if !requested || decided {
		return ErrNoPendingApproval
	}
	turn, err := s.Store.EventByID(ctx, turnID)
	if err != nil {
		return err
	}
	if _, err := s.Store.AppendEvent(ctx, tx, db.NewEvent{SessionID: sessionID, ParentID: &turnID, ActorID: actor,
		Scope: turn.Scope, Payload: d}); err != nil {
		return err
	}
	return s.enqueueRun(ctx, tx, sessionID, turnID)
}

// ScheduleApprovalTimeout enqueues the job that rejects an undecided call.
func (s *Service) ScheduleApprovalTimeout(ctx context.Context, turnID int64, callID string) error {
	if s.Cfg.Agent.Approval == nil {
		return nil
	}
	at := time.Now().Add(s.Cfg.Agent.Approval.Timeout.D())
	_, err := s.Store.Enqueue(ctx, s.Store.Pool, db.NewJob{
		Type: types.JobApprovalTimeout, Queue: types.QueueTurns,
		Payload: ApprovalTimeoutPayload{TurnEventID: turnID, CallID: callID}, RunAt: &at,
		MaxAttempts: s.Cfg.Queue.MaxAttempts,
	})
	return err
}
