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
sudo cp etc/config.toml /etc/isolator/config.toml
sudo cp etc/profile /etc/isolator/profile
sudo cp etc/CLAUDE.md /etc/isolator/CLAUDE.md
sudo chmod 644 /etc/isolator/config.toml /etc/isolator/profile /etc/isolator/CLAUDE.md

# 2. Edit config.toml — set your admin username

# 3. Enable Remote Login (for SSH-based isolation)
#    System Settings → General → Sharing → Remote Login → ON

# 4. Install iso to PATH
sudo cp bin/iso /usr/local/bin/iso

# 5. Create users (auto-added to config.toml if not present)
iso create acm --keychain
iso create click --keychain

# 6. Load firewall rules (optional)
iso pf

# 7. Run (uses SSH under the hood — proper macOS login session)
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
iso <user> [command] [args...]    # Run command as isolated user (default: bash)
iso <user> remote [--bg]         # Start sandboxed remote session for Claude Desktop
iso <user> remote --status       # Show remote session status
iso <user> remote --stop         # Stop remote session
```

When running `claude`, `iso` automatically injects `--permission-mode bypassPermissions`.
Codex bypass is configured via `config.toml` (`approval_policy = "never"`).

Any other command passes through unchanged: `iso acm bash`, `iso acm npm install`, etc.

## Claude Desktop + Sandboxed Agent

Run a sandboxed agent in remote mode and connect from Claude Desktop's full GUI:

```bash
iso acm remote                   # starts claude --remote, prints connection URL
```

Open Claude Desktop → Connect to remote → paste the URL. The agent runs as `acm` (sandboxed: filesystem isolation, network whitelist, read-only config) while you get Desktop's rich UI.

```bash
iso acm remote --bg              # start in background
iso acm remote --status          # check if running
iso acm remote --stop            # stop the session
```

**Why this matters:**
- Desktop's full UI (file preview, diffs, images) + OS-level sandboxing
- Persistent sessions — agent survives terminal disconnect
- Multiple sessions — run `iso acm remote --bg` and `iso click remote --bg` simultaneously
- Works across machines — agent on a Linux server, Desktop on your Mac

### Create options

```bash
iso create acm                              # create (no auth)
iso create acm --keychain                   # with keychain auth (encrypted)
iso create acm --token sk-ant-oat01-...     # with token auth (plaintext)
iso create acm --from click                 # copy config from another user
iso create --all --keychain                 # create all from config
```

If the user doesn't exist in `config.toml`, it's auto-added with the next available UID.

### Re-running create (refresh)

`iso create` is idempotent — safe to re-run on existing users. It overwrites config files but preserves the user account and home contents.

**Refresh config only** (shell rc, Claude/Codex settings, plugins):
```bash
iso create acm
```
Re-copies shell config, Claude settings, Codex config, and plugins from the source user. Useful after changing your `.bashrc`, Claude plugins, MCP servers, etc. Does not touch auth — existing keychain stays as-is.

**Refresh config + auth** (also re-copies credentials):
```bash
iso create acm --keychain
```
Same as above, plus re-copies your current OAuth credentials from keychain with a new secure password. Use after token rotation or when the agent gets 401 errors.

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

Copies Claude Code OAuth credentials from your macOS Keychain into a new encrypted keychain for the isolated user.

```bash
iso create acm --keychain
```

How it works:
1. Reads `Claude Code-credentials` from your keychain
2. Generates a secure random password (stored in `/etc/isolator/keychain/<user>`, root-only mode 400)
3. Creates a login keychain for the user with that password
4. Stores the credential there
5. On `iso acm claude`, the `iso` script reads the root-only password and unlocks the keychain before launching

The agent never sees the keychain password. Credentials are encrypted at rest. The `iso` script (via sudo) is the only thing that can unlock the keychain.

### Mode 2: OAuth token

```bash
# Generate token (run once as yourself)
claude setup-token
# Creates a 1-year token, prints: sk-ant-oat01-...

