package svc

import (
	_ "embed"

	"encoding/json"

	"github.com/alash3al/hfy/internal/types"
)

// The canned notes a held call's one tool_result ends with. They live here so
// svc (ending a turn) and agent (serving a decided call) say the same thing.
// Files carry no trailing newline (embeds would keep it).

//go:embed prompts/declined.txt
var PromptDeclined string

//go:embed prompts/timeout.txt
var PromptTimeout string

//go:embed prompts/stopped.txt
var PromptStopped string

// AnswersText renders a decision's answers as the tool_result text, an object
// re-marshaled from what the stored event decoded to (Postgres re-orders jsonb
// object keys, so only decoding yields a stable order). Empty when the
// decision's answers are unusable.
func AnswersText(raw json.RawMessage) string {
	var answ map[string]any
	if json.Unmarshal(raw, &answ) != nil || answ == nil {
		return ""
	}
	b, err := json.Marshal(map[string]any{"answers": answ})
	if err != nil {
		return ""
	}
	return string(b)
}

// HeldResultText is the one tool_result a held call ever gets, wherever the
// turn ends: the held content joined with the answers (approved), the
// declined or timeout note (rejected), or the stopped note (the turn ended
// before a decision landed). It keeps every writer in the codebase from
// drifting apart.
func HeldResultText(req types.ApprovalRequested, d types.ApprovalDecided, decided bool) string {
	note := PromptDeclined
	switch {
	case !decided:
		note = PromptStopped
	case d.Reason == "timeout":
		note = PromptTimeout
	case d.Approve:
		note = AnswersText(d.Answers)
	}
	if note == "" { // approved with unusable answers
		note = PromptDeclined
	}
	content := ""
	if req.Held != nil {
		content = req.Held.Content
	}
	if content == "" {
		return note
	}
	return content + "\n\n" + note
}
