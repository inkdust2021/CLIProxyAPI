package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestNormalizeFatalAuthEnabledRequiresExplicitEnableOrAction(t *testing.T) {
	if got := normalizeFatalAuthEnabled("", ""); got != "false" {
		t.Fatalf("expected empty fatal auth settings to stay false, got %q", got)
	}
	if got := normalizeFatalAuthEnabled("", "delete"); got != "true" {
		t.Fatalf("expected configured fatal-auth-action to imply true, got %q", got)
	}
	if got := normalizeFatalAuthEnabled(config.FatalAuthModeAuto, "delete"); got != "auto" {
		t.Fatalf("expected explicit auto to be kept, got %q", got)
	}
}

func TestNormalizeFatalAuthActionDefaultsToDisable(t *testing.T) {
	if got := normalizeFatalAuthAction(""); got != "disable" {
		t.Fatalf("expected default disable, got %q", got)
	}
	if got := normalizeFatalAuthAction("DELETE"); got != "delete" {
		t.Fatalf("expected delete, got %q", got)
	}
	if got := normalizeFatalAuthAction("unexpected"); got != "disable" {
		t.Fatalf("expected invalid values to normalize to disable, got %q", got)
	}
}

func TestGetConfigDefaultsFatalAuthEnabledToFalseWithoutSettings(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)

	h.GetConfig(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to decode config payload: %v", err)
	}
	if got, ok := payload["fatal-auth-enabled"].(string); !ok || got != "false" {
		t.Fatalf("expected fatal-auth-enabled to default to false without settings, got %#v", payload["fatal-auth-enabled"])
	}
	if got, _ := payload["fatal-auth-action"].(string); got != "disable" {
		t.Fatalf("expected fatal-auth-action to default to disable, got %#v", payload["fatal-auth-action"])
	}
}

func TestGetConfigEnablesFatalAuthWhenActionConfigured(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	h := NewHandlerWithoutConfigFilePath(&config.Config{SDKConfig: config.SDKConfig{FatalAuthAction: "delete"}}, nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)

	h.GetConfig(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to decode config payload: %v", err)
	}
	if got, ok := payload["fatal-auth-enabled"].(string); !ok || got != "true" {
		t.Fatalf("expected fatal-auth-enabled to become true when action is configured, got %#v", payload["fatal-auth-enabled"])
	}
	if got, _ := payload["fatal-auth-action"].(string); got != "delete" {
		t.Fatalf("expected fatal-auth-action to stay delete, got %#v", payload["fatal-auth-action"])
	}
}

func TestGetConfigKeepsExplicitFatalAuthEnabledAuto(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	h := NewHandlerWithoutConfigFilePath(&config.Config{SDKConfig: config.SDKConfig{FatalAuthEnabled: config.FatalAuthModeAuto, FatalAuthAction: "delete"}}, nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)

	h.GetConfig(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to decode config payload: %v", err)
	}
	if got, ok := payload["fatal-auth-enabled"].(string); !ok || got != "auto" {
		t.Fatalf("expected fatal-auth-enabled to stay auto, got %#v", payload["fatal-auth-enabled"])
	}
}
