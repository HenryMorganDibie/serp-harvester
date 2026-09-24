// Command harvester runs the SERP harvesting pipeline end to end: load
// config, build the queue/proxy pool/rate limiter/fetcher/parser/sink, run
// the worker pool, and report throughput.
//
// Mock mode (default) is fully offline and deterministic — safe to run
// anywhere, including CI. Live mode issues real HTTP requests and should
// only be run deliberately, at low volume, by someone who has reviewed the
// target site's Terms of Service. See README.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/HenryMorganDibie/serp-harvester/internal/config"
	"github.com/HenryMorganDibie/serp-harvester/internal/fetcher"
	"github.com/HenryMorganDibie/serp-harvester/internal/metrics"
	"github.com/HenryMorganDibie/serp-harvester/internal/parser"
	"github.com/HenryMorganDibie/serp-harvester/internal/proxy"
	"github.com/HenryMorganDibie/serp-harvester/internal/queue"
	"github.com/HenryMorganDibie/serp-harvester/internal/ratelimit"
	"github.com/HenryMorganDibie/serp-harvester/internal/store"
	"github.com/HenryMorganDibie/serp-harvester/internal/worker"
)

func main() {
	configPath := flag.String("config", "configs/config.example.yaml", "path to YAML config")
	mode := flag.String("mode", "", "override config mode: mock|live")
	queriesFlag := flag.String("queries", "", "comma-separated queries, overrides config")
	iAcceptLiveRisk := flag.Bool("i-have-reviewed-tos", false, "required to run --mode live")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	if *mode != "" {
		cfg.Mode = *mode
	}
	if *queriesFlag != "" {
		cfg.Queries = strings.Split(*queriesFlag, ",")
	}
	if len(cfg.Queries) == 0 {
		log.Fatal("no queries configured (set queries: in config or pass -queries)")
	}

	var f fetcher.Fetcher
	switch cfg.Mode {
	case "mock":
		f, err = fetcher.NewMockFromDir(cfg.MockFixtureDir)
		if err != nil {
			log.Fatalf("build mock fetcher: %v", err)
		}
	case "live":
		if !*iAcceptLiveRisk {
			fmt.Fprintln(os.Stderr, liveModeWarning)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, liveModeBanner(cfg))
		f, err = fetcher.NewHTTPFetcher(cfg.LiveEndpoint, cfg.RequestTimeout)
		if err != nil {
			log.Fatalf("build http fetcher: %v", err)
		}
	default:
		log.Fatalf("unknown mode %q (want mock|live)", cfg.Mode)
	}

	var sink store.Sink
	if cfg.OutputPath == "-" || cfg.OutputPath == "" {
		sink = store.NewJSONLSink(os.Stdout)
	} else {
		out, err := os.Create(cfg.OutputPath)
		if err != nil {
			log.Fatalf("create output file: %v", err)
		}
		defer out.Close()
		sink = store.NewJSONLSink(out)
	}

	proxyPool := proxy.NewPool(cfg.Proxies, cfg.ProxyBanFails, cfg.ProxyBanCooldown)
	limiter := ratelimit.New(cfg.RatePerProxyRPS, cfg.RateBurst)
	counters := &metrics.Counters{}

	pool := &worker.Pool{
		Concurrency: cfg.Concurrency,
		MaxRetries:  cfg.MaxRetries,
		Fetcher:     f,
		ProxyPool:   proxyPool,
		Limiter:     limiter,
		Parser:      parser.New(),
		Sink:        sink,
		Metrics:     counters,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	reportCtx, cancelReport := context.WithCancel(ctx)
	reportDone := counters.StartReporter(reportCtx, cfg.ReportInterval)

	log.Printf(
		"starting harvest: mode=%s queries=%d concurrency=%d proxies=%d rate/proxy=%.2f req/s",
		cfg.Mode, len(cfg.Queries), cfg.Concurrency, proxyPool.Size(), cfg.RatePerProxyRPS,
	)

	jobs := queue.New(cfg.Queries, cfg.Concurrency*2)
	pool.Run(ctx, jobs)

	cancelReport()
	<-reportDone

	log.Println("harvest complete")
}

const liveModeWarning = `refusing to run --mode live without -i-have-reviewed-tos

Live mode sends real HTTP requests to the configured endpoint (default:
Google Search). Before running it you should have reviewed:
  - The target site's Terms of Service and robots.txt
  - Applicable law in your and the target's jurisdiction
  - Your own risk tolerance for IP blocks / CAPTCHAs at the configured rate

Re-run with -i-have-reviewed-tos once you have done so.`

func liveModeBanner(cfg config.Config) string {
	return fmt.Sprintf(
		"[live mode] endpoint=%s concurrency=%d rate/proxy=%.2f req/s — this WILL make real network requests",
		cfg.LiveEndpoint, cfg.Concurrency, cfg.RatePerProxyRPS,
	)
}
