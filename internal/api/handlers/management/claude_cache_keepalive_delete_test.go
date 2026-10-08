package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
)

func deleteKeepaliveHandler(t *testing.T, h *Handler, id string) *httptest.ResponseRecorder {
	t.Helper()
	deleter, ok := any(h).(interface{ DeleteClaudeCacheKeepaliveSession(*gin.Context) })
	if !ok {
		t.Fatal("handler does not support session deletion")
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set(ConfigV8ContextKey, true)
	c.Params = gin.Params{{Key: "id", Value: id}}
	c.Request = httptest.NewRequest(http.MethodDelete, "/sessions/"+id, nil)
	deleter.DeleteClaudeCacheKeepaliveSession(c)
	return w
}

func newDeleteKeepaliveHandler(t *testing.T) (*Handler, string, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	raw := "# keep comments\nserver: {port: 8317}\noauth:\n  providers:\n    claude:\n      cache-keepalive: true\n      model-level-cooling: true\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	k := helps.NewClaudeCacheKeepalive(nil)
	t.Cleanup(k.Close)
	k.SetEnabled(true)
	authDir := filepath.Join(dir, "auths")
	if err := k.SetPersistenceDir(authDir); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "https://example.test/v1/messages", nil)
	body := []byte(`{"model":"claude-sonnet-5","max_tokens":100,"cache_control":{"type":"ephemeral","ttl":"1h"},"messages":[{"role":"user","content":"synthetic question"}]}`)
	k.Begin("a", "s", req, body)(true)
	id := k.LogSnapshot().SessionDetails[0].ID
	return &Handler{cfg: cfg, configFilePath: path, claudeCacheKeepalive: k}, id, authDir
}

