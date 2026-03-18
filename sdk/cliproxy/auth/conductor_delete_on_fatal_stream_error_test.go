package auth

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

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

func TestManager_MarkResult_DeletesAuthOnFatalKeywords(t *testing.T) {
	tests := []struct {
		name    string
		message string
	}{
		{name: "exact_disconnect_error", message: fatalCodexResponseCompletedDisconnectMessage},
		{name: "usage_limit_reached", message: "provider error: usage_limit_reached"},
		{name: "stream_disconnect_keyword", message: "upstream stream disconnect during completion"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			store := &deletingStore{}
			manager := NewManager(store, nil, nil)
			auth := registerDeleteTestAuth(t, manager, tc.name+".json")
			reg := registry.GetGlobalRegistry()

			manager.MarkResult(context.Background(), Result{
				AuthID:   auth.ID,
				Provider: auth.Provider,
				Model:    "gpt-5.3-codex",
				Success:  false,
				Error: &Error{
					Message:    tc.message,
					HTTPStatus: 408,
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

func TestManager_MarkResult_DoesNotDeleteAuthOnOtherErrors(t *testing.T) {
	store := &deletingStore{}
	manager := NewManager(store, nil, nil)
	auth := registerDeleteTestAuth(t, manager, "auth-non-fatal.json")

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

	if _, ok := manager.GetByID(auth.ID); !ok {
		t.Fatalf("expected auth %s to remain", auth.ID)
	}
	if got := store.saveCount.Load(); got != 2 {
		t.Fatalf("expected register + failure persistence, got %d saves", got)
	}
	if deletedIDs := store.DeletedIDs(); len(deletedIDs) != 0 {
		t.Fatalf("expected no delete calls, got %v", deletedIDs)
	}
}
