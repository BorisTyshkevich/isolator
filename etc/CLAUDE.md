# Sandboxed Environment

You are running as an isolated macOS user. Your filesystem, network, and credentials are sandboxed.

## Workspaces

Your workspace is `/Users/Workspaces/<your-username>/`. You start here by default. Place project code here.

## Temporary files

Use `$TMPDIR` (set to `~/tmp/`) instead of `/tmp`. `/tmp` is shared with other sandbox users and **blocked** for Docker bind mounts. Tools that respect `$TMPDIR` (Python tempfile, Go, Node, etc.) work automatically. For Docker:
```bash
docker run -v "$TMPDIR/cache:/cache" ...   # OK
docker run -v /tmp/cache:/cache ...         # BLOCKED
```

## Chrome Browser — CRITICAL

**NEVER launch Chrome yourself.** Do not run `open -a "Google Chrome"`, do not run the Chrome binary, do not `pkill` Chrome. You will break the admin's browser and cause zombie processes.

A dedicated agent Chrome is already running on `localhost:9222` with an empty profile. Use the `chrome-devtools` MCP tools to interact with it:
- `list_pages` — see open tabs
- `new_page` — open a new tab with a URL
- `navigate_page` — go to a URL in the current tab
- `take_screenshot` — capture the page

If MCP returns "Target closed" or connection errors, tell the user:
> "Agent Chrome is not running. Please run `iso chrome` in your terminal."

Do NOT try to fix Chrome yourself.

## Docker

Use the `$DOCKER_NETWORK` environment variable for container networking:
```bash
docker run --network=$DOCKER_NETWORK ...
```

Or in docker-compose:
```yaml
networks:
  default:
    external: true
    name: ${DOCKER_NETWORK:-bridge}
```

## Network

Your outbound network is restricted to whitelisted hosts only. If a connection fails, the host may not be in your allowlist. Tell the user:
> "Connection to <host> failed. It may need to be added to config.toml. Run `iso pf` after adding it."

Localhost is always allowed — local services (Docker, Chrome DevTools, MCP servers) work without restriction.

## Secrets and credential output — CRITICAL

Every command output you produce is recorded: terminal scrollback, `~/.claude/projects/<id>.jsonl` on disk, and Anthropic's API logs (~30-day retention). A single `cat` of the wrong file or `echo` of the wrong env var leaks credentials irrevocably — rotating the secret remains the only fix.

**Don't print credential values, directly or indirectly:**

- **`cat`/`tail`/`head` of unfamiliar config files** — `~/.acm.env`, `~/.netrc`, `~/.aws/credentials`, `~/.acmctl.yaml`, `~/.clickhouse-client/config.xml`, `~/.ssh/*` all carry plaintext, including in commented-out lines.
- **`head -c N` of any `op`/`curl`/`gh` output** — even ~8–12 chars of a fresh token narrows brute-force.
- **`jq '.'`, `jq 'to_entries'`, `jq 'select(...)'` returning whole objects** — API JSON nests secrets in unexpected fields (TLS keys, AWS access keys, embedded passwords).
- **`<tool> -v` / `--verbose` for tools that fetch credentials** — verbose dumps HTTP bodies.
- **`bash -c "test \"$SECRET\""`-across-iso-boundary** — outer/inner shell quoting differences cause the inner shell to expand the value and emit it on filename-too-long etc. errors.

**Do instead:**

- Verify presence by **length only**: `${#VAR}`, `wc -c < <(cmd)`, `[ -n "$VAR" ] && echo set`.
- Verify JSON shape with `jq 'keys'`, `jq 'type'`, `jq '.field | length'` — never `jq '.'`.
- Use **config files or env vars** to feed secrets to subcommands, never `--password X` / `--token X` on the command line (visible in `ps eww`, in shell history, in error messages).
- Pipe `op read` directly to env or file; never to a pager.
- Filter captured stderr through `sed 's/<token-pattern>/<REDACTED>/g'` before printing back.

**If a secret leaks anyway**: tell the user immediately, name the credential, add it to the rotation list. Don't keep working as if nothing happened.

## Sandbox is not a leak shield

The sandbox restricts your filesystem and network — it does NOT protect what you print. Tool outputs cross the sandbox boundary back to the admin's terminal and to Anthropic's API. The "Secrets and credential output" rules above apply *especially* in this sandbox: the admin's home (`/Users/<admin>/`) is unreadable, but admin secrets surface in env vars (e.g., `ACM_API_KEY` resolved from 1Password). Treat them as if they were your own.
