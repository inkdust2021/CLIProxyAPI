package helps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	claudeCacheMaxSessions = 8
	claudeCacheMaxBody     = 4 << 20
)

// ClaudeCacheSnapshot holds a final upstream request without access credentials.
// AuthID pins replay to the original account; credentials are resolved at send time.
type ClaudeCacheSnapshot struct {
	AuthID  string
	URL     string
	Headers http.Header
	Body    []byte
}

type ClaudeCacheReplay func(context.Context, ClaudeCacheSnapshot) (cacheRead int64, err error)

// NewClaudeCacheUsageReporter marks each replay independently of concurrent chats.
func NewClaudeCacheUsageReporter(ctx context.Context, model string, auth *cliproxyauth.Auth) *UsageReporter {
	reporter := NewUsageReporter(ctx, "claude", model, auth)
	reporter.source = "claude-cache-keepalive"
	return reporter
}

type claudeCacheSession struct {
	snapshot   ClaudeCacheSnapshot
	lastPrompt string
	seen       time.Time
	anchor     time.Time
	interval   time.Duration
	ttl        time.Duration
	ready      bool
	paused     bool
	misses     int
	cancel     context.CancelFunc
}

// ClaudeCacheKeepalive owns bounded session snapshots and their renewal lifecycle.
type ClaudeCacheKeepalive struct {
	mu          sync.Mutex
	ctx         context.Context
	cancel      context.CancelFunc
	epoch       context.Context
	cancelEpoch context.CancelFunc
	enabled     bool
	running     bool
	sessions    map[[32]byte]*claudeCacheSession
	pending     map[[32]byte]*claudeCacheSession
	busy        map[[32]byte]int
	replay      ClaudeCacheReplay
	now         func() time.Time
	logs        []ClaudeCacheLogEvent
	logID       uint64
	disabled    map[string]bool
}

func NewClaudeCacheKeepalive(replay ClaudeCacheReplay) *ClaudeCacheKeepalive {
	ctx, cancel := context.WithCancel(context.Background())
	return &ClaudeCacheKeepalive{ctx: ctx, cancel: cancel, replay: replay, now: time.Now}
}

func (k *ClaudeCacheKeepalive) SetEnabled(enabled bool) {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.ctx.Err() != nil || k.enabled == enabled {
		return
	}
	if k.cancelEpoch != nil {
		k.cancelEpoch()
	}
	k.enabled = enabled
	k.sessions = make(map[[32]byte]*claudeCacheSession)
	k.pending = make(map[[32]byte]*claudeCacheSession)
	k.busy = make(map[[32]byte]int)
	if enabled {
		k.epoch, k.cancelEpoch = context.WithCancel(k.ctx)
	}
	outcome := "disabled"
	if enabled {
		outcome = "enabled"
	}
	k.appendLogLocked(ClaudeCacheLogEvent{Time: k.now().UTC(), Outcome: outcome})
}

func (k *ClaudeCacheKeepalive) Close() {
	if k != nil {
		k.SetEnabled(false)
		k.cancel()
	}
}

func (k *ClaudeCacheKeepalive) Run(ctx context.Context) {
	if k == nil {
		return
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	defer k.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case <-k.ctx.Done():
			return
		case <-ticker.C:
			k.ReplayDue(ctx, k.now())
		}
	}
}

