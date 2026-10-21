package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/mcp"
	"github.com/alash3al/hfy/internal/provider"
	"github.com/alash3al/hfy/internal/types"
)

type providerMessage = provider.Message

// compactAt is the share of context_window (the model's real window) at
// which history is summarized: the rest is room for the reply and the tool
// results of the step, so a request never overflows before compaction.
const compactAt = 0.8

// summarizeHook triggers summarization before a model request once the
// prompt reached compactAt of the agent's context_window, then rebuilds the
// loop's context.
func (rt *Runtime) summarizeHook(env *TurnEnv) func(context.Context, *loop) error {
	if rt.Summ == nil {
		return nil
	}
	return func(ctx context.Context, l *loop) error {
		if float64(l.promptSize()) < compactAt*float64(l.spec.Settings.ContextWindow) {
			return nil
		}
		changed, err := rt.summarize(ctx, env)
		if err != nil || !changed {
			return err
		}
		bc, err := rt.BuildContext(ctx, env.ConversationID, env.TurnID, l.spec)
		if err != nil {
			return err
		}
		l.system, l.msgs = bc.System, bc.Msgs
		l.measure(0)
		l.deriveActivated(ctx)
		return nil
	}
}

// summarize appends a summary event covering every turn except the current one
// and the last keep_recent turns. History is never deleted. It reports whether
// a summary was written.
func (rt *Runtime) summarize(ctx context.Context, env *TurnEnv) (bool, error) {
	sm := rt.Cfg.Summarizer
	prev, after, err := rt.summaryStart(ctx, env.ConversationID)
	if err != nil {
		return false, err
	}
	events, err := rt.Store.LoadContext(ctx, env.ConversationID, after, env.TurnID)
	if err != nil {
		return false, err
	}
	var turns []int64
	for _, e := range events {
		if e.Type == types.EventTurn && e.ID != env.TurnID {
			turns = append(turns, e.ID)
		}
	}
	if len(turns) <= sm.KeepRecent {
		return false, nil
	}
	covered := turns[:len(turns)-sm.KeepRecent]
	coveredSet := map[int64]bool{}
	for _, id := range covered {
		coveredSet[id] = true
	}
	var inScope []db.EventRow
	for _, e := range events {
		if coveredSet[e.TurnID()] {
			inScope = append(inScope, e)
		}
	}
	transcript, err := rt.transcriptOf(ctx, inScope)
	if err != nil {
		return false, err
	}

	actor, err := rt.Svc.AgentActor(ctx, types.AgentSummarizer)
	if err != nil {
		return false, err
	}
	text := ""
	if prev != nil {
		text = prev.Text
	}
	// The summarizer's input must fit its own window: fold the transcript in chunks.
	chunkChars := max(rt.Summ.Settings.ContextWindow*3, 2000)
	for _, chunk := range splitChunks(transcript, chunkChars) {
		input := "New conversation to fold in:\n" + chunk
		if text != "" {
			input = "Summary so far:\n" + text + "\n\n" + input
		}
		sink := &quietSink{rt: rt, env: env, actor: actor, persisted: true}
		l := newLoop(rt, rt.Summ, sink, env, rt.Summ.System)
		l.msgs = []provider.Message{{Role: "user", Content: input}}
		res, err := l.run(ctx)
		if err != nil {
			return false, err
		}
		if res.Outcome != Done || strings.TrimSpace(res.Final) == "" {
			return false, fmt.Errorf("summarizer failed: %s", res.Reason)
		}
		text = strings.TrimSpace(res.Final)
	}

	lastCovered, err := rt.Store.EventByID(ctx, rt.Store.Pool, covered[len(covered)-1])
	if err != nil {
		return false, err
	}
	ev, err := rt.Store.AppendEvent(ctx, rt.Store.Pool, db.NewEvent{
		ConversationID: env.ConversationID, ParentID: &env.TurnID, ActorID: actor, Scope: env.Scope,
		Payload: types.Summary{Text: text, CoversUntilTurn: lastCovered.UUID.String()}})
	if err != nil {
		return false, err
	}
	return true, rt.Store.SetSummary(ctx, rt.Store.Pool, env.ConversationID, ev.ID)
}

// transcriptOf renders events for the summarizer, one framed line per
// message, call or result (see frame), so the one summary every agent reads
// keeps who said and did what.
func (rt *Runtime) transcriptOf(ctx context.Context, events []db.EventRow) (string, error) {
	var b strings.Builder
	for _, e := range events {
		switch e.Type {
		case types.EventMessageText:
			m, err := e.Payload[types.MessageText]()
			if err != nil {
				return "", err
			}
			b.WriteString(frame(e, "message", rt.userMessage(ctx, m, false, rt.Summ).Content) + "\n")
		case types.EventToolCall:
			c, err := e.Payload[types.ToolCall]()
			if err != nil {
				return "", err
			}
			b.WriteString(frame(e, "tool_call", c.Name+" "+argsText(c.Arguments)) + "\n")
		case types.EventToolResult:
			r, err := e.Payload[types.ToolResult]()
			if err != nil {
				return "", err
			}
			c := toolText(mcp.Result{Content: r.Content, IsError: r.IsError})
			if len(c) > 2000 {
				c = c[:2000] + "..."
			}
			b.WriteString("<tool_result>" + escText.Replace(c) + "</tool_result>\n")
		}
	}
	return b.String(), nil
}

// splitChunks cuts text on line boundaries into pieces of at most n bytes.
func splitChunks(s string, n int) []string {
	var out []string
	var cur strings.Builder
	for _, line := range strings.SplitAfter(s, "\n") {
		for len(line) > n { // a single huge line
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
			out = append(out, line[:n])
			line = line[n:]
		}
		if cur.Len()+len(line) > n && cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
		cur.WriteString(line)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}
