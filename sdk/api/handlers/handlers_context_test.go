package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	"golang.org/x/net/context"
)

func TestGetContextWithCancel_PreservesRequestInfo(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req = req.WithContext(coreauth.WithHTTPRequestInfo(req.Context(), req, "client-key-123", "openai", map[string]string{
		"tenant": "alpha",
	}))
	ginCtx.Request = req

	handler := &BaseAPIHandler{Cfg: &config.SDKConfig{}}
	cliCtx, cancel := handler.GetContextWithCancel(nil, ginCtx, context.Background())
	defer cancel()

	info := coreauth.GetRequestInfo(cliCtx)
	if info == nil {
		t.Fatal("expected request info to be preserved on execution context")
	}
	if info.Principal != "client-key-123" {
		t.Fatalf("principal = %q, want %q", info.Principal, "client-key-123")
	}
	if info.Provider != "openai" {
		t.Fatalf("provider = %q, want %q", info.Provider, "openai")
	}
	if info.Metadata["tenant"] != "alpha" {
		t.Fatalf("tenant metadata = %q, want %q", info.Metadata["tenant"], "alpha")
	}
}

func TestGetContextWithCancel_MergesExecutionMetadataIntoRequestContext(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = req.WithContext(coreauth.WithHTTPRequestInfo(req.Context(), req, "client-key-123", "", nil))
	ginCtx.Request = req

	selectedAuthID := ""
	parentCtx := WithPinnedAuthID(context.Background(), "auth-1")
	parentCtx = WithExecutionSessionID(parentCtx, "session-1")
	parentCtx = WithSelectedAuthIDCallback(parentCtx, func(authID string) {
		selectedAuthID = authID
	})

	handler := &BaseAPIHandler{Cfg: &config.SDKConfig{}}
	cliCtx, cancel := handler.GetContextWithCancel(nil, ginCtx, parentCtx)
	defer cancel()

	if got := pinnedAuthIDFromContext(cliCtx); got != "auth-1" {
		t.Fatalf("pinned auth id = %q, want %q", got, "auth-1")
	}
	if got := executionSessionIDFromContext(cliCtx); got != "session-1" {
		t.Fatalf("execution session id = %q, want %q", got, "session-1")
	}

	callback := selectedAuthIDCallbackFromContext(cliCtx)
	if callback == nil {
		t.Fatal("expected selected auth callback to be preserved")
	}
	callback("auth-2")
	if selectedAuthID != "auth-2" {
		t.Fatalf("selected auth id = %q, want %q", selectedAuthID, "auth-2")
	}
	if coreauth.GetRequestInfo(cliCtx) == nil {
		t.Fatal("expected request info to remain available after merge")
	}
}
