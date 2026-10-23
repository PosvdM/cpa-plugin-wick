package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseEmptyUsesDefaults(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil || cfg != Default() {
		t.Fatalf("cfg %+v err %v", cfg, err)
	}
	if cfg.IntervalMin != 260 || cfg.IntervalMax != 290 || cfg.MaxPings != 6 || cfg.MinBodyKB != 64 || cfg.HistoryRetentionDays != 40 || cfg.DataDir != "" {
		t.Fatalf("defaults %+v", cfg)
	}
}

func TestParseKeepsMissingKeys(t *testing.T) {
	cfg, err := Parse([]byte("enabled: true\nmax_pings: 3\n"))
	if err != nil || cfg.MaxPings != 3 || cfg.IntervalMin != 260 {
		t.Fatalf("cfg %+v err %v", cfg, err)
	}
}

func TestParseRejectsInvalidIntervals(t *testing.T) {
	for _, raw := range []string{"interval_max: 300\n", "interval_min: 200\ninterval_max: 100\n", "interval_min: 0\n", "max_pings: -1\n", "history_retention_days: 0\n"} {
		cfg, err := Parse([]byte(raw))
		if err == nil || cfg != Default() {
			t.Errorf("%q: cfg %+v err %v", raw, cfg, err)
		}
	}
}

func TestPluginEnabled(t *testing.T) {
	on, err := PluginEnabled([]byte("plugins:\n  enabled: true\n  configs:\n    wick:\n      enabled: true\n"))
	if err != nil || !on {
		t.Fatalf("on %v err %v", on, err)
	}
	off, _ := PluginEnabled([]byte("plugins:\n  enabled: true\n  configs:\n    wick:\n      enabled: false\n"))
	if off {
		t.Fatal("disabled plugin reported as enabled")
	}
}

func TestHostConfigPath(t *testing.T) {
	if got := HostConfigPath([]string{"cpa", "-config", "/etc/cpa.yaml"}, "/w"); got != "/etc/cpa.yaml" {
		t.Fatalf("got %q", got)
	}
	if got := HostConfigPath([]string{"cpa", "--config=/x.yaml"}, "/w"); got != "/x.yaml" {
		t.Fatalf("got %q", got)
	}
}

func TestDataDir(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte("plugins:\n  dir: ./my-plugins\n"), 0o600)
	if got, want := DataDir("", cfgPath, dir), filepath.Join(dir, "my-plugins", "data", "wick"); got != want {
		t.Fatalf("default %q, want %q", got, want)
	}
	if got, want := DataDir("", filepath.Join(dir, "missing.yaml"), dir), filepath.Join(dir, "plugins", "data", "wick"); got != want {
		t.Fatalf("without config %q, want %q", got, want)
	}
	if got, want := DataDir("state", cfgPath, dir), filepath.Join(dir, "state"); got != want {
		t.Fatalf("relative data_dir %q, want %q", got, want)
	}
}
