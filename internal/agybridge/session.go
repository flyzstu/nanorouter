package agybridge

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"time"

	json "github.com/goccy/go-json"
)

// SessionEntry 是外部会话键到 agy conversation 的绑定。
type SessionEntry struct {
	ConversationID string    `json:"conversation_id"`
	Model          string    `json:"model"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// SessionStore 把 X-Session-Affinity / X-Session-Id 映射到 agy conversation ID。
// 文件落盘使用会话键的 SHA-256，不保存原始会话键。
type SessionStore struct {
	mu      sync.Mutex
	entries map[string]SessionEntry
	ttl     time.Duration
	file    string
}

func NewSessionStore(file string, ttl time.Duration) *SessionStore {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	store := &SessionStore{entries: map[string]SessionEntry{}, ttl: ttl, file: file}
	store.load()
	return store
}

func (s *SessionStore) Get(sessionKey, model string) (string, bool) {
	if sessionKey == "" {
		return "", false
	}
	key := sessionStorageKey(sessionKey, model)
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	if !ok {
		return "", false
	}
	if time.Since(entry.UpdatedAt) > s.ttl {
		delete(s.entries, key)
		return "", false
	}
	return entry.ConversationID, entry.ConversationID != ""
}

func (s *SessionStore) Put(sessionKey, model, conversationID string) {
	if sessionKey == "" || conversationID == "" {
		return
	}
	key := sessionStorageKey(sessionKey, model)
	s.mu.Lock()
	s.entries[key] = SessionEntry{ConversationID: conversationID, Model: model, UpdatedAt: time.Now()}
	s.mu.Unlock()
	s.save()
}

func (s *SessionStore) Delete(sessionKey, model string) {
	if sessionKey == "" {
		return
	}
	key := sessionStorageKey(sessionKey, model)
	s.mu.Lock()
	delete(s.entries, key)
	s.mu.Unlock()
	s.save()
}

func (s *SessionStore) load() {
	if s.file == "" {
		return
	}
	data, err := os.ReadFile(s.file)
	if err != nil {
		return
	}
	var entries map[string]SessionEntry
	if json.Unmarshal(data, &entries) != nil {
		return
	}
	if entries != nil {
		s.entries = entries
	}
}

func (s *SessionStore) save() {
	if s.file == "" {
		return
	}
	s.mu.Lock()
	data, err := json.Marshal(s.entries)
	s.mu.Unlock()
	if err != nil {
		return
	}
	dir := filepath.Dir(s.file)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return
		}
	}
	tmp := s.file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, s.file)
}

func sessionStorageKey(sessionKey, model string) string {
	sum := sha256.Sum256([]byte(sessionKey + "\x00" + model))
	return hex.EncodeToString(sum[:])
}
