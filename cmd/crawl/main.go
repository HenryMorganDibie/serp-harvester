// Command crawl is the general-purpose web crawler: it fetches and
// extracts structured data from any website, using the same acquisition
// stack as the SERP harvester (proxy pool, adaptive per-host rate limiting,
// block classification and cooldowns, plain HTTP or headless Chromium).
//
// Each target in the config names start URLs, how far to follow links,
// how to render pages and what to extract. robots.txt is obeyed by default.
// Like live SERP modes, it sends real requests, so it refuses to run
// without -i-have-reviewed-tos. See README.md, "Crawling any website".
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/HenryMorganDibie/serp-harvester/internal/config"
	"github.com/HenryMorganDibie/serp-harvester/internal/crawl"
	"github.com/HenryMorganDibie/serp-harvester/internal/fetcher"
	"github.com/HenryMorganDibie/serp-harvester/internal/metrics"
	"github.com/HenryMorganDibie/serp-harvester/internal/proxy"
	"github.com/HenryMorganDibie/serp-harvester/internal/ratelimit"
	"github.com/HenryMorganDibie/serp-harvester/internal/robots"
	"github.com/HenryMorganDibie/serp-harvester/internal/store"
	"github.com/HenryMorganDibie/serp-harvester/internal/worker"
)

// crawlConfig is the crawler's config file: the harvester's settings for
// concurrency, retries, proxies, rate limiting, the browser, sinks and
// metrics (see internal/config), plus the crawler's own.
type crawlConfig struct {
	config.Config `yaml:",inline"`

	// UserAgent is sent on every request. Identify the crawler honestly
	// and give site owners a way to reach you.
	UserAgent string `yaml:"user_agent"`
	// RobotsAgent is the product token matched against robots.txt
	// User-agent lines.
	RobotsAgent string `yaml:"robots_agent"`
	// RatePerHostRPS is the request rate to any one host, across all
	// proxies. Adaptive throttling (adaptive_rate) lowers it for a host
	// that rate-limits or blocks.
	RatePerHostRPS float64 `yaml:"rate_per_host_rps"`

	Targets []crawl.Target `yaml:"targets"`
}

func defaults() crawlConfig {
	c := crawlConfig{
		Config:         config.Default(),
		UserAgent:      "Mozilla/5.0 (compatible; serp-harvester/0.1; +https://github.com/HenryMorganDibie/serp-harvester)",
		RobotsAgent:    "serp-harvester",
		RatePerHostRPS: 1,
	}
	c.Concurrency = 4
	c.RequestTimeout = 20 * time.Second
	return c
}

