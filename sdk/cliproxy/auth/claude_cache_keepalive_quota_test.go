package auth

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func reserveQuotaConfig(t *testing.T, keepalive, reserve, home bool) *internalconfig.Config {
	t.Helper()
	cfg, err := internalconfig.ParseConfigBytes([]byte(fmt.Sprintf("oauth: {providers: {claude: {cache-keepalive: %t, cache-keepalive-reserve-quota: %t}}}\nhome: {enabled: %t}\n", keepalive, reserve, home)))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func reservedClaudeAuth(id string, observed time.Time, signals map[string]string) *Auth {
	return &Auth{ID: id, Provider: "claude", Status: StatusActive, Attributes: map[string]string{AttributeAuthKind: AuthKindOAuth}, Quota: QuotaState{ObservedAt: observed, Signals: signals}}
}

func TestClaudeCacheKeepaliveReserveQuotaWindows(t *testing.T) {
	now := time.Now()
	reset := strconv.FormatInt(now.Add(time.Hour).Unix(), 10)
	past := strconv.FormatInt(now.Add(-time.Hour).Unix(), 10)
	for _, tc := range []struct {
		name, utilization, reset, window string
		observed                         time.Time
		blocked                          bool
	}{
		{"below threshold", "0.989999", reset, "5h", now, false},
		{"at threshold", "0.99", reset, "5h", now, true},
		{"weekly at threshold", "0.99", reset, "7d", now, true},
		{"full", "1", reset, "5h", now, true},
		{"negative", "-1", reset, "5h", now, false},
		{"overage", "1.05", reset, "5h", now, true},
		{"malformed", "bad", reset, "5h", now, false},
		{"nan", "NaN", reset, "5h", now, false},
		{"infinity", "+Inf", reset, "5h", now, false},
		{"missing", "", reset, "5h", now, false},
		{"expired", "0.99", past, "5h", now, false},
		{"missing reset fresh", "0.99", "", "5h", now, true},
		{"missing reset stale 5h", "0.99", "", "5h", now.Add(-5 * time.Hour), false},
		{"missing reset stale weekly", "0.99", "", "7d", now.Add(-7 * 24 * time.Hour), false},
		{"missing observation", "0.99", "", "5h", time.Time{}, false},
		{"invalid reset fresh", "0.99", "bad", "5h", now, true},
		{"invalid reset stale", "0.99", "bad", "5h", now.Add(-5 * time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			manager.SetConfig(reserveQuotaConfig(t, true, true, false))
			manager.RegisterExecutor(schedulerTestExecutor{provider: "claude"})
			prefix := "Anthropic-Ratelimit-Unified-" + tc.window
			a := reservedClaudeAuth("window-"+t.Name(), tc.observed, map[string]string{prefix + "-Utilization": tc.utilization, prefix + "-Reset": tc.reset})
			if _, err := manager.Register(context.Background(), a); err != nil {
				t.Fatal(err)
			}
			got, _, err := manager.pickNext(context.Background(), "claude", "", cliproxyexecutor.Options{}, nil)
			if tc.blocked && (err == nil || got != nil) {
				t.Fatalf("reserved account selected: auth=%v err=%v", got, err)
			}
			if !tc.blocked && (err != nil || got == nil) {
				t.Fatalf("eligible account rejected: auth=%v err=%v", got, err)
			}
			stored, _ := manager.GetByID(a.ID)
			if !reflect.DeepEqual(stored.Quota, a.Quota) || stored.Unavailable || stored.Disabled {
				t.Fatal("reservation changed availability or quota state")
			}
		})
	}
}

func TestClaudeCacheKeepaliveReserveQuotaSelectionPaths(t *testing.T) {
	for _, path := range []string{"fast", "fast-rr", "fast-weighted", "legacy", "mixed-fast", "mixed-fast-rr", "mixed-fast-weighted", "mixed-legacy", "plugin", "mixed-plugin", "affinity"} {
		t.Run(path, func(t *testing.T) {
			var selector Selector = &FillFirstSelector{}
			if strings.HasSuffix(path, "-rr") {
				selector = &RoundRobinSelector{}
			}
			if strings.HasSuffix(path, "-weighted") {
				selector = &WeightedRoundRobinSelector{}
			}
			if path == "legacy" || path == "mixed-legacy" {
				selector = &trackingSelector{}
			}
			if path == "affinity" {
				affinity := NewSessionAffinitySelector(&FillFirstSelector{})
				t.Cleanup(affinity.Stop)
				selector = affinity
			}
			manager := NewManager(nil, selector, nil)
			manager.RegisterExecutor(schedulerTestExecutor{provider: "claude"})
			manager.RegisterExecutor(schedulerTestExecutor{provider: "codex"})
			manager.SetConfig(reserveQuotaConfig(t, true, false, false))
			model := "reserve-model-" + t.Name()
			registerSchedulerModels(t, "claude", model, "a-reserved-"+t.Name(), "b-available-"+t.Name())
			for _, a := range []*Auth{reservedClaudeAuth("a-reserved-"+t.Name(), time.Now(), map[string]string{"Anthropic-Ratelimit-Unified-5h-Utilization": "0.99"}), reservedClaudeAuth("b-available-"+t.Name(), time.Now(), nil)} {
				if _, err := manager.Register(context.Background(), a); err != nil {
					t.Fatal(err)
				}
			}
			plugin := &fakePluginScheduler{pick: func(_ context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
				return pluginapi.SchedulerPickResponse{AuthID: req.Candidates[0].ID, Handled: true}, true, nil
			}}
			if path == "plugin" || path == "mixed-plugin" {
				manager.SetPluginScheduler(plugin)
			}
			opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.CanonicalSessionIDMetadataKey: "reserve-session"}}
			pick := func(tried map[string]struct{}) (*Auth, error) {
				if strings.HasPrefix(path, "mixed-") {
					a, _, _, err := manager.pickNextMixed(context.Background(), []string{"claude", "codex"}, model, opts, tried)
					return a, err
				}
				a, _, err := manager.pickNext(context.Background(), "claude", model, opts, tried)
				return a, err
			}
			// Seed an affinity binding to the account before enabling reservation.
			if path == "affinity" {
				a, err := pick(nil)
				if err != nil || a.ID != "a-reserved-"+t.Name() {
					t.Fatalf("initial binding = %v, %v", a, err)
				}
			}
			manager.SetConfig(reserveQuotaConfig(t, true, true, false))
			for i := 0; i < 3; i++ {
				a, err := pick(nil)
				if err != nil || a == nil || a.ID != "b-available-"+t.Name() {
					t.Fatalf("fallback = %v, %v", a, err)
				}
			}
			if path == "plugin" || path == "mixed-plugin" {
				for _, req := range plugin.requests {
					if len(req.Candidates) != 1 || req.Candidates[0].ID != "b-available-"+t.Name() {
						t.Fatalf("plugin received reserved candidate: %+v", req.Candidates)
					}
				}
			}
			tried := map[string]struct{}{"b-available-" + t.Name(): {}}
			if a, err := pick(tried); err == nil || a != nil {
				t.Fatalf("all reserved selected: %v, %v", a, err)
			}
			manager.SetConfig(reserveQuotaConfig(t, true, false, false))
			if a, err := pick(tried); err != nil || a == nil || a.ID != "a-reserved-"+t.Name() {
				t.Fatalf("switch off did not restore auth: %v, %v", a, err)
			}
		})
	}
}

