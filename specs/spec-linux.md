# Isolator — Linux Spec

Run AI coding agents inside per-user namespace isolation on Linux.
No Docker-as-sandbox. Uses namespaces, ACL, and a proxy for network control.

## Architecture

```
admin (has root via sudo)
 |
 |-- iso acm claude                 # ACM project
 |-- iso click codex                # ClickHouse project
 |-- iso tools claude               # shared tooling
 |
 +-- acm    uid=600  /home/acm/    chmod 700
 |   namespace: net + mount + pid
 |   network: localhost:3128 -> proxy -> whitelisted hosts only
 |
 +-- click  uid=601  /home/click/  chmod 700
     namespace: net + mount + pid
     network: localhost:3128 -> proxy -> whitelisted hosts only

proxy (tinyproxy, runs as root/dedicated user)
 |-- listens on veth gateway per namespace
 |-- allowlist from config per user
 |-- everything else denied
```

---

## Config

Same format as macOS, `/etc/isolator/config.toml`:

```toml
admin = "boris"

[global]
hosts = [
    "registry.npmjs.org",
    "pypi.org",
    "files.pythonhosted.org",
]

[users.acm]
uid = 600
hosts = [
    "api.anthropic.com",
    "sentry.io",
]

[users.click]
uid = 601
hosts = [
    "api.anthropic.com",
    "api.openai.com",
]
```

Users auto-added to config if not present.

---

## The `iso` command

Same CLI as macOS:

```bash
iso create <name> [options]       # Create isolated user
iso create --all [options]        # Create all from config
iso delete <name>                 # Delete user and home
iso pf                            # Generate proxy allowlists + namespace setup
iso list                          # List configured users
iso <user> [command] [args...]    # Run command as user (default: bash)
```

### Create options

```bash
--keychain              # Copy credentials (stored in /etc/isolator/keychain/, root 400)
--token TOKEN           # Write OAuth token to .credentials.json
--from USER             # Copy config from USER instead of admin
```

---

## iso create

Same purpose as macOS. Differences:

| macOS | Linux |
|-------|-------|
| `dscl . -create` | `useradd --system --shell /bin/bash` |
| `chmod +a "admin allow read,write,..."` | `setfacl -Rm u:admin:rwX` + `setfacl -dRm u:admin:rwX` |
| IsHidden 1 | `--system` flag (no login screen) |
| `/Users/<name>` | `/home/<name>` |
| macOS Keychain | Credentials file in `/etc/isolator/keychain/` (root 400) |

Steps:

1. `useradd` — create user (system, no password, home `/home/<name>`)
2. `chmod 700` home
3. `setfacl` — grant admin recursive read/write + default ACL for new files
4. Copy shell config from source user (detect bash/zsh from `/etc/passwd`)
5. Copy Claude config — same as macOS (settings.json + overrides, .claude.json, skills, plugins)
6. Copy Codex config — same as macOS (sanitized config.toml with bypass overrides, auth.json, plugins)
7. Inject auth — keychain-equivalent (root-only credential file) or plaintext token
8. Create skeleton dirs
9. Root-own config files, chmod 444
10. Exclude from backups (if applicable)

### ACL syntax

```bash
# Admin can read and write everything in user home
setfacl -Rm u:admin:rwX /home/acm

# Default ACL — applies to all new files/dirs automatically
setfacl -dRm u:admin:rwX /home/acm
```

---

## iso run (namespace launch)

This is the Linux-specific part. Replaces the simple `sudo -u acm -i claude` from macOS.

```bash
iso acm claude
iso click codex --model o3
iso acm bash
```

### What it does

1. **Start per-user proxy** (if not already running)
2. **Create network namespace** with veth pair
3. **Create mount namespace** — bind-mount shared tools read-only, hide sensitive dirs
4. **Create PID namespace** — agent can't see other processes
5. **Set cgroup limits** — memory, CPU
6. **Unlock credentials** — read root-only password, make available to agent
7. **Drop into namespace as user** and exec the agent command

### Bypass permissions

