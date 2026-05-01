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
# 1. Clone and install
git clone https://github.com/BorisTyshkevich/isolator.git
cd isolator
sudo ./install.sh

# 2. Edit config — set your admin username and hosts
sudo vi /etc/isolator/config.toml

# 3. Enable Remote Login (for SSH-based isolation)
#    System Settings → General → Sharing → Remote Login → ON

# 4. One-time host setup: deploy /etc/isolator + sshd drop-in
sudo iso install

# 5. Create sandbox users
iso create acm
iso create click

# 6. Authenticate (first run only — stores in macOS keychain)
iso acm claude        # /login → authenticate → done
iso click claude      # same

# 7. Apply firewall rules
sudo iso pf
```

## The `iso` command

Single script for everything:

```bash
iso create <name> [options]       # Create an isolated user
iso create --all [options]        # Create all users from config
iso delete <name>                 # Delete user and home directory
iso delete --all                  # Delete all users from config
iso install                       # One-time setup: deploy /etc/isolator files + sshd drop-in
iso install --dry-run             # Preview file installs
iso pf                            # Apply firewall rules (run after host edits)
iso pf --dry-run                  # Print rules without loading
iso list                          # List configured users
iso copy <claude|codex> <user>    # Copy admin's claude/codex config to <user>
                                  # [--from <src-user>]  (default: admin)
iso <user> [command] [args...]    # Run command as isolated user (default: bash)
iso <user> remote [--bg]         # Start sandboxed remote session for Claude Desktop
iso <user> remote --status       # Show remote session status
iso <user> remote --stop         # Stop remote session
iso <user> acm [args...]         # Launch sandboxed acm-shell session as <user>
                                  # (Altinity Cloud Manager helper; needs acm-shell)
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

## Sandboxed acm-shell (Altinity)

