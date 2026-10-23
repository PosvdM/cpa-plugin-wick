// Package config parses the plugins.configs.wick subtree of the CPA config.
package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// PluginID is the CPA plugin ID and the key under plugins.configs.
const PluginID = "wick"

// The prompt cache TTL is 300 seconds, counted from when Anthropic receives
// the request, so every interval must stay below it.
const maxIntervalSeconds = 299

const maxRetentionDays = 3650

// Config holds the keep-alive settings.
type Config struct {
	// IntervalMin and IntervalMax bound the random delay, in seconds, between
	// the start of the last request to a conversation and the next ping.
	IntervalMin int `yaml:"interval_min" json:"interval_min"`
	IntervalMax int `yaml:"interval_max" json:"interval_max"`
	// MaxPings is the number of pings sent during one idle period.
	MaxPings int `yaml:"max_pings" json:"max_pings"`
	// MinBodyKB skips requests whose body is smaller than this many KiB.
	MinBodyKB int `yaml:"min_body_kb" json:"min_body_kb"`
	// HistoryRetentionDays is how many days of ping history are kept on disk.
	HistoryRetentionDays int `yaml:"history_retention_days" json:"history_retention_days"`
	// DataDir overrides the directory of the ping history. Empty means
	// <plugins.dir>/data/wick.
	DataDir string `yaml:"data_dir" json:"data_dir"`
}

// Default returns the default settings.
func Default() Config {
	return Config{IntervalMin: 260, IntervalMax: 290, MaxPings: 6, MinBodyKB: 64, HistoryRetentionDays: 40}
}

// Parse decodes the plugin config subtree. Missing keys keep their defaults.
// On an invalid value it returns the defaults together with the error.
func Parse(raw []byte) (Config, error) {
	cfg := Default()
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return Default(), fmt.Errorf("parse wick config: %w", err)
		}
	}
	if err := cfg.validate(); err != nil {
		return Default(), err
	}
	cfg.DataDir = strings.TrimSpace(cfg.DataDir)
	return cfg, nil
}

func (c Config) validate() error {
	if c.IntervalMin < 1 || c.IntervalMax < c.IntervalMin || c.IntervalMax > maxIntervalSeconds {
		return fmt.Errorf("interval_min and interval_max must satisfy 1 <= interval_min <= interval_max <= %d", maxIntervalSeconds)
	}
	if c.MaxPings < 0 {
		return fmt.Errorf("max_pings must not be negative")
	}
	if c.MinBodyKB < 0 {
		return fmt.Errorf("min_body_kb must not be negative")
	}
	if c.HistoryRetentionDays < 1 || c.HistoryRetentionDays > maxRetentionDays {
		return fmt.Errorf("history_retention_days must be between 1 and %d", maxRetentionDays)
	}
	return nil
}
