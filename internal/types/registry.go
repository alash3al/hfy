package types

// ephemeral lists every event type and whether it expires per retention.trace.
var ephemeral = map[EventType]bool{}

func register[P Payload](expires bool) {
	var zero P
	ephemeral[zero.EventType()] = expires
}

func init() {
	register[Turn](false)
	register[TurnStateEvent](false)
	register[TurnCancelRequested](false)
	register[MessageText](false)
	register[Reasoning](false)
	register[ToolCall](false)
	register[ToolCallInterrupted](false)
	register[ToolResult](false)
	register[ApprovalRequested](false)
	register[ApprovalDecided](false)
	register[ModelUsage](false)
	register[Summary](false)
	register[TextDelta](true)
	register[ReasoningDelta](true)
}

// Ephemeral reports whether events of type t expire per retention.trace.
func Ephemeral(t EventType) bool { return ephemeral[t] }
