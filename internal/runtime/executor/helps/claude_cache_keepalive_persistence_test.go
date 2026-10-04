package helps

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func attachKeepalivePersistence(t *testing.T, k *ClaudeCacheKeepalive, dir string) {
	t.Helper()
	persistence, ok := any(k).(interface{ SetPersistenceDir(string) error })
	if !ok {
		t.Fatal("keeper does not support persistence")
	}
	if err := persistence.SetPersistenceDir(dir); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeCacheKeepalivePersistenceRestart(t *testing.T) {
	for _, closeFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "replacement", true: "close"}[closeFirst], func(t *testing.T) {
			dir := t.TempDir()
			now := time.Unix(1000000, 0)
			k := NewClaudeCacheKeepalive(nil)
			defer k.Close()
			k.now = func() time.Time { return now }
			k.SetEnabled(true)
			attachKeepalivePersistence(t, k, dir)
			k.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
			before := k.LogSnapshot()
			if closeFirst {
				k.Close()
				k.Close()
			}
			calls := 0
			restored := NewClaudeCacheKeepalive(func(_ context.Context, snapshot ClaudeCacheSnapshot) (int64, error) {
				calls++
				if !bytes.Equal(snapshot.Body, []byte(keepaliveTestBody)) || snapshot.Headers.Get("Authorization") != "" {
					t.Fatal("restored wrong body or credentials")
				}
				return 123, nil
			})
			defer restored.Close()
			restored.now = func() time.Time { return now }
			restored.SetEnabled(true)
			attachKeepalivePersistence(t, restored, dir)
			after := restored.LogSnapshot()
			if after.Sessions != 1 || !reflect.DeepEqual(before.Events, after.Events) || after.SessionDetails[0].ID != before.SessionDetails[0].ID {
				t.Fatalf("restart lost state: %+v", after)
			}
			now = now.Add(50 * time.Minute)
			restored.ReplayDue(context.Background(), now)
			if calls != 1 || restored.LogSnapshot().Events[0].ID != before.Events[0].ID+1 {
				t.Fatal("restart did not renew or continue event IDs")
			}
			for _, name := range []string{".claude-cache-keepalive.bin", ".claude-cache-keepalive.key"} {
				path := filepath.Join(dir, name)
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(raw, []byte("unchanged prefix")) || bytes.Contains(raw, []byte("Bearer secret")) {
					t.Fatal("plaintext private material on disk")
				}
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0600 {
					t.Fatalf("file mode = %o", info.Mode().Perm())
				}
			}
		})
	}
}

func newPersistentTestKeeper(t *testing.T, dir string, now *time.Time, replay ClaudeCacheReplay, disabled []string) *ClaudeCacheKeepalive {
	t.Helper()
	k := NewClaudeCacheKeepalive(replay)
	t.Cleanup(k.Close)
	k.now = func() time.Time { return *now }
	k.SetEnabled(true)
	k.SetDisabledSessions(disabled)
	attachKeepalivePersistence(t, k, dir)
	return k
}

func TestClaudeCacheKeepalivePersistencePendingAndLateChat(t *testing.T) {
	for _, outcome := range []string{"pending", "failed", "late-success"} {
		t.Run(outcome, func(t *testing.T) {
			dir := t.TempDir()
			now := time.Unix(1000000, 0)
			k := newPersistentTestKeeper(t, dir, &now, nil, nil)
			k.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
			replacement := bytes.ReplaceAll([]byte(keepaliveTestBody), []byte("question"), []byte("unfinished private chat"))
			finish := k.Begin("a", "s", keepaliveTestRequest(t), replacement)
			if outcome == "failed" {
				finish(false)
			}
			if outcome == "late-success" {
				k.Close()
				finish(true)
			}
			restored := newPersistentTestKeeper(t, dir, &now, nil, nil)
			if snapshot := restored.LogSnapshot(); snapshot.Sessions != 1 || snapshot.SessionDetails[0].LastPrompt != "question" || len(restored.pending) != 0 || len(restored.busy) != 0 {
				t.Fatalf("persisted unfinished replacement: %+v", snapshot)
			}
		})
	}
}

