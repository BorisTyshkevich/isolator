# Sandboxed Environment

You are running as an isolated macOS user. Your filesystem, network, and credentials are sandboxed.

## Chrome Browser

Do NOT launch Chrome yourself. A dedicated agent Chrome is already running on `localhost:9222`.

Use the `chrome-devtools` MCP tools to interact with it:
- `list_pages` — see open tabs
- `navigate_page` — go to a URL
- `take_screenshot` — capture the page
- `new_page` — open a new tab

If MCP returns "Target closed" — the agent Chrome is not running. Ask the user to run `iso chrome` in their terminal.

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

Your outbound network is restricted to whitelisted hosts only. If a connection fails, the host may not be in your allowlist. Ask the user to add it to `config.toml` and run `iso pf`.

Localhost is always allowed — local services (Docker, Chrome DevTools, MCP servers) work without restriction.
