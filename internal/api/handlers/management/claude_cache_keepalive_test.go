package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
)

func TestGetClaudeCacheKeepaliveSnapshot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{}
	keeper := helps.NewClaudeCacheKeepalive(nil)
	defer keeper.Close()
	for _, enabled := range []bool{false, true, false} {
		keeper.SetEnabled(enabled)
		h.SetClaudeCacheKeepalive(keeper)
		var previous string
		for i := 0; i < 2; i++ {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/v8/management/observability/claude-cache-keepalive", nil)
			h.GetClaudeCacheKeepalive(c)
			var got helps.ClaudeCacheLogSnapshot
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if w.Code != 200 || got.Enabled != enabled || got.Capacity != 200 || got.Events == nil {
				t.Fatalf("invalid snapshot: %s", w.Body.String())
			}
			if i > 0 && previous != w.Body.String() {
				t.Fatal("read consumed events")
			}
			previous = w.Body.String()
		}
	}
	h.SetClaudeCacheKeepalive(nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	h.GetClaudeCacheKeepalive(c)
	if w.Code != 200 {
		t.Fatal("unavailable keeper should return an empty disabled snapshot")
	}
}

func TestPatchClaudeCacheKeepaliveSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "# keep comments\nserver: {port: 8317}\noauth:\n  providers:\n    claude:\n      cache-keepalive: true\n      model-level-cooling: true\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	k := helps.NewClaudeCacheKeepalive(nil)
	defer k.Close()
	k.SetEnabled(true)
	req := httptest.NewRequest(http.MethodPost, "https://example.test/v1/messages", nil)
	body := []byte(`{"model":"claude-sonnet-5","max_tokens":100,"cache_control":{"type":"ephemeral"},"messages":[{"role":"user","content":"private human prompt"}]}`)
	k.Begin("a", "s", req, body)(true)
	id := k.LogSnapshot().SessionDetails[0].ID
	h := &Handler{cfg: cfg, configFilePath: path, claudeCacheKeepalive: k}
	router := gin.New()
	router.PATCH("/sessions/:id", func(c *gin.Context) { c.Set(ConfigV8ContextKey, true) }, h.PatchClaudeCacheKeepaliveSession)
	for _, enabled := range []bool{false, false, true} {
		want := "false"
		if enabled {
			want = "true"
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodPatch, "/sessions/"+id, strings.NewReader(`{"enabled":`+want+`}`)))
		if w.Code != 200 {
			t.Fatalf("patch: %d %s", w.Code, w.Body.String())
		}
		loaded, errLoad := config.LoadConfig(path)
		if errLoad != nil {
			t.Fatal(errLoad)
		}
		if (len(loaded.Claude.CacheKeepaliveDisabledSessions) == 0) != enabled || !loaded.Claude.ModelLevelCooling {
			t.Fatal("preference did not persist or changed siblings")
		}
		saved, _ := os.ReadFile(path)
		if !strings.Contains(string(saved), "# keep comments") || strings.Contains(string(saved), "private human prompt") {
			t.Fatal("save lost comments or persisted prompt")
		}
		if (k.LogSnapshot().SessionDetails[0].State != "disabled") != enabled {
			t.Fatal("runtime preference not applied immediately")
		}
	}
	for _, tc := range []struct {
		id, body string
		status   int
	}{
		{"bad", `{"enabled":false}`, 400},
		{id, `{}`, 400},
		{id, `{"enabled":"no"}`, 400},
		{strings.Repeat("f", 64), `{"enabled":false}`, 404},
	} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodPatch, "/sessions/"+tc.id, strings.NewReader(tc.body)))
		if w.Code != tc.status {
			t.Fatalf("invalid input %s %s: %d", tc.id, tc.body, w.Code)
		}
	}
	h.configFilePath = filepath.Join(t.TempDir(), "missing", "config.yaml")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodPatch, "/sessions/"+id, strings.NewReader(`{"enabled":false}`)))
	if w.Code != 500 || k.LogSnapshot().DisabledSessions != 0 || len(h.cfg.Claude.CacheKeepaliveDisabledSessions) != 0 {
		t.Fatal("save failure applied unpersisted preference")
	}
	h.claudeCacheKeepalive = nil
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodPatch, "/sessions/"+id, strings.NewReader(`{"enabled":false}`)))
	if w.Code != 503 {
		t.Fatal("unavailable keeper did not return 503")
	}
}

func TestPatchClaudeCacheKeepaliveSessionCanonicalID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	id := strings.Repeat("ab", 32)
	cfg := &config.Config{Claude: config.ClaudeConfig{CacheKeepaliveDisabledSessions: []string{strings.ToUpper(id)}}}
	if err := os.WriteFile(path, []byte("config-version: 8\n"), 0600); err != nil {
		t.Fatal(err)
	}
	k := helps.NewClaudeCacheKeepalive(nil)
	defer k.Close()
	k.SetDisabledSessions(cfg.Claude.CacheKeepaliveDisabledSessions)
	h := &Handler{cfg: cfg, configFilePath: path, claudeCacheKeepalive: k}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: id}}
	c.Request = httptest.NewRequest(http.MethodPatch, "/sessions/"+id, strings.NewReader(`{"enabled":true}`))
	h.PatchClaudeCacheKeepaliveSession(c)
	if w.Code != 200 || k.LogSnapshot().DisabledSessions != 0 || len(cfg.Claude.CacheKeepaliveDisabledSessions) != 0 {
		t.Fatal("canonical session ID did not remove persisted uppercase preference")
	}
}
