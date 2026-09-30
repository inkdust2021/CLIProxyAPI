package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClaudeCacheKeepaliveConfig(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		want       bool
	}{
		{"default", "server:\n  port: 8317\n", false},
		{"legacy", "claude:\n  cache-keepalive: true\n", true},
		{"v8", "oauth:\n  providers:\n    claude:\n      cache-keepalive: true\n", true},
		{"disabled", "oauth:\n  providers:\n    claude:\n      cache-keepalive: false\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseConfigBytes([]byte(tc.yaml))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Claude.CacheKeepalive != tc.want {
				t.Fatalf("cache-keepalive = %v, want %v", cfg.Claude.CacheKeepalive, tc.want)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0600); err != nil {
				t.Fatal(err)
			}
			cfg.Claude.CacheKeepalive = !tc.want
			if err := SaveConfigPreserveComments(path, cfg); err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Claude.CacheKeepalive != !tc.want {
				t.Fatal("switch was lost when saving configuration")
			}
		})
	}
}

func TestClaudeCacheKeepaliveDisabledSessionsConfig(t *testing.T) {
	id := "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
	raw := "# preferences\nserver: {port: 8317}\noauth:\n  providers:\n    claude:\n      cache-keepalive: true\n      cache-keepalive-disabled-sessions: [" + id + "]\n"
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Claude.CacheKeepaliveDisabledSessions) != 1 || cfg.Claude.CacheKeepaliveDisabledSessions[0] != id {
		t.Fatal("disabled choice was not loaded")
	}
	if err = SaveConfigPreserveComments(path, cfg, true); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Claude.CacheKeepaliveDisabledSessions) != 1 {
		t.Fatal("disabled choice was lost on save")
	}
	cfg.Claude.CacheKeepaliveDisabledSessions = nil
	if err = SaveConfigPreserveComments(path, cfg, true); err != nil {
		t.Fatal(err)
	}
	loaded, err = LoadConfig(path)
	if err != nil || len(loaded.Claude.CacheKeepaliveDisabledSessions) != 0 {
		t.Fatal("last disabled choice could not be removed")
	}
}
