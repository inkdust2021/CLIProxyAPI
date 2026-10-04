package helps

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

const keepaliveTestBody = `{"model":"claude-sonnet-5","max_tokens":100,"stream":true,"system":[{"type":"text","text":"unchanged prefix","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":"question"}],"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}}`

func keepaliveTestRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://example.test/v1/messages?beta=true", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Anthropic-Beta", "extended-cache-ttl-2025-04-11")
	return req
}

func TestClaudeCacheKeepaliveDisabledAndTTL(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		interval   time.Duration
	}{
		{"1h", keepaliveTestBody, 50 * time.Minute},
		{"5m", `{"model":"claude-sonnet-5","max_tokens":100,"cache_control":{"type":"ephemeral"},"messages":[{"role":"user","content":"q"}]}`, 4 * time.Minute},
		{"mixed", `{"model":"claude-sonnet-5","max_tokens":100,"cache_control":{"type":"ephemeral","ttl":"1h"},"system":[{"type":"text","text":"q","cache_control":{"type":"ephemeral"}}]}`, 4 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			k := NewClaudeCacheKeepalive(func(ctx context.Context, snap ClaudeCacheSnapshot) (int64, error) {
				calls++
				if snap.AuthID != "account-a" || snap.Headers.Get("Anthropic-Beta") == "" {
					t.Fatal("replay lost account or beta headers")
				}
				if snap.Headers.Get("Authorization") != "" {
					t.Fatal("snapshot retained an access token")
				}
				return 1000, nil
			})
			defer k.Close()
			now := time.Unix(1000000, 0)
			k.now = func() time.Time { return now }
			k.Begin("account-a", "session", keepaliveTestRequest(t), []byte(tc.body))(true)
			k.ReplayDue(context.Background(), now.Add(tc.interval))
			if calls != 0 {
				t.Fatal("disabled keepalive sent a request")
			}
			k.SetEnabled(true)
			finish := k.Begin("account-a", "session", keepaliveTestRequest(t), []byte(tc.body))
			k.ReplayDue(context.Background(), now.Add(tc.interval))
			if calls != 0 {
				t.Fatal("replayed an unfinished chat")
			}
			finish(true)
			k.ReplayDue(context.Background(), now.Add(tc.interval-time.Second))
			if calls != 0 {
				t.Fatal("replayed before renewal was due")
			}
			k.ReplayDue(context.Background(), now.Add(tc.interval))
			if calls != 1 {
				t.Fatalf("due replay count = %d", calls)
			}
			k.ReplayDue(context.Background(), now.Add(tc.interval+time.Second))
			if calls != 1 {
				t.Fatal("renewed the same snapshot twice")
			}
		})
	}
}

func TestClaudeCacheKeepalivePreservesPrefix(t *testing.T) {
	got, err := ClaudeCacheReplayBody([]byte(keepaliveTestBody))
	if err != nil {
		t.Fatal(err)
	}
	before := gjson.Parse(keepaliveTestBody).Value().(map[string]any)
	after := gjson.ParseBytes(got).Value().(map[string]any)
	delete(before, "max_tokens")
	delete(after, "max_tokens")
	if !reflect.DeepEqual(before, after) || gjson.GetBytes(got, "max_tokens").Int() != 1 {
		t.Fatalf("replay changed the cache prefix: %s", got)
	}
}

