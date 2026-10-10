package management

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	"golang.org/x/crypto/bcrypt"
)

func TestAuthenticateManagementKey_FailedAttemptsDoNotBanClient(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("test-secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"environment", "configured", "local password"} {
		for _, localClient := range []bool{true, false} {
			if mode == "local password" && !localClient {
				continue
			}
			for _, provided := range []string{"", "wrong-secret"} {
				h := NewHandler(&config.Config{}, "", nil)
				h.envSecret = ""
				h.allowRemoteOverride = false
				h.cfg.RemoteManagement.AllowRemote = true
				h.cfg.RemoteManagement.SecretKey = string(hash)
				validKey := "test-secret"
				if mode == "environment" {
					h.envSecret = validKey
					h.cfg.RemoteManagement.SecretKey = ""
				}
				if mode == "local password" {
					validKey = "test-local-secret"
					h.localPassword = validKey
				}
				wantMessage := "invalid management key"
				if provided == "" {
					wantMessage = "missing management key"
				}
				for i := 0; i < 10; i++ {
					allowed, statusCode, errMsg := h.AuthenticateManagementKey("172.20.0.1", localClient, provided)
					if allowed || statusCode != http.StatusUnauthorized || errMsg != wantMessage {
						t.Fatalf("local=%v attempt=%d: allowed=%v status=%d msg=%q", localClient, i+1, allowed, statusCode, errMsg)
					}
				}
				allowed, statusCode, errMsg := h.AuthenticateManagementKey("172.20.0.1", localClient, validKey)
				if !allowed || statusCode != 0 || errMsg != "" {
					t.Fatalf("correct key after failures: allowed=%v status=%d msg=%q", allowed, statusCode, errMsg)
				}
			}
		}
	}
}

func TestAuthenticateManagementKey_AccessRestrictions(t *testing.T) {
	h := &Handler{cfg: &config.Config{}}
	allowed, statusCode, errMsg := h.AuthenticateManagementKey("172.20.0.1", false, "test-secret")
	if allowed || statusCode != http.StatusForbidden || errMsg != "remote management disabled" {
		t.Fatalf("remote access without enablement: allowed=%v status=%d msg=%q", allowed, statusCode, errMsg)
	}
	h.cfg.RemoteManagement.AllowRemote = true
	allowed, statusCode, errMsg = h.AuthenticateManagementKey("172.20.0.1", false, "test-secret")
	if allowed || statusCode != http.StatusForbidden || errMsg != "remote management key not set" {
		t.Fatalf("access without configured key: allowed=%v status=%d msg=%q", allowed, statusCode, errMsg)
	}
}

func TestMiddlewareSetsSupportPluginHeader(t *testing.T) {

	h := &Handler{
		cfg:       &config.Config{},
		envSecret: "test-secret",
	}
	middleware := h.Middleware()

	t.Run("invalid key", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
		c.Request.RemoteAddr = "127.0.0.1:12345"
		c.Request.Header.Set("X-Management-Key", "wrong-secret")

		middleware(c)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
		if got := rec.Header().Get("X-CPA-SUPPORT-PLUGIN"); got != pluginhost.SupportPluginHeaderValue() {
			t.Fatalf("X-CPA-SUPPORT-PLUGIN = %q, want %q", got, pluginhost.SupportPluginHeaderValue())
		}
	})

	t.Run("valid key", func(t *testing.T) {
		engine := gin.New()
		engine.GET("/v0/management/config", middleware, func(c *gin.Context) {
			c.Status(http.StatusOK)
		})

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("X-Management-Key", "test-secret")
		engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if got := rec.Header().Get("X-CPA-SUPPORT-PLUGIN"); got != pluginhost.SupportPluginHeaderValue() {
			t.Fatalf("X-CPA-SUPPORT-PLUGIN = %q, want %q", got, pluginhost.SupportPluginHeaderValue())
		}
	})
}
