package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
)

func TestGetClaudeCacheKeepaliveSnapshot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{}
	keeper := helps.NewClaudeCacheKeepalive(nil)
	defer keeper.Close()
	for _, enabled := range []bool{false, true, false} {
		keeper.SetEnabled(enabled)
		h.SetClaudeCacheKeepalive(keeper)
		var previous string
		for i := 0; i < 2; i++ {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/v8/management/observability/claude-cache-keepalive", nil)
			h.GetClaudeCacheKeepalive(c)
			var got helps.ClaudeCacheLogSnapshot
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if w.Code != 200 || got.Enabled != enabled || got.Capacity != 200 || got.Events == nil {
				t.Fatalf("invalid snapshot: %s", w.Body.String())
			}
			if i > 0 && previous != w.Body.String() {
				t.Fatal("read consumed events")
			}
			previous = w.Body.String()
		}
	}
	h.SetClaudeCacheKeepalive(nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	h.GetClaudeCacheKeepalive(c)
	if w.Code != 200 {
		t.Fatal("unavailable keeper should return an empty disabled snapshot")
	}
}
