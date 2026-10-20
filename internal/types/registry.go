package types

import (
	"encoding/json"
	"fmt"
)

// Upgrader rewrites a stored payload from one version of its shape to the next.
type Upgrader func(json.RawMessage) (json.RawMessage, error)

type spec struct {
	ephemeral bool
	upgrades  []Upgrader // upgrades[i] turns version i+1 into version i+2
}

var registry = map[EventType]spec{}

// register declares an event type. Its current version is 1 + len(upgrades).
//
// Versioning rules (events are kept forever and never rewritten):
//   - An additive change (a new optional field) keeps the version: old rows
//     decode with the field zero, and older binaries ignore it.
//   - A breaking change (rename, retype, new meaning) appends an Upgrader that
//     turns the previous version's JSON into the new shape. Rows are upgraded
//     in memory when read.
//   - Fields read by SQL are frozen, since SQL sees rows as stored:
//     turn.state "state", message.text "text", model.usage "input", "output"
//     and "cached".
func register[P Payload](ephemeral bool, upgrades ...Upgrader) {
	var zero P
	registry[zero.EventType()] = spec{ephemeral: ephemeral, upgrades: upgrades}
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
func Ephemeral(t EventType) bool { return registry[t].ephemeral }

// Version is the version events of type t are written with (0: unknown type).
func Version(t EventType) int {
	s, ok := registry[t]
	if !ok {
		return 0
	}
	return len(s.upgrades) + 1
}

// Upgrade brings a stored payload of version v to the current version and
// returns it with its new version. A payload this binary cannot know about (an
// unknown type or a newer version, written by a newer hfy during a rolling
// deploy) is returned unchanged.
func Upgrade(t EventType, v int, data json.RawMessage) (json.RawMessage, int, error) {
	cur := Version(t)
	if v < 1 {
		return nil, v, fmt.Errorf("%s: invalid version %d", t, v)
	}
	for ; v < cur; v++ {
		next, err := registry[t].upgrades[v-1](data)
		if err != nil {
			return nil, v, fmt.Errorf("upgrade %s from v%d: %w", t, v, err)
		}
		data = next
	}
	return data, v, nil
}
