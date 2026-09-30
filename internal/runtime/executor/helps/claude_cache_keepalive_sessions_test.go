package helps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestClaudeCacheKeepaliveSessionDetails(t *testing.T) {
	k := NewClaudeCacheKeepalive(nil)
	defer k.Close()
	k.SetEnabled(true)
	body := `{"model":"claude-sonnet-5","max_tokens":100,"cache_control":{"type":"ephemeral","ttl":"1h"},"messages":[{"role":"user","content":[{"type":"text","text":"<script>latest human question</script>"}]},{"role":"assistant","content":"answer"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool","content":"private tool output"}]}]}`
	k.Begin("a", "s", keepaliveTestRequest(t), []byte(body))(true)
	raw, err := json.Marshal(k.LogSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		Details []struct {
			ID     string `json:"id"`
			Prompt string `json:"last_prompt"`
			State  string `json:"state"`
		} `json:"session_details"`
	}
	if err = json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Details) != 1 || len(snapshot.Details[0].ID) != 64 || snapshot.Details[0].Prompt != "<script>latest human question</script>" || snapshot.Details[0].State != "active" {
		t.Fatalf("missing current session and latest human text: %s", raw)
	}
	var events struct {
		Events []json.RawMessage `json:"events"`
	}
	_ = json.Unmarshal(raw, &events)
	for _, event := range events.Events {
		if strings.Contains(string(event), "human question") || strings.Contains(string(event), "tool output") {
			t.Fatal("prompt leaked into event history")
		}
	}
}
func TestClaudeCacheKeepaliveSessionDisableSurvivesChat(t *testing.T) {
	now := time.Unix(1000000, 0)
	calls := 0
	k := NewClaudeCacheKeepalive(func(context.Context, ClaudeCacheSnapshot) (int64, error) { calls++; return 1000, nil })
	defer k.Close()
	k.now = func() time.Time { return now }
	k.SetEnabled(true)
	k.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
	id := k.LogSnapshot().SessionDetails[0].ID
	k.SetDisabledSessions([]string{id})
	k.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
	k.Begin("a", "other", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
	now = now.Add(50 * time.Minute)
	k.ReplayDue(context.Background(), now)
	if calls != 1 {
		t.Fatalf("disabled session was renewed: %d", calls)
	}
	snapshot := k.LogSnapshot()
	if snapshot.DisabledSessions != 1 {
		t.Fatalf("disabled state lost: %+v", snapshot)
	}
	snapshot.SessionDetails[0].LastPrompt = "mutated"
	if k.LogSnapshot().SessionDetails[0].LastPrompt == "mutated" {
		t.Fatal("snapshot aliases keeper")
	}
	k.SetEnabled(false)
	k.SetEnabled(true)
	k.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
	now = now.Add(50 * time.Minute)
	k.ReplayDue(context.Background(), now)
	if calls != 1 {
		t.Fatal("global toggle forgot disabled choice")
	}
	k.SetDisabledSessions(nil)
	k.ReplayDue(context.Background(), now)
	if calls != 2 {
		t.Fatal("resume did not renew valid snapshot")
	}
}

func TestClaudeCacheKeepaliveResumeDoesNotExtendTTL(t *testing.T) {
	now := time.Unix(1000000, 0)
	k := NewClaudeCacheKeepalive(func(context.Context, ClaudeCacheSnapshot) (int64, error) {
		t.Fatal("expired snapshot replayed")
		return 0, nil
	})
	defer k.Close()
	k.now = func() time.Time { return now }
	k.SetEnabled(true)
	k.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
	id := k.LogSnapshot().SessionDetails[0].ID
	k.SetDisabledSessions([]string{id})
	now = now.Add(time.Hour)
	k.SetDisabledSessions(nil)
	k.ReplayDue(context.Background(), now)
	if k.LogSnapshot().Sessions != 0 {
		t.Fatal("resume extended expired TTL")
	}
}

func TestClaudeCacheKeepaliveManualDisableCancelsReplay(t *testing.T) {
	var k *ClaudeCacheKeepalive
	k = NewClaudeCacheKeepalive(func(ctx context.Context, _ ClaudeCacheSnapshot) (int64, error) {
		id := k.LogSnapshot().SessionDetails[0].ID
		k.SetDisabledSessions([]string{id})
		if ctx.Err() == nil {
			t.Fatal("manual disable did not cancel replay")
		}
		k.SetDisabledSessions(nil)
		return 0, ctx.Err()
	})
	defer k.Close()
	now := time.Unix(1000000, 0)
	k.now = func() time.Time { return now }
	k.SetEnabled(true)
	k.Begin("a", "s", keepaliveTestRequest(t), []byte(keepaliveTestBody))(true)
	k.ReplayDue(context.Background(), now.Add(50*time.Minute))
	if k.LogSnapshot().PausedSessions != 0 {
		t.Fatal("cancelled replay paused resumed snapshot")
	}
}

func TestClaudeCacheKeepalivePromptPreviewLimit(t *testing.T) {
	k := NewClaudeCacheKeepalive(nil)
	defer k.Close()
	k.SetEnabled(true)
	body := strings.Replace(keepaliveTestBody, "question", strings.Repeat("问", 2100), 1)
	k.Begin("a", "s", keepaliveTestRequest(t), []byte(body))(true)
	prompt := k.LogSnapshot().SessionDetails[0].LastPrompt
	if len([]rune(prompt)) > 2000 || !strings.HasSuffix(prompt, "…") {
		t.Fatal("preview is unbounded or broken Unicode")
	}
	k.SetEnabled(false)
	k.SetEnabled(true)
	body = strings.Replace(keepaliveTestBody, `"question"`, `[{"type":"image","source":{"data":"secret image"}}]`, 1)
	k.Begin("a", "s", keepaliveTestRequest(t), []byte(body))(true)
	if k.LogSnapshot().SessionDetails[0].LastPrompt != "" {
		t.Fatal("preview included image data")
	}
}
