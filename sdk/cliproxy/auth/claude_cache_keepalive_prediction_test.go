package auth

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func predictionRecord(authID string, at time.Time, utilization float64) usage.Record {
	return usage.Record{AuthID: authID, Provider: "claude", AuthType: AuthKindOAuth, Model: "claude-opus-4-6", RequestedAt: at, Latency: time.Second, Detail: usage.Detail{InputTokens: 1000}, ResponseHeaders: http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": {fmt.Sprintf("%.4f", utilization)},
		"Anthropic-Ratelimit-Unified-5h-Reset":       {fmt.Sprint(at.Add(4 * time.Hour).Truncate(time.Hour).Unix())},
	}}
}

func trainPrediction(p *claudeQuotaPredictor, auth *Auth, now time.Time, start, delta float64) {
	for i := 0; i < 4; i++ {
		p.observe(auth, predictionRecord(auth.ID, now.Add(time.Duration(i)*time.Minute), start+float64(i)*delta))
	}
}

func TestClaudeQuotaPredictionRequiresRealCalibration(t *testing.T) {
	now := time.Unix(1791440000, 0)
	a := reservedClaudeAuth("prediction", now, nil)
	p := newClaudeQuotaPredictor()
	p.observe(a, predictionRecord(a.ID, now, .98))
	if p.reserved(a, "claude-opus-4-6", now.Add(time.Minute)) {
		t.Fatal("tokens alone invented quota calibration")
	}
	p.seed(a, "claude-opus-4-6", usage.Detail{InputTokens: 1000000}, now)
	if p.reserved(a, "claude-opus-4-6", now.Add(time.Minute)) {
		t.Fatal("historical workload invented quota calibration")
	}
	trainPrediction(p, a, now.Add(time.Minute), .95, .01)
	if !p.reserved(a, "claude-opus-4-6", now.Add(5*time.Minute)) {
		t.Fatal("paired utilization changes did not predict crossing the reserved 1%")
	}
	if p.reserved(a, "unknown-model", now.Add(5*time.Minute)) {
		t.Fatal("unknown model inherited a fabricated token prior")
	}
}

func TestClaudeQuotaPredictionNewerTrustedObservationReleases(t *testing.T) {
	now := time.Unix(1791440000, 0)
	a := reservedClaudeAuth("prediction-new-reset", now, nil)
	p := newClaudeQuotaPredictor()
	trainPrediction(p, a, now, .95, .01)
	if !p.reserved(a, "claude-opus-4-6", now.Add(4*time.Minute)) {
		t.Fatal("fixture was not reserved")
	}
	a.Quota.ObservedAt = now.Add(5 * time.Minute)
	a.Quota.Signals = map[string]string{"Anthropic-Ratelimit-Unified-5h-Utilization": "0.01", "Anthropic-Ratelimit-Unified-5h-Reset": fmt.Sprint(now.Add(9 * time.Hour).Unix())}
	if p.reserved(a, "claude-opus-4-6", now.Add(6*time.Minute)) {
		t.Fatal("persisted forecast overrode a newer real reset observation")
	}
}

func TestClaudeQuotaPredictionWeights(t *testing.T) {
	detail := usage.Detail{InputTokens: 100, OutputTokens: 10, ReasoningTokens: 5, CacheReadTokens: 100, CacheCreationTokens: 20, CacheCreation5mTokens: 10, CacheCreation1hTokens: 10}
	for _, tc := range []struct {
		model string
		want  float64
	}{{"claude-opus-4-6", 192.5}, {"claude-opus-5-5", 150}, {"claude-opus-5.5-20261001(high)", 150}, {"claude-sonnet-4-6", 115.5}, {"claude-sonnet-5-5", 75}} {
		got, ok := claudeQuotaWork(tc.model, detail)
		if !ok || math.Abs(got-tc.want) > 1e-9 {
			t.Fatalf("work(%s)=%v,%v want %v", tc.model, got, ok, tc.want)
		}
	}
	if _, ok := claudeQuotaWork("claude-opus-4-6", usage.Detail{InputTokens: -1}); ok {
		t.Fatal("negative tokens accepted")
	}
}

func TestClaudeQuotaPredictionCalibratesCurrentRequest(t *testing.T) {
	now := time.Unix(1791440000, 0)
	a := reservedClaudeAuth("prediction-current-request", now, nil)
	p := newClaudeQuotaPredictor()
	utilization := .90
	for i, tokens := range []int64{100, 2000, 500, 3000} {
		if i > 0 {
			utilization += float64(tokens) * .00001
		}
		record := predictionRecord(a.ID, now.Add(time.Duration(i)*time.Minute), utilization)
		record.Detail.InputTokens = tokens
		p.observe(a, record)
	}
	window := p.accounts[claudeQuotaAccountKey(a)].Windows["5h"]
	if len(window.Ratios) != 3 {
		t.Fatalf("paired calibration count=%d, want 3", len(window.Ratios))
	}
	for _, ratio := range window.Ratios {
		if math.Abs(ratio-.00001) > 1e-12 {
			t.Fatalf("current delta was paired with another request's tokens: %v", ratio)
		}
	}
	if window.PendingWork != 0 {
		t.Fatalf("already observed work was forecast twice: %v", window.PendingWork)
	}
}