func TestClaudeCacheKeepalivePersistenceExpiryAndDisable(t *testing.T) {
	for _, mode := range []string{"expired", "disabled", "disabled-before-attach"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			now := time.Unix(1000000, 0)
			k := newPersistentTestKeeper(t, dir, &now, nil, nil)
			k.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
			originalID := k.LogSnapshot().Events[0].ID
			if mode == "expired" {
				now = now.Add(time.Hour)
			}
			if mode == "disabled" {
				k.SetEnabled(false)
			}
			restored := NewClaudeCacheKeepalive(nil)
			defer restored.Close()
			restored.now = func() time.Time { return now }
			restored.SetEnabled(mode != "disabled-before-attach")
			attachKeepalivePersistence(t, restored, dir)
			if snapshot := restored.LogSnapshot(); snapshot.Sessions != 0 || len(snapshot.Events) == 0 || snapshot.Events[len(snapshot.Events)-2].ID > originalID {
				t.Fatalf("lost history or restored invalid cache: %+v", snapshot)
			}
			if mode == "disabled-before-attach" {
				final := newPersistentTestKeeper(t, dir, &now, nil, nil)
				if final.LogSnapshot().Sessions != 0 {
					t.Fatal("disabled startup did not purge persisted snapshots")
				}
			}
		})
	}
}

func TestClaudeCacheKeepalivePersistencePausePreferencesAndDuplicateLoad(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1000000, 0)
	k := newPersistentTestKeeper(t, dir, &now, func(context.Context, ClaudeCacheSnapshot) (int64, error) { return 0, nil }, nil)
	k.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
	now = now.Add(50 * time.Minute)
	k.ReplayDue(context.Background(), now)
	id := k.LogSnapshot().SessionDetails[0].ID
	restored := newPersistentTestKeeper(t, dir, &now, func(context.Context, ClaudeCacheSnapshot) (int64, error) { return 0, nil }, []string{id})
	if restored.LogSnapshot().SessionDetails[0].State != "disabled" {
		t.Fatal("restored cache ignored configured disabled preference")
	}
	restored.SetDisabledSessions(nil)
	// Unchanged preference leaves the persisted miss count intact across restart.
	resumed := newPersistentTestKeeper(t, dir, &now, func(context.Context, ClaudeCacheSnapshot) (int64, error) { return 0, nil }, nil)
	now = now.Add(50 * time.Minute)
	resumed.ReplayDue(context.Background(), now)
	// Explicit operator resume resets misses, so its first miss must not pause.
	if resumed.LogSnapshot().PausedSessions != 0 {
		t.Fatal("manual resume did not reset misses")
	}
	now = now.Add(50 * time.Minute)
	resumed.ReplayDue(context.Background(), now)
	if resumed.LogSnapshot().PausedSessions != 1 {
		t.Fatal("repeated misses did not pause")
	}
	paused := newPersistentTestKeeper(t, dir, &now, nil, nil)
	before := paused.LogSnapshot()
	if before.PausedSessions != 1 {
		t.Fatal("pause was not restored")
	}
	attachKeepalivePersistence(t, paused, filepath.Join(dir, "."))
	if !reflect.DeepEqual(before, paused.LogSnapshot()) {
		t.Fatal("duplicate attachment reset state")
	}
}

func TestClaudeCacheKeepalivePersistenceMissCountAndHistoryBound(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1000000, 0)
	k := newPersistentTestKeeper(t, dir, &now, func(context.Context, ClaudeCacheSnapshot) (int64, error) { return 0, nil }, nil)
	for i := 0; i < 220; i++ {
		k.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
	}
	now = now.Add(50 * time.Minute)
	k.ReplayDue(context.Background(), now)
	before := k.LogSnapshot()
	restored := newPersistentTestKeeper(t, dir, &now, func(context.Context, ClaudeCacheSnapshot) (int64, error) { return 0, nil }, nil)
	if !reflect.DeepEqual(before.Events, restored.LogSnapshot().Events) || len(restored.LogSnapshot().Events) != 200 {
		t.Fatal("newest 200 events not restored")
	}
	now = now.Add(50 * time.Minute)
	restored.ReplayDue(context.Background(), now)
	if restored.LogSnapshot().PausedSessions != 1 {
		t.Fatal("restart lost first miss count")
	}
}

func TestClaudeCacheKeepalivePersistenceCloseCancelsWithoutDirtying(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1000000, 0)
	started, done := make(chan struct{}), make(chan struct{})
	k := newPersistentTestKeeper(t, dir, &now, func(ctx context.Context, _ ClaudeCacheSnapshot) (int64, error) {
		close(started)
		<-ctx.Done()
		return 0, ctx.Err()
	}, nil)
	k.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
	before, err := os.ReadFile(filepath.Join(dir, ".claude-cache-keepalive.bin"))
	if err != nil {
		t.Fatal(err)
	}
	go func() { k.ReplayDue(context.Background(), now.Add(50*time.Minute)); close(done) }()
	<-started
	k.Close()
	<-done
	k.Close()
	after, err := os.ReadFile(filepath.Join(dir, ".claude-cache-keepalive.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("shutdown replay completion dirtied persisted snapshot")
	}
	restored := newPersistentTestKeeper(t, dir, &now, nil, nil)
	if snapshot := restored.LogSnapshot(); snapshot.Sessions != 1 || snapshot.PausedSessions != 0 {
		t.Fatalf("shutdown lost or paused successful cache: %+v", snapshot)
	}
}

