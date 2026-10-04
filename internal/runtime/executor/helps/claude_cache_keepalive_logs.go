package helps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/tidwall/gjson"
)

const claudeCacheLogCapacity = 200

// ClaudeCacheLogEvent contains operational metadata only, never request bodies or errors.
type ClaudeCacheLogEvent struct {
	ID              uint64    `json:"id"`
	Time            time.Time `json:"time"`
	Outcome         string    `json:"outcome"`
	Model           string    `json:"model,omitempty"`
	Account         string    `json:"account,omitempty"`
	Session         string    `json:"session,omitempty"`
	CacheReadTokens int64     `json:"cache_read_tokens"`
	DurationMS      int64     `json:"duration_ms"`
	Paused          bool      `json:"paused"`
	StatusCode      int       `json:"status_code,omitempty"`
}

type ClaudeCacheLogSnapshot struct {
	Enabled          bool                       `json:"enabled"`
	Sessions         int                        `json:"sessions"`
	PausedSessions   int                        `json:"paused_sessions"`
	DisabledSessions int                        `json:"disabled_sessions"`
	SessionDetails   []ClaudeCacheSessionDetail `json:"session_details"`
	Capacity         int                        `json:"capacity"`
	Events           []ClaudeCacheLogEvent      `json:"events"`
}

// LogSnapshot returns a non-destructive copy, newest first, independent of file logging.
func (k *ClaudeCacheKeepalive) LogSnapshot() ClaudeCacheLogSnapshot {
	out := ClaudeCacheLogSnapshot{Capacity: claudeCacheLogCapacity, Events: []ClaudeCacheLogEvent{}, SessionDetails: []ClaudeCacheSessionDetail{}}
	if k == nil {
		return out
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	out.Enabled, out.Sessions = k.enabled, len(k.sessions)
	out.DisabledSessions = len(k.disabled)
	out.SessionDetails = k.sessionDetailsLocked()
	for _, item := range k.sessions {
		if item.paused {
			out.PausedSessions++
		}
	}
	for i := len(k.logs) - 1; i >= 0; i-- {
		out.Events = append(out.Events, k.logs[i])
	}
	return out
}

func (k *ClaudeCacheKeepalive) appendLogLocked(event ClaudeCacheLogEvent) {
	k.logID++
	event.ID = k.logID
	if len(k.logs) == claudeCacheLogCapacity {
		copy(k.logs, k.logs[1:])
		k.logs[len(k.logs)-1] = event
	} else {
		k.logs = append(k.logs, event)
	}
	k.persistLocked()
}

func (k *ClaudeCacheKeepalive) sessionLog(id [32]byte, item *claudeCacheSession, at time.Time, outcome string) ClaudeCacheLogEvent {
	account := sha256.Sum256([]byte(item.snapshot.AuthID))
	model := gjson.GetBytes(item.snapshot.Body, "model").String()
	if len(model) > 128 {
		model = model[:128]
	}
	return ClaudeCacheLogEvent{
		Time: at.UTC(), Outcome: outcome, Model: model,
		Account: hex.EncodeToString(account[:6]), Session: hex.EncodeToString(id[:6]),
	}
}

func claudeCacheLogOutcome(read int64, err error, cancelled bool) string {
	if cancelled || errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if err != nil {
		return "failed"
	}
	if read == 0 {
		return "cache_miss"
	}
	return "renewed"
}

func claudeCacheLogStatus(err error) int {
	var status interface{ StatusCode() int }
	if errors.As(err, &status) {
		return status.StatusCode()
	}
	return 0
}
