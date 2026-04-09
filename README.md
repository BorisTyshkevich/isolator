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
sudo mkdir -p /etc/isolator/keys
sudo cp config.toml /etc/isolator/config.toml
sudo cp profile /etc/isolator/profile
sudo chmod 644 /etc/isolator/config.toml /etc/isolator/profile

# 2. Store your API keys
echo "sk-ant-..." | sudo tee /etc/isolator/keys/anthropic > /dev/null
echo "sk-..."     | sudo tee /etc/isolator/keys/openai > /dev/null
sudo chmod 600 /etc/isolator/keys/*

# 3. Edit config.toml — set your admin username and per-slot hosts/auth

# 4. Create all users
sudo ./create-user --all

# 5. Load firewall rules
sudo ./apply-pf

# 6. Run an agent
sudo -u slot-0 -i claude
sudo -u slot-1 -i codex
```

## Files

| File | Purpose |
|------|---------|
| `create-user` | Create isolated macOS users with config copied from admin |
| `apply-pf` | Generate and load per-user pf firewall rules |
| `config.toml` | User definitions, allowed hosts, auth key paths |
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

[users.slot-0.auth]
ANTHROPIC_API_KEY = "/etc/isolator/keys/anthropic"

[users.slot-1]
uid = 601
from = "slot-0"    # copy config from slot-0 instead of admin
hosts = ["api.openai.com"]

[users.slot-1.auth]
OPENAI_API_KEY = "/etc/isolator/keys/openai"
```

## create-user

```bash
sudo ./create-user slot-0              # create one user (config from admin)
sudo ./create-user slot-0 --from bvt   # create one user (config from specific user)
sudo ./create-user --all               # create all users from config
```

What it does:

1. Creates a hidden macOS user via `dscl`
2. Sets up home with `chmod 700` and ACL for admin read access
3. Detects admin's shell (bash/zsh) and copies the matching rc files
4. Copies Claude Code config (settings.json, MCP servers, skills, plugins)
5. Injects API keys from `/etc/isolator/keys/` into `~/.env`
6. Makes all config files root-owned and read-only (agent can't modify)
7. Locks admin home to `chmod 700` if it isn't already

The `--from` flag (or `from` in config) lets you use a customized slot as a template instead of the admin user.

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

## What gets copied to slot users

From the source user (admin or `--from`):

- Shell rc files (`.bash_profile`/`.bashrc` or `.zprofile`/`.zshrc`)
- `~/.claude/settings.json` (merged with `bypassPermissions` mode)
- `~/.claude.json` MCP servers (global only, not per-project)
- `~/.claude/skills` symlink
- `~/.claude/plugins/` directory

What does NOT get copied: `~/.ssh`, `~/.aws`, `~/.kube`, session history, debug caches, per-project MCP configs.

## Shared tools

Install tools as admin — all slots can use them:

```bash
brew install node clickhouse-client kubectl
npm install -g @anthropic-ai/claude-code
```

Slots read `/opt/homebrew` and `/usr/local` but can't write. Upgrade a tool once, all slots get it. Slots can also install packages locally via `npm install -g` (goes to `~/.npm-global/`) and `pip install` (goes to `~/.local/`).
