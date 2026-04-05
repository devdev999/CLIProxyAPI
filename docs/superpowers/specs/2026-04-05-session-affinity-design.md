# Session Affinity for Claude Prompt Cache Optimization

**Date:** 2026-04-05
**Status:** Proposed
**Scope:** Claude provider routing only (extensible to others)

## Problem

The proxy routes successive requests from the same Claude Code conversation to different backend Claude accounts via round-robin. Anthropic's server-side prompt cache is scoped per-account — when conversation messages hit account A, then B, then A again, each account builds a separate cache. This doubles cache-creation costs and eliminates prompt caching savings.

## Solution

Add session-aware sticky routing: requests sharing the same `X-Claude-Code-Session-Id` header are routed to the same backend credential, with graceful failover and re-pin when the preferred credential is unavailable.

## Design

### Component 1: Session Affinity Cache

**File:** `internal/cache/session_affinity.go` (new, ~90 lines)

A flat `sessionID → Entry` cache (where Entry contains `authID` and `lastAccess` timestamp):

- **Storage:** `sync.Map` — optimized for append-mostly, high-read-concurrency workloads.
- **TTL:** `SessionAffinityTTL = 1 * time.Hour` (named constant), sliding expiration refreshed on every lookup hit.
- **Cleanup:** Background goroutine every 10 minutes purges expired entries (reuses `CacheCleanupInterval`).
- **Memory bounds:** Each entry is ~100 bytes (two UUID strings + timestamp). A hard cap of `maxAffinityEntries = 10000` is enforced via an `atomic.Int64` counter to prevent unbounded memory growth from caller-controlled session IDs. New sessions are rejected at capacity; existing sessions can still be updated. TTL-based expiration reclaims slots as sessions go idle.
- **API:**
  - `SetSessionAffinity(sessionID, authID)` — create or update mapping.
  - `GetSessionAffinity(sessionID) string` — returns authID or empty string; refreshes TTL on hit.
  - `ClearSessionAffinity(sessionID)` — explicit removal (for admin API / auth removal hooks).

### Component 2: PreferredAuthID in the Scheduler

**Files:**
- `sdk/cliproxy/executor/types.go` — add `PreferredAuthIDMetadataKey` constant (1 line)
- `sdk/cliproxy/auth/scheduler.go` — two-pass pick in `pickSingle` + `pickMixed` (~15 lines each)
- `sdk/cliproxy/auth/conductor.go` — two-pass pick in `pickNextLegacy` + `pickNextMixedLegacy` (~15 lines each)

A new metadata key `preferred_auth_id` enables a soft preference (unlike the existing `pinned_auth_id` which is a hard constraint).

#### Scheduler fast path (`pickSingle` / `pickMixed`)

The scheduler performs a two-pass pick:

1. **Pass 1 (optimistic):** Try the preferred auth. If it is ready and not in the `tried` set, return it immediately.
2. **Pass 2 (fallback):** Normal round-robin/fill-first selection — existing code, unchanged.

#### Legacy path (`pickNextLegacy` / `pickNextMixedLegacy`)

The legacy selector path must also handle `preferred_auth_id`. The approach mirrors how `pinnedAuthID` is already handled (conductor.go:2407): filter candidates for the preferred auth first. If no ready candidates match, rebuild the candidate list without the preference and proceed through normal `selector.Pick`.

