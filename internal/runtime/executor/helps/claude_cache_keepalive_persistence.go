package helps

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	claudeCachePersistenceFile  = ".claude-cache-keepalive.bin"
	claudeCachePersistenceKey   = ".claude-cache-keepalive.key"
	claudeCachePersistenceLimit = 48 << 20
	claudeCachePersistenceMagic = "CCK1"
)

var errClaudeCachePersistence = errors.New("claude cache keepalive persistence unavailable or invalid")

type claudeCachePersistence struct {
	dir     string
	aead    cipher.AEAD
	blocked bool
}

type claudeCachePersistedSession struct {
	ID       string
	Snapshot ClaudeCacheSnapshot
	Seen     time.Time
	Anchor   time.Time
	TTL      time.Duration
	Interval time.Duration
	Paused   bool
	Misses   int
}

type claudeCachePersistedState struct {
	Version  int
	Sessions []claudeCachePersistedSession
	Events   []ClaudeCacheLogEvent
	LogID    uint64
}

// SetPersistenceDir attaches encrypted snapshots and event history. Configure the
// enable switch and disabled-session preferences before attaching a directory.
// A failed load preserves both files and blocks writes until persistence detaches.
func (k *ClaudeCacheKeepalive) SetPersistenceDir(dir string) error {
	if k == nil {
		return nil
	}
	if dir != "" {
		resolved, err := filepath.Abs(dir)
		if err != nil {
			return errClaudeCachePersistence
		}
		dir = filepath.Clean(resolved)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.ctx.Err() != nil {
		return nil
	}
	if dir == "" {
		k.persistence = nil
		return nil
	}
	if k.persistence != nil && k.persistence.dir == dir {
		if k.persistence.blocked {
			return errClaudeCachePersistence
		}
		return nil
	}
	persistence := &claudeCachePersistence{dir: dir, blocked: true}
	k.persistence = persistence
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errClaudeCachePersistence
	}
	data, errData := claudeCacheReadFile(filepath.Join(dir, claudeCachePersistenceFile), claudeCachePersistenceLimit)
	if errData != nil && !errors.Is(errData, os.ErrNotExist) {
		return errClaudeCachePersistence
	}
	keyPath := filepath.Join(dir, claudeCachePersistenceKey)
	key, errKey := claudeCacheReadFile(keyPath, 32)
	if errors.Is(errKey, os.ErrNotExist) && errors.Is(errData, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return errClaudeCachePersistence
		}
		if err := claudeCacheAtomicWrite(keyPath, key); err != nil {
			return errClaudeCachePersistence
		}
	} else if errKey != nil || len(key) != 32 {
		return errClaudeCachePersistence
	}
	block, errCipher := aes.NewCipher(key)
	if errCipher != nil {
		return errClaudeCachePersistence
	}
	aead, errGCM := cipher.NewGCM(block)
	if errGCM != nil {
		return errClaudeCachePersistence
	}
	persistence.aead = aead
	if errData == nil {
		prefix := len(claudeCachePersistenceMagic)
		if len(data) < prefix+aead.NonceSize()+aead.Overhead() || string(data[:prefix]) != claudeCachePersistenceMagic {
			return errClaudeCachePersistence
		}
		raw, errOpen := aead.Open(nil, data[prefix:prefix+aead.NonceSize()], data[prefix+aead.NonceSize():], []byte(claudeCachePersistenceMagic))
		if errOpen != nil {
			return errClaudeCachePersistence
		}
		var state claudeCachePersistedState
		if err := json.Unmarshal(raw, &state); err != nil {
			return errClaudeCachePersistence
		}
		sessions, errRestore := claudeCacheRestoreState(state, k.now())
		if errRestore != nil {
			return errClaudeCachePersistence
		}
		if k.enabled {
			// Keep any newer in-memory success when changing the persistence directory.
			for id, item := range sessions {
				if current := k.sessions[id]; current == nil || current.seen.Before(item.seen) {
					k.sessions[id] = item
				}
			}
			for len(k.sessions) > claudeCacheMaxSessions {
				var oldestID [32]byte
				var oldest *claudeCacheSession
				for id, item := range k.sessions {
					if oldest == nil || item.seen.Before(oldest.seen) {
						oldestID, oldest = id, item
					}
				}
				if oldest.cancel != nil {
					oldest.cancel()
				}
				delete(k.sessions, oldestID)
			}
		}
		k.logs, k.logID = state.Events, state.LogID
	}
	if err := os.Chmod(keyPath, 0600); err != nil {
		return errClaudeCachePersistence
	}
	if errData == nil {
		if err := os.Chmod(filepath.Join(dir, claudeCachePersistenceFile), 0600); err != nil {
			return errClaudeCachePersistence
		}
	}
	persistence.blocked = false
	return k.savePersistenceLocked()
}