Same as macOS:
- `iso acm claude` → auto-injects `--permission-mode bypassPermissions`
- Codex bypass via `config.toml` overrides (`approval_policy = "never"`, `sandbox_mode = "danger-full-access"`)

### Namespace setup

```bash
# Create network namespace
ip netns add acm-ns

# Create veth pair: host side + namespace side
ip link add veth-acm type veth peer name eth0-acm
ip link set eth0-acm netns acm-ns

# Assign addresses
ip addr add 10.200.0.1/30 dev veth-acm
ip netns exec acm-ns ip addr add 10.200.0.2/30 dev eth0-acm

# Bring up
ip link set veth-acm up
ip netns exec acm-ns ip link set eth0-acm up
ip netns exec acm-ns ip link set lo up

# Default route inside namespace -> host side (where proxy lives)
ip netns exec acm-ns ip route add default via 10.200.0.1

# DNS -> proxy host
echo "nameserver 10.200.0.1" > /etc/netns/acm-ns/resolv.conf
```

Each user gets its own /30 subnet: `10.200.N.0/30`.

### Mount namespace

```bash
unshare --mount --propagation slave bash -c '
    # Shared tools — read-only bind mounts
    mount --bind /usr/local /usr/local
    mount -o remount,ro,bind /usr/local
    mount --bind /opt /opt
    mount -o remount,ro,bind /opt

    # Hide sensitive dirs — mount empty tmpfs over them
    mount -t tmpfs tmpfs /home/admin
    mount -t tmpfs tmpfs /root

    # Run as user
    exec su - acm -c "claude --permission-mode bypassPermissions"
'
```

The agent sees `/usr/local` and `/opt` (read-only) but `/home/admin` and `/root` are empty tmpfs — not permission-denied, completely invisible.

### PID namespace

```bash
unshare --pid --fork ...
```

Agent only sees its own processes. Can't enumerate or signal anything outside the namespace.

### cgroup limits

```bash
systemd-run --uid=acm --scope \
    --property=MemoryMax=4G \
    --property=CPUQuota=200% \
    ...
```

### Combined launch

```bash
ip netns exec acm-ns \
    unshare --mount --pid --fork \
    bash -c '
        # Mount setup (read-only shared tools, hide sensitive dirs)
        ...
        # cgroup (or use systemd-run wrapper)
        ...
        # Drop privileges and exec
        exec su - acm -c "claude --permission-mode bypassPermissions"
    '
```

---

## Network Proxy

### Why proxy instead of firewall

- Network namespace starts with zero connectivity
- No firewall rules to maintain or refresh
- Domain-level allowlisting (not IP-level — no CDN leakage)
- No DNS resolution/refresh cron needed

### tinyproxy

One tinyproxy instance per user, listening on the veth gateway.

Per-user config (`/etc/isolator/proxy/acm.conf`):

```
User nobody
Group nogroup
Port 3128
Listen 10.200.0.1
MaxClients 20

FilterDefaultDeny Yes
Filter "/etc/isolator/proxy/acm.filter"
ConnectPort 443
```

Filter file (`/etc/isolator/proxy/acm.filter`):

```
^api\.anthropic\.com$
^sentry\.io$
^registry\.npmjs\.org$
^pypi\.org$
^files\.pythonhosted\.org$
```

Generated from config by `iso pf`.

### How the agent uses it

The profile sets:

```bash
export http_proxy="http://10.200.0.1:3128"
export https_proxy="http://10.200.0.1:3128"
export HTTP_PROXY="$http_proxy"
export HTTPS_PROXY="$https_proxy"
export no_proxy="localhost,127.0.0.1"
```

Most tools respect these env vars. For tools that don't, the namespace has no route to anything except the proxy — connections just fail.

---

## Docker on Linux

Docker runs natively on Linux (no VM). Container processes are real Linux processes.

### Network isolation for containers

Two options, depending on Docker setup:

**Option A: Docker inside namespace (recommended)**

Run Docker daemon per user inside the network namespace. Containers inherit the namespace's network restrictions — all traffic goes through the proxy.

**Option B: Shared Docker daemon + iptables**

