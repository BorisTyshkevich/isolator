# docker-proxy: Go Implementation Specification

> This document specifies the complete behaviour of a per-user Docker socket
> proxy. It is self-contained: a Go developer can implement the proxy from this
> spec alone, without reading the Python prototype.

---

## 1. Overview

### Purpose

`docker-proxy` is a Unix domain socket proxy that sits between a sandboxed user
and the real Docker daemon socket (`/var/run/docker.sock`). Each sandboxed user
gets their own proxy instance, bound to a per-user socket path.

### Threat Model

The proxy enforces **multi-tenant isolation on a shared Docker host**. Each
sandbox user can only:

- Create containers whose filesystem mounts are restricted to the user's own
  workspace directory and per-user tmp directory.
- Interact with containers they own (identified by an ownership label).
- Pull images from registries (egress gating is pf's job, not the proxy's).
- Build images from local context.
- Use only their own isolated network (`iso-{user}`).

### What It Protects Against

| Attack                                      | Defense                                          |
|---------------------------------------------|--------------------------------------------------|
| Escape via privileged container              | Reject `Privileged`, `CapAdd`, `Devices`, etc.   |
| Read other user's files via bind mount       | Path validation + realpath rewrite (TOCTOU-safe) |
| Mount Docker socket inside container         | Block any path containing `docker.sock`          |
| Access other user's containers               | Ownership label check on every action            |
| HTTP request smuggling                       | Go `net/http` framing + explicit TE check (§7)    |
| Socket hijack by wrong user                  | `SO_PEERCRED` / `LOCAL_PEERCRED` UID check       |
| Container running as root                    | Block `User: "root"`, `"0"`, `"0:0"`            |
| DNS spoofing from inside container           | Block `DNS`, `DNSOptions`, `DNSSearch` fields    |
| Join another user's network at create time   | NetworkMode and EndpointsConfig validation        |
| Connect own container to another iso network | NetworkMode validation (post-create connect is allowed; see §6.4) |
| Named volume data exfiltration               | Block mount type `volume`; block named Binds      |
| Import tarball from arbitrary URL            | Block `fromSrc` on `/images/create`              |

### What It Deliberately Does NOT Protect

- **Network egress filtering**: pf firewall rules control which registries and
  hosts are reachable. The proxy allows `/images/{name}/push` because pf gates
  the actual TCP connection.
- **Image content scanning**: The proxy does not inspect image layers.
- **Resource limits enforcement**: cgroups and Docker defaults handle this;
  memory/CPU fields in HostConfig are allowed through.

> **See also §19** for the complete catalogue of accepted v1 risks (cross-tenant info leakage, unrestricted volumes API, network connect/delete leniency) and the v2 roadmap.

---

## 2. CLI & Configuration

### Flags

| Flag                      | Required | Default                  | Description                                            |
|---------------------------|----------|--------------------------|--------------------------------------------------------|
| `--user`                  | Yes      | --                       | Username of the sandboxed user                         |
| `--socket`                | Yes      | --                       | Path for the proxy's Unix socket                       |
| `--upstream`              | No       | `/var/run/docker.sock`   | Path to the real Docker daemon socket                  |
| `--insecure-skip-checks`  | No       | `false`                  | Skip parent dir ownership and user UID checks (testing)|

### Constants

```go
const (
    WorkspacesDir = "/Users/Workspaces"
    MaxBodySize   = 16 * 1024 * 1024 // 16 MB
    OwnerLabel    = "dev.boris.isolator.user"
)
```

---

## 3. Startup & Socket Setup

Execute the following steps in order:

1. **Parse flags** via `flag` package or similar.

2. **Resolve target user UID**:
   - Look up `--user` via `os/user.Lookup(username)`.
   - If not found and `--insecure-skip-checks` is set, use `os.Getuid()` and continue. The resulting `targetUID` is used only for the startup log line (step 9); chown (step 6) and the parent-dir ownership check (step 3) are skipped under `--insecure-skip-checks`, so the value need not be authoritative.
   - If not found and checks are enabled, print `FATAL: user "<user>" not found` to stderr and `os.Exit(1)`.

3. **Verify parent directory ownership** (unless `--insecure-skip-checks`):
   - `os.Stat(filepath.Dir(socketPath))` -> check `Sys().(*syscall.Stat_t).Uid == 0`.
   - If not root-owned: print `FATAL: <dir> is not root-owned (uid=<N>) -- refusing to start` and `os.Exit(1)`.

4. **Remove stale socket** if it exists: `os.Remove(socketPath)` (ignore ENOENT).

5. **Bind socket with restrictive permissions**:
   - `syscall.Umask(0077)` before `net.Listen("unix", socketPath)`, restore old umask after.
   - This creates the socket file with mode `0600`.

6. **Set socket ownership** (unless `--insecure-skip-checks`):
   - `os.Chown(socketPath, targetUID, -1)`.

7. **Set socket mode explicitly**: `os.Chmod(socketPath, 0600)`.

8. **Set listen backlog**: Go's `net.Listen` uses `SOMAXCONN` by default which is fine. The Python version uses 32; Go's default is acceptable.

9. **Log startup**: `[<user>] proxy: <socket> (uid=<targetUID>, mode 600) -> <upstream>`.

10. **Register signal handlers** for `SIGTERM` and `SIGINT`:
    - Close the listener.
    - `os.Remove(socketPath)` (ignore errors).
    - `os.Exit(0)`.

---

## 4. Connection Handling

### Accept Loop

```
for {
    conn, err := listener.Accept()
    if err != nil { break }
    go handleConnection(conn, ...)
}
```

One goroutine per accepted connection (not one per request).

### Peer UID Verification

Before processing any request on a connection, verify the connecting process's UID.

#### macOS (darwin build tag)

The `xucred` struct returned by `LOCAL_PEERCRED`:

```
cr_version uint32     // offset 0, size 4
cr_uid     uint32     // offset 4, size 4
cr_ngroups int16      // offset 8, size 2
_pad       [2]byte    // offset 10, size 2 (alignment)
cr_groups  [16]uint32 // offset 12, size 64
```
Total: 76 bytes.

Use `golang.org/x/sys/unix.GetsockoptXucred`:

```go
import "golang.org/x/sys/unix"

func getPeerUID(conn *net.UnixConn) (uid uint32, ok bool) {
    raw, err := conn.SyscallConn()
    if err != nil {
        return 0, false
    }
    var peerUID uint32
    var sysErr error
    err = raw.Control(func(fd uintptr) {
        buf, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
        if err != nil {
            sysErr = err
            return
        }
        peerUID = buf.Uid
    })
    if err != nil || sysErr != nil {
        return 0, false
    }
    return peerUID, true
}
```

#### Linux (linux build tag)

```go
import "golang.org/x/sys/unix"

func getPeerUID(conn *net.UnixConn) (uid uint32, ok bool) {
    raw, err := conn.SyscallConn()
    if err != nil {
        return 0, false
    }
    var peerUID uint32
    var sysErr error
    err = raw.Control(func(fd uintptr) {
        cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
        if err != nil {
            sysErr = err
            return
        }
        peerUID = cred.Uid
    })
    if err != nil || sysErr != nil {
        return 0, false
    }
    return peerUID, true
}
```

#### Enforcement Logic

- If `getPeerUID` succeeds and `uid != expectedUID`: close connection immediately, log `BLOCKED: peer uid=<N> does not match expected uid=<M>`.
- If `getPeerUID` fails (returns `ok=false`): **allow the connection** (defense-in-depth only; do not hard-fail on platforms that lack peer credentials).

### Connection Timeout

Set a **30-second header read deadline** on the client socket: reset the deadline each time a new request starts arriving and clear it once the headers have been parsed (so body reads can take as long as needed for large uploads). This addresses the Python bug of no timeout on stalled clients without breaking legitimate large-body endpoints.

```go
conn.SetReadDeadline(time.Now().Add(30 * time.Second))
// ... read headers ...
conn.SetReadDeadline(time.Time{}) // clear before reading body
```

When using `http.Server`, `ReadHeaderTimeout` (§5) does this automatically.

### Keep-Alive Loop

Each connection supports HTTP/1.1 keep-alive: after processing one request-response cycle, loop back to read the next request on the same connection. Exit the loop on:

- Client EOF (clean close).
- Read error or timeout.
- Streaming endpoint entered (attach/wait/exec/logs/events) -- these take over the connection.

---

## 5. HTTP Request Reading

### Recommended Architecture: `net/http.Server` with Custom Listener

**Do NOT hand-roll HTTP parsing.** Use Go's `net/http` server machinery:

```go
server := &http.Server{
    Handler:           proxyHandler,
    ReadHeaderTimeout: 30 * time.Second,
    IdleTimeout:       30 * time.Second,
}
server.Serve(unixListener)
```

> **Do not set `ReadTimeout`.** It bounds the entire request including body, which would kill legitimate `POST /build` and `POST /images/load` uploads of large tarballs over slow links. `ReadHeaderTimeout` covers only the request line + headers, which is what the 30s "stalled client" defense actually targets. `IdleTimeout` covers the gap between requests on a kept-alive connection.

The `proxyHandler` is an `http.Handler` that receives fully parsed `*http.Request` objects. The `Body` is already decoded (chunked transfer is transparent). This eliminates three Python bugs by construction:

1. **Chunked end-detection false positives**: Go's `net/http` reads chunked encoding correctly.
2. **`endswith("0\r\n\r\n")` false positives**: Not applicable; body is decoded.
3. **Leftover byte accounting**: `net/http` handles connection reuse internally.
4. **Duplicate Content-Length**: `net/http` rejects this with 400 automatically.

### Alternative: Manual Approach

If `net/http.Server` cannot be used for streaming endpoints (hijacking is needed), the handler can use `http.Hijacker` to take over the raw connection for bidirectional relay. See section 12.

---

## 6. Endpoint Allowlist (Default-Deny)

All endpoint matching uses the **path component only** (strip query string before matching). The version prefix pattern is `/v` followed by one or more digits/dots: regex fragment `v[\d.]+`.

If no rule matches, return **403 Forbidden** with JSON body:
```json
{"message": "isolator: endpoint not allowed: <METHOD> <path>"}
```

Log: `BLOCKED endpoint: <METHOD> <path>`.

### 6.1 System

