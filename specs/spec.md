# Isolator — macOS Spec

Run AI coding agents inside per-user Unix isolation on macOS.
No VM, no Docker. Just `iso` + pf + ACL.

## What it does

One Python script (`iso`), stdlib only:

```bash
iso create acm --keychain         # create user, copy config, inject auth
iso pf                            # generate pf + Docker iptables rules
iso acm claude                    # run agent (bypass permissions auto-injected)
iso acm                           # open shell as user
```

---

## Config

`/etc/isolator/config.toml`:

```toml
admin = "bvt"

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

[users.tools]
uid = 602
from = "acm"
hosts = [
    "api.anthropic.com",
]
```

Users are auto-added to config if not present (`iso create newuser` assigns next available UID).

---

## The `iso` command

```bash
iso create <name> [options]       # Create isolated user
iso create --all [options]        # Create all users from config
iso delete <name>                 # Delete user and home
iso delete --all                  # Delete all users
iso pf                            # Apply pf + Docker iptables rules
iso pf --dry-run                  # Print rules without loading
iso list                          # List configured users
iso <user> [command] [args...]    # Run command as user (default: bash)
```

### Create options

```bash
--keychain              # Copy admin's keychain credentials (encrypted, recommended)
--token TOKEN           # Write OAuth token to .credentials.json (plaintext)
--from USER             # Copy config from USER instead of admin
```

### Bypass permissions

- `iso acm claude` → auto-injects `--permission-mode bypassPermissions`
- Codex bypass set via `config.toml` overrides (`approval_policy = "never"`, `sandbox_mode = "danger-full-access"`)
- Profile alias as fallback for interactive shells: `alias claude='claude --permission-mode bypassPermissions'`

---

## iso create

### Config source resolution

1. `--from` CLI flag (highest priority)
2. `from` field in config
3. `admin` user from config (default)

### Steps

1. **Create macOS user** — `dscl` (hidden, no password, staff group, same shell as source)
2. **Home dir** — `mkdir`, `chown`, `chmod 700`
3. **ACL** — `chmod +a` granting admin full read/write, file+directory inherit
4. **Copy shell config** — detected rc files for bash/zsh. Append `source /etc/isolator/profile` to all rc files (login + interactive)
5. **Copy Claude config:**
   - `~/.claude/settings.json` — merge `defaultMode: bypassPermissions`, `skipDangerousModePermissionPrompt: true`
   - `~/.claude.json` — `mcpServers`, `oauthAccount`, onboarding state
   - `~/.claude/skills` symlink, `~/.claude/plugins/` directory
6. **Copy Codex config:**
   - `~/.codex/config.toml` — drop `[projects."..."]` trust entries, override `approval_policy = "never"`, `sandbox_mode = "danger-full-access"`
   - `~/.codex/auth.json`, `plugins/`, `skills/`, `agents/`, `AGENTS.md`
7. **Inject auth** (only if `--keychain` or `--token` provided):
   - Keychain: generate secure random password → `/etc/isolator/keychain/<user>` (root 400), create user keychain, copy credentials
   - Token: write `.credentials.json`
8. **Create Docker network** — `iso-<name>` with dedicated subnet
9. **Skeleton dirs** — `.local/bin`, `.local/lib`, `.npm-global`, `.cache`, `workspace`
10. **Normalize shared tools** — fix Homebrew codex permissions
11. **Set ownership** — config files root-owned 444, runtime dirs user-owned
12. **Exclude from Time Machine** — `tmutil addexclusion` for home and keychain dir

### What gets copied

| Source | Destination | Why |
|--------|------------|-----|
| Shell rc files | `~user/` | Login + interactive shell config |
| `~source/.claude/settings.json` | Merged with overrides | Plugins, preferences + bypass mode |
| `~source/.claude.json` → select keys | `~user/.claude.json` | MCP servers, OAuth account |
| `~source/.claude/skills`, `plugins/` | `~user/.claude/` | Custom skills and plugins |
| `~source/.codex/config.toml` | Sanitized + bypass overrides | Codex preferences without project trust |
| `~source/.codex/auth.json`, `plugins/`, etc. | `~user/.codex/` | Codex auth and extensions |

### What does NOT get copied

`~/.env`, `~/.ssh`, `~/.aws`, `~/.kube`, session histories, debug caches, sqlite DBs, per-project configs, Codex trusted project entries.

---

## Authentication

Two modes, mutually exclusive:

### Keychain (recommended)

```bash
iso create acm --keychain
```