func TestClaudeCacheKeepalivePersistenceCorruptRetained(t *testing.T) {
	for _, name := range []string{".claude-cache-keepalive.bin", ".claude-cache-keepalive.key"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			now := time.Unix(1000000, 0)
			k := newPersistentTestKeeper(t, dir, &now, nil, nil)
			k.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
			if err := os.WriteFile(filepath.Join(dir, name), []byte("corrupt private file"), 0600); err != nil {
				t.Fatal(err)
			}
			rawBin, _ := os.ReadFile(filepath.Join(dir, ".claude-cache-keepalive.bin"))
			rawKey, _ := os.ReadFile(filepath.Join(dir, ".claude-cache-keepalive.key"))
			restored := NewClaudeCacheKeepalive(nil)
			defer restored.Close()
			restored.now = func() time.Time { return now }
			restored.SetEnabled(true)
			persistence, ok := any(restored).(interface{ SetPersistenceDir(string) error })
			if !ok {
				t.Fatal("missing persistence")
			}
			if persistence.SetPersistenceDir(dir) == nil {
				t.Fatal("accepted corrupt persistence")
			}
			if restored.LogSnapshot().Sessions != 0 {
				t.Fatal("restored corrupt cache")
			}
			restored.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
			restored.SetEnabled(false)
			restored.Close()
			afterBin, _ := os.ReadFile(filepath.Join(dir, ".claude-cache-keepalive.bin"))
			afterKey, _ := os.ReadFile(filepath.Join(dir, ".claude-cache-keepalive.key"))
			if !bytes.Equal(rawBin, afterBin) || !bytes.Equal(rawKey, afterKey) {
				t.Fatal("overwrote corrupt data or key")
			}
		})
	}
}

func TestClaudeCacheKeepalivePersistenceDetachAndNoReload(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1000000, 0)
	k := newPersistentTestKeeper(t, dir, &now, nil, nil)
	k.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
	path := filepath.Join(dir, claudeCachePersistenceFile)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	attachKeepalivePersistence(t, k, filepath.Join(dir, "."))
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || k.LogSnapshot().Sessions != 1 {
		t.Fatal("same directory reloaded or rewrote saved state")
	}
	attachKeepalivePersistence(t, k, "")
	k.SetEnabled(false)
	after, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("detached keeper wrote persistence")
	}
}

func TestClaudeCacheKeepalivePersistenceRejectsInvalidState(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*claudeCachePersistedState)
	}{
		{"version", func(s *claudeCachePersistedState) { s.Version++ }},
		{"session capacity", func(s *claudeCachePersistedState) {
			for len(s.Sessions) <= claudeCacheMaxSessions {
				s.Sessions = append(s.Sessions, s.Sessions[0])
			}
		}},
		{"body capacity", func(s *claudeCachePersistedState) {
			s.Sessions[0].Snapshot.Body = bytes.Repeat([]byte(" "), claudeCacheMaxBody+1)
		}},
		{"ineligible", func(s *claudeCachePersistedState) { s.Sessions[0].Snapshot.Body = []byte(`{"max_tokens":100}`) }},
		{"ttl", func(s *claudeCachePersistedState) { s.Sessions[0].TTL = 2 * time.Hour }},
		{"interval", func(s *claudeCachePersistedState) { s.Sessions[0].Interval = time.Minute }},
		{"future anchor", func(s *claudeCachePersistedState) { s.Sessions[0].Anchor = s.Sessions[0].Anchor.Add(time.Hour) }},
		{"duplicate IDs", func(s *claudeCachePersistedState) { s.Sessions = append(s.Sessions, s.Sessions[0]) }},
		{"miss count", func(s *claudeCachePersistedState) { s.Sessions[0].Misses = 100 }},
		{"event capacity", func(s *claudeCachePersistedState) {
			for len(s.Events) <= claudeCacheLogCapacity {
				s.Events = append(s.Events, s.Events[0])
			}
		}},
		{"event order", func(s *claudeCachePersistedState) { s.Events[1].ID = s.Events[0].ID }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			now := time.Unix(1000000, 0)
			k := newPersistentTestKeeper(t, dir, &now, nil, nil)
			k.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
			path := filepath.Join(dir, claudeCachePersistenceFile)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			prefix := len(claudeCachePersistenceMagic)
			aead := k.persistence.aead
			raw, err := aead.Open(nil, data[prefix:prefix+aead.NonceSize()], data[prefix+aead.NonceSize():], []byte(claudeCachePersistenceMagic))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(raw, []byte("Bearer secret")) {
				t.Fatal("encrypted snapshot retained credential headers")
			}
			var state claudeCachePersistedState
			if err = json.Unmarshal(raw, &state); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&state)
			raw, err = json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			nonce := make([]byte, aead.NonceSize())
			if _, err = rand.Read(nonce); err != nil {
				t.Fatal(err)
			}
			invalid := aead.Seal(append([]byte(claudeCachePersistenceMagic), nonce...), nonce, raw, []byte(claudeCachePersistenceMagic))
			if err = os.WriteFile(path, invalid, 0600); err != nil {
				t.Fatal(err)
			}
			restored := NewClaudeCacheKeepalive(nil)
			defer restored.Close()
			restored.now = func() time.Time { return now }
			restored.SetEnabled(true)
			if restored.SetPersistenceDir(dir) == nil || restored.LogSnapshot().Sessions != 0 {
				t.Fatal("accepted invalid persisted state")
			}
			restored.SetEnabled(false)
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(invalid, after) {
				t.Fatal("invalid file was overwritten")
			}
		})
	}
}

