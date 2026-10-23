package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/mcp"
	"github.com/alash3al/hfy/internal/provider"
	"github.com/alash3al/hfy/internal/types"
)

type CallInfo struct {
	ID   string
	Name string // full <server>.<tool> when the tool is known
	Args json.RawMessage
}

type StepOut struct {
	Reasoning string
	Text      string
	Usage     types.Usage
	Model     string
	RequestID int64
}

// Sink is what differs between the main agent (persists everything as events)
// and sub-agents / the summarizer (persist usage only).
type Sink interface {
	TextDelta(string)
	ReasoningDelta(string)
	// Restart drops what the current model call streamed: the provider is
	// answering again from the start (a retried attempt).
	Restart()
	Step(ctx context.Context, s StepOut) error
	CallStarted(ctx context.Context, c CallInfo) error
	CallFinished(ctx context.Context, c CallInfo, r mcp.Result) error
}

// recorder writes outbound_requests rows for a turn.
func (rt *Runtime) recorder(turnID *int64, kind types.OutboundKind) func(provider.Record) int64 {
	return func(r provider.Record) int64 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		id, err := rt.Store.InsertOutbound(ctx, db.OutboundRow{
			TurnEventID: turnID, Kind: kind, URL: r.URL, Request: r.Request, Response: r.Response,
			StatusCode: r.Status, LatencyMS: r.LatencyMS, Attempt: r.Attempt, Error: r.Error,
		})
		if err != nil { // the call itself goes on; only its trace is lost
			slog.Warn("record outbound request", "turn", turnID, "kind", kind, "url", r.URL, "err", err)
			return 0
		}
		return id
	}
}

// turnSink persists the main agent's run as events under its turn.
type turnSink struct {
	rt    *Runtime
	env   *TurnEnv
	actor int64
	ctx   context.Context // the turn's context, for batched deltas

	flushMu sync.Mutex // one flush at a time, so no delta lands after its step's message
	mu      sync.Mutex
	text    string // pending delta batch
	reason  string
	timer   *time.Timer
	partial string // everything streamed in the current step, for a stop
	lastReq int64
}

func newTurnSink(ctx context.Context, rt *Runtime, env *TurnEnv, actor int64) *turnSink {
	return &turnSink{rt: rt, env: env, actor: actor, ctx: ctx}
}

func (s *turnSink) append(ctx context.Context, p types.Payload, req *int64) error {
	_, err := s.appendRow(ctx, s.rt.Store.Pool, p, req)
	return err
}

func (s *turnSink) appendRow(ctx context.Context, q db.DBTX, p types.Payload, req *int64) (db.EventRow, error) {
	var rid *int64
	if req != nil && *req != 0 {
		rid = req
	}
	return s.rt.Store.AppendEvent(ctx, q, db.NewEvent{
		ConversationID: s.env.ConversationID, ParentID: &s.env.TurnID, ActorID: s.actor,
		Scope: s.env.Scope, Payload: p, RequestID: rid})
}

func (s *turnSink) TextDelta(t string) {
	s.mu.Lock()
	s.text += t
	s.partial += t
	s.armLocked()
	s.mu.Unlock()
}

func (s *turnSink) ReasoningDelta(t string) {
	s.mu.Lock()
	s.reason += t
	s.armLocked()
	s.mu.Unlock()
}

// Restart forgets the failed attempt's text, so a stop keeps only the answer
// being streamed now. Deltas already sent stay in the live preview, which the
// step's message.text replaces.
func (s *turnSink) Restart() {
	s.mu.Lock()
	s.text, s.reason, s.partial = "", "", ""
	s.mu.Unlock()
}

// armLocked batches deltas: one event per ~100 ms instead of one per token.
func (s *turnSink) armLocked() {
	if s.timer == nil {
		s.timer = time.AfterFunc(100*time.Millisecond, func() { s.flush(s.ctx) })
	}
}

