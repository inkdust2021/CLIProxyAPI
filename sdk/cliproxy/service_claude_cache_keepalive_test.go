package cliproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestServiceClaudeCacheKeepaliveReload(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		keys = append(keys, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","model":"claude-sonnet-5","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":1000}}`)
	}))
	defer server.Close()
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: "original-account", Provider: "claude", Status: coreauth.StatusActive, Attributes: map[string]string{"api_key": "old-token", "base_url": server.URL}}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	s := &Service{cfg: &config.Config{}, coreManager: manager}
	defer func() { _ = s.Shutdown(context.Background()) }()
	cfg := &config.Config{Claude: internalconfig.ClaudeConfig{CacheKeepalive: true}, DisableClaudeCloakMode: true}
	if !s.applyConfigUpdateWithAuthSynthesis(context.Background(), cfg, false) {
		t.Fatal("failed to enable keepalive through config reload")
	}
	k := s.claudeCacheKeeper()
	e, ok := manager.Executor("claude")
	if !ok {
		t.Fatal("Claude executor was not bound")
	}
	body := []byte(`{"model":"claude-sonnet-5","max_tokens":100,"stream":false,"system":[{"type":"text","text":"prefix","cache_control":{"type":"ephemeral","ttl":"5m"}}],"messages":[{"role":"user","content":"q"}]}`)
	if _, err := e.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "claude-sonnet-5", Payload: body}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}); err != nil {
		t.Fatal(err)
	}
	updated := auth.Clone()
	updated.Attributes["api_key"] = "fresh-token"
	if _, err := manager.Update(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	k.ReplayDue(context.Background(), time.Now().Add(4*time.Minute+time.Second))
	mu.Lock()
	if len(keys) != 2 || keys[1] != "Bearer fresh-token" {
		t.Fatalf("replay did not resolve fresh original credentials: %v", keys)
	}
	mu.Unlock()
	disabled := cfg.CloneForRuntime()
	disabled.Claude.CacheKeepalive = false
	if !s.applyConfigUpdateWithAuthSynthesis(context.Background(), disabled, false) {
		t.Fatal("failed to disable keepalive through config reload")
	}
	k.ReplayDue(context.Background(), time.Now().Add(8*time.Minute))
	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 2 {
		t.Fatal("disabled keepalive sent an additional upstream request")
	}
}

func TestServiceClaudeCacheKeepaliveRejectsUnavailableAuth(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	s := &Service{cfg: &config.Config{}, coreManager: manager}
	snapshot := helps.ClaudeCacheSnapshot{AuthID: "missing"}
	if _, err := s.replayClaudeCache(context.Background(), snapshot); err == nil {
		t.Fatal("replay accepted a missing account")
	}
	for _, auth := range []*coreauth.Auth{
		{ID: "disabled", Provider: "claude", Disabled: true},
		{ID: "cooling", Provider: "claude", NextRetryAfter: time.Now().Add(time.Hour)},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
		snapshot.AuthID = auth.ID
		if _, err := s.replayClaudeCache(context.Background(), snapshot); err == nil {
			t.Fatalf("replay accepted unavailable account %s", auth.ID)
		}
	}
}

func TestServiceClaudeCacheKeepaliveDisabledSessionReload(t *testing.T) {
	s := &Service{cfg: &config.Config{Claude: internalconfig.ClaudeConfig{CacheKeepalive: true}}}
	k := s.claudeCacheKeeper()
	defer k.Close()
	req, err := http.NewRequest(http.MethodPost, "https://example.test/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"claude-sonnet-5","max_tokens":100,"cache_control":{"type":"ephemeral"},"messages":[{"role":"user","content":"q"}]}`)
	k.Begin("a", "s", req, body)(true)
	id := k.LogSnapshot().SessionDetails[0].ID
	cfg := s.cfg.CloneForRuntime()
	cfg.Claude.CacheKeepaliveDisabledSessions = []string{id}
	s.configureClaudeCacheKeepalive(cfg)
	if k.LogSnapshot().SessionDetails[0].State != "disabled" {
		t.Fatal("reload did not apply disabled session preference")
	}
	fresh := &Service{cfg: cfg}
	other := fresh.claudeCacheKeeper()
	defer other.Close()
	other.Begin("a", "s", req, body)(true)
	if other.LogSnapshot().SessionDetails[0].State != "disabled" {
		t.Fatal("startup forgot persisted disabled session")
	}
	cfg.Claude.CacheKeepaliveDisabledSessions = nil
	s.configureClaudeCacheKeepalive(cfg)
	if k.LogSnapshot().SessionDetails[0].State != "active" {
		t.Fatal("reload failed to resume valid session")
	}
}

func TestServiceClaudeCacheKeepaliveRestoresAfterRestart(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		keys = append(keys, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","model":"claude-sonnet-5","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":1000}}`)
	}))
	defer server.Close()
	cfg := &config.Config{AuthDir: t.TempDir(), Claude: internalconfig.ClaudeConfig{CacheKeepalive: true}, DisableClaudeCloakMode: true}
	newService := func(token string) (*Service, *coreauth.Auth) {
		t.Helper()
		manager := coreauth.NewManager(nil, nil, nil)
		auth := &coreauth.Auth{ID: "original-account", Provider: "claude", Status: coreauth.StatusActive, Attributes: map[string]string{"api_key": token, "base_url": server.URL}}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
		s := &Service{cfg: &config.Config{}, coreManager: manager}
		if !s.applyConfigUpdateWithAuthSynthesis(context.Background(), cfg.CloneForRuntime(), false) {
			t.Fatal("failed to configure service")
		}
		t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
		return s, auth
	}
	first, auth := newService("old-token")
	e, ok := first.coreManager.Executor("claude")
	if !ok {
		t.Fatal("Claude executor not registered")
	}
	body := []byte(`{"model":"claude-sonnet-5","max_tokens":100,"stream":false,"cache_control":{"type":"ephemeral"},"messages":[{"role":"user","content":"restart continuity"}]}`)
	if _, err := e.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "claude-sonnet-5", Payload: body}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}); err != nil {
		t.Fatal(err)
	}
	before := first.claudeCacheKeeper().LogSnapshot()
	if before.Sessions != 1 {
		t.Fatal("initial request was not tracked")
	}
	if err := first.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	second, _ := newService("fresh-token")
	after := second.claudeCacheKeeper().LogSnapshot()
	if after.Sessions != 1 || after.SessionDetails[0].ID != before.SessionDetails[0].ID || after.SessionDetails[0].LastPrompt != "restart continuity" {
		t.Fatal("restart lost the original session and prompt")
	}
	foundHistory := false
	for _, event := range after.Events {
		if event.ID == before.Events[0].ID && event.Time.Equal(before.Events[0].Time) && event.Outcome == before.Events[0].Outcome {
			foundHistory = true
		}
	}
	if !foundHistory {
		t.Fatal("restart lost the last tracked event")
	}
	second.claudeCacheKeeper().ReplayDue(context.Background(), time.Now().Add(4*time.Minute+time.Second))
	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 2 || keys[1] != "Bearer fresh-token" {
		t.Fatalf("restored replay did not use current original-account credentials: %v", keys)
	}
}
