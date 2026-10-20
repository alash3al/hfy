package agent

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/alash3al/hfy/internal/db"
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
	summary, err := rt.Store.Summary(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	var after int64
	if summary != nil {
		if id, err := rt.Store.TurnIDByUUID(ctx, sessionID, mustUUID(summary.CoversUntilTurn)); err == nil {
			after = id
		}
	}
	events, err := rt.Store.LoadContext(ctx, sessionID, after, currentTurn)
	if err != nil {
		return nil, err
	}
	bc := &BuiltContext{Vars: map[string]string{}}
	for _, e := range events {
		if e.Type == types.EventTurn {
			bc.Turns = append(bc.Turns, e.ID)
			if e.ID == currentTurn {
				var t types.Turn
				if json.Unmarshal(e.Data, &t) == nil && t.Vars != nil {
					bc.Vars = t.Vars
				}
			}
		}
	}
	bc.System = svc.Render(spec.System, bc.Vars)
	if summary != nil {
		bc.System += "\n\nSummary of the earlier conversation:\n" + summary.Text
	}
	bc.Msgs = rt.messagesFrom(ctx, events, currentTurn, spec)
	return bc, nil
}

// messagesFrom turns events into provider messages. Only message.text,
// tool_call, tool_call.interrupted and tool_result reach the model.
func (rt *Runtime) messagesFrom(ctx context.Context, events []db.EventRow, currentTurn int64, spec *Spec) []provider.Message {
	var out []provider.Message
	var asst *provider.Message
	pending := map[string]bool{}
	flush := func() {
		if asst != nil {
			out = append(out, *asst)
			asst = nil
		}
	}
	for _, e := range events {
		switch e.Type {
		case types.EventMessageText:
			var m types.MessageText
			if json.Unmarshal(e.Data, &m) != nil {
				continue
			}
			flush()
			if e.ActorType == string(types.ActorAgent) {
				asst = &provider.Message{Role: "assistant", Content: m.Text}
				continue
			}
			out = append(out, rt.userMessage(ctx, m, e.TurnID() == currentTurn, spec))
		case types.EventToolCall:
			var c types.ToolCall
			if json.Unmarshal(e.Data, &c) != nil {
				continue
			}
			if asst == nil {
				asst = &provider.Message{Role: "assistant"}
			}
			asst.ToolCalls = append(asst.ToolCalls, provider.ToolCall{
				ID: c.CallID, Name: wireOf(rt, c.Name), Arguments: argsText(c.Arguments)})
			pending[c.CallID] = true
		case types.EventToolResult:
			var r types.ToolResult
			if json.Unmarshal(e.Data, &r) != nil || !pending[r.CallID] {
				continue
			}
			flush()
			delete(pending, r.CallID)
			content := r.Content
			if r.IsError && !strings.HasPrefix(content, "Error") {
				content = "Error: " + content
			}
			out = append(out, provider.Message{Role: "tool", ToolCallID: r.CallID, Content: content})
		case types.EventToolCallInterrupted:
			var c types.ToolCallInterrupted
			if json.Unmarshal(e.Data, &c) != nil || !pending[c.CallID] {
				continue
			}
			flush()
			delete(pending, c.CallID)
			out = append(out, provider.Message{Role: "tool", ToolCallID: c.CallID, Content: strings.TrimSpace(promptInterrupted)})
		}
	}
	flush()
	// A call with no result at all (stopped mid-flight) must not break the API contract.
	for id := range pending {
		out = append(out, provider.Message{Role: "tool", ToolCallID: id, Content: strings.TrimSpace(promptInterrupted)})
	}
	return out
}

// userMessage expands image attachments only in the turn they arrive in; later
// turns get a short placeholder (small context, and presigned URLs expire).
func (rt *Runtime) userMessage(ctx context.Context, m types.MessageText, current bool, spec *Spec) provider.Message {
	if len(m.Attachments) == 0 {
		return provider.Message{Role: "user", Content: m.Text}
	}
	text := m.Text
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

// wireOf maps a stored full name ("files.read") to the provider function name.
func wireOf(rt *Runtime, full string) string {
	server, tool, ok := strings.Cut(full, ".")
	if !ok {
		return full
	}
	return wireJoin(server, tool)
}
