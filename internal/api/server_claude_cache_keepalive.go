package api

import "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"

// SetClaudeCacheKeepalive exposes service-owned operational history to management.
func (s *Server) SetClaudeCacheKeepalive(keeper *helps.ClaudeCacheKeepalive) {
	s.mgmt.SetClaudeCacheKeepalive(keeper)
}
