package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"uuid"

	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/mcp"
	"github.com/alash3al/hfy/internal/provider"
	"github.com/alash3al/hfy/internal/svc"
	"github.com/alash3al/hfy/internal/types"
)

// BuiltContext is everything a model call needs from the event log.
type BuiltContext struct {
	System string
	Msgs   []provider.Message
	Vars   map[string]string
	// Turns lists the turn ids in the context (after the summary), oldest first.
	Turns []int64
}

// BuildContext = the latest summary + the durable events of the turns after
// the ones it covers, up to and including the current turn. One query.
func (rt *Runtime) BuildContext(ctx context.Context, sessionID, currentTurn int64, spec *Spec) (*BuiltContext, error) {
	summary, after, err := rt.summaryStart(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	events, err := rt.Store.LoadContext(ctx, sessionID, after, currentTurn)
	if err != nil {
		return nil, err
	}
	bc := &BuiltContext{Vars: map[string]string{}}
	for _, e := range events {
		if e.Type != types.EventTurn {
			continue
		}
		bc.Turns = append(bc.Turns, e.ID)
		if e.ID == currentTurn {
			t, err := e.Payload[types.Turn]()
			if err != nil {
				return nil, err
			}
			if t.Vars != nil {
				bc.Vars = t.Vars
			}
		}
	}
	bc.System = svc.Render(spec.System, bc.Vars)
	if summary != nil {
		bc.System += "\n\nSummary of the earlier conversation:\n" + summary.Text
	}
	if bc.Msgs, err = rt.messagesFrom(ctx, events, currentTurn, spec); err != nil {
		return nil, err
	}
	return bc, nil
}

// summaryStart returns the session's latest summary (nil if none) and the id
// of the last turn it covers (0 if none): context starts after that turn.
func (rt *Runtime) summaryStart(ctx context.Context, sessionID int64) (*types.Summary, int64, error) {
	summary, err := rt.Store.Summary(ctx, sessionID)
	if err != nil || summary == nil {
		return nil, 0, err
	}
	u, err := uuid.Parse(summary.CoversUntilTurn)
	if err != nil {
		return nil, 0, fmt.Errorf("summary of session %d: bad covers_until_turn_id: %w", sessionID, err)
	}
	after, err := rt.Store.TurnIDByUUID(ctx, sessionID, u)
	if err != nil {
		return nil, 0, fmt.Errorf("summary of session %d: covered turn %s: %w", sessionID, u, err)
	}
	return summary, after, nil
}

// messagesFrom turns events into the messages spec's agent sees. Only
// message.text, tool_call, tool_call.interrupted and tool_result reach the
// model. A session is a room: the agent's own words and calls are its
// assistant turns; everyone else's words are user turns labelled with who
// said them (labels exist only in the prompt). Other agents' tool calls are
// theirs and stay out.
func (rt *Runtime) messagesFrom(ctx context.Context, events []db.EventRow, currentTurn int64, spec *Spec) ([]provider.Message, error) {
	var out []provider.Message
	var asst *provider.Message
	pending := map[string]bool{}
	flush := func() {
		if asst != nil {
			out = append(out, *asst)
			asst = nil
		}
	}
	interrupted := strings.TrimSpace(promptInterrupted)
	many := manyPeople(events)
	for _, e := range events {
		own := e.ActorType == string(types.ActorAgent) && e.ActorRef == spec.Name
		switch e.Type {
		case types.EventMessageText:
			m, err := e.Payload[types.MessageText]()
			if err != nil {
				return nil, err
			}
			flush()
			if own {
				asst = &provider.Message{Role: "assistant", Content: m.Text}
				continue
			}
			out = appendUser(out, rt.userMessage(ctx, m, e.TurnID() == currentTurn, spec, label(e, many)))
		case types.EventToolCall:
			if !own {
				continue // its result is skipped too: it is not pending
			}
			c, err := e.Payload[types.ToolCall]()
			if err != nil {
				return nil, err
			}
			if asst == nil {
				asst = &provider.Message{Role: "assistant"}
			}
			asst.ToolCalls = append(asst.ToolCalls, provider.ToolCall{
				ID: c.CallID, Name: mcp.WireNameOf(c.Name), Arguments: argsText(c.Arguments)})
			pending[c.CallID] = true
		case types.EventToolResult:
			r, err := e.Payload[types.ToolResult]()
			if err != nil {
				return nil, err
			}
			if !pending[r.CallID] {
				continue
			}
			flush()
			delete(pending, r.CallID)
			out = append(out, provider.Message{Role: "tool", ToolCallID: r.CallID,
				Content: toolText(mcp.Result{Content: r.Content, IsError: r.IsError})})
		case types.EventToolCallInterrupted:
			c, err := e.Payload[types.ToolCallInterrupted]()
			if err != nil {
				return nil, err
			}
			if !pending[c.CallID] {
				continue
			}
			flush()
			delete(pending, c.CallID)
			out = append(out, provider.Message{Role: "tool", ToolCallID: c.CallID, Content: interrupted})
		}
	}
	flush()
	// A call with no result at all (stopped mid-flight) must not break the API contract.
	for id := range pending {
		out = append(out, provider.Message{Role: "tool", ToolCallID: id, Content: interrupted})
	}
	return out, nil
}

// label is how the model is told who said a message that is not its own:
// "[agent:<ref>] " for an agent, "[<ref>] " for a person when more than one
// person spoke (with one there is nobody to tell apart).
func label(e db.EventRow, manyPeople bool) string {
	switch {
	case e.ActorType == string(types.ActorAgent):
		return "[agent:" + e.ActorRef + "] "
	case manyPeople:
		return "[" + e.ActorRef + "] "
	}
	return ""
}

// manyPeople reports whether messages from more than one person (any
// non-agent actor) are among events.
func manyPeople(events []db.EventRow) bool {
	first := ""
	for _, e := range events {
		if e.Type != types.EventMessageText || e.ActorType == string(types.ActorAgent) {
			continue
		}
		if who := e.ActorType + "/" + e.ActorRef; first == "" {
			first = who
		} else if who != first {
			return true
		}
	}
	return false
}

// appendUser merges a user message into a preceding one: some chat templates
// (e.g. Mistral's) reject two user turns in a row, and Anthropic's API merges
// them the same way.
func appendUser(out []provider.Message, m provider.Message) []provider.Message {
	n := len(out)
	if n == 0 || out[n-1].Role != "user" {
		return append(out, m)
	}
	prev := &out[n-1]
	if prev.Parts == nil && m.Parts == nil {
		prev.Content += "\n\n" + m.Content
		return out
	}
	if prev.Parts == nil {
		prev.Parts, prev.Content = []provider.Part{{Type: "text", Text: prev.Content}}, ""
	}
	if m.Parts == nil {
		m.Parts = []provider.Part{{Type: "text", Text: m.Content}}
	}
	prev.Parts = append(prev.Parts, m.Parts...)
	return out
}

// userMessage expands image attachments only in the turn they arrive in; later
// turns get a short placeholder (small context, and presigned URLs expire).
// label (see label) goes before the text.
func (rt *Runtime) userMessage(ctx context.Context, m types.MessageText, current bool, spec *Spec, label string) provider.Message {
	text := label + m.Text
	if len(m.Attachments) == 0 {
		return provider.Message{Role: "user", Content: text}
	}
	var images []provider.Part
	for _, a := range m.Attachments {
		isImage := strings.HasPrefix(a.Mime, "image/")
		switch {
		case !isImage:
			text += "\n[attachment: " + a.URL + " (" + a.Mime + ")]"
		case !current || !spec.Settings.Vision:
			text += "\n[image: " + a.Mime + "]"
		default:
			ref := a.URL
			if inlineImages(spec.Client) {
				uri, err := rt.http.dataURI(ctx, a.URL, a.Mime)
				if err != nil {
					text += "\n[image unavailable: " + a.Mime + "]"
					continue
				}
				ref = uri
			}
			images = append(images, provider.Part{Type: "image_url", ImageURL: &provider.ImageURL{URL: ref}})
		}
	}
	if len(images) == 0 {
		return provider.Message{Role: "user", Content: text}
	}
	parts := append([]provider.Part{{Type: "text", Text: text}}, images...)
	return provider.Message{Role: "user", Parts: parts}
}

// argsText converts stored arguments back to the text a provider expects.
func argsText(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if strings.HasPrefix(s, `"`) { // invalid JSON the model sent, stored as a string
		var text string
		if json.Unmarshal(raw, &text) == nil {
			return text
		}
	}
	if s == "" {
		return "{}"
	}
	return s
}
