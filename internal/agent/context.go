package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
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
func (rt *Runtime) BuildContext(ctx context.Context, conversationID, currentTurn int64, spec *Spec) (*BuiltContext, error) {
	summary, after, err := rt.summaryStart(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	events, err := rt.Store.LoadContext(ctx, conversationID, after, currentTurn)
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
	room := inRoom(events, spec.Name)
	bc.System = svc.Render(spec.System, bc.Vars)
	if room {
		bc.System += "\n\n" + strings.TrimSpace(promptRoom)
	}
	if summary != nil {
		text := summary.Text
		if room { // tag-like text in it must not read as a message
			text = escText.Replace(text)
		}
		bc.System += "\n\nSummary of the earlier conversation:\n" + text
	}
	if bc.Msgs, err = rt.messagesFrom(ctx, events, currentTurn, spec, room); err != nil {
		return nil, err
	}
	return bc, nil
}

// summaryStart returns the conversation's latest summary (nil if none) and the id
// of the last turn it covers (0 if none): context starts after that turn.
func (rt *Runtime) summaryStart(ctx context.Context, conversationID int64) (*types.Summary, int64, error) {
	summary, err := rt.Store.Summary(ctx, conversationID)
	if err != nil || summary == nil {
		return nil, 0, err
	}
	u, err := uuid.Parse(summary.CoversUntilTurn)
	if err != nil {
		return nil, 0, fmt.Errorf("summary of conversation %d: bad covers_until_turn_id: %w", conversationID, err)
	}
	after, err := rt.Store.TurnIDByUUID(ctx, conversationID, u)
	if err != nil {
		return nil, 0, fmt.Errorf("summary of conversation %d: covered turn %s: %w", conversationID, u, err)
	}
	return summary, after, nil
}

// messagesFrom turns events into the messages spec's agent sees. Only
// message.text, tool_call, tool_call.interrupted and tool_result reach the
// model. The agent's own words and calls are its assistant turns; everyone
// else's words are user turns, framed with who said them when room is set
// (see inRoom). Other agents' tool calls are theirs and stay out.
func (rt *Runtime) messagesFrom(ctx context.Context, events []db.EventRow, currentTurn int64, spec *Spec, room bool) ([]provider.Message, error) {
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
			msg := rt.userMessage(ctx, m, e.TurnID() == currentTurn, spec)
			if room {
				msg = framed(msg, e)
			}
			out = appendUser(out, msg)
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

// inRoom reports whether anyone besides self and one person speaks in
// events. Then every message that is not self's own is framed with its author
// (see frame) and the system prompt explains the frames; a one-to-one chat
// stays plain, with no frames to imitate.
func inRoom(events []db.EventRow, self string) bool {
	person := ""
	for _, e := range events {
		switch {
		case e.Type != types.EventMessageText:
		case e.ActorType == string(types.ActorAgent):
			if e.ActorRef != self {
				return true
			}
		case person == "":
			person = e.ActorType + "/" + e.ActorRef
		case person != e.ActorType+"/"+e.ActorRef:
			return true
		}
	}
	return false
}

var (
	escText = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	escAttr = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
)

// frame wraps text in tag, attributed to e's actor:
// <message from="u1" kind="person">…</message> (an anonymous person has no
// from). The text and the ref are escaped, so nothing a speaker writes can
// close the frame or open another: only frames say who wrote what.
func frame(e db.EventRow, tag, text string) string {
	kind := "person"
	if e.ActorType == string(types.ActorAgent) {
		kind = "agent"
	}
	attrs := ` kind="` + kind + `"`
	if e.ActorType != string(types.ActorAnonymous) {
		attrs = ` from="` + escAttr.Replace(e.ActorRef) + `"` + attrs
	}
	return "<" + tag + attrs + ">" + escText.Replace(text) + "</" + tag + ">"
}

// framed frames a user message's text (its first part when it has parts;
// images follow it).
func framed(m provider.Message, e db.EventRow) provider.Message {
	if m.Parts == nil {
		m.Content = frame(e, "message", m.Content)
		return m
	}
	m.Parts = slices.Clone(m.Parts)
	m.Parts[0].Text = frame(e, "message", m.Parts[0].Text)
	return m
}

// appendUser merges a user message into a preceding one: some chat templates
// (e.g. Mistral's) reject two user turns in a row, and Anthropic's API merges
// them the same way. Frames keep the merged authors apart.
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
func (rt *Runtime) userMessage(ctx context.Context, m types.MessageText, current bool, spec *Spec) provider.Message {
	text := m.Text
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
