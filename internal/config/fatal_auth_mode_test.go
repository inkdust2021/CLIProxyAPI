package config

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestFatalAuthModeUnmarshalYAMLAcceptsBoolAndAuto(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want FatalAuthMode
	}{
		{name: "bool_true", yaml: "fatal-auth-enabled: true\n", want: FatalAuthModeTrue},
		{name: "bool_false", yaml: "fatal-auth-enabled: false\n", want: FatalAuthModeFalse},
		{name: "string_auto", yaml: "fatal-auth-enabled: auto\n", want: FatalAuthModeAuto},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var cfg SDKConfig
			if err := yaml.Unmarshal([]byte(tc.yaml), &cfg); err != nil {
				t.Fatalf("yaml unmarshal: %v", err)
			}
			if cfg.FatalAuthEnabled != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, cfg.FatalAuthEnabled)
			}
		})
	}
}

func TestFatalAuthModeUnmarshalJSONAcceptsBoolAndAuto(t *testing.T) {
	tests := []struct {
		name string
		body string
		want FatalAuthMode
	}{
		{name: "bool_true", body: `{"fatal-auth-enabled":true}`, want: FatalAuthModeTrue},
		{name: "bool_false", body: `{"fatal-auth-enabled":false}`, want: FatalAuthModeFalse},
		{name: "string_auto", body: `{"fatal-auth-enabled":"auto"}`, want: FatalAuthModeAuto},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var cfg SDKConfig
			if err := json.Unmarshal([]byte(tc.body), &cfg); err != nil {
				t.Fatalf("json unmarshal: %v", err)
			}
			if cfg.FatalAuthEnabled != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, cfg.FatalAuthEnabled)
			}
		})
	}
}

func TestNormalizeFatalAuthModeKeepsLegacyFallback(t *testing.T) {
	if got := NormalizeFatalAuthMode("", "delete"); got != FatalAuthModeTrue {
		t.Fatalf("expected configured action to imply true, got %q", got)
	}
	if got := NormalizeFatalAuthMode("", ""); got != FatalAuthModeFalse {
		t.Fatalf("expected empty config to imply false, got %q", got)
	}
}