func TestClaudeCacheKeepaliveRetainsLastGoodSnapshot(t *testing.T) {
	for _, outcome := range []string{"failed-or-no-cache-usage", "ineligible"} {
		t.Run(outcome, func(t *testing.T) {
			now := time.Unix(1000000, 0)
			calls := 0
			k := NewClaudeCacheKeepalive(func(context.Context, ClaudeCacheSnapshot) (int64, error) {
				calls++
				return 1000, nil
			})
			defer k.Close()
			k.now = func() time.Time { return now }
			k.SetEnabled(true)
			k.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
			now = now.Add(50 * time.Minute)
			k.ReplayDue(context.Background(), now)
			now = now.Add(39 * time.Minute)
			body := keepaliveTestBody
			if outcome == "ineligible" {
				body = strings.ReplaceAll(body, `"adaptive"`, `"enabled"`)
			}
			finish := k.Begin("a", "s", keepaliveTestRequest(t), []byte(body))
			now = now.Add(11 * time.Minute)
			k.ReplayDue(context.Background(), now)
			if calls != 1 {
				t.Fatal("replayed while a real chat was in flight")
			}
			finish(outcome == "ineligible")
			if snapshot := k.LogSnapshot(); snapshot.Sessions != 1 || snapshot.PausedSessions != 0 {
				t.Fatalf("new request discarded the last good snapshot: %+v", snapshot)
			}
			for round := 2; round <= 5; round++ {
				k.ReplayDue(context.Background(), now)
				if calls != round {
					t.Fatalf("renewal round %d: calls = %d", round, calls)
				}
				now = now.Add(50 * time.Minute)
			}
		})
	}
}

func TestClaudeCacheKeepaliveFailedReplacementDoesNotExtendTTL(t *testing.T) {
	now := time.Unix(1000000, 0)
	k := NewClaudeCacheKeepalive(func(context.Context, ClaudeCacheSnapshot) (int64, error) {
		t.Fatal("replayed a cache that expired during a failed replacement")
		return 0, nil
	})
	defer k.Close()
	k.now = func() time.Time { return now }
	k.SetEnabled(true)
	k.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
	now = now.Add(59 * time.Minute)
	finish := k.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))
	now = now.Add(2 * time.Minute)
	finish(false)
	k.ReplayDue(context.Background(), now)
	snapshot := k.LogSnapshot()
	if snapshot.Sessions != 0 || snapshot.Events[0].Outcome != "expired" {
		t.Fatalf("failed chat changed the old cache expiration: %+v", snapshot)
	}
}

func TestClaudeCacheKeepaliveRepeatedChatUsesOneSession(t *testing.T) {
	k := NewClaudeCacheKeepalive(nil)
	defer k.Close()
	k.SetEnabled(true)
	for i := 0; i < 5; i++ {
		k.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
	}
	snapshot := k.LogSnapshot()
	if snapshot.Sessions != 1 || len(snapshot.Events) != 6 {
		t.Fatalf("chat events should update one session, not create duplicate tasks: %+v", snapshot)
	}
}

func TestClaudeCacheKeepaliveOlderCompletionCannotReplaceNewestSnapshot(t *testing.T) {
	for _, olderSuccess := range []bool{false, true} {
		k := NewClaudeCacheKeepalive(func(_ context.Context, snap ClaudeCacheSnapshot) (int64, error) {
			if gjson.GetBytes(snap.Body, "max_tokens").Int() != 200 {
				t.Fatal("older completion replaced the newest successful chat")
			}
			return 1000, nil
		})
		k.SetEnabled(true)
		now := time.Unix(1000000, 0)
		k.now = func() time.Time { return now }
		older := k.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))
		newestBody := strings.ReplaceAll(keepaliveTestBody, `"max_tokens":100`, `"max_tokens":200`)
		k.Begin("a", "s", keepaliveTestRequest(t), []byte(newestBody))(true)
		older(olderSuccess)
		now = now.Add(50 * time.Minute)
		k.ReplayDue(context.Background(), now)
		if len(k.pending) != 0 || len(k.busy) != 0 {
			t.Fatal("completed chats retained pending state")
		}
		k.Close()
	}
}

func TestClaudeCacheKeepaliveFailedChatRetainsCancelledSnapshot(t *testing.T) {
	started, done := make(chan struct{}), make(chan struct{})
	calls := 0
	k := NewClaudeCacheKeepalive(func(ctx context.Context, _ ClaudeCacheSnapshot) (int64, error) {
		calls++
		if calls == 1 {
			close(started)
			<-ctx.Done()
			return 0, ctx.Err()
		}
		return 1000, nil
	})
	defer k.Close()
	k.SetEnabled(true)
	now := time.Unix(1000000, 0)
	k.now = func() time.Time { return now }
	k.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
	go func() {
		k.ReplayDue(context.Background(), now.Add(50*time.Minute))
		close(done)
	}()
	<-started
	finish := k.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))
	finish(false)
	<-done
	k.ReplayDue(context.Background(), now.Add(50*time.Minute))
	if snapshot := k.LogSnapshot(); calls != 2 || snapshot.Sessions != 1 || snapshot.PausedSessions != 0 {
		t.Fatalf("cancelled stale replay stopped the retained snapshot: calls=%d snapshot=%+v", calls, snapshot)
	}
}

