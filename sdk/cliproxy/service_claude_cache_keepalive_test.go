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
