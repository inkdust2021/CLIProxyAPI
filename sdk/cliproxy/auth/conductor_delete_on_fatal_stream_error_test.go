package auth

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
)

type deletingStore struct {
	saveCount atomic.Int32

	mu         sync.Mutex
	deletedIDs []string
}

func (s *deletingStore) List(context.Context) ([]*Auth, error) { return nil, nil }

func (s *deletingStore) Save(context.Context, *Auth) (string, error) {
	s.saveCount.Add(1)
	return "", nil
}

func (s *deletingStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletedIDs = append(s.deletedIDs, id)
	return nil
}

func (s *deletingStore) DeletedIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.deletedIDs))
	copy(out, s.deletedIDs)
	return out
}

func registerDeleteTestAuth(t *testing.T, manager *Manager, authID string) *Auth {
	t.Helper()
	auth := &Auth{
		ID:       authID,
		Provider: "codex",
		FileName: authID,
		Metadata: map[string]any{"type": "codex"},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "gpt-5.3-codex"}})
	t.Cleanup(func() {
		reg.UnregisterClient(auth.ID)
	})
	return auth
}

func TestManager_MarkResult_DeletesAuthOnAnyErrorWhenConfigured(t *testing.T) {
	tests := []struct {
		name    string
		message string
		status  int
	}{
		{name: "timeout", message: "stream error: upstream timeout", status: 408},
		{name: "quota", message: "provider error: usage_limit_reached", status: 429},
		{name: "bad_request", message: "provider error: invalid request", status: 400},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			store := &deletingStore{}
			manager := NewManager(store, nil, nil)
			manager.SetConfig(&internalconfig.Config{SDKConfig: internalconfig.SDKConfig{
				FatalAuthEnabled: internalconfig.FatalAuthModeTrue,
				FatalAuthAction:  fatalAuthActionDelete,
			}})
			auth := registerDeleteTestAuth(t, manager, tc.name+".json")
			reg := registry.GetGlobalRegistry()

			manager.MarkResult(context.Background(), Result{
				AuthID:   auth.ID,
				Provider: auth.Provider,
				Model:    "gpt-5.3-codex",
				Success:  false,
				Error: &Error{
					Message:    tc.message,
					HTTPStatus: tc.status,
				},
			})

			if _, ok := manager.GetByID(auth.ID); ok {
				t.Fatalf("expected auth %s to be deleted", auth.ID)
			}
			if reg.ClientSupportsModel(auth.ID, "gpt-5.3-codex") {
				t.Fatalf("expected registry entry for %s to be removed", auth.ID)
			}
			if got := store.saveCount.Load(); got != 1 {
				t.Fatalf("expected only register persistence, got %d saves", got)
			}
			deletedIDs := store.DeletedIDs()
			if len(deletedIDs) != 1 || deletedIDs[0] != auth.ID {
				t.Fatalf("unexpected delete calls: %v", deletedIDs)
			}
		})
	}
}

func TestManager_MarkResult_DisablesAuthOnAnyErrorWhenConfigured(t *testing.T) {
	store := &deletingStore{}
	manager := NewManager(store, nil, nil)
	manager.SetConfig(&internalconfig.Config{SDKConfig: internalconfig.SDKConfig{
		FatalAuthEnabled: internalconfig.FatalAuthModeTrue,
		FatalAuthAction:  fatalAuthActionDisable,
	}})
	auth := registerDeleteTestAuth(t, manager, "auth-disable.json")
	reg := registry.GetGlobalRegistry()

	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "gpt-5.3-codex",
		Success:  false,
		Error: &Error{
			Message:    "stream error: upstream timeout",
			HTTPStatus: 408,
		},
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth %s to remain disabled", auth.ID)
	}
	if !updated.Disabled {
		t.Fatalf("expected auth %s to be disabled", auth.ID)
	}
	if updated.Status != StatusDisabled {
		t.Fatalf("expected status %s, got %s", StatusDisabled, updated.Status)
	}
	if updated.StatusMessage != "stream error: upstream timeout" {
		t.Fatalf("unexpected status message: %q", updated.StatusMessage)
	}
	if reg.ClientSupportsModel(auth.ID, "gpt-5.3-codex") {
		t.Fatalf("expected registry entry for %s to be removed", auth.ID)
	}
	if got := store.saveCount.Load(); got != 2 {
		t.Fatalf("expected register + disable persistence, got %d saves", got)
	}
	if deletedIDs := store.DeletedIDs(); len(deletedIDs) != 0 {
		t.Fatalf("expected no delete calls, got %v", deletedIDs)
	}
}

