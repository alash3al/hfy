package svc

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
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

var askTypes = []string{"single", "multi", "text", "confirm"}

var questionID = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)

// ParseQuestions validates a stored hfy.ask_user tool call's arguments
// ({"questions": [...]}) and returns its questions. The same rules the agent
// applies before pausing: an invalid call is refused instead of pausing.
func ParseQuestions(args json.RawMessage) ([]Question, error) {
	var in struct {
		Questions []struct {
			ID      string `json:"id"`
			Text    string `json:"text"`
			Type    string `json:"type"`
			Options []struct {
				Value string `json:"value"`
				Label string `json:"label"`
			} `json:"options"`
			Required    *bool  `json:"required"`
			Placeholder string `json:"placeholder"`
		} `json:"questions"`
	}
	if json.Unmarshal(args, &in) != nil {
		return nil, fmt.Errorf("arguments must be {\"questions\": [...]}")
	}
	if len(in.Questions) == 0 || len(in.Questions) > maxQuestions {
		return nil, fmt.Errorf("questions must have 1 to %d items", maxQuestions)
	}
	seen := map[string]bool{}
	qs := make([]Question, len(in.Questions))
	for i, raw := range in.Questions {
		q := Question{ID: raw.ID, Text: raw.Text, Type: raw.Type, Placeholder: raw.Placeholder, Required: raw.Required == nil || *raw.Required}
		if !questionID.MatchString(q.ID) || seen[q.ID] {
			return nil, fmt.Errorf("questions[%d].id: must be 1-40 chars of [a-z0-9_], unique in the call (%q)", i, q.ID)
		}
		seen[q.ID] = true
		if utf8.RuneCountInString(raw.Text) == 0 || utf8.RuneCountInString(raw.Text) > maxQText {
			return nil, fmt.Errorf("questions[%d].text: 1 to %d characters", i, maxQText)
		}
		if !slices.Contains(askTypes, q.Type) {
			return nil, fmt.Errorf("questions[%d].type %q: must be one of single, multi, text, confirm", i, raw.Type)
		}
		choice := q.Type == "single" || q.Type == "multi"
		if len(raw.Options) > 0 && !choice {
			return nil, fmt.Errorf("questions[%d].options: allowed only for single and multi", i)
		}
		if choice {
			if len(raw.Options) < 2 || len(raw.Options) > maxOptions {
				return nil, fmt.Errorf("questions[%d].options: 2 to %d options (got %d)", i, maxOptions, len(raw.Options))
			}
			vals := map[string]bool{}
			for _, o := range raw.Options {
				if o.Value == "" {
					return nil, fmt.Errorf("questions[%d].options: every option needs a value", i)
				}
				if vals[o.Value] {
					return nil, fmt.Errorf("questions[%d].options: value %q repeats", i, o.Value)
				}
				vals[o.Value] = true
				q.Options = append(q.Options, Option{Value: o.Value, Label: o.Label})
			}
		}
		if q.Placeholder != "" && q.Type != "text" {
			return nil, fmt.Errorf("questions[%d].placeholder: allowed only for text questions", i)
		}
		qs[i] = q
	}
	return qs, nil
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
