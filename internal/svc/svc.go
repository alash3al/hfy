// Package svc holds the conversation mutations shared by the HTTP API and the
// workers: posting, stopping, approvals and ending turns. All of them take the
// conversation row lock, so they serialize per conversation.
package svc

import (
	"cmp"
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
	// ErrNotTurnOwner: only the actor who started a turn may decide its calls.
	ErrNotTurnOwner = errors.New("only who started the turn may do that")
)

type RunTurnPayload struct {
	TurnEventID int64 `json:"turn_event_id"`
}

type ApprovalTimeoutPayload struct {
	TurnEventID int64  `json:"turn_event_id"`
	CallID      string `json:"call_id"`
}

type Service struct {
	store *db.Store
	cfg   *config.Config
}

func New(store *db.Store, cfg *config.Config) *Service { return &Service{store: store, cfg: cfg} }

func (s *Service) AgentActor(ctx context.Context, ref string) (int64, error) {
	return s.store.FindOrCreateActor(ctx, s.store.Pool, types.ActorAgent, ref)
}

type PostInput struct {
	ConversationID int64
	Actor          int64
	Scope          string
	Vars           map[string]string
	Text           string
	Attachments    []types.Attachment
	Files          []PostedFile
	Agent          string // the configured agent that runs the turn ("" = main)
}

// PostedFile carries one verified file attachment's facts into Post: the
// verified copy under conversations/ (never the upload slot), its type and
// size as stored, and the client's filename.
type PostedFile struct {
	Key  string
	Mime string
	Size int64
	Name string
}

// Post appends a turn with its user message and a queued state, and enqueues
// run_turn unless the conversation already has an active (or queued) turn.
func (s *Service) Post(ctx context.Context, in PostInput) (uuid.UUID, error) {
	main, err := s.AgentActor(ctx, cmp.Or(in.Agent, types.AgentMain))
	if err != nil {
		return uuid.UUID{}, err
	}
	var turnUUID uuid.UUID
	err = s.store.Tx(ctx, func(tx pgx.Tx) error {
		conv, err := s.store.LockConversation(ctx, tx, in.ConversationID)
		if err != nil {
			return err
		}
		turn, err := s.store.AppendEvent(ctx, tx, db.NewEvent{ConversationID: in.ConversationID, ActorID: in.Actor,
			Scope: in.Scope, Payload: types.Turn{Vars: in.Vars, Scope: in.Scope, Agent: in.Agent}})
		if err != nil {
			return err
		}
		turnUUID = turn.UUID
		msg, err := s.store.AppendEvent(ctx, tx, db.NewEvent{ConversationID: in.ConversationID, ParentID: &turn.ID, ActorID: in.Actor,
			Scope: in.Scope, Payload: types.MessageText{Text: in.Text, Attachments: in.Attachments}})
		if err != nil {
			return err
		}
		// The files index commits with the message it points at.
		for _, f := range in.Files {
			if err := s.store.InsertConversationFile(ctx, tx, db.NewConversationFile{
				ConversationID: in.ConversationID, EventID: msg.ID, ActorID: in.Actor,
				Key: f.Key, Mime: f.Mime, Size: f.Size, Name: f.Name,
			}); err != nil {
				return err
			}
		}
		if _, err := s.store.AppendEvent(ctx, tx, db.NewEvent{ConversationID: in.ConversationID, ParentID: &turn.ID, ActorID: main,
			Scope: in.Scope, Payload: types.TurnStateEvent{State: types.StateQueued}}); err != nil {
			return err
		}
		if in.Text != "" && s.cfg.Embeddings != nil {
			if err := s.EnqueueEmbed(ctx, tx, msg.ID); err != nil {
				return err
			}
		}
		if conv.ActiveTurnID != nil {
			return nil // the running (or paused) turn chains to this one when it ends
		}
		queued, err := s.store.PendingTurnJob(ctx, tx, in.ConversationID, types.JobQueued)
		if err != nil || queued {
			return err
		}
		return s.enqueueRun(ctx, tx, in.ConversationID, turn.ID)
	})
	return turnUUID, err
}

