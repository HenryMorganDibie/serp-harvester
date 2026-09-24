package metrics

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// PrometheusCollector exposes Counters (and, if set, a LatencyRecorder) as
// Prometheus metrics without the hot path (IncSuccess/IncFailure/...)
// knowing Prometheus exists — it just reads the same atomic counters on
// every scrape. This is the piece a client asking about sustained 10M+/day
// volume will actually want: a /metrics endpoint their existing
// Grafana/Prometheus stack can scrape, not just println throughput lines.
type PrometheusCollector struct {
	counters  *Counters
	latencies *LatencyRecorder // optional; nil means no latency metrics exposed

	success            *prometheus.Desc
	failure            *prometheus.Desc
	dropped            *prometheus.Desc
	retried            *prometheus.Desc
	aiOverviewPresent  *prometheus.Desc
	calibrationFlagged *prometheus.Desc
	proxyBanned        *prometheus.Desc
	latencyP50         *prometheus.Desc
	latencyP95         *prometheus.Desc
	latencyP99         *prometheus.Desc
}

// NewPrometheusCollector wraps c for Prometheus scraping. latencies may be
// nil if latency percentiles aren't being tracked for this run.
func NewPrometheusCollector(c *Counters, latencies *LatencyRecorder) *PrometheusCollector {
	return &PrometheusCollector{
		counters:           c,
		latencies:          latencies,
		success:            prometheus.NewDesc("serp_harvester_success_total", "Total successful fetch+parse operations.", nil, nil),
		failure:            prometheus.NewDesc("serp_harvester_failure_total", "Total failed fetch or parse attempts.", nil, nil),
		dropped:            prometheus.NewDesc("serp_harvester_dropped_total", "Total jobs dropped after exhausting retries.", nil, nil),
		retried:            prometheus.NewDesc("serp_harvester_retried_total", "Total retry attempts.", nil, nil),
		aiOverviewPresent:  prometheus.NewDesc("serp_harvester_ai_overview_total", "Total successful results whose SerpResult included an AI Overview.", nil, nil),
		calibrationFlagged: prometheus.NewDesc("serp_harvester_parser_drift_total", "Total successful parses flagged for missing an expected block (selector drift signal).", nil, nil),
		proxyBanned:        prometheus.NewDesc("serp_harvester_proxy_banned_total", "Total times a proxy transitioned into cooldown after repeated failures.", nil, nil),
		latencyP50:         prometheus.NewDesc("serp_harvester_latency_p50_ms", "Approximate p50 fetch latency in milliseconds over the current sample window.", nil, nil),
		latencyP95:         prometheus.NewDesc("serp_harvester_latency_p95_ms", "Approximate p95 fetch latency in milliseconds over the current sample window.", nil, nil),
		latencyP99:         prometheus.NewDesc("serp_harvester_latency_p99_ms", "Approximate p99 fetch latency in milliseconds over the current sample window.", nil, nil),
	}
}

// Describe implements prometheus.Collector.
func (p *PrometheusCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- p.success
	ch <- p.failure
	ch <- p.dropped
	ch <- p.retried
	ch <- p.aiOverviewPresent
	ch <- p.calibrationFlagged
	ch <- p.proxyBanned
	if p.latencies != nil {
		ch <- p.latencyP50
		ch <- p.latencyP95
		ch <- p.latencyP99
	}
}

// Collect implements prometheus.Collector.
func (p *PrometheusCollector) Collect(ch chan<- prometheus.Metric) {
	s := p.counters.Snapshot()
	ch <- prometheus.MustNewConstMetric(p.success, prometheus.CounterValue, float64(s.Success))
	ch <- prometheus.MustNewConstMetric(p.failure, prometheus.CounterValue, float64(s.Failure))
	ch <- prometheus.MustNewConstMetric(p.dropped, prometheus.CounterValue, float64(s.Dropped))
	ch <- prometheus.MustNewConstMetric(p.retried, prometheus.CounterValue, float64(s.Retried))
	ch <- prometheus.MustNewConstMetric(p.aiOverviewPresent, prometheus.CounterValue, float64(s.AIOverviewPresent))
	ch <- prometheus.MustNewConstMetric(p.calibrationFlagged, prometheus.CounterValue, float64(s.CalibrationFlagged))
	ch <- prometheus.MustNewConstMetric(p.proxyBanned, prometheus.CounterValue, float64(s.ProxyBanned))

	if p.latencies != nil {
		ls := p.latencies.Stats()
		ch <- prometheus.MustNewConstMetric(p.latencyP50, prometheus.GaugeValue, float64(ls.P50.Milliseconds()))
		ch <- prometheus.MustNewConstMetric(p.latencyP95, prometheus.GaugeValue, float64(ls.P95.Milliseconds()))
		ch <- prometheus.MustNewConstMetric(p.latencyP99, prometheus.GaugeValue, float64(ls.P99.Milliseconds()))
	}
}

// ServeHTTP builds an http.Handler serving /metrics and /healthz for c on a
// fresh registry (so it never collides with the default global registry).
// latencies may be nil.
func ServeHTTP(c *Counters, latencies *LatencyRecorder) http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewPrometheusCollector(c, latencies))
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	return mux
}

// StartServer serves /metrics and /healthz on addr until ctx is done,
// shutting down gracefully. It returns once the server has stopped.
func StartServer(ctx context.Context, addr string, c *Counters, latencies *LatencyRecorder) error {
	srv := &http.Server{Addr: addr, Handler: ServeHTTP(c, latencies)}

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("metrics: serve %s: %w", addr, err)
		}
		return nil
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