// Begin marks a real request in flight. Only successful completion makes its
// snapshot eligible, and older completions cannot replace a newer request.
func (k *ClaudeCacheKeepalive) Begin(authID, sessionID string, req *http.Request, body []byte) func(bool) {
	noop := func(bool) {}
	if k == nil || req == nil || authID == "" {
		return noop
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.enabled {
		return noop
	}
	model := gjson.GetBytes(body, "model").String()
	if sessionID == "" {
		sessionID = ExtractClaudeCodeSessionID(req.Context(), body, req.Header)
	}
	if sessionID == "" {
		sessionID = gjson.GetBytes(body, "system").Raw + "\x00" + gjson.GetBytes(body, "tools").Raw
		for _, message := range gjson.GetBytes(body, "messages").Array() {
			if message.Get("role").String() == "user" {
				sessionID += "\x00" + message.Get("content").Raw
				break
			}
		}
	}
	id := sha256.Sum256([]byte(authID + "\x00" + req.URL.String() + "\x00" + model + "\x00" + sessionID))
	ttl := claudeCacheTTL(req.Header, body)
	eligible := req.Method == http.MethodPost && ttl != 0 && len(body) <= claudeCacheMaxBody
	if !eligible && k.sessions[id] == nil && k.pending[id] == nil {
		return noop
	}
	if old := k.sessions[id]; old != nil && old.cancel != nil {
		old.cancel()
		// A cancelled replay must not pause the retained successful snapshot.
		retained := *old
		retained.cancel = nil
		k.sessions[id] = &retained
	}
	item := &claudeCacheSession{}
	if eligible {
		headers := req.Header.Clone()
		for key := range headers {
			switch strings.ToLower(key) {
			case "authorization", "x-api-key", "proxy-authorization", "cookie", "content-length":
				delete(headers, key)
			}
		}
		interval := 4 * time.Minute
		if ttl == time.Hour {
			interval = 50 * time.Minute
		}
		now := k.now()
		item = &claudeCacheSession{
			snapshot: ClaudeCacheSnapshot{AuthID: authID, URL: req.URL.String(), Headers: headers, Body: append([]byte(nil), body...)},
			seen:     now, anchor: now, ttl: ttl, interval: interval, lastPrompt: claudeCacheLastPrompt(body),
		}
	}
	// Replace the last successful snapshot only after the newest chat succeeds.
	k.pending[id] = item
	k.busy[id]++
	epoch := k.epoch
	var once sync.Once
	return func(success bool) {
		once.Do(func() {
			k.mu.Lock()
			defer k.mu.Unlock()
			if k.epoch != epoch || !k.enabled {
				return
			}
			k.busy[id]--
			if k.busy[id] == 0 {
				delete(k.busy, id)
			}
			if k.pending[id] == item {
				delete(k.pending, id)
				if success && eligible {
					if k.sessions[id] == nil && len(k.sessions) >= claudeCacheMaxSessions {
						var oldestID [32]byte
						var oldest *claudeCacheSession
						for key, retained := range k.sessions {
							if oldest == nil || retained.seen.Before(oldest.seen) {
								oldestID, oldest = key, retained
							}
						}
						if oldest.cancel != nil {
							oldest.cancel()
						}
						delete(k.sessions, oldestID)
					}
					item.ready = true
					k.sessions[id] = item
					k.appendLogLocked(k.sessionLog(id, item, k.now(), "tracked"))
				}
			}
		})
	}
}

// ReplayDue renews unexpired snapshots. It is serialized independently of normal
// chats, and replay results apply only to the exact snapshot that was sent.
func (k *ClaudeCacheKeepalive) ReplayDue(ctx context.Context, now time.Time) {
	if k == nil {
		return
	}
	k.mu.Lock()
	if !k.enabled || k.running || k.replay == nil {
		k.mu.Unlock()
		return
	}
	k.running = true
	epoch := k.epoch
	keys := make([][32]byte, 0, len(k.sessions))
	for id := range k.sessions {
		keys = append(keys, id)
	}
	k.mu.Unlock()
	clockStart := k.now()
	defer func() {
		k.mu.Lock()
		k.running = false
		k.mu.Unlock()
	}()
	for _, id := range keys {
		started := now.Add(k.now().Sub(clockStart))
		k.mu.Lock()
		item := k.sessions[id]
		if !k.enabled || k.epoch != epoch || ctx.Err() != nil {
			k.mu.Unlock()
			return
		}
		if item == nil || !item.ready || k.disabled[hex.EncodeToString(id[:])] || item.paused || k.busy[id] > 0 || started.Sub(item.anchor) < item.interval {
			k.mu.Unlock()
			continue
		}
		if started.Sub(item.anchor) >= item.ttl {
			k.appendLogLocked(k.sessionLog(id, item, started, "expired"))
			delete(k.sessions, id)
			k.mu.Unlock()
			continue
		}
		pingCtx, cancel := context.WithCancel(epoch)
		stop := context.AfterFunc(ctx, cancel)
		item.cancel = cancel
		snapshot := item.snapshot
		snapshot.Body = append([]byte(nil), snapshot.Body...)
		snapshot.Headers = snapshot.Headers.Clone()
		k.mu.Unlock()
		replayStart := k.now()
		read, errReplay := k.replay(pingCtx, snapshot)
		duration := k.now().Sub(replayStart)
		wasCancelled := pingCtx.Err() != nil
		stop()
		cancel()
		k.mu.Lock()
		if k.sessions[id] == item {
			item.cancel = nil
			item.anchor = started
			if errReplay != nil {
				item.paused = true
			} else if read == 0 {
				item.misses++
				item.paused = item.misses >= 2
			} else {
				item.misses = 0
			}
			if item.paused {
				// Avoid logging upstream bodies, URLs, or account credentials.
				log.Warn("claude cache keepalive paused a session after a replay failure or repeated cache misses")
			}
		}
		event := k.sessionLog(id, item, started.Add(duration), claudeCacheLogOutcome(read, errReplay, wasCancelled))
		event.CacheReadTokens = read
		event.DurationMS = duration.Milliseconds()
		event.Paused = k.sessions[id] == item && item.paused
		event.StatusCode = claudeCacheLogStatus(errReplay)
		k.appendLogLocked(event)
		k.mu.Unlock()
	}
}

func ClaudeCacheReplayBody(body []byte) ([]byte, error) {
	return sjson.SetBytes(body, "max_tokens", 1)
}

func ClaudeCacheIsSubagent(headers http.Header, body []byte) bool {
	return IsClaudeSubagentRequest(headers, body) ||
		strings.EqualFold(HeaderValueCaseInsensitive(headers, "X-App"), "cli-bg") ||
		gjson.GetBytes(body, "task_budget").Exists() ||
		strings.Contains(strings.ToLower(gjson.GetBytes(body, "system").Raw), "cc_is_subagent=true")
}

func claudeCacheTTL(headers http.Header, body []byte) time.Duration {
	if !gjson.ValidBytes(body) || !gjson.GetBytes(body, "max_tokens").Exists() ||
		ClaudeCacheIsSubagent(headers, body) || gjson.GetBytes(body, "thinking.type").String() == "enabled" {
		return 0
	}
	for _, tool := range gjson.GetBytes(body, "tools").Array() {
		if kind := tool.Get("type").String(); kind != "" && kind != "custom" {
			return 0 // Server-side tools can perform billable work during a replay.
		}
	}
	var ttl time.Duration
	invalid := false
	observe := func(control gjson.Result) {
		if control.Get("type").String() != "ephemeral" {
			return
		}
		duration := 5 * time.Minute
		switch control.Get("ttl").String() {
		case "1h":
			duration = time.Hour
		case "", "5m":
		default:
			invalid = true
		}
		if ttl == 0 || duration < ttl {
			ttl = duration
		}
	}
	root := gjson.ParseBytes(body)
	observe(root.Get("cache_control"))
	for _, field := range []string{"system", "tools"} {
		for _, block := range root.Get(field).Array() {
			observe(block.Get("cache_control"))
		}
	}
	for _, message := range root.Get("messages").Array() {
		for _, block := range message.Get("content").Array() {
			observe(block.Get("cache_control"))
		}
	}
	if invalid {
		return 0
	}
	return ttl
}
