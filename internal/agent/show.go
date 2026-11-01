package agent

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"uuid"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/provider"
	"github.com/alash3al/hfy/internal/types"
)

// PromptView is the model request the current code and config would build for
// a conversation's newest turn, from its stored events.
type PromptView struct {
	Agent          string
	Body           []byte // the JSON body the provider adapter would send, without tools
	EstimateTokens int64
	ContextWindow  int
}

// ShowPrompt builds a conversation's prompt for agentName ("" = the agent of
// its newest turn) the way a turn would, and reads only: it calls no model,
// runs no tool and writes nothing. Stored files get the placeholder URL
// "hfy-file:<key>" instead of a signature; tools are left out because they
// need live servers.
func (rt *Runtime) ShowPrompt(ctx context.Context, conversation uuid.UUID, agentName string) (PromptView, error) {
	conv, err := rt.Store.ConversationByUUID(ctx, conversation)
	if db.IsNotFound(err) {
		return PromptView{}, fmt.Errorf("no conversation %s", conversation)
	}
	if err != nil {
		return PromptView{}, err
	}
	turnID, err := rt.Store.LatestTurnID(ctx, rt.Store.Pool, conv.ID)
	if db.IsNotFound(err) {
		return PromptView{}, fmt.Errorf("conversation %s has no turn yet", conversation)
	}
	if err != nil {
		return PromptView{}, err
	}
	if agentName == "" {
		turn, err := rt.Store.EventByID(ctx, rt.Store.Pool, turnID)
		if err != nil {
			return PromptView{}, err
		}
		t, err := turn.Payload[types.Turn]()
		if err != nil {
			return PromptView{}, err
		}
		agentName = t.AgentName()
	}
	spec, ok := rt.agents[agentName]
	if !ok {
		return PromptView{}, fmt.Errorf("agent %q is not configured (have: %v)", agentName, slices.Sorted(maps.Keys(rt.agents)))
	}
	system, msgs, err := rt.promptFor(ctx, conv.ID, turnID, spec, func(key string) string { return "hfy-file:" + key })
	if err != nil {
		return PromptView{}, err
	}
	req := (&loop{spec: spec, system: system, msgs: msgs}).request(nil)
	if c, ok := spec.Client.(controlled); ok {
		req.Controls = c.ctl
	}
	var maxParam string
	if a, ok := rt.Cfg.AgentNamed(agentName); ok {
		prov, _ := config.SplitModel(a.Model)
		maxParam = rt.Cfg.Providers[prov].MaxTokensParam
	}
	body, err := provider.RequestBody(req, maxParam)
	if err != nil {
		return PromptView{}, err
	}
	return PromptView{Agent: agentName, Body: body, EstimateTokens: estimateTokens(system, msgs), ContextWindow: spec.Settings.ContextWindow}, nil
}
