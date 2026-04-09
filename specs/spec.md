# Isolator

Run AI coding agents inside per-user Unix isolation on macOS.
No VM, no Docker. Just sudo + pf + ACL.

## What it does

Two Python scripts (stdlib only, no packages):

1. **`create-user <name>`** — create macOS user, ACL, copy source user's shell/Claude/Codex config, inject auth
2. **`apply-pf`** — generate and load pf firewall rules from config (per-user + global hosts)

After that:
```bash
sudo -u slot-0 -i claude
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
hosts = [
    "api.openai.com",
]

[users.slot-1.auth]
OPENAI_API_KEY = "/etc/isolator/keys/openai"

[users.slot-2]
uid = 602
from = "slot-0"                # copy shell/claude config from slot-0 instead of admin
hosts = [
    "api.anthropic.com",
    "mcp.demo.altinity.cloud",
]

[users.slot-2.auth]
ANTHROPIC_API_KEY = "/etc/isolator/keys/anthropic"
```

Key files: `/etc/isolator/keys/` — root:wheel 600, one raw key per file.

---

## create-user

`create-user <name> [--from USER]` or `create-user --all`

### Config source resolution

1. `--from` CLI flag (highest priority)
2. `from` field in config (`[users.<name>]`)
3. `admin` user from config (default)

The source is just a home directory — works the same whether it's admin or another slot.

### Shell detection

Reads the source user's shell from `dscl . -read /Users/<source> UserShell`.
Sets the same shell for the slot user. Copies the matching rc files:
- **bash**: `.bash_profile`, `.bashrc`
- **zsh**: `.zshrc`, `.zprofile`

### Steps

1. **Create macOS user** — `dscl` (hidden, no password, staff group, same shell as source, home `/Users/<name>`)
2. **Home dir** — `mkdir`, `chown`, `chmod 700`
3. **ACL** — `chmod +a` granting admin `read,list,search,readattr,readextattr,readsecurity,file_inherit,directory_inherit`
4. **Copy source's shell config** — detected rc files for the shell. Append `source /etc/isolator/profile` to the login rc (`.bash_profile` or `.zprofile`).
5. **Copy source's Claude config:**
   - `~/.claude/settings.json` — copy and merge `"defaultMode": "bypassPermissions"`, `"skipDangerousModePermissionPrompt": true`
   - `~/.claude.json` — create new file with `mcpServers` + `oauthAccount` from source
   - `~/.claude/skills` — copy symlink if exists
   - `~/.claude/plugins/` — copy dir if exists
6. **Copy source's Codex config:**
   - `~/.codex/config.toml` — copy but drop all `[projects."..."]` trust entries
   - `~/.codex/auth.json` — copy as slot login/account state
   - `~/.codex/plugins/`, `~/.codex/skills/`, `~/.codex/agents/` — copy if present
   - `~/.codex/AGENTS.md` — copy if present
   - Do not copy histories, logs, sqlite DBs, caches, archived sessions, or tmp state
7. **Inject auth** — read key files from config `[users.<name>.auth]`, write `~/.env` (chmod 400).
   For Claude Code: run `claude setup-token` once as admin, store the long-lived OAuth token
   in `/etc/isolator/keys/claude-oauth`, reference as `CLAUDE_CODE_OAUTH_TOKEN` in config.
8. **Skeleton dirs** — `.local/bin`, `.local/lib`, `.npm-global`, `.cache`
9. **Normalize shared tool access** — if Homebrew `codex` is installed with user-private npm permissions, fix `/opt/homebrew/bin/codex` and its package tree to be world-readable/executable
10. **Set ownership** — static config files owned by root, Codex `auth.json` owned by the slot user so runtime token refresh can work

### What gets copied and why

| Source | Destination | Why |
|--------|------------|-----|
| `~source/.bash_profile` or `.zprofile` | `~slot/...` | Login shell config |
| `~source/.bashrc` or `.zshrc` | `~slot/...` | Interactive shell config |
| `~source/.claude/settings.json` | `~slot/.claude/settings.json` | Plugins, status line, preferences |
| `~source/.claude.json` → `mcpServers` | `~slot/.claude.json` | Global MCP servers |
| `~source/.claude/skills` | `~slot/.claude/skills` | Custom skills symlink |
| `~source/.claude/plugins/` | `~slot/.claude/plugins/` | Installed plugins |
| `~source/.codex/config.toml` | `~slot/.codex/config.toml` | Codex preferences and MCP/plugins without source-user project trust |
| `~source/.codex/auth.json` | `~slot/.codex/auth.json` | Codex login/account state |
| `~source/.codex/plugins/` | `~slot/.codex/plugins/` | Installed Codex plugins |
| `~source/.codex/skills/` | `~slot/.codex/skills/` | Installed Codex skills |
| `~source/.codex/agents/` | `~slot/.codex/agents/` | Agent presets |
| `/etc/isolator/keys/*` | `~slot/.env` | API keys per config |

