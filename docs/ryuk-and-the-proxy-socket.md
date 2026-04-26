# Ryuk and the proxy socket

This document explains how Ryuk (the testcontainers cleanup process) interacts
with the isolator docker-proxy, why we allow it to bind-mount the per-user
proxy socket, and the security analysis that justifies the carve-out.

The intended audience is anyone running tests under isolator who needs to know
why testcontainers works the way it does — and security reviewers asking "why
are you mounting a Docker socket into a container".

---

## What Ryuk is

Ryuk is a tiny container that the
[testcontainers](https://www.testcontainers.org/) library starts before any of
your test containers. Its single job: when your test process dies, delete
every container, network, and volume your tests created.

It exists because container leaks are a chronic problem with integration
tests. Tests can fail in ways the test framework can't intercept:

- The test JVM/Python interpreter crashes
- CI sends `SIGKILL` after the timeout fires
- A laptop runs out of battery mid-run
- An assertion library throws an exception the test framework doesn't catch
- A developer hits `Ctrl-C` at the wrong moment

In each case the `try { ... } finally { container.stop(); }` blocks never run
and the container keeps running. Multiply by hundreds of test runs and you
end up with hundreds of orphaned ClickHouse / Postgres / Kafka / etc.
containers, each holding RAM and disk.

(We learned this the hard way: 211 leaked containers on this host before we
added Ryuk back.)

## How Ryuk works

Ryuk is a dead man's switch built on TCP socket lifetime.

```
┌───────────────┐       TCP localhost:NNNN       ┌──────────────────┐
│  test process │  ─────────────────────────────►│  ryuk container  │
│  (JVM/Python) │                                │                  │
└───────┬───────┘  on disconnect, ryuk reaps     │  /var/run/...sock│
        │          all containers/networks       │       │          │
        │          matching session-id label     │       ▼          │
        │                                        │  Docker daemon   │
        ▼                                        │  (cleanup ops)   │
   creates labeled                               └──────────────────┘
   containers
```

1. testcontainers creates one Ryuk container per test process. It generates a
   unique `session-id` UUID and attaches it as a Docker label on every
   subsequent container, network, and volume.
2. The test process opens a TCP connection to Ryuk and sends a list of label
   filters: *"watch for resources with `org.testcontainers.session-id=<uuid>`"*.
3. Ryuk records the filters and **holds the TCP connection open**.
4. The instant the connection drops — for any reason — Ryuk calls
   `DELETE /containers/{id}?force=true` and similar endpoints for every
   resource matching the filters.

The TCP connection itself is the heartbeat. The kernel handles closing
sockets when a process dies; no application-level code needs to run for
cleanup to fire. This is the same trick `ssh` uses for cleanup-on-disconnect
and what makes the design unkillable.

## Why Ryuk needs a Docker socket

To delete containers, Ryuk has to talk to the Docker daemon. Inside the
container, "the Docker daemon" must be reachable through the same socket the
test process is using. testcontainers does this by:

1. Bind-mounting the host's Docker socket into the Ryuk container at the same
   path it has on the host.
2. Setting `DOCKER_HOST=unix:///var/run/docker.sock` (or whatever path the
   test process used) inside Ryuk.

Without that bind, Ryuk has nothing to talk to — it's a Docker client with no
daemon — and testcontainers degrades by either failing to start Ryuk or by
ignoring it. The "fail closed" path is for users to set
`TESTCONTAINERS_RYUK_DISABLED=true`, which is the path that produced our
50 GB pile of leaked containers.

## The isolator twist

In our setup, the test process is **not** talking to `/var/run/docker.sock`.
It's talking to a per-user proxy socket:

```
/var/run/isolator-docker/<user>.sock
```

The proxy validates every API call: blocks privileged containers, blocks
host-path bind mounts, filters container lists to the user's own containers,
runs ownership checks before exec/stop/kill. The real OrbStack socket is
never directly reachable.

When testcontainers tries to start Ryuk, it bind-mounts our proxy socket into
Ryuk at the same path. **Without an exception, our path validation rejects
the bind** because the proxy socket isn't under the user's workspace or tmp
directories, and the `docker.sock` substring check is suspicious of anything
shaped like a Docker socket.

So the choice was: keep blocking the bind and lose Ryuk, or allow the bind
and analyze whether that's safe.

## Why allowing the bind is safe

The threat model with bind-mounted Docker sockets is **container escape via
privileged daemon access**. Specifically: if a container has the *real*
Docker socket mounted, it can:

- Create a privileged container that mounts `/` from the host
- Create a container with `--cap-add=ALL` or `--security-opt=seccomp=unconfined`
- Mount arbitrary host paths — including `/etc/shadow`, ssh keys, etc.
- Read other users' containers' filesystems
- Pull arbitrary images and execute them as root

Each of those bypasses every protection we built. Mounting
`/var/run/docker.sock` into a container is effectively giving that container
root on the host.

**But the per-user proxy socket is not the real Docker socket.** It's our
proxy — the same enforcement gate the user is already using from the host.
A container with the proxy socket mounted gets a Docker client that:

| Attempt                              | Outcome                                       |
|--------------------------------------|-----------------------------------------------|
| Create privileged container          | Blocked (`Privileged is not allowed`)         |
| Mount `/etc/passwd`                  | Blocked (`bind mount not allowed`)            |
| Mount real `/var/run/docker.sock`    | Blocked (`docker.sock` substring check)       |
| Cap-add `NET_ADMIN`                  | Blocked (`CapAdd is not allowed`)             |
| Pull from arbitrary registry         | Blocked (pf egress allowlist)                 |
| Stop another user's container        | Blocked (ownership label check)               |
| List all containers                  | Filtered to user's own only                   |
| Run as `root` (UID 0 inside)         | Blocked (`container user 'root' not allowed`) |

In other words: a container with the proxy socket mounted has **exactly the
privileges the user already has on the host**. There's no escalation. The
in-container client is bounded by the same proxy that bounds the host client.

This is the key insight: our proxy socket is a *safe* alternative to the real
Docker socket. Mounting it into a container doesn't grant new powers — it
grants the same powers, scoped by the same user.

## What we allow, exactly

The path check in `internal/proxy/paths.go` allows the following bind
sources:

1. The user's workspace: `/Users/Workspaces/<user>/[**]`
2. The user's tmp dir: `/Users/<user>/tmp/[**]`
3. **The user's own proxy socket**: `/var/run/isolator-docker/<user>.sock`

The third entry is the new exception. It's narrow:

- **Exact path match.** No prefix matches; only the literal socket file. The
  parent directory (`/var/run/isolator-docker`) and other users' sockets
  remain rejected.
- **Per-user.** A container running as `altinity` cannot bind-mount
  `slot-0`'s proxy socket. The check uses the user's own name to construct
  the allowed path.
- **Symlink-aware.** macOS resolves `/var` to `/private/var`, so the resolved
  path comparison happens against both forms.

## What we still reject

- `/var/run/docker.sock` — the real OrbStack socket. Path doesn't match user
  workspace/tmp, doesn't match `<user>.sock`, also blocked by the
  `docker.sock` substring check.
- `~/.orbstack/run/docker.sock` — same reasons.
- `/var/run/isolator-docker/other.sock` — wrong user; not allowed.
- `/Users/Workspaces/<user>/docker.sock` — even though the user could create
  a file with that name in their workspace, the substring check fires.

## Recursive proxy access

A natural question: *if a container can mount the proxy socket, it can create
another container that also mounts the proxy socket. Is that a problem?*

No. Each child container is also bounded by the proxy. The recursion doesn't
escalate; it just gives the same user the same access at every layer. This is
no different from the user opening multiple shells on the host — each shell
has the same powers, none has more than the user.

## What this means for security review

The change extends `IsPathAllowed` by one exact-match entry. The diff is
~5 lines plus tests. The security argument rests on a single claim:

> Mounting the per-user proxy socket into a container grants no privilege
> beyond what the user already has on the host.

This is true because the proxy enforces the same policy regardless of which
client speaks to it. The proxy doesn't know or care whether the client is on
the host or inside a container, and it can't know — Unix socket connections
have no transport-level identity beyond the connecting UID, which is the
same on both sides.

The defense in depth that previously made the bind-mount block load-bearing
(via the `docker.sock` substring check) was guarding against the *real*
Docker socket. That check still fires for any path containing the substring
`docker.sock`. The proxy socket doesn't contain that substring (it's
`<user>.sock`), so the precise-match exception is the only difference.

