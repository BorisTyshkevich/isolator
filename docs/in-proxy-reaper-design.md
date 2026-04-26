# In-proxy reaper: design discussion

> **Status:** Open design — soliciting feedback before implementation.
> **Goal:** Reliable container cleanup that does not depend on the
> testcontainers Ryuk container.

## Why

testcontainers' Ryuk reaper is broken on macOS for testcontainers-go users
(see `ryuk-and-the-proxy-socket.md`). Without Ryuk, every crashed test run
leaks containers — exactly the failure mode that piled up 211 leaked
ClickHouse containers and 50 GB of disk last week before we cleaned house.

Rather than wait for upstream testcontainers-go [#3662](https://github.com/testcontainers/testcontainers-go/issues/3662),
build cleanup into the proxy itself. Same outcome, no third-party reaper
container, works on every platform, removes a moving part from our trust
boundary.

## What Ryuk gives us, and what to preserve

Ryuk's value is two things:

1. **Liveness-tied cleanup.** Containers are reaped when the test process
   stops being alive — even on `kill -9`, OOM, laptop crash. The TCP socket
   teardown is the heartbeat.
2. **Per-test-run scoping.** Each test session gets its own UUID label;
   only that session's containers get reaped. Two test runs can coexist
   without stepping on each other.

The in-proxy reaper has to deliver (1) at minimum. (2) is desirable but
optional — we can ship a coarser version first and refine.

## Three design options

### Option A — Time-based TTL

Every container the proxy creates gets an age-based TTL. A background
sweeper runs every minute and force-removes any user-owned container whose
age exceeds the TTL.

| | |
|---|---|
| **State needed** | None (containers carry the TTL info as labels) |
| **Heartbeat** | None — pure time |
| **Granularity** | Per container, fixed at creation |
| **Pros** | Trivial to implement; no inter-process coordination; survives proxy restarts |
| **Cons** | Long-running interactive containers get killed; need to choose TTL carefully; chains of dependent containers get reaped at the same time regardless of usage |

**TTL choice:** 1 hour for tests is generous; 24 hours for interactive use
is sloppy. A single fixed value is hard. Per-container hints via labels
(`isolator.ttl=24h`) help but require user discipline.

### Option B — Activity-based (peer UID)

The proxy tracks last-activity timestamp per UID (every request resets it).
A sweeper periodically checks: for each user, if the last-activity
timestamp is older than N seconds AND there are no active proxy
connections from that user, reap their owned containers.

| | |
|---|---|
| **State needed** | In-memory map: UID → last-activity timestamp + active-conn count |
| **Heartbeat** | Implicit — every API call refreshes the timestamp |
| **Granularity** | Per user (all the user's containers reaped at once when idle) |
| **Pros** | Closest to "process is gone" semantics; survives crashes naturally (no requests = idle = reap); long-running interactive use is preserved as long as the agent is making requests |
| **Cons** | All-or-nothing per user — can't have one test session that finishes while another is still running. Multiple agents per UID share fate (they always do, but here it manifests as cleanup). State is in-memory, lost across proxy restarts. |

**Idle threshold:** ~5 minutes feels right. Tests pause for less, idle
sessions exceed it. Configurable per deployment.

### Option C — Session-based (mimics Ryuk fully)

Each *connection* to the proxy gets a session UUID. Containers created via
that connection are tagged with the session. When the connection drops
(any reason — clean exit, crash, network) the proxy reaps containers
labeled with that session ID after a short grace period.

| | |
|---|---|
| **State needed** | Per-connection: session UUID. Per-session: created container IDs |
| **Heartbeat** | Connection presence (TCP/Unix socket lifetime) |
| **Granularity** | Per test-session (Ryuk-equivalent) |
| **Pros** | Multiple test runs in the same UID coexist cleanly; closest to Ryuk's behavior; semantically clean |
| **Cons** | Needs the client to use a single connection per test session — testcontainers-go opens a connection per request. Either we accept fragility (reap on first idle period after last conn drop) or define a session protocol the client opts into. State complexity higher. |

**The connection-per-request issue is real.** Go's `http.Transport` pools
connections and they can drop arbitrarily. Without an explicit session
protocol, "session ends when connection drops" doesn't map to "test run
ended."

## Recommendation

**Option B** with a **session-protocol bolt-on later** if needed.

Reasoning:

- Option B handles the dominant case: tests run, agent makes requests, then
  goes idle when tests finish. No protocol changes for clients. Works for
  any tool, not just testcontainers.
- Option C's value is per-test-run granularity, but most isolator users
  run one test session at a time per user. Multi-session-per-user is rare
  and can be added later as a session-ID label that complements (not
  replaces) the UID-idle rule.
- Option A is too crude for interactive use (Claude agents can sit idle
  for hours mid-session, but their containers shouldn't die).

A label-based exemption (`isolator.no-reap=true`) lets users opt
specific containers out of cleanup — useful for long-lived database
containers shared across runs.

## Proposed implementation sketch

```
internal/proxy/reaper/
    reaper.go           — sweeper goroutine: list user-owned containers, decide, reap
    activity.go         — UID activity tracker (atomic timestamps)
    config.go           — IdleThreshold, SweepInterval, exempt-label name
    reaper_test.go      — fake clock + mock upstream
```

Wired in `cmd/docker-proxy/main.go`:

```go
reaper := reaper.New(reaperConfig, upstreamSocket, username)
go reaper.Run(ctx)
```

The handler updates activity on every request:

```go
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    h.activity.Touch()
    // ... existing logic
}
```

When the reaper decides to sweep:

1. List containers with `Labels[OwnerLabel] == h.user` via subrequest
2. Skip any with the exempt label
3. For each remaining: `DELETE /v1.51/containers/<id>?force=true`
4. Log: `[user] reaped <id> (idle <duration>)`

Sweep frequency 30s; idle threshold 5min default, override via flag.

State is in-memory only — proxy restart resets the activity timer (which is
fine; restart implies a clean break).

## Risks and mitigations

| Risk | Mitigation |
|------|------------|
| Reaping a container the user is actively using | 5min idle threshold; user can refresh by making any docker call (`docker ps`) |
| Reaping a long-lived integration env (e.g. shared dev ClickHouse) | Exempt label `isolator.no-reap=true` |
| Proxy restart resets timer — too eager / too lazy? | Sweep doesn't run for first `IdleThreshold` after start; gives in-flight tests a grace period |
| Concurrent sweeps from multiple proxy instances | Only one proxy per user, enforced by Unix socket binding |
| Race: reap fires while user is creating a new container | Container creation is atomic at upstream; either the create finishes and gets owner-labeled (not reaped this round), or it loses the race and the next sweep round catches stale ones (delay <60s) |

## Open questions

1. **Idle threshold default.** 5 minutes is a guess. Should it be
   per-user via config? CI runners might want shorter; interactive Claude
   sessions might want longer.
2. **Should the sweep also handle networks and volumes?** Ryuk reaps all
   three by session label. We could match that — list user-owned networks
   and remove unused ones, list user-owned volumes ditto. Adds code but
   prevents network/volume leaks.
3. **Logging granularity.** Per-reap log line is good for audit but noisy
   under heavy churn. Aggregate count after each sweep instead?
4. **Interaction with `iso delete <user>`.** Today `iso delete` removes
   the user account. Should it also reap any containers the user still
   owned at delete time, or assume the in-proxy reaper has already done
   it? Probably reap defensively in `iso delete` — user might be deleted
   while proxy is down.
5. **Behavior when the proxy crashes mid-sweep.** A partial sweep is fine
   (next round catches the rest). No state corruption to worry about.

## What's NOT in scope

- **Cross-user reaping.** The proxy is per-user; one user's reaper never
  touches another user's containers. Cross-user cleanup (e.g., when a
  user account is deleted) belongs in `iso delete`, not the proxy.
- **Image/build cache GC.** Images and build cache are shared across
  users. Standard `docker system prune` belongs at the macOS-host
  admin level, not per-proxy.
- **Replacing Ryuk for users who already have it working.** On Linux
  testcontainers-go, Ryuk works fine via the bind path. Users can keep
  using it. The in-proxy reaper is the macOS unblock and a Linux backup.

## Decisions wanted before implementation

1. Option B vs A vs C — does B feel right, or do we want per-session granularity?
2. Idle threshold default (5min? 10min? configurable per user?).
3. Reap networks/volumes too, or just containers in v1?
4. Exempt label name — `isolator.no-reap=true` vs something else?