func TestManager_MarkResult_DefaultsToDisableWhenExplicitlyEnabled(t *testing.T) {
	store := &deletingStore{}
	manager := NewManager(store, nil, nil)
	manager.SetConfig(&internalconfig.Config{SDKConfig: internalconfig.SDKConfig{FatalAuthEnabled: internalconfig.FatalAuthModeTrue}})
	auth := registerDeleteTestAuth(t, manager, "auth-default-disable.json")
	reg := registry.GetGlobalRegistry()

	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "gpt-5.3-codex",
		Success:  false,
		Error: &Error{
			Message:    "provider error: invalid request",
			HTTPStatus: 400,
		},
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth %s to remain disabled", auth.ID)
	}
	if !updated.Disabled {
		t.Fatalf("expected auth %s to be disabled by default action", auth.ID)
	}
	if updated.Status != StatusDisabled {
		t.Fatalf("expected status %s, got %s", StatusDisabled, updated.Status)
	}
	if updated.StatusMessage != "provider error: invalid request" {
		t.Fatalf("unexpected status message: %q", updated.StatusMessage)
	}
	if deletedIDs := store.DeletedIDs(); len(deletedIDs) != 0 {
		t.Fatalf("expected no delete calls, got %v", deletedIDs)
	}
	if got := store.saveCount.Load(); got != 2 {
		t.Fatalf("expected register + disable persistence, got %d saves", got)
	}
	if reg.ClientSupportsModel(auth.ID, "gpt-5.3-codex") {
		t.Fatalf("expected registry entry for %s to be removed", auth.ID)
	}
}

func TestManager_MarkResult_SkipsFatalAuthActionWhenDisabled(t *testing.T) {
	store := &deletingStore{}
	manager := NewManager(store, nil, nil)
	manager.SetConfig(&internalconfig.Config{SDKConfig: internalconfig.SDKConfig{
		FatalAuthEnabled: internalconfig.FatalAuthModeFalse,
		FatalAuthAction:  fatalAuthActionDelete,
	}})
	auth := registerDeleteTestAuth(t, manager, "auth-fatal-disabled.json")

	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "gpt-5.3-codex",
		Success:  false,
		Error: &Error{
			Message:    "provider error: invalid request",
			HTTPStatus: 400,
		},
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth %s to remain present", auth.ID)
	}
	if updated.Disabled {
		t.Fatalf("expected auth %s not to be disabled when fatal auth is off", auth.ID)
	}
	if updated.Status != StatusError {
		t.Fatalf("expected status %s, got %s", StatusError, updated.Status)
	}
	if updated.StatusMessage != "provider error: invalid request" {
		t.Fatalf("unexpected status message: %q", updated.StatusMessage)
	}
	if deletedIDs := store.DeletedIDs(); len(deletedIDs) != 0 {
		t.Fatalf("expected no delete calls, got %v", deletedIDs)
	}
	if got := store.saveCount.Load(); got != 2 {
		t.Fatalf("expected register + error persistence, got %d saves", got)
	}
}

func TestManager_MarkResult_AutoDisablesAuthOnUsageLimitReached(t *testing.T) {
	store := &deletingStore{}
	manager := NewManager(store, nil, nil)
	manager.SetConfig(&internalconfig.Config{SDKConfig: internalconfig.SDKConfig{
		FatalAuthEnabled: internalconfig.FatalAuthModeAuto,
		FatalAuthAction:  fatalAuthActionDelete,
	}})
	auth := registerDeleteTestAuth(t, manager, "auth-auto-disable.json")

	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "gpt-5.3-codex",
		Success:  false,
		Error: &Error{
			Message:    "provider error: usage_limit_reached",
			HTTPStatus: http.StatusTooManyRequests,
		},
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth %s to remain present", auth.ID)
	}
	if !updated.Disabled {
		t.Fatalf("expected auth %s to be disabled in auto mode", auth.ID)
	}
	if updated.Status != StatusDisabled {
		t.Fatalf("expected status %s, got %s", StatusDisabled, updated.Status)
	}
	if deletedIDs := store.DeletedIDs(); len(deletedIDs) != 0 {
		t.Fatalf("expected no delete calls, got %v", deletedIDs)
	}
}