func (s *Service) enqueueRun(ctx context.Context, q db.DBTX, conversation, turn int64) error {
	_, err := s.store.Enqueue(ctx, q, db.NewJob{
		Type: types.JobRunTurn, Queue: types.QueueTurns, Payload: RunTurnPayload{TurnEventID: turn},
		MaxAttempts: s.cfg.Queue.MaxAttempts, Key: db.ConversationKey(conversation),
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
	_, err := s.store.Enqueue(ctx, q, db.NewJob{
		Type: types.JobEmbedEvents, Queue: types.QueueEmbeddings, Payload: EmbedPayload{EventIDs: eventIDs},
		MaxAttempts: s.cfg.Queue.MaxAttempts, Key: key,
	})
	return err
}

// appendTo appends an event under a turn, copying its scope. Inside a
// mutation every read and write goes through tx: a second pool connection
// taken while the transaction holds one deadlocks once the pool is exhausted.
func (s *Service) appendTo(ctx context.Context, tx pgx.Tx, turn db.EventRow, actor int64, p types.Payload) error {
	_, err := s.store.AppendEvent(ctx, tx, db.NewEvent{ConversationID: turn.ConversationID, ParentID: &turn.ID,
		ActorID: actor, Scope: turn.Scope, Payload: p})
	return err
}

// agentOf returns the actor of the agent a turn runs as: it authors the
// turn's agent-side events (state, approvals, replies).
func (s *Service) agentOf(ctx context.Context, q db.DBTX, turn db.EventRow) (int64, error) {
	t, err := turn.Payload[types.Turn]()
	if err != nil {
		return 0, err
	}
	return s.store.FindOrCreateActor(ctx, q, types.ActorAgent, t.AgentName())
}

// EndTurn appends the terminal state of a turn, books its usage on the
// conversation, clears the active pointer and chains to the next queued turn.
// It is idempotent: a turn that already ended is left alone.
func (s *Service) EndTurn(ctx context.Context, conversationID, turnID int64, st types.TurnStateEvent) error {
	var ended bool
	err := s.store.Tx(ctx, func(tx pgx.Tx) (err error) {
		ended, err = s.endTurnTx(ctx, tx, conversationID, turnID, st)
		return err
	})
	if err == nil && ended {
		metrics.Turns.WithLabelValues(string(st.State)).Inc()
	}
	return err
}

// endTurnTx reports whether it ended the turn (false: it had already ended).
func (s *Service) endTurnTx(ctx context.Context, tx pgx.Tx, conversationID, turnID int64, st types.TurnStateEvent) (bool, error) {
	conv, err := s.store.LockConversation(ctx, tx, conversationID)
	if err != nil {
		return false, err
	}
	if cur, err := s.store.TurnStateOf(ctx, tx, turnID); err != nil {
		return false, err
	} else if cur != nil && cur.State.Terminal() {
		return false, nil
	}
	turn, err := s.store.EventByID(ctx, tx, turnID)
	if err != nil {
		return false, err
	}
	main, err := s.agentOf(ctx, tx, turn)
	if err != nil {
		return false, err
	}
	// A tool that ran and asked questions holds its finished result until its
	// questions are decided. If the turn now ends first (stop or exhaustion),
	// the model still gets its one tool_result, as an unanswered one.
	events, err := s.store.TurnEvents(ctx, tx, turnID)
	if err != nil {
		return false, err
	}
	v, err := ViewOf(events)
	if err != nil {
		return false, err
	}
	for _, c := range v.Calls {
		req := v.RequestData[c.CallID]
		if _, decided := v.Decided[c.CallID]; req.Held == nil || v.Done[c.CallID] || decided {
			continue
		}
		text := "The user stopped before answering."
		if h := req.Held.Content; h != "" {
			text = h + "\n\n" + text
		}
		if err := s.appendTo(ctx, tx, events[0], main, types.ToolResult{CallID: c.CallID,
			Content: text, Structured: req.Held.Structured, UI: req.UI}); err != nil {
			return false, err
		}
	}
	usage, _, err := s.store.TurnUsage(ctx, tx, turnID, main)
	if err != nil {
		return false, err
	}
	st.Usage = &usage
	if err := s.appendTo(ctx, tx, turn, main, st); err != nil {
		return false, err
	}
	if err := s.store.AddConversationUsage(ctx, tx, conversationID, usage.Input, usage.Output, usage.Cached); err != nil {
		return false, err
	}
	if err := s.settle(ctx, tx, conversationID); err != nil {
		return false, err
	}
	if conv.ActiveTurnID != nil && *conv.ActiveTurnID == turnID {
		if err := s.store.SetActiveTurn(ctx, tx, conversationID, nil); err != nil {
			return false, err
		}
	} else if conv.ActiveTurnID != nil {
		return true, nil // another turn is active; it will chain on its own
	}
	return true, s.chainNext(ctx, tx, conversationID, turnID)
}

// settle moves the conversation's settled_until watermark past the turns that have
// ended, in the transaction that ended them. It can lag but never pass an
// open turn: turn states are final, and only Post creates turns, under the
// same conversation lock this runs under.
func (s *Service) settle(ctx context.Context, tx pgx.Tx, conversationID int64) error {
	open, err := s.store.OpenTurns(ctx, tx, conversationID, 1)
	if err != nil {
		return err
	}
	var oldest *int64
	if len(open) > 0 {
		oldest = &open[0]
	}
	return s.store.SettleTurns(ctx, tx, conversationID, oldest)
}

// chainNext enqueues run_turn for the oldest queued turn after the one that
// ended (turns run oldest first, so none is older), if no job waits.
func (s *Service) chainNext(ctx context.Context, tx pgx.Tx, conversationID, after int64) error {
	queued, err := s.store.QueuedTurns(ctx, tx, conversationID, after)
	if err != nil || len(queued) == 0 {
		return err
	}
	waiting, err := s.store.PendingTurnJob(ctx, tx, conversationID, types.JobQueued)
	if err != nil || waiting {
		return err
	}
	return s.enqueueRun(ctx, tx, conversationID, queued[0])
}

// StartTurn marks the turn running and active. It returns false when the turn
// must not run now (finished, or an older turn is still open).
func (s *Service) StartTurn(ctx context.Context, conversationID, turnID int64) (bool, error) {
	var ok bool
	err := s.store.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := s.store.LockConversation(ctx, tx, conversationID); err != nil {
			return err
		}
		st, err := s.store.TurnStateOf(ctx, tx, turnID)
		if err != nil || st == nil || st.State.Terminal() {
			return err
		}
		open, err := s.store.OpenTurns(ctx, tx, conversationID, 1)
		if err != nil || len(open) == 0 || open[0] != turnID {
			return err // an older turn goes first; its end chains to this one
		}
		turn, err := s.store.EventByID(ctx, tx, turnID)
		if err != nil {
			return err
		}
		if err := s.store.SetActiveTurn(ctx, tx, conversationID, &turnID); err != nil {
			return err
		}
		ok = true
		if st.State == types.StateRunning {
			return nil // resuming after a crash
		}
		agent, err := s.agentOf(ctx, tx, turn)
		if err != nil {
			return err
		}
		return s.appendTo(ctx, tx, turn, agent, types.TurnStateEvent{State: types.StateRunning})
	})
	return ok, err
}

