package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"uuid"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/mcp"
	"github.com/alash3al/hfy/internal/provider"
	"github.com/alash3al/hfy/internal/types"
)

// buildContext loads what a prompt is made of (the latest summary and the
// durable events of the turns after the ones it covers: one query) and hands
// it to buildPrompt.
func (rt *Runtime) buildContext(ctx context.Context, conversationID, currentTurn int64, spec *Spec) (system string, msgs []provider.Message, err error) {
	summary, after, err := rt.summaryStart(ctx, conversationID)
	if err != nil {
		return "", nil, err
	}
	events, err := rt.Store.LoadContext(ctx, conversationID, after, currentTurn)
	if err != nil {
		return "", nil, err
	}
	return buildPrompt(promptInput{Events: events, Summary: summary, Current: currentTurn, Spec: spec,
		Sign: func(key string) string { return rt.downloadURL(ctx, key) }})
}

// promptInput is everything a prompt is made of.
type promptInput struct {
	Events  []db.EventRow // loaded by LoadContext: grouped by turn, in event order
	Summary *types.Summary
	Current int64 // the running turn's id
	Spec    *Spec
	// Sign returns a provider-reachable URL for a stored file ("" for none).
	// It is only asked for an image that is actually sent.
	Sign func(key string) string
}

// buildPrompt is the one place a prompt is assembled: the system text
// (template, room rules, summary) and the messages. It is pure: the same
// input gives the same bytes, so provider caches hold and context:show shows
// what a turn would send.
func buildPrompt(in promptInput) (system string, msgs []provider.Message, err error) {
	var vars map[string]string
	for _, e := range in.Events {
		if e.Type == types.EventTurn && e.ID == in.Current {
			t, err := e.Payload[types.Turn]()
			if err != nil {
				return "", nil, err
			}
			vars = t.Vars
		}
	}
	room := inRoom(in.Events, in.Spec.Name)
	system = config.Render(in.Spec.System, vars)
	if room {
		system += "\n\n" + strings.TrimSpace(promptRoom)
	}
	if in.Summary != nil {
		text := in.Summary.Text
		if room { // tag-like text in it must not read as a message
			text = escText.Replace(text)
		}
		system += "\n\nSummary of the earlier conversation:\n" + text
	}
	msgs, err = messagesFrom(in.Events, in.Current, in.Spec, room, in.Sign)
	return system, msgs, err
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
func messagesFrom(events []db.EventRow, currentTurn int64, spec *Spec, room bool, sign func(string) string) ([]provider.Message, error) {
	var out []provider.Message
	var asst *provider.Message
	pending := map[string]bool{}
	var calls []string     // every call id in event order, for a deterministic prompt
	at := map[string]int{} // call id -> index in out of the assistant message of its last use
	flush := func() {
		if asst != nil {
			for _, tc := range asst.ToolCalls {
				at[tc.ID] = len(out)
			}
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
			msg := userMessage(m, e.TurnID() == currentTurn, spec, sign)
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
			calls = append(calls, c.CallID)
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
	// A call with no result at all (stopped mid-flight) must not break the API
	// contract: it is answered right after the tool answers that follow its
	// assistant message, at its last use if the id was reused.
	late := map[int][]provider.Message{}
	for i, id := range calls {
		if pending[id] && !slices.Contains(calls[i+1:], id) {
			late[at[id]] = append(late[at[id]], provider.Message{Role: "tool", ToolCallID: id, Content: interrupted})
		}
	}
	if len(late) == 0 {
		return out, nil
	}
	merged := make([]provider.Message, 0, len(out)+len(calls))
	for i := 0; i < len(out); i++ {
		merged = append(merged, out[i])
		if answers, ok := late[i]; ok {
			for i+1 < len(out) && out[i+1].Role == "tool" {
				i++
				merged = append(merged, out[i])
			}
			merged = append(merged, answers...)
		}
	}
	return merged, nil
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
// <message from="u1" kind="person">…</message>. The text and the ref are
// escaped, so nothing a speaker writes can close the frame or open another:
// only frames say who wrote what.
func frame(e db.EventRow, tag, text string) string {
	kind := "person"
	if e.ActorType == string(types.ActorAgent) {
		kind = "agent"
	}
	return "<" + tag + ` from="` + escAttr.Replace(e.ActorRef) + `" kind="` + kind + `">` +
		escText.Replace(text) + "</" + tag + ">"
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
// Only an image that is actually sent gets a signed URL: a stored file is
// otherwise named by its key, and signatures must not land in prompts,
// traces or summaries.
func userMessage(m types.MessageText, current bool, spec *Spec, sign func(string) string) provider.Message {
	text := m.Text
	if len(m.Attachments) == 0 {
		return provider.Message{Role: "user", Content: text}
	}
	var images []provider.Part
	for _, a := range m.Attachments {
		isImage := strings.HasPrefix(a.Mime, "image/")
		var url string
		if isImage && current && spec.Settings.Vision { // sign only what will be sent
			url = a.URL
			if a.File != "" {
				url = ""
				if sign != nil {
					url = sign(a.File)
				}
			}
		}
		switch {
		case !isImage:
			// A stored file is named by its key, never by a signed URL:
			// signed URLs must not land in prompts, traces or summaries.
			ref := a.URL
			if a.File != "" {
				ref = a.File
			}
			text += "\n[attachment: " + ref + " (" + a.Mime + ")]"
		case !current || !spec.Settings.Vision || url == "":
			text += "\n[image: " + a.Mime + "]"
		default:
			images = append(images, provider.Part{Type: "image_url", ImageURL: &provider.ImageURL{URL: url}})
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
	if len(s) > maxReplayArgs { // a row from before the bounds: never re-send megabytes
		return argsMarker(s)
	}
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
