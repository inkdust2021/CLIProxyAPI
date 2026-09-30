package management

import (
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
)

// SetClaudeCacheKeepalive attaches the service-owned scheduler's event history.
func (h *Handler) SetClaudeCacheKeepalive(keeper *helps.ClaudeCacheKeepalive) {
	h.mu.Lock()
	h.claudeCacheKeepalive = keeper
	h.mu.Unlock()
}

func (h *Handler) GetClaudeCacheKeepalive(c *gin.Context) {
	h.mu.Lock()
	keeper := h.claudeCacheKeepalive
	h.mu.Unlock()
	c.JSON(http.StatusOK, keeper.LogSnapshot())
}

func (h *Handler) PatchClaudeCacheKeepaliveSession(c *gin.Context) {
	id := c.Param("id")
	decoded, err := hex.DecodeString(id)
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err != nil || len(decoded) != 32 || id != strings.ToLower(id) || c.ShouldBindJSON(&body) != nil || body.Enabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid session ID or body"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	keeper := h.claudeCacheKeepalive
	if keeper == nil || h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "keepalive unavailable"})
		return
	}
	known := false
	for _, item := range keeper.LogSnapshot().SessionDetails {
		if item.ID == id {
			known = true
			break
		}
	}
	if !known {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	previous := h.cfg.Claude.CacheKeepaliveDisabledSessions
	next := make([]string, 0, len(previous)+1)
	for _, key := range previous {
		if !strings.EqualFold(key, id) {
			next = append(next, key)
		}
	}
	if !*body.Enabled {
		next = append(next, id)
	}
	h.cfg.Claude.CacheKeepaliveDisabledSessions = next
	if errSave := config.SaveConfigPreserveComments(h.configFilePath, h.cfg, true); errSave != nil {
		h.cfg.Claude.CacheKeepaliveDisabledSessions = previous
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save session preference"})
		return
	}
	keeper.SetDisabledSessions(next)
	snapshot := h.reloadSnapshotConfigLocked()
	h.reloadConfigAfterManagementSaveAsync(c.Request.Context(), snapshot)
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
