# Isolator FAQ

Troubleshooting tips and workarounds for running sandboxed users on macOS.

## Docker bind mount blocked: `/tmp/...` not allowed

The Docker proxy blocks `/tmp` and `/private/tmp` because they're shared between sandbox users (proxy sockets live there too — cross-user attack vector).

**Fix:** use `$TMPDIR` instead. The profile sets `TMPDIR=$HOME/tmp`, which is per-user and allowed for Docker mounts:

```bash
# Before:
docker run -v /tmp/cache:/cache alpine ...   # BLOCKED

# After:
docker run -v "$TMPDIR/cache:/cache" alpine ...   # OK
```

Tools that respect `$TMPDIR` (Python `tempfile`, Go `os.TempDir()`, Node `os.tmpdir()`, mktemp, etc.) work automatically. Only hand-written `/tmp/...` paths need updating.

`~/tmp/` is not auto-cleaned. Check size with `du -sh ~/tmp/`, clean with `rm -rf ~/tmp/*` when needed.

## ClickHouse client: `Killed: 9` or `mkstemp: Permission denied`

ClickHouse ships as a compressed self-extracting binary. On first run it decompresses itself next to the binary file. Sandboxed users can't write to `/opt/homebrew/bin/`, so the decompression fails.

**Fix:** run it once as admin to trigger decompression:

```bash
clickhouse-client --version
```

After that, all sandboxed users can run it.

## ClickHouse client: `cannot get current directory`

ClickHouse fails if the current directory doesn't exist or isn't accessible. This happens when running via `sudo -u <user>` without `-i` (no login shell).

**Fix:** always use `iso <user>` which runs `sudo -u <user> -i` (login shell, sets HOME and cd).

## sudo doesn't work in iTerm (no Touch ID, password rejected)

iTerm's session restoration breaks the PAM security context. Touch ID prompts don't appear and password auth may fail.

**Fix:** install `pam_reattach`:

```bash
brew install pam-reattach
sudo sh -c 'echo "auth       optional       /opt/homebrew/lib/pam/pam_reattach.so
auth       sufficient     pam_tid.so" > /etc/pam.d/sudo_local'
```

## Default browser is Safari in sandboxed users

macOS Launch Services doesn't respect the plist for non-GUI users. The `open` command defaults to Safari regardless of settings.

**Fix:** Isolator sets `BROWSER=/etc/isolator/open-browser` in the profile, which wraps `open -a "Google Chrome"`. This works for tools that respect `$BROWSER` (including Claude Code).

## Chrome zombie process after sandboxed user runs Chrome

If a sandboxed user launches Chrome GUI (e.g., via an auth flow), the process can get stuck in uninterruptible wait (`UE` state) and block your main Chrome from starting.

**Fix:** reboot to clear the zombie. To prevent it, avoid launching Chrome GUI as sandboxed users — use `iso chrome` to start a dedicated agent Chrome instead.

## GoLand / IDE can't save files in sandboxed user's home

The ACL on the sandboxed user's home may be missing `read` and `write` permissions (only has directory-level `list`, `add_file`, etc.). This happens if the user was created before the ACL fix.

**Fix:** re-run `iso create <user>` to re-apply the correct ACL, or fix manually:

```bash
sudo chmod -a# 0 /Users/<user>
sudo chmod +a "bvt allow read,write,append,delete,list,search,readattr,writeattr,readextattr,writeextattr,readsecurity,file_inherit,directory_inherit" /Users/<user>
```

## Go module cache: `permission denied` on toolchain files

Go marks downloaded modules as read-only (`dr-xr-xr-x`). When Go needs to update `go.sum` inside a cached toolchain, it fails.

**Fix:** make the cached toolchain writable:

```bash
chmod -R u+w /Users/<user>/<project>/.tmp/gomodcache/golang.org/toolchain@*/
```

Or delete it and let Go re-download:

```bash
rm -rf /Users/<user>/<project>/.tmp/gomodcache/golang.org/toolchain@*/
```

## Homebrew binaries not accessible by sandboxed users

Some Homebrew packages install with user-private permissions. Sandboxed users get `Permission denied`.

**Fix:** `iso create` automatically fixes permissions for known tools (e.g., Codex). For others:

```bash
sudo chmod -R a+rX /opt/homebrew/lib/node_modules/<package>
```

## Docker: `Cannot connect to the Docker daemon`

OrbStack's socket is a symlink into admin's home (`~/.orbstack/run/docker.sock`). Sandboxed users can't traverse `~/`.

**Fix:** Isolator's launchd daemon replaces the symlink with a hardlink. Install it:

```bash
sudo cp etc/com.isolator.docker-proxy.plist /Library/LaunchDaemons/
sudo launchctl load /Library/LaunchDaemons/com.isolator.docker-proxy.plist
```

If the hardlink breaks after OrbStack restart, the launchd `WatchPaths` trigger re-creates it automatically.

## Docker: testcontainers/ryuk fails with `Cannot connect to daemon`

Testcontainers mounts `DOCKER_HOST` into the ryuk container, but ryuk expects `/var/run/docker.sock` by default.

**Fix:** Isolator's hardlink approach replaces the OrbStack symlink at `/var/run/docker.sock` directly — no `DOCKER_HOST` needed. Containers, testcontainers, and ryuk all use the default path.

## kubectl OIDC login opens Safari (or wrong browser)

`kubelogin` (kubectl oidc-login) opens a browser for auth. It respects `$BROWSER` which Isolator sets to `/etc/isolator/open-browser` (opens Chrome). If that doesn't work, add `--browser-command` to your kubeconfig:

```yaml
users:
- name: oidc-user
  user:
    exec:
      command: kubectl
      args:
        - oidc-login
        - get-token
        - --oidc-issuer-url=https://...
        - --browser-command=/etc/isolator/open-browser
```

Or use `--skip-open-browser` to get a URL and open it manually:

```bash
kubectl oidc-login get-token --skip-open-browser ...
```

Note: `kubelogin` listens on `127.0.0.1:8000` for the callback. This works because `iso pf` allows all localhost TCP.

Sources: [kubelogin usage](https://github.com/int128/kubelogin/blob/master/docs/usage.md), [browser command issue](https://github.com/int128/kubelogin/issues/942)

## Claude Code: `Please run /login` / 401 authentication error

The OAuth token is stale or wasn't copied. Possible causes:

1. **Keychain not unlocked** — `iso` unlocks the keychain before launch; if you're running `sudo -u <user> -i claude` directly, the keychain stays locked.
2. **Stale `.credentials.json`** — from a previous session with a now-revoked token.
3. **No auth configured** — forgot `--keychain` or `--token` on create.

**Fix:** re-create with fresh auth:

```bash
iso create acm --keychain
```

## Claude Code: permission prompts despite bypass mode

`defaultMode: bypassPermissions` in `settings.json` may not take effect. The CLI flag is more reliable.

**Fix:** Isolator handles this two ways:
- `iso acm claude` auto-injects `--permission-mode bypassPermissions`
- Profile alias: `alias claude='claude --permission-mode bypassPermissions'`

Both are set up automatically by `iso create`.

## Network: agent can reach hosts not in the whitelist

If you haven't run `iso pf`, there are no firewall rules. Network isolation is not automatic — you must apply it:

```bash
iso pf
```

Re-run after changing hosts in `config.toml` or after DNS changes (IPs may rotate).

## Docker containers bypass network isolation

Container traffic goes through OrbStack's Linux VM, not through macOS `pf`. This is expected.

**Fix:** `iso pf` generates both macOS pf rules AND Docker iptables rules. Make sure to run it. See `specs/docker-security.md` for the full threat model.