// Stop stops the caller's own turns: it cancels their queued ones and asks
// the active one to stop if they started it. In a shared conversation nobody stops
// another person's turn; everyone else's keep their place in line.
func (s *Service) Stop(ctx context.Context, conversationID, actor int64) error {
	var ended []types.TurnState // counted once committed
	err := s.store.Tx(ctx, func(tx pgx.Tx) error {
		ended = ended[:0]
		conv, err := s.store.LockConversation(ctx, tx, conversationID)
		if err != nil {
			return err
		}
		queued, err := s.store.QueuedTurns(ctx, tx, conversationID, 0)
		if err != nil {
			return err
		}
		for _, q := range queued {
			if conv.ActiveTurnID != nil && *conv.ActiveTurnID == q {
				continue
			}
			turn, err := s.store.EventByID(ctx, tx, q)
			if err != nil {
				return err
			}
			if turn.ActorID != actor {
				continue
			}
			agent, err := s.agentOf(ctx, tx, turn)
			if err != nil {
				return err
			}
			if err := s.appendTo(ctx, tx, turn, agent,
				types.TurnStateEvent{State: types.StateCancelled, Reason: "stop requested"}); err != nil {
				return err
			}
			ended = append(ended, types.StateCancelled)
		}
		if err := s.settle(ctx, tx, conversationID); err != nil {
			return err
		}
		var active db.EventRow
		if conv.ActiveTurnID != nil {
			if active, err = s.store.EventByID(ctx, tx, *conv.ActiveTurnID); err != nil {
				return err
			}
			if active.ActorID != actor {
				return nil // another person's turn runs; it chains to the next one when it ends
			}
		}
		// At most one run_turn job waits. With the caller's turn active it is
		// that turn's resume, dropped as the turn stops (its end chains on).
		// With none active it is the next queued turn's, maybe one just
		// cancelled, so it is dropped and the line restarts at whoever is next.
		if err := s.store.CancelQueuedTurnJobs(ctx, tx, conversationID); err != nil {
			return err
		}
		if conv.ActiveTurnID == nil {
			return s.chainNext(ctx, tx, conversationID, 0)
		}
		if err := s.appendTo(ctx, tx, active, actor, types.TurnCancelRequested{}); err != nil {
			return err
		}
		st, err := s.store.TurnStateOf(ctx, tx, active.ID)
		if err != nil {
			return err
		}
		if st != nil && st.State == types.StateAwaitingApproval { // no worker runs it
			stopped, err := s.endTurnTx(ctx, tx, conversationID, active.ID, types.TurnStateEvent{State: types.StateStopped, Reason: "stop requested"})
			if stopped {
				ended = append(ended, types.StateStopped)
			}
			return err
		}
		return s.store.NotifyCancel(ctx, tx, active.ID)
	})
	if err == nil {
		for _, st := range ended {
			metrics.Turns.WithLabelValues(string(st)).Inc()
		}
	}
	return err
}