func TestClaudeQuotaPredictionResetMalformedAndOld(t *testing.T) {
	now := time.Unix(1791440000, 0)
	a := reservedClaudeAuth("prediction", now, nil)
	p := newClaudeQuotaPredictor()
	trainPrediction(p, a, now, .95, .01)
	if p.reserved(a, "claude-opus-4-6", now.Add(8*time.Hour)) {
		t.Fatal("expired quota still reserved")
	}
	malformed := predictionRecord(a.ID, now.Add(4*time.Minute), .98)
	malformed.ResponseHeaders.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "NaN")
	p.observe(a, malformed)
	if p.reserved(a, "claude-opus-4-6", now.Add(5*time.Minute)) {
		t.Fatal("malformed latest observation retained a forecast")
	}
	trainPrediction(p, a, now.Add(5*time.Minute), .95, .01)
	r := predictionRecord(a.ID, now.Add(5*time.Minute), .01)
	r.RequestedAt = now.Add(9 * time.Minute)
	r.ResponseHeaders.Set("Anthropic-Ratelimit-Unified-5h-Reset", fmt.Sprint(now.Add(9*time.Hour).Unix()))
	p.observe(a, r)
	if p.reserved(a, "claude-opus-4-6", now.Add(10*time.Minute)) {
		t.Fatal("reset retained prior-window calibration")
	}
	r.ResponseHeaders.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "NaN")
	p.observe(a, r)
	if p.reserved(a, "claude-opus-4-6", now.Add(10*time.Minute)) {
		t.Fatal("malformed utilization produced prediction")
	}
}

