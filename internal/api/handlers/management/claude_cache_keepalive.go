package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
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
