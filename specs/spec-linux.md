# Isolator — Linux Spec

Run AI coding agents inside per-user namespace isolation on Linux.
No Docker. Uses namespaces, ACL, and a proxy for network control.

## Architecture

```
admin (has root via sudo)
 |
 |-- isolator run slot-0 claude
 |-- isolator run slot-1 codex
 |
 +-- slot-0  uid=600  /home/slot-0/  chmod 700
 |   namespace: net + mount + pid
 |   network: localhost:3128 -> proxy -> allowlisted hosts only
 |
 +-- slot-1  uid=601  /home/slot-1/  chmod 700
     namespace: net + mount + pid
     network: localhost:3128 -> proxy -> allowlisted hosts only

proxy (tinyproxy, runs as root/dedicated user)
 |-- listens on veth gateway per namespace
 |-- allowlist from config per slot
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

[users.slot-0]
uid = 600
hosts = [
    "api.anthropic.com",
    "sentry.io",
]

[users.slot-0.auth]
ANTHROPIC_API_KEY = "/etc/isolator/keys/anthropic"

[users.slot-1]
uid = 601
from = "slot-0"
hosts = [
    "api.openai.com",
]

[users.slot-1.auth]
OPENAI_API_KEY = "/etc/isolator/keys/openai"
```

---

## Scripts

Three scripts (Python, stdlib only):

1. **`create-user`** — same as macOS but with `useradd` and `setfacl`
2. **`run-agent`** — launch agent in namespaced environment
3. **`proxy-config`** — generate per-slot proxy allowlists

No `apply-pf` equivalent. Network control is via namespaces + proxy.

---

## create-user

Same purpose as macOS version. Differences:

| macOS | Linux |
|-------|-------|
| `dscl . -create` | `useradd --system --shell /bin/bash` |
| `chmod +a "admin allow read,..."` | `setfacl -Rm u:admin:rX` + `setfacl -dRm u:admin:rX` |
| IsHidden 1 | `--system` flag (no login screen) |
| `/Users/slot-N` | `/home/slot-N` |

Steps:

1. `useradd` — create user (system, no password, home `/home/slot-N`)
2. `chmod 700` home
3. `setfacl` — grant admin recursive read + default ACL for new files
4. Copy shell config from source user (detect bash/zsh from `/etc/passwd`)
5. Copy Claude/Codex config (same logic as macOS)
6. Inject auth keys to `~/.env`
7. Create skeleton dirs
8. Root-own config files, chmod 444

### ACL syntax

```bash
# Admin can read everything in slot home
setfacl -Rm u:admin:rX /home/slot-0

# Default ACL — applies to all new files/dirs automatically
setfacl -dRm u:admin:rX /home/slot-0
```

---

## run-agent

This is the Linux-specific script. Replaces the simple `sudo -u slot-0 -i claude` from macOS.

```bash
isolator run slot-0 claude
isolator run slot-1 codex --full-auto
isolator run slot-0 claude --add-dir /home/admin/projects/foo
```

### What it does

1. **Start per-slot proxy** (if not already running)
2. **Create network namespace** with veth pair
3. **Create mount namespace** — bind-mount shared tools read-only, hide sensitive dirs
4. **Create PID namespace** — agent can't see other processes
5. **Set cgroup limits** — memory, CPU
6. **Drop into namespace as slot user** and exec the agent command

### Namespace setup

```bash
# Create network namespace
ip netns add slot-0-ns

# Create veth pair: host side + namespace side
ip link add veth-slot-0 type veth peer name eth0-slot-0
ip link set eth0-slot-0 netns slot-0-ns

# Assign addresses
ip addr add 10.200.0.1/30 dev veth-slot-0
ip netns exec slot-0-ns ip addr add 10.200.0.2/30 dev eth0-slot-0

# Bring up
ip link set veth-slot-0 up
ip netns exec slot-0-ns ip link set eth0-slot-0 up
ip netns exec slot-0-ns ip link set lo up

# Default route inside namespace -> host side (where proxy lives)
ip netns exec slot-0-ns ip route add default via 10.200.0.1

# DNS -> proxy host
echo "nameserver 10.200.0.1" > /etc/netns/slot-0-ns/resolv.conf
```

Each slot gets its own /30 subnet: `10.200.N.0/30`.

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

    # Run as slot user
    exec su - slot-0 -c "claude"
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
systemd-run --uid=slot-0 --scope \
    --property=MemoryMax=4G \
    --property=CPUQuota=200% \
    ...
```

Or manually via cgroups v2:

```bash
mkdir /sys/fs/cgroup/isolator-slot-0
echo "4294967296" > /sys/fs/cgroup/isolator-slot-0/memory.max
echo "200000 100000" > /sys/fs/cgroup/isolator-slot-0/cpu.max
echo $PID > /sys/fs/cgroup/isolator-slot-0/cgroup.procs
```

### Combined launch

The full `run-agent` combines all namespaces in one `unshare` call:

```bash
ip netns exec slot-0-ns \
    unshare --mount --pid --fork \
    bash -c '
        # Mount setup (read-only shared tools, hide sensitive dirs)
        ...
        # cgroup (or use systemd-run wrapper)
        ...
        # Drop privileges and exec
        exec su - slot-0 -c "claude --dangerously-skip-permissions"
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

