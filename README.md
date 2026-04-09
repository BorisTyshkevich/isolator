# Isolator

Zero-overhead sandboxing for AI coding agents on macOS.

Run Claude Code, Codex, Gemini CLI (or any agent) with full autonomy — no permission prompts — inside isolated Unix users with locked-down filesystem and network access.

No VM. No Docker. Just macOS users + ACL + pf.

## How it works

Each agent session runs as a dedicated macOS user (`slot-0`, `slot-1`, etc.) that:

- **Can't read** your home directory, SSH keys, AWS credentials, or other slots
- **Can't access** the network except whitelisted hosts (per-user pf firewall)
- **Can't modify** its own config (root-owned, read-only)
- **Can read** shared tools in `/opt/homebrew` and `/usr/local` (install once, use everywhere)
- **Can install** packages locally in its home (`npm install -g`, `pip install`)

You keep full read access to every slot's home via root-set POSIX ACLs.

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

# 2. Edit config.toml — set your admin username and per-slot hosts

# 3. Create users with auth (choose one method)
sudo ./create-user slot-0 --keychain-pass isolator    # copies keychain credentials
sudo ./create-user slot-0 --token sk-ant-oat01-...    # injects OAuth token

# 4. Load firewall rules (optional)
sudo ./apply-pf

# 5. Run
sudo -u slot-0 -i claude
sudo -u slot-1 -i codex
```

## Authentication

Two auth modes, both passed at `create-user` time. No files in `/etc` needed for auth.

### Claude Code

Claude uses the auth flows below: keychain copy, injected OAuth token, or config file keys.

### Mode 1: Keychain (recommended)

Copies Claude Code OAuth credentials from your macOS Keychain into a new keychain for the slot user. The slot's profile auto-unlocks it on login.

```bash
sudo ./create-user slot-0 --keychain-pass isolator
```

How it works:
1. Reads `Claude Code-credentials` from your keychain
2. Creates a login keychain for slot-0 with the given password
3. Stores the credential there
4. On `sudo -u slot-0 -i`, the isolator profile runs `security unlock-keychain -p isolator` automatically

The keychain password is not a secret — security comes from filesystem isolation (other slots can't read the keychain file). The keychain protects against reading raw file bytes.

### Mode 2: OAuth token

Generates a long-lived token and injects it as an environment variable in the slot's `.env` file.

```bash
# Generate token (run once as yourself)
claude setup-token
# Creates a 1-year token, prints: sk-ant-oat01-...

# Create user with the token
sudo ./create-user slot-0 --token sk-ant-oat01-...
```

The token is written to `~slot-0/.env` as `CLAUDE_CODE_OAUTH_TOKEN` and sourced on login.

### Mode comparison

| | Keychain | Token |
|---|---|---|
| Credentials stored in | macOS Keychain (encrypted) | `~/.env` (plaintext) |
| Survives token refresh | Yes (Claude updates keychain) | No (need new token) |
| Agent can read secret | Via keychain API only | Can `cat ~/.env` |
| Setup | One command | `claude setup-token` first |

### Auth via environment variables

Both modes also work via env vars (useful for scripting):

```bash
CLAUDE_CODE_OAUTH_TOKEN=sk-ant-... sudo ./create-user slot-0
ISOLATOR_KEYCHAIN_PASS=isolator   sudo ./create-user slot-0
```

### Fallback: config file keys

If neither flag nor env var is set, `create-user` falls back to key files referenced in `config.toml`:

```toml
[users.slot-0.auth]
CLAUDE_CODE_OAUTH_TOKEN = "/etc/isolator/keys/claude-oauth"
```

### Codex

Codex does not use a separate `create-user` auth flag. Instead, `create-user` copies a curated subset of the source user's `~/.codex`:

- `config.toml` — with source-user `[projects."..."]` trust entries removed
- `auth.json` — copied as the slot's Codex login state
- `plugins/`, `skills/`, `agents/`
- `AGENTS.md` if present

It does not copy histories, logs, sqlite DBs, caches, archived sessions, or tmp state. This keeps the slot authenticated for `codex` without inheriting the source user's machine-specific trusted project paths.

## Files

| File | Purpose |
|------|---------|
| `create-user` | Create isolated macOS users with config copied from admin |
| `apply-pf` | Generate and load per-user pf firewall rules |
| `config.toml` | User definitions, allowed hosts, optional auth key paths |
| `profile` | Shell profile sourced by all slot users |
| `spec.md` | Design spec |

## Config

```toml
admin = "yourname"

[global]
hosts = [
    "registry.npmjs.org",
    "pypi.org",
    "files.pythonhosted.org",
]

[users.slot-0]
uid = 600
hosts = ["api.anthropic.com"]

