package svc

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/alash3al/hfy/internal/types"
)

// AskUserTool is the full name of the built-in ask tool (hfy.ask_user; the
// tool itself is declared in mcp/builtins), the only call that carries
// answers on an approval decision.
const AskUserTool = "hfy.ask_user"

// Option is one choice of a single/multi question: value is what the model
// receives, label is display text (defaults to the value for the UI).
type Option struct {
	Value string
	Label string
}

// Question is one validated hfy.ask_user question.
type Question struct {
	ID, Text, Type string
	Options        []Option
	Required       bool
	Placeholder    string
}

// Values returns the option values of a single/multi question.
func (q Question) Values() []string {
	out := make([]string, len(q.Options))
	for i, o := range q.Options {
		out[i] = o.Value
	}
	return out
}

const (
	maxQuestions = 10
	maxOptions   = 20
	maxQText     = 300 // a question's text, in characters
	maxAnswers   = 32 << 10
	maxAText     = 2000 // a text answer, in characters
)

// ParseQuestions validates a hfy.ask_user call's arguments ({"questions":
// [...]}) and returns its questions. It forgives the slips small models make
// instead of refusing them (refusing leaves the viewer with plain text):
// options given as plain strings, missing or ill-formed ids, type synonyms,
// a choice with fewer than two options (asked as text), options on a text
// question (asked as a choice). What it cannot repair is an error the agent
// sees before anything pauses. Normalizing is idempotent, so the loop stores
// MarshalQuestions(qs) and every later reader parses the same questions.
func ParseQuestions(args json.RawMessage) ([]Question, error) {
	var in struct {
		Questions []struct {
			ID          string            `json:"id"`
			Text        string            `json:"text"`
			Type        string            `json:"type"`
			Options     []json.RawMessage `json:"options"`
			Required    *bool             `json:"required"`
			Placeholder string            `json:"placeholder"`
		} `json:"questions"`
	}
	if json.Unmarshal(args, &in) != nil {
		return nil, errors.New(`arguments must be {"questions": [...]}; ` + askExample)
	}
	if len(in.Questions) == 0 || len(in.Questions) > maxQuestions {
		return nil, fmt.Errorf("questions must have 1 to %d items; %s", maxQuestions, askExample)
	}
	seen := map[string]bool{}
	qs := make([]Question, len(in.Questions))
	for i, raw := range in.Questions {
		q := Question{Text: strings.TrimSpace(raw.Text), Placeholder: raw.Placeholder, Required: raw.Required == nil || *raw.Required}
		if n := utf8.RuneCountInString(q.Text); n == 0 || n > maxQText {
			return nil, fmt.Errorf("questions[%d].text: 1 to %d characters; %s", i, maxQText, askExample)
		}
		q.ID = askID(raw.ID, i)
		for seen[q.ID] {
			q.ID = askID(fmt.Sprintf("%s_%d", q.ID, i+1), i)
		}
		seen[q.ID] = true
		vals := map[string]bool{}
		for _, o := range raw.Options {
			opt, ok := askOption(o)
			if !ok || vals[opt.Value] {
				continue // empty or repeated options are dropped, not fatal
			}
			vals[opt.Value] = true
			q.Options = append(q.Options, opt)
		}
		if len(q.Options) > maxOptions {
			return nil, fmt.Errorf("questions[%d].options: at most %d options", i, maxOptions)
		}
		q.Type = askType(raw.Type, len(q.Options))
		if (q.Type == "single" || q.Type == "multi") && len(q.Options) < 2 {
			q.Type = "text" // nothing to choose from: ask it as text
		}
		if q.Type != "single" && q.Type != "multi" {
			q.Options = nil
		}
		if q.Type != "text" {
			q.Placeholder = ""
		}
		qs[i] = q
	}
	return qs, nil
}

const askExample = `example: {"questions":[{"id":"size","text":"Which size?","type":"single","options":[{"value":"m","label":"Medium"},{"value":"l","label":"Large"}]},{"id":"note","text":"Any note?","type":"text","required":false}]}`

var askIDJunk = regexp.MustCompile(`[^a-z0-9_]+`)

// askID turns whatever id the model sent into [a-z0-9_]{1,40}, or q<n>.
func askID(raw string, i int) string {
	id := strings.Trim(askIDJunk.ReplaceAllString(strings.ToLower(strings.TrimSpace(raw)), "_"), "_")
	if len(id) > 40 {
		id = id[:40]
	}
	if id == "" {
		id = fmt.Sprintf("q%d", i+1)
	}
	return id
}