func TestClaudeCacheKeepaliveSkipsIneligibleAndExpired(t *testing.T) {
	for _, body := range []string{
		`{"max_tokens":100,"messages":[]}`,
		`{"cache_control":{"type":"ephemeral"}}`,
		`{"max_tokens":100,"cache_control":{"type":"ephemeral"},"thinking":{"type":"enabled","budget_tokens":1024}}`,
		`{"max_tokens":100,"cache_control":{"type":"ephemeral"},"tools":[{"type":"web_search_20250305","name":"web_search"}]}`,
		`{"max_tokens":100,"system":"cc_is_subagent=true","cache_control":{"type":"ephemeral"}}`,
		`{"max_tokens":100,"metadata":{"user_id":{"parent_session_id":"parent"}},"cache_control":{"type":"ephemeral"}}`,
		`{"max_tokens":100,"metadata":{"user_id":"{\"parent_session_id\":\"parent\"}"},"cache_control":{"type":"ephemeral"}}`,
		`{"max_tokens":100,"cache_control":{"type":"ephemeral","ttl":"invalid"}}`,
	} {
		k := NewClaudeCacheKeepalive(func(context.Context, ClaudeCacheSnapshot) (int64, error) {
			t.Fatalf("replayed an ineligible request: %s", body)
			return 0, nil
		})
		k.SetEnabled(true)
		now := time.Unix(1000000, 0)
		k.now = func() time.Time { return now }
		k.Begin("a", "s", keepaliveTestRequest(t), []byte(body))(true)
		k.ReplayDue(context.Background(), now.Add(4*time.Minute))
		k.Close()
	}
	k := NewClaudeCacheKeepalive(func(context.Context, ClaudeCacheSnapshot) (int64, error) {
		t.Fatal("rewrote an expired cache")
		return 0, nil
	})
	defer k.Close()
	k.SetEnabled(true)
	now := time.Unix(1000000, 0)
	k.now = func() time.Time { return now }
	k.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
	k.ReplayDue(context.Background(), now.Add(time.Hour))
}

