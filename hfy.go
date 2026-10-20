// Package hfy is a simple harness as a service: one agent, served over HTTP,
// durable on Postgres. It can run as the hfy binary or be embedded:
//
//	cfg, err := hfy.LoadConfig("harness.yaml")
//	if err != nil { ... }
//	err = hfy.New(cfg).Run(ctx)
package hfy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/alash3al/hfy/internal/agent"
	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/httpapi"
	"github.com/alash3al/hfy/internal/jobs"
	"github.com/alash3al/hfy/internal/mcp"
	"github.com/alash3al/hfy/internal/types"
)

// Config is the parsed harness.yaml.
type Config = config.Config

// LoadConfig reads, expands and validates a config file.
func LoadConfig(path string) (*Config, error) { return config.Load(path) }

type App struct{ cfg *config.Config }

func New(cfg *Config) *App { return &App{cfg: cfg} }

// Run serves the API and runs the workers in one process.
func (a *App) Run(ctx context.Context) error { return a.Serve(ctx, true) }

type instance struct {
	store   *db.Store
	rt      *agent.Runtime
	clients []*mcp.Client
}

func (i *instance) close() {
	for _, c := range i.clients {
		c.Close()
	}
	i.store.Close()
}

// boot connects, migrates and prepares the runtime. Every process does this.
func (a *App) boot(ctx context.Context, extraConns int) (*instance, error) {
	var emb *db.Embeddings
	if e := a.cfg.Embeddings; e != nil {
		_, model := config.SplitModel(e.Model)
		emb = &db.Embeddings{Model: model, Dims: e.Dimensions}
	}
	store, err := db.Open(ctx, a.cfg.Storage.URL, int32(extraConns+a.cfg.Queue.Workers+8), emb)
	if err != nil {
		return nil, err
	}
	if _, err := store.Migrate(ctx, "up"); err != nil {
		store.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	mgr := mcp.NewManager()
	inst := &instance{store: store, clients: mcp.StartAll(ctx, mgr, a.cfg.MCP)}
	if inst.rt, err = agent.NewRuntime(a.cfg, store, mgr); err != nil {
		inst.close()
		return nil, err
	}
	if err := inst.rt.Boot(ctx); err != nil {
		inst.close()
		return nil, fmt.Errorf("boot: %w", err)
	}
	return inst, nil
}

// Serve runs the HTTP API and SSE; with work it also runs the workers.
func (a *App) Serve(ctx context.Context, work bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	inst, err := a.boot(ctx, 16)
	if err != nil {
		return err
	}
	defer inst.close()

	hub := httpapi.NewHub(inst.store)
	api := httpapi.New(a.cfg, inst.store, inst.rt.Svc, hub)
	srv := &http.Server{
		Addr: a.cfg.Server.Listen, Handler: api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx }, // shutdown ends SSE streams
	}
	var wg sync.WaitGroup
	wg.Go(func() { hub.Run(ctx) })
	if work {
		w := a.worker(inst, nil)
		wg.Go(func() { w.Run(ctx, a.cfg.Queue.Workers) })
		wg.Go(func() { inst.rt.ListenCancel(ctx) })
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("hfy listening", "addr", a.cfg.Server.Listen, "workers", work)

	select {
	case err = <-errc: // e.g. the address is in use: stop everything else too
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err = srv.Shutdown(shutdown)
	}
	cancel()
	wg.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

// Work runs only the workers. queues is priority-ordered; nil = all queues.
func (a *App) Work(ctx context.Context, queues []string, workers int) error {
	inst, err := a.boot(ctx, 0)
	if err != nil {
		return err
	}
	defer inst.close()
	if workers <= 0 {
		workers = a.cfg.Queue.Workers
	}
	w := a.worker(inst, queues)
	var wg sync.WaitGroup
	wg.Go(func() { inst.rt.ListenCancel(ctx) })
	slog.Info("hfy workers started", "workers", workers, "queues", queues)
	w.Run(ctx, workers)
	wg.Wait()
	return nil
}

func (a *App) worker(inst *instance, queues []string) *jobs.Worker {
	if len(queues) == 0 {
		queues = []string{types.QueueTurns, types.QueueEmbeddings, types.QueueMaintenance}
	}
	w := jobs.NewWorker(inst.store, a.cfg.Queue.Lease.D(), queues)
	inst.rt.RegisterJobs(w)
	return w
}

// Migrate runs "up", "down" or "status" and returns the report.
func (a *App) Migrate(ctx context.Context, cmd string) (string, error) {
	store, err := db.Open(ctx, a.cfg.Storage.URL, 4, nil)
	if err != nil {
		return "", err
	}
	defer store.Close()
	return store.Migrate(ctx, cmd)
}