If you have [acm-shell](https://github.com/Altinity/acm-shell)
installed at `/usr/local/acm-shell/`, `iso <user> acm [args...]` runs
the kube-context selection / cluster-pick flow on the admin side, then
exec's into the sandbox so kubectl/helm/k9s/clickhouse-client all run
under the sandbox user with the sandbox's network whitelist.

```bash
iso acm acm <deployment> <env_filter>   # sandbox user = acm
iso demo acm <deployment> <env_filter>  # sandbox user = demo (any iso user)
```

The legacy `iso-acm <args>` wrapper still works (defaults to user
`acm`). The internals are documented at the top of `bin/iso-acm` and
`bin/iso-acm-launcher`.

## Creating sandbox users

### Options

```bash
iso create acm                              # create user
iso create acm --from click                 # copy config from another user
iso create --all                            # create all from config
```

If the user doesn't exist in `config.toml`, it's auto-added with the next available UID.

`iso create` is idempotent — safe to re-run. It overwrites config files but preserves the user account, home contents, and auth.

### What it does

1. Creates a hidden macOS user via `dscl` (skipped if exists)
2. Sets up home with `chmod 700` and ACL for admin read/write access
3. Writes minimal shell rc files that source `/etc/isolator/profile`
   (no inheritance from admin's shell — see [Shell config convention](#shell-config-convention))
4. Installs `/etc/isolator/CLAUDE.md` as `~/.claude/CLAUDE.md` (locked
   sandbox policy doc — agent can't rewrite its own restrictions)
5. Creates per-user Docker network and workspace
6. Sets up SSH key for passwordless access
7. Sandbox shell rc files stay user-editable; CLAUDE.md is root-owned 444

`iso create` deliberately does **not** copy agent configs (Claude Code,
Codex). Use [`iso copy`](#copying-agent-config) if you want to seed a
new sandbox with your settings, MCP servers, or custom commands.

## Copying agent config

`iso copy` selectively transfers a small set of *configuration* files
from your admin user (or any other user via `--from`) into a sandbox
home. It does **not** copy auth or chat history — sandbox users
authenticate themselves on first run (`claude /login`,
`codex auth login`).

```bash
sudo iso copy claude acm                  # admin's claude config → acm
sudo iso copy codex  acm                  # admin's codex config  → acm
sudo iso copy claude acm --from click     # copy from another user
```

**`copy claude` writes:**

| Path | Notes |
|---|---|
| `~/.claude/settings.json` | Verbatim. Warns if `hooks` is set — admin paths likely won't resolve in the sandbox; review after copy. |
| `~/.claude/agents/`, `commands/`, `keybindings.json` | Verbatim snapshots. |
| `~/.claude.json` | Selective: just `mcpServers` + a pre-trust entry for the sandbox home. Admin's onboarding flags and `oauthAccount` are dropped. |
| `~/.claude/plugins/`, `skills/` | **Reset to empty** — admin paths in plugin configs would not resolve in the sandbox. Re-install in the sandbox. |

**`copy codex` writes:**

| Path | Notes |
|---|---|
| `~/.codex/config.toml` | Sanitized: `[projects.*]` trust entries dropped; `approval_policy = "never"` and `sandbox_mode = "danger-full-access"` forced. |
| `~/.codex/AGENTS.md`, `agents/` | Verbatim snapshots. |
| `~/.codex/plugins/`, `skills/` | **Reset to empty.** |

**Never copied** (sandbox user must authenticate themselves):

- Claude: tokens live in the per-user macOS Keychain.
- Codex: `~/.codex/auth.json`. (Admin's auth.json on disk was the
  inconsistency that made `iso create` stop copying these in the
  first place — every other secret goes through 1Password URIs.)

`iso copy` is destructive by design — it always overwrites. Re-run any
time you want to refresh from admin.

## Authentication

### Claude Code

Claude Code manages its own auth via macOS keychain. On first run:

```bash
iso acm claude      # → /login → authenticate in browser → done
```

Claude Code stores the token in the macOS keychain with an ACL that only allows Claude Code to read it. The agent **cannot** extract the raw token — it can only use it through Claude Code's authenticated session. Combined with network isolation (pf), the token can't be exfiltrated.

Re-authentication: if the token expires, just `/login` again.

### Other API keys (OPENAI_API_KEY, etc.)

Auth values must be 1Password URIs. Declare them in `config.toml`:

```toml
[users.click.auth]
OPENAI_API_KEY = "op://Personal/openai-api-key/credential"
```

The admin's `op` CLI resolves these at session start (one biometric
prompt batch per `op signin` window). Resolved values are delivered to
the sandbox process **in RAM** via `sudo --preserve-env` or
`ssh SendEnv` — never written to disk, never persisted past the
session. Rotating a credential is a vault edit; no `iso create` re-run
needed.

Setup, vault layout, SSH agent forwarding via `op ssh agent`, and the
threat model are in [`docs/secrets-via-1password.md`](docs/secrets-via-1password.md).

### Codex

```bash
iso acm codex auth login    # → browser flow → tokens saved to ~/.codex/auth.json
```

Each sandbox user authenticates themselves; admin's `auth.json` is
never copied. Use [`iso copy codex <user>`](#copying-agent-config) to
transfer admin's `config.toml` (sanitized: project trust entries
dropped, bypass mode forced), `AGENTS.md`, and custom agents.

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

Sandbox shells don't inherit admin's rc files — `iso create` writes a
minimal `.bash_profile`/`.zshrc` per user that just sources
`/etc/isolator/profile` (PATH, DOCKER_HOST, TMPDIR, 1Password
unwrappers). The rc files stay user-editable; sandbox users add their
own aliases / SDK paths / etc. on top.

The key rule for admin: **no secrets in your shell rc files**. Tokens
and API keys live in 1Password; declare them in `config.toml` under
`[users.<name>.auth]` as `op://...` URIs. They get unwrapped into the
sandbox process at session start, in RAM, never on disk. See
[`docs/secrets-via-1password.md`](docs/secrets-via-1password.md).

### What `iso create` writes

- Minimal `.bash_profile`/`.bashrc`/`.zprofile`/`.zshrc` (3 lines each)
- `~/.claude/CLAUDE.md` — sandbox policy doc, root-locked
- Workspace at `/Users/Workspaces/<user>/`
- SSH key + authorized_keys for `iso -s`-mode access
- Per-user Docker network + filtered socket

### What `iso create` does NOT touch

- `~/.claude/settings.json`, `~/.claude.json`, `~/.codex/config.toml` —
  use [`iso copy`](#copying-agent-config) to seed these from admin.
- `~/.env`, `~/.ssh/id_*`, `~/.aws/`, `~/.kube/`, `~/.netrc` — never
  copied. Per-sandbox auth via 1Password URIs in `config.toml`.
- Chat history (`~/.claude/projects/`, `~/.codex/sessions/`),
  caches, sqlite DBs, IDE state.

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

### Socket access (proxy-only, no shared socket)

OrbStack's socket is inside the admin's home (`~/.orbstack/run/docker.sock`) — sandboxed users can't traverse there. Each gets a per-user filtered socket at `/var/run/isolator-docker/<user>.sock`. There is **no `/var/run/docker.sock`** — sandbox users have no path to the unfiltered daemon.

The proxy is auto-started by `iso` on first use (no manual setup). Admin keeps using Docker via OrbStack context.

### Volume mount protection

**The problem:** Docker socket = root-level filesystem access. A sandboxed agent could `docker run -v /Users/admin/.ssh:/mnt` and read all your credentials.

**The fix:** per-user proxy filters every `containers/create` request:
- **Bind mounts**: only `/Users/Workspaces/<user>/` (paths rewritten via `realpath()` for TOCTOU safety)
- **Named volumes**: blocked entirely (including bind-backed local-driver volumes)
- **NetworkMode**: only `bridge` or `iso-<user>` (other networks rejected)
- **Rejected fields**: `Privileged`, `--net=host`, `--pid=host`, `--volumes-from`, `--device`, `CapAdd`, `SecurityOpt`, `IpcMode`, `UTSMode`, `UsernsMode`, `CgroupnsMode`, `CgroupParent`, `Runtime`, `Sysctls`, `Ulimits`, `OomScoreAdj`, `OomKillDisable`, `DeviceCgroupRules`, `DeviceRequests`
- **Rejected**: explicit `User=root`, `Transfer-Encoding` headers, duplicate `Content-Length` (HTTP smuggling)
- **Daemon endpoints**: only a narrow allowlist is exposed; proxy-side `docker build`, image push/import, remote `fromSrc` fetches, and volume creation are blocked

Sandboxed users must use `/opt/homebrew/bin/docker` (not the OrbStack shim at `/usr/local/bin/docker`, which bypasses `DOCKER_HOST`).

```bash
# Verify
iso acm docker run --rm -v /Users/Workspaces/acm:/app alpine echo OK    # allowed
iso acm docker run --rm -v /Users/admin/.ssh:/mnt alpine cat /mnt/id_rsa # blocked
iso acm docker volume create secret-bind                                  # blocked
iso acm docker build .                                                    # blocked through proxy
```

### Sandbox user names

`iso` now rejects unsafe sandbox names before any root-owned paths are touched. Valid names are simple local account names using only letters, digits, `_`, `.`, and `-`.

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

