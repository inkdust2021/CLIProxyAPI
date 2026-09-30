package executor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	sigcompat "github.com/router-for-me/CLIProxyAPI/v8/internal/signature"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ClaudeExecutor is a stateless executor for Anthropic Claude over the messages API.
// If api_key is unavailable on auth, it falls back to legacy via ClientAdapter.
type ClaudeExecutor struct {
	cfg                     *config.Config
	requestLogProvider      string
	upstreamModelNormalizer func(string) string
	oauthProfileFetcher     claudeOAuthProfileFetcher
	cacheKeepalive          *helps.ClaudeCacheKeepalive
}

type claudeOAuthCancellationError struct {
	cause error
}

func (e *claudeOAuthCancellationError) Error() string {
	if e == nil || e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

func (e *claudeOAuthCancellationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *claudeOAuthCancellationError) IsRequestScoped() bool {
	return e != nil
}

func newClaudeOAuthCancellationError(ctx context.Context, oauth bool, err error) error {
	if !oauth {
		return nil
	}
	cause := err
	if ctx != nil && ctx.Err() != nil {
		cause = ctx.Err()
	}
	if !errors.Is(cause, context.Canceled) {
		return nil
	}
	return &claudeOAuthCancellationError{cause: cause}
}

func shouldSanitizeClaudeMessagesForUpstream(baseModel string) bool {
	return sigcompat.SignatureProviderFromModelName(baseModel) == sigcompat.SignatureProviderClaude
}

func sanitizeClaudeMessagesForClaudeUpstreamWithDebug(ctx context.Context, body []byte, baseModel string, preserveEmptyThinkingBlocks ...bool) []byte {
	sanitized := body
	preserveEmpty := len(preserveEmptyThinkingBlocks) > 0 && preserveEmptyThinkingBlocks[0]
	if shouldSanitizeClaudeMessagesForUpstream(baseModel) || preserveEmpty {
		var report sigcompat.SignatureSanitizeReport
		sanitized, report = sigcompat.SanitizeClaudeMessagesForClaudeUpstream(body, baseModel, preserveEmptyThinkingBlocks...)
		logClaudeSignatureSanitizeReport(ctx, baseModel, report)
	}
	return sanitizeClaudeWebSearchDomains(sanitized)
}

// sanitizeClaudeWebSearchDomains removes empty allowed_domains/blocked_domains
// arrays from built-in web_search tools. Some clients (e.g. litellm) emit an
// empty array instead of omitting the field, and Anthropic rejects it with
// "Empty list of domains is ambiguous. Provide at least one domain or null.".
// Deleting the key is equivalent to leaving it unset.
func sanitizeClaudeWebSearchDomains(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return body
	}
	tools.ForEach(func(index, tool gjson.Result) bool {
		if !strings.HasPrefix(tool.Get("type").String(), "web_search_") {
			return true
		}
		for _, field := range []string{"allowed_domains", "blocked_domains"} {
			value := tool.Get(field)
			if value.Exists() && value.IsArray() && len(value.Array()) == 0 {
				path := fmt.Sprintf("tools.%d.%s", index.Int(), field)
				if updated, errDelete := sjson.DeleteBytes(body, path); errDelete == nil {
					body = updated
				}
			}
		}
		return true
	})
	return body
}

func logClaudeSignatureSanitizeReport(ctx context.Context, baseModel string, report sigcompat.SignatureSanitizeReport) {
	if report.DroppedBlocks == 0 && report.DroppedSignatures == 0 && report.ReplacedSignatures == 0 {
		return
	}

	fields := log.Fields{
		"component":           "signature_sanitizer",
		"executor":            "claude",
		"action":              "sanitize_claude_messages",
		"target_provider":     string(report.TargetProvider),
		"target_model":        baseModel,
		"preserved":           report.Preserved,
		"dropped_blocks":      report.DroppedBlocks,
		"dropped_signatures":  report.DroppedSignatures,
		"replaced_signatures": report.ReplacedSignatures,
	}
	if len(report.Decisions) > 0 {
		decision := report.Decisions[0]
		fields["first_block_kind"] = string(decision.BlockKind)
		fields["first_detected_provider"] = string(decision.DetectedProvider)
		fields["first_reason"] = decision.Reason
	}

	helps.LogWithRequestID(ctx).WithFields(fields).Debug("claude executor: sanitized signature history before upstream")
}