Source = `--from` user, or config `from`, or admin (default).

### What does NOT get copied

- `~admin/.claude/projects/` — session state, per-project MCP (path-specific)
- `~admin/.claude/history.jsonl` — prompt history
- `~admin/.claude/debug/`, `file-history/`, `session-env/` — runtime caches
- `~admin/.codex/history.jsonl`, `session_index.jsonl`, `logs*`, `state_*.sqlite*` — Codex history/runtime state
- `~admin/.codex/cache/`, `tmp/`, `.tmp/`, `archived_sessions/`, `sessions/`, `shell_snapshots/` — Codex caches and transient state
- `~admin/.ssh/`, `~admin/.aws/`, `~admin/.kube/` — sensitive credentials
- Per-project MCP servers from `~admin/.claude.json` `projects.*` — these reference admin's paths; project-level MCP comes from `.mcp.json` in the workspace itself
- Project trust entries from `~admin/.codex/config.toml` `[projects."..."]` — these reference the source user's paths and must be dropped

---

## Isolator profile

`/etc/isolator/profile` — sourced at end of slot's `.bash_profile`:

```bash
# Auth keys
[[ -f "$HOME/.env" ]] && source "$HOME/.env"

# Override PATH — global tools read-only, local installs in home
export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"
export PATH="$HOME/.local/bin:$HOME/.npm-global/bin:$PATH"

# npm — local installs, no postinstall hooks
export NPM_CONFIG_PREFIX="$HOME/.npm-global"
export NODE_PATH="/usr/local/lib/node_modules"
export NPM_CONFIG_ignore_scripts=true

# pip — user installs only
export PIP_USER=1
export PYTHONUSERBASE="$HOME/.local"
```

---

## apply-pf

Reads config, resolves hostnames via `socket.getaddrinfo()`, generates pf anchor, loads it.

### Generated output (`/etc/pf.anchors/isolator`):

```
table <slot-0-allowed> { 104.18.32.1, 104.18.33.2, 151.101.1.67, ... }
table <slot-1-allowed> { 104.18.40.5, ... }
table <slot-2-allowed> { 104.18.32.1, 104.18.33.2, ... }

block out proto tcp user { 600 601 602 }
block out proto udp user { 600 601 602 }

pass out proto tcp user 600 to <slot-0-allowed> port 443
pass out proto tcp user 601 to <slot-1-allowed> port 443
pass out proto tcp user 602 to <slot-2-allowed> port 443

pass out proto udp user { 600 601 602 } to 127.0.0.1 port 53
```

Each user's table = their `hosts` + `[global].hosts`, all resolved to IPs.

Also:
- Adds anchor to `/etc/pf.conf` if missing
- Runs `pfctl -ef /etc/pf.conf`

---

## Usage

```bash
# One-time: store keys
echo "sk-ant-..." | sudo tee /etc/isolator/keys/anthropic
sudo chmod 600 /etc/isolator/keys/anthropic

# Create all users (copies from admin by default)
sudo create-user --all

# Create one user copying from admin
sudo create-user slot-0

# Create user copying from a customized slot
sudo create-user slot-2 --from slot-0

# Load firewall
sudo apply-pf

# Run claude
sudo -u slot-0 -i claude

# Run codex
sudo -u slot-1 -i codex

# Read slot's work
cat /Users/slot-0/workspace/main.py

# Refresh firewall after DNS changes
sudo apply-pf
```

---

## Security model

| Layer | Enforcement | Bypassable by slot? |
|-------|------------|---------------------|
| Filesystem | chmod 700 + root-set ACL | No |
| Network | pf by UID | No (kernel) |
| No sudo | No password, no sudoers entry | No |
| Admin home | chmod 700 (DAC) | No |
| npm hooks | NPM_CONFIG_ignore_scripts | Yes (cosmetic) |

---

## Open questions

1. **Home reset** — add `reset-user <name>` if needed between runs.
2. **Workspace sharing** — if agent needs admin's project dir, need ACL on that dir. Could be: `grant-workspace <user> <path>`.
3. **Codex auth portability** — copied `auth.json` is assumed to remain valid across slot users on the same machine.