One tinyproxy instance per slot, or one shared instance with per-source-IP rules.

Per-slot config (`/etc/isolator/proxy/slot-0.conf`):

```
User nobody
Group nogroup
Port 3128
Listen 10.200.0.1
MaxClients 20

# Allowlist — domain-based, no IP resolution needed
FilterDefaultDeny Yes
Filter "/etc/isolator/proxy/slot-0.filter"

# Connect method only (HTTPS tunneling)
ConnectPort 443
```

Filter file (`/etc/isolator/proxy/slot-0.filter`):

```
^api\.anthropic\.com$
^sentry\.io$
^registry\.npmjs\.org$
^pypi\.org$
^files\.pythonhosted\.org$
```

Generated from config by `proxy-config` script.

### How the agent uses it

The slot's profile sets:

```bash
export http_proxy="http://10.200.0.1:3128"
export https_proxy="http://10.200.0.1:3128"
export HTTP_PROXY="$http_proxy"
export HTTPS_PROXY="$https_proxy"
export no_proxy="localhost,127.0.0.1"
```

Most tools (curl, npm, pip, node fetch) respect these env vars. For tools that don't, the namespace has no route to anything except the proxy anyway — connections just fail.

---

## Comparison: macOS vs Linux

| Feature | macOS (isolator) | Linux (isolator) |
|---------|-----------------|-----------------|
| User creation | dscl | useradd |
| File ACL | chmod +a | setfacl |
| Network isolation | pf by UID | Network namespace |
| Network control | IP allowlist (pf table) | Domain allowlist (proxy) |
| DNS handling | Resolve + refresh cron | Proxy handles it |
| Mount isolation | None (just permissions) | Mount namespace (dirs invisible) |
| PID isolation | None (ps shows all) | PID namespace |
| Resource limits | None | cgroups (memory, CPU) |
| Shared tools | /opt/homebrew read-only | /usr/local bind-mount read-only |
| Config copy | Same | Same |
| Auth injection | Same | Same |

Linux version is strictly stronger in every isolation dimension.

---

## Security model

| Layer | Mechanism | Agent can bypass? |
|-------|-----------|:-:|
| Filesystem | chmod 700 + setfacl | No |
| Network | Namespace + proxy | No (no route exists) |
| Mount | Sensitive dirs hidden via tmpfs | No (invisible) |
| PID | PID namespace | No (can't see others) |
| Resources | cgroups v2 | No (kernel) |
| Privilege | No password, no sudoers | No |
| Config | Root-owned, chmod 444 | No |
| Shared tools | Read-only bind mount | No |

---

## Profile

`/etc/isolator/profile` — same as macOS plus proxy env:

```bash
[[ -f "$HOME/.env" ]] && source "$HOME/.env"

export PATH="/usr/local/bin:/usr/bin:/bin"
export PATH="$HOME/.local/bin:$HOME/.npm-global/bin:$PATH"

export NPM_CONFIG_PREFIX="$HOME/.npm-global"
export NODE_PATH="/usr/local/lib/node_modules"
export NPM_CONFIG_ignore_scripts=true

export PIP_USER=1
export PYTHONUSERBASE="$HOME/.local"

# Proxy — all traffic goes through allowlisting proxy
export http_proxy="http://10.200.0.1:3128"
export https_proxy="http://10.200.0.1:3128"
export HTTP_PROXY="$http_proxy"
export HTTPS_PROXY="$https_proxy"
export no_proxy="localhost,127.0.0.1"
```

---

## Usage

```bash
# One-time setup
sudo ./create-user --all
sudo apt install tinyproxy
sudo ./proxy-config --all

# Run agents
sudo ./run-agent slot-0 claude
sudo ./run-agent slot-1 codex --full-auto

# Read their work
cat /home/slot-0/workspace/main.py

# Update proxy allowlists after config change
sudo ./proxy-config --all
```

---

## Open questions

1. **systemd-run vs manual namespaces** — systemd-run can do most of this in one command but is less portable (needs systemd). Manual unshare works everywhere.

2. **Shared proxy vs per-slot proxy** — one tinyproxy per slot is simpler to reason about but uses more resources. A shared proxy with per-source-IP filter files is more efficient.

3. **Rootless option** — user namespaces allow unprivileged namespace creation. Could the entire setup work without root after initial user creation? Possibly, with slirp4netns for networking.

4. **Workspace sharing** — same as macOS. Bind-mount the workspace into the namespace with appropriate permissions, or setfacl on the directory.

5. **Target distro** — Ubuntu/Debian first? Or generic Linux with distro detection?
