package cliproxy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/tidwall/gjson"
)

func (s *Service) claudeCacheKeeper() *helps.ClaudeCacheKeepalive {
	s.claudeCacheMu.Lock()
	defer s.claudeCacheMu.Unlock()
	if s.claudeCache == nil {
		s.claudeCache = helps.NewClaudeCacheKeepalive(s.replayClaudeCache)
		s.cfgMu.RLock()
		cfg := s.cfg
		s.cfgMu.RUnlock()
		s.claudeCache.SetEnabled(cfg != nil && cfg.Claude.CacheKeepalive && !cfg.Home.Enabled)
	}
	return s.claudeCache
}

func (s *Service) configureClaudeCacheKeepalive(cfg *config.Config) {
	// Home credentials belong to individual execution sessions and cannot be
	// safely renewed by a detached local worker.
	s.claudeCacheKeeper().SetEnabled(cfg != nil && cfg.Claude.CacheKeepalive && !cfg.Home.Enabled)
}

func (s *Service) replayClaudeCache(ctx context.Context, snapshot helps.ClaudeCacheSnapshot) (int64, error) {
	if s.coreManager == nil {
		return 0, fmt.Errorf("claude cache keepalive: auth manager unavailable")
	}
	auth, ok := s.coreManager.GetByID(snapshot.AuthID)
	if !ok || auth.Disabled || auth.Status == coreauth.StatusDisabled ||
		!strings.EqualFold(auth.Provider, "claude") || auth.NextRetryAfter.After(time.Now()) {
		return 0, fmt.Errorf("claude cache keepalive: original credential unavailable")
	}
	model := gjson.GetBytes(snapshot.Body, "model").String()
	if state := auth.ModelStates[model]; state != nil &&
		(state.Status == coreauth.StatusDisabled || state.NextRetryAfter.After(time.Now())) {
		return 0, fmt.Errorf("claude cache keepalive: original model unavailable")
	}
	current, ok := s.coreManager.Executor("claude")
	if !ok {
		return 0, fmt.Errorf("claude cache keepalive: executor unavailable")
	}
	claude, ok := current.(*executor.ClaudeExecutor)
	if !ok {
		return 0, fmt.Errorf("claude cache keepalive: executor does not support snapshot replay")
	}
	return claude.ReplayCache(ctx, auth, snapshot)
}
