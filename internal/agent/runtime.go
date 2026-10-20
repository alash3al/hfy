// Package agent is the one agent loop. The main agent, sub-agents and the
// summarizer all run it; they differ only in their Spec and in what the
// loop's sink persists.
package agent

import (
	"context"
	_ "embed"
	"maps"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/mcp"
	"github.com/alash3al/hfy/internal/mcp/harness"
	"github.com/alash3al/hfy/internal/provider"
	"github.com/alash3al/hfy/internal/svc"
	"github.com/alash3al/hfy/internal/types"
)

//go:embed prompts/interrupted.txt
var promptInterrupted string

//go:embed prompts/rejected.txt
var promptRejected string

//go:embed prompts/repeated.txt
var promptRepeated string

//go:embed prompts/summarizer.txt
var promptSummarizer string

// Spec is one resolved agent definition.
type Spec struct {
	Name        string // actor ref: main, a sub-agent name, or summarizer
	Description string
	Client      *provider.Client
	Model       string // the model part of <provider>/<model>
	Settings    config.Settings
	System      string
	Limits      config.Limits
	MCP         []string        // nil = all servers, through tool search
	Harness     map[string]bool // harness tools this agent may use
}

func (s *Spec) isMain() bool { return s.Name == types.AgentMain }

// Runtime is everything the loop needs besides the turn itself.
type Runtime struct {
	Cfg   *config.Config
	Store *db.Store
	Svc   *svc.Service
	MCP   *mcp.Manager

	Providers map[string]*provider.Client
	Main      *Spec
	Subs      map[string]*Spec
	Summ      *Spec // nil when no summarizer is configured
	Embedder  *provider.Client
	EmbModel  string

	harness *mcp.Local
	cancels sync.Map // turn event id -> context.CancelCauseFunc
	http    *attachmentFetcher
}

func NewRuntime(cfg *config.Config, store *db.Store, mgr *mcp.Manager) (*Runtime, error) {
	rt := &Runtime{
		Cfg: cfg, Store: store, Svc: svc.New(store, cfg), MCP: mgr,
		Providers: map[string]*provider.Client{}, Subs: map[string]*Spec{},
		harness: mcp.NewLocal(harness.Server),
	}
	for name, p := range cfg.Providers {
		rt.Providers[name] = provider.New(name, p.URL, p.APIKey, p.Timeout.D())
	}
	spec := func(name, model string, st config.Settings, system string, lim config.Limits, servers []string) *Spec {
		prov, m := config.SplitModel(model)
		return &Spec{Name: name, Client: rt.Providers[prov], Model: m, Settings: st, System: system,
			Limits: lim, MCP: servers, Harness: map[string]bool{}}
	}
	a := cfg.Agent
	rt.Main = spec(types.AgentMain, a.Model, a.Settings, a.System, a.Limits, a.MCP)
	if a.MCP == nil {
		rt.Main.Harness[harness.ToolSearch] = true
	}
	if cfg.Embeddings != nil {
		prov, m := config.SplitModel(cfg.Embeddings.Model)
		rt.Embedder, rt.EmbModel = rt.Providers[prov], m
		rt.Main.Harness[harness.SessionSearch] = true
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Subagents)) {
		sc := cfg.Subagents[name]
		s := spec(name, sc.Model, sc.Settings, sc.System, sc.Limits, sc.MCP)
		s.Description = sc.Description
		if sc.MCP == nil {
			s.Harness[harness.ToolSearch] = true
		}
		rt.Subs[name] = s
		rt.Main.Harness[name] = true
	}
	if sm := cfg.Summarizer; sm != nil {
		system := sm.System
		if system == "" {
			system = promptSummarizer
		}
		rt.Summ = spec(types.AgentSummarizer, sm.Model, sm.Settings, system, config.Limits{}, []string{})
	}
	rt.http = newAttachmentFetcher(cfg)
	rt.registerHarness()
	mgr.Add(rt.harness)
	return rt, nil
}

// Boot runs the config-dependent start-up work: embeddings backfill and the
// recurring retention job.
func (rt *Runtime) Boot(ctx context.Context) error {
	if err := rt.Store.Boot(ctx); err != nil {
		return err
	}
	if rt.Cfg.Embeddings != nil {
		if err := rt.Svc.EnqueueEmbed(ctx, rt.Store.Pool); err != nil { // backfill
			return err
		}
	}
	_, err := rt.Store.Enqueue(ctx, rt.Store.Pool, db.NewJob{
		Type: types.JobRetentionClean, Queue: types.QueueMaintenance, Payload: struct{}{},
		MaxAttempts: rt.Cfg.Queue.MaxAttempts, Key: "retention_cleanup",
		DedupeStates: []types.JobState{types.JobQueued, types.JobRunning},
	})
	return err
}

// CancelTurn asks the worker running a turn (in this process) to stop it.
func (rt *Runtime) CancelTurn(turnID int64) {
	if c, ok := rt.cancels.Load(turnID); ok {
		c.(context.CancelCauseFunc)(errStopped)
	}
}

// inlineImages reports whether images must be sent as bytes: local servers
// (Ollama and friends) cannot fetch URLs on the network the harness sees.
func inlineImages(c *provider.Client) bool {
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return false
	}
	h := u.Hostname()
	if h == "localhost" || strings.HasSuffix(h, ".local") || strings.HasSuffix(h, ".internal") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate()
	}
	return !strings.Contains(h, ".") // docker/k8s service names
}

// ListenCancel relays stop requests (NOTIFY from any instance) to the turn
// running in this process. It blocks until ctx ends.
func (rt *Runtime) ListenCancel(ctx context.Context) {
	rt.Store.Listen(ctx, func(_, payload string) {
		if id, err := strconv.ParseInt(payload, 10, 64); err == nil {
			rt.CancelTurn(id)
		}
	}, nil, db.ChanCancel)
}