func TestManager_MarkResult_AutoDeletesAuthOnUnauthorized(t *testing.T) {
	store := &deletingStore{}
	manager := NewManager(store, nil, nil)
	manager.SetConfig(&internalconfig.Config{SDKConfig: internalconfig.SDKConfig{
		FatalAuthEnabled: internalconfig.FatalAuthModeAuto,
		FatalAuthAction:  fatalAuthActionDisable,
	}})
	auth := registerDeleteTestAuth(t, manager, "auth-auto-delete.json")

	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "gpt-5.3-codex",
		Success:  false,
		Error: &Error{
			Message:    "401 Unauthorized",
			HTTPStatus: http.StatusUnauthorized,
		},
	})

	if _, ok := manager.GetByID(auth.ID); ok {
		t.Fatalf("expected auth %s to be deleted in auto mode", auth.ID)
	}
	deletedIDs := store.DeletedIDs()
	if len(deletedIDs) != 1 || deletedIDs[0] != auth.ID {
		t.Fatalf("unexpected delete calls: %v", deletedIDs)
	}
}

func TestManager_MarkResult_AutoIgnoresOtherErrors(t *testing.T) {
	store := &deletingStore{}
	manager := NewManager(store, nil, nil)
	manager.SetConfig(&internalconfig.Config{SDKConfig: internalconfig.SDKConfig{
		FatalAuthEnabled: internalconfig.FatalAuthModeAuto,
		FatalAuthAction:  fatalAuthActionDelete,
	}})
	auth := registerDeleteTestAuth(t, manager, "auth-auto-ignore.json")
	reg := registry.GetGlobalRegistry()

	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "gpt-5.3-codex",
		Success:  false,
		Error: &Error{
			Message:    "stream error: upstream timeout",
			HTTPStatus: http.StatusGatewayTimeout,
		},
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth %s to remain present", auth.ID)
	}
	if updated.Disabled {
		t.Fatalf("expected auth %s not to be disabled in auto ignore path", auth.ID)
	}
	if updated.Status != "" {
		t.Fatalf("expected auth status to remain unchanged, got %q", updated.Status)
	}
	if updated.StatusMessage != "" {
		t.Fatalf("expected auth status message to remain empty, got %q", updated.StatusMessage)
	}
	if got := store.saveCount.Load(); got != 1 {
		t.Fatalf("expected only register persistence, got %d saves", got)
	}
	if deletedIDs := store.DeletedIDs(); len(deletedIDs) != 0 {
		t.Fatalf("expected no delete calls, got %v", deletedIDs)
	}
	if !reg.ClientSupportsModel(auth.ID, "gpt-5.3-codex") {
		t.Fatalf("expected registry entry for %s to stay active", auth.ID)
	}
}

func TestManager_MarkResult_DoesNotApplyFatalAuthActionWithoutConfig(t *testing.T) {
	store := &deletingStore{}
	manager := NewManager(store, nil, nil)
	auth := registerDeleteTestAuth(t, manager, "auth-no-fatal-config.json")

	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "gpt-5.3-codex",
		Success:  false,
		Error: &Error{
			Message:    "provider error: invalid request",
			HTTPStatus: 400,
		},
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth %s to remain present", auth.ID)
	}
	if updated.Disabled {
		t.Fatalf("expected auth %s not to be disabled without fatal auth config", auth.ID)
	}
	if updated.Status != StatusError {
		t.Fatalf("expected status %s, got %s", StatusError, updated.Status)
	}
	if deletedIDs := store.DeletedIDs(); len(deletedIDs) != 0 {
		t.Fatalf("expected no delete calls, got %v", deletedIDs)
	}
}