func TestClaudeCacheKeepalivePersistenceMissingKeyRetained(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1000000, 0)
	k := newPersistentTestKeeper(t, dir, &now, nil, nil)
	k.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
	if err := os.Remove(filepath.Join(dir, claudeCachePersistenceKey)); err != nil {
		t.Fatal(err)
	}
	restored := NewClaudeCacheKeepalive(nil)
	defer restored.Close()
	restored.SetEnabled(true)
	if restored.SetPersistenceDir(dir) == nil {
		t.Fatal("accepted data without encryption key")
	}
	restored.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
	if _, err := os.Stat(filepath.Join(dir, claudeCachePersistenceKey)); !os.IsNotExist(err) {
		t.Fatal("generated a replacement key for retained encrypted data")
	}
}

func TestClaudeCacheKeepaliveRunReplaysImmediately(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1000000, 0)
	k := newPersistentTestKeeper(t, dir, &now, nil, nil)
	k.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
	now = now.Add(50 * time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	restored := newPersistentTestKeeper(t, dir, &now, func(context.Context, ClaudeCacheSnapshot) (int64, error) { calls++; cancel(); return 100, nil }, nil)
	restored.Run(ctx)
	if calls != 1 {
		t.Fatal("startup did not replay restored due snapshot")
	}
}

func TestClaudeCacheKeepalivePersistenceBlockedDirectoryReportsError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, claudeCachePersistenceFile), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	k := NewClaudeCacheKeepalive(nil)
	defer k.Close()
	k.SetEnabled(true)
	if k.SetPersistenceDir(dir) == nil {
		t.Fatal("initial load accepted corrupt file")
	}
	if k.SetPersistenceDir(filepath.Join(dir, ".")) == nil {
		t.Fatal("blocked directory silently reported success")
	}
}

func TestClaudeCacheKeepalivePersistenceLiveExpiryPurgesDisk(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1000000, 0)
	k := newPersistentTestKeeper(t, dir, &now, func(context.Context, ClaudeCacheSnapshot) (int64, error) {
		t.Fatal("expired cache replayed")
		return 0, nil
	}, nil)
	k.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
	now = now.Add(time.Hour)
	k.ReplayDue(context.Background(), now)
	data, err := os.ReadFile(filepath.Join(dir, claudeCachePersistenceFile))
	if err != nil {
		t.Fatal(err)
	}
	aead := k.persistence.aead
	prefix := len(claudeCachePersistenceMagic)
	raw, err := aead.Open(nil, data[prefix:prefix+aead.NonceSize()], data[prefix+aead.NonceSize():], []byte(claudeCachePersistenceMagic))
	if err != nil {
		t.Fatal(err)
	}
	var state claudeCachePersistedState
	if err = json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Sessions) != 0 || state.Events[len(state.Events)-1].Outcome != "expired" {
		t.Fatal("live expiry retained snapshot on disk or lost event")
	}
}

func TestClaudeCacheKeepalivePersistenceRunCancellationPreservesSnapshot(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1000000, 0)
	started, done := make(chan struct{}), make(chan struct{})
	k := newPersistentTestKeeper(t, dir, &now, func(ctx context.Context, _ ClaudeCacheSnapshot) (int64, error) {
		close(started)
		<-ctx.Done()
		return 0, ctx.Err()
	}, nil)
	k.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
	path := filepath.Join(dir, ".claude-cache-keepalive.bin")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(50 * time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { k.Run(ctx); close(done) }()
	<-started
	cancel()
	<-done
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("service cancellation persisted a shutdown pause or advanced TTL")
	}
	restored := newPersistentTestKeeper(t, dir, &now, nil, nil)
	if state := restored.LogSnapshot(); state.Sessions != 1 || state.PausedSessions != 0 {
		t.Fatal("service cancellation lost or paused the last successful snapshot")
	}
}
