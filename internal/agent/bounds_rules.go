package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/alash3al/hfy/internal/provider"
	"github.com/alash3al/hfy/internal/types"
)

// maxNumberDigits bounds a number once written out in plain decimal. A float64
// needs 309 digits, and Postgres allows 131072 before the point and 16383 after.
const maxNumberDigits = 1000

// maxName bounds a tool name or call id stored in an event.
const maxName = 256

// Why storable refuses JSON. errTooLarge is also how arguments are refused
// when their numbers, written out, would outgrow maxArgBytes.
var (
	errNotJSON  = errors.New("they are not valid JSON")
	errNotText  = errors.New("they hold a NUL character, an unpaired surrogate escape or invalid UTF-8")
	errNumber   = errors.New("number out of range: at most " + strconv.Itoa(maxNumberDigits) + " digits once written out")
	errTooLarge = errors.New("too large")
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

// storable reports why raw cannot be stored in an event, or nil if it can. An
// event that cannot be stored wedges the turn. Postgres refuses NUL and unpaired
// surrogate escapes in jsonb, and it writes each number out in full on every
// read: 1e100000 is 10 bytes in and 100,003 bytes out. So a number counts at its
// written-out length, each is capped at maxNumberDigits, and raw plus what
// writing the numbers out adds must fit limit.
func storable(raw string, limit int) error {
	if len(raw) > limit {
		return errTooLarge
	}
	if !json.Valid([]byte(raw)) {
		return errNotJSON
	}
	if !utf8.ValidString(raw) || hasLoneSurrogate(raw) {
		return errNotText
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return errNotJSON
	}
	grown := 0
	if err := walkJSON(v, &grown); err != nil {
		return err
	}
	if len(raw)+grown > limit {
		return errTooLarge
	}
	return nil
}

// walkJSON refuses NUL in any string or key, and every number too long once
// written out. grown adds up what writing the numbers out adds to raw.
func walkJSON(v any, grown *int) error {
	switch v := v.(type) {
	case string:
		if strings.ContainsRune(v, 0) {
			return errNotText
		}
	case json.Number:
		n, err := writtenLen(string(v))
		if err != nil {
			return err
		}
		*grown += n - len(v)
	case []any:
		for _, x := range v {
			if err := walkJSON(x, grown); err != nil {
				return err
			}
		}
	case map[string]any:
		for k, x := range v {
			if strings.ContainsRune(k, 0) {
				return errNotText
			}
			if err := walkJSON(x, grown); err != nil {
				return err
			}
		}
	}
	return nil
}

// writtenLen is the length of the JSON number s written out in plain decimal,
// the exponent expanded as Postgres writes it, or errNumber when that is over
// maxNumberDigits. The digit counts are upper bounds, never short.
func writtenLen(s string) (int, error) {
	mant, exp := s, 0
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		e, err := strconv.Atoi(s[i+1:])
		if err != nil || e > maxNumberDigits || e < -maxNumberDigits {
			return 0, errNumber
		}
		mant, exp = s[:i], e
	}
	sign := 0
	if strings.HasPrefix(mant, "-") {
		sign, mant = 1, mant[1:]
	}
	whole, frac, _ := strings.Cut(mant, ".")
	digits := max(len(whole)+exp, 1)
	after := max(len(frac)-exp, 0)
	if digits+after > maxNumberDigits {
		return 0, errNumber
	}
	if after > 0 {
		after++ // the point
	}
	return sign + digits + after, nil
}

// storableText reports whether s can be stored as a tool name or call id
// unchanged: short enough, with no NUL and no invalid UTF-8.
func storableText(s string) bool {
	return len(s) <= maxName && types.Clean(s) == s
}

// clipName is a tool name as the event log stores it: cleaned and cut to
// maxName bytes. An empty name is stored as "(unnamed)", since an event needs one.
func clipName(s string) string {
	s = truncate(types.Clean(s), maxName)
	if s == "" {
		return "(unnamed)"
	}
	return s
}

// keepStored returns raw when it can be stored whole, or nil: the tool's output
// is dropped with a warning that names the tool, never its content.
func keepStored(raw json.RawMessage, limit int, what, tool string) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	if err := storable(string(raw), limit); err != nil {
		slog.Warn("tool's "+what+" dropped", "tool", tool, "reason", err.Error())
		return nil
	}
	return raw
}

// hasLoneSurrogate reports a \u escape of a surrogate without its pair. The
// decoder would replace it silently, so the raw text is scanned. raw is valid
// JSON, so every \u is followed by four hex digits.
func hasLoneSurrogate(raw string) bool {
	hex4 := func(at int) int {
		n, _ := strconv.ParseUint(raw[at:at+4], 16, 32)
		return int(n)
	}
	for i := 0; i+1 < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		if raw[i+1] != 'u' {
			i++ // an escaped character, a backslash included, starts no escape
			continue
		}
		switch r := hex4(i + 2); {
		case r >= 0xDC00 && r <= 0xDFFF:
			return true
		case r >= 0xD800 && r <= 0xDBFF:
			if i+12 > len(raw) || raw[i+6] != '\\' || raw[i+7] != 'u' {
				return true
			}
			if lo := hex4(i + 8); lo < 0xDC00 || lo > 0xDFFF {
				return true
			}
			i += 11
		default:
			i += 5
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
