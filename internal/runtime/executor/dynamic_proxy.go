package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

var dynamicProxyTemplatePattern = regexp.MustCompile(`\{([a-z_]+)(?::([^{}]+))?\}`)

func proxyURLContainsTemplate(raw string) bool {
	return dynamicProxyTemplatePattern.MatchString(strings.TrimSpace(raw))
}

func resolveDynamicProxyURL(ctx context.Context, raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || !proxyURLContainsTemplate(trimmed) {
		return trimmed, nil
	}

	requestInfo := cliproxyauth.GetRequestInfo(ctx)
	if requestInfo == nil {
		return "", fmt.Errorf("dynamic proxy-url requires request context")
	}

	var unresolved []string
	resolved := dynamicProxyTemplatePattern.ReplaceAllStringFunc(trimmed, func(match string) string {
		submatches := dynamicProxyTemplatePattern.FindStringSubmatch(match)
		if len(submatches) < 2 {
			unresolved = append(unresolved, match)
			return match
		}
		key := strings.ToLower(strings.TrimSpace(submatches[1]))
		arg := ""
		if len(submatches) > 2 {
			arg = strings.TrimSpace(submatches[2])
		}
		value, ok := resolveDynamicProxyTemplateValue(requestInfo, key, arg)
		if !ok {
			unresolved = append(unresolved, match)
			return match
		}
		return escapeDynamicProxyTemplateValue(value)
	})
	if len(unresolved) > 0 {
		return "", fmt.Errorf("unresolved proxy-url placeholders: %s", strings.Join(unresolved, ", "))
	}
	return resolved, nil
}

func resolveDynamicProxyTemplateValue(info *cliproxyauth.RequestInfo, key, arg string) (string, bool) {
	if info == nil {
		return "", false
	}

	switch key {
	case "client_api_key":
		value := strings.TrimSpace(info.Principal)
		return value, value != ""
	case "client_api_key_hash":
		value := strings.TrimSpace(info.Principal)
		if value == "" {
			return "", false
		}
		return stableProxyHash(value), true
	case "access_provider":
		value := strings.TrimSpace(info.Provider)
		return value, value != ""
	case "request_header":
		if info.Headers == nil || arg == "" {
			return "", false
		}
		value := strings.TrimSpace(info.Headers.Get(arg))
		return value, value != ""
	case "request_header_hash":
		if info.Headers == nil || arg == "" {
			return "", false
		}
		value := strings.TrimSpace(info.Headers.Get(arg))
		if value == "" {
			return "", false
		}
		return stableProxyHash(value), true
	case "request_query":
		if info.Query == nil || arg == "" {
			return "", false
		}
		value := strings.TrimSpace(info.Query.Get(arg))
		return value, value != ""
	case "request_query_hash":
		if info.Query == nil || arg == "" {
			return "", false
		}
		value := strings.TrimSpace(info.Query.Get(arg))
		if value == "" {
			return "", false
		}
		return stableProxyHash(value), true
	default:
		return "", false
	}
}

func stableProxyHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func escapeDynamicProxyTemplateValue(value string) string {
	// 动态占位符常用于代理 URL 的 userinfo（用户名/密码）。
	// QueryEscape 会对 ':'、'@' 等分隔符进行转义，避免破坏 userinfo 结构。
	escaped := url.QueryEscape(value)
	return strings.ReplaceAll(escaped, "+", "%20")
}
