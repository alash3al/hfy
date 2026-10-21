package types

import (
	"encoding/json"
	"errors"
)

// Turn is the root event of a turn.
type Turn struct {
	Vars  map[string]string `json:"vars"`
	Scope string            `json:"scope"`
}

func (Turn) EventType() EventType { return EventTurn }
func (Turn) Validate() error      { return nil }

type TurnStateEvent struct {
	State  TurnState `json:"state"`
	Reason string    `json:"reason,omitempty"`
	Error  string    `json:"error,omitempty"`
	Usage  *Usage    `json:"usage,omitempty"`
}

func (TurnStateEvent) EventType() EventType { return EventTurnState }
func (s TurnStateEvent) Validate() error {
	if s.State == "" {
		return errors.New("turn.state: state required")
	}
	return nil
}

type TurnCancelRequested struct{}

func (TurnCancelRequested) EventType() EventType { return EventTurnCancelRequested }
func (TurnCancelRequested) Validate() error      { return nil }

type MessageText struct {
	Text        string       `json:"text"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

func (MessageText) EventType() EventType { return EventMessageText }
func (m MessageText) Validate() error {
	if m.Text == "" && len(m.Attachments) == 0 {
		return errors.New("message.text: text or attachments required")
	}
	return nil
}

type Reasoning struct {
	Text string `json:"text"`
}

func (Reasoning) EventType() EventType { return EventReasoning }
func (Reasoning) Validate() error      { return nil }

type TextDelta struct {
	Text string `json:"text"`
}

func (TextDelta) EventType() EventType { return EventTextDelta }
func (TextDelta) Validate() error      { return nil }

type ReasoningDelta struct {
	Text string `json:"text"`
}

func (ReasoningDelta) EventType() EventType { return EventReasoningDelta }
func (ReasoningDelta) Validate() error      { return nil }

type ToolCall struct {
	Name      string          `json:"name"`
	CallID    string          `json:"call_id"`
	Arguments json.RawMessage `json:"arguments"`
}

func (ToolCall) EventType() EventType { return EventToolCall }
func (c ToolCall) Validate() error {
	if c.Name == "" || c.CallID == "" {
		return errors.New("tool_call: name and call_id required")
	}
	return nil
}

type ToolCallInterrupted struct {
	CallID string `json:"call_id"`
}

func (ToolCallInterrupted) EventType() EventType { return EventToolCallInterrupted }
func (c ToolCallInterrupted) Validate() error {
	if c.CallID == "" {
		return errors.New("tool_call.interrupted: call_id required")
	}
	return nil
}

type ToolResult struct {
	CallID  string `json:"call_id"`
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
	// Structured is the tool's MCP structuredContent, passed through to
	// clients (e.g. UI components); the model sees Content.
	Structured json.RawMessage `json:"structured,omitempty"`
}

func (ToolResult) EventType() EventType { return EventToolResult }
func (r ToolResult) Validate() error {
	if r.CallID == "" {
		return errors.New("tool_result: call_id required")
	}
	return nil
}

type ApprovalRequested struct {
	CallID string `json:"call_id"`
}

func (ApprovalRequested) EventType() EventType { return EventApprovalRequested }
func (a ApprovalRequested) Validate() error {
	if a.CallID == "" {
		return errors.New("approval.requested: call_id required")
	}
	return nil
}

type ApprovalDecided struct {
	CallID  string `json:"call_id"`
	Approve bool   `json:"approve"`
	Result  string `json:"result,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

func (ApprovalDecided) EventType() EventType { return EventApprovalDecided }
func (a ApprovalDecided) Validate() error {
	if a.CallID == "" {
		return errors.New("approval.decided: call_id required")
	}
	return nil
}

type ModelUsage struct {
	Model  string `json:"model"`
	Input  int64  `json:"input"`
	Output int64  `json:"output"`
	Cached int64  `json:"cached"`
}

func (ModelUsage) EventType() EventType { return EventModelUsage }
func (ModelUsage) Validate() error      { return nil }

// Summary covers every turn up to and including CoversUntilTurn (a turn uuid).
type Summary struct {
	Text            string `json:"text"`
	CoversUntilTurn string `json:"covers_until_turn_id"`
}

func (Summary) EventType() EventType { return EventSummary }
func (s Summary) Validate() error {
	if s.Text == "" || s.CoversUntilTurn == "" {
		return errors.New("summary: text and covers_until_turn_id required")
	}
	return nil
}
