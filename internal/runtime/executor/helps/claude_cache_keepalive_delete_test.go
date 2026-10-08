package helps

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func deleteKeepaliveSession(t *testing.T, k *ClaudeCacheKeepalive, id string) (bool, error) {
	t.Helper()
	deleter, ok := any(k).(interface{ DeleteSession(string) (bool, error) })
	if !ok {
		t.Fatal("keeper does not support session deletion")
	}
	return deleter.DeleteSession(id)
}

func TestClaudeCacheKeepaliveDeleteRestoresWithoutSession(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1000000, 0)
	k := newPersistentTestKeeper(t, dir, &now, nil, nil)
	k.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
	id := k.LogSnapshot().SessionDetails[0].ID
	k.SetDisabledSessions([]string{id})
	before := k.LogSnapshot().Events
	if ok, err := deleteKeepaliveSession(t, k, id); !ok || err != nil {
		t.Fatalf("delete: ok=%v err=%v", ok, err)
	}
	if got := k.LogSnapshot(); got.Sessions != 0 || len(got.SessionDetails) != 0 || !reflect.DeepEqual(got.Events, before) {
		t.Fatal("deletion retained a session or changed history")
	}
	if ok, err := deleteKeepaliveSession(t, k, id); ok || err != nil {
		t.Fatal("missing deletion did not return false")
	}
	restored := newPersistentTestKeeper(t, dir, &now, nil, nil)
	if got := restored.LogSnapshot(); got.Sessions != 0 || !reflect.DeepEqual(got.Events, before) {
		t.Fatal("restart resurrected deleted session or lost history")
	}
	restored.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
	if got := restored.LogSnapshot(); got.Sessions != 1 || got.SessionDetails[0].State != "active" {
		t.Fatal("future chat did not create an active session")
	}
}

func TestClaudeCacheKeepaliveDeleteDisabledOnly(t *testing.T) {
	k := NewClaudeCacheKeepalive(nil)
	defer k.Close()
	id := strings.Repeat("a", 64)
	k.SetDisabledSessions([]string{id, strings.Repeat("b", 64)})
	if ok, err := deleteKeepaliveSession(t, k, id); !ok || err != nil {
		t.Fatal("disabled-only deletion failed")
	}
	if got := k.LogSnapshot(); got.DisabledSessions != 1 || len(got.SessionDetails) != 1 || got.SessionDetails[0].ID == id {
		t.Fatal("deleted wrong preferences")
	}
}

func TestClaudeCacheKeepaliveDeletePendingChat(t *testing.T) {
	for _, withSnapshot := range []bool{false, true} {
		for _, newFirst := range []bool{false, true} {
			t.Run(strings.Join([]string{map[bool]string{true: "retained", false: "pending"}[withSnapshot], map[bool]string{true: "new-first", false: "old-first"}[newFirst]}, "/"), func(t *testing.T) {
				now := time.Unix(1000000, 0)
				calls := 0
				k := newPersistentTestKeeper(t, t.TempDir(), &now, func(context.Context, ClaudeCacheSnapshot) (int64, error) { calls++; return 100, nil }, nil)
				req := keepaliveTestRequest(t)
				body := []byte(keepaliveTestBody)
				idBytes := sha256.Sum256([]byte("a\x00" + req.URL.String() + "\x00claude-sonnet-5\x00s"))
				id := hex.EncodeToString(idBytes[:])
				if withSnapshot {
					k.Begin("a", "s", req, body)(true)
				}
				oldDone := k.Begin("a", "s", req, body)
				if ok, err := deleteKeepaliveSession(t, k, id); !ok || err != nil {
					t.Fatalf("pending deletion failed: %v %v", ok, err)
				}
				if k.busy[idBytes] != 1 || k.pending[idBytes] != nil {
					t.Fatal("deleted in-flight count or retained pending completion")
				}
				newBody := bytes.ReplaceAll(body, []byte("question"), []byte("new question"))
				newDone := k.Begin("a", "s", req, newBody)
				if k.busy[idBytes] != 2 {
					t.Fatal("new request lost the old in-flight count")
				}
				if newFirst {
					newDone(true)
					if k.busy[idBytes] != 1 {
						t.Fatal("new completion lost old busy count")
					}
					k.ReplayDue(context.Background(), now.Add(50*time.Minute))
					if calls != 0 {
						t.Fatal("replayed while old request was still in flight")
					}
					oldDone(true)
				} else {
					oldDone(true)
					if k.busy[idBytes] != 1 || k.LogSnapshot().Sessions != 0 {
						t.Fatal("old completion resurrected deleted session or lost new busy count")
					}
					newDone(true)
				}
				if len(k.busy) != 0 || len(k.pending) != 0 || k.LogSnapshot().SessionDetails[0].LastPrompt != "new question" {
					t.Fatal("overlapping completion lost the new snapshot")
				}
				k.ReplayDue(context.Background(), now.Add(50*time.Minute))
				if calls != 1 {
					t.Fatal("new snapshot could not renew after all chats finished")
				}
			})
		}
	}
}