func TestClaudeQuotaPredictionConcurrentReservation(t *testing.T) {
	now := time.Unix(1791440000, 0)
	a := reservedClaudeAuth("prediction", now, nil)
	p := newClaudeQuotaPredictor()
	trainPrediction(p, a, now, .961, .007)
	at := now.Add(4 * time.Minute)
	if p.reserved(a, "claude-opus-4-6", at) {
		t.Fatal("one next request should still fit")
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var releases []func()
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			finish, ok := p.begin(a, "claude-opus-4-6", at)
			if ok {
				mu.Lock()
				releases = append(releases, finish)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(releases) != 1 {
		t.Fatalf("concurrent admissions=%d want 1", len(releases))
	}
	if !p.reserved(a, "claude-opus-4-6", at) {
		t.Fatal("in-flight forecast was ignored")
	}
	for _, finish := range releases {
		finish()
		finish()
	}
	if p.reserved(a, "claude-opus-4-6", at) {
		t.Fatal("completed request leaked a temporary reservation")
	}
}

func TestClaudeQuotaPredictionPersistenceAndIdentity(t *testing.T) {
	now := time.Unix(1791440000, 0)
	a := reservedClaudeAuth("prediction", now, nil)
	a.Metadata = map[string]any{"email": "fixture@example.test"}
	dir := t.TempDir()
	p := newClaudeQuotaPredictor()
	if err := p.setPersistenceDir(dir); err != nil {
		t.Fatal(err)
	}
	trainPrediction(p, a, now, .95, .01)
	fresh := newClaudeQuotaPredictor()
	if err := fresh.setPersistenceDir(dir); err != nil {
		t.Fatal(err)
	}
	rotated := a.Clone()
	rotated.ID = "replacement-file"
	if !fresh.reserved(rotated, "claude-opus-4-6", now.Add(4*time.Minute)) {
		t.Fatal("restart or credential replacement lost account prediction")
	}
	if fresh.reserved(rotated, "claude-opus-4-6", now.Add(8*time.Hour)) {
		t.Fatal("restart restored expired reservation")
	}
	path := filepath.Join(dir, claudeQuotaPredictionFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "" {
		t.Fatal("prediction was not persisted")
	}
	if err := os.WriteFile(path, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	broken := newClaudeQuotaPredictor()
	if err := broken.setPersistenceDir(dir); err == nil {
		t.Fatal("corrupt prediction file accepted")
	}
	if broken.reserved(a, "claude-opus-4-6", now.Add(4*time.Minute)) {
		t.Fatal("corrupt state blocked ordinary selection")
	}
}

func TestClaudeQuotaPredictionReplayProfileAndManagerEligibility(t *testing.T) {
	now := time.Now().Truncate(time.Hour).Add(-10 * time.Minute)
	a := reservedClaudeAuth("prediction", now, nil)
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(reserveQuotaConfig(t, true, true, false))
	manager.RegisterExecutor(schedulerTestExecutor{provider: "claude"})
	if _, err := manager.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		manager.ObserveClaudeQuotaUsage(predictionRecord(a.ID, now.Add(time.Duration(i-4)*time.Minute), .945+float64(i)*.012))
	}
	if selected, _, err := manager.pickNext(context.Background(), "claude", "", cliproxyexecutor.Options{}, nil); selected != nil || err == nil {
		t.Fatalf("predicted reservation bypassed eligibility: %v,%v", selected, err)
	}
	p := newClaudeQuotaPredictor()
	p.seed(a, "claude-opus-4-6", usage.Detail{InputTokens: 1000}, now)
	r := predictionRecord(a.ID, now, .9)
	r.Source = "claude-cache-keepalive"
	r.Detail.InputTokens = 1000000
	p.observe(a, r)
	if work := p.nextWorkLocked(p.accounts[claudeQuotaAccountKey(a)], "claude-opus-4-6"); work != 1000 {
		t.Fatalf("replay polluted ordinary workload: %v", work)
	}
}

func TestClaudeQuotaPredictionReplayUpdatesWindowWithoutLearningRatio(t *testing.T) {
	now := time.Unix(1791440000, 0)
	a := reservedClaudeAuth("replay-calibration", now, nil)
	p := newClaudeQuotaPredictor()
	trainPrediction(p, a, now, .90, .01)
	key := claudeQuotaAccountKey(a)
	original := len(p.accounts[key].Windows["5h"].Ratios)
	replay := predictionRecord(a.ID, now.Add(4*time.Minute), .95)
	replay.Source = "claude-cache-keepalive"
	replay.Detail.InputTokens = 1000000
	p.observe(a, replay)
	window := p.accounts[key].Windows["5h"]
	if window.Utilization != .95 || len(window.Ratios) != original {
		t.Fatalf("replay changed calibration: utilization=%v ratios=%d", window.Utilization, len(window.Ratios))
	}
	if got := p.nextWorkLocked(p.accounts[key], "claude-opus-4-6"); got != 1000 {
		t.Fatalf("replay polluted ordinary workload: %v", got)
	}
	overlapping := predictionRecord(a.ID, now.Add(3*time.Minute+30*time.Second), .96)
	overlapping.Latency = 2 * time.Minute
	p.observe(a, overlapping)
	if got := len(p.accounts[key].Windows["5h"].Ratios); got != original {
		t.Fatalf("overlapping ordinary request learned a contaminated ratio: %d", got)
	}
}

func TestClaudeQuotaPredictionCountTokensExcluded(t *testing.T) {
	now := time.Now()
	a := reservedClaudeAuth("prediction-count", now, map[string]string{"Anthropic-Ratelimit-Unified-5h-Utilization": "0.99"})
	m := NewManager(nil, nil, nil)
	m.SetConfig(reserveQuotaConfig(t, true, true, false))
	m.RegisterExecutor(schedulerTestExecutor{provider: "claude"})
	registerSchedulerModels(t, "claude", "claude-opus-4-6", a.ID)
	if _, err := m.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ExecuteCount(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: "claude-opus-4-6"}, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("count tokens was reserved: %v", err)
	}
	if len(m.claudeQuotaPrediction.accounts) != 0 {
		t.Fatal("count tokens created prediction state")
	}
}

func TestClaudeQuotaPredictionStreamReservationLifetime(t *testing.T) {
	now := time.Unix(1791440000, 0)
	a := reservedClaudeAuth("prediction-stream", now, nil)
	m := NewManager(nil, nil, nil)
	p := m.claudeQuotaPrediction
	trainPrediction(p, a, now, .961, .007)
	finish, ok := p.begin(a, "claude-opus-4-6", now.Add(4*time.Minute))
	if !ok {
		t.Fatal("first stream should fit")
	}
	ctx := context.WithValue(context.Background(), claudeQuotaReleaseContextKey{}, finish)
	remaining := make(chan cliproxyexecutor.StreamChunk)
	result := m.wrapStreamResult(ctx, a, "claude", "", "", nil, nil, remaining, OAuthModelAliasResult{}, false, cliproxyexecutor.Options{})
	if !p.reserved(a, "claude-opus-4-6", now.Add(4*time.Minute)) {
		t.Fatal("stream returned before its temporary reservation was held")
	}
	close(remaining)
	for range result.Chunks {
	}
	if p.reserved(a, "claude-opus-4-6", now.Add(4*time.Minute)) {
		t.Fatal("stream completion retained temporary reservation")
	}
}
