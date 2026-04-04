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

**File:** `internal/cache/session_affinity.go` (new, ~80 lines)

A flat `sessionID → authID` cache following the same pattern as `signature_cache.go`:

- **Storage:** `sync.Map` — optimized for append-mostly, high-read-concurrency workloads.
- **TTL:** 1-hour sliding expiration, refreshed on every lookup hit.
- **Cleanup:** Background goroutine every 10 minutes purges expired entries.
- **API:**
  - `SetSessionAffinity(sessionID, authID)` — create or update mapping.
  - `GetSessionAffinity(sessionID) string` — returns authID or empty string; refreshes TTL on hit.
  - `ClearSessionAffinity(sessionID)` — explicit removal.

### Component 2: PreferredAuthID in the Scheduler

**Files:**
- `sdk/cliproxy/executor/types.go` — add `PreferredAuthIDMetadataKey` constant (1 line)
- `sdk/cliproxy/auth/scheduler.go` — two-pass pick in `pickSingle` + `pickMixed` (~15 lines each)

A new metadata key `preferred_auth_id` enables a soft preference (unlike the existing `pinned_auth_id` which is a hard constraint). The scheduler performs a two-pass pick:

1. **Pass 1 (optimistic):** Try the preferred auth. If it is ready and not in the `tried` set, return it immediately.
2. **Pass 2 (fallback):** Normal round-robin/fill-first selection — existing code, unchanged.

Key behaviors:
- Preferred available → returned immediately (prompt cache hit on Anthropic's side).
- Preferred in cooldown → silently falls back to normal selection (re-pin moment).
- No preference set → existing behavior, zero overhead (the `if` is skipped).
- `pinnedAuthID` retains precedence — hard-pin semantics are unchanged.

### Component 3: Handler Integration

**File:** `sdk/api/handlers/handlers.go` (~20 lines in `ExecuteWithAuthManager` / `ExecuteStreamWithAuthManager`)

Request flow:

1. Extract `X-Claude-Code-Session-Id` from incoming request headers.
2. Look up `GetSessionAffinity(sessionID)` → `preferredAuthID`.
3. Set `preferred_auth_id` in execution metadata.
4. Register `WithSelectedAuthIDCallback` to capture which auth was actually selected.
5. Execute normally — scheduler uses preferred if available, falls back if not.
6. Callback fires → `SetSessionAffinity(sessionID, selectedAuthID)`:
   - If `selectedAuthID == preferredAuthID`: cache refreshed (sliding TTL).
   - If `selectedAuthID != preferredAuthID`: re-pinned to new credential.

Non-Claude-Code clients: if `X-Claude-Code-Session-Id` is absent, no affinity is applied. Zero impact on existing behavior.

## Edge Cases

| Scenario | Behavior |
|----------|----------|
| New session (no affinity yet) | Normal round-robin picks auth, callback caches it |
| Preferred auth in cooldown | Falls back to round-robin, re-pins to new auth |
| Preferred auth removed from config (hot reload) | Scheduler can't find it, falls through to normal pick |
| Proxy restart (cache lost) | First request per session gets a new auth — one cache miss, then re-pinned |
| Multiple proxy instances (no shared state) | Each instance builds its own affinity — acceptable for single-instance; shared storage is future work |

## Observability

- `debug` log when preferred auth is used (cache hit).
- `info` log when preferred auth is unavailable and fallback occurs (re-pin event — signals prompt cache invalidation).

## Files Changed

| File | Change | Lines |
|------|--------|-------|
| `internal/cache/session_affinity.go` | New — affinity cache | ~80 |
| `sdk/cliproxy/executor/types.go` | Add `PreferredAuthIDMetadataKey` constant | ~1 |
| `sdk/cliproxy/auth/scheduler.go` | Two-pass preferred pick in `pickSingle` + `pickMixed` | ~30 |
| `sdk/api/handlers/handlers.go` | Extract session ID, set preferred metadata, register callback | ~20 |

**Total: ~130 lines across 4 files. No breaking changes.**

## Out of Scope

- Distributed affinity store (Redis/Postgres) for multi-instance deployments.
- Config toggle to disable affinity (absent session ID = no affinity = existing behavior).
- Affinity for non-Claude providers (extensible by adding session ID extraction to other handlers).