# Create user with the token
iso create acm --token sk-ant-oat01-...
```

Writes the token to `~/.claude/.credentials.json` (plaintext). Simpler but less secure — the agent can read the raw token from disk.

### Mode comparison

| | Keychain | Token |
|---|---|---|
| Credentials stored in | macOS Keychain (encrypted) | `.credentials.json` (plaintext) |
| Password stored in | `/etc/isolator/keychain/` (root 400) | N/A |
| Survives token refresh | Yes (Claude updates keychain) | No (need new token) |
| Agent can read raw token | Only while keychain is unlocked | Always |
| Encrypted at rest | Yes | No |
| Setup | One command | `claude setup-token` first |

### Config key files (for any env var)

For API keys that aren't Claude OAuth (e.g., `OPENAI_API_KEY`), define them in `config.toml`:

```toml
[users.click.auth]
OPENAI_API_KEY = "/etc/isolator/keys/openai"
```

Store the key in a root-only file:

```bash
echo "sk-..." | sudo tee /etc/isolator/keys/openai > /dev/null
sudo chmod 400 /etc/isolator/keys/openai
```

On `iso create`, each key is read and written to `~/.env` (root-owned, read-only 444). The profile sources `~/.env` on login. This works for any environment variable — `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `HF_TOKEN`, etc.

Config key files are **always** processed, regardless of `--keychain` or `--token`. So a user can have Claude auth via keychain AND an OpenAI key via config:

```bash
iso create click --keychain       # Claude via keychain + OPENAI_API_KEY from config
```

### Codex

Codex auth is handled by copying a curated subset of the source user's `~/.codex`:

- `config.toml` — with source-user `[projects."..."]` trust entries removed, bypass mode enabled
- `auth.json` — Codex login state
- `plugins/`, `skills/`, `agents/`, `AGENTS.md`

## SSH mode

`iso` uses SSH to localhost instead of `sudo -u` for switching to sandboxed users. This creates a proper macOS login session, which fixes several issues with the sudo approach:

| Problem with sudo | SSH fixes it |
|---|---|
| "Keychain Not Found" dialogs | Real login session |
| Safari opens instead of Chrome | Launch Services works |
| Workspace trust prompt every time | Session state persists |
| Chrome zombie processes | Clean process lifecycle |

A single Ed25519 keypair is generated on first `iso create`. The public key is installed to every sandboxed user's `~/.ssh/authorized_keys`.

**Prerequisites:**
- Remote Login enabled: System Settings → General → Sharing → Remote Login → ON
- `iso create` handles everything else (key generation, SSH ACL group, authorized_keys)

If `~/.ssh/isolator` doesn't exist, `iso` falls back to sudo mode automatically.

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
log = true                        # log blocked + allowed traffic

[users.click]
uid = 601
from = "acm"
hosts = ["api.openai.com"]

[users.tools]
uid = 602
hosts = ["*"]                     # unrestricted network access
```

- `hosts = ["*"]` — unrestricted network (no pf/iptables rules for this user)
- `log = true` — log all traffic via pf log + Docker iptables LOG

Users are auto-added to config when created via `iso create <name>`.

## Files

| Path | Purpose |
|------|---------|
| `bin/iso` | Unified command: create, delete, run, firewall |
| `etc/config.toml` | User definitions, allowed hosts, optional auth key paths |
| `etc/profile` | Shell profile sourced by all isolated users |
| `etc/com.isolator.docker-proxy.plist` | launchd plist for Docker socket proxy |
| `specs/spec.md` | Design spec (macOS) |
| `specs/spec-linux.md` | Design spec (Linux) |
| `slides/` | Presentation slides (Marp source + HTML + PDF) |

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

## Workspaces

Each sandbox user gets a workspace in `/Users/Workspaces/<user>/`, created by `iso create`:

```bash
/Users/Workspaces/
  acm/                  # iso acm claude starts here
  click/                # iso click codex starts here
  otel/                 # iso otel claude starts here
