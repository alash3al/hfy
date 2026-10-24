package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"gocloud.dev/blob"

	"github.com/alash3al/hfy/internal/agent"
	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/db"
	"github.com/alash3al/hfy/internal/files"
	"github.com/alash3al/hfy/internal/httpapi"
	"github.com/alash3al/hfy/internal/jobs"
	"github.com/alash3al/hfy/internal/mcp"
	"github.com/alash3al/hfy/internal/metrics"
	"github.com/alash3al/hfy/internal/types"
)

type instance struct {
	store   *db.Store
	rt      *agent.Runtime
	clients []*mcp.Client
	bucket  *blob.Bucket
}

func (i *instance) close() {
	for _, c := range i.clients {
		c.Close()
	}
	if i.bucket != nil {
		i.bucket.Close()
	}
	i.store.Close()
}

// boot connects, migrates and prepares the runtime. Every process does this.
func boot(ctx context.Context, cfg *config.Config, extraConns int) (*instance, error) {
	var emb *db.Embeddings
	if e := cfg.Embeddings; e != nil {
		_, model := config.SplitModel(e.Model)
		emb = &db.Embeddings{Model: model, Dims: e.Dimensions}
	}
	store, err := db.Open(ctx, cfg.Database.URL, int32(extraConns+cfg.Queue.Workers+8), emb)
	if err != nil {
		return nil, err
	}
	if _, err := store.Migrate(ctx, "up"); err != nil {
		store.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	mgr := mcp.NewManager()
	inst := &instance{store: store, clients: mcp.StartAll(ctx, mgr, cfg.MCP)}
	if inst.rt, err = agent.NewRuntime(cfg, store, mgr); err != nil {
		inst.close()
		return nil, err
	}
	if cfg.Filesystem != nil {
		bucket, err := files.Open(ctx, cfg.Filesystem.DSN)
		if err != nil {
			inst.close()
			return nil, fmt.Errorf("filesystem: %w", err)
		}
		inst.bucket = bucket
		inst.rt.EnableFilesystem(bucket)
		if !files.Direct(files.Scheme(cfg.Filesystem.DSN)) && cfg.Filesystem.PublicURL == "" {
			slog.Warn("filesystem.public_url is unset: providers cannot fetch stored images (they arrive as placeholders)")
		}
	}
	if err := inst.rt.Boot(ctx); err != nil {
		inst.close()
		return nil, fmt.Errorf("boot: %w", err)
	}
	return inst, nil
}

// serve runs the HTTP API and SSE; withWorkers also runs the workers.
func serve(ctx context.Context, cfg *config.Config, withWorkers bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	inst, err := boot(ctx, cfg, 16)
	if err != nil {
		return err
	}
	defer inst.close()

	stopOps := serveOps(ctx, cfg, inst)
	defer stopOps()
	hub := httpapi.NewHub(inst.store)
	api := httpapi.New(cfg, inst.store, inst.rt.Svc, hub)
	if inst.bucket != nil {
		api.EnableFilesystem(inst.bucket)
	}
	srv := &http.Server{
		Addr: cfg.Server.Listen, Handler: api.Handler(),
		// Bound how long a request may take to arrive (no trickled bodies) and
		// how long an idle connection stays open; SSE streams outlive both.
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute,
		BaseContext: func(net.Listener) context.Context { return ctx }, // shutdown ends SSE streams
	}
	var wg sync.WaitGroup
	var w *jobs.Worker
	if withWorkers {
		w = newWorker(cfg, inst, nil)
		wg.Go(func() { w.Run(ctx, cfg.Queue.Workers) })
	}
	wg.Go(func() { hub.Run(ctx) })
	wg.Go(func() { listen(ctx, inst, hub, w) })
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("hfy listening", "addr", cfg.Server.Listen, "workers", withWorkers)

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

// work runs only the workers. queues is priority-ordered; nil = all queues.
func work(ctx context.Context, cfg *config.Config, queues []string, workers int) error {
	inst, err := boot(ctx, cfg, 0)
	if err != nil {
		return err
	}
	defer inst.close()
	if workers <= 0 {
		workers = cfg.Queue.Workers
	}
	stopOps := serveOps(ctx, cfg, inst)
	defer stopOps()
	w := newWorker(cfg, inst, queues)
	var wg sync.WaitGroup
	wg.Go(func() { listen(ctx, inst, nil, w) })
	slog.Info("hfy workers started", "workers", workers, "queues", queues)
	w.Run(ctx, workers)
	wg.Wait()
	return nil
}

// serveOps starts the operational listener (metrics.listen) if configured and
// returns its stop function. A failure to listen is logged, not fatal: the
// harness keeps serving without metrics.
func serveOps(ctx context.Context, cfg *config.Config, inst *instance) (stop func()) {
	metrics.SetQueueDepth(inst.store.JobCounts)
	if cfg.Metrics == nil {
		return func() {}
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics.Handler())
	mux.HandleFunc("GET /healthz", httpapi.Healthz)
	mux.Handle("GET /readyz", httpapi.Readyz(inst.store))
	srv := &http.Server{Addr: cfg.Metrics.Listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("metrics listener", "addr", cfg.Metrics.Listen, "err", err)
		}
	}()
	return func() {
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			slog.Warn("metrics listener shutdown", "err", err)
		}
	}
}

// listen holds the process's one LISTEN connection until ctx ends: events go
// to the hub, jobs wake the workers and stop requests reach the turn running
// here. A nil hub or w is a role the process does not have.
func listen(ctx context.Context, inst *instance, hub *httpapi.Hub, w *jobs.Worker) {
	var channels []string
	var onReconnect func()
	if hub != nil {
		channels = append(channels, db.ChanEvents)
	}
	if w != nil {
		channels = append(channels, db.ChanJobs, db.ChanCancel)
		onReconnect = w.Wake
	}
	inst.store.Listen(ctx, func(channel, payload string) {
		switch channel {
		case db.ChanEvents:
			hub.Notify(ctx, payload)
		case db.ChanJobs:
			w.Wake()
		case db.ChanCancel:
			inst.rt.CancelTurn(payload)
		}
	}, onReconnect, channels...)
}

func newWorker(cfg *config.Config, inst *instance, queues []string) *jobs.Worker {
	if len(queues) == 0 {
		queues = []string{types.QueueTurns, types.QueueEmbeddings, types.QueueMaintenance}
	}
	w := jobs.NewWorker(inst.store, cfg.Queue.Lease.D(), queues)
	inst.rt.RegisterJobs(w)
	return w
}

// migrate runs "up", "down" or "status" and returns the report.
func migrate(ctx context.Context, cfg *config.Config, cmd string) (string, error) {
	store, err := db.Open(ctx, cfg.Database.URL, 4, nil)
	if err != nil {
		return "", err
	}
	defer store.Close()
	return store.Migrate(ctx, cmd)
}