func TestClaudeCacheKeepaliveDeleteCancelsReplay(t *testing.T) {
	now := time.Unix(1000000, 0)
	started, done := make(chan context.Context, 1), make(chan struct{})
	dir := t.TempDir()
	k := newPersistentTestKeeper(t, dir, &now, func(ctx context.Context, _ ClaudeCacheSnapshot) (int64, error) {
		started <- ctx
		<-ctx.Done()
		return 0, ctx.Err()
	}, nil)
	k.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
	id := k.LogSnapshot().SessionDetails[0].ID
	go func() { k.ReplayDue(context.Background(), now.Add(50*time.Minute)); close(done) }()
	ctx := <-started
	if ok, err := deleteKeepaliveSession(t, k, id); !ok || err != nil {
		t.Fatal("delete failed")
	}
	if ctx.Err() == nil {
		t.Fatal("delete did not cancel active replay")
	}
	<-done
	if k.LogSnapshot().Sessions != 0 {
		t.Fatal("replay completion resurrected deleted session")
	}
	restored := newPersistentTestKeeper(t, dir, &now, nil, nil)
	if restored.LogSnapshot().Sessions != 0 {
		t.Fatal("replay completion persisted deleted session")
	}
}

func TestClaudeCacheKeepaliveDeleteSaveFailureRollsBack(t *testing.T) {
	now := time.Unix(1000000, 0)
	started, release, done := make(chan context.Context, 1), make(chan struct{}), make(chan struct{})
	dir := t.TempDir()
	k := newPersistentTestKeeper(t, dir, &now, func(ctx context.Context, _ ClaudeCacheSnapshot) (int64, error) {
		started <- ctx
		<-release
		return 100, nil
	}, nil)
	k.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
	id := k.LogSnapshot().SessionDetails[0].ID
	go func() { k.ReplayDue(context.Background(), now.Add(50*time.Minute)); close(done) }()
	ctx := <-started
	// A nonempty destination directory deterministically rejects atomic replacement.
	path := filepath.Join(dir, claudeCachePersistenceFile)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "occupied"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	k.mu.Lock()
	k.disabled[id] = true
	k.mu.Unlock()
	finish := k.Begin("a", "other", keepaliveTestRequest(t), []byte(keepaliveTestBody))
	before := k.LogSnapshot()
	if ok, err := deleteKeepaliveSession(t, k, id); ok || err == nil {
		t.Fatal("save failure reported successful deletion")
	}
	if !reflect.DeepEqual(k.LogSnapshot(), before) || ctx.Err() != nil {
		t.Fatal("save failure removed state or cancelled replay")
	}
	close(release)
	<-done
	finish(false)
}

func TestClaudeCacheKeepaliveDeleteRejectsBlockedPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, claudeCachePersistenceFile)
	raw := []byte("corrupt fixture")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	k := NewClaudeCacheKeepalive(nil)
	defer k.Close()
	k.SetEnabled(true)
	if err := k.SetPersistenceDir(dir); err == nil {
		t.Fatal("accepted corrupt file")
	}
	k.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
	id := k.LogSnapshot().SessionDetails[0].ID
	before := k.LogSnapshot()
	if ok, err := deleteKeepaliveSession(t, k, id); ok || err == nil {
		t.Fatal("blocked persistence silently deleted session")
	}
	if !reflect.DeepEqual(k.LogSnapshot(), before) {
		t.Fatal("blocked persistence mutated session")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatal("overwrote corrupt persistence")
	}
}
