package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

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

func TestGetConfigDefaultsFatalAuthActionToDisable(t *testing.T) {
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
	if got, _ := payload["fatal-auth-action"].(string); got != "disable" {
		t.Fatalf("expected fatal-auth-action to default to disable, got %#v", payload["fatal-auth-action"])
	}
}
