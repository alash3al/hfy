package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/db/dbtest"
)

// A failed listen must stop the process, not leave it hanging on its workers.
func TestServeReturnsWhenTheAddressIsTaken(t *testing.T) {
	store := dbtest.New(t, nil)
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	cfg := &config.Config{
		Server:    config.Server{Listen: taken.Addr().String(), Secret: "s"},
		Database:  config.Database{URL: store.Pool.Config().ConnString()},
		Queue:     config.Queue{Workers: 1, Lease: config.Duration(5 * time.Second), MaxAttempts: 1},
		Retention: config.Retention{Trace: config.Duration(time.Hour)},
		Providers: map[string]config.Provider{"p": {URL: "http://127.0.0.1:1/v1"}},
		Agent:     config.Agent{Model: "p/m", Settings: config.Settings{ContextWindow: 100}},
	}
	done := make(chan error, 1)
	go func() { done <- serve(context.Background(), cfg, true) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want the listen error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve hung after the listener failed")
	}
}

func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func get(t *testing.T, url string) (int, string) {
	resp, err := http.Get(url)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// The API lives under /v1; /healthz and /readyz answer on both listeners;
// /metrics only on the operational one.
func TestOperationalEndpoints(t *testing.T) {
	store := dbtest.New(t, nil)
	api, ops := freeAddr(t), freeAddr(t)
	cfg := &config.Config{
		Server:    config.Server{Listen: api, Secret: "s"},
		Database:  config.Database{URL: store.Pool.Config().ConnString()},
		Queue:     config.Queue{Workers: 1, Lease: config.Duration(5 * time.Second), MaxAttempts: 1},
		Retention: config.Retention{Trace: config.Duration(time.Hour)},
		Providers: map[string]config.Provider{"p": {URL: "http://127.0.0.1:1/v1"}},
		Agent:     config.Agent{Model: "p/m", Settings: config.Settings{ContextWindow: 100}},
		Metrics:   &config.Metrics{Listen: ops},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, true) }()
	defer func() { cancel(); <-done }()
	for range 100 {
		if code, _ := get(t, "http://"+api+"/healthz"); code == 200 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	for _, base := range []string{api, ops} {
		if code, body := get(t, "http://"+base+"/readyz"); code != 200 {
			t.Fatalf("readyz on %s: %d %s", base, code, body)
		}
	}
	if code, _ := get(t, "http://"+api+"/v1/conversation/history"); code != 401 {
		t.Fatalf("v1 route without a token: %d", code)
	}
	if code, _ := get(t, "http://"+api+"/conversation/history"); code != 404 {
		t.Fatalf("unversioned route must not exist: %d", code)
	}
	if code, _ := get(t, "http://"+api+"/metrics"); code != 404 {
		t.Fatal("metrics must not be on the public API port")
	}
	code, body := get(t, "http://"+ops+"/metrics")
	if code != 200 {
		t.Fatalf("metrics: %d", code)
	}
	for _, want := range []string{
		"go_goroutines",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics lack %q\n%s", want, body)
		}
	}
	// The 401 counter is positive and only grows (the registry is process-
	// wide, so -count=30 sees it above 1): the series just has to exist.
	if !gauge(body, `hfy_http_requests_total{code="401",method="GET",route="/v1/conversation/history"}`) {
		t.Fatalf("metrics lack the 401 counter\n%s", body)
	}
	// The retention job is enqueued at boot, but the single worker may have
	// claimed it by now (state=running, or done and re-scheduled) while the
	// scrape happened, so the queued gauge is not always 1: poll until the job
	// shows up as queued or running.
	for range 100 { // ~5s in 50ms steps
		if gauge(body, `hfy_jobs{queue="maintenance",state="queued"}`) ||
			gauge(body, `hfy_jobs{queue="maintenance",state="running"}`) {
			return
		}
		time.Sleep(50 * time.Millisecond)
		code, b := get(t, "http://"+ops+"/metrics")
		if code == 200 {
			body = b
		}
	}
	t.Fatalf("metrics never show the retention job queued or running\n%s", body)
}

// gauge reports whether the metric (name with its full labels) is present
// with a positive value. Works for counters and gauges alike.
func gauge(body, metric string) bool {
	for line := range strings.SplitSeq(body, "\n") {
		if !strings.HasPrefix(line, metric+" ") {
			continue
		}
		v, err := strconv.ParseFloat(strings.Fields(line)[1], 64)
		return err == nil && v > 0
	}
	return false
}
