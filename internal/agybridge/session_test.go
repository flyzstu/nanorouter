package agybridge

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSessionStorePutGetDelete(t *testing.T) {
	store := NewSessionStore("", time.Hour)
	if _, ok := store.Get("s1", "m1"); ok {
		t.Fatal("empty store returned a session")
	}
	store.Put("s1", "m1", "c1")
	if got, ok := store.Get("s1", "m1"); !ok || got != "c1" {
		t.Fatalf("get=%q ok=%v", got, ok)
	}
	if _, ok := store.Get("s1", "m2"); ok {
		t.Fatal("model must isolate session bindings")
	}
	store.Delete("s1", "m1")
	if _, ok := store.Get("s1", "m1"); ok {
		t.Fatal("delete did not remove binding")
	}
}

func TestSessionStoreTTL(t *testing.T) {
	store := NewSessionStore("", time.Hour)
	store.Put("s1", "m1", "c1")
	key := sessionStorageKey("s1", "m1")
	store.mu.Lock()
	entry := store.entries[key]
	entry.UpdatedAt = time.Now().Add(-2 * time.Hour)
	store.entries[key] = entry
	store.mu.Unlock()
	if _, ok := store.Get("s1", "m1"); ok {
		t.Fatal("expired binding should be ignored")
	}
}

func TestSessionStorePersistsWithoutRawKey(t *testing.T) {
	file := t.TempDir() + "/sessions.json"
	store := NewSessionStore(file, time.Hour)
	store.Put("raw-session-key", "m1", "c1")

	reloaded := NewSessionStore(file, time.Hour)
	if got, ok := reloaded.Get("raw-session-key", "m1"); !ok || got != "c1" {
		t.Fatalf("reloaded get=%q ok=%v", got, ok)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "raw-session-key") {
		t.Fatalf("session file leaked raw key: %s", data)
	}
	if !strings.Contains(string(data), "c1") {
		t.Fatalf("session file missing conversation id: %s", data)
	}
}

func TestSessionStoreConcurrentAccess(t *testing.T) {
	store := NewSessionStore("", time.Hour)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			store.Put("s1", "m1", "c1")
			_, _ = store.Get("s1", "m1")
		}()
	}
	wg.Wait()
	if got, ok := store.Get("s1", "m1"); !ok || got != "c1" {
		t.Fatalf("get=%q ok=%v", got, ok)
	}
}