| Method     | Pattern                                                    | Notes                                   |
|------------|------------------------------------------------------------|-----------------------------------------|
| HEAD, GET  | `/_ping`                                                   | Unversioned                             |
| GET        | `/v<ver>/_ping`                                            |                                         |
| GET        | `/v<ver>/version`                                          |                                         |
| GET        | `/v<ver>/info`                                             |                                         |
| GET        | `/info`                                                    | Unversioned; Go Docker SDK calls this   |
| GET        | `/v<ver>/system/df`                                        |                                         |
| GET        | `/v<ver>/system/events`                                    |                                         |
| POST       | `/auth`                                                    | Unversioned                             |
| POST       | `/v<ver>/auth`                                             | Versioned                               |

**Regex for versioned system endpoints:**
```
^/v[\d.]+/(_ping|version|info|system/df|system/events)$
```

**Regex for auth:**
```
^(/v[\d.]+)?/auth$
```

### 6.2 Containers

| Method     | Pattern                                                    | Notes                                   |
|------------|------------------------------------------------------------|-----------------------------------------|
| GET        | `/v<ver>/containers/json`                                  | List; response is filtered (see 11)    |
| POST       | `/v<ver>/containers/create`                                | Body validated (see 9)                 |
| ANY        | `/v<ver>/containers/<id>/<action>[/...]`                   | See action list below                   |
| DELETE     | `/v<ver>/containers/<id>`                                  | Ownership checked                       |
| ANY        | `/v<ver>/exec/<id>/<action>`                               | See exec actions below                  |

**Container ID pattern:** `[a-zA-Z0-9_.-]+`

**Allowed container actions** (regex group):
```
start|stop|restart|kill|pause|unpause|wait|attach|logs|inspect|stats|top|changes|export|exec|json
```

**Container action regex:**
```
^/v[\d.]+/containers/[a-zA-Z0-9_.-]+/(start|stop|restart|kill|pause|unpause|wait|attach|logs|inspect|stats|top|changes|export|exec|json)(/.*)?$
```

Note: the `(/.*)?` suffix tolerates trailing path segments on container action endpoints. It is not currently required by any Docker API path, but harmless and keeps the regex resilient to minor route variations.

**Container delete regex:**
```
^/v[\d.]+/containers/[a-zA-Z0-9_.-]+$
```

**Exec action regex:**
```
^/v[\d.]+/exec/[a-zA-Z0-9_.-]+/(start|resize|json)$
```

### 6.3 Images

| Method | Pattern                          | Conditions                                                              |
|--------|----------------------------------|-------------------------------------------------------------------------|
| GET    | `/v<ver>/images/<name>/json`     | Image inspect; `<name>` = `[a-zA-Z0-9_./:@%-]+`                        |
| GET    | `/v<ver>/images/json`            | Image list                                                              |
| POST   | `/v<ver>/images/create`          | Pull only; see conditions below                                         |
| POST   | `/v<ver>/images/<name>/push`     | Egress controlled by pf; `<name>` = `[a-zA-Z0-9_./:@%-]+`              |

**Image inspect regex** (matches push regex; `%` is allowed for URL-encoded names):
```
^/v[\d.]+/images/[a-zA-Z0-9_./:@%-]+/json$
```

**Image push regex:**
```
^/v[\d.]+/images/[a-zA-Z0-9_./:@%-]+/push$
```

> The Python prototype's inspect regex omitted `%`, so URL-encoded image names failed inspect but worked for push. The Go spec unifies the character classes; this is an intentional, minor deviation from Python.

**`/images/create` validation** (query string checks):
1. If query contains `fromSrc` key -> **block** (prevents importing tarballs from URLs).
2. Query keys must be a subset of `{fromImage, tag, platform}`. Any other key -> **block**.
3. `fromImage` must be present and non-empty. If missing or empty -> **block**.

### 6.4 Networks

| Method  | Pattern                                          | Notes                                    |
|---------|--------------------------------------------------|------------------------------------------|
| GET     | `/v<ver>/networks`                               | List networks                            |
| GET     | `/v<ver>/networks/json`                          | List networks (alternate)                |
| GET     | `/v<ver>/networks/<name>`                        | Inspect any network (read-only)          |
| ALL     | `/v<ver>/networks/iso-<user>[/...]`              | Full access to user's own iso network    |
| POST    | `/v<ver>/networks/<name>/connect`                | Connect container to network             |
| POST    | `/v<ver>/networks/<name>/disconnect`             | Disconnect container from network        |
| DELETE  | `/v<ver>/networks/<name>`                        | Delete network                           |
| POST    | `/v<ver>/networks/create`                        | Create network                           |

**Network name pattern:** `[a-zA-Z0-9_.-]+`

**Network list regex:**
```
^/v[\d.]+/networks(/json)?$
```

**Network inspect regex:**
```
^/v[\d.]+/networks/[a-zA-Z0-9_.-]+$
```

**User's iso-network regex** (constructed per-user):
```
^/v[\d.]+/networks/iso-<escaped_user>(/.*)?$
```

**Network connect/disconnect regex:**
```
^/v[\d.]+/networks/[a-zA-Z0-9_.-]+/(connect|disconnect)$
```

**Network create regex:**
```
^/v[\d.]+/networks/create$
```

**Network create body validation (`check_network_create`).** Required for `POST /v<ver>/networks/create`. Read body (≤16 MB, same limit as container create) and parse as JSON `map[string]interface{}`. Apply these checks:

1. **Driver allowlist.** `Driver` field, if present and non-empty, must be `"bridge"`. Any other value (`"host"`, `"overlay"`, `"macvlan"`, `"ipvlan"`, `"none"`, custom plugin names) → **403**: `"network driver '<value>' not allowed (use bridge)"`. Empty/missing is allowed (Docker defaults to bridge).

2. **Name policy.** `Name` field is required and non-empty. If `Name` starts with `"iso-"`, it must be either exactly `"iso-<user>"` or start with `"iso-<user>-"`. Any other `iso-` prefix → **403**: `"network name '<name>' reserved for another user"`. This prevents a sandbox user from creating `iso-victim` and confusing the iso-network namespace. Names without the `iso-` prefix are allowed (testcontainers and similar tools generate random names like `testcontainers-abc123`).

3. **Block `ConfigFrom`.** If `ConfigFrom` is non-nil/non-empty → **403**: `"ConfigFrom is not allowed"`. This prevents inheriting config from another network outside the user's scope.

4. **Owner label injection.** Identical to container create (§9.8): set `Labels["dev.boris.isolator.user"] = user`, merging with any existing labels. The Go implementation must support ownership-checked deletion in v2 by writing this label now.

5. **Re-serialize** following §9.9 strategy A — mutate the parsed map, marshal back, set `ContentLength`, drop `Transfer-Encoding`. Pass-through fields (`IPAM`, `Internal`, `Attachable`, `EnableIPv6`, `Options`, etc.) are preserved unchanged.

> **Why no IPAM restriction:** Docker auto-assigns private subnets when IPAM is omitted, and testcontainers sometimes pins specific subnets for reproducibility. Restricting IPAM would break these tools. Cross-tenant subnet collision is a Docker-level concern (it errors on overlap), not an isolation breach.

**Network delete regex:** (same as inspect, method=DELETE)
```
^/v[\d.]+/networks/[a-zA-Z0-9_.-]+$
```

**Network delete: ownership not currently enforced (v1 limitation).** A sandbox user can issue `DELETE /v<ver>/networks/<name>` for any non-system network. With network create now injecting an owner label (above), v2 should add an ownership subrequest before delete, identical to container ownership checks (§10). Documented as accepted v1 risk in §19.

