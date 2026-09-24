// Package config loads the harvester's run configuration from YAML.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the full run configuration.
type Config struct {
	// Mode selects the fetch path:
	//   "mock"     - offline, deterministic, fixture-backed. Default.
	//   "live"     - real HTTP requests directly to LiveEndpoint.
	//   "provider" - real HTTP requests to a third-party SERP data API
	//                (ProviderBaseURL), parsed as JSON instead of HTML.
	Mode string `yaml:"mode"`

	Queries []string `yaml:"queries"`

	// QueueBackend selects the job source: "memory" (default) or "redis"
	// (a Redis Stream, shared across any number of harvester processes).
	QueueBackend   string `yaml:"queue_backend"`
	RedisAddr      string `yaml:"redis_addr"`
	RedisStream    string `yaml:"redis_stream"`
	RedisGroup     string `yaml:"redis_group"`
	RedisConsumer  string `yaml:"redis_consumer"`
	RedisSeedQueue bool   `yaml:"redis_seed_queue"` // push Queries onto the stream before consuming, for local demos

	Concurrency int `yaml:"concurrency"`
	MaxRetries  int `yaml:"max_retries"`

	// RatePerProxyRPS is the max sustained requests/second per proxy
	// (or per direct egress IP, if Proxies is empty).
	RatePerProxyRPS float64 `yaml:"rate_per_proxy_rps"`
	RateBurst       int     `yaml:"rate_burst"`

	Proxies          []string      `yaml:"proxies"`
	ProxyBanFails    int           `yaml:"proxy_ban_fails"`
	ProxyBanCooldown time.Duration `yaml:"proxy_ban_cooldown"`
	// ProxyStrategy selects proxy.Strategy: "round_robin" (default),
	// "random", or "weighted_success_rate".
	ProxyStrategy string `yaml:"proxy_strategy"`

	LiveEndpoint   string        `yaml:"live_endpoint"`
	RequestTimeout time.Duration `yaml:"request_timeout"`
	MockFixtureDir string        `yaml:"mock_fixture_dir"`

	// Third-party SERP provider config, used when Mode is "provider". The
	// API key is read from the environment variable named by
	// ProviderAPIKeyEnv, never from this file, so it's never committed.
	ProviderBaseURL   string `yaml:"provider_base_url"`
	ProviderAPIKeyEnv string `yaml:"provider_api_key_env"`
	ProviderEngine    string `yaml:"provider_engine"`

	// SinkBackend selects the result sink: "jsonl" (default, writes to
	// OutputPath) or "postgres" (structured storage, see PostgresDSNEnv).
	SinkBackend string `yaml:"sink_backend"`
	// PostgresDSNEnv names the environment variable holding the Postgres
	// connection string, read the same way ProviderAPIKeyEnv is — never
	// written to this file.
	PostgresDSNEnv string `yaml:"postgres_dsn_env"`

	OutputPath     string        `yaml:"output_path"`
	ReportInterval time.Duration `yaml:"report_interval"`

	// MetricsAddr, if set (e.g. ":9090"), serves Prometheus metrics at
	// /metrics for the duration of the run.
	MetricsAddr string `yaml:"metrics_addr"`
}

// Default returns sane defaults; Load overlays whatever the YAML file sets
// on top of these.
func Default() Config {
	return Config{
		Mode:             "mock",
		QueueBackend:     "memory",
		RedisAddr:        "localhost:6379",
		RedisStream:      "serp-harvester:queries",
		RedisGroup:       "harvesters",
		RedisConsumer:    "worker-1",
		Concurrency:      8,
		MaxRetries:       2,
		RatePerProxyRPS:  1,
		RateBurst:        1,
		ProxyBanFails:    3,
		ProxyBanCooldown: 30 * time.Second,
		ProxyStrategy:    "round_robin",
		LiveEndpoint:     "https://www.google.com/search",
		RequestTimeout:   10 * time.Second,
		MockFixtureDir:   "internal/parser/testdata",
		ProviderEngine:   "google",
		SinkBackend:      "jsonl",
		PostgresDSNEnv:   "POSTGRES_DSN",
		OutputPath:       "-", // stdout
		ReportInterval:   2 * time.Second,
	}
}

// Load reads and parses the YAML config at path, applied on top of Default().
func Load(path string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("config: read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("config: parse %s: %w", path, err)
	}
	return cfg, nil
}