## Operational notes

- Ryuk uses the docker socket only for Docker API calls. It does not need
  root, capabilities, or privileged mode. It runs as an unprivileged user
  inside the container.
- Ryuk exposes a TCP control port (8080 inside, mapped to a random host
  port). The test process connects to that port. **This connection does not
  go through our proxy** — it's a regular TCP connection, gated by pf rules
  on outgoing traffic from the user's UID.

## macOS limitation: in-container socket bridging

The path-check exception above is *necessary* for Ryuk to work. On Linux
(rootless Docker, real Docker daemon on the host), it is also *sufficient* —
the bind-mount works, the container connects to the proxy socket, Ryuk
operates normally.

**On macOS with OrbStack (or Docker Desktop) it is necessary but not
sufficient.** The reason is architectural: containers run inside a Linux VM,
not on macOS directly. When you bind-mount a macOS-host Unix socket into a
container:

- The socket file becomes *visible* inside the container (via VirtioFS or
  similar host-share mechanism).
- But `connect()` from inside the container returns `ECONNREFUSED`.

Unix domain sockets are kernel objects bound to a specific kernel's namespace.
The VM's kernel sees a special file at the bind path but has no way to reach
across the VM boundary to the macOS-side socket the file represents.

OrbStack's *own* Docker daemon socket appears to work through a bind-mount
because OrbStack runs a TCP-to-Unix bridge on macOS that forwards into the
VM. Our proxy doesn't have that bridge.