**Network connect/disconnect: ownership not enforced (intentional v1 behavior).** `POST /v<ver>/networks/<name>/(connect|disconnect)` is allowed for any network name. The container create validation (§9.3, §9.4) restricts initial network attachment to `{"", "default", "bridge", "iso-<user>"}`, but post-create a user can connect their own container to another user's `iso-<other>` network. The defenses against this are: (1) the *target* container in another user's iso network is not reachable (each user's containers are owned and can't be exec'd into by others, §10); (2) pf egress rules constrain what the network can talk to. The risk is limited to network-level visibility (the joining user can see traffic on the target network if other users emit unencrypted traffic). Documented as accepted v1 risk in §19; v2 should restrict connect/disconnect to networks the user owns by label.

### 6.5 Build & Load

| Method | Pattern                    | Notes                              |
|--------|----------------------------|------------------------------------|
| POST   | `/v<ver>/build`            | Raw passthrough; see section 8     |
| POST   | `/v<ver>/images/load`      | Raw passthrough; see section 8     |

**Regex:**
```
^/v[\d.]+/build$
^/v[\d.]+/images/load$
```

### 6.6 Volumes

| Method | Pattern                       | Notes                              |
|--------|-------------------------------|------------------------------------|
| ALL    | `/v<ver>/volumes[/...]`       | Full CRUD access (v1 unrestricted) |

**Regex:**
```
^/v[\d.]+/volumes(/.*)?$
```

> **Accepted v1 risk: cross-tenant volume API.** All methods on `/volumes[/...]` are allowed without owner labeling, name scoping, or filtering. A sandbox user can list, inspect, create, and delete *any* user's named volumes via this endpoint. This is mitigated only by the create-time block on `volume`-typed mounts (§9.6) — a malicious user cannot *use* another user's volume from inside a container, but can disrupt it (delete) or enumerate it (list/inspect).
>
> The reason this is open in v1: locking it down requires owner-label injection on volume create plus ownership subrequests on every other verb, mirroring the container ownership system, and we have not built that yet. Documented in §19 as a v2 work item.

### 6.7 Events

| Method | Pattern              |
|--------|----------------------|
| GET    | `/v<ver>/events`     |

**Regex:**
```
^/v[\d.]+/events$
```

### 6.8 Blocked Endpoints (explicit examples for testing)

These must return 403:

- `POST /v1.51/containers/{id}/update` -- not in action list
- `POST /v1.51/swarm/init` -- not in any allowlist
- `POST /v1.51/swarm/join`
- `POST /v1.51/plugins/pull`
- `POST /grpc`
- `POST /v1.51/images/create?fromSrc=http://evil.com/rootkit.tar`
- `GET /v1.51/secrets`
- `GET /v1.51/configs`
- Any unknown path

---

## 7. Anti-Smuggling Defenses

### Handled by `net/http` Automatically

- **Duplicate `Content-Length`**: `net/http.Server` returns 400 automatically.
- **Chunked decoding**: `net/http` decodes `Transfer-Encoding: chunked` and presents the decoded body in `Request.Body`. No manual chunked parsing needed.
- **Hop-by-hop headers**: `net/http/httputil.ReverseProxy` handles `Transfer-Encoding` correctly when forwarding.

### Explicit Check: Non-Chunked Transfer-Encoding

If the request has `Transfer-Encoding` with a value other than `chunked` (e.g., `identity`, `compress`, `gzip`), return **400 Bad Request**:

```json
{"message": "isolator: Transfer-Encoding not allowed"}
```

In practice, `net/http.Server` rejects unknown transfer encodings, but add an explicit check as defense-in-depth if implementing outside `net/http.Server`.

### Do NOT Forward `Transfer-Encoding: chunked` to Upstream (Modified-Body Path)

When the proxy has *rewritten* the request body (§9.9: container create) and knows the new length, forward to upstream with `Content-Length` framing and drop `Transfer-Encoding`. `httputil.ReverseProxy` handles this correctly if the body has a known length.

**Exception — passthrough endpoints (§8).** For `POST /v<ver>/build` and `POST /v<ver>/images/load`, the body is streamed without modification and its length is unknown to the proxy. Preserve the client's original framing including `Transfer-Encoding: chunked`. Do NOT attempt to convert chunked → Content-Length here; that would require buffering the entire body, defeating the streaming requirement. The Python prototype does raw bidirectional relay for these endpoints, which preserves chunked framing on the wire — the Go implementation must do the same (either via `Hijacker` + `io.Copy`, or by letting `ReverseProxy` stream a body of unknown length, which forwards chunked).

---

## 8. Large Binary Passthrough (No Body Buffering)

For these endpoints:
- `POST /v<ver>/build`
- `POST /v<ver>/images/load`

**Do NOT buffer the request body.** Build contexts and image tarballs can be hundreds of megabytes. Buffering would cause OOM or trigger the 16 MB body limit.

### Implementation Strategy

**Option A: Hijack + bidirectional io.Copy**

After the allowlist check passes, hijack the client connection and the upstream connection, forward headers, then bidirectionally stream:

```go
// 1. Dial upstream
upstreamConn, _ := net.Dial("unix", upstreamSocket)
// 2. Write request headers + any already-buffered body bytes to upstream
// 3. Bidirectional relay:
go io.Copy(client, upstreamConn) // upstream -> client
io.Copy(upstreamConn, client)    // client -> upstream
```

**Option B: httputil.ReverseProxy**

Use `ReverseProxy` with no body modification. The `Director` sets the scheme/host, and the body streams through without buffering. The `ReverseProxy` handles `Transfer-Encoding` correctly.

For these endpoints, skip `check_create`, skip body size limits, skip ownership checks. Just verify the endpoint is allowed and pass through.

**Framing**: preserve the client's original `Transfer-Encoding` (typically `chunked`) on these endpoints. The "drop chunked" rule in §7 applies only to the modified-body path (§9.9), not to streaming passthrough.

### Detection

Match request path: if path contains `/build` (specifically `POST /v<ver>/build`) or `/images/load` (specifically `POST /v<ver>/images/load`), use passthrough mode.

---

## 9. Container Create Validation (`check_create`)

Applies to: `POST /v<ver>/containers/create`

### Body Reading

- Read full body (max `MaxBodySize` = 16 MB).
- If body exceeds 16 MB, return **413 Payload Too Large**:
  ```json
  {"message": "isolator: request body too large (<N> bytes)"}
  ```
- Parse as JSON. If invalid JSON, return **400 Bad Request**:
  ```json
  {"message": "isolator: invalid JSON body"}
  ```

### 9.1 Dangerous HostConfig Fields

If **any** of these fields in `HostConfig` is truthy (non-nil, non-zero, non-empty), return **403 Forbidden** with message `"<Field> is not allowed"`:

```
Privileged       bool
VolumesFrom      []string
Devices          []DeviceMapping
DeviceCgroupRules []string
DeviceRequests   []DeviceRequest
CapAdd           []string
CapDrop          []string
SecurityOpt      []string
PidMode          string
IpcMode          string
UTSMode          string
UsernsMode       string
CgroupnsMode     string
CgroupParent     string
Cgroup           string
Runtime          string
Sysctls          map[string]string
Ulimits          []Ulimit
OomScoreAdj      int
OomKillDisable   *bool
DNS              []string
DNSOptions       []string
DNSSearch        []string
Links            []string
```

**"Truthy" definition for each type:**
- `bool`: `true`
- `string`: non-empty after default
- `[]T`: non-nil and `len > 0`
- `map[K]V`: non-nil and `len > 0`
- `*bool`: non-nil and `*val == true`
- `int`: non-zero

> Note: The Python code checks `if val:` which is falsy for `0`, `""`, `[]`, `{}`, `None`, `False`. Match this behaviour. An `OomScoreAdj` of `0` is allowed; any non-zero value is blocked.

### 9.2 ExtraHosts Validation

For each entry in `HostConfig.ExtraHosts` (a `[]string`):
- Trim whitespace.
- Must exactly equal `"host.docker.internal:host-gateway"`.
- Any other value -> **403**: `"ExtraHosts entry '<entry>' not allowed"`.

### 9.3 NetworkMode Validation

`HostConfig.NetworkMode` must be one of:
- `""` (empty string)
- `"default"`
- `"bridge"`
- `"iso-<user>"` (where `<user>` is the `--user` flag value)

Any other value -> **403**: `"NetworkMode '<value>' not allowed (use iso-<user>)"`.

### 9.4 NetworkingConfig.EndpointsConfig Validation

For each key in `NetworkingConfig.EndpointsConfig` (a `map[string]interface{}`):
- Key must be in the same allowed set as NetworkMode: `{"", "default", "bridge", "iso-<user>"}`.
- Any other key -> **403**: `"network '<key>' not allowed (use iso-<user>)"`.

### 9.5 Binds Validation and Rewriting

`HostConfig.Binds` is `[]string`, each entry formatted as `source[:dest[:options]]`.

For each bind:
1. Split on `:` to extract `source` (first element).
2. If `source` does not start with `/` -> **403**: `"named volume '<source>' not allowed"`.
3. Resolve `source` to a normalized absolute path:
   - `cleaned := filepath.Clean(source)` — collapses `..` and `.` segments. **Always do this, regardless of whether the path exists.** Python's `Path.resolve()` does this transparently; Go's `EvalSymlinks` does not, so without `Clean` a payload like `/Users/Workspaces/acm/../other/secret` would slip past the prefix check on a non-existent path.
   - `resolved, err := filepath.EvalSymlinks(cleaned)`.
   - If `err != nil` (typical cause: path does not exist yet), fall back to `resolved = cleaned`. This matches Python's lenient `Path.resolve()` semantics.
4. Check `isPathAllowed(resolved, user)`:
   - `resolved == WorkspacesDir + "/" + user` OR `strings.HasPrefix(resolved, WorkspacesDir + "/" + user + "/")` -> allowed.
   - `resolved == "/Users/" + user + "/tmp"` OR `strings.HasPrefix(resolved, "/Users/" + user + "/tmp/")` -> allowed.
   - Everything else -> **403**: `"bind mount not allowed: <source>"`.
5. Check `!strings.Contains(resolved, "docker.sock")` -> if contains, **403**: `"mounting Docker socket is not allowed"`.
6. **Rewrite**: replace the source in the bind string with the resolved path. This is the TOCTOU defense -- Docker will mount what we validated, not what a symlink might later point to.

After validation, replace `HostConfig.Binds` with the rewritten slice.

### 9.6 Mounts Validation and Rewriting

`HostConfig.Mounts` is `[]Mount`, where each mount has a `Type` field.

| Type     | Action                                                       |
|----------|--------------------------------------------------------------|
| `bind`   | Validate + rewrite `Source` identically to Binds (see 9.5)  |
| `volume` | **403**: `"named volumes are not allowed"`                   |
| `tmpfs`  | Allow as-is                                                  |
| other    | **403**: `"mount type '<type>' not allowed"`                 |

For `bind` mounts: normalize `Source` with `filepath.Clean` then `filepath.EvalSymlinks` (falling back to the cleaned path if the target does not exist; see §9.5 step 3), validate with `isPathAllowed`, check for `docker.sock`, and rewrite `Source` to the resolved path.

### 9.7 Container User

The `User` field (top-level, not in HostConfig) is read as a string. After `strings.TrimSpace`, block if **any** of these is true:
- value equals `"root"`
- value equals `"0"`
- value starts with `"0:"` (covers `"0:0"`, `"0:1000"`, etc. — any uid=0 regardless of gid)

-> **403**: `"container user '<value>' not allowed"`.

Empty string and other numeric UIDs (e.g., `"1000"`, `"1000:1000"`) are allowed.

> The Python prototype only blocked the literal set `{"root", "0", "0:0"}`, so `"0:1000"` slipped through. The Go spec closes this gap.

If the JSON `User` field is not a string (e.g., a number), reject as **400 Bad Request** with `"invalid User field type"` rather than coercing.

### 9.7a Fields Deliberately Not Validated

These `HostConfig` and top-level fields are passed through unchanged:

- `PortBindings`, `ExposedPorts` — host port mapping is permitted. Conflicts between users are resolved by the kernel (EADDRINUSE); this is not an isolation breach because pf still controls inbound/outbound traffic and each container lives on its own iso network.
- `Memory`, `MemorySwap`, `CpuShares`, `NanoCPUs`, `CpusetCpus`, etc. — resource limits are the user's choice; cgroups enforces them.
- `RestartPolicy`, `AutoRemove`, `Init`, `StopSignal`, `StopTimeout`, etc. — lifecycle fields, no isolation impact.
- `Env`, `Entrypoint`, `Cmd`, `WorkingDir`, `Hostname`, `Domainname` — image/runtime config, no isolation impact.
- `Tmpfs`, `ShmSize`, `ReadonlyRootfs`, `MaskedPaths`, `ReadonlyPaths` — already constrained by Docker defaults.

This list is informational; the implementation does not need to maintain it. The principle is: validate fields that grant capabilities, escape isolation, or break the multi-tenant model. Pass everything else through.

### 9.8 Owner Label Injection

- Get existing `Labels` map (top-level field). If nil, create one.
- Set `Labels["dev.boris.isolator.user"] = user`.
- This merges with any existing labels; do not discard them.

### 9.9 Re-serialization

> **Critical: preserve unknown fields.** Docker's container-create payload has dozens of fields and grows with every API release. Decoding into a typed Go struct and re-marshalling will silently drop any field the struct doesn't declare (e.g. `Healthcheck`, `StopTimeout`, future fields). This breaks legitimate functionality in non-obvious ways. **Do not use a fully typed struct as the round-trip carrier.**
>
> Use one of these two strategies:
>
> **Strategy A (preferred): `map[string]interface{}` at the top level, typed structs only for inspection.**
>
> ```go
> var raw map[string]interface{}
> if err := json.Unmarshal(body, &raw); err != nil { /* 400 */ }
>
> // For each field that needs validation/mutation, decode the sub-tree
> // into a typed struct, mutate, then write the result back into `raw`.
> // Example for HostConfig:
> if hcRaw, ok := raw["HostConfig"]; ok {
>     hcBytes, _ := json.Marshal(hcRaw)
>     var hc HostConfig
>     json.Unmarshal(hcBytes, &hc)
>     // ... validation, bind/mount rewriting ...
>     // Write back ONLY the fields we touched, by mutating the map directly:
>     hcMap := hcRaw.(map[string]interface{})
>     hcMap["Binds"] = hc.Binds   // rewritten paths
>     hcMap["Mounts"] = hc.Mounts // rewritten sources
> }
>
> // Inject owner label by mutating the labels map in place (don't replace).
> labels, _ := raw["Labels"].(map[string]interface{})
> if labels == nil { labels = map[string]interface{}{}; raw["Labels"] = labels }
> labels[OwnerLabel] = user
>
> newBody, _ := json.Marshal(raw)
> ```
>
> Mutating the map preserves every untouched field exactly. Only fields the proxy explicitly rewrites (`Binds`, `Mounts`, `Labels`) are replaced.
>
> **Strategy B: `json.RawMessage` for opaque sub-trees.** Define a struct where validated fields are typed and everything else is `json.RawMessage`. More verbose; same effect.
>
> **Test requirement**: `create_test.go` must include a fidelity test — feed a body containing fields not declared anywhere in the proxy's structs (e.g. `"Healthcheck": {...}`, `"StopSignal": "SIGUSR1"`, `"FutureField": {"x": 1}`) and assert they appear unchanged in the output bytes (compare via re-parsed maps to avoid key-ordering issues).

After all validation and rewriting:
1. Marshal the (mutated) `map[string]interface{}` back to bytes via `json.Marshal`.
2. Replace `req.Body` with `io.NopCloser(bytes.NewReader(newBody))`.
3. Set **`req.ContentLength = int64(len(newBody))`** — this is a struct field separate from the header map, and `httputil.ReverseProxy` uses it to drive framing. Forgetting this is the most common Go bug here: the upstream sees the new header but `ReverseProxy` still tries to send the old length.
4. Set the header too: `req.Header.Set("Content-Length", strconv.Itoa(len(newBody)))`.
5. Drop any `Transfer-Encoding` header (`req.Header.Del("Transfer-Encoding")`) — the body is now a known-length `*bytes.Reader`.
6. Forward the modified request to upstream via `ReverseProxy`.

---

## 10. Container Ownership Verification

### When to Check

For container action endpoints matching:
```
/v<ver>/containers/<id>/<action>[/...]
```

**Skip ownership check** when:
- Path is `/containers/create` (handled by create validation).
- Path is `/containers/json` (list endpoint; handled by response filtering).
- The `<id>` segment is `json` or `create` (these are not container IDs).

**Always check ownership** for:
- All other container actions: start, stop, restart, kill, pause, unpause, wait, attach, logs, inspect, stats, top, changes, export, exec, delete.

### How to Extract Container ID

Parse the URL path into segments: `["v1.51", "containers", "<id>", "<action>"]`.
- If `segments[1] == "containers"` and `len(segments) >= 3` and `segments[2]` is not `"json"` or `"create"`: container ID is `segments[2]`, API version is `segments[0]`.

### Ownership Check Procedure

1. Make a **subrequest** (see section 13): `GET /v<ver>/containers/<id>/json` to upstream.
2. If upstream returns 404 -> **403**: `"container not found"`.
3. If upstream returns non-200 -> **403**: `"container inspect failed"`.
4. Parse response body as JSON.
5. Extract labels: `data.Config.Labels` (fallback to `data.Labels` if Config is absent).
6. If `labels["dev.boris.isolator.user"] != user` -> **403**: `"container '<id>' is not owned by <user>"`.

### Exec Ownership Check

For exec action endpoints matching `/v<ver>/exec/<id>/(start|resize|json)`:

1. Extract exec ID from path segments.
2. Make subrequest: `GET /v<ver>/exec/<id>/json` to upstream.
3. If 404 -> **403**: `"exec not found"`.
4. If non-200 -> **403**: `"exec inspect failed"`.
5. Parse JSON, extract `ContainerID` field.
6. If `ContainerID` is empty -> **403**: `"exec inspect missing ContainerID"`.
7. Run the container ownership check (above) on the extracted `ContainerID`.

---

## 11. Container List Filtering

Applies to: `GET /v<ver>/containers/json`

After receiving the upstream response:

1. Parse response body as JSON array.
2. Filter: keep only items where `item.Labels["dev.boris.isolator.user"] == user`.
3. Re-serialize the filtered array.
4. Replace `resp.Body` with `io.NopCloser(bytes.NewReader(newBody))`.
5. Set **`resp.ContentLength = int64(len(newBody))`** — like `req.ContentLength` on the request side, this struct field drives framing in the response writer; updating only the header is not enough.
6. Set `resp.Header.Set("Content-Length", strconv.Itoa(len(newBody)))`.
7. Remove `Transfer-Encoding`: `resp.Header.Del("Transfer-Encoding")` (body is now known-length).
8. Return `nil` from `ModifyResponse`; `httputil.ReverseProxy` writes the modified response.

If JSON parsing fails, return `nil` without modification — `ReverseProxy` forwards the original response unchanged.

---

## 12. Streaming Endpoints (Bidirectional Relay)

After forwarding the request to upstream, these endpoints require **switching to raw bidirectional I/O** for the remainder of the connection. Do not attempt to parse further HTTP on the connection after entering relay mode.

### Streaming Endpoints

Detection by **path substring** (matching the Python implementation):

- `/attach` -- container attach
- `/exec` -- exec start (the `/exec/{id}/start` path contains `/exec`)
- `/wait` -- container wait
- `/logs` -- container logs
- `/events` -- system events stream

> **Important**: The `/exec` substring match means that `POST /v<ver>/containers/{id}/exec` (create exec) also enters streaming mode. This is intentional: after creating an exec instance, the response is immediate but the connection is used for subsequent exec operations.
>
> The same substring match also fires on `POST /v<ver>/exec/{id}/resize` and `GET /v<ver>/exec/{id}/json`, which are not actually streaming. Treating them as streaming is harmless — the response completes and both sides close — but the proxy loses keep-alive on those connections. This is faithful to the Python behavior; do not optimize.

### Implementation

Using `http.Hijacker`:

```go
hijacker, ok := w.(http.Hijacker)
if !ok { /* error */ }
clientConn, clientBuf, _ := hijacker.Hijack()

// Forward any buffered data from clientBuf to upstream
// Then bidirectional relay:
go func() {
    io.Copy(clientConn, upstreamConn)
    clientConn.CloseWrite()
}()
io.Copy(upstreamConn, clientConn)
upstreamConn.CloseWrite()
```

Wait for both directions to finish (one goroutine + main goroutine), then close both connections.

### Events Endpoint

`GET /v<ver>/events` is also a streaming endpoint -- Docker sends newline-delimited JSON events indefinitely. Treat it the same as attach: bidirectional relay until either side closes.

---

## 13. Subrequest Plumbing

For ownership checks (sections 10, 11), the proxy needs to make HTTP GET requests to the upstream Docker daemon.

### Implementation

```go
func subrequest(upstreamPath, httpPath string) (statusCode int, body []byte, err error) {
    conn, err := net.Dial("unix", upstreamPath)
    if err != nil {
        return 0, nil, err
    }
    defer conn.Close()

    req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: docker\r\nConnection: close\r\n\r\n", httpPath)
    conn.Write([]byte(req))

    // Read full response using bufio + http.ReadResponse
    resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
    if err != nil {
        return 0, nil, err
    }
    defer resp.Body.Close()
    body, _ = io.ReadAll(resp.Body)
    return resp.StatusCode, body, nil
}
```

Key requirements:
- Open a **new** connection to the upstream Unix socket for each subrequest (do not reuse the main connection).
- Use `Connection: close` to ensure clean termination.
- Read full response body (these are small JSON payloads).
- Parse JSON from the body.
- Close connection after use.

> Connection churn is acceptable here. The proxy handles roughly 10–100 req/s at peak, and only ownership-checked endpoints incur a subrequest. Pooling subrequest connections is unnecessary at this traffic level and would complicate the lifecycle for no measurable benefit.

---

## 14. Suggested Go Architecture

### Module

```
module github.com/bvt/isolator

go 1.22

require golang.org/x/sys v0.28.0
```

`docker-proxy` lives at `cmd/docker-proxy/main.go` as one binary inside the `github.com/bvt/isolator` module so that internal packages under `internal/proxy/` are importable.

### Directory Structure

```
cmd/docker-proxy/
    main.go                  -- flag parsing, socket setup, signal handling, server start

internal/proxy/
    proxy.go                 -- http.Handler implementation, ReverseProxy setup, routing
    allowlist.go             -- isEndpointAllowed(method, path, query, user) bool
    create.go                -- checkCreate(body []byte, user string) ([]byte, error)
    ownership.go             -- containerOwned, execOwned, filterContainerList
    paths.go                 -- isPathAllowed, resolvePath
    peer_darwin.go           -- getPeerUID for macOS (build tag: //go:build darwin)
    peer_linux.go            -- getPeerUID for Linux (build tag: //go:build linux)
    relay.go                 -- bidirectional relay helper

    allowlist_test.go
    create_test.go
    ownership_test.go
    paths_test.go
    peer_darwin_test.go      -- build tag: //go:build darwin
    integration_test.go
```

### Key Design Decisions

**Use `net/http.Server`** serving on the Unix listener. The handler dispatches based on the allowlist. For most endpoints, use `httputil.ReverseProxy` with a custom `Transport` that dials the upstream Unix socket:

```go
transport := &http.Transport{
    DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
        return net.Dial("unix", upstreamSocketPath)
    },
}

reverseProxy := &httputil.ReverseProxy{
    Director: func(req *http.Request) {
        req.URL.Scheme = "http"
        req.URL.Host = "docker"
    },
    Transport:      transport,
    ModifyResponse: modifyResponseFunc, // for container list filtering
}
```

For **streaming endpoints**, hijack the connection after forwarding the request and switch to bidirectional `io.Copy`.

**Routing order matters.** Two rules:

1. Streaming endpoints must be detected and dispatched to a hijacking handler *before* `ReverseProxy.ServeHTTP` is called — `ReverseProxy` does not expect to share the connection with hijacked I/O.
2. Ownership checks must run *before* hijacking. Streaming endpoints like `attach`, `exec`, `logs`, `wait` are per-container actions and require the same ownership verification as non-streaming actions (§10). Skipping the check on the streaming path would let any sandbox user attach to any container — a multi-tenant isolation breach. The Python prototype runs the ownership subrequest, then enters the relay loop; the Go implementation must preserve that order.

The handler should look like:

```go
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    if !isEndpointAllowed(r.Method, r.URL.RequestURI(), h.user) {
        writeJSONError(w, 403, "endpoint not allowed: " + r.Method + " " + r.URL.Path)
        return
    }
    // Large-body passthrough first: /build and /images/load are not per-container,
    // do not require ownership, and must not be routed through the create-body
    // or ownership branches.
    if isLargeBodyPath(r.Method, r.URL.Path) {
        h.handleLargeBody(w, r) // hijack or ReverseProxy streaming
        return
    }
    // Container create: body inspect + modify, then ReverseProxy.
    if isContainerCreatePath(r.Method, r.URL.Path) {
        h.handleCreate(w, r)
        return
    }
    // Ownership check applies to per-container and per-exec endpoints,
    // INCLUDING streaming ones (attach/exec/logs/wait). Run this before
    // any hijack so unauthorized requests never enter relay mode.
    if needsOwnershipCheck(r) {
        if err := h.checkOwnership(r); err != nil {
            writeJSONError(w, 403, err.Error())
            return
        }
    }
    // Now safe to hijack into bidirectional relay for streaming endpoints.
    if isStreamingPath(r.URL.Path) {
        h.handleStreaming(w, r) // hijacks; ownership already verified above
        return
    }
    h.reverseProxy.ServeHTTP(w, r)
}

// isContainerCreatePath uses an exact regex match, not a substring check, to
// avoid matching attacker-controlled paths that merely contain "/containers/create".
var containerCreateRE = regexp.MustCompile(`^/v[\d.]+/containers/create$`)

func isContainerCreatePath(method, path string) bool {
    return method == "POST" && containerCreateRE.MatchString(path)
}
```

> **Why ownership-then-hijack works**: the ownership check is a separate HTTP subrequest to upstream (§13), not a peek at the in-flight request. By the time `handleStreaming` runs, no bytes from the client request body have been forwarded yet, and the response writer has not been touched. Hijacking is still clean.

For **container create**, read the body, run `checkCreate`, replace the body with the rewritten version, and forward via the reverse proxy (or manually dial upstream and write the modified request).

For **large passthrough endpoints** (`/build`, `/images/load`), either let `ReverseProxy` stream the body (it does this by default for `io.Reader` bodies with unknown length) or hijack and manually relay.

### No CGo Required

The `golang.org/x/sys/unix` package provides pure-Go access to `getsockopt` for peer credentials. No CGo needed.

### Build Command

```bash
go build -o docker-proxy ./cmd/docker-proxy
```

### Minimum Go Version

Go 1.22 (for `slices` package and range-over-int syntax).

### External Dependencies

- `golang.org/x/sys` -- for `unix.GetsockoptXucred` (macOS) and `unix.GetsockoptUcred` (Linux).

No other external dependencies. The standard library provides everything else needed.

---

## 15. Test Suite Specification

All tests use Go's `testing` package. Table-driven tests are preferred.

### 15.1 Unit: `allowlist_test.go`

Table-driven tests for `isEndpointAllowed(method, path, user) bool`. Use `user = "acm"` as default test user.

#### System Endpoints

| Method | Path                          | Expected | Notes                     |
|--------|-------------------------------|----------|---------------------------|
| HEAD   | `/_ping`                      | true     | Unversioned               |
| GET    | `/_ping`                      | true     | Unversioned               |
| POST   | `/_ping`                      | false    | Wrong method              |
| GET    | `/v1.51/_ping`                | true     |                           |
| HEAD   | `/v1.51/_ping`                | false    | HEAD only on unversioned  |
| GET    | `/v1.51/version`              | true     |                           |
| GET    | `/v1.51/info`                 | true     |                           |
| GET    | `/info`                       | true     | Unversioned Go SDK call   |
| POST   | `/info`                       | false    | Wrong method              |
| GET    | `/v1.51/system/df`            | true     |                           |
| GET    | `/v1.51/system/events`        | true     |                           |
| POST   | `/auth`                       | true     | Unversioned               |
| POST   | `/v1.51/auth`                 | true     | Versioned                 |
| GET    | `/auth`                       | false    | Wrong method              |

#### Container Endpoints

| Method | Path                                         | Expected |
|--------|----------------------------------------------|----------|
| GET    | `/v1.51/containers/json`                     | true     |
| POST   | `/v1.51/containers/create`                   | true     |
| POST   | `/v1.51/containers/abc123/start`             | true     |
| POST   | `/v1.51/containers/abc123/stop`              | true     |
| POST   | `/v1.51/containers/abc123/restart`           | true     |
| POST   | `/v1.51/containers/abc123/kill`              | true     |
| POST   | `/v1.51/containers/abc123/pause`             | true     |
| POST   | `/v1.51/containers/abc123/unpause`           | true     |
| POST   | `/v1.51/containers/abc123/wait`              | true     |
| POST   | `/v1.51/containers/abc123/attach`            | true     |
| GET    | `/v1.51/containers/abc123/logs`              | true     |
| GET    | `/v1.51/containers/abc123/json`              | true     |
| GET    | `/v1.51/containers/abc123/stats`             | true     |
| GET    | `/v1.51/containers/abc123/top`               | true     |
| GET    | `/v1.51/containers/abc123/changes`           | true     |
| GET    | `/v1.51/containers/abc123/export`            | true     |
| POST   | `/v1.51/containers/abc123/exec`              | true     |
| DELETE | `/v1.51/containers/abc123`                   | true     |
| POST   | `/v1.51/containers/abc123/update`            | false    | Not in action list        |
| GET    | `/v1.51/exec/exec123/json`                   | true     |
| POST   | `/v1.51/exec/exec123/start`                  | true     |
| POST   | `/v1.51/exec/exec123/resize`                 | true     |

#### Image Endpoints

| Method | Path                                                            | Expected | Notes                  |
|--------|-----------------------------------------------------------------|----------|------------------------|
| GET    | `/v1.51/images/alpine/json`                                     | true     |                        |
| GET    | `/v1.51/images/myrepo/myimage:latest/json`                      | true     |                        |
| GET    | `/v1.51/images/json`                                            | true     |                        |
| POST   | `/v1.51/images/create?fromImage=alpine&tag=latest`              | true     | Valid pull             |
| POST   | `/v1.51/images/create?fromImage=alpine`                         | true     | Tag optional           |
| POST   | `/v1.51/images/create?fromImage=alpine&platform=linux/amd64`    | true     | Platform allowed       |
| POST   | `/v1.51/images/create?fromSrc=http://evil.com/rootkit.tar`      | false    | fromSrc blocked        |
| POST   | `/v1.51/images/create?fromImage=alpine&fromSrc=x`              | false    | fromSrc blocked        |
| POST   | `/v1.51/images/create`                                          | false    | No fromImage           |
| POST   | `/v1.51/images/create?fromImage=`                               | false    | Empty fromImage        |
| POST   | `/v1.51/images/create?fromImage=alpine&repo=evil`               | false    | Extra query key        |
| POST   | `/v1.51/images/alpine/push`                                     | true     |                        |

#### Network Endpoints

| Method | Path                                         | Expected | Notes                     |
|--------|----------------------------------------------|----------|---------------------------|
| GET    | `/v1.51/networks`                            | true     |                           |
| GET    | `/v1.51/networks/json`                       | true     |                           |
| GET    | `/v1.51/networks/bridge`                     | true     | Read-only inspect         |
| GET    | `/v1.51/networks/iso-acm`                    | true     | Inspect own network       |
| POST   | `/v1.51/networks/iso-acm/connect`            | true     | Own network (also general)|
| DELETE | `/v1.51/networks/iso-acm`                    | true     |                           |
| PUT    | `/v1.51/networks/iso-acm`                    | true     | ALL methods on own net    |
| POST   | `/v1.51/networks/iso-slot-0/connect`         | true     | Connect is allowed for any|
| DELETE | `/v1.51/networks/iso-slot-0`                 | true     | Delete is allowed for any |
| PUT    | `/v1.51/networks/iso-slot-0`                 | false    | Not user's iso- network   |
| POST   | `/v1.51/networks/bridge/connect`             | true     |                           |
| POST   | `/v1.51/networks/bridge/disconnect`          | true     |                           |
| POST   | `/v1.51/networks/create`                     | true     | Body validated separately, see 15.3a |

#### Build, Load, Volumes, Events

| Method | Path                          | Expected |
|--------|-------------------------------|----------|
| POST   | `/v1.51/build`                | true     |
| POST   | `/v1.51/images/load`          | true     |
| GET    | `/v1.51/volumes`              | true     |
| POST   | `/v1.51/volumes/create`       | true     |
| DELETE | `/v1.51/volumes/myvol`        | true     |
| GET    | `/v1.51/events`               | true     |

#### Blocked (Must Be False)

| Method | Path                                     |
|--------|------------------------------------------|
| POST   | `/v1.51/swarm/init`                      |
| POST   | `/v1.51/swarm/join`                      |
| POST   | `/v1.51/plugins/pull`                    |
| POST   | `/grpc`                                  |
| GET    | `/v1.51/secrets`                         |
| GET    | `/v1.51/configs`                         |
| POST   | `/v1.51/containers/abc123/update`        |
| GET    | `/some/random/path`                      |

### 15.2 Unit: `paths_test.go`

Test `isPathAllowed(path, user)` with `user = "acm"`.

| Input Path                               | Expected | Notes                                     |
|------------------------------------------|----------|-------------------------------------------|
| `/Users/Workspaces/acm`                  | true     | Workspace root                            |
| `/Users/Workspaces/acm/project`          | true     | Workspace subdir                          |
| `/Users/Workspaces/acm/project/deep/dir` | true     | Deep subdir                               |
| `/Users/acm/tmp`                         | true     | User tmp root                             |
| `/Users/acm/tmp/subdir`                  | true     | User tmp subdir                           |
| `/tmp`                                   | false    | Shared tmp                                |
| `/private/tmp`                           | false    | macOS shared tmp                          |
| `/Users/acm/tmp`                         | true     | (duplicate, but confirms per-user tmp)    |
| `/Users/Workspaces/other`                | false    | Other user's workspace root               |
| `/Users/Workspaces/other/project`        | false    | Other user's workspace subdir             |
| `/Users/Workspaces/acm-evil`             | false    | Prefix attack: "acm-evil" != "acm"       |
| `/Users/Workspaces/acm-evil/project`     | false    | Prefix attack with subdir                 |
| `/Users/Workspaces/acm/../other/secret`  | false    | `..` traversal — must be rejected after `filepath.Clean` |
| `/Users/Workspaces/acm/./project`        | true     | `.` segment — normalized by `filepath.Clean` |
| `/Users/admin`                           | false    | Admin home                                |
| `/etc`                                   | false    | System dir                                |
| `/etc/passwd`                            | false    | System file                               |
| `/var/run`                               | false    | Var dir                                   |
| `/var/run/docker.sock`                   | false    | Docker socket path                        |
| `/`                                      | false    | Root                                      |
| `/Users/Workspaces`                      | false    | Parent of all workspaces (not any user's) |

**Symlink test** (requires filesystem setup in test):
- Create a temp dir under a valid workspace path.
- Create a symlink inside the workspace pointing outside (e.g., to `/etc`).
- `resolvePath(symlink)` should resolve to `/etc`.
- `isPathAllowed(resolvedPath, user)` should return false.

### 15.3 Unit: `create_test.go`

Table-driven tests for `checkCreate(body []byte, user string) (newBody []byte, err error)`. Use `user = "acm"`.

Each test case provides a JSON body and expects either success (with optional body checks) or a specific error message.

| Test Name                        | Body (key fields)                                              | Expected | Error Contains                              |
|----------------------------------|----------------------------------------------------------------|----------|---------------------------------------------|
| clean create                     | `{"Image":"alpine"}`                                           | ok       | Owner label injected                        |
| privileged true                  | `{"HostConfig":{"Privileged":true}}`                           | blocked  | `Privileged is not allowed`                 |
| cap add                          | `{"HostConfig":{"CapAdd":["NET_ADMIN"]}}`                      | blocked  | `CapAdd is not allowed`                     |
| cap drop                         | `{"HostConfig":{"CapDrop":["ALL"]}}`                           | blocked  | `CapDrop is not allowed`                    |
| devices set                      | `{"HostConfig":{"Devices":[{"PathOnHost":"/dev/sda"}]}}`       | blocked  | `Devices is not allowed`                    |
| dns set                          | `{"HostConfig":{"DNS":["8.8.8.8"]}}`                          | blocked  | `DNS is not allowed`                        |
| dns options set                  | `{"HostConfig":{"DNSOptions":["ndots:5"]}}`                   | blocked  | `DNSOptions is not allowed`                 |
| dns search set                   | `{"HostConfig":{"DNSSearch":["evil.com"]}}`                   | blocked  | `DNSSearch is not allowed`                  |
| pid mode host                    | `{"HostConfig":{"PidMode":"host"}}`                            | blocked  | `PidMode is not allowed`                    |
| ipc mode host                    | `{"HostConfig":{"IpcMode":"host"}}`                            | blocked  | `IpcMode is not allowed`                    |
| uts mode host                    | `{"HostConfig":{"UTSMode":"host"}}`                            | blocked  | `UTSMode is not allowed`                    |
| userns mode host                 | `{"HostConfig":{"UsernsMode":"host"}}`                         | blocked  | `UsernsMode is not allowed`                 |
| cgroup ns mode host              | `{"HostConfig":{"CgroupnsMode":"host"}}`                       | blocked  | `CgroupnsMode is not allowed`               |
| security opt                     | `{"HostConfig":{"SecurityOpt":["seccomp=unconfined"]}}`        | blocked  | `SecurityOpt is not allowed`                |
| sysctls set                      | `{"HostConfig":{"Sysctls":{"net.ipv4.ip_forward":"1"}}}`       | blocked  | `Sysctls is not allowed`                    |
| ulimits set                      | `{"HostConfig":{"Ulimits":[{"Name":"nofile","Soft":1024}]}}`   | blocked  | `Ulimits is not allowed`                    |
| runtime set                      | `{"HostConfig":{"Runtime":"nvidia"}}`                          | blocked  | `Runtime is not allowed`                    |
| oom score adj                    | `{"HostConfig":{"OomScoreAdj":-500}}`                          | blocked  | `OomScoreAdj is not allowed`                |
| oom score adj zero               | `{"HostConfig":{"OomScoreAdj":0}}`                             | ok       | Zero is falsy → not blocked                 |
| oom kill disable                 | `{"HostConfig":{"OomKillDisable":true}}`                       | blocked  | `OomKillDisable is not allowed`             |
| oom kill disable false           | `{"HostConfig":{"OomKillDisable":false}}`                      | ok       | False is falsy → not blocked                |
| privileged false                 | `{"HostConfig":{"Privileged":false}}`                          | ok       | False is falsy → not blocked                |
| volumes from                     | `{"HostConfig":{"VolumesFrom":["other_container"]}}`           | blocked  | `VolumesFrom is not allowed`                |
| device cgroup rules              | `{"HostConfig":{"DeviceCgroupRules":["c 1:3 rmw"]}}`          | blocked  | `DeviceCgroupRules is not allowed`          |
| device requests                  | `{"HostConfig":{"DeviceRequests":[{"Count":-1}]}}`             | blocked  | `DeviceRequests is not allowed`             |
| cgroup parent                    | `{"HostConfig":{"CgroupParent":"/system.slice"}}`              | blocked  | `CgroupParent is not allowed`               |
| links                            | `{"HostConfig":{"Links":["db:db"]}}`                           | blocked  | `Links is not allowed`                      |
| extra hosts valid                | `{"HostConfig":{"ExtraHosts":["host.docker.internal:host-gateway"]}}` | ok |                                             |
| extra hosts evil                 | `{"HostConfig":{"ExtraHosts":["evil:1.2.3.4"]}}`              | blocked  | `ExtraHosts entry`                          |
| extra hosts mixed                | `{"HostConfig":{"ExtraHosts":["host.docker.internal:host-gateway","evil:1.2.3.4"]}}` | blocked | `ExtraHosts entry` |
| network mode iso-acm            | `{"HostConfig":{"NetworkMode":"iso-acm"}}`                     | ok       |                                             |
| network mode iso-other           | `{"HostConfig":{"NetworkMode":"iso-slot-0"}}`                  | blocked  | `NetworkMode`                               |
| network mode host                | `{"HostConfig":{"NetworkMode":"host"}}`                        | blocked  | `NetworkMode`                               |
| network mode empty               | `{"HostConfig":{"NetworkMode":""}}`                            | ok       |                                             |
| network mode default             | `{"HostConfig":{"NetworkMode":"default"}}`                     | ok       |                                             |
| network mode bridge              | `{"HostConfig":{"NetworkMode":"bridge"}}`                      | ok       |                                             |
| endpoints config iso-acm        | `{"NetworkingConfig":{"EndpointsConfig":{"iso-acm":{}}}}`      | ok       |                                             |
| endpoints config other           | `{"NetworkingConfig":{"EndpointsConfig":{"iso-other":{}}}}`    | blocked  | `network 'iso-other' not allowed`           |
| bind valid workspace             | `{"HostConfig":{"Binds":["/Users/Workspaces/acm/project:/work"]}}` | ok  | Path rewritten to realpath                  |
| bind etc passwd                  | `{"HostConfig":{"Binds":["/etc/passwd:/etc/passwd"]}}`         | blocked  | `bind mount not allowed`                    |
| bind other user                  | `{"HostConfig":{"Binds":["/Users/Workspaces/other/x:/x"]}}`   | blocked  | `bind mount not allowed`                    |
| bind shared tmp                  | `{"HostConfig":{"Binds":["/tmp/x:/x"]}}`                      | blocked  | `bind mount not allowed`                    |
| bind named volume                | `{"HostConfig":{"Binds":["named-vol:/x"]}}`                   | blocked  | `named volume`                              |
| bind /var/run/docker.sock        | `{"HostConfig":{"Binds":["/var/run/docker.sock:/var/run/docker.sock"]}}` | blocked | `bind mount not allowed` (path fails first) |
| bind docker.sock inside ws       | `{"HostConfig":{"Binds":["/Users/Workspaces/acm/docker.sock:/x"]}}` | blocked | `Docker socket` (substring check fires)     |
| mount type bind workspace        | `{"HostConfig":{"Mounts":[{"Type":"bind","Source":"/Users/Workspaces/acm/project","Target":"/work"}]}}` | ok | |
| mount type bind etc              | `{"HostConfig":{"Mounts":[{"Type":"bind","Source":"/etc","Target":"/etc"}]}}` | blocked | `mount not allowed` |
| mount bind docker.sock           | `{"HostConfig":{"Mounts":[{"Type":"bind","Source":"/Users/Workspaces/acm/docker.sock","Target":"/x"}]}}` | blocked | `Docker socket` |
| mount type volume                | `{"HostConfig":{"Mounts":[{"Type":"volume","Source":"myvol","Target":"/data"}]}}` | blocked | `named volumes are not allowed` |
| mount type tmpfs                 | `{"HostConfig":{"Mounts":[{"Type":"tmpfs","Target":"/tmp"}]}}` | ok      |                                             |
| mount type unknown               | `{"HostConfig":{"Mounts":[{"Type":"npipe","Source":"x","Target":"/x"}]}}` | blocked | `mount type` |
| user root string                 | `{"User":"root"}`                                              | blocked  | `container user`                            |
| user zero                        | `{"User":"0"}`                                                 | blocked  | `container user`                            |
| user zero colon zero             | `{"User":"0:0"}`                                               | blocked  | `container user`                            |
| user zero colon nonzero          | `{"User":"0:1000"}`                                            | blocked  | `container user` (uid=0 with any gid blocked) |
| user 1000                        | `{"User":"1000"}`                                              | ok       |                                             |
| user 1000 colon 1000             | `{"User":"1000:1000"}`                                         | ok       |                                             |
| user empty                       | `{"User":""}`                                                  | ok       |                                             |
| user wrong type                  | `{"User":0}`                                                   | blocked  | `invalid User field type` (400)             |
| owner label injected             | `{"Image":"alpine"}`                                           | ok       | Verify `Labels["dev.boris.isolator.user"]=="acm"` |
| owner label merges               | `{"Image":"alpine","Labels":{"env":"test"}}`                   | ok       | Both `env` and owner label present          |
| multiple binds one bad           | `{"HostConfig":{"Binds":["/Users/Workspaces/acm/ok:/a","/etc/bad:/b"]}}` | blocked | `bind mount not allowed` |
| invalid json                     | `not json at all`                                              | blocked  | `invalid JSON body`                         |
| bind symlink outside workspace   | *(create symlink pointing to /etc during test setup)*          | blocked  | `bind mount not allowed`                    |
| bind path rewrite                | *(valid path)* verify output body has resolved path            | ok       | Output body contains realpath               |
| fidelity: unknown top-level      | `{"Image":"alpine","Healthcheck":{"Test":["CMD","true"]},"StopSignal":"SIGUSR1","FutureField":{"x":1}}` | ok | Output (re-parsed as map) preserves `Healthcheck`, `StopSignal`, `FutureField` exactly |
| fidelity: unknown HostConfig     | `{"Image":"alpine","HostConfig":{"Tmpfs":{"/run":""},"FutureHC":42}}`                              | ok | Output preserves `Tmpfs` and `FutureHC` under HostConfig |

### 15.3a Unit: `network_create_test.go`

Table-driven tests for `checkNetworkCreate(body []byte, user string) (newBody []byte, err error)`. Use `user = "acm"`.

| Test Name                       | Body (key fields)                                                          | Expected | Error / Assertion                            |
|---------------------------------|----------------------------------------------------------------------------|----------|----------------------------------------------|
| clean bridge                    | `{"Name":"my-net","Driver":"bridge"}`                                      | ok       | Owner label `dev.boris.isolator.user=acm` injected |
| missing driver                  | `{"Name":"my-net"}`                                                        | ok       | Empty driver allowed (defaults to bridge)    |
| empty driver                    | `{"Name":"my-net","Driver":""}`                                            | ok       |                                              |
| host driver                     | `{"Name":"my-net","Driver":"host"}`                                        | blocked  | `network driver 'host' not allowed`          |
| overlay driver                  | `{"Name":"my-net","Driver":"overlay"}`                                     | blocked  | `network driver 'overlay' not allowed`       |
| macvlan driver                  | `{"Name":"my-net","Driver":"macvlan"}`                                     | blocked  | `network driver 'macvlan' not allowed`       |
| ipvlan driver                   | `{"Name":"my-net","Driver":"ipvlan"}`                                      | blocked  | `network driver 'ipvlan' not allowed`        |
| missing name                    | `{"Driver":"bridge"}`                                                      | blocked  | name required                                |
| empty name                      | `{"Name":"","Driver":"bridge"}`                                            | blocked  | name required                                |
| iso-acm name                    | `{"Name":"iso-acm","Driver":"bridge"}`                                     | ok       |                                              |
| iso-acm-suffix name             | `{"Name":"iso-acm-test","Driver":"bridge"}`                                | ok       |                                              |
| iso-other name (reserved)       | `{"Name":"iso-other","Driver":"bridge"}`                                   | blocked  | `reserved for another user`                  |
| iso-acmevil prefix attack       | `{"Name":"iso-acmevil","Driver":"bridge"}`                                 | blocked  | `reserved for another user` (prefix is `iso-`, not exact `iso-acm` and not `iso-acm-`) |
| testcontainers random name      | `{"Name":"tc-abc123","Driver":"bridge"}`                                   | ok       | Non-`iso-` names freely allowed              |
| ConfigFrom set                  | `{"Name":"my-net","ConfigFrom":{"Network":"other-net"}}`                   | blocked  | `ConfigFrom is not allowed`                  |
| owner label injected            | `{"Name":"my-net"}`                                                        | ok       | Output `Labels["dev.boris.isolator.user"]=="acm"` |
| owner label merges              | `{"Name":"my-net","Labels":{"env":"test"}}`                                | ok       | Both `env` and owner label present in output |
| IPAM passthrough                | `{"Name":"my-net","IPAM":{"Config":[{"Subnet":"10.99.0.0/24"}]}}`         | ok       | IPAM block reaches upstream unchanged        |
| Internal flag passthrough       | `{"Name":"my-net","Internal":true}`                                        | ok       | Internal preserved                           |
| body too large                  | (>16 MB body)                                                              | blocked  | 413                                          |
| invalid JSON                    | `not json`                                                                 | blocked  | 400 invalid JSON body                        |
| fidelity: unknown fields        | `{"Name":"my-net","Driver":"bridge","FutureField":{"x":1},"Attachable":true}` | ok    | Output (re-parsed) preserves `FutureField` and `Attachable` exactly |

### 15.4 Unit: `ownership_test.go`

Use a mock upstream (Unix socket that returns canned HTTP responses).

| Test Name                | Mock Response                                                    | Expected | Error Contains                              |
|--------------------------|------------------------------------------------------------------|----------|---------------------------------------------|
| container owned          | 200, `{"Config":{"Labels":{"dev.boris.isolator.user":"acm"}}}` | ok       |                                             |
| container wrong owner    | 200, `{"Config":{"Labels":{"dev.boris.isolator.user":"other"}}}`| 403      | `is not owned by acm`                      |
| container no label       | 200, `{"Config":{"Labels":{}}}`                                  | 403      | `is not owned by acm`                      |
| container not found      | 404, `{"message":"not found"}`                                   | 403      | `container not found`                       |
| container inspect 500    | 500, `{"message":"error"}`                                       | 403      | `container inspect failed`                  |
| exec owned               | Exec inspect returns `{"ContainerID":"abc"}`, then container inspect returns owned | ok | |
| exec wrong owner         | Exec inspect returns `{"ContainerID":"abc"}`, container owned by other | 403 | `is not owned by acm`             |
| exec not found           | 404 on exec inspect                                              | 403      | `exec not found`                            |
| exec missing containerID | 200, `{}`                                                        | 403      | `exec inspect missing ContainerID`          |

### 15.5 Integration: `integration_test.go`

Spin up a real proxy instance with a mock upstream Unix socket.

**Setup:**
- Create two temp Unix sockets: one for mock upstream, one for the proxy.
- Start mock upstream: a `net.Listener` that accepts connections and returns scripted HTTP responses.
- Start the proxy: instantiate the proxy handler, bind to the proxy socket.
- Connect to the proxy via `net.Dial("unix", proxySocket)`.

**Test Cases:**

| Test Name                     | Action                                                         | Expected                                         |
|-------------------------------|----------------------------------------------------------------|--------------------------------------------------|
| endpoint 403                  | `POST /v1.51/swarm/init`                                      | 403 JSON with "endpoint not allowed"             |
| ping passthrough              | `GET /_ping`                                                   | Response from mock upstream arrives at client     |
| container create clean        | `POST /v1.51/containers/create` with valid body                | Upstream receives body with owner label injected  |
| container create privileged   | `POST /v1.51/containers/create` with `Privileged:true`         | 403 before request reaches upstream               |
| container list filtering      | Mock returns 3 containers (2 owned, 1 not)                     | Client receives 2 containers                     |
| build passthrough             | `POST /v1.51/build` with >16 MB body                          | Full body reaches upstream without 413            |
| images load passthrough       | `POST /v1.51/images/load` with >16 MB body                    | Full body reaches upstream without 413            |
| streaming relay               | `/containers/{id}/attach` + send data both directions; mock returns owned container on inspect | Data flows bidirectionally                        |
| streaming ownership denied    | `/containers/{id}/attach`; mock returns container owned by other user on inspect | 403 before any hijack; client never enters relay  |
| streaming exec ownership denied | `/exec/{id}/start`; mock exec inspect returns ContainerID; container inspect returns wrong owner | 403 before any hijack                            |
| socket permissions            | Check socket file mode and ownership                           | Mode 0600, owned by target UID                   |
| keep-alive                    | Send two sequential requests on same connection                | Both handled correctly                            |
| peer UID enforcement          | Connect with wrong UID (skip if running as root)               | Connection closed immediately                     |
| duplicate content-length      | Send request with two Content-Length headers                   | 400 response                                      |
| body too large                | `POST /v1.51/containers/create` with >16 MB body              | 413 response                                      |
| connection timeout            | Connect, send partial request, wait                            | Connection closed after 30s                       |

### 15.6 Platform-Specific: `peer_darwin_test.go`

Build tag: `//go:build darwin`

| Test Name                  | Action                                                      | Expected                                  |
|----------------------------|-------------------------------------------------------------|-------------------------------------------|
| same process UID           | Create Unix socket pair, call getPeerUID                    | Returns `os.Getuid()`                     |
| subprocess UID             | Fork child via `exec.Command`, child connects to socket, call getPeerUID | Returns the test process's UID — confirms the syscall works across processes. Note: this does **not** test cross-UID enforcement, which would require dropping privileges (root). |

---

## 16. Error Response Format

All proxy-generated error responses use this format:

```
HTTP/1.1 <status>
Content-Type: application/json
Content-Length: <len>
Connection: close

{"message": "isolator: <description>"}
```

The `"isolator: "` prefix distinguishes proxy errors from Docker daemon errors.

Status codes used:
- **400 Bad Request**: malformed request (bad JSON, bad Transfer-Encoding, duplicate Content-Length)
- **403 Forbidden**: policy violation (endpoint blocked, ownership failed, dangerous config)
- **413 Payload Too Large**: body exceeds MaxBodySize

---

## 17. Logging

All log output goes to stdout with format:
```
[<user>] <message>
```

Log these events:
- Startup: socket path, UID, mode, upstream path.
- `BLOCKED endpoint: <METHOD> <path>` -- endpoint allowlist rejection.
- `BLOCKED: <reason>` -- create validation, ownership check, or smuggling defense rejection.
- `BLOCKED: peer uid=<N> does not match expected uid=<M>` -- peer UID mismatch.
- `ERROR: <message>` -- unexpected errors during connection handling.

**Output must go to stdout, not stderr.** Go's `log` package defaults to `os.Stderr`. Either call `log.SetOutput(os.Stdout)` once at startup, or use `fmt.Fprintln(os.Stdout, ...)` directly. The `iso` launcher script reads from stdout via the proxy's log file — logs sent to stderr will be missed.

Go does not add stdio-style userspace buffering to `os.Stdout`; each `Write` is a `write(2)` syscall, and `log.Printf` writes one line per call. No explicit flushing is required.

---

## 18. Known Python Bugs Fixed by Go Implementation

### Framing / lifecycle bugs (eliminated by `net/http`)

| # | Python Bug | Go Fix |
|---|-----------|--------|
| 1 | Chunked body end-detection uses `b"\r\n0\r\n"` substring match, which can false-positive inside tar data | `net/http` decodes chunked encoding correctly by construction |
| 2 | `read_http_response` uses `body_buf.endswith(b"0\r\n\r\n")` which can false-positive on binary data ending with those bytes | `net/http` / `http.ReadResponse` reads chunked correctly |
| 3 | `leftover` bytes in keep-alive loop require exact manual byte accounting, prone to off-by-one | `net/http.Server` handles connection reuse and request framing internally |
| 4 | No connection timeout on stalled clients — a client that connects but never sends data holds a goroutine/thread forever | `ReadTimeout` and `IdleTimeout` on `http.Server` enforce 30s deadline |

### Allowlist / behavior gaps (intentional spec deviations from Python)

These are deliberate fixes — the Go implementation does not match Python here:

| # | Python Behavior | Go Spec Behavior | Reason |
|---|----------------|-----------------|--------|
| 5 | `/images/load` is in `_LARGE_BODY_PATHS` for streaming passthrough but **not** in the endpoint allowlist — `docker load` returns 403 before reaching the passthrough | `/images/load` allowed in §6.5 and handled as large-body passthrough in §8 | Python's allowlist gap makes the passthrough unreachable; spec closes the gap |
| 6 | `/events` is allowed by the endpoint allowlist but not in the streaming relay set — `read_http_response` would block reading chunked events forever | `/events` is a streaming endpoint in §12 (bidirectional relay) | Events stream NDJSON indefinitely; relay is the only correct behavior |
| 7 | Dead `path.endswith("/json?all=1")` check in main handler skips ownership for `/containers/{id}/json?all=1` (likely accidental — the inner function already returns `None` for the legitimate `/containers/json?all=1` list endpoint) | Spec applies ownership check uniformly via `get_api_version_and_id` semantics | The Python carve-out is not a documented behavior and `?all=1` on inspect is not a real Docker API |

---

## 19. Known Limitations & v2 Roadmap

This section catalogues every gap, ambiguity, and accepted risk that the v1 spec deliberately leaves open. v2 should close these or make a documented decision to keep them.

### 19.1 Cross-tenant information leakage (accepted v1 risk)

The following endpoints return data about resources owned by other users on the same Docker host. v1 does not filter them. A sandbox user can enumerate but not modify cross-tenant resources via these reads.

| Endpoint                       | Leaked information                                                  |
|--------------------------------|---------------------------------------------------------------------|
| `GET /v<ver>/events`           | Container start/stop, image pull, volume create events for all users |
| `GET /v<ver>/system/events`    | Same as `/events` (alias)                                           |
| `GET /v<ver>/system/df`        | Disk usage of all images, all volumes, all containers system-wide   |
| `GET /v<ver>/networks`         | Names of all networks including `iso-<other-user>`                  |
| `GET /v<ver>/networks/<name>`  | Inspect data for any network (subnet, attached containers)          |
| `GET /v<ver>/images/json`      | Names/tags of all images on host (shared image cache)               |
| `GET /v<ver>/images/<name>/json` | Inspect of any image                                              |
| `GET /v<ver>/info`             | Daemon-wide stats: container count, image count, plugins, etc.      |

**Why accepted in v1:** Filtering each of these requires per-endpoint response rewriting (events would need NDJSON stream parsing mid-relay), which is substantial work and not part of the threat model for this release. The user is on a shared host *by design*; the proxy protects against modification and bind-mount escape, not against host-level metadata visibility.

**v2 actions:**
- Filter `/events` and `/system/events` by container/image/network owner label (requires NDJSON-aware streaming filter).
- Filter `/networks` list to networks the user owns or is participating in.
- Filter `/system/df` by owner label (drop other users' containers/volumes from the response).
- Optionally filter `/images/json` if image-cache enumeration becomes a concern (shared cache makes this debatable).

### 19.2 Volumes API is unrestricted (v1)

See §6.6. All verbs on `/volumes[/...]` are allowed without owner-label filtering. v2 work:
- Inject owner label on `POST /volumes/create` (mirror §9.8).
- Owner subrequest before `DELETE`, `GET <name>`, etc.
- Filter `GET /volumes` list response by owner label (mirror §11).

### 19.3 Network endpoint gaps (v1)

See §6.4. Two open items:

- **Connect/disconnect cross-tenant.** A user can `POST /networks/<other-user-iso>/connect` to attach their container to another user's iso network. v2: ownership subrequest on connect/disconnect that checks the *network's* owner label (now injected at create time, see §6.4 network create validation).
- **Delete cross-tenant.** `DELETE /networks/<name>` does not check ownership. v2: same subrequest mechanism.

### 19.4 Operations intentionally blocked in v1

These Docker API endpoints are not in the allowlist (§6) and therefore return 403. They are blocked deliberately, not by oversight. Each row notes the user-visible impact and the v2 plan.

| Endpoint                                            | Blocked because                                          | User impact                | v2 plan                                       |
|-----------------------------------------------------|----------------------------------------------------------|----------------------------|-----------------------------------------------|
| `GET/PUT/HEAD /v<ver>/containers/<id>/archive`      | `docker cp` lets a user pull arbitrary files out of a container — not a security risk per se, but no validation written | `docker cp` does not work | Allow with ownership check on the container  |
| `POST /v<ver>/containers/<id>/rename`               | Not validated                                            | `docker rename` fails      | Allow with ownership check                    |
| `POST /v<ver>/containers/<id>/update`               | Resource-limit field set is large; not validated         | `docker update` fails      | Define an allowed-fields whitelist + ownership |
| `POST /v<ver>/images/<name>/tag`                    | Not validated                                            | `docker tag` fails         | Allow (low risk; tagging is local-only)       |
| `DELETE /v<ver>/images/<name>`                      | Image cache is shared; deleting another user's image is disruptive | `docker rmi` fails | Owner-label images at build/pull time, restrict delete to owned images |
| `GET /v<ver>/images/<name>/history`                 | Not in allowlist                                         | `docker history` fails     | Allow (read-only metadata)                    |
| `POST /v<ver>/containers/prune`                     | Bulk operation; would prune across users                 | `docker container prune` fails | Filter to owned containers                |
| `POST /v<ver>/images/prune`                         | Same as above                                            | `docker image prune` fails | Filter to owned images                        |
| `POST /v<ver>/volumes/prune`                        | Same as above                                            | `docker volume prune` fails | Filter to owned volumes (after 19.2)         |
| `POST /v<ver>/networks/prune`                       | Same as above                                            | `docker network prune` fails | Filter to owned networks                    |
| `POST /v<ver>/swarm/*`, `POST /v<ver>/services/*`   | Multi-host orchestration; out of scope for sandboxes     | Swarm mode unavailable     | No plan — out of scope                        |
| `GET /v<ver>/secrets`, `GET /v<ver>/configs`        | Swarm features                                           | Unavailable                | No plan — out of scope                        |
| `POST /v<ver>/plugins/*`                            | Plugins run with elevated privilege on the host          | Plugin install fails       | No plan — security boundary                   |
| `POST /v<ver>/grpc`                                 | BuildKit gRPC endpoint; not a documented stable API      | BuildKit gRPC mode fails   | Evaluate when buildx becomes default          |

> **For implementers:** the table above is documentation of intent, not new spec. The §6 allowlist is authoritative — anything not matched there returns 403, regardless of whether it appears in this table.

### 19.5 Implementation ambiguities resolved with a default (revisit in v2)

| § | Ambiguity                                                          | v1 default                                          | v2 question                                  |
|---|--------------------------------------------------------------------|-----------------------------------------------------|----------------------------------------------|
| §3 | `WorkspacesDir = "/Users/Workspaces"` is hardcoded                 | Constant; only correct on darwin deployments        | Make configurable via flag or build tag for Linux deployments |
| §10 | 403 `"container not found"` vs `"is not owned by <user>"` distinguishes existence | Two messages, leaks existence to attacker | Collapse to a single message?               |
| §13 | Subrequest opens a new upstream connection each time               | Acceptable at 10–100 req/s                          | Add pooling if traffic grows                 |
| §15.6 | Only `peer_darwin_test.go` specified                              | Linux peer-cred path is implemented but untested via spec | Add `peer_linux_test.go` with same shape    |
| §17 | Log format mixing `[<user>]` prefix with `BLOCKED:`/`ERROR:` markers | Implementation choice; spec doesn't lock format | Pin format if downstream parsers depend on it |
| §6.2 | Container ID regex `[a-zA-Z0-9_.-]+` allows `..` segments         | Docker upstream rejects malformed IDs               | Tighten to `[a-zA-Z0-9_-]{1,64}` defensively |
| §9 | Body size limit (16 MB) only applied to `/containers/create`        | Other JSON bodies (`/exec`, `/networks/create`) are not bounded | Apply `MaxBodySize` to all JSON bodies      |

### 19.6 Out-of-scope by design

These are not bugs and not v2 work — they are the threat model boundary:

- **Network egress filtering**: pf, not the proxy.
- **Image content scanning**: not the proxy's job.
- **Resource exhaustion (container count, exec count per user)**: cgroups + Docker defaults; the proxy does not throttle.
- **Side-channel attacks (CPU cache, kernel exploits)**: kernel/Docker boundary.
- **Host-level metadata visibility (kernel version, daemon config)**: a sandbox user is on a shared host and can `uname` from inside their own container.
