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
	// Mode is "mock" (offline, deterministic, fixture-backed) or "live"
	// (real HTTP requests). Defaults to "mock" if unset.
	Mode string `yaml:"mode"`

	Queries []string `yaml:"queries"`

	Concurrency int `yaml:"concurrency"`
	MaxRetries  int `yaml:"max_retries"`

	// RatePerProxyRPS is the max sustained requests/second per proxy
	// (or per direct egress IP, if Proxies is empty).
	RatePerProxyRPS float64 `yaml:"rate_per_proxy_rps"`
	RateBurst       int     `yaml:"rate_burst"`

	Proxies          []string      `yaml:"proxies"`
	ProxyBanFails    int           `yaml:"proxy_ban_fails"`
	ProxyBanCooldown time.Duration `yaml:"proxy_ban_cooldown"`

	LiveEndpoint   string        `yaml:"live_endpoint"`
	RequestTimeout time.Duration `yaml:"request_timeout"`
	MockFixtureDir string        `yaml:"mock_fixture_dir"`
	OutputPath     string        `yaml:"output_path"`
	ReportInterval time.Duration `yaml:"report_interval"`
}

// Default returns sane defaults; Load overlays whatever the YAML file sets
// on top of these.
func Default() Config {
	return Config{
		Mode:             "mock",
		Concurrency:      8,
		MaxRetries:       2,
		RatePerProxyRPS:  1,
		RateBurst:        1,
		ProxyBanFails:    3,
		ProxyBanCooldown: 30 * time.Second,
		LiveEndpoint:     "https://www.google.com/search",
		RequestTimeout:   10 * time.Second,
		MockFixtureDir:   "internal/parser/testdata",
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
