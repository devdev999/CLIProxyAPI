package cache

import (
	"sync"
	"sync/atomic"
	"time"
)

// SessionAffinityTTL is how long a session-to-auth mapping remains valid.
const SessionAffinityTTL = 1 * time.Hour

// maxAffinityEntries caps the number of concurrent session affinity mappings
// to bound memory usage from caller-controlled session IDs.
const maxAffinityEntries = 10000

// affinityEntry holds a cached session-to-auth mapping with timestamp.
type affinityEntry struct {
	AuthID    string
	Timestamp time.Time
}

// sessionAffinityCache maps sessionID → affinityEntry with sliding TTL.
var sessionAffinityCache sync.Map

// affinityCount tracks the approximate number of entries for cap enforcement.
var affinityCount atomic.Int64

// affinityCleanupOnce ensures the background cleanup goroutine starts only once.
var affinityCleanupOnce sync.Once

// SetSessionAffinity stores or updates the auth ID for a given session.
func SetSessionAffinity(sessionID, authID string) {
	if sessionID == "" || authID == "" {
		return
	}
	affinityCleanupOnce.Do(startAffinityCleanup)
	entry := affinityEntry{AuthID: authID, Timestamp: time.Now()}
	// Swap atomically replaces any existing value or inserts a new one.
	// Using Swap instead of Load+Store avoids a TOCTOU race where a concurrent
	// delete between the two calls would re-insert without incrementing the counter.
	if _, loaded := sessionAffinityCache.Swap(sessionID, entry); !loaded {
		// New entry — increment counter and enforce cap.
		if affinityCount.Add(1) > maxAffinityEntries {
			// Over cap; remove this specific entry only if it hasn't been
			// replaced by another goroutine since we inserted it.
			if sessionAffinityCache.CompareAndDelete(sessionID, entry) {
				affinityCount.Add(-1)
			}
		}
	}
}

// GetSessionAffinity retrieves the cached auth ID for a given session.
// Returns empty string if not found or expired. Refreshes TTL on hit (sliding expiration).
func GetSessionAffinity(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	raw, ok := sessionAffinityCache.Load(sessionID)
	if !ok {
		return ""
	}
	entry := raw.(affinityEntry)
	now := time.Now()
	if now.Sub(entry.Timestamp) > SessionAffinityTTL {
		// CompareAndDelete only removes the entry if it still holds the same
		// (expired) value — a concurrent refresh won't be clobbered.
		if sessionAffinityCache.CompareAndDelete(sessionID, entry) {
			affinityCount.Add(-1)
		}
		return ""
	}
	// Refresh TTL on access (sliding expiration).
	entry.Timestamp = now
	sessionAffinityCache.Store(sessionID, entry)
	return entry.AuthID
}

// ClearSessionAffinity removes the affinity mapping for a given session.
func ClearSessionAffinity(sessionID string) {
	if sessionID == "" {
		return
	}
	if _, loaded := sessionAffinityCache.LoadAndDelete(sessionID); loaded {
		affinityCount.Add(-1)
	}
}

// startAffinityCleanup launches a background goroutine that periodically
// removes expired session affinity entries.
func startAffinityCleanup() {
	go func() {
		ticker := time.NewTicker(CacheCleanupInterval)
		defer ticker.Stop()
		for range ticker.C {
			purgeExpiredAffinities()
		}
	}()
}

// purgeExpiredAffinities removes all expired session affinity entries.
func purgeExpiredAffinities() {
	now := time.Now()
	sessionAffinityCache.Range(func(key, value any) bool {
		entry := value.(affinityEntry)
		if now.Sub(entry.Timestamp) > SessionAffinityTTL {
			if sessionAffinityCache.CompareAndDelete(key, entry) {
				affinityCount.Add(-1)
			}
		}
		return true
	})
}