```

`iso acm claude`, `iso acm bash`, `iso acm` — all start in `/Users/Workspaces/acm/`. Admin has full read/write access via ACL.

Workspaces are **not deleted** by `iso delete` — your project code survives user recreation.

## Shared tools

Install tools as admin — all users can use them:

```bash
brew install node clickhouse-client kubectl
npm install -g @anthropic-ai/claude-code
```

Users read `/opt/homebrew` and `/usr/local` but can't write. Upgrade a tool once, all users get it. Users can also install packages locally via `npm install -g` (goes to `~/.npm-global/`) and `pip install` (goes to `~/.local/`).

## Docker (OrbStack)

### Socket access

OrbStack's socket lives inside the admin's home — sandboxed users can't reach it. A launchd daemon creates a hardlink at `/var/run/docker.sock`:

```bash
sudo cp etc/com.isolator.docker-proxy.plist /Library/LaunchDaemons/
sudo launchctl load /Library/LaunchDaemons/com.isolator.docker-proxy.plist
```

### Volume mount protection

**The problem:** Docker socket = root-level filesystem access. A sandboxed agent can `docker run -v /Users/admin/.ssh:/mnt` and read all your credentials.

**The fix:** per-user Docker socket proxy. Each sandboxed user gets a filtered socket that blocks dangerous mounts:

```
agent → /tmp/isolator-docker/acm.sock → proxy → Docker daemon
```

The proxy inspects every `containers/create` API call and blocks:
- Bind mounts outside `/Users/Workspaces/<user>/` and `/tmp/`
- `--privileged`, `--net=host`, `--pid=host`
- `--volumes-from`, `--device`
- Mounting the Docker socket into containers

The proxy runs as admin (no sudo), auto-started by `iso` on first use. Sandboxed users must use `/opt/homebrew/bin/docker` (not the OrbStack shim at `/usr/local/bin/docker`).

```bash
# Verify
iso acm docker run --rm -v /Users/Workspaces/acm:/app alpine echo OK    # allowed
iso acm docker run --rm -v /Users/admin/.ssh:/mnt alpine cat /mnt/id_rsa # blocked
```

## Chrome MCP (browser access from sandbox)

Sandboxed agents can control a dedicated Chrome instance via [Chrome DevTools MCP](https://github.com/ChromeDevTools/chrome-devtools-mcp). The agent gets a **clean browser** (empty profile, no cookies, no saved passwords) — your real Chrome is untouched.

**1. Start agent Chrome** (empty profile, debug port 9222):

```bash
iso chrome                       # start
iso chrome --stop                # stop when done
```

**2. Add Chrome DevTools MCP to your admin config:**

```bash
claude mcp add chrome-devtools --scope user -- npx chrome-devtools-mcp@latest --browserUrl http://127.0.0.1:9222
```

**Important:** `--browserUrl` is required — without it the MCP server tries to launch its own Chrome, which fails for sandboxed users. This is Google's official [chrome-devtools-mcp](https://github.com/ChromeDevTools/chrome-devtools-mcp).

**3. Copy to sandboxed users:**

```bash
iso create acm                   # re-copies MCP config from admin
```

**4. Use it:**

```bash
iso acm claude
# Agent can now navigate, screenshot, fill forms, run browser tests
```

**Security model:**
- Agent Chrome runs with an empty profile (`/tmp/chrome-agent`) — no real cookies or passwords
- Your main Chrome has no debug port — not accessible via CDP
- `iso pf` allows localhost TCP (local services are admin-controlled)
- Network isolation still blocks exfiltration to non-whitelisted hosts
- `/tmp/chrome-agent` is wiped on reboot

## Network Logging

Enable per-user logging in `config.toml`:

```toml
[users.acm]
uid = 600
hosts = ["api.anthropic.com"]
log = true
```

Then re-apply rules: `iso pf`

### Viewing logs

**macOS pf** (host process traffic):

```bash
# Live stream — blocked and allowed connections
sudo tcpdump -i pflog0 -n

# Filter by user's UID
sudo tcpdump -i pflog0 -n 2>&1 | grep "uid 600"

# Save to file for later analysis
sudo tcpdump -i pflog0 -n -w /var/log/isolator-pf.pcap
```

**Docker iptables** (container traffic, inside OrbStack VM):

```bash
# Recent drops
orb dmesg | grep iso-acm-drop

# Live tail
orb dmesg -w | grep iso-

# All logged traffic (drops + unrestricted user traffic)
orb dmesg | grep "iso-"
```

**pf logs** — macOS handles `pflog0` as a virtual interface all logs in-RAM rotated automatically.
**Docker iptables logs** — stored in the OrbStack VM's kernel ring buffer (`dmesg`), which auto-rotates, too. 