func TestClaudeCacheKeepaliveReserveQuotaPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, provider, kind string
		keepalive, reserve   bool
	}{
		{"switch off", "claude", AuthKindOAuth, true, false},
		{"keepalive off", "claude", AuthKindOAuth, false, true},
		{"API key", "claude", AuthKindAPIKey, true, true},
		{"other provider", "codex", AuthKindOAuth, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			manager.SetConfig(reserveQuotaConfig(t, tc.keepalive, tc.reserve, false))
			manager.RegisterExecutor(schedulerTestExecutor{provider: tc.provider})
			a := reservedClaudeAuth(t.Name(), time.Now(), map[string]string{"Anthropic-Ratelimit-Unified-7d-Utilization": "1"})
			a.Provider = tc.provider
			a.Attributes[AttributeAuthKind] = tc.kind
			if _, err := manager.Register(context.Background(), a); err != nil {
				t.Fatal(err)
			}
			if got, _, err := manager.pickNext(context.Background(), tc.provider, "", cliproxyexecutor.Options{}, nil); err != nil || got == nil {
				t.Fatalf("non-reserved account rejected: %v", err)
			}
		})
	}
}

func TestClaudeCacheKeepaliveReserveQuotaMarkResult(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(reserveQuotaConfig(t, true, true, false))
	manager.RegisterExecutor(schedulerTestExecutor{provider: "claude"})
	a := reservedClaudeAuth(t.Name(), time.Now(), nil)
	if _, err := manager.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	ctx := internallogging.WithResponseHeadersHolder(context.Background())
	internallogging.SetResponseHeaders(ctx, http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.99"}})
	manager.MarkResult(ctx, Result{AuthID: a.ID, Provider: "claude", Success: true})
	if got, _, err := manager.pickNext(context.Background(), "claude", "", cliproxyexecutor.Options{Metadata: map[string]any{"cache-keepalive-reserve-quota": false}}, nil); err == nil || got != nil {
		t.Fatalf("observed quota/client metadata bypass = %v, %v", got, err)
	}
	internallogging.SetResponseHeaders(ctx, http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.1"}})
	manager.MarkResult(ctx, Result{AuthID: a.ID, Provider: "claude", Success: true})
	if got, _, err := manager.pickNext(context.Background(), "claude", "", cliproxyexecutor.Options{}, nil); err != nil || got == nil {
		t.Fatalf("latest observation did not restore selection: %v, %v", got, err)
	}
}

func TestClaudeCacheKeepaliveReserveQuotaExpiry(t *testing.T) {
	now := time.Unix(1791440000, 0)
	for _, window := range []struct {
		name     string
		duration time.Duration
	}{{"5h", 5 * time.Hour}, {"7d", 7 * 24 * time.Hour}} {
		t.Run(window.name, func(t *testing.T) {
			a := reservedClaudeAuth(t.Name(), now, map[string]string{"Anthropic-Ratelimit-Unified-" + window.name + "-Utilization": "0.99"})
			deadline := now.Add(window.duration)
			if !claudeCacheQuotaReserved(a, deadline.Add(-time.Nanosecond)) || claudeCacheQuotaReserved(a, deadline) {
				t.Fatal("missing reset did not expire at the observation window boundary")
			}
			a.Quota.Signals["Anthropic-Ratelimit-Unified-"+window.name+"-Reset"] = strconv.FormatInt(now.Add(time.Hour).Unix(), 10)
			if !claudeCacheQuotaReserved(a, now.Add(time.Hour-time.Nanosecond)) || claudeCacheQuotaReserved(a, now.Add(time.Hour)) {
				t.Fatal("reset did not release reservation at its exact boundary")
			}
		})
	}
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(reserveQuotaConfig(t, true, true, true))
	if !manager.authSelectionEligibilityForRequest(context.Background(), "", cliproxyexecutor.Options{}).allows(reservedClaudeAuth("home", time.Now(), map[string]string{"Anthropic-Ratelimit-Unified-5h-Utilization": "1"})) {
		t.Fatal("Home configuration inherited local reservation policy")
	}
}

func TestClaudeCacheKeepaliveReserveQuotaMixedProviderFallback(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(reserveQuotaConfig(t, true, true, false))
	for _, provider := range []string{"claude", "codex"} {
		manager.RegisterExecutor(schedulerTestExecutor{provider: provider})
		a := reservedClaudeAuth(provider+"-"+t.Name(), time.Now(), map[string]string{"Anthropic-Ratelimit-Unified-7d-Utilization": "0.99"})
		a.Provider = provider
		if _, err := manager.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		a, _, provider, err := manager.pickNextMixed(context.Background(), []string{"claude", "codex"}, "", cliproxyexecutor.Options{}, nil)
		if err != nil || a == nil || provider != "codex" {
			t.Fatalf("mixed provider fallback = %v, %s, %v", a, provider, err)
		}
	}
}

func TestClaudeCacheKeepaliveReserveQuotaPinnedAuth(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(reserveQuotaConfig(t, true, true, false))
	manager.RegisterExecutor(schedulerTestExecutor{provider: "claude"})
	a := reservedClaudeAuth(t.Name(), time.Now(), map[string]string{"Anthropic-Ratelimit-Unified-5h-Utilization": "0.99"})
	if _, err := manager.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: a.ID}}
	if got, _, err := manager.pickNext(context.Background(), "claude", "", opts, nil); got != nil || err == nil {
		t.Fatalf("pinned account bypassed reservation = %v, %v", got, err)
	}
}
