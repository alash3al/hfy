// Package metrics is hfy's Prometheus instrumentation. The metrics are
// package-level, as is idiomatic for Prometheus, on hfy's own registry rather
// than the global one, so embedding hfy never collides with the host's metrics.
//
// Labels are bounded: routes are patterns, tools are labelled by server, and
// outcomes are fixed words.
package metrics

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var Registry = prometheus.NewRegistry()

var (
	HTTPRequests = counter("hfy_http_requests_total",
		"HTTP requests by route pattern, method and status code.", "route", "method", "code")
	HTTPDuration = histogram("hfy_http_request_duration_seconds",
		"HTTP request latency by route pattern and method (event streams excluded).", "route", "method")
	Streams = gauge("hfy_event_streams", "Open SSE event streams on this instance.")

	Turns  = counter("hfy_turns_total", "Turns ended, by final state.", "state")
	Tokens = counter("hfy_tokens_total",
		"Model tokens by agent (main, a sub-agent, summarizer) and kind (input, output, cached).", "agent", "kind")

	ProviderRequests = counter("hfy_provider_requests_total",
		"Provider HTTP attempts by provider, kind (chat, embedding) and outcome (ok, error).", "provider", "kind", "outcome")
	ProviderDuration = histogram("hfy_provider_request_duration_seconds",
		"Provider HTTP attempt latency by provider and kind.", "provider", "kind")

	ToolCalls = counter("hfy_tool_calls_total",
		"Tool calls by MCP server and outcome (ok, error).", "server", "outcome")
	ToolDuration = histogram("hfy_tool_call_duration_seconds", "Tool call latency by MCP server.", "server")

	Jobs = counter("hfy_jobs_total",
		"Jobs by type and outcome (done, retry, failed, lost, released, expired).", "type", "outcome")
	JobDuration = histogram("hfy_job_duration_seconds", "Job handler run time by type.", "type")
)

func init() {
	Registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		depthCollector{})
}

func counter(name, help string, labels ...string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels)
	Registry.MustRegister(c)
	return c
}

func histogram(name, help string, labels ...string) *prometheus.HistogramVec {
	// Model and tool calls run from milliseconds to minutes.
	h := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: name, Help: help,
		Buckets: []float64{.005, .025, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300}}, labels)
	Registry.MustRegister(h)
	return h
}

func gauge(name, help string) prometheus.Gauge {
	g := prometheus.NewGauge(prometheus.GaugeOpts{Name: name, Help: help})
	Registry.MustRegister(g)
	return g
}

// Since observes the seconds elapsed from start.
func Since(h prometheus.Observer, start time.Time) { h.Observe(time.Since(start).Seconds()) }

// Outcome is "error" when failed, else "ok".
func Outcome(failed bool) string {
	if failed {
		return "error"
	}
	return "ok"
}

// QueueDepth reports jobs by queue and state; it is read at scrape time.
type QueueDepth func(ctx context.Context) (map[[2]string]int64, error)

var depth atomic.Pointer[QueueDepth]

// SetQueueDepth sets where hfy_jobs{queue,state} is read from (queued and
// running jobs across all instances: the queue is the database).
func SetQueueDepth(fn QueueDepth) { depth.Store(&fn) }

var depthDesc = prometheus.NewDesc("hfy_jobs",
	"Jobs waiting or running, by queue and state, across all instances.", []string{"queue", "state"}, nil)

type depthCollector struct{}

func (depthCollector) Describe(ch chan<- *prometheus.Desc) { ch <- depthDesc }

func (depthCollector) Collect(ch chan<- prometheus.Metric) {
	fn := depth.Load()
	if fn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	counts, err := (*fn)(ctx)
	if err != nil {
		slog.Warn("metrics: queue depth", "err", err)
		ch <- prometheus.NewInvalidMetric(depthDesc, err)
		return
	}
	for k, n := range counts {
		ch <- prometheus.MustNewConstMetric(depthDesc, prometheus.GaugeValue, float64(n), k[0], k[1])
	}
}

// Handler serves the registry in the Prometheus text format.
func Handler() http.Handler {
	return promhttp.HandlerFor(Registry, promhttp.HandlerOpts{Registry: Registry})
}
