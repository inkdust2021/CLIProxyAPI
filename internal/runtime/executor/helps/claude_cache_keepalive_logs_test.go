package helps

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestClaudeCacheKeepaliveLogsOutcomesAndPrivacy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		read   int64
		err    error
		want   string
		paused bool
	}{
		{"hit", 42, nil, "renewed", false},
		{"miss", 0, nil, "cache_miss", false},
		{"failure", 0, errors.New("Bearer secret: private upstream body"), "failed", true},
		{"http failure", 0, homeStatusErr{code: 429, msg: "Bearer secret: private upstream body"}, "failed", true},
		{"cancel", 0, context.Canceled, "cancelled", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Unix(1000000, 0)
			k := NewClaudeCacheKeepalive(func(context.Context, ClaudeCacheSnapshot) (int64, error) {
				now = now.Add(25 * time.Millisecond)
				return tc.read, tc.err
			})
			defer k.Close()
			k.now = func() time.Time { return now }
			k.SetEnabled(true)
			k.Begin("private-account", "private-session", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
			now = now.Add(50 * time.Minute)
			k.ReplayDue(context.Background(), now)
			snapshot := k.LogSnapshot()
			if !snapshot.Enabled || snapshot.Sessions != 1 || len(snapshot.Events) < 2 {
				t.Fatalf("unexpected state: %+v", snapshot)
			}
			event := snapshot.Events[0]
			if event.Outcome != tc.want || event.CacheReadTokens != tc.read || event.DurationMS != 25 || event.Paused != tc.paused {
				t.Fatalf("unexpected replay event: %+v", event)
			}
			if tc.name == "http failure" && event.StatusCode != 429 {
				t.Fatal("upstream HTTP status not recorded")
			}
			raw, errJSON := json.Marshal(snapshot)
			if errJSON != nil {
				t.Fatal(errJSON)
			}
			for _, secret := range []string{"private-account", "private-session", "Bearer secret", "private upstream body", "unchanged prefix", "example.test"} {
				if strings.Contains(string(raw), secret) {
					t.Fatalf("log contains private material: %s", secret)
				}
			}
			snapshot.Events[0].Outcome = "mutated"
			if k.LogSnapshot().Events[0].Outcome != tc.want {
				t.Fatal("snapshot mutates internal log")
			}
			k.SetEnabled(false)
			if next := k.LogSnapshot(); next.Enabled || next.Sessions != 0 || len(next.Events) <= len(snapshot.Events) {
				t.Fatal("disabling lost log history or retained sessions")
			}
		})
	}
}

func TestClaudeCacheKeepaliveLogsCancelledAfterDisable(t *testing.T) {
	var k *ClaudeCacheKeepalive
	k = NewClaudeCacheKeepalive(func(ctx context.Context, _ ClaudeCacheSnapshot) (int64, error) {
		k.SetEnabled(false)
		if ctx.Err() == nil {
			t.Fatal("disable did not cancel replay")
		}
		return 100, nil
	})
	defer k.Close()
	now := time.Unix(1000000, 0)
	k.now = func() time.Time { return now }
	k.SetEnabled(true)
	k.Begin("account", "session", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
	k.ReplayDue(context.Background(), now.Add(50*time.Minute))
	snapshot := k.LogSnapshot()
	if snapshot.Enabled || snapshot.Sessions != 0 || snapshot.Events[0].Outcome != "cancelled" || snapshot.Events[0].Paused {
		t.Fatalf("cancelled replay restored cleared state or lost its history: %+v", snapshot)
	}
}

func TestClaudeCacheKeepaliveLogsBoundsAndExpiry(t *testing.T) {
	k := NewClaudeCacheKeepalive(nil)
	defer k.Close()
	for i := 0; i < 300; i++ {
		k.SetEnabled(i%2 == 0)
	}
	snapshot := k.LogSnapshot()
	if len(snapshot.Events) != 200 || snapshot.Capacity != 200 {
		t.Fatalf("unbounded history: %+v", snapshot)
	}
	if snapshot.Events[0].ID <= snapshot.Events[199].ID {
		t.Fatal("events are not newest first")
	}
	if k.LogSnapshot().Events[0].ID != snapshot.Events[0].ID {
		t.Fatal("read consumed history")
	}
	k.replay = func(context.Context, ClaudeCacheSnapshot) (int64, error) { return 0, nil }
	now := time.Unix(1000000, 0)
	k.now = func() time.Time { return now }
	k.SetEnabled(true)
	k.Begin("account", "session", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
	k.ReplayDue(context.Background(), now.Add(time.Hour))
	if next := k.LogSnapshot(); next.Sessions != 0 || next.Events[0].Outcome != "expired" {
		t.Fatal("expiration not logged")
	}
}

func TestClaudeCacheKeepaliveLogsRepeatedMissPause(t *testing.T) {
	k := NewClaudeCacheKeepalive(func(context.Context, ClaudeCacheSnapshot) (int64, error) { return 0, nil })
	defer k.Close()
	now := time.Unix(1000000, 0)
	k.now = func() time.Time { return now }
	k.SetEnabled(true)
	k.Begin("account", "session", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
	k.ReplayDue(context.Background(), now.Add(50*time.Minute))
	k.ReplayDue(context.Background(), now.Add(100*time.Minute))
	if snapshot := k.LogSnapshot(); snapshot.PausedSessions != 1 || !snapshot.Events[0].Paused || snapshot.Events[0].Outcome != "cache_miss" {
		t.Fatalf("missing pause information: %+v", snapshot)
	}
}
