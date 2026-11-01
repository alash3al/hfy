package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/alash3al/hfy/internal/provider"
	"github.com/alash3al/hfy/internal/types"
)

// argsMarker stands in for arguments over maxArgBytes, in the event log and in
// the prompt. The digest keeps identical oversize calls one repeat key and
// different ones apart.
func argsMarker(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	b, _ := json.Marshal(map[string]any{"truncated": true, "bytes": len(raw),
		"sha256": hex.EncodeToString(sum[:]), "preview": types.Clean(truncate(raw, 256))})
	return string(b)
}

// storableJSON reports whether raw is JSON the event log can hold: valid,
// UTF-8, and free of NUL, which Postgres refuses in jsonb even as \u0000.
func storableJSON(raw string) bool {
	if !json.Valid([]byte(raw)) || !utf8.ValidString(raw) {
		return false
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	return dec.Decode(&v) == nil && !jsonHasNUL(v)
}

func jsonHasNUL(v any) bool {
	switch v := v.(type) {
	case string:
		return strings.ContainsRune(v, 0)
	case []any:
		return slices.ContainsFunc(v, jsonHasNUL)
	case map[string]any:
		for k, x := range v {
			if strings.ContainsRune(k, 0) || jsonHasNUL(x) {
				return true
			}
		}
	}
	return false
}

// requestBytes is the text a request carries: system prompt, messages, tool
// calls, tool definitions and image URLs. JSON framing is not counted.
func requestBytes(req provider.Request) int {
	n := 0
	for _, m := range req.Messages {
		n += len(m.Content) + len(m.ToolCallID)
		for _, p := range m.Parts {
			n += len(p.Text)
			if p.ImageURL != nil {
				n += len(p.ImageURL.URL)
			}
		}
		for _, tc := range m.ToolCalls {
			n += len(tc.ID) + len(tc.Name) + len(tc.Arguments)
		}
	}
	for _, t := range req.Tools {
		n += len(t.Name) + len(t.Description) + len(t.Parameters)
	}
	return n
}
