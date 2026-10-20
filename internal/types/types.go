// Package types is the shared vocabulary: enums, the event envelope and the
// payload structs. It imports nothing internal and no third-party packages.
package types

import (
	"encoding/json"
	"strings"
	"time"
	"uuid"
)

type EventType string

const (
	EventTurn                EventType = "turn"
	EventTurnState           EventType = "turn.state"
	EventTurnCancelRequested EventType = "turn.cancel_requested"
	EventMessageText         EventType = "message.text"
	EventReasoning           EventType = "reasoning"
	EventToolCall            EventType = "tool_call"
	EventToolCallInterrupted EventType = "tool_call.interrupted"
	EventToolResult          EventType = "tool_result"
	EventApprovalRequested   EventType = "approval.requested"
	EventApprovalDecided     EventType = "approval.decided"
	EventModelUsage          EventType = "model.usage"
	EventSummary             EventType = "summary"
	EventTextDelta           EventType = "text.delta"
	EventReasoningDelta      EventType = "reasoning.delta"
)

type TurnState string

const (
	StateQueued           TurnState = "queued"
	StateRunning          TurnState = "running"
	StateAwaitingApproval TurnState = "awaiting_approval"
	StateCompleted        TurnState = "completed"
	StateFailed           TurnState = "failed"
	StateStopped          TurnState = "stopped"
	StateCancelled        TurnState = "cancelled"
)

// Terminal reports whether no further work happens in this state.
func (s TurnState) Terminal() bool {
	switch s {
	case StateCompleted, StateFailed, StateStopped, StateCancelled:
		return true
	}
	return false
}

type ActorType string

const (
	ActorExternal  ActorType = "external"
	ActorAgent     ActorType = "agent"
	ActorAnonymous ActorType = "anonymous"
)

const (
	AgentMain       = "main"
	AgentSummarizer = "summarizer"
	// NoScope is the fixed scope and anonymous ref used when auth is off.
	NoScope = "n/a"
)

type JobType string

const (
	JobRunTurn         JobType = "run_turn"
	JobEmbedEvents     JobType = "embed_events"
	JobRetentionClean  JobType = "retention_cleanup"
	JobApprovalTimeout JobType = "approval_timeout"
)

type JobState string

const (
	JobQueued  JobState = "queued"
	JobRunning JobState = "running"
	JobDone    JobState = "done"
	JobFailed  JobState = "failed"
)

type OutboundKind string

const (
	OutboundModel      OutboundKind = "model"
	OutboundSummarizer OutboundKind = "summarizer"
	OutboundEmbedding  OutboundKind = "embedding"
	OutboundMCPRemote  OutboundKind = "mcp_remote"
)

// Queue names.
const (
	QueueTurns       = "turns"
	QueueEmbeddings  = "embeddings"
	QueueMaintenance = "maintenance"
)

// Actor is how an actor appears in API responses.
type Actor struct {
	ID   uuid.UUID `json:"uuid"`
	Type ActorType `json:"type"`
	Ref  string    `json:"ref"`
}

// Event is the one envelope used by the live stream and history.
type Event struct {
	ID        uuid.UUID       `json:"id"`
	Type      EventType       `json:"type"`
	Parent    *uuid.UUID      `json:"parent"`
	Actor     Actor           `json:"actor"`
	Data      json.RawMessage `json:"data"`
	CreatedAt time.Time       `json:"created_at"`
}

// Payload is implemented by every event payload struct.
type Payload interface {
	EventType() EventType
	Validate() error
}

// Usage is token accounting for a model call or a whole turn.
type Usage struct {
	Input  int64 `json:"input"`
	Output int64 `json:"output"`
	Cached int64 `json:"cached"`
}

func (u *Usage) Add(o Usage) {
	u.Input += o.Input
	u.Output += o.Output
	u.Cached += o.Cached
}

func (u Usage) Total() int64 { return u.Input + u.Output }

type Attachment struct {
	URL  string `json:"url"`
	Mime string `json:"mime"`
}

// Clean makes model and tool output safe to store: invalid UTF-8 is replaced
// and NUL bytes (rejected by Postgres jsonb) are dropped.
func Clean(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "�"), "\x00", "")
}