// askOption reads an option given as {"value","label"} or as a plain string.
func askOption(raw json.RawMessage) (Option, bool) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		s = strings.TrimSpace(s)
		return Option{Value: s, Label: s}, s != ""
	}
	var o struct {
		Value any    `json:"value"`
		Label string `json:"label"`
	}
	if json.Unmarshal(raw, &o) != nil {
		return Option{}, false
	}
	v := strings.TrimSpace(fmt.Sprint(o.Value))
	if o.Value == nil {
		v = strings.TrimSpace(o.Label)
	}
	label := strings.TrimSpace(o.Label)
	if label == "" {
		label = v
	}
	return Option{Value: v, Label: label}, v != ""
}

// askType maps the model's type, synonyms included, onto the four types;
// an unknown or missing type is a choice when there are options, else text.
func askType(raw string, options int) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "single", "choice", "radio", "select", "single_choice", "dropdown":
		return "single"
	case "multi", "multiple", "checkbox", "checkboxes", "multi_choice", "multiple_choice":
		return "multi"
	case "confirm", "boolean", "bool", "yes_no", "yesno":
		return "confirm"
	case "text", "string", "input", "textarea", "free_text":
		if options >= 2 {
			return "single"
		}
		return "text"
	}
	if options >= 2 {
		return "single"
	}
	return "text"
}

// MarshalQuestions is the canonical {"questions": [...]} the loop stores, so
// the UI renders and Approve validates exactly what ParseQuestions returned.
func MarshalQuestions(qs []Question) json.RawMessage {
	type opt struct {
		Value string `json:"value"`
		Label string `json:"label"`
	}
	type question struct {
		ID          string `json:"id"`
		Text        string `json:"text"`
		Type        string `json:"type"`
		Options     []opt  `json:"options,omitempty"`
		Required    bool   `json:"required"`
		Placeholder string `json:"placeholder,omitempty"`
	}
	out := struct {
		Questions []question `json:"questions"`
	}{}
	for _, q := range qs {
		qq := question{ID: q.ID, Text: q.Text, Type: q.Type, Required: q.Required, Placeholder: q.Placeholder}
		for _, o := range q.Options {
			qq.Options = append(qq.Options, opt(o))
		}
		out.Questions = append(out.Questions, qq)
	}
	b, _ := json.Marshal(out) // plain strings and bools: cannot fail
	return b
}

// ParseAnswers validates a decision's answers against qs and returns the
// canonical object for the approval.decided payload: every key is a known
// question id, single is one option value, multi a subset, text a cleaned
// string and confirm a boolean; required questions are answered non-empty.
func ParseAnswers(qs []Question, raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) > maxAnswers {
		return nil, badf("answers exceed %d bytes", maxAnswers)
	}
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil || in == nil {
		return nil, badf("answers must be a JSON object keyed by question id")
	}
	out := map[string]any{}
	for _, q := range qs {
		v, ok := in[q.ID]
		if !ok {
			if q.Required {
				return nil, badf("answers.%s is required", q.ID)
			}
			continue
		}
		a, err := parseAnswer(v, q)
		if err != nil {
			return nil, err
		}
		if a != nil {
			out[q.ID] = a
		}
	}
	for id := range in {
		if !slices.ContainsFunc(qs, func(q Question) bool { return q.ID == id }) {
			return nil, badf("answers.%s: unknown question", id)
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// parseAnswer validates one answer (a string, an array of them, or a boolean)
// for a single file question and returns what is stored (nil for an unset
// optional). Its mistakes belong to the client: 400s naming the question.
func parseAnswer(v any, q Question) (any, error) {
	switch q.Type {
	case "single":
		s, ok := v.(string)
		if !ok || !slices.Contains(q.Values(), s) {
			return nil, badf("answers.%s: pick one of the options", q.ID)
		}
		return s, nil
	case "multi":
		raw, ok := v.([]any)
		if !ok {
			return nil, badf("answers.%s: must be an array of option values", q.ID)
		}
		vals := q.Values()
		picked := make([]string, 0, len(raw))
		for _, e := range raw {
			s, ok := e.(string)
			if !ok || !slices.Contains(vals, s) {
				return nil, badf("answers.%s: %v is not one of the options", q.ID, e)
			}
			picked = append(picked, s)
		}
		if q.Required && len(picked) == 0 {
			return nil, badf("answers.%s: pick at least one option", q.ID)
		}
		if len(picked) == 0 {
			return nil, nil
		}
		return picked, nil
	case "text":
		s, ok := v.(string)
		if !ok {
			return nil, badf("answers.%s: must be a string", q.ID)
		}
		s = types.Clean(s)
		if utf8.RuneCountInString(s) > maxAText {
			return nil, badf("answers.%s: at most %d characters", q.ID, maxAText)
		}
		if s == "" {
			if q.Required {
				return nil, badf("answers.%s is required", q.ID)
			}
			return nil, nil
		}
		return s, nil
	default: // confirm
		b, ok := v.(bool)
		if !ok {
			return nil, badf("answers.%s: must be true or false", q.ID)
		}
		return b, nil
	}
}
