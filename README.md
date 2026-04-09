# Isolator

Zero-overhead sandboxing for AI coding agents on macOS.

Run Claude Code, Codex, Gemini CLI (or any agent) with full autonomy — no permission prompts — inside isolated Unix users with locked-down filesystem and network access.

No VM. No Docker. Just macOS users + ACL + pf.

## How it works

Each agent session runs as a dedicated macOS user that:

- **Can't read** your home directory, SSH keys, AWS credentials, or other users
- **Can't access** the network except whitelisted hosts (per-user pf firewall)
- **Can't modify** its own config (root-owned, read-only)
- **Can read** shared tools in `/opt/homebrew` and `/usr/local` (install once, use everywhere)
- **Can install** packages locally in its home (`npm install -g`, `pip install`)

You keep full read/write access to every isolated user's home via root-set POSIX ACLs.

## Requirements

- macOS (tested on Sonoma/Sequoia)
- Python 3.11+ (ships with macOS or Homebrew)
- Root access (sudo with Touch ID)

No third-party Python packages needed — stdlib only.

## Quick start

```bash
# 1. Install config and profile
sudo mkdir -p /etc/isolator
sudo cp config.toml /etc/isolator/config.toml
sudo cp profile /etc/isolator/profile
sudo chmod 644 /etc/isolator/config.toml /etc/isolator/profile

# 2. Edit config.toml — set your admin username

# 3. Install iso to PATH
sudo cp iso /usr/local/bin/iso

# 4. Create users (auto-added to config.toml if not present)
iso create acm --keychain-pass ttt
iso create click --keychain-pass ttt

# 5. Load firewall rules (optional)
iso pf

# 6. Run
iso acm claude
iso click codex
```

## The `iso` command

Single script for everything:

```bash
iso create <name> [options]       # Create an isolated user
iso create --all [options]        # Create all users from config
iso delete <name>                 # Delete user and home directory
iso delete --all                  # Delete all users from config
iso pf                            # Apply firewall rules
iso pf --dry-run                  # Print rules without loading
iso list                          # List configured users
iso <user> <command> [args...]    # Run command as isolated user
```

When running `claude` or `codex`, `iso` automatically injects bypass flags:

- `iso acm claude` runs `claude --permission-mode bypassPermissions`
- `iso acm codex` runs `codex --dangerously-bypass-approvals-and-sandbox`

Any other command passes through unchanged: `iso acm bash`, `iso acm npm install`, etc.

### Create options

```bash
iso create acm                              # create (no auth)
iso create acm --keychain-pass ttt          # with keychain auth
iso create acm --token sk-ant-oat01-...     # with token auth
iso create acm --from click                 # copy config from another user
iso create --all --keychain-pass ttt        # create all from config
```

If the user doesn't exist in `config.toml`, it's auto-added with the next available UID.

### Re-running create (refresh)

`iso create` is idempotent — safe to re-run on existing users. It overwrites config files but preserves the user account and home contents.

**Refresh config only** (shell rc, Claude/Codex settings, plugins):
```bash
iso create acm
```
Re-copies shell config, Claude settings, Codex config, and plugins from the source user. Useful after changing your `.bashrc`, Claude plugins, MCP servers, etc. Does not touch auth — existing keychain and `.env` stay as-is.

**Refresh config + auth** (also re-copies credentials):
```bash
iso create acm --keychain-pass ttt
```
Same as above, plus re-copies your current OAuth credentials from keychain. Use after token rotation or when the agent gets 401 errors.

### What `create` does

