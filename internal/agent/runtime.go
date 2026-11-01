// Package agent is the one agent loop. The main agent, sub-agents and the
// summarizer all run it; they differ only in their Spec and in what the
// loop's sink persists.
package agent

import (
	"context"
	_ "embed"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gocloud.dev/blob"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/files"
	"github.com/alash3al/hfy/internal/mcp"
	"github.com/alash3al/hfy/internal/mcp/builtins"
	"github.com/alash3al/hfy/internal/provider"
	"github.com/alash3al/hfy/internal/svc"
	"github.com/alash3al/hfy/internal/types"
)

//go:embed prompts/interrupted.txt
var promptInterrupted string

//go:embed prompts/rejected.txt
var promptRejected string

// The declined/timeout notes are shared with svc (HeldResultText), which
// ends turns with them; one source of truth, svc/prompts.
var (
	promptDeclined = svc.PromptDeclined
	promptTimeout  = svc.PromptTimeout
)

//go:embed prompts/repeated.txt
var promptRepeated string

//go:embed prompts/summarizer.txt
var promptSummarizer string

//go:embed prompts/room.txt
var promptRoom string

//go:embed prompts/notes.txt
var promptNotes string

// core gives the spec hfy's core tools (builtins.CoreTools).
func (s *Spec) core() {
	for _, c := range builtins.CoreTools() {
		s.Builtins[c.Tool.Name] = true
	}
}

// Spec is one resolved agent definition.
type Spec struct {
	Name        string // actor ref: main, a sub-agent name, or summarizer
	Description string
	Client      provider.Adapter
	Model       string // the model part of <provider>/<model>
	Settings    config.Settings
	System      string
	Limits      config.Limits
	MCP         []string        // nil = all servers, through tool search
	Builtins    map[string]bool // built-in (hfy.*) tools this agent may use
	Top         bool            // a configured agent that runs turns (not a sub-agent or the summarizer)
	Approval    *config.Approval
}

// controlled is the client of one agent: every call it makes carries the
// agent's request controls, so the main agent, agents, sub-agents and the
// summarizer behave alike.
type controlled struct {
	provider.Adapter
	ctl provider.Controls
}

func (c controlled) Chat(ctx context.Context, req provider.Request, o provider.Opts) (*provider.Response, error) {
	req.Controls = c.ctl
	return c.Adapter.Chat(ctx, req, o)
}

// Runtime is everything the loop needs besides the turn itself.
type Runtime struct {
	Cfg    *config.Config
	Store  *db.Store
	Svc    *svc.Service
	mcp    *mcp.Manager
	bucket *blob.Bucket

	agents map[string]*Spec // the configured agents by name; "main" is the default
	subs   map[string]*Spec
	summ   *Spec // nil when no summarizer is configured

	signedURLs sync.Map     // file key -> signedURL: one stable URL per file per TTL window
	urlStores  atomic.Int64 // storeSignedURL calls since the last prune
	builtins   []builtin    // hfy.* tools, in the order they are offered
	cancels    sync.Map     // turn event id -> context.CancelCauseFunc
}

func NewRuntime(cfg *config.Config, store *db.Store, mgr *mcp.Manager) (*Runtime, error) {
	rt := &Runtime{
		Cfg: cfg, Store: store, Svc: svc.New(store, cfg), mcp: mgr,
		agents: map[string]*Spec{}, subs: map[string]*Spec{},
	}
	providers := map[string]provider.Adapter{}
	for name, p := range cfg.Providers {
		if p.Type == "mock" {
			o := provider.MockOptions{Reply: p.Mock.Reply, Reasoning: p.Mock.Reasoning, Delay: p.Mock.Delay.D(),
				ErrorStatus: p.Mock.ErrorStatus}
			if p.Mock.ToolCall != nil {
				o.ToolName, o.ToolArguments = p.Mock.ToolCall.Name, p.Mock.ToolCall.Arguments
			}
			providers[name] = provider.NewMock(name, o)
			continue
		}
		client := provider.New(name, p.URL, p.APIKey, p.Timeout.D())
		client.MaxTokensParam = p.MaxTokensParam
		providers[name] = client
	}
	spec := func(name, model string, st config.Settings, system string, lim config.Limits, servers []string) *Spec {
		prov, m := config.SplitModel(model)
		ctl := provider.Controls{ReasoningEffort: st.ReasoningEffort, MaxOutputTokens: st.MaxOutputTokens,
			CallTimeout: st.CallTimeout.D(), Extra: st.Extra}
		return &Spec{Name: name, Client: controlled{providers[prov], ctl}, Model: m, Settings: st, System: system,
			Limits: lim, MCP: servers, Builtins: map[string]bool{}}
	}
	for name, a := range cfg.Agents {
		s := spec(name, a.Model, a.Settings, a.System, a.Limits, a.MCP)
		s.Top, s.Approval = true, a.Approval
		s.core()
		s.Builtins[builtins.ToolSearch] = a.MCP == nil
		s.Builtins[builtins.AskUser] = a.AskUser // opt-in, top agents only
		for sub := range cfg.Subagents {
			s.Builtins[sub] = true // every agent may delegate to every sub-agent
		}
		rt.agents[name] = s
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Subagents)) {
		sc := cfg.Subagents[name]
		s := spec(name, sc.Model, sc.Settings, sc.System, sc.Limits, sc.MCP)
		s.Description = sc.Description
		s.core()
		if sc.MCP == nil {
			s.Builtins[builtins.ToolSearch] = true
		}
		rt.subs[name] = s
	}
	if sm := cfg.Summarizer; sm != nil {
		system := sm.System
		if system == "" {
			system = promptSummarizer
		}
		rt.summ = spec(types.AgentSummarizer, sm.Model, sm.Settings, system, config.Limits{}, []string{})
	}
	rt.registerBuiltins()
	return rt, nil
}

