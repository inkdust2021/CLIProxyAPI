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