Same approach as macOS: per-user Docker networks with `DOCKER-USER` iptables rules. Simpler but weaker (agent could create unrestricted network).

### Docker socket

Native `/var/run/docker.sock` — no hardlink needed (no VM indirection on Linux).

---

## Comparison: macOS vs Linux

| Feature | macOS (isolator) | Linux (isolator) |
|---------|-----------------|-----------------|
| User creation | dscl | useradd |
| File ACL | chmod +a | setfacl |
| Network isolation | pf by UID | Network namespace |
| Network control | IP allowlist (pf table) | Domain allowlist (proxy) |
| Container network | Docker iptables (DOCKER-USER) | Namespace or iptables |
| DNS handling | Resolve + refresh with `iso pf` | Proxy handles it |
| Mount isolation | None (just permissions) | Mount namespace (dirs invisible) |
| PID isolation | None (ps shows all) | PID namespace |
| Resource limits | None | cgroups (memory, CPU) |
| Shared tools | /opt/homebrew read-only | /usr/local bind-mount read-only |
| Auth storage | macOS Keychain (encrypted) | /etc/isolator/keychain/ (root 400) |
| Docker socket | Hardlink (OrbStack VM workaround) | Native /var/run/docker.sock |
| Backup exclusion | tmutil addexclusion | N/A (no Time Machine) |
| Config/auth copy | Same | Same |
| Bypass permissions | Same | Same |

Linux version is strictly stronger in mount, PID, and network isolation.

---

## Security model

| Layer | Mechanism | Bypassable? |
|-------|-----------|:-:|
| Filesystem | chmod 700 + setfacl | No |
| Network | Namespace + proxy | No (no route exists) |
| Container network | Namespace or iptables | No |
| Mount | Sensitive dirs hidden via tmpfs | No (invisible) |
| PID | PID namespace | No (can't see others) |
| Resources | cgroups v2 | No (kernel) |
| Privilege | No password, no sudoers | No |
| Config | Root-owned, chmod 444 | No |
| Shared tools | Read-only bind mount | No |
| Auth | /etc/isolator/keychain/ (root 400) | No |

---

## Profile

`/etc/isolator/profile`:

```bash
# Keychain unlocked by iso script

# PATH — prepend local installs
export PATH="$HOME/.local/bin:$HOME/.npm-global/bin:$PATH"

# npm — local installs, no postinstall hooks
export NPM_CONFIG_PREFIX="$HOME/.npm-global"
export NODE_PATH="/usr/local/lib/node_modules"
export NPM_CONFIG_ignore_scripts=true

# pip — user installs only
export PIP_USER=1
export PYTHONUSERBASE="$HOME/.local"

# Docker — per-user isolated network
export DOCKER_NETWORK="iso-$(whoami)"

# Proxy — all traffic through allowlisting proxy
export http_proxy="http://10.200.0.1:3128"
export https_proxy="http://10.200.0.1:3128"
export HTTP_PROXY="$http_proxy"
export HTTPS_PROXY="$https_proxy"
export no_proxy="localhost,127.0.0.1"

# Bypass permissions — fallback for interactive shells
alias claude='claude --permission-mode bypassPermissions'
```

---

## Usage

```bash
# One-time setup
iso create acm --keychain
iso create click --keychain
iso pf

# Run agents
iso acm claude
iso click codex

# Read their work
cat /home/acm/workspace/main.py

# Refresh after config changes
iso create acm                    # re-copies config
iso pf                            # refresh proxy allowlists
```

---

## Open questions

1. **systemd-run vs manual namespaces** — systemd-run can do most of this in one command but is less portable. Manual unshare works everywhere.
2. **Shared proxy vs per-user proxy** — one tinyproxy per user is simpler; shared proxy with per-source-IP filters is more efficient.
3. **Rootless option** — user namespaces allow unprivileged namespace creation. Could work without root after initial setup, with slirp4netns for networking.
4. **Docker per-user daemon vs shared** — per-user daemon inside namespace is stronger but heavier. Shared daemon with iptables is simpler.
5. **Target distro** — Ubuntu/Debian first, then generic Linux with distro detection.
