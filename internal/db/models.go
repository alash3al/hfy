package db

import (
	"encoding/json"
	"time"
	"uuid"

	"github.com/alash3al/hfy/internal/types"
)

type SessionRow struct {
	ID           int64     `db:"id"`
	UUID         uuid.UUID `db:"uuid"`
	ActiveTurnID *int64    `db:"active_turn_id"`
	SummaryID    *int64    `db:"summary_id"`
	InputTokens  int64     `db:"input_tokens"`
	OutputTokens int64     `db:"output_tokens"`
	CachedTokens int64     `db:"cached_tokens"`
	CreatedAt    time.Time `db:"created_at"`
	UpdatedAt    time.Time `db:"updated_at"`
}

// EventRow is a session_events row joined with its parent's uuid and its actor.
type EventRow struct {
	ID         int64           `db:"id"`
	UUID       uuid.UUID       `db:"uuid"`
	SessionID  int64           `db:"session_id"`
	ParentID   *int64          `db:"parent_id"`
	ParentUUID *uuid.UUID      `db:"parent_uuid"`
	ActorID    int64           `db:"actor_id"`
	ActorUUID  uuid.UUID       `db:"actor_uuid"`
	ActorType  string          `db:"actor_type"`
	ActorRef   string          `db:"actor_ref"`
	Scope      string          `db:"scope"`
	Type       types.EventType `db:"type"`
	Data       json.RawMessage `db:"data"`
	Ephemeral  bool            `db:"ephemeral"`
	RequestID  *int64          `db:"request_id"`
	CreatedAt  time.Time       `db:"created_at"`
}

// TurnID is the id of the turn this event belongs to (itself for a turn event).
func (e EventRow) TurnID() int64 {
	if e.ParentID != nil {
		return *e.ParentID
	}
	return e.ID
}

// Envelope converts the row to the public event shape: uuids only.
func (e EventRow) Envelope() types.Event {
	return types.Event{
		ID:     e.UUID,
		Type:   e.Type,
		Parent: e.ParentUUID,
		Actor: types.Actor{
			ID: e.ActorUUID, Type: types.ActorType(e.ActorType), Ref: e.ActorRef,
		},
		Data:      e.Data,
		CreatedAt: e.CreatedAt,
	}
}

// Payload decodes the row's data through the registry.
func (e EventRow) Payload() (types.Payload, error) { return e.Envelope().Decode() }

type JobRow struct {
	ID             int64           `db:"id"`
	Type           types.JobType   `db:"type"`
	Queue          string          `db:"queue"`
	Payload        json.RawMessage `db:"payload"`
	State          types.JobState  `db:"state"`
	RunAt          time.Time       `db:"run_at"`
	Attempts       int             `db:"attempts"`
	MaxAttempts    int             `db:"max_attempts"`
	LeaseUntil     *time.Time      `db:"lease_until"`
	ConcurrencyKey *string         `db:"concurrency_key"`
	LastError      *string         `db:"last_error"`
	CreatedAt      time.Time       `db:"created_at"`
}

// OutboundRow is a row to insert into outbound_requests.
type OutboundRow struct {
	TurnEventID *int64
	Kind        types.OutboundKind
	URL         string
	Request     json.RawMessage
	Response    json.RawMessage
	StatusCode  int
	LatencyMS   int
	Attempt     int
	Error       string
}