// Anthropic-compatible upstreams may reject or even crash when Claude models
// omit max_tokens. Prefer registered model metadata before using a fallback.
const defaultModelMaxTokens = 1024

func NewClaudeExecutor(cfg *config.Config) *ClaudeExecutor { return &ClaudeExecutor{cfg: cfg} }

// SetCacheKeepalive binds the service-owned scheduler before executor registration.
func (e *ClaudeExecutor) SetCacheKeepalive(keeper *helps.ClaudeCacheKeepalive) {
	e.cacheKeepalive = keeper
}

func (e *ClaudeExecutor) beginCacheKeepalive(auth *cliproxyauth.Auth, sessionID string, req *http.Request, incoming http.Header, body, original []byte) func(bool) {
	if e.cacheKeepalive == nil || e.cfg == nil || !e.cfg.Claude.CacheKeepalive ||
		auth == nil || !strings.EqualFold(auth.Provider, "claude") || helps.ClaudeCacheIsSubagent(incoming, original) {
		return func(bool) {}
	}
	if sessionID == "" {
		sessionID = helps.ExtractClaudeCodeSessionID(req.Context(), body, incoming)
	}
	return e.cacheKeepalive.Begin(auth.ID, sessionID, req, body)
}

// ReplayCache sends a final upstream snapshot with fresh credentials, without
// rerunning translation, cloaking, continuity updates, or tool alias generation.
func (e *ClaudeExecutor) ReplayCache(ctx context.Context, auth *cliproxyauth.Auth, snapshot helps.ClaudeCacheSnapshot) (cacheRead int64, err error) {
	if e.cfg == nil || !e.cfg.Claude.CacheKeepalive || auth == nil || auth.Disabled || auth.ID != snapshot.AuthID {
		return 0, fmt.Errorf("claude cache keepalive: disabled or unavailable credential")
	}
	_, baseURL := claudeCreds(auth)
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	if snapshot.URL != fmt.Sprintf("%s/v1/messages?beta=true", baseURL) {
		return 0, fmt.Errorf("claude cache keepalive: upstream changed")
	}
	body, err := helps.ClaudeCacheReplayBody(snapshot.Body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, snapshot.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header = snapshot.Headers.Clone()
	// Preserve the original transport mode and CCH: max_tokens is excluded from
	// the CCH signature, so the signed system prefix does not need rewriting.
	if err = e.PrepareRequest(req, auth); err != nil {
		return 0, err
	}
	reporter := helps.NewClaudeCacheUsageReporter(ctx, gjson.GetBytes(body, "model").String(), auth)
	reporter.SetStream(gjson.GetBytes(body, "stream").Bool())
	reporter.SetTranslatedReasoningEffort(body, "claude")
	defer reporter.TrackFailure(ctx, &err)
	client := reporter.TrackHTTPClient(helps.NewUtlsHTTPClient(ctx, e.cfg, auth, 0))
	resp, err := doClaudeUpstreamRequest(client, req)
	if err != nil {
		return 0, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, resp.StatusCode, resp.Header.Clone())
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debug("claude cache keepalive: response close failed")
		}
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, statusErr{code: resp.StatusCode, msg: "claude cache keepalive: upstream rejected replay"}
	}
	decoded, err := decodeResponseBody(resp.Body, claudeResponseContentEncoding(resp.Header))
	if err != nil {
		return 0, err
	}
	defer func() {
		if errClose := decoded.Close(); errClose != nil {
			log.Debug("claude cache keepalive: decoded response close failed")
		}
	}()
	if gjson.GetBytes(body, "stream").Bool() {
		var buffer helps.StreamUsageBuffer
		complete := false
		scanner := bufio.NewScanner(decoded)
		scanner.Buffer(nil, 1<<20)
		for scanner.Scan() {
			line := scanner.Bytes()
			buffer.ObserveClaudeStream(line)
			payload := bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(line), []byte("data:")))
			switch gjson.GetBytes(payload, "type").String() {
			case "error":
				return 0, fmt.Errorf("claude cache keepalive: upstream stream error")
			case "message_stop":
				complete = true
			}
			if complete {
				break
			}
		}
		if err = scanner.Err(); err != nil {
			return 0, err
		}
		detail, ok := buffer.Detail()
		if !complete || !ok {
			return 0, fmt.Errorf("claude cache keepalive: incomplete stream or missing usage")
		}
		buffer.Publish(ctx, reporter)
		return detail.CacheReadTokens, nil
	}
	raw, err := io.ReadAll(io.LimitReader(decoded, 1<<20))
	if err != nil {
		return 0, err
	}
	if !gjson.ValidBytes(raw) || gjson.GetBytes(raw, "type").String() != "message" || !gjson.GetBytes(raw, "usage").Exists() {
		return 0, fmt.Errorf("claude cache keepalive: invalid response or missing usage")
	}
	detail := helps.ParseClaudeUsage(raw)
	reporter.ObserveResponseModel(raw)
	reporter.Publish(ctx, detail)
	return detail.CacheReadTokens, nil
}

