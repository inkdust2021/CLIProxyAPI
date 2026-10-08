package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestClaudeCacheKeepaliveReserveQuotaConfig(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      bool
	}{
		{"default", "server: {port: 8317}\n", false},
		{"legacy", "claude: {cache-keepalive-reserve-quota: true}\n", true},
		{"v8", "oauth: {providers: {claude: {cache-keepalive-reserve-quota: true}}}\n", true},
		{"disabled", "oauth: {providers: {claude: {cache-keepalive-reserve-quota: false}}}\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseConfigBytes([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			assertReserve := func(value *Config, want bool) {
				t.Helper()
				data, err := json.Marshal(value.Claude)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]any
				if err := json.Unmarshal(data, &fields); err != nil {
					t.Fatal(err)
				}
				if got, ok := fields["cache-keepalive-reserve-quota"].(bool); !ok || got != want {
					t.Fatalf("reserve switch = %v, want %v", fields["cache-keepalive-reserve-quota"], want)
				}
			}
			assertReserve(cfg, tc.want)
			assertReserve(cfg.CloneForRuntime(), tc.want)
			data, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := ParseConfigBytes(data)
			if err != nil {
				t.Fatal(err)
			}
			assertReserve(snapshot, tc.want)
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.raw), 0600); err != nil {
				t.Fatal(err)
			}
			if err := SaveConfigPreserveComments(path, cfg, true); err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			assertReserve(loaded, tc.want)
			assertReserve(loaded.ForAPIKey(), false)
			cfg.Claude.CacheKeepaliveReserveQuota = !tc.want
			if err := SaveConfigPreserveComments(path, cfg, true); err != nil {
				t.Fatal(err)
			}
			loaded, err = LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			assertReserve(loaded, !tc.want)
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var doc yaml.Node
			if err := yaml.Unmarshal(saved, &doc); err != nil {
				t.Fatal(err)
			}
			if yamlPath(doc.Content[0], "oauth.providers.claude.cache-keepalive-reserve-quota") == nil {
				t.Fatal("switch was not saved in canonical OAuth scope")
			}
		})
	}
}