### Compatibility matrix (read this first)

Ryuk support depends on **which testcontainers library** you use AND which
**host platform** you're on. They behave differently:

| Library              | Linux                 | macOS (OrbStack / Docker Desktop)                |
|----------------------|-----------------------|--------------------------------------------------|
| testcontainers-java  | bind-mount path works | TLS TCP path works (env-var setup, see below)    |
| testcontainers-go    | bind-mount path works | **Unsupported today** (see "macOS + Go" below)   |

The short version: if you're on Linux, the path-check exception is enough
and you don't need to do anything special — Ryuk just works. On macOS the
story splits by library.

### macOS + testcontainers-java: TLS TCP listener

testcontainers-java honors `TESTCONTAINERS_RYUK_DOCKER_SOCKET_OVERRIDE`,
which lets us redirect Ryuk to a TCP+TLS endpoint instead of the bind-mounted
Unix socket.

**What `iso` sets up automatically.** When `iso <user> ...` runs, the proxy
starts with:

- A Unix socket at `/var/run/isolator-docker/<user>.sock` (existing).
- A TCP listener on `127.0.0.1:<port>` where `port = 40000 + (uid - 600)`.
  altinity (uid 606) → port 40006, slot-0 (uid 600) → port 40000, etc.
- TLS with mutual auth: server cert + client cert generated by a per-user
  CA on first run, written to `/Users/<user>/.isolator-docker-proxy/`:
  - `ca.pem` — CA root
  - `ca.key` — CA private key (root-only)
  - `server.crt`, `server.key` — server identity
  - `cert.pem`, `key.pem` — client cert + key (Docker CLI naming convention)

Cert generation is one-shot via `docker-proxy-go --init-tls`. iso runs that
the first time it sees a missing `ca.pem`, then chowns the dir to the user
so testcontainers can read the files for bind-mounting.

**Env vars to set in the test process.** For Ryuk to use the TLS endpoint:

```bash
# Test process keeps using the fast Unix socket.
DOCKER_HOST=unix:///var/run/isolator-docker/altinity.sock

# Ryuk override: point Ryuk's docker client at the TLS endpoint.
TESTCONTAINERS_RYUK_DOCKER_SOCKET_OVERRIDE=tcp://host.docker.internal:40006
DOCKER_TLS_VERIFY=1
DOCKER_CERT_PATH=/Users/altinity/.isolator-docker-proxy
TESTCONTAINERS_HOST_OVERRIDE=host.docker.internal
```

testcontainers-java bind-mounts `DOCKER_CERT_PATH` into the Ryuk container at
the same path, sets `DOCKER_HOST=tcp://host.docker.internal:40006` inside
Ryuk, and Ryuk's standard Docker client library handles the TLS handshake.

**Why this is safe.** The TLS layer is the auth boundary, not the port number:

| Attack                                            | Outcome                                       |
|---------------------------------------------------|-----------------------------------------------|
| Container scans localhost ports, finds 40006      | Connection rejected without client cert       |
| Container has user A's certs, hits user B's port  | Cert chain fails — different per-user CA      |
| Plaintext HTTP to TLS port                        | Rejected by TLS handshake                     |
| Sandbox user A reads user B's cert files          | Blocked by Unix permissions (mode 0700, owned by B) |