1. Reads `Claude Code-credentials` from admin's macOS Keychain
2. Generates secure random password → `/etc/isolator/keychain/acm` (root:wheel 400)
3. Creates login keychain for user, stores credential
4. `iso acm claude` reads root-only password file, unlocks keychain before launching

Agent never sees the keychain password. Encrypted at rest.

### Token

```bash
iso create acm --token sk-ant-oat01-...
```

Writes token to `~/.claude/.credentials.json` (plaintext). Simpler, less secure.

### Re-running create (refresh)

- `iso create acm` — refreshes config only (shell, Claude, Codex settings)
- `iso create acm --keychain` — refreshes config + re-copies credentials

---

## Isolator profile

`/etc/isolator/profile` — sourced from all slot rc files:

```bash
# Keychain unlocked by iso script (password in /etc/isolator/keychain/)

# PATH — prepend local installs, ensure homebrew
export PATH="$HOME/.local/bin:$HOME/.npm-global/bin:$PATH"
[[ ":$PATH:" != *":/opt/homebrew/bin:"* ]] && export PATH="/opt/homebrew/bin:$PATH"

# npm — local installs, no postinstall hooks
export NPM_CONFIG_PREFIX="$HOME/.npm-global"
export NODE_PATH="/usr/local/lib/node_modules"
export NPM_CONFIG_ignore_scripts=true

# pip — user installs only
export PIP_USER=1
export PYTHONUSERBASE="$HOME/.local"

# Docker — per-user isolated network
export DOCKER_NETWORK="iso-$(whoami)"

# Bypass permissions — fallback for interactive shells
alias claude='claude --permission-mode bypassPermissions'
```

---

## Network isolation

### Layer 1: macOS pf (host processes)

`iso pf` resolves hostnames from config, generates per-user pf tables:

```
table <acm-allowed> { 104.18.32.1, 104.18.33.2, ... }

block out proto tcp user { 600 601 602 }
block out proto udp user { 600 601 602 }

pass out proto tcp user 600 to <acm-allowed> port 443
pass out proto udp user { 600 601 602 } to 127.0.0.1 port 53
```

### Layer 2: Docker iptables (container traffic)

Containers run inside OrbStack's Linux VM — macOS pf doesn't see them. `iso pf` also generates iptables rules in the `DOCKER-USER` chain:

```
ACCEPT  172.30.0.0/24 → 172.30.0.0/24       (container-to-container)
ACCEPT  172.30.0.0/24 → 0.0.0.0/0 :53       (DNS)
ACCEPT  172.30.0.0/24 → <allowed-ips> :443   (whitelisted hosts)
DROP    172.30.0.0/24 → 0.0.0.0/0            (everything else)
```

Rules applied via `nsenter` into Docker's network namespace using `nicolaka/netshoot`.

### Docker socket access

OrbStack's socket (`~/.orbstack/run/docker.sock`) is inside admin's home (chmod 700). A launchd daemon replaces the `/var/run/docker.sock` symlink with a hardlink — standard path, zero overhead, no proxy.

---

## Security model

| Layer | Mechanism | Bypassable? |
|-------|-----------|:-:|
| Filesystem | chmod 700 + root-set ACL | No |
| Host network | pf by UID | No (kernel) |
| Container network | Docker iptables by subnet | No (kernel) |
| Privilege escalation | No password, no sudoers | No |
| Config immutability | Root-owned, chmod 444 | No |
| Admin home | chmod 700 (DAC) | No |
| Shared tools | World-readable, not writable | No |
| Auth (keychain) | macOS Keychain, root-only password | No |
| Backup exclusion | tmutil addexclusion | N/A |

---

## Docker

Per-user Docker networks with dedicated subnets (172.30.N.0/24). Created by `iso create`, firewalled by `iso pf`.

`docker pull` works (daemon operation). Container egress restricted to same-subnet + DNS + whitelisted hosts.

Docker socket: hardlink at `/var/run/docker.sock`, re-created by launchd when OrbStack restarts.

See `specs/docker-security.md` for full threat model.

---

## Backups

`iso create` auto-excludes user homes and `/etc/isolator/keychain/` from Time Machine. Agent work is ephemeral — push to git.

---

## Open questions

1. **Workspace sharing** — if agent needs admin's project dir, `chown` or ACL on that dir.
2. **Docker escape** — agent could create unrestricted Docker network or use `--net=host`. Future: Docker socket proxy to enforce policies.
3. **Codex auth portability** — copied `auth.json` assumed valid across users on same machine.
