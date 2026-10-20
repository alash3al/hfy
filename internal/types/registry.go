package types

import "reflect"

type ctor struct {
	new       func() Payload // returns a pointer to a zero payload
	ephemeral bool
}

var registry = map[EventType]ctor{}

func register[P Payload](ephemeral bool) {
	var zero P
	registry[zero.EventType()] = ctor{
		new:       func() Payload { return any(new(P)).(Payload) },
		ephemeral: ephemeral,
	}
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

// Known reports whether t is a registered event type.
func Known(t EventType) bool { _, ok := registry[t]; return ok }

// Ephemeral reports whether events of type t expire per retention.trace.
func Ephemeral(t EventType) bool { return registry[t].ephemeral }

// deref turns the *P from the registry into a P (value receivers).
func deref(p Payload) Payload {
	return reflect.ValueOf(p).Elem().Interface().(Payload)
}