func TestDeleteClaudeCacheKeepaliveSession(t *testing.T) {
	for _, kind := range []string{"active", "disabled", "disabled-only", "config-only"} {
		t.Run(kind, func(t *testing.T) {
			h, id, authDir := newDeleteKeepaliveHandler(t)
			k := h.claudeCacheKeepalive
			if kind != "active" {
				if kind == "disabled-only" || kind == "config-only" {
					id = strings.Repeat("ab", 32)
				}
				h.cfg.Claude.CacheKeepaliveDisabledSessions = []string{strings.ToUpper(id), strings.Repeat("c", 64)}
				if kind != "config-only" {
					k.SetDisabledSessions(h.cfg.Claude.CacheKeepaliveDisabledSessions)
				}
				if err := config.SaveConfigPreserveComments(h.configFilePath, h.cfg, true); err != nil {
					t.Fatal(err)
				}
			}
			authPath := filepath.Join(authDir, "credential-fixture.json")
			if err := os.WriteFile(authPath, []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			before := k.LogSnapshot().Events
			reloads := make(chan *config.Config, 2)
			h.SetConfigReloadHook(func(_ context.Context, cfg *config.Config) { reloads <- cfg })
			response := deleteKeepaliveHandler(t, h, id)
			if response.Code != 200 || strings.TrimSpace(response.Body.String()) != `{"status":"ok"}` {
				t.Fatalf("delete: %d %s", response.Code, response.Body.String())
			}
			for _, detail := range k.LogSnapshot().SessionDetails {
				if detail.ID == id {
					t.Fatal("runtime retained deleted session")
				}
			}
			if !reflect.DeepEqual(k.LogSnapshot().Events, before) {
				t.Fatal("deletion erased or changed history")
			}
			loaded, err := config.LoadConfig(h.configFilePath)
			if err != nil {
				t.Fatal(err)
			}
			if !loaded.Claude.ModelLevelCooling {
				t.Fatal("changed sibling preference")
			}
			for _, pref := range loaded.Claude.CacheKeepaliveDisabledSessions {
				if strings.EqualFold(pref, id) {
					t.Fatal("disabled preference remained")
				}
			}
			saved, err := os.ReadFile(h.configFilePath)
			if err != nil || !strings.Contains(string(saved), "# keep comments") || strings.Contains(string(saved), "synthetic question") {
				t.Fatal("save lost comments or exposed prompt")
			}
			auth, err := os.ReadFile(authPath)
			if err != nil || string(auth) != "fixture" {
				t.Fatal("deletion changed authentication material")
			}
			if kind == "active" {
				if h.reloadGeneration != 0 {
					t.Fatal("unchanged preference triggered configuration reload")
				}
			} else {
				select {
				case cfg := <-reloads:
					if len(cfg.Claude.CacheKeepaliveDisabledSessions) != 1 {
						t.Fatal("reload retained deleted preference")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("preference change did not reload")
				}
			}
			restored := helps.NewClaudeCacheKeepalive(nil)
			defer restored.Close()
			restored.SetEnabled(true)
			restored.SetDisabledSessions(loaded.Claude.CacheKeepaliveDisabledSessions)
			if err := restored.SetPersistenceDir(authDir); err != nil {
				t.Fatal(err)
			}
			for _, detail := range restored.LogSnapshot().SessionDetails {
				if detail.ID == id {
					t.Fatal("restart restored deleted session")
				}
			}
			if response = deleteKeepaliveHandler(t, h, id); response.Code != 404 {
				t.Fatal("repeat deletion did not return 404")
			}
		})
	}
}

func TestDeleteClaudeCacheKeepaliveSessionErrors(t *testing.T) {
	h, id, _ := newDeleteKeepaliveHandler(t)
	for _, invalid := range []string{"bad", strings.Repeat("a", 63), strings.Repeat("g", 64), strings.Repeat("A", 64)} {
		if response := deleteKeepaliveHandler(t, h, invalid); response.Code != 400 {
			t.Fatal("invalid ID accepted")
		}
	}
	h.configFilePath = filepath.Join(t.TempDir(), "missing", "config.yaml")
	if response := deleteKeepaliveHandler(t, h, strings.Repeat("f", 64)); response.Code != 404 {
		t.Fatal("unknown ID did not return 404")
	}
	h.cfg.Claude.CacheKeepaliveDisabledSessions = []string{id}
	h.claudeCacheKeepalive.SetDisabledSessions([]string{id})
	before := h.claudeCacheKeepalive.LogSnapshot()
	if response := deleteKeepaliveHandler(t, h, id); response.Code != 500 {
		t.Fatal("failed configuration save did not return 500")
	}
	if !reflect.DeepEqual(h.claudeCacheKeepalive.LogSnapshot(), before) || len(h.cfg.Claude.CacheKeepaliveDisabledSessions) != 1 || h.reloadGeneration != 0 {
		t.Fatal("failed configuration save mutated state")
	}
	h.claudeCacheKeepalive = nil
	if response := deleteKeepaliveHandler(t, h, id); response.Code != 503 {
		t.Fatal("unavailable keeper did not return 503")
	}
	h, id, _ = newDeleteKeepaliveHandler(t)
	h.cfg = nil
	if response := deleteKeepaliveHandler(t, h, id); response.Code != 503 {
		t.Fatal("unavailable config did not return 503")
	}
}

func TestDeleteClaudeCacheKeepalivePersistenceFailureRestoresPreference(t *testing.T) {
	for _, mode := range []string{"unwritable", "corrupt", "config-only-corrupt"} {
		t.Run(mode, func(t *testing.T) {
			h, id, authDir := newDeleteKeepaliveHandler(t)
			if mode == "config-only-corrupt" {
				id = strings.Repeat("ab", 32)
			}
			h.cfg.Claude.CacheKeepaliveDisabledSessions = []string{id, strings.Repeat("c", 64)}
			if mode != "config-only-corrupt" {
				h.claudeCacheKeepalive.SetDisabledSessions(h.cfg.Claude.CacheKeepaliveDisabledSessions)
			}
			if err := config.SaveConfigPreserveComments(h.configFilePath, h.cfg, true); err != nil {
				t.Fatal(err)
			}
			if mode != "unwritable" {
				badDir := t.TempDir()
				if err := os.WriteFile(filepath.Join(badDir, ".claude-cache-keepalive.bin"), []byte("corrupt fixture"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := h.claudeCacheKeepalive.SetPersistenceDir(badDir); err == nil {
					t.Fatal("accepted corrupt state")
				}
			} else {
				path := filepath.Join(authDir, ".claude-cache-keepalive.bin")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "occupied"), []byte("fixture"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before := h.claudeCacheKeepalive.LogSnapshot()
			response := deleteKeepaliveHandler(t, h, id)
			if response.Code != 500 {
				t.Fatal("failed snapshot save did not return 500")
			}
			if !reflect.DeepEqual(h.claudeCacheKeepalive.LogSnapshot(), before) || h.reloadGeneration != 0 {
				t.Fatal("failed delete changed runtime or reloaded configuration")
			}
			loaded, err := config.LoadConfig(h.configFilePath)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(loaded.Claude.CacheKeepaliveDisabledSessions, h.cfg.Claude.CacheKeepaliveDisabledSessions) || len(loaded.Claude.CacheKeepaliveDisabledSessions) != 2 || !loaded.Claude.ModelLevelCooling {
				t.Fatal("failed deletion did not restore saved preferences")
			}
			saved, err := os.ReadFile(h.configFilePath)
			if err != nil || !strings.Contains(string(saved), "# keep comments") {
				t.Fatal("rollback lost comments")
			}
			var payload map[string]string
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || payload["error"] == "" {
				t.Fatal("missing failure response")
			}
		})
	}
}

func TestDeleteClaudeCacheKeepaliveSkipsSupersededReload(t *testing.T) {
	h, id, _ := newDeleteKeepaliveHandler(t)
	h.cfg.Claude.CacheKeepaliveDisabledSessions = []string{id}
	h.claudeCacheKeepalive.SetDisabledSessions([]string{id})
	h.mu.Lock()
	older := h.reloadSnapshotConfigLocked()
	h.mu.Unlock()
	applied := make(chan *config.Config, 2)
	h.SetConfigReloadHook(func(_ context.Context, cfg *config.Config) {
		h.claudeCacheKeepalive.SetDisabledSessions(cfg.Claude.CacheKeepaliveDisabledSessions)
		h.SetConfig(cfg)
		applied <- cfg
	})
	// Reserve the reload mutex so the deletion's new snapshot stays queued.
	h.reloadMu.Lock()
	h.cfg.Claude.CacheKeepaliveDisabledSessions = nil
	h.mu.Lock()
	newer := h.reloadSnapshotConfigLocked()
	h.mu.Unlock()
	h.reloadMu.Unlock()
	h.reloadConfigAfterManagementSave(context.Background(), older)
	select {
	case <-applied:
		t.Fatal("superseded reload restored a deleted preference")
	default:
	}
	h.reloadConfigAfterManagementSave(context.Background(), newer)
	if got := <-applied; len(got.Claude.CacheKeepaliveDisabledSessions) != 0 {
		t.Fatal("latest reload restored deleted preference")
	}
}

func TestDeleteClaudeCacheKeepaliveWaitsForRunningReload(t *testing.T) {
	h, activeID, _ := newDeleteKeepaliveHandler(t)
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	h.cfg.Claude.CacheKeepaliveDisabledSessions = []string{a, b}
	h.claudeCacheKeepalive.SetDisabledSessions([]string{a, b})
	if err := config.SaveConfigPreserveComments(h.configFilePath, h.cfg, true); err != nil {
		t.Fatal(err)
	}
	started, applyFirst, firstDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	calls := 0
	h.SetConfigReloadHook(func(_ context.Context, cfg *config.Config) {
		calls++
		first := calls == 1
		if first {
			close(started)
			<-applyFirst
		}
		h.claudeCacheKeepalive.SetDisabledSessions(cfg.Claude.CacheKeepaliveDisabledSessions)
		h.SetConfig(cfg)
		if first {
			close(firstDone)
		}
	})
	if w := deleteKeepaliveHandler(t, h, a); w.Code != 200 {
		t.Fatal(w.Code)
	}
	<-started
	// The hook is still running while the next deletion starts.
	deletionStarted, deletionDone := make(chan struct{}), make(chan *httptest.ResponseRecorder, 1)
	go func() { close(deletionStarted); deletionDone <- deleteKeepaliveHandler(t, h, b) }()
	<-deletionStarted
	close(applyFirst)
	<-firstDone
	if w := <-deletionDone; w.Code != 200 {
		t.Fatal(w.Code)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set(ConfigV8ContextKey, true)
	c.Params = gin.Params{{Key: "id", Value: activeID}}
	c.Request = httptest.NewRequest(http.MethodPatch, "/sessions/"+activeID, strings.NewReader(`{"enabled":false}`))
	h.PatchClaudeCacheKeepaliveSession(c)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	h.mu.Lock()
	snapshot := h.reloadSnapshotConfigLocked()
	h.mu.Unlock()
	h.reloadConfigAfterManagementSave(context.Background(), snapshot)
	loaded, err := config.LoadConfig(h.configFilePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range loaded.Claude.CacheKeepaliveDisabledSessions {
		if id == a || id == b {
			t.Fatal("older reload restored a deleted preference")
		}
	}
	for _, detail := range h.claudeCacheKeepalive.LogSnapshot().SessionDetails {
		if detail.ID == a || detail.ID == b {
			t.Fatal("older reload restored a deleted runtime row")
		}
	}
}