func TestClaudeCacheKeepaliveAccountIsolationAndPause(t *testing.T) {
	for _, replayError := range []error{nil, errors.New("upstream failure")} {
		calls := map[string]int{}
		k := NewClaudeCacheKeepalive(func(_ context.Context, snap ClaudeCacheSnapshot) (int64, error) {
			calls[snap.AuthID]++
			return 0, replayError
		})
		k.SetEnabled(true)
		now := time.Unix(1000000, 0)
		k.now = func() time.Time { return now }
		for _, id := range []string{"a", "b"} {
			k.Begin(id, "same-session", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
		}
		for i := 1; i <= 3; i++ {
			k.ReplayDue(context.Background(), now.Add(time.Duration(i)*50*time.Minute))
		}
		want := 2
		if replayError != nil {
			want = 1
		}
		if calls["a"] != want || calls["b"] != want {
			t.Fatalf("unexpected per-account attempts: %v", calls)
		}
		now = now.Add(3 * time.Hour)
		k.Begin("a", "same-session", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
		k.ReplayDue(context.Background(), now.Add(50*time.Minute))
		if calls["a"] != want+1 || calls["b"] != want {
			t.Fatal("new chat did not resume just its own snapshot")
		}
		k.Close()
	}
}

func TestClaudeCacheKeepaliveDisableCancelsReplay(t *testing.T) {
	started, done := make(chan struct{}), make(chan struct{})
	k := NewClaudeCacheKeepalive(func(ctx context.Context, snap ClaudeCacheSnapshot) (int64, error) {
		close(started)
		<-ctx.Done()
		return 0, ctx.Err()
	})
	defer k.Close()
	k.SetEnabled(true)
	now := time.Unix(1000000, 0)
	k.now = func() time.Time { return now }
	k.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
	go func() {
		k.ReplayDue(context.Background(), now.Add(50*time.Minute))
		close(done)
	}()
	<-started
	k.SetEnabled(false)
	<-done
	if len(k.sessions) != 0 {
		t.Fatal("disabled switch retained conversation snapshots")
	}
}

func TestClaudeCacheKeepaliveCloseAndBounds(t *testing.T) {
	k := NewClaudeCacheKeepalive(func(context.Context, ClaudeCacheSnapshot) (int64, error) { return 100, nil })
	k.SetEnabled(true)
	now := time.Unix(1000000, 0)
	k.now = func() time.Time { return now }
	for i := 0; i < 12; i++ {
		k.Begin("a", fmt.Sprint(i), keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
		now = now.Add(time.Second)
	}
	if len(k.sessions) != 8 {
		t.Fatalf("session cap = %d", len(k.sessions))
	}
	k.Close()
	k.SetEnabled(true)
	k.Begin("a", "closed", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
	if len(k.sessions) != 0 {
		t.Fatal("closed scheduler accepted a new snapshot")
	}
}

func TestClaudeCacheKeepaliveNewChatCancelsStaleReplay(t *testing.T) {
	started, done := make(chan struct{}), make(chan struct{})
	calls := 0
	k := NewClaudeCacheKeepalive(func(ctx context.Context, snap ClaudeCacheSnapshot) (int64, error) {
		calls++
		if calls == 1 {
			close(started)
			<-ctx.Done()
			return 0, ctx.Err()
		}
		return 1000, nil
	})
	defer k.Close()
	k.SetEnabled(true)
	now := time.Unix(1000000, 0)
	k.now = func() time.Time { return now }
	k.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
	go func() {
		k.ReplayDue(context.Background(), now.Add(50*time.Minute))
		close(done)
	}()
	<-started
	finish := k.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))
	<-done
	finish(true)
	k.ReplayDue(context.Background(), now.Add(50*time.Minute))
	if calls != 2 {
		t.Fatal("stale replay failure paused a newer chat")
	}
}

func TestClaudeCacheKeepaliveIgnoresSchemaCacheControl(t *testing.T) {
	calls := 0
	k := NewClaudeCacheKeepalive(func(context.Context, ClaudeCacheSnapshot) (int64, error) {
		calls++
		return 1000, nil
	})
	defer k.Close()
	k.SetEnabled(true)
	now := time.Unix(1000000, 0)
	k.now = func() time.Time { return now }
	body := []byte(`{"model":"claude-sonnet-5","max_tokens":100,"tools":[{"name":"custom","input_schema":{"type":"object","properties":{"cache_control":{"type":"ephemeral"}}}}],"messages":[{"role":"user","content":"q"}]}`)
	k.Begin("a", "s", keepaliveTestRequest(t), body)(true)
	k.ReplayDue(context.Background(), now.Add(4*time.Minute))
	if calls != 0 {
		t.Fatal("mistook tool schema content for an actual cache breakpoint")
	}
}

func TestClaudeCacheKeepaliveRechecksTTLAfterSlowReplay(t *testing.T) {
	now := time.Unix(1000000, 0)
	calls := 0
	k := NewClaudeCacheKeepalive(func(context.Context, ClaudeCacheSnapshot) (int64, error) {
		calls++
		now = now.Add(2 * time.Minute)
		return 1000, nil
	})
	defer k.Close()
	k.SetEnabled(true)
	k.now = func() time.Time { return now }
	body := []byte(`{"model":"claude-sonnet-5","max_tokens":100,"cache_control":{"type":"ephemeral"},"messages":[{"role":"user","content":"q"}]}`)
	for _, id := range []string{"a", "b"} {
		k.Begin(id, "s", keepaliveTestRequest(t), body)(true)
	}
	k.ReplayDue(context.Background(), now.Add(4*time.Minute))
	if calls != 1 {
		t.Fatalf("expired queued snapshot was sent after a slow replay: %d requests", calls)
	}
}
