package agent

import (
	"bytes"
	"encoding/json"
)

// verdict is what the repeat guard does with a call.
type verdict int

const (
	verdictOK    verdict = iota // the call runs
	verdictWarn                 // the call is refused with advice
	verdictAbort                // the call is refused and the turn ends
)

// repeatVerdict decides a call from how many identical calls the turn has
// made so far, this one included.
func repeatVerdict(n int) verdict {
	switch {
	case n >= repeatAbort:
		return verdictAbort
	case n >= repeatWarn:
		return verdictWarn
	}
	return verdictOK
}

// callKey identifies a call for the repeat guard: its tool name and its
// arguments with object keys sorted. The store rewrites JSON (jsonb reorders
// keys and whitespace), so the key must not depend on the bytes as sent.
// shortcut: numbers compare as written, but Postgres stores 1e2 as 100, so a
// resumed turn may see an exponent-form repeat under a new key.
func callKey(name string, args json.RawMessage) string {
	var v any
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.UseNumber()
	if dec.Decode(&v) == nil {
		if b, err := json.Marshal(v); err == nil {
			return name + "\x00" + string(b)
		}
	}
	return name + "\x00" + string(args)
}
