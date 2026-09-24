package metrics

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// PrometheusCollector exposes Counters as Prometheus metrics without the hot
// path (IncSuccess/IncFailure/...) knowing Prometheus exists — it just reads
// the same atomic counters on every scrape. This is the piece a client
// asking about sustained 10M+/day volume will actually want: a /metrics
// endpoint their existing Grafana/Prometheus stack can scrape, not just
// println throughput lines.
type PrometheusCollector struct {
	counters *Counters

	success *prometheus.Desc
	failure *prometheus.Desc
	dropped *prometheus.Desc
	retried *prometheus.Desc
}

// NewPrometheusCollector wraps c for Prometheus scraping.
func NewPrometheusCollector(c *Counters) *PrometheusCollector {
	return &PrometheusCollector{
		counters: c,
		success:  prometheus.NewDesc("serp_harvester_success_total", "Total successful fetch+parse operations.", nil, nil),
		failure:  prometheus.NewDesc("serp_harvester_failure_total", "Total failed fetch or parse attempts.", nil, nil),
		dropped:  prometheus.NewDesc("serp_harvester_dropped_total", "Total jobs dropped after exhausting retries.", nil, nil),
		retried:  prometheus.NewDesc("serp_harvester_retried_total", "Total retry attempts.", nil, nil),
	}
}

// Describe implements prometheus.Collector.
func (p *PrometheusCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- p.success
	ch <- p.failure
	ch <- p.dropped
	ch <- p.retried
}

// Collect implements prometheus.Collector.
func (p *PrometheusCollector) Collect(ch chan<- prometheus.Metric) {
	s := p.counters.Snapshot()
	ch <- prometheus.MustNewConstMetric(p.success, prometheus.CounterValue, float64(s.Success))
	ch <- prometheus.MustNewConstMetric(p.failure, prometheus.CounterValue, float64(s.Failure))
	ch <- prometheus.MustNewConstMetric(p.dropped, prometheus.CounterValue, float64(s.Dropped))
	ch <- prometheus.MustNewConstMetric(p.retried, prometheus.CounterValue, float64(s.Retried))
}

// ServeHTTP builds an http.Handler serving /metrics for c on a fresh
// registry (so it never collides with the default global registry).
func ServeHTTP(c *Counters) http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewPrometheusCollector(c))
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	return mux
}

// StartServer serves /metrics on addr until ctx is done, shutting down
// gracefully. It returns once the server has stopped.
func StartServer(ctx context.Context, addr string, c *Counters) error {
	srv := &http.Server{Addr: addr, Handler: ServeHTTP(c)}

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
