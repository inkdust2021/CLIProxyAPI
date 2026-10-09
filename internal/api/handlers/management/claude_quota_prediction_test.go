package management

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestGetClaudeQuotaPrediction(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg, err := config.ParseConfigBytes([]byte("oauth: {providers: {claude: {cache-keepalive: true, cache-keepalive-reserve-quota: true}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	auth := &coreauth.Auth{ID: "prediction-api", Provider: "claude", Status: coreauth.StatusActive, Attributes: map[string]string{coreauth.AttributeAuthKind: coreauth.AuthKindOAuth}}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1791440000, 0)
	reset := fmt.Sprint(now.Add(4 * time.Hour).Truncate(time.Hour).Unix())
	for i, utilization := range []string{"0.90", "0.91", "0.92", "0.93", "0.95"} {
		at := now.Add(time.Duration(i) * time.Minute)
		manager.ObserveClaudeQuotaUsage(usage.Record{
			AuthID: auth.ID, Provider: "claude", Model: "claude-opus-4-6", RequestedAt: at, Latency: time.Second,
			Detail: usage.Detail{InputTokens: 1000},
			ResponseHeaders: http.Header{
				"Anthropic-Ratelimit-Unified-5h-Utilization": {utilization},
				"Anthropic-Ratelimit-Unified-5h-Reset":       {reset},
			},
		})
	}
	h := NewHandler(cfg, "", manager)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v8/management/observability/claude-quota-prediction", nil)
	h.GetClaudeQuotaPrediction(c)
	var got struct {
		Samples []coreauth.ClaudeQuotaPredictionEvaluation `json:"samples"`
		Summary map[string]claudeQuotaPredictionError      `json:"summary"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || len(got.Samples) != 1 || got.Summary["5h"].Count != 1 || got.Summary["7d"].Count != 0 ||
		math.Abs(got.Summary["5h"].MeanAbsoluteErrorPP-1) > 1e-9 || math.Abs(got.Summary["5h"].MeanSignedErrorPP+1) > 1e-9 {
		t.Fatalf("unexpected forecast evaluation: %d %s", w.Code, w.Body.String())
	}
}