func (e *ClaudeExecutor) Identifier() string { return "claude" }

func (e *ClaudeExecutor) modelLevelCooling() bool {
	return e != nil && e.cfg != nil && e.cfg.Claude.ModelLevelCooling
}

func (e *ClaudeExecutor) upstreamRequestLogProvider() string {
	if provider := strings.TrimSpace(e.requestLogProvider); provider != "" {
		return provider
	}
	return e.Identifier()
}

func (e *ClaudeExecutor) upstreamModel(baseModel string) string {
	if e.upstreamModelNormalizer != nil {
		return e.upstreamModelNormalizer(baseModel)
	}
	return baseModel
}

func (e *ClaudeExecutor) restoreResponseModel(payload []byte, model string) []byte {
	if e.upstreamModelNormalizer == nil || strings.TrimSpace(model) == "" {
		return payload
	}
	return restoreClaudeResponseModel(payload, model)
}

func restoreClaudeResponseModel(payload []byte, model string) []byte {
	if updated, changed := setClaudeResponseModel(payload, model); changed {
		return updated
	}

	trimmed := bytes.TrimSpace(payload)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return payload
	}
	dataIndex := bytes.Index(payload, []byte("data:"))
	if dataIndex < 0 {
		return payload
	}
	rawJSON := bytes.TrimSpace(payload[dataIndex+len("data:"):])
	updated, changed := setClaudeResponseModel(rawJSON, model)
	if !changed {
		return payload
	}
	rebuilt := make([]byte, 0, dataIndex+len("data: ")+len(updated))
	rebuilt = append(rebuilt, payload[:dataIndex]...)
	rebuilt = append(rebuilt, []byte("data: ")...)
	rebuilt = append(rebuilt, updated...)
	return rebuilt
}

func setClaudeResponseModel(payload []byte, model string) ([]byte, bool) {
	if !gjson.ValidBytes(payload) {
		return payload, false
	}
	updated := payload
	changed := false
	for _, path := range []string{"model", "message.model"} {
		if !gjson.GetBytes(updated, path).Exists() {
			continue
		}
		next, errSet := sjson.SetBytes(updated, path, model)
		if errSet != nil {
			continue
		}
		updated = next
		changed = true
	}
	return updated, changed
}

// PrepareRequest injects Claude credentials into the outgoing HTTP request.
func (e *ClaudeExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	apiKey, _ := claudeCreds(auth)
	useAPIKey := auth != nil && (auth.AuthKind() == cliproxyauth.AuthKindAPIKey || (auth.Attributes != nil && strings.TrimSpace(auth.Attributes["api_key"]) != ""))
	isAnthropicBase := isAnthropicUpstreamURL(req.URL)
	if strings.TrimSpace(apiKey) != "" {
		if isAnthropicBase && useAPIKey {
			req.Header.Del("Authorization")
			req.Header.Set("x-api-key", apiKey)
		} else {
			req.Header.Del("x-api-key")
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
	} else {
		req.Header.Del("Authorization")
		req.Header.Del("x-api-key")
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects Claude credentials into the request and executes it.
func (e *ClaudeExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("claude executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := helps.NewUtlsHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}