#### Key behaviors (both paths):
- Preferred available → returned immediately (prompt cache hit on Anthropic's side).
- Preferred in cooldown → silently falls back to normal selection (re-pin moment).
- No preference set → existing behavior, zero overhead (the `if` is skipped).
- `pinnedAuthID` retains precedence — hard-pin semantics are unchanged.

### Component 3: Handler Integration

**File:** `sdk/api/handlers/handlers.go` (~25 lines across `ExecuteWithAuthManager`, `ExecuteStreamWithAuthManager`, and `ExecuteCountWithAuthManager`)

#### Affinity update strategy

The affinity cache is updated **after successful execution**, not via `WithSelectedAuthIDCallback`. This is deliberate: the callback fires on every scheduler pick inside the retry loop (conductor.go:1197/1275/1361), before execution succeeds or fails. Using the callback would create a brief window where concurrent requests could read a stale affinity pointing at a failing credential.

Instead, after `Execute`/`ExecuteStream`/`ExecuteCount` returns successfully, the handler reads `SelectedAuthMetadataKey` from `opts.Metadata` and calls `SetSessionAffinity(sessionID, selectedAuthID)`.

#### Request flow:

1. Extract `X-Claude-Code-Session-Id` from incoming request headers.
2. Look up `GetSessionAffinity(sessionID)` → `preferredAuthID`.
3. Set `preferred_auth_id` in execution metadata (`opts.Metadata`).
4. Execute normally — scheduler uses preferred if available, falls back if not.
5. On success: read `selected_auth_id` from `opts.Metadata` → `SetSessionAffinity(sessionID, selectedAuthID)`:
   - If `selectedAuthID != preferredAuthID`: re-pinned to new credential (updates mapping and TTL).
   - If `selectedAuthID == preferredAuthID`: no action needed (TTL already refreshed by `GetSessionAffinity`).

#### Covered execution paths:
- `ExecuteWithAuthManager` — non-streaming Claude messages.
- `ExecuteStreamWithAuthManager` — streaming Claude messages (including bootstrap retries; affinity is only written after final success, so retries re-read the same preference from the cache).
- `ExecuteCountWithAuthManager` — token counting (`/v1/messages/count_tokens`). Token counting benefits from affinity because Anthropic's cache lookup still applies.

#### Non-Claude-Code clients:
If `X-Claude-Code-Session-Id` is absent, no affinity is applied. Zero impact on existing behavior.

#### Header disambiguation:
The client-supplied `X-Claude-Code-Session-Id` is used **only for internal routing affinity** (the `preferred_auth_id` metadata key). The outbound `X-Claude-Code-Session-Id` sent to Anthropic is set via `EnsureHeader` (claude_executor.go:886), which prefers the client's inbound header value when present; when absent, it falls back to a per-credential value from `helps.CachedSessionID(apiKey)`. The internal routing affinity key (`preferred_auth_id` in metadata) is never sent upstream — it exists only in the execution metadata for scheduler consumption.

## Edge Cases

| Scenario | Behavior |
|----------|----------|
| New session (no affinity yet) | Normal round-robin picks auth, post-success metadata read caches it |
| Preferred auth in cooldown | Falls back to round-robin, re-pins to new auth |
| Preferred auth removed from config (hot reload) | Scheduler can't find it, falls through to normal pick |
| Proxy restart (cache lost) | First request per session gets a new auth — one cache miss, then re-pinned |
| Multiple proxy instances (no shared state) | Each instance builds its own affinity — acceptable for single-instance; shared storage is future work |
| Bootstrap retry (streaming) | Affinity only written after success; retries re-read same preference from cache |
| Concurrent requests for same session during re-pin | Brief window where both old and new auth may be tried; resolves on next request |

## Observability

- `debug` log when preferred auth is used (cache hit).
- `info` log when preferred auth is unavailable and fallback occurs (re-pin event — signals prompt cache invalidation).

## Files Changed

| File | Change | Lines |
|------|--------|-------|
| `internal/cache/session_affinity.go` | New — affinity cache | ~90 |
| `sdk/cliproxy/executor/types.go` | Add `PreferredAuthIDMetadataKey` constant | ~1 |
| `sdk/cliproxy/auth/scheduler.go` | Two-pass preferred pick in `pickSingle` + `pickMixed` | ~30 |
| `sdk/cliproxy/auth/conductor.go` | Two-pass preferred pick in `pickNextLegacy` + `pickNextMixedLegacy` | ~30 |
| `sdk/api/handlers/handlers.go` | Extract session ID, set preferred metadata, update cache post-success | ~25 |

**Total: ~175 lines across 5 files. No breaking changes.**

## Out of Scope

- Distributed affinity store (Redis/Postgres) for multi-instance deployments.
- Config toggle to disable affinity (absent session ID = no affinity = existing behavior).
- Affinity for non-Claude providers (extensible by adding session ID extraction to other handlers).
