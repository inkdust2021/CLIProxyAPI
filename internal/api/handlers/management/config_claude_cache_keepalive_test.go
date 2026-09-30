package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestConfigV8ClaudeCacheKeepaliveSwitch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "# Native Claude keepalive\nserver: {port: 8317}\nplugins: {enabled: false}\noauth:\n  providers:\n    claude:\n      cache-keepalive: false\n      model-level-cooling: true\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg, configFilePath: path}
	reloads := make(chan *config.Config, 2)
	h.SetConfigReloadHook(func(_ context.Context, snapshot *config.Config) {
		reloads <- snapshot
	})
	router := gin.New()
	router.PATCH("/v8/management/config", h.ConfigV8)
	router.GET("/v8/management/config/*path", h.ConfigV8)
	url := "/v8/management/config/oauth/providers/claude/cache-keepalive"
	for _, enabled := range []bool{true, false} {
		want := "false"
		if enabled {
			want = "true"
		}
		body := `{"oauth":{"providers":{"claude":{"cache-keepalive":` + want + `}}}}`
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodPatch, "/v8/management/config", strings.NewReader(body)))
		if w.Code != http.StatusOK {
			t.Fatalf("saving switch: status=%d body=%s", w.Code, w.Body.String())
		}
		loaded, errLoad := config.LoadConfig(path)
		if errLoad != nil {
			t.Fatal(errLoad)
		}
		if loaded.Claude.CacheKeepalive != enabled || !loaded.Claude.ModelLevelCooling || loaded.Plugins.Enabled {
			t.Fatal("switch save lost its value or changed sibling settings")
		}
		saved, errRead := os.ReadFile(path)
		if errRead != nil {
			t.Fatal(errRead)
		}
		if errValidate := config.ValidateV8Config(saved); errValidate != nil {
			t.Fatal(errValidate)
		}
		if !strings.Contains(string(saved), "# Native Claude keepalive") {
			t.Fatal("switch save lost the configuration comment")
		}
		select {
		case snapshot := <-reloads:
			if snapshot.Claude.CacheKeepalive != enabled {
				t.Fatal("runtime reload received the wrong switch value")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("switch save did not trigger runtime reload")
		}
		w = httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
		if w.Code != http.StatusOK || w.Body.String() != want {
			t.Fatalf("switch readback: status=%d body=%s", w.Code, w.Body.String())
		}
	}
}