1. Creates a hidden macOS user via `dscl` (skipped if exists)
2. Sets up home with `chmod 700` and ACL for admin read/write access
3. Detects source user's shell (bash/zsh) and copies the matching rc files
4. Copies Claude Code config and curated Codex config from the source user
5. Injects auth — only if `--keychain-pass` or `--token` is provided
6. Normalizes shared-tool permissions for Homebrew `codex`
7. Makes config files root-owned and read-only (agent can't modify)

## Authentication

Two auth modes, both passed at create time.

### Mode 1: Keychain (recommended)

Copies Claude Code OAuth credentials from your macOS Keychain into a new keychain for the isolated user. The profile auto-unlocks it on login.

```bash
iso create acm --keychain-pass ttt
```

How it works:
1. Reads `Claude Code-credentials` from your keychain
2. Creates a login keychain for the user with the given password
3. Stores the credential there
4. Writes `.credentials.json` so Claude Code uses the fresh token
5. On login, the isolator profile runs `security unlock-keychain` automatically

### Mode 2: OAuth token

```bash
# Generate token (run once as yourself)
claude setup-token
# Creates a 1-year token, prints: sk-ant-oat01-...

# Create user with the token
iso create acm --token sk-ant-oat01-...
```

### Mode comparison

| | Keychain | Token |
|---|---|---|
| Credentials stored in | macOS Keychain (encrypted) | `~/.env` (plaintext) |
| Survives token refresh | Yes (Claude updates keychain) | No (need new token) |
| Agent can read secret | Via keychain API only | Can `cat ~/.env` |
| Setup | One command | `claude setup-token` first |

### Auth via environment variables

```bash
CLAUDE_CODE_OAUTH_TOKEN=sk-ant-... iso create acm
ISOLATOR_KEYCHAIN_PASS=ttt         iso create acm
```

### Codex

Codex auth is handled by copying a curated subset of the source user's `~/.codex`:

- `config.toml` — with source-user `[projects."..."]` trust entries removed
- `auth.json` — Codex login state
- `plugins/`, `skills/`, `agents/`, `AGENTS.md`

## Config

```toml
admin = "yourname"

[global]
hosts = [
    "registry.npmjs.org",
    "pypi.org",
    "files.pythonhosted.org",
]

[users.acm]
uid = 600
hosts = ["api.anthropic.com", "sentry.io"]

[users.click]
uid = 601
from = "acm"    # copy config from acm instead of admin
hosts = ["api.openai.com"]
```

Users are auto-added to config when created via `iso create <name>`.

## Files

| File | Purpose |
|------|---------|
| `iso` | Unified command: create, delete, run, firewall |
| `config.toml` | User definitions, allowed hosts, optional auth key paths |
| `profile` | Shell profile sourced by all isolated users |
| `spec.md` | Design spec |

## Security model

| Layer | Mechanism | Bypassable by agent? |
|-------|-----------|:--------------------:|
| Filesystem | chmod 700 + root-set ACL | No |
| Network | pf firewall by UID | No (kernel) |
| Privilege escalation | No password, no sudoers entry | No |
| Config immutability | Root-owned, chmod 444 | No |
| Admin home | chmod 700 | No |
| Shared tools | World-readable, not writable | No |
| Auth (keychain mode) | macOS Keychain, encrypted | No |

## Shell config convention

Isolator expects your shell config split into three files:

| File | Purpose | Copied? |
|------|---------|:---:|
| `~/.zprofile` / `~/.bash_profile` | PATH, LANG, EDITOR, SDK paths | Yes |
| `~/.zshrc` / `~/.bashrc` | Aliases, completions, prompt | Yes |
| `~/.env` | All tokens, API keys, credentials | **No** |

The key rule: **no secrets in your shell rc files**. Put all `*_KEY`, `*_TOKEN`, `*_SECRET` exports into `~/.env`. This file is never copied. Isolated users get their own auth via keychain or injected `.env`.

### What gets copied

From the source user (admin or `--from`):

- Shell rc files — with isolator profile appended
- `~/.claude/settings.json` — merged with `bypassPermissions` mode
- `~/.claude.json` — MCP servers, OAuth account, onboarding state
- `~/.claude/skills` symlink, `~/.claude/plugins/` directory
- `~/.codex/config.toml` — with trusted project entries removed
- `~/.codex/auth.json`, `plugins/`, `skills/`, `agents/`

What does NOT get copied: `~/.env`, `~/.ssh`, `~/.aws`, `~/.kube`, session histories, debug caches, sqlite DBs, per-project configs.

## Shared tools

Install tools as admin — all users can use them:

```bash
brew install node clickhouse-client kubectl
npm install -g @anthropic-ai/claude-code
```

Users read `/opt/homebrew` and `/usr/local` but can't write. Upgrade a tool once, all users get it. Users can also install packages locally via `npm install -g` (goes to `~/.npm-global/`) and `pip install` (goes to `~/.local/`).

## Docker (OrbStack)

OrbStack's docker socket lives inside the admin's home, which isolated users can't access. Isolator includes a socat proxy that exposes a shared socket:

```bash
# Install (one-time)
brew install socat
sudo cp com.isolator.docker-proxy.plist /Library/LaunchDaemons/
sudo launchctl load /Library/LaunchDaemons/com.isolator.docker-proxy.plist
```

This creates `/var/run/docker-shared.sock` (group `staff`, mode `770`) that proxies to the real docker socket. The isolator profile auto-sets `DOCKER_HOST` when the shared socket exists.

```bash
# Verify
iso click docker ps
```

The proxy starts automatically on boot and restarts when OrbStack recreates the socket (via `PathState` watch on `/var/run/docker.sock`).