A successful connection to user A's TLS endpoint requires:
1. A client cert signed by user A's CA, AND
2. The CA private key, which is root-only on the macOS host.

Once authenticated, the container's docker client gets the same proxy-bounded
privileges as the host user — same gate, same policy, same ownership label.

### macOS + testcontainers-go: unsupported today

testcontainers-go has **no equivalent** of `TESTCONTAINERS_RYUK_DOCKER_SOCKET_OVERRIDE`.
Its reaper code (`reaper.go`) hard-codes a Unix-socket bind-mount regardless
of `DOCKER_HOST` scheme:

```go
hc.Binds = []string{dockerHostMount + ":/var/run/docker.sock"}
```

Setting the four env vars from the Java recipe **does not work**:

- `TESTCONTAINERS_RYUK_DOCKER_SOCKET_OVERRIDE` is silently ignored — Go has no such variable.
- `DOCKER_TLS_VERIFY=1` makes the test process's *own* docker client try
  HTTPS over the Unix socket, which panics:
  ```
  panic: docker info: Get "https://%2Fvar%2Frun%2Fisolator-docker%2Faltinity.sock/v1.51/info":
  http: server gave HTTP response to HTTPS client
  ```

Without the env vars, testcontainers-go falls back to bind-mount, which our
proxy now allows (commit `c3a55c1`) but which doesn't actually work on macOS
because Unix sockets can't traverse the OrbStack VM boundary (the bind makes
the file visible inside the container, but `connect()` returns
`ECONNREFUSED`).

**Two paths forward:**

1. **Upstream fix** — testcontainers-go [#3662](https://github.com/testcontainers/testcontainers-go/issues/3662)
   proposes adding TCP/TLS daemon-host support to the reaper, paralleling
   what testcontainers-java already supports. Multi-week timeline at best.
2. **In-proxy reaper** — implement Ryuk-equivalent cleanup *inside* the
   docker-proxy itself, so testcontainers can run with `TESTCONTAINERS_RYUK_DISABLED=true`
   while still getting reliable cleanup. This is the unblock for our use
   case today; it removes the third-party Ryuk dependency entirely. Design
   discussion in progress (see commit log).

Until either lands, testcontainers-go users on macOS should set
`TESTCONTAINERS_RYUK_DISABLED=true` and accept that crashed test runs may
leak containers. The proxy's container-list filtering still scopes leaks to
the user's own namespace, so a periodic
`docker rm -f $(docker ps -aq)` from inside the user's session is a safe
manual cleanup.

## Diagnosing Ryuk failures

Failure mode → where to look:

- **`bind mount not allowed`** — path check rejected the bind. Confirm the
  path is exactly `/var/run/isolator-docker/<your-user>.sock`, no trailing
  slash, no different user.
- **`Cannot connect to the Docker daemon`** (from inside the container) —
  the bind worked but the connection didn't reach the proxy. On macOS this
  is the VM-boundary issue: containers can't connect to host-side Unix
  sockets even when the file is visible. Use the TLS TCP path
  (testcontainers-java) or wait on the in-proxy reaper / upstream
  testcontainers-go #3662.
- **`http: server gave HTTP response to HTTPS client`** (test process
  panic) — `DOCKER_TLS_VERIFY=1` is set globally and Go's docker client is
  trying to TLS-handshake over a Unix socket. This happens specifically
  with testcontainers-go when the four-env-var Java recipe is applied. Drop
  `DOCKER_TLS_VERIFY=1` from the test process; it can't be scoped to
  Ryuk-only on Go.
- **`isolator: <something>` errors visible to Ryuk** — connection reached
  the proxy and the proxy is enforcing policy. Look in the proxy log
  (`/var/run/isolator-docker/<user>.log`) for the exact rule that fired.

## References

- testcontainers Ryuk source: https://github.com/testcontainers/moby-ryuk
- testcontainers-go upstream issue (TCP/TLS Ryuk): https://github.com/testcontainers/testcontainers-go/issues/3662
- testcontainers-java parallel issue: https://github.com/testcontainers/testcontainers-java/issues/9137
- The path check: `internal/proxy/paths.go`, `IsPathAllowed`
- The substring check that still fires for real docker sockets:
  `internal/proxy/create.go`, look for `docker.sock`
- The docker-proxy spec: `docs/docker-proxy-go-spec.md`