// Boot runs the start-up work: the recurring retention job.
func (rt *Runtime) Boot(ctx context.Context) error {
	_, err := rt.Store.Enqueue(ctx, rt.Store.Pool, db.NewJob{
		Type: types.JobRetentionClean, Queue: types.QueueMaintenance, Payload: struct{}{},
		MaxAttempts: rt.Cfg.Queue.MaxAttempts, Key: "retention_cleanup",
		DedupeStates: []types.JobState{types.JobQueued, types.JobRunning},
	})
	return err
}

// EnableFilesystem sets the bucket attachment downloads are signed against;
// nil (no files.dsn configured) leaves stored files unexpanded.
func (rt *Runtime) EnableFilesystem(b *blob.Bucket) { rt.bucket = b }

type signedURL struct {
	url string
	exp time.Time
}

// downloadURL returns a provider-reachable URL for a stored file, or ""
// when none can be made (no bucket, or file:// storage without public_url).
// URLs are cached until near expiry: a URL that changed on every step
// would bust the provider's prompt cache for the rest of the conversation.
func (rt *Runtime) downloadURL(ctx context.Context, key string) string {
	fs := rt.Cfg.Files
	if rt.bucket == nil || !fs.Uploads() {
		return ""
	}
	if v, ok := rt.signedURLs.Load(key); ok && time.Until(v.(signedURL).exp) > time.Minute {
		return v.(signedURL).url
	}
	var u string
	var exp time.Time
	if files.Direct(files.Scheme(fs.DSN)) {
		exp = time.Now().Add(fs.DownloadTTL.D())
		signed, err := rt.bucket.SignedURL(ctx, key, &blob.SignedURLOptions{Expiry: fs.DownloadTTL.D()})
		if err != nil {
			slog.Error("signing a download URL failed", "key", key, "err", err)
			return ""
		}
		u = signed
	} else if fs.PublicURL != "" {
		exp = time.Now().Add(fs.DownloadTTL.D())
		u = strings.TrimRight(fs.PublicURL, "/") + "/v1/files/" + key +
			"?exp=" + strconv.FormatInt(exp.Unix(), 10) + "&sig=" + files.SignGet(rt.Cfg.Server.Secret, key, exp)
	} else {
		return ""
	}
	rt.storeSignedURL(key, u, exp)
	return u
}

// signedURLCachePruneEvery is how many storeSignedURL calls pass before the
// expired entries are dropped: a long-lived worker's cache must not grow
// forever without a per-conversation limit to lean on.
const signedURLCachePruneEvery = 256

// storeSignedURL caches u for key until exp, pruning expired entries every
// signedURLCachePruneEvery stores.
func (rt *Runtime) storeSignedURL(key, u string, exp time.Time) {
	rt.signedURLs.Store(key, signedURL{url: u, exp: exp})
	if rt.urlStores.Add(1)%signedURLCachePruneEvery != 0 {
		return
	}
	now := time.Now()
	rt.signedURLs.Range(func(k, v any) bool {
		if !v.(signedURL).exp.After(now) {
			rt.signedURLs.Delete(k)
		}
		return true
	})
}

// CancelTurn asks the worker running a turn (in this process) to stop it;
// payload is the turn event id, as db.ChanCancel carries it.
func (rt *Runtime) CancelTurn(payload string) {
	id, err := strconv.ParseInt(payload, 10, 64)
	if c, ok := rt.cancels.Load(id); err == nil && ok {
		c.(context.CancelCauseFunc)(errStopped)
	}
}