func load(path string) (crawlConfig, error) {
	cfg := defaults()
	if path == "" {
		return cfg, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

func main() {
	configPath := flag.String("config", "", "path to a crawl config (see configs/crawl.example.yaml)")
	urls := flag.String("url", "", "comma-separated start URLs for an ad hoc target (instead of the config's targets)")
	depth := flag.Int("depth", 0, "ad hoc target: link depth to follow (0 = only the given URLs)")
	maxPages := flag.Int("max-pages", crawl.DefaultMaxPages, "ad hoc target: page budget")
	render := flag.String("render", "http", "ad hoc target: http|browser|auto")
	only := flag.String("target", "", "run only the config target with this name")
	runID := flag.String("run-id", "", "run ID stamped on every page (default: generated)")
	reviewed := flag.Bool("i-have-reviewed-tos", false, "required: confirms you have reviewed each target's terms and robots.txt")
	flag.Parse()

	cfg, err := load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	if *urls != "" {
		cfg.Targets = []crawl.Target{{
			Name:      "adhoc",
			StartURLs: strings.Split(*urls, ","),
			MaxDepth:  *depth,
			MaxPages:  *maxPages,
			Render:    *render,
		}}
	}
	if *only != "" {
		var kept []crawl.Target
		for _, t := range cfg.Targets {
			if t.Name == *only {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			log.Fatalf("no target named %q in %s", *only, *configPath)
		}
		cfg.Targets = kept
	}
	if len(cfg.Targets) == 0 {
		log.Fatal("no targets: set targets: in -config, or pass -url")
	}
	if !*reviewed {
		fmt.Fprintln(os.Stderr, reviewWarning)
		os.Exit(1)
	}
	if *runID == "" {
		*runID = newRunID()
	}

	needsBrowser := false
	for _, t := range cfg.Targets {
		if t.Render == "browser" || t.Render == "auto" {
			needsBrowser = true
		}
	}
	counters := &metrics.Counters{}
	if needsBrowser {
		counters.Browser = &metrics.BrowserCounters{}
	}
	latencies := metrics.NewLatencyRecorder(10000)

	httpFetcher, err := fetcher.NewHTTPFetcher("", cfg.RequestTimeout)
	if err != nil {
		log.Fatalf("build http fetcher: %v", err)
	}
	httpFetcher.Generic = true
	// render: auto targets use a second HTTP fetcher that reports app
	// shells, so the FailoverFetcher renders them in Chromium.
	shellAware, err := fetcher.NewHTTPFetcher("", cfg.RequestTimeout)
	if err != nil {
		log.Fatalf("build http fetcher: %v", err)
	}
	shellAware.Generic, shellAware.DetectAppShells = true, true
	var browser *fetcher.PlaywrightFetcher
	if needsBrowser {
		browser, err = fetcher.NewPlaywrightFetcher(fetcher.PlaywrightConfig{
			Timeout:        cfg.RequestTimeout,
			PoolSize:       cfg.BrowserPoolSize,
			MaxSessionUses: cfg.BrowserMaxSessionUses,
			Headless:       !cfg.BrowserHeadful,
			ExecutablePath: cfg.BrowserExecutablePath,
			Generic:        true,
			Metrics:        counters.Browser,
		})
		if err != nil {
			log.Fatalf("build playwright fetcher: %v (install the driver and Chromium with `make playwright-install` / scripts/install-playwright.sh)", err)
		}
		defer browser.Close()
	}

	sink, closeSink := buildSink(cfg)
	defer closeSink()

	// One limiter for every target, keyed by host: two targets on the same
	// site share its rate.
	limiter := ratelimit.New(cfg.RatePerHostRPS, cfg.RateBurst)
	if cfg.AdaptiveRate {
		limiter = ratelimit.NewAdaptive(cfg.RatePerHostRPS, cfg.RateBurst, cfg.RateMinRPS)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Each target gets its own proxy pool, so a block on one site cools
	// an egress IP for that site only.
	var mu sync.Mutex
	var proxyPools []*proxy.Pool
	counters.ProxyGauges = func() metrics.ProxyGauges {
		mu.Lock()
		defer mu.Unlock()
		var g metrics.ProxyGauges
		for _, pp := range proxyPools {
			a, c := pp.Counts()
			g.Available += a
			g.Cooling += c
		}
		g.Throttled = limiter.Throttled()
		return g
	}

	if cfg.MetricsAddr != "" {
		go func() {
			log.Printf("serving Prometheus metrics on %s/metrics (health at /healthz)", cfg.MetricsAddr)
			if err := metrics.StartServer(ctx, cfg.MetricsAddr, counters, latencies); err != nil {
				log.Printf("metrics server stopped: %v", err)
			}
		}()
	}
	reportCtx, cancelReport := context.WithCancel(ctx)
	reportDone := counters.StartReporter(reportCtx, cfg.ReportInterval)

	log.Printf("crawl %s: %d target(s), %.2f req/s per host, concurrency %d per target, proxies=%d",
		*runID, len(cfg.Targets), cfg.RatePerHostRPS, cfg.Concurrency, len(cfg.Proxies))

	var wg sync.WaitGroup
	for _, t := range cfg.Targets {
		pp := proxy.NewPool(cfg.Proxies, cfg.ProxyBanFails, cfg.ProxyBanCooldown)
		pp.Strategy = parseProxyStrategy(cfg.ProxyStrategy)
		pp.MaxCooldown = cfg.ProxyBanCooldownMax
		mu.Lock()
		proxyPools = append(proxyPools, pp)
		mu.Unlock()

		var f fetcher.Fetcher = httpFetcher
		switch t.Render {
		case "browser":
			f = browser
		case "auto":
			f = &fetcher.FailoverFetcher{
				Primary:    shellAware,
				Secondary:  browser,
				OnFailover: func(fetcher.Outcome) { counters.IncFailover() },
			}
		}
		pool := &worker.Pool{
			Concurrency:  cfg.Concurrency,
			MaxRetries:   cfg.MaxRetries,
			Fetcher:      f,
			ProxyPool:    pp,
			Limiter:      limiter,
			Metrics:      counters,
			Latencies:    latencies,
			MaxProxyWait: cfg.MaxProxyWait,
			UserAgent:    cfg.UserAgent,
		}
		robotsPool := *pool
		robotsPool.Fetcher = httpFetcher // robots.txt is never rendered
		c := &crawl.Crawler{
			Pool:   pool,
			Robots: &robots.Cache{Agent: cfg.RobotsAgent, Get: crawl.RobotsGetter(&robotsPool)},
			Sink:   sink,
			RunID:  *runID,
		}

		wg.Add(1)
		go func(t crawl.Target) {
			defer wg.Done()
			if t.RespectRobots != nil && !*t.RespectRobots {
				log.Printf("target %s: respect_robots is false: robots.txt will NOT be consulted", t.Name)
			}
			start := time.Now()
			stats, err := c.Run(ctx, t)
			if err != nil && ctx.Err() == nil {
				log.Printf("target %s: %v", t.Name, err)
				return
			}
			log.Printf("target %s done in %s: fetched=%d failed=%d robots_disallowed=%d flagged=%d discovered=%d budget_exhausted=%v",
				t.Name, time.Since(start).Round(time.Millisecond), stats.Fetched, stats.Failed, stats.RobotsDisallow,
				stats.Flagged, stats.Discovered, stats.BudgetExhausted)
		}(t)
	}
	wg.Wait()

	cancelReport()
	<-reportDone
	log.Printf("crawl %s complete", *runID)
}

// buildSink returns the page sink and a function that closes it.
func buildSink(cfg crawlConfig) (store.PageSink, func()) {
	switch cfg.SinkBackend {
	case "", "jsonl":
		if cfg.OutputPath == "-" || cfg.OutputPath == "" {
			return store.NewJSONLSink(os.Stdout), func() {}
		}
		out, err := os.Create(cfg.OutputPath)
		if err != nil {
			log.Fatalf("create output file: %v", err)
		}
		return store.NewJSONLSink(out), func() { out.Close() }
	case "postgres":
		dsn := os.Getenv(cfg.PostgresDSNEnv)
		if dsn == "" {
			log.Fatalf("sink_backend: postgres requires the %s environment variable to be set", cfg.PostgresDSNEnv)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		sink, err := store.NewPostgresSink(ctx, dsn)
		if err != nil {
			log.Fatalf("build postgres sink: %v", err)
		}
		return sink, func() { sink.Close() }
	default:
		log.Fatalf("unknown sink_backend %q (want jsonl|postgres)", cfg.SinkBackend)
		return nil, nil
	}
}

func parseProxyStrategy(s string) proxy.Strategy {
	switch s {
	case "", "round_robin":
		return proxy.RoundRobin
	case "random":
		return proxy.Random
	case "weighted_success_rate":
		return proxy.WeightedSuccessRate
	default:
		log.Fatalf("unknown proxy_strategy %q (want round_robin|random|weighted_success_rate)", s)
		return proxy.RoundRobin
	}
}

func newRunID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "crawl-" + hex.EncodeToString(b)
}

const reviewWarning = `refusing to crawl without -i-have-reviewed-tos

The crawler sends real requests to every target. Before running it you
should have reviewed, for each site:
  - Its Terms of Service (many prohibit automated access or reuse of data)
  - Its robots.txt (obeyed by default; respect_robots: false is for sites
    you own or have permission to crawl)
  - Applicable law in your and the site's jurisdiction, including data
    protection law if pages contain personal data
  - That your rate_per_host_rps and page budgets are ones the site can
    comfortably absorb

Re-run with -i-have-reviewed-tos once you have done so.`
