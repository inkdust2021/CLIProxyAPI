package helps

import (
	"encoding/hex"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// ClaudeCacheSessionDetail describes one current snapshot, not its event history.
// Prompt previews are derived from snapshots and exposed through authenticated management.
type ClaudeCacheSessionDetail struct {
	ID           string     `json:"id"`
	Model        string     `json:"model"`
	Account      string     `json:"account"`
	Session      string     `json:"session"`
	LastPrompt   string     `json:"last_prompt"`
	State        string     `json:"state"`
	LastActivity time.Time  `json:"last_activity,omitempty"`
	NextRenewal  *time.Time `json:"next_renewal,omitempty"`
}

func (k *ClaudeCacheKeepalive) sessionDetailsLocked() []ClaudeCacheSessionDetail {
	details := make([]ClaudeCacheSessionDetail, 0, len(k.sessions)+len(k.disabled))
	now := k.now()
	present := make(map[string]bool, len(k.sessions))
	for id, item := range k.sessions {
		key := hex.EncodeToString(id[:])
		present[key] = true
		event := k.sessionLog(id, item, now, "")
		detail := ClaudeCacheSessionDetail{ID: key, Model: event.Model, Account: event.Account, Session: event.Session, LastPrompt: item.lastPrompt, LastActivity: item.seen.UTC(), State: "active"}
		switch {
		case k.disabled[key]:
			detail.State = "disabled"
		case now.Sub(item.anchor) >= item.ttl:
			detail.State = "expired"
		case item.paused:
			detail.State = "paused"
		case item.cancel != nil:
			detail.State = "renewing"
		case k.busy[id] > 0:
			detail.State = "updating"
		}
		if k.enabled && detail.State == "active" {
			due := item.anchor.Add(item.interval).UTC()
			detail.NextRenewal = &due
		}
		details = append(details, detail)
	}
	for key := range k.disabled {
		if !present[key] {
			details = append(details, ClaudeCacheSessionDetail{ID: key, Session: key[:12], State: "disabled"})
		}
	}
	sort.Slice(details, func(i, j int) bool {
		if details[i].LastActivity.Equal(details[j].LastActivity) {
			return details[i].ID < details[j].ID
		}
		return details[i].LastActivity.After(details[j].LastActivity)
	})
	return details
}

func claudeCacheLastPrompt(body []byte) string {
	messages := gjson.GetBytes(body, "messages").Array()
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Get("role").String() != "user" {
			continue
		}
		content := messages[i].Get("content")
		var parts []string
		if content.Type == gjson.String {
			parts = append(parts, content.String())
		} else {
			for _, block := range content.Array() {
				if block.Get("type").String() == "text" {
					parts = append(parts, block.Get("text").String())
				}
			}
		}
		text := strings.TrimSpace(strings.Join(parts, "\n"))
		if text == "" {
			continue
		}
		runes := []rune(text)
		if len(runes) > 2000 {
			return string(runes[:1999]) + "…"
		}
		return text
	}
	return ""
}

// SetDisabledSessions applies operator preferences without discarding valid snapshots.
// Session preferences are supplied by configuration, separately from encrypted snapshots.
func (k *ClaudeCacheKeepalive) SetDisabledSessions(ids []string) {
	if k == nil {
		return
	}
	next := make(map[string]bool, len(ids))
	for _, id := range ids {
		decoded, err := hex.DecodeString(id)
		if err == nil && len(decoded) == 32 {
			next[hex.EncodeToString(decoded)] = true
		}
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.ctx.Err() != nil {
		return
	}
	for id, item := range k.sessions {
		key := hex.EncodeToString(id[:])
		if k.disabled[key] == next[key] {
			continue
		}
		retained := *item
		if item.cancel != nil {
			item.cancel()
			retained.cancel = nil
		}
		if !next[key] {
			retained.paused = false
			retained.misses = 0
		}
		k.sessions[id] = &retained
		outcome := "session_enabled"
		if next[key] {
			outcome = "session_disabled"
		}
		k.appendLogLocked(k.sessionLog(id, &retained, k.now(), outcome))
	}
	k.disabled = next
	k.persistLocked()
}

// DeleteSession removes a snapshot and its preference without removing event history.
// Pending completions are invalidated, but their in-flight counts remain until they finish.
func (k *ClaudeCacheKeepalive) DeleteSession(key string) (bool, error) {
	if k == nil {
		return false, nil
	}
	decoded, err := hex.DecodeString(key)
	if err != nil || len(decoded) != 32 || key != strings.ToLower(key) {
		return false, nil
	}
	id := [32]byte(decoded)
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.ctx.Err() != nil || (k.persistence != nil && k.persistence.blocked) {
		return false, errClaudeCachePersistence
	}
	item, pending, disabled := k.sessions[id], k.pending[id], k.disabled[key]
	if item == nil && pending == nil && !disabled {
		return false, nil
	}
	delete(k.sessions, id)
	delete(k.pending, id)
	delete(k.disabled, key)
	if errSave := k.savePersistenceLocked(); errSave != nil {
		if item != nil {
			k.sessions[id] = item
		}
		if pending != nil {
			k.pending[id] = pending
		}
		if disabled {
			k.disabled[key] = true
		}
		return false, errSave
	}
	if item != nil && item.cancel != nil {
		item.cancel()
	}
	return true, nil
}