// Approve records a client's decision on a pending tool call and resumes the
// turn. Only the actor who started the turn decides: in a shared conversation
// nobody approves what another person's turn is about to do.
func (s *Service) Approve(ctx context.Context, conversationID, actor int64, d types.ApprovalDecided) error {
	return s.store.Tx(ctx, func(tx pgx.Tx) error {
		turn, call, req, err := s.pendingCall(ctx, tx, conversationID, d.CallID)
		if err != nil {
			return err
		}
		if turn.ActorID != actor {
			return ErrNotTurnOwner
		}
		// Answers go to a call that is asking questions: hfy.ask_user picks
		// them from its arguments, a tool that asked carries them now in its
		// approval request.
		qraw := req.Questions
		if call.Name == AskUserTool && len(qraw) == 0 {
			qraw = call.Arguments
		}
		if len(d.Answers) > 0 && len(qraw) == 0 {
			return badf("answers are only accepted for %s calls", AskUserTool)
		}
		if d.Approve && len(qraw) > 0 {
			// The stored questions were validated before the turn paused;
			// the answers themselves are the client's input: 400s, here.
			qs, err := ParseQuestions(qraw)
			if err != nil {
				return badf("the stored questions are invalid: %v", err)
			}
			if d.Answers, err = ParseAnswers(qs, d.Answers); err != nil {
				return err
			}
		}
		return s.decide(ctx, tx, turn, actor, d)
	})
}

