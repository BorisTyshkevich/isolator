# Loopback policy: pass quick on lo0

> **Decision (2026-04-27):** sandbox users have unrestricted access to the
> macOS loopback interface (`lo0`), both as clients and as listeners. The
> generated pf anchor includes `pass quick on lo0 all` as its first rule.

This document explains *why* — what the policy permits, what it excludes,
which threats it accepts and which it still defends against.

## What changed

Before: each sandbox user had a `pass out proto tcp from any to 127.0.0.1
user N flags S/SA keep state` rule. This let sandbox users **initiate**
loopback connections (the SYN matched the rule, state was created, replies
flowed via state). It did **not** let sandbox users *accept* loopback
connections — when a sandbox process listened on a port, the SYN-ACK reply
to an incoming SYN had `flags=SA`, not `flags=S`, didn't match the pass
rule, fell through to the `block out tcp user N` rule, and got dropped.
Result: sandbox users could connect outbound to localhost services but
their own listeners were broken (clients hung in `SYN_SENT`).

After: a single `pass quick on lo0 all` rule at the top of the anchor
exempts all `lo0` traffic from further filtering. The `quick` keyword
makes the rule terminal — matching packets bypass the block rules
entirely, regardless of TCP flags.

The trigger was [`franchb/embedded-clickhouse`](https://github.com/franchb/embedded-clickhouse):
running ClickHouse as a child process under a sandbox user, with the test
process (also that user) connecting to it on a random localhost port.
Both sides ran as `altinity`; the SYN-ACK went out from `altinity`'s
clickhouse, hit the block rule, never reached the test process.

## Why "set skip on lo0" wasn't used directly

`set skip on <interface>` is the canonical way to bypass pf for a given
interface, but it's a global directive that can only appear in the main
`/etc/pf.conf` ruleset. Inside an anchor like ours, only rule statements
are allowed.

`pass quick on lo0 all` inside the anchor is the equivalent: any packet
on `lo0` matches and exits rule evaluation immediately. The behavioral
difference is nil for our use case.

## What this permits

The policy intentionally allows:

1. **Sandbox-user TCP/UDP listeners on loopback.** Any sandbox user can
   bind a server socket on `127.0.0.1` and accept incoming connections.
   Used by: `embedded-clickhouse`, test HTTP servers (`httptest.NewServer`),
   mock services, MCP local stdio servers, Chrome DevTools, etc.
2. **Cross-user loopback access.** Sandbox user A's process can connect
   to sandbox user B's localhost listener (and vice versa). The admin
   user can connect to any sandbox user's listener and vice versa.
3. **Connections to admin's local services.** Sandbox users can reach any
   service the admin (or any other user) is running on `127.0.0.1`,
   regardless of port.

## What this still blocks

The policy does **not** weaken any of these:

1. **External network egress.** `block out` rules for sandbox UIDs still
   apply on every interface other than `lo0`. Exfiltration to a public
   IP is gated by the same per-user pf table allowlist as before.
2. **Binding to non-loopback interfaces.** `pass quick on lo0` only
   matches `lo0`; binding a sandbox-user listener on `0.0.0.0` or any
   real interface IP doesn't get this exemption. The block rules still
   drop all non-loopback inbound for sandbox users.
3. **Privileged ports.** Standard Unix semantics: sandbox users (UID ≥
   500) can't bind to ports below 1024. Squatting on `:80`, `:443`,
   `:5432` etc. is impossible at the OS level, before pf is consulted.
4. **The docker-proxy security model.** The proxy listens on a Unix
   socket, not TCP. pf doesn't see Unix sockets at all. Loopback policy
   has zero interaction with the proxy's per-user UID enforcement, the
   bind-mount rules in `IsPathAllowed`, or the container-create
   validation.
5. **macOS file permissions.** Loopback access doesn't grant filesystem
   access. A sandbox user reaching another sandbox user's localhost
   service is constrained to whatever that service exposes, not the
   user's full home directory.

## Threats considered

### T1 — Sandbox user attacks admin's local dev services

Status: **already possible before this change; not introduced.**

Sandbox users have always had outbound to 127.0.0.1 (the previous
`pass out ... flags S/SA` rule allowed it). If the admin runs Postgres,
Redis, MongoDB, or other dev tools on localhost without strong auth
(common assumption "loopback = trusted"), sandbox users can connect to
them today and could before this change.

Mitigation (recommended for admins, not enforced by isolator): treat
localhost like any other interface. Configure local services to require
auth, or bind them to a Unix socket instead of a TCP port. Audit:
```bash
sudo lsof -iTCP -sTCP:LISTEN -P | grep 127.0.0.1
```
Anything sensitive should not be reachable from sandbox UIDs. If it is,
either add auth, switch to Unix socket, or move the service to a
namespace not visible to sandbox users.

### T2 — Cross-sandbox-user data exfiltration via loopback

Status: **enabled by this change; equivalent to existing capability via
shared `/tmp` and home-directory ownership.**

Sandbox user A could run a service on `127.0.0.1:NNNN`, sandbox user B
could connect to it, and they could exchange data. This is new — before
this fix, A's listener accept was broken.

But: sandbox users already share `/tmp` (read-write by all) and could
already pass data via files there. The only new capability is
**synchronous** communication, which adds nothing in terms of what data
can be exfiltrated — only how. No additional egress is enabled, since
each user's pf-blocked outbound is unchanged.

### T3 — SSRF via redirect into local listener

Status: **already possible before this change; not introduced.**

A sandbox AI agent that fetches an attacker-controlled URL and follows
redirects could be redirected to `localhost:NNNN`, hitting either a
local service or its own loopback. This worked before this change (the
existing pass rule allowed outbound to 127.0.0.1).

Mitigation (in agent code, not in firewall): HTTP clients used by AI
agents should disable cross-origin redirects to private IP space. This
is a code-level concern, not a firewall one.

### T4 — Sandbox impersonates a trusted localhost service

Status: **enabled; mitigated by privileged port restriction.**

Sandbox user A binds `127.0.0.1:5432` first (before the legitimate
Postgres starts). A process expecting Postgres connects to A's fake
service and may leak credentials in the handshake.

Why this is small in practice:
- 5432 is unprivileged, so this is possible — but
- Squatting requires winning a race against the legitimate service
  starting, and
- Postgres-style auth involves SCRAM-SHA-256 by default — handshake
  doesn't leak useful credentials to a fake server unless misconfigured
- Standard system-service ports (1–1023) are unreachable to sandbox
  users by Unix permissions.

If a specific port is sensitive (a custom dev service on `:8000`, say),
the operator can preemptively bind it themselves before any sandbox
runs.

### T5 — Sandbox listener as command-and-control channel

Status: **mitigated by non-loopback rules.**

A compromised sandbox user wants to receive commands from an external
attacker. They run a listener on a high port, the attacker connects.

`pass quick on lo0` only matches `lo0`. External connections arrive on
the real network interfaces (`en0`, etc.) and are still subject to the
default macOS posture (which doesn't expose arbitrary ports outside the
host without explicit configuration). The block rules also still drop
sandbox-user outbound to non-allowlisted destinations, so even if a
listener were somehow reachable externally, the sandbox couldn't
exfiltrate effectively.

## Operator audit recommendation

Run this when setting up isolator on a new host, and periodically:

```bash
# Anything bound to localhost that sandbox users can reach
sudo lsof -iTCP -sTCP:LISTEN -P | grep 127.0.0.1
```

For each entry, ask:
- Does it require auth on connect?
- Is the auth strong enough (not just "you can connect, therefore you're trusted")?
- Could a sandbox user abuse it?

If the answer is "no auth, sensitive operations" — change the service.
isolator deliberately does not firewall localhost because doing so would
break the legitimate use cases (test servers, embedded databases, MCP
stdio bridges) that motivated this policy in the first place.

## What would tighten this further

If a future deployment needs strict cross-user loopback isolation, the
options ranked by surgical-ness:

1. **Per-user lo0 UID matching.** Replace `pass quick on lo0 all` with
   `pass quick on lo0 user N` for each sandbox UID. Each user can talk
   to themselves on loopback but not to other users. macOS pf supports
   `user` on `pass` rules.
2. **Drop loopback access entirely for restricted users.** Remove the
   loopback pass; restricted users have no localhost. Breaks every
   in-process testing pattern, MCP stdio, etc. — extreme.
3. **Network namespaces.** Move beyond pf into per-user network
   namespaces (Linux) or per-user VM (macOS, expensive). Different
   architecture; out of scope.

None of these are needed for the current threat model. Documenting them
here so the option-space is preserved for future audits.

## Tests / verification

After applying:

```bash
# Sandbox user can listen + accept on loopback (was broken)
sudo -u altinity python3 -c "
import socket, threading, time
srv = socket.socket(); srv.bind(('127.0.0.1', 0)); srv.listen(1)
port = srv.getsockname()[1]
threading.Thread(target=lambda: socket.create_connection(('127.0.0.1', port), timeout=3).close()).start()
srv.settimeout(3); srv.accept()[0].close()
print('OK')
"

# Sandbox user still blocked on external egress (control)
sudo -u altinity curl -sS -m 3 https://1.1.1.1/ 2>&1 | head -2
# Expected: timeout (pf drop), not response

# Allowed host still works
sudo -u altinity curl -sS -m 5 https://api.github.com/ 2>&1 | head -1
# Expected: HTTP response from GitHub
```