func claudeCacheRestoreState(state claudeCachePersistedState, now time.Time) (map[[32]byte]*claudeCacheSession, error) {
	if state.Version != 1 || len(state.Sessions) > claudeCacheMaxSessions || len(state.Events) > claudeCacheLogCapacity || state.LogID == ^uint64(0) {
		return nil, errClaudeCachePersistence
	}
	var lastID uint64
	for _, event := range state.Events {
		if event.ID <= lastID || event.ID > state.LogID || event.Time.IsZero() || len(event.Model) > 128 || len(event.Account) > 12 || len(event.Session) > 12 || len(event.Outcome) > 32 {
			return nil, errClaudeCachePersistence
		}
		lastID = event.ID
	}
	sessions := make(map[[32]byte]*claudeCacheSession, len(state.Sessions))
	seenIDs := make(map[[32]byte]bool, len(state.Sessions))
	for _, saved := range state.Sessions {
		decoded, errID := hex.DecodeString(saved.ID)
		if errID != nil || len(decoded) != 32 {
			return nil, errClaudeCachePersistence
		}
		id := [32]byte(decoded)
		if seenIDs[id] {
			return nil, errClaudeCachePersistence
		}
		seenIDs[id] = true
		snapshot := saved.Snapshot
		if len(snapshot.Body) == 0 || len(snapshot.Body) > claudeCacheMaxBody || snapshot.AuthID == "" || len(snapshot.AuthID) > 4096 || len(snapshot.URL) > 16384 {
			return nil, errClaudeCachePersistence
		}
		parsed, errURL := url.Parse(snapshot.URL)
		if errURL != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return nil, errClaudeCachePersistence
		}
		headerSize := 0
		for name, values := range snapshot.Headers {
			headerSize += len(name)
			for _, value := range values {
				headerSize += len(value)
			}
		}
		if headerSize > 64<<10 {
			return nil, errClaudeCachePersistence
		}
		snapshot.Headers = claudeCacheSnapshotHeaders(snapshot.Headers)
		ttl := claudeCacheTTL(snapshot.Headers, snapshot.Body)
		interval := 4 * time.Minute
		if ttl == time.Hour {
			interval = 50 * time.Minute
		}
		if ttl == 0 || ttl != saved.TTL || interval != saved.Interval || saved.Seen.IsZero() || saved.Anchor.IsZero() || saved.Seen.After(saved.Anchor) || saved.Anchor.After(now) || saved.Misses < 0 || saved.Misses > 2 || (saved.Misses == 2 && !saved.Paused) {
			return nil, errClaudeCachePersistence
		}
		if now.Sub(saved.Anchor) >= ttl {
			continue
		}
		sessions[id] = &claudeCacheSession{snapshot: snapshot, lastPrompt: claudeCacheLastPrompt(snapshot.Body), seen: saved.Seen, anchor: saved.Anchor, ttl: ttl, interval: interval, ready: true, paused: saved.Paused, misses: saved.Misses}
	}
	return sessions, nil
}

func (k *ClaudeCacheKeepalive) savePersistenceLocked() error {
	persistence := k.persistence
	if persistence == nil || persistence.blocked || k.ctx.Err() != nil {
		return nil
	}
	state := claudeCachePersistedState{Version: 1, Events: k.logs, LogID: k.logID}
	for id, item := range k.sessions {
		if !item.ready {
			continue
		}
		snapshot := item.snapshot
		snapshot.Headers = claudeCacheSnapshotHeaders(snapshot.Headers)
		state.Sessions = append(state.Sessions, claudeCachePersistedSession{ID: hex.EncodeToString(id[:]), Snapshot: snapshot, Seen: item.seen, Anchor: item.anchor, TTL: item.ttl, Interval: item.interval, Paused: item.paused, Misses: item.misses})
	}
	raw, errJSON := json.Marshal(state)
	if errJSON != nil || len(raw)+len(claudeCachePersistenceMagic)+persistence.aead.NonceSize()+persistence.aead.Overhead() > claudeCachePersistenceLimit {
		return errClaudeCachePersistence
	}
	nonce := make([]byte, persistence.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return errClaudeCachePersistence
	}
	data := append([]byte(claudeCachePersistenceMagic), nonce...)
	data = persistence.aead.Seal(data, nonce, raw, []byte(claudeCachePersistenceMagic))
	if err := claudeCacheAtomicWrite(filepath.Join(persistence.dir, claudeCachePersistenceFile), data); err != nil {
		return errClaudeCachePersistence
	}
	return nil
}

func (k *ClaudeCacheKeepalive) persistLocked() {
	if err := k.savePersistenceLocked(); err != nil {
		log.Warn("claude cache keepalive persistence could not be saved")
	}
}

func claudeCacheSnapshotHeaders(headers map[string][]string) map[string][]string {
	out := make(map[string][]string, len(headers))
	for key, values := range headers {
		switch strings.ToLower(key) {
		case "authorization", "x-api-key", "proxy-authorization", "cookie", "content-length":
			continue
		}
		out[key] = append([]string(nil), values...)
	}
	return out
}

func claudeCacheReadFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if errClose := file.Close(); errClose != nil {
			log.Warn("claude cache keepalive persistence file could not be closed")
		}
	}()
	data, errRead := io.ReadAll(io.LimitReader(file, limit+1))
	if errRead != nil {
		return nil, errRead
	}
	if int64(len(data)) > limit {
		return nil, errClaudeCachePersistence
	}
	return data, nil
}

func claudeCacheAtomicWrite(path string, data []byte) error {
	file, errCreate := os.CreateTemp(filepath.Dir(path), ".claude-cache-keepalive-*.tmp")
	if errCreate != nil {
		return errCreate
	}
	temp := file.Name()
	closed := false
	defer func() {
		if !closed {
			if errClose := file.Close(); errClose != nil {
				log.Warn("claude cache keepalive persistence file could not be closed")
			}
		}
		if errRemove := os.Remove(temp); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
			log.Warn("claude cache keepalive persistence temporary file could not be removed")
		}
	}()
	if err := file.Chmod(0600); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	errClose := file.Close()
	closed = true
	if errClose != nil {
		return errClose
	}
	return os.Rename(temp, path)
}
