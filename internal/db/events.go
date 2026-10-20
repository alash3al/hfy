package db

import (
	"context"
	"encoding/json"
	"fmt"
	"uuid"

	"github.com/jackc/pgx/v5"

	"github.com/alash3al/hfy/internal/types"
)

// FindOrCreateActor returns the actor's internal id, creating the row on first use.
func (s *Store) FindOrCreateActor(ctx context.Context, typ types.ActorType, ref string) (int64, error) {
	key := string(typ) + "/" + ref
	if v, ok := s.actors.Load(key); ok {
		return v.(int64), nil
	}
	args := pgx.NamedArgs{"type": string(typ), "ref": ref}
	var id int64
	err := s.Pool.QueryRow(ctx, s.q("InsertActor"), args).Scan(&id)
	if IsNotFound(err) { // lost the race, or it already existed
		err = s.Pool.QueryRow(ctx, s.q("SelectActor"), args).Scan(&id)
	}
	if err != nil {
		return 0, err
	}
	s.actors.Store(key, id)
	return id, nil
}

// NewEvent describes an event to append.
type NewEvent struct {
	SessionID int64
	ParentID  *int64
	ActorID   int64
	Scope     string
	Payload   types.Payload
	RequestID *int64
}

// AppendEvent validates and inserts an event and notifies listeners on commit.
// Events are never updated.
func (s *Store) AppendEvent(ctx context.Context, q DBTX, in NewEvent) (EventRow, error) {
	if err := in.Payload.Validate(); err != nil {
		return EventRow{}, err
	}
	data, err := json.Marshal(in.Payload)
	if err != nil {
		return EventRow{}, fmt.Errorf("encode %s: %w", in.Payload.EventType(), err)
	}
	typ := in.Payload.EventType()
	row, err := one[EventRow](q.Query(ctx, s.q("InsertEvent"), pgx.NamedArgs{
		"session_id": in.SessionID, "parent_id": in.ParentID, "actor_id": in.ActorID,
		"scope": in.Scope, "type": string(typ), "data": data,
		"ephemeral": types.Ephemeral(typ), "request_id": in.RequestID,
	}))
	if err != nil {
		return EventRow{}, err
	}
	_, err = q.Exec(ctx, "SELECT pg_notify($1, $2)", ChanEvents, fmt.Sprintf("%d:%d", row.SessionID, row.ID))
	return row, err
}

func (s *Store) EventByID(ctx context.Context, id int64) (EventRow, error) {
	return one[EventRow](s.Pool.Query(ctx, s.q("SelectEventByID"), pgx.NamedArgs{"id": id}))
}

// EventIDByUUID maps a public event uuid (cursor, Last-Event-ID) to its internal id.
func (s *Store) EventIDByUUID(ctx context.Context, session int64, id uuid.UUID) (int64, error) {
	var out int64
	err := s.Pool.QueryRow(ctx, s.q("EventIDByUUID"), pgx.NamedArgs{"uuid": id, "session": session}).Scan(&out)
	return out, err
}

// EventsAfter returns up to limit events (durable and ephemeral) after an internal id.
func (s *Store) EventsAfter(ctx context.Context, session, after int64, limit int) ([]EventRow, error) {
	return collect[EventRow](s.Pool.Query(ctx, s.q("EventsAfter"),
		pgx.NamedArgs{"session": session, "after": after, "limit": limit}))
}

func (s *Store) LatestEventID(ctx context.Context, session int64) (int64, error) {
	var id int64
	err := s.Pool.QueryRow(ctx, s.q("LatestEventID"), pgx.NamedArgs{"session": session}).Scan(&id)
	return id, err
}