// ApprovalTimeout rejects a still-undecided call; it does nothing otherwise.
func (s *Service) ApprovalTimeout(ctx context.Context, p ApprovalTimeoutPayload) error {
	turn, err := s.store.EventByID(ctx, s.store.Pool, p.TurnEventID)
	if err != nil {
		return err
	}
	main, err := s.agentOf(ctx, s.store.Pool, turn)
	if err != nil {
		return err
	}
	err = s.store.Tx(ctx, func(tx pgx.Tx) error {
		turn, _, _, err := s.pendingCall(ctx, tx, turn.ConversationID, p.CallID)
		if err != nil {
			return err
		}
		return s.decide(ctx, tx, turn, main, types.ApprovalDecided{CallID: p.CallID, Approve: false, Reason: "timeout"})
	})
	if errors.Is(err, ErrNoPendingApproval) {
		return nil
	}
	return err
}

// pendingCall locks the conversation and returns the active turn if callID is
// awaiting a decision in it, with that call as stored and its approval
// request's payload (questions, card and held result, if any).
func (s *Service) pendingCall(ctx context.Context, tx pgx.Tx, conversationID int64, callID string) (db.EventRow, types.ToolCall, types.ApprovalRequested, error) {
	conv, err := s.store.LockConversation(ctx, tx, conversationID)
	if err != nil {
		return db.EventRow{}, types.ToolCall{}, types.ApprovalRequested{}, err
	}
	if conv.ActiveTurnID == nil {
		return db.EventRow{}, types.ToolCall{}, types.ApprovalRequested{}, ErrNoPendingApproval
	}
	events, err := s.store.TurnEvents(ctx, tx, *conv.ActiveTurnID)
	if err != nil {
		return db.EventRow{}, types.ToolCall{}, types.ApprovalRequested{}, err
	}
	v, err := ViewOf(events)
	if err != nil {
		return db.EventRow{}, types.ToolCall{}, types.ApprovalRequested{}, err
	}
	if _, decided := v.Decided[callID]; v.State != types.StateAwaitingApproval || !v.Requested[callID] || decided {
		return db.EventRow{}, types.ToolCall{}, types.ApprovalRequested{}, ErrNoPendingApproval
	}
	for _, c := range v.Calls {
		if c.CallID == callID {
			return events[0], c, v.RequestData[callID], nil // the turn, the call it paused on, its request
		}
	}
	return db.EventRow{}, types.ToolCall{}, types.ApprovalRequested{}, ErrNoPendingApproval
}

// decide records d on turn (from pendingCall, in the same tx) and resumes it.
func (s *Service) decide(ctx context.Context, tx pgx.Tx, turn db.EventRow, actor int64, d types.ApprovalDecided) error {
	if err := s.appendTo(ctx, tx, turn, actor, d); err != nil {
		return err
	}
	return s.enqueueRun(ctx, tx, turn.ConversationID, turn.ID)
}

// RequestApproval pauses the running turn on a tool call. The request, the
// awaiting_approval state and the timeout job commit together under the
// conversation lock, so a stop that committed first wins (ErrStopRequested) and a
// crash cannot leave a pause without its timeout. req carries the call id and,
// for a tool that asks the viewer questions (design B), its questions, display
// card and held result.
// timeout is the agent's approval.timeout.
func (s *Service) RequestApproval(ctx context.Context, conversationID, turnID int64, req types.ApprovalRequested, timeout time.Duration) error {
	return s.store.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := s.store.LockConversation(ctx, tx, conversationID); err != nil {
			return err
		}
		events, err := s.store.TurnEvents(ctx, tx, turnID)
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
		if req.CallID == "" {
			return errors.New("approval request: call_id required")
		}
		turn := events[0]
		main, err := s.agentOf(ctx, tx, turn)
		if err != nil {
			return err
		}
		if err := s.appendTo(ctx, tx, turn, main, req); err != nil {
			return err
		}
		if err := s.appendTo(ctx, tx, turn, main, types.TurnStateEvent{State: types.StateAwaitingApproval}); err != nil {
			return err
		}
		at := time.Now().Add(timeout)
		_, err = s.store.Enqueue(ctx, tx, db.NewJob{
			Type: types.JobApprovalTimeout, Queue: types.QueueTurns,
			Payload: ApprovalTimeoutPayload{TurnEventID: turnID, CallID: req.CallID}, RunAt: &at,
			MaxAttempts: s.cfg.Queue.MaxAttempts,
		})
		return err
	})
}
