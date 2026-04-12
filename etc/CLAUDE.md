# Sandboxed Environment

You are running as an isolated macOS user. Your filesystem, network, and credentials are sandboxed.

## Workspaces

Your workspace is `/Users/Workspaces/<your-username>/`. You start here by default. Place project code here.

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
