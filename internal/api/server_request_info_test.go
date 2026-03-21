package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestRequestInfoMiddlewareInjectsAuthenticatedContext(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("apiKey", "client-key-123")
		c.Set("accessProvider", "config-inline")
		c.Set("accessMetadata", map[string]string{"source": "authorization"})
		c.Next()
	})
	router.Use(RequestInfoMiddleware())
	router.GET("/test", func(c *gin.Context) {
		info := coreauth.GetRequestInfo(c.Request.Context())
		if info == nil {
			t.Fatal("expected request info in context")
		}
		if info.Principal != "client-key-123" {
			t.Fatalf("unexpected principal: %q", info.Principal)
		}
		if info.Provider != "config-inline" {
			t.Fatalf("unexpected provider: %q", info.Provider)
		}
		if got := info.Headers.Get("X-Resin-Account"); got != "demo-user" {
			t.Fatalf("unexpected header value: %q", got)
		}
		if got := info.Query.Get("resin_account"); got != "query-user" {
			t.Fatalf("unexpected query value: %q", got)
		}
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodGet, "/test?resin_account=query-user", nil)
	req.Header.Set("X-Resin-Account", "demo-user")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusNoContent {
		t.Fatalf("unexpected status code: %d", resp.Code)
	}
}
