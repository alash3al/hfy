package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/provider"
	"github.com/alash3al/hfy/internal/types"
)

type providerMessage = provider.Message

// summarizeHook triggers summarization before a model request once the prompt
// reached the agent's context_window, then rebuilds the loop's context.
func (rt *Runtime) summarizeHook(env *TurnEnv) func(context.Context, *loop) error {
	if rt.Summ == nil {
		return nil
	}
	return func(ctx context.Context, l *loop) error {
		if l.lastPrompt < int64(l.spec.Settings.ContextWindow) {
			return nil
		}
		changed, err := rt.summarize(ctx, env)
		if err != nil || !changed {
			return err
		}
		bc, err := rt.BuildContext(ctx, env.SessionID, env.TurnID, l.spec)
		if err != nil {
			return err
		}
		l.system, l.msgs = bc.System, bc.Msgs
		l.lastPrompt = estimateTokens(bc.System, bc.Msgs)
		l.deriveActivated()
		return nil
	}
}

// summarize appends a summary event covering every turn except the current one
// and the last keep_recent turns. History is never deleted. It reports whether
// a summary was written.
func (rt *Runtime) summarize(ctx context.Context, env *TurnEnv) (bool, error) {
	sm := rt.Cfg.Summarizer
	prev, err := rt.Store.Summary(ctx, env.SessionID)
	if err != nil {
		return false, err
	}
	var after int64
	if prev != nil {
		if id, err := rt.Store.TurnIDByUUID(ctx, env.SessionID, mustUUID(prev.CoversUntilTurn)); err == nil {
			after = id
		}
	}
	events, err := rt.Store.LoadContext(ctx, env.SessionID, after, env.TurnID)
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
	transcript := transcriptOf(rt.messagesFrom(ctx, inScope, -1, rt.Summ))

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

	lastCovered, err := rt.Store.EventByID(ctx, covered[len(covered)-1])
	if err != nil {
		return false, err
	}
	ev, err := rt.Store.AppendEvent(ctx, rt.Store.Pool, db.NewEvent{
		SessionID: env.SessionID, ParentID: &env.TurnID, ActorID: actor, Scope: env.Scope,
		Payload: types.Summary{Text: text, CoversUntilTurn: lastCovered.UUID.String()}})
	if err != nil {
		return false, err
	}
	return true, rt.Store.SetSummary(ctx, rt.Store.Pool, env.SessionID, ev.ID)
}

func transcriptOf(msgs []provider.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case "user":
			b.WriteString("User: " + m.Content + "\n")
		case "assistant":
			if m.Content != "" {
				b.WriteString("Assistant: " + m.Content + "\n")
			}
			for _, tc := range m.ToolCalls {
				b.WriteString("Assistant called " + tc.Name + " " + tc.Arguments + "\n")
			}
		case "tool":
			c := m.Content
			if len(c) > 2000 {
				c = c[:2000] + "..."
			}
			b.WriteString("Tool result: " + c + "\n")
		}
	}
	return b.String()
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