[users.slot-1]
uid = 601
from = "slot-0"    # copy config from slot-0 instead of admin
hosts = ["api.openai.com"]
```

## create-user

```bash
sudo ./create-user slot-0                            # create (no auth)
sudo ./create-user slot-0 --keychain-pass isolator   # with keychain auth
sudo ./create-user slot-0 --token sk-ant-oat01-...   # with token auth
sudo ./create-user slot-0 --from slot-1              # copy config from another slot
sudo ./create-user --all --keychain-pass isolator    # create all from config
```

What it does:

1. Creates a hidden macOS user via `dscl`
2. Sets up home with `chmod 700` and ACL for admin read access
3. Detects source user's shell (bash/zsh) and copies the matching rc files
4. Copies Claude Code config and curated Codex config from the source user
5. Injects auth (keychain credentials or token to `.env`)
6. Normalizes shared-tool permissions for Homebrew `codex` so slot users can execute it
7. Makes config files root-owned and read-only (agent can't modify)
8. Locks admin home to `chmod 700` if it isn't already

The `--from` flag (or `from` in config) lets you use a customized slot as a template.

## apply-pf

```bash
sudo ./apply-pf              # resolve hosts, generate rules, load
sudo ./apply-pf --dry-run    # print generated rules without loading
```

Reads `config.toml`, resolves all hostnames to IPs via DNS, generates per-user pf tables, and loads them. Each slot can only reach its own allowed hosts + the global list, on port 443 only. DNS is restricted to `127.0.0.1:53`.

Re-run after DNS changes to refresh IP tables.

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

Isolator expects your shell config split into three files. macOS defaults to zsh since Catalina (2019); bash is also supported.

**zsh (macOS default):**

| File | Purpose | Copied to slots? |
|------|---------|:---:|
| `~/.zprofile` | PATH, LANG, EDITOR, SDK paths. Sources `.env` and `.zshrc`. | Yes |
| `~/.zshrc` | Aliases, completions, prompt, interactive tools. No secrets. | Yes |
| `~/.env` | All tokens, API keys, credentials. `chmod 600`. | **No** |

**bash:**

| File | Purpose | Copied to slots? |
|------|---------|:---:|
| `~/.bash_profile` | Same as `.zprofile`. | Yes |
| `~/.bashrc` | Same as `.zshrc`. | Yes |
| `~/.env` | Same. | **No** |

The key rule: **no secrets in your shell rc files**. Put all `*_KEY`, `*_TOKEN`, `*_SECRET`, `*_CREDENTIALS` exports into `~/.env`. This file is never copied to slot users. Slots get their own auth via keychain or isolator-injected `.env`.

`create-user` auto-detects the source user's shell and copies the matching files.

### What gets copied to slot users

From the source user (admin or `--from`):

- `.zprofile` / `.zshrc` (or `.bash_profile` / `.bashrc`) — with isolator profile appended
- `~/.claude/settings.json` — merged with `bypassPermissions` mode
- `~/.claude.json` — MCP servers, OAuth account, onboarding state
- `~/.claude/skills` symlink, `~/.claude/plugins/` directory
- `~/.codex/config.toml` — copied with source-user trusted project entries removed
- `~/.codex/auth.json` — Codex login/account state
- `~/.codex/plugins/`, `~/.codex/skills/`, `~/.codex/agents/`

What does NOT get copied: `~/.env` (secrets), `~/.ssh`, `~/.aws`, `~/.kube`, Claude session history/debug caches, Codex histories/logs/sqlite DBs/caches/tmp state, per-project MCP configs, and Codex trusted project entries from the source user.

## Shared tools

Install tools as admin — all slots can use them:

```bash
brew install node clickhouse-client kubectl
npm install -g @anthropic-ai/claude-code
```

Slots read `/opt/homebrew` and `/usr/local` but can't write. Upgrade a tool once, all slots get it. Slots can also install packages locally via `npm install -g` (goes to `~/.npm-global/`) and `pip install` (goes to `~/.local/`).

If `codex` was installed with user-private npm permissions under `/opt/homebrew/lib/node_modules/@openai/codex`, `create-user` fixes that tree to be world-readable/executable so isolated users can run the shared binary.

## Shell aliases

Add to your `.bashrc` / `.zshrc`:

```bash
# Run claude/codex in an isolated slot
alias iso0='sudo -u slot-0 -i claude'
alias iso1='sudo -u slot-1 -i codex'
alias iso2='sudo -u slot-2 -i claude'

# Run any command in a slot
iso() { sudo -u "slot-${1}" -i "${@:2}"; }
# Usage: iso 0 claude
#        iso 1 codex --full-auto
#        iso 2 bash
```
