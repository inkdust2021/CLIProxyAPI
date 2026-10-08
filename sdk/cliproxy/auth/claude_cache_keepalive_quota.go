package auth

import (
	"context"
	"math"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func (m *Manager) authSelectionEligibilityForRequest(ctx context.Context, model string, opts cliproxyexecutor.Options) authSelectionEligibility {
	eligibility := authSelectionEligibilityForRequest(ctx, opts)
	cfg := m.runtimeConfigSnapshot()
	eligibility.reserveClaudeQuota = cfg != nil && cfg.Claude.CacheKeepalive && cfg.Claude.CacheKeepaliveReserveQuota && !cfg.Home.Enabled
	if ctx != nil {
		countTokens, _ := ctx.Value(claudeQuotaCountTokensContextKey{}).(bool)
		eligibility.reserveClaudeQuota = eligibility.reserveClaudeQuota && !countTokens
	}
	eligibility.now = time.Now()
	if eligibility.reserveClaudeQuota {
		eligibility.predictedQuotaReserved = func(auth *Auth) bool {
			return m.claudeQuotaPrediction.reserved(auth, m.selectionModelKeyForAuth(auth, model), eligibility.now)
		}
	}
	return eligibility
}

func (s *authScheduler) authSelectionEligibilityForRequest(ctx context.Context, model string, opts cliproxyexecutor.Options) authSelectionEligibility {
	if s.selectionEligibility != nil {
		return s.selectionEligibility(ctx, model, opts)
	}
	return authSelectionEligibilityForRequest(ctx, opts)
}

// claudeCacheQuotaReserved affects ordinary selection only; replay uses the original credential directly.
func claudeCacheQuotaReserved(auth *Auth, now time.Time) bool {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") || auth.AuthKind() != AuthKindOAuth {
		return false
	}
	return claudeCacheQuotaWindowReserved(auth.Quota, "5h", 5*time.Hour, now) ||
		claudeCacheQuotaWindowReserved(auth.Quota, "7d", 7*24*time.Hour, now)
}

func claudeCacheQuotaWindowReserved(quota QuotaState, window string, duration time.Duration, now time.Time) bool {
	prefix := "Anthropic-Ratelimit-Unified-" + window
	utilization, errParse := strconv.ParseFloat(strings.TrimSpace(quota.Signals[prefix+"-Utilization"]), 64)
	if errParse != nil || math.IsNaN(utilization) || math.IsInf(utilization, 0) || utilization < 0.99 {
		return false
	}
	reset := strings.TrimSpace(quota.Signals[prefix+"-Reset"])
	if reset != "" {
		seconds, errReset := strconv.ParseInt(reset, 10, 64)
		if errReset == nil {
			return now.Before(time.Unix(seconds, 0))
		}
	}
	// An observation without a valid reset expires after the corresponding quota window.
	return !quota.ObservedAt.IsZero() && !quota.ObservedAt.After(now) && now.Before(quota.ObservedAt.Add(duration))
}
