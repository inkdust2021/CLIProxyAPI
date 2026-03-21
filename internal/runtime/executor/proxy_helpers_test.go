package executor

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestNewProxyAwareHTTPClientDirectBypassesGlobalProxy(t *testing.T) {
	t.Parallel()

	client := newProxyAwareHTTPClient(
		context.Background(),
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"}},
		&cliproxyauth.Auth{ProxyURL: "direct"},
		0,
	)

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("expected direct transport to disable proxy function")
	}
}

func TestResolveDynamicProxyURL_ClientAPIKeyHash(t *testing.T) {
	t.Parallel()

	ctx := cliproxyauth.WithRequestInfo(context.Background(), &cliproxyauth.RequestInfo{
		Principal: "client-key-123",
	})
	resolved, err := resolveDynamicProxyURL(ctx, "http://team.{client_api_key_hash}:token@proxy.example.com:2260")
	if err != nil {
		t.Fatalf("resolveDynamicProxyURL returned error: %v", err)
	}
	parsed, err := url.Parse(resolved)
	if err != nil {
		t.Fatalf("resolved proxy URL parse failed: %v", err)
	}
	if parsed.User == nil {
		t.Fatal("expected resolved proxy URL to keep user info")
	}
	username := parsed.User.Username()
	if username == "" || username == "team.{client_api_key_hash}" {
		t.Fatalf("expected placeholder to be resolved, got %q", username)
	}
}

func TestResolveDynamicProxyURL_ResinDefaultPlatformClientHash(t *testing.T) {
	t.Parallel()

	ctx := cliproxyauth.WithRequestInfo(context.Background(), &cliproxyauth.RequestInfo{
		Principal: "client-key-123",
	})
	resolved, err := resolveDynamicProxyURL(ctx, "http://Default.{client_api_key_hash}:my-token@resin:2260")
	if err != nil {
		t.Fatalf("resolveDynamicProxyURL returned error: %v", err)
	}
	parsed, err := url.Parse(resolved)
	if err != nil {
		t.Fatalf("resolved proxy URL parse failed: %v", err)
	}
	if parsed.User == nil {
		t.Fatal("expected resolved proxy URL to keep user info")
	}
	password, hasPassword := parsed.User.Password()
	if !hasPassword {
		t.Fatal("expected resolved proxy URL to keep password")
	}
	if password != "my-token" {
		t.Fatalf("password = %q, want %q", password, "my-token")
	}
	wantUsername := "Default." + stableProxyHash("client-key-123")
	if got := parsed.User.Username(); got != wantUsername {
		t.Fatalf("username = %q, want %q", got, wantUsername)
	}
}

func TestResolveDynamicProxyURL_ResinAccountWithColon(t *testing.T) {
	t.Parallel()

	ctx := cliproxyauth.WithRequestInfo(context.Background(), &cliproxyauth.RequestInfo{
		Headers: http.Header{
			"X-Resin-Account": []string{"bEA:234"},
		},
	})
	resolved, err := resolveDynamicProxyURL(ctx, "http://MyHub.{request_header:X-Resin-Account}:resin-123456@resin:2260")
	if err != nil {
		t.Fatalf("resolveDynamicProxyURL returned error: %v", err)
	}
	parsed, err := url.Parse(resolved)
	if err != nil {
		t.Fatalf("resolved proxy URL parse failed: %v", err)
	}
	if parsed.User == nil {
		t.Fatal("expected resolved proxy URL to keep user info")
	}
	password, hasPassword := parsed.User.Password()
	if !hasPassword {
		t.Fatal("expected resolved proxy URL to keep password")
	}
	if password != "resin-123456" {
		t.Fatalf("password = %q, want %q", password, "resin-123456")
	}
	if got := parsed.User.Username(); got != "MyHub.bEA:234" {
		t.Fatalf("username = %q, want %q", got, "MyHub.bEA:234")
	}
}

func TestNewProxyAwareHTTPClient_UsesDynamicAuthProxyURL(t *testing.T) {
	t.Parallel()

	ctx := cliproxyauth.WithRequestInfo(context.Background(), &cliproxyauth.RequestInfo{
		Principal: "client-key-123",
	})
	client := newProxyAwareHTTPClient(
		ctx,
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"}},
		&cliproxyauth.Auth{ProxyURL: "http://team.{client_api_key_hash}:token@proxy.example.com:2260"},
		0,
	)

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	req, err := http.NewRequest(http.MethodGet, "https://api.openai.com/v1/models", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	proxyURL, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("transport.Proxy returned error: %v", err)
	}
	if proxyURL == nil {
		t.Fatal("expected proxy URL to be configured")
	}
	if proxyURL.Host != "proxy.example.com:2260" {
		t.Fatalf("unexpected proxy host: %s", proxyURL.Host)
	}
	if proxyURL.User == nil || proxyURL.User.Username() == "team.{client_api_key_hash}" {
		t.Fatalf("expected dynamic proxy username, got %v", proxyURL.User)
	}
}

func TestResolveEffectiveProxyURL_SkipsDynamicGlobalProxyWithoutRequestContext(t *testing.T) {
	t.Parallel()

	resolved, ok := resolveEffectiveProxyURL(
		context.Background(),
		"http://team.{client_api_key_hash}:token@proxy.example.com:2260",
		"global",
	)
	if ok {
		t.Fatalf("expected dynamic proxy without request context to be skipped, got %q", resolved)
	}
	if resolved != "" {
		t.Fatalf("resolved = %q, want empty", resolved)
	}
}

func TestEffectiveProxyURL_UsesResinProxyWhenEnabled(t *testing.T) {
	t.Parallel()

	ctx := cliproxyauth.WithRequestInfo(context.Background(), &cliproxyauth.RequestInfo{
		Principal: "client-key-123",
	})
	cfg := &config.Config{
		SDKConfig: sdkconfig.SDKConfig{
			ProxyURL:          "http://global-proxy.example.com:8080",
			ResinProxyEnabled: true,
			ResinProxyURL:     "http://team.{client_api_key_hash}:token@resin:2260",
		},
	}
	auth := &cliproxyauth.Auth{ProxyURL: "http://auth-proxy.example.com:1080"}

	resolved := effectiveProxyURL(ctx, cfg, auth)
	if resolved == "" {
		t.Fatal("expected resin proxy URL to be resolved")
	}
	parsed, err := url.Parse(resolved)
	if err != nil {
		t.Fatalf("parse resolved resin proxy URL: %v", err)
	}
	if parsed.Host != "resin:2260" {
		t.Fatalf("unexpected host %q, want %q", parsed.Host, "resin:2260")
	}
	if parsed.User == nil || parsed.User.Username() == "team.{client_api_key_hash}" {
		t.Fatalf("expected resolved resin username, got %v", parsed.User)
	}
}

func TestEffectiveProxyURL_ResinEnabledDoesNotFallbackLegacyProxy(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		SDKConfig: sdkconfig.SDKConfig{
			ProxyURL:          "http://global-proxy.example.com:8080",
			ResinProxyEnabled: true,
			ResinProxyURL:     "http://team.{client_api_key_hash}:token@resin:2260",
		},
	}
	auth := &cliproxyauth.Auth{ProxyURL: "http://auth-proxy.example.com:1080"}

	resolved := effectiveProxyURL(context.Background(), cfg, auth)
	if resolved != "" {
		t.Fatalf("expected no fallback proxy when resin is enabled without request context, got %q", resolved)
	}
}
