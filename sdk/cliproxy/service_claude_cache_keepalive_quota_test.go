package cliproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestServiceClaudeCacheKeepaliveReservedQuotaReplay(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Header.Get("Authorization") != "Bearer original-token" {
			t.Error("replay did not use the original credential")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","usage":{"cache_read_input_tokens":1000,"output_tokens":1}}`)
	}))
	defer server.Close()
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: t.Name(), Provider: "claude", Status: coreauth.StatusActive, Attributes: map[string]string{coreauth.AttributeAuthKind: coreauth.AuthKindOAuth, "api_key": "original-token", "base_url": server.URL}, Quota: coreauth.QuotaState{ObservedAt: time.Now(), Signals: map[string]string{"Anthropic-Ratelimit-Unified-5h-Utilization": "0.99"}}}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	s := &Service{cfg: &config.Config{}, coreManager: manager}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	cfg := &config.Config{Claude: internalconfig.ClaudeConfig{CacheKeepalive: true, CacheKeepaliveReserveQuota: true}, DisableClaudeCloakMode: true}
	if !s.applyConfigUpdateWithAuthSynthesis(context.Background(), cfg, false) {
		t.Fatal("failed to configure reservation")
	}
	if selected, err := manager.SelectAuth(context.Background(), "claude", "", cliproxyexecutor.Options{}); selected != nil || err == nil {
		t.Fatalf("ordinary request used reserved credential: %v, %v", selected, err)
	}
	snapshot := helps.ClaudeCacheSnapshot{AuthID: auth.ID, URL: server.URL + "/v1/messages?beta=true", Headers: make(http.Header), Body: []byte(`{"model":"claude-sonnet-5","max_tokens":1,"stream":false,"messages":[{"role":"user","content":"synthetic replay"}]}`)}
	if cacheRead, err := s.replayClaudeCache(context.Background(), snapshot); err != nil || cacheRead != 1000 {
		t.Fatalf("reserved replay = %d, %v", cacheRead, err)
	}
	stored, _ := manager.GetByID(auth.ID)
	if !reflect.DeepEqual(stored.Quota, auth.Quota) || stored.Unavailable || stored.Disabled {
		t.Fatal("reservation or replay mutated quota availability")
	}
	stored.NextRetryAfter = time.Now().Add(time.Hour)
	if _, err := manager.Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	if _, err := s.replayClaudeCache(context.Background(), snapshot); err == nil {
		t.Fatal("reservation bypassed a real cooldown during replay")
	}
	if requests.Load() != 1 {
		t.Fatalf("replay requests = %d, want 1", requests.Load())
	}
}

func TestServiceClaudeCacheKeepaliveReplayQuotaHeadersAreObserved(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.99")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", fmt.Sprint(time.Now().Add(time.Hour).Unix()))
		_, _ = io.WriteString(w, `{"type":"message","usage":{"cache_read_input_tokens":1000,"output_tokens":1}}`)
	}))
	defer server.Close()

	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: t.Name(), Provider: "claude", Status: coreauth.StatusActive,
		Attributes: map[string]string{coreauth.AttributeAuthKind: coreauth.AuthKindOAuth, "api_key": "original-token", "base_url": server.URL},
		Quota:      coreauth.QuotaState{ObservedAt: time.Now(), Signals: map[string]string{"Anthropic-Ratelimit-Unified-5h-Utilization": "0.2"}},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	s := &Service{cfg: &config.Config{}, coreManager: manager}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	cfg := &config.Config{Claude: internalconfig.ClaudeConfig{CacheKeepalive: true, CacheKeepaliveReserveQuota: true}, DisableClaudeCloakMode: true}
	if !s.applyConfigUpdateWithAuthSynthesis(context.Background(), cfg, false) {
		t.Fatal("failed to configure reservation")
	}
	if selected, err := manager.SelectAuth(context.Background(), "claude", "", cliproxyexecutor.Options{}); err != nil || selected == nil {
		t.Fatalf("ordinary auth unavailable before replay: %v, %v", selected, err)
	}
	snapshot := helps.ClaudeCacheSnapshot{AuthID: auth.ID, URL: server.URL + "/v1/messages?beta=true", Headers: make(http.Header), Body: []byte(`{"model":"claude-sonnet-5","max_tokens":1,"stream":false,"messages":[{"role":"user","content":"synthetic replay"}]}`)}
	if _, err := s.replayClaudeCache(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if selected, err := manager.SelectAuth(context.Background(), "claude", "", cliproxyexecutor.Options{}); selected != nil || err == nil {
		t.Fatalf("replay quota header did not reserve ordinary auth: %v, %v", selected, err)
	}
}
