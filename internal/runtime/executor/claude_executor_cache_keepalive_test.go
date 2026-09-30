package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestClaudeExecutorCacheKeepalive(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmtKeepaliveCase(stream, enabled), func(t *testing.T) {
				var mu sync.Mutex
				var bodies [][]byte
				var keys []string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					mu.Lock()
					bodies = append(bodies, body)
					keys = append(keys, r.Header.Get("Authorization"))
					mu.Unlock()
					if gjson.GetBytes(body, "stream").Bool() {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"model\":\"claude-sonnet-5\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1,\"cache_read_input_tokens\":1000,\"output_tokens\":0}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
					} else {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","model":"claude-sonnet-5","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":1000}}`)
					}
				}))
				defer server.Close()
				cfg := &config.Config{Claude: config.ClaudeConfig{CacheKeepalive: enabled}, DisableClaudeCloakMode: true}
				auth := &cliproxyauth.Auth{ID: "account-a", Provider: "claude", Attributes: map[string]string{"api_key": "original-token", "base_url": server.URL}}
				e := NewClaudeExecutor(cfg)
				k := helps.NewClaudeCacheKeepalive(func(ctx context.Context, snap helps.ClaudeCacheSnapshot) (int64, error) {
					fresh := auth.Clone()
					fresh.Attributes["api_key"] = "refreshed-token"
					return e.ReplayCache(ctx, fresh, snap)
				})
				defer k.Close()
				// Enable the scheduler even in the disabled case to test executor gating.
				k.SetEnabled(true)
				e.SetCacheKeepalive(k)
				body := []byte(`{"model":"claude-sonnet-5","max_tokens":100,"system":[{"type":"text","text":"stable","cache_control":{"type":"ephemeral","ttl":"5m"}}],"messages":[{"role":"user","content":"question"}]}`)
				body, _ = sjson.SetBytes(body, "stream", stream)
				req := cliproxyexecutor.Request{Model: "claude-sonnet-5", Payload: body}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}
				if stream {
					result, err := e.ExecuteStream(context.Background(), auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				} else if _, err := e.Execute(context.Background(), auth, req, opts); err != nil {
					t.Fatal(err)
				}
				k.ReplayDue(context.Background(), time.Now().Add(4*time.Minute+time.Second))
				mu.Lock()
				defer mu.Unlock()
				want := 1
				if enabled {
					want = 2
				}
				if len(bodies) != want {
					t.Fatalf("upstream requests = %d, want %d", len(bodies), want)
				}
				if enabled {
					before := gjson.ParseBytes(bodies[0]).Value().(map[string]any)
					after := gjson.ParseBytes(bodies[1]).Value().(map[string]any)
					delete(before, "max_tokens")
					delete(after, "max_tokens")
					if !reflect.DeepEqual(before, after) || gjson.GetBytes(bodies[1], "max_tokens").Int() != 1 {
						t.Fatal("replay changed the final upstream prefix or output cap")
					}
					if keys[1] != "Bearer refreshed-token" {
						t.Fatalf("replay used stale credentials: %q", keys[1])
					}
				}
			})
		}
	}
}

func fmtKeepaliveCase(stream, enabled bool) string {
	name := "nonstream"
	if stream {
		name = "stream"
	}
	if enabled {
		return name + "/enabled"
	}
	return name + "/disabled"
}
