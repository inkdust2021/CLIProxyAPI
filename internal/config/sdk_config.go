// Package config provides configuration management for the CLI Proxy API server.
// It handles loading and parsing YAML configuration files, and provides structured
// access to application settings including server port, authentication directory,
// debug settings, proxy configuration, and API keys.
package config

import (
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// FatalAuthMode describes how runtime auth failures should be handled.
type FatalAuthMode string

const (
	// FatalAuthModeFalse disables fatal auth handling.
	FatalAuthModeFalse FatalAuthMode = "false"
	// FatalAuthModeTrue applies fatal-auth-action to any runtime error.
	FatalAuthModeTrue FatalAuthMode = "true"
	// FatalAuthModeAuto applies selective built-in rules.
	FatalAuthModeAuto FatalAuthMode = "auto"
)

// ParseFatalAuthMode normalizes supported string values into canonical modes.
func ParseFatalAuthMode(value string) (FatalAuthMode, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "on", "enable", "enabled":
		return FatalAuthModeTrue, true
	case "false", "off", "disable", "disabled":
		return FatalAuthModeFalse, true
	case "auto":
		return FatalAuthModeAuto, true
	default:
		return "", false
	}
}

// NormalizeFatalAuthMode returns the effective runtime mode.
// When fatal-auth-enabled is omitted, the legacy behavior remains:
// configured fatal-auth-action implies true, otherwise false.
func NormalizeFatalAuthMode(mode FatalAuthMode, action string) FatalAuthMode {
	if normalized, ok := ParseFatalAuthMode(string(mode)); ok {
		return normalized
	}
	if strings.TrimSpace(action) != "" {
		return FatalAuthModeTrue
	}
	return FatalAuthModeFalse
}

func fatalAuthModeFromBool(enabled bool) FatalAuthMode {
	if enabled {
		return FatalAuthModeTrue
	}
	return FatalAuthModeFalse
}

// UnmarshalYAML accepts both legacy bool values and string modes.
func (m *FatalAuthMode) UnmarshalYAML(value *yaml.Node) error {
	if value == nil {
		*m = ""
		return nil
	}
	switch value.Tag {
	case "!!null":
		*m = ""
		return nil
	case "!!bool":
		var enabled bool
		if err := value.Decode(&enabled); err != nil {
			return err
		}
		*m = fatalAuthModeFromBool(enabled)
		return nil
	default:
		var raw string
		if err := value.Decode(&raw); err != nil {
			return err
		}
		raw = strings.TrimSpace(raw)
		if raw == "" {
			*m = ""
			return nil
		}
		normalized, ok := ParseFatalAuthMode(raw)
		if !ok {
			return fmt.Errorf("invalid fatal-auth-enabled: %q", raw)
		}
		*m = normalized
		return nil
	}
}

// UnmarshalJSON accepts both legacy bool values and string modes.
func (m *FatalAuthMode) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		*m = ""
		return nil
	}
	var enabled bool
	if err := json.Unmarshal(data, &enabled); err == nil {
		*m = fatalAuthModeFromBool(enabled)
		return nil
	}
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		*m = ""
		return nil
	}
	normalized, ok := ParseFatalAuthMode(raw)
	if !ok {
		return fmt.Errorf("invalid fatal-auth-enabled: %q", raw)
	}
	*m = normalized
	return nil
}

// SDKConfig represents the application's configuration, loaded from a YAML file.
type SDKConfig struct {
	// ProxyURL is the URL of an optional proxy server to use for outbound requests.
	ProxyURL string `yaml:"proxy-url" json:"proxy-url"`

	// ForceModelPrefix requires explicit model prefixes (e.g., "teamA/gemini-3-pro-preview")
	// to target prefixed credentials. When false, unprefixed model requests may use prefixed
	// credentials as well.
	ForceModelPrefix bool `yaml:"force-model-prefix" json:"force-model-prefix"`

	// RequestLog enables or disables detailed request logging functionality.
	RequestLog bool `yaml:"request-log" json:"request-log"`

	// APIKeys is a list of keys for authenticating clients to this proxy server.
	APIKeys []string `yaml:"api-keys" json:"api-keys"`

	// PassthroughHeaders controls whether upstream response headers are forwarded to downstream clients.
	// Default is false (disabled).
	PassthroughHeaders bool `yaml:"passthrough-headers" json:"passthrough-headers"`

	// Streaming configures server-side streaming behavior (keep-alives and safe bootstrap retries).
	Streaming StreamingConfig `yaml:"streaming" json:"streaming"`

	// NonStreamKeepAliveInterval controls how often blank lines are emitted for non-streaming responses.
	// <= 0 disables keep-alives. Value is in seconds.
	NonStreamKeepAliveInterval int `yaml:"nonstream-keepalive-interval,omitempty" json:"nonstream-keepalive-interval,omitempty"`

	// FatalAuthEnabled 控制账号出错后的自动禁用/删除模式。
	// 支持值：true、false、auto。
	// 未显式配置时，会跟随 fatal-auth-action 是否已配置。
	// auto 模式下：usage_limit_reached 自动禁用，401 自动删除，其他错误忽略。
	FatalAuthEnabled FatalAuthMode `yaml:"fatal-auth-enabled,omitempty" json:"fatal-auth-enabled,omitempty"`

	// FatalAuthAction 控制账号出现任意错误后的处理方式。
	// 仅在 fatal-auth-enabled 为 true 时生效。
	// 支持值："delete" 和 "disable"（默认）。
	FatalAuthAction string `yaml:"fatal-auth-action,omitempty" json:"fatal-auth-action,omitempty"`
}

// StreamingConfig holds server streaming behavior configuration.
type StreamingConfig struct {
	// KeepAliveSeconds controls how often the server emits SSE heartbeats (": keep-alive\n\n").
	// <= 0 disables keep-alives. Default is 0.
	KeepAliveSeconds int `yaml:"keepalive-seconds,omitempty" json:"keepalive-seconds,omitempty"`

	// BootstrapRetries controls how many times the server may retry a streaming request before any bytes are sent,
	// to allow auth rotation / transient recovery.
	// <= 0 disables bootstrap retries. Default is 0.
	BootstrapRetries int `yaml:"bootstrap-retries,omitempty" json:"bootstrap-retries,omitempty"`
}
