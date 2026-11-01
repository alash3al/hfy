package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
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

// maxReplayArgs bounds the stored arguments replayed into a prompt. A call that
// passed storable reads back at most maxArgBytes, so this is twice what any
// row written by this version needs.
const maxReplayArgs = 2 * maxArgBytes

// maxName bounds a tool name or call id stored in an event.
const maxName = 256

// maxDepth bounds how deep stored JSON nests. Postgres keeps any depth Go's
// decoder reads (10,000), but every event payload wraps the value: arguments
// sit one level down in a tool_call, structured and ui one in a tool_result,
// and a held result's structured content two in an approval request. The HTTP
// envelope around an event adds a few more. 64 plus those wrappers (about 70
// at most) stays far under the 10,000 that Go reads back and that json.Valid
// accepts; real arguments and structured results nest a handful of levels.
const maxDepth = 64

// Why storable refuses JSON. errTooLarge is also how arguments are refused
// when their numbers, written out, would outgrow maxArgBytes.
var (
	errNotJSON  = errors.New("they are not valid JSON")
	errNotText  = errors.New("they hold a NUL character, an unpaired surrogate escape or invalid UTF-8")
	errNumber   = errors.New("number out of range: at most " + strconv.Itoa(maxNumberDigits) + " digits once written out")
	errDepth    = errors.New("nested more than " + strconv.Itoa(maxDepth) + " levels")
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
// surrogate escapes in jsonb, parses every value of an object even when keys
// repeat (and keeps the last), and writes each number out in full on every
// read: 1e100000 is 10 bytes in and 100,003 bytes out. It also prints a space
// after every "," and ":" on read. So every token is looked at, one by one, so
// that a value under a repeated key is checked too; nesting is capped at
// maxDepth, with no recursion; and the size that counts is what is read back:
// raw, plus a byte per separator, plus what writing the numbers out adds. That
// must fit limit. This is deliberately conservative: it overcounts spaces raw
// already has and repeated keys Postgres drops, and refuses valid numbers that
// are too long written out (a 1001-digit integer, 0e1000, -0.0e-999).
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
	grown := separators(raw)
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	depth := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return errNotJSON
		}
		switch t := tok.(type) {
		case json.Delim:
			if t == '[' || t == '{' {
				if depth++; depth > maxDepth {
					return errDepth
				}
			} else {
				depth--
			}
		case string: // a key or a value
			if strings.ContainsRune(t, 0) {
				return errNotText
			}
		case json.Number:
			n, err := writtenLen(string(t))
			if err != nil {
				return err
			}
			grown += n - len(t)
		}
	}
	if len(raw)+grown > limit {
		return errTooLarge
	}
	return nil
}

// separators counts the "," and ":" outside strings in raw, which is valid
// JSON: Postgres adds a space after each when it reads the value back.
func separators(raw string) int {
	n, inString := 0, false
	for i := 0; i < len(raw); i++ {
		switch c := raw[i]; {
		case inString && c == '\\':
			i++
		case c == '"':
			inString = !inString
		case !inString && (c == ',' || c == ':'):
			n++
		}
	}
	return n
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