// HistoryTurnIDs returns turn ids newest first, keyset-paginated by before (nil = start).
func (s *Store) HistoryTurnIDs(ctx context.Context, session int64, before *int64, limit int) ([]int64, error) {
	rows, err := s.Pool.Query(ctx, s.q("HistoryTurns"),
		pgx.NamedArgs{"session": session, "before": before, "limit": limit})
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// HistoryEvents returns the durable events of the given turns, grouped by turn.
func (s *Store) HistoryEvents(ctx context.Context, session int64, turns []int64) ([]EventRow, error) {
	return collect[EventRow](s.Pool.Query(ctx, s.q("HistoryEvents"),
		pgx.NamedArgs{"session": session, "turns": turns}))
}

// TurnEvents returns the durable events of one turn (the turn event first).
func (s *Store) TurnEvents(ctx context.Context, q DBTX, turn int64) ([]EventRow, error) {
	return collect[EventRow](q.Query(ctx, s.q("TurnEvents"), pgx.NamedArgs{"turn": turn}))
}

// TurnIDByUUID resolves a turn uuid within a session.
func (s *Store) TurnIDByUUID(ctx context.Context, session int64, id uuid.UUID) (int64, error) {
	var out int64
	err := s.Pool.QueryRow(ctx, s.q("TurnIDByUUID"), pgx.NamedArgs{"uuid": id, "session": session}).Scan(&out)
	return out, err
}

// LoadContext returns the durable, prompt-relevant events of turns in
// (afterTurn, uptoTurn], grouped by turn. One query.
func (s *Store) LoadContext(ctx context.Context, session, afterTurn, uptoTurn int64) ([]EventRow, error) {
	return collect[EventRow](s.Pool.Query(ctx, s.q("LoadContext"),
		pgx.NamedArgs{"session": session, "after_turn": afterTurn, "upto_turn": uptoTurn}))
}

// TurnStateOf returns the latest turn.state payload of a turn (nil if none).
func (s *Store) TurnStateOf(ctx context.Context, q DBTX, turn int64) (*types.TurnStateEvent, error) {
	var raw []byte
	err := q.QueryRow(ctx, s.q("LatestTurnState"), pgx.NamedArgs{"turn": turn}).Scan(&raw)
	if IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st types.TurnStateEvent
	return &st, json.Unmarshal(raw, &st)
}

// QueuedTurns returns ids of turns whose latest state is queued, oldest first.
func (s *Store) QueuedTurns(ctx context.Context, q DBTX, session, after int64) ([]int64, error) {
	rows, err := q.Query(ctx, s.q("QueuedTurns"), pgx.NamedArgs{"session": session, "after": after})
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// TurnUsage sums the model.usage events of a turn; calls is how many there were.
func (s *Store) TurnUsage(ctx context.Context, turn int64) (u types.Usage, calls int64, err error) {
	err = s.Pool.QueryRow(ctx, s.q("TurnUsage"), pgx.NamedArgs{"turn": turn}).
		Scan(&u.Input, &u.Output, &u.Cached, &calls)
	return
}

// Summary returns the session's latest summary payload (nil if none).
func (s *Store) Summary(ctx context.Context, session int64) (*types.Summary, error) {
	var raw []byte
	err := s.Pool.QueryRow(ctx, s.q("SelectSummary"), pgx.NamedArgs{"session": session}).Scan(&raw)
	if IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sm types.Summary
	return &sm, json.Unmarshal(raw, &sm)
}

func (s *Store) SetSummary(ctx context.Context, q DBTX, session, summaryEvent int64) error {
	_, err := q.Exec(ctx, s.q("SetSummary"), pgx.NamedArgs{"id": session, "summary": summaryEvent})
	return err
}

func (s *Store) NotifyCancel(ctx context.Context, q DBTX, turn int64) error {
	_, err := q.Exec(ctx, "SELECT pg_notify($1, $2)", ChanCancel, fmt.Sprint(turn))
	return err
}

// InsertOutbound records an outbound HTTP call and returns its id.
func (s *Store) InsertOutbound(ctx context.Context, o OutboundRow) (int64, error) {
	nullable := func(b json.RawMessage) any {
		if len(b) == 0 {
			return nil
		}
		return []byte(b)
	}
	var errText any
	if o.Error != "" {
		errText = o.Error
	}
	var id int64
	err := s.Pool.QueryRow(ctx, s.q("InsertOutbound"), pgx.NamedArgs{
		"turn_event_id": o.TurnEventID, "kind": string(o.Kind), "url": o.URL,
		"request": nullable(o.Request), "response": nullable(o.Response),
		"status_code": o.StatusCode, "latency_ms": o.LatencyMS, "attempt": o.Attempt, "error": errText,
	}).Scan(&id)
	return id, err
}
