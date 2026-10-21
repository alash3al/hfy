package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/alash3al/hfy/internal/mcp"
	"github.com/alash3al/hfy/internal/mcp/builtins"
	"github.com/alash3al/hfy/internal/provider"
	"github.com/alash3al/hfy/internal/types"
)

// registerBuiltins serves the built-ins from the in-process "hfy" server, so
// they take the same path as every other tool.
func (rt *Runtime) registerBuiltins() {
	for _, c := range builtins.CoreTools() {
		rt.builtins.Add(c.Tool, c.Run)
	}
	rt.builtins.Add(builtins.ToolSearchTool(), rt.toolSearch)
	if rt.Cfg.Embeddings != nil {
		rt.builtins.Add(builtins.ConversationSearchTool(), rt.conversationSearch)
	}
	for _, name := range slices.Sorted(maps.Keys(rt.Subs)) {
		sub := rt.Subs[name]
		rt.builtins.Add(builtins.SubagentTool(name, sub.Description),
			func(ctx context.Context, args json.RawMessage) (mcp.Result, error) {
				var in struct {
					Task string `json:"task"`
				}
				if err := json.Unmarshal(args, &in); err != nil || strings.TrimSpace(in.Task) == "" {
					return errResult("task is required"), nil
				}
				return rt.runSubagent(ctx, sub, in.Task)
			})
	}
}

type searchArgs struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

func (rt *Runtime) toolSearch(ctx context.Context, raw json.RawMessage) (mcp.Result, error) {
	var a searchArgs
	if json.Unmarshal(raw, &a) != nil || strings.TrimSpace(a.Query) == "" {
		return errResult("query is required"), nil
	}
	if a.Limit <= 0 || a.Limit > 20 {
		a.Limit = 8
	}
	var servers []string // nil = all
	if cc := callFrom(ctx); cc != nil {
		servers = cc.spec.MCP
	}
	all, errs := rt.MCP.Tools(ctx, servers)
	words := strings.Fields(strings.ToLower(a.Query))
	type scored struct {
		t mcp.Tool
		n int
	}
	var hits []scored
	for _, t := range all {
		if t.Server == builtins.Server {
			continue
		}
		name := strings.ToLower(t.FullName())
		desc := strings.ToLower(t.Description)
		n := 0
		for _, w := range words {
			if strings.Contains(name, w) {
				n += 3
			}
			if strings.Contains(desc, w) {
				n++
			}
		}
		if n > 0 {
			hits = append(hits, scored{t, n})
		}
	}
	slices.SortStableFunc(hits, func(a, b scored) int { return cmp.Compare(b.n, a.n) })
	if len(hits) > a.Limit {
		hits = hits[:a.Limit]
	}
	if len(hits) == 0 {
		msg := "No tools matched. Try different keywords."
		if len(errs) > 0 {
			msg += " Some servers were unreachable."
		}
		return mcp.Result{Content: msg}, nil
	}
	type out struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	}
	res := make([]out, len(hits))
	for i, h := range hits {
		res[i] = out{mcp.WireName(h.t), h.t.Description, h.t.Schema}
	}
	b, _ := json.Marshal(res)
	return mcp.Result{Content: string(b)}, nil
}

func (rt *Runtime) conversationSearch(ctx context.Context, raw json.RawMessage) (mcp.Result, error) {
	var a searchArgs
	if json.Unmarshal(raw, &a) != nil || strings.TrimSpace(a.Query) == "" {
		return errResult("query is required"), nil
	}
	if a.Limit <= 0 || a.Limit > 20 {
		a.Limit = 5
	}
	cc := callFrom(ctx)
	if cc == nil {
		return mcp.Result{}, errors.New("conversation_search needs a turn")
	}
	// The harness injects the scope; the model never passes it.
	vecs, err := rt.Embedder.Embed(ctx, rt.EmbModel, []string{truncate(a.Query, maxEmbedChars)},
		rt.recorder(&cc.env.TurnID, types.OutboundEmbedding))
	if err != nil {
		return errResult("search is unavailable: " + err.Error()), nil
	}
	hits, err := rt.Store.SearchEmbeddings(ctx, cc.env.Scope, vecs[0], a.Limit)
	if err != nil {
		return mcp.Result{}, err
	}
	if len(hits) == 0 {
		return mcp.Result{Content: "No matching earlier messages."}, nil
	}
	b, _ := json.Marshal(hits)
	return mcp.Result{Content: string(b)}, nil
}

// maxEmbedChars caps text sent to the embedding model (stored and queried alike).
const maxEmbedChars = 8000

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

// runSubagent runs a specialist in isolation: it sees only the task, no parent
// history and no parent vars. Its usage counts toward the parent's turn.
func (rt *Runtime) runSubagent(ctx context.Context, sub *Spec, task string) (mcp.Result, error) {
	cc := callFrom(ctx)
	if cc == nil {
		return mcp.Result{}, errors.New("sub-agents run inside a turn")
	}
	actor, err := rt.Svc.AgentActor(ctx, sub.Name)
	if err != nil {
		return mcp.Result{}, err
	}
	sink := &quietSink{rt: rt, env: cc.env, actor: actor, persisted: true}
	l := newLoop(rt, sub, sink, cc.env, sub.System)
	l.msgs = []provider.Message{{Role: "user", Content: task}}
	res, err := l.run(ctx)
	if err != nil {
		return mcp.Result{}, err
	}
	if res.Outcome != Done {
		return errResult(sub.Name + " failed: " + res.Reason), nil
	}
	return mcp.Result{Content: res.Final}, nil
}