func (s *turnSink) flush(ctx context.Context) {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	s.mu.Lock()
	text, reason := s.text, s.reason
	s.text, s.reason, s.timer = "", "", nil
	s.mu.Unlock()
	// Deltas are a live preview; the step's message.text is the record. A lost
	// delta is logged, not fatal.
	if reason != "" {
		s.warn(s.append(ctx, types.ReasoningDelta{Text: types.Clean(reason)}, nil), "reasoning delta")
	}
	if text != "" {
		s.warn(s.append(ctx, types.TextDelta{Text: types.Clean(text)}, nil), "text delta")
	}
}

func (s *turnSink) warn(err error, what string) {
	if err != nil && s.ctx.Err() == nil {
		slog.Warn("append "+what, "conversation", s.env.ConversationID, "turn", s.env.TurnID, "err", err)
	}
}

func (s *turnSink) Step(ctx context.Context, o StepOut) error {
	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
	}
	s.mu.Unlock()
	s.flush(ctx)
	s.mu.Lock()
	s.partial = ""
	s.mu.Unlock()
	s.lastReq = o.RequestID
	if o.Reasoning != "" {
		if err := s.append(ctx, types.Reasoning{Text: o.Reasoning}, &o.RequestID); err != nil {
			return err
		}
	}
	if o.Text != "" {
		err := s.rt.Store.Tx(ctx, func(tx pgx.Tx) error { // the reply and its embed job commit together
			row, err := s.appendRow(ctx, tx, types.MessageText{Text: o.Text}, &o.RequestID)
			if err != nil || s.rt.Cfg.Embeddings == nil {
				return err
			}
			return s.rt.Svc.EnqueueEmbed(ctx, tx, row.ID)
		})
		if err != nil {
			return err
		}
	}
	return s.append(ctx, types.ModelUsage{Model: o.Model, Input: o.Usage.Input, Output: o.Usage.Output,
		Cached: o.Usage.Cached}, &o.RequestID)
}

// FlushPartial keeps what was streamed before a stop as a regular message.
func (s *turnSink) FlushPartial(ctx context.Context) {
	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
	}
	partial := s.partial
	s.partial = ""
	s.mu.Unlock()
	s.flush(ctx)
	if partial != "" {
		if err := s.append(ctx, types.MessageText{Text: types.Clean(partial)}, nil); err != nil {
			slog.Error("keep partial reply", "conversation", s.env.ConversationID, "turn", s.env.TurnID, "err", err)
		}
	}
}

func (s *turnSink) CallStarted(ctx context.Context, c CallInfo) error {
	return s.append(ctx, types.ToolCall{Name: c.Name, CallID: c.ID, Arguments: c.Args}, &s.lastReq)
}

func (s *turnSink) CallFinished(ctx context.Context, c CallInfo, r mcp.Result) error {
	return s.append(ctx, types.ToolResult{CallID: c.ID, Content: r.Content, IsError: r.IsError, Structured: r.Structured}, nil)
}

// quietSink is for sub-agents and the summarizer: of their runs only
// model.usage and the outbound trace are kept.
type quietSink struct {
	rt    *Runtime
	env   *TurnEnv
	actor int64
}

func (s *quietSink) TextDelta(string)      {}
func (s *quietSink) ReasoningDelta(string) {}
func (s *quietSink) Restart()              {}

func (s *quietSink) Step(ctx context.Context, o StepOut) error {
	var rid *int64
	if o.RequestID != 0 {
		rid = &o.RequestID
	}
	_, err := s.rt.Store.AppendEvent(ctx, s.rt.Store.Pool, db.NewEvent{
		ConversationID: s.env.ConversationID, ParentID: &s.env.TurnID, ActorID: s.actor, Scope: s.env.Scope,
		Payload:   types.ModelUsage{Model: o.Model, Input: o.Usage.Input, Output: o.Usage.Output, Cached: o.Usage.Cached},
		RequestID: rid})
	return err
}

func (s *quietSink) CallStarted(context.Context, CallInfo) error              { return nil }
func (s *quietSink) CallFinished(context.Context, CallInfo, mcp.Result) error { return nil }
