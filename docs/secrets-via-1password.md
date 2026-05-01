# Secrets via 1Password

isolator resolves sandbox-user secrets (OAuth tokens, API keys, anything
declared in `[users.<name>.auth]`) through 1Password's `op` CLI at
session start, and delivers them to the sandbox process **in RAM**
through `sudo --preserve-env` or `ssh SendEnv`. **Nothing touches disk.**

This document covers:
1. Why this design.
2. How to set it up on a fresh host.
3. The vault layout we expect.
4. SSH agent forwarding (1Password's `op ssh agent`).
5. Lifecycle, rotation, and audit.
6. Troubleshooting.
7. Threat model and known limitations.

## Why

Previously each sandbox user had a long-lived plaintext `~/.env` file
(mode 0400) with every credential they'd ever need. That:

- Persisted on disk indefinitely between sessions.
- Was readable by every process the user ran (including any malware).
- Had no rotation story — admin had to hand-edit files.
- Left forensic artifacts after a session ended.
- Was visible to direct shell logins (`su altinity` would inherit
  everything via the profile-sourced `.env`).

The 1Password design eliminates all five.

## Setup on a fresh host

### 1. Install the 1Password CLI

```bash
brew install --cask 1password-cli
```

After install, `op --version` should print a version string.

### 2. Sign in (interactive, biometric)

```bash
eval $(op signin)
```

This opens 1Password's biometric prompt, then exports `OP_SESSION_*`
env vars to your shell. The session lasts ~30 minutes by default. Any
shell you start `iso` from must have these env vars; the simplest way
is to add `eval $(op signin)` to your `.zprofile` or run it manually
when you start your day.

### 3. Configure auth references in `/etc/isolator/config.toml`

For each sandbox user, the `[users.<name>.auth]` table maps env-var
name → 1Password reference URI:

```toml
[users.altinity.auth]
CLAUDE_CODE_OAUTH_TOKEN = "op://Personal/claude-code/credential"
GITHUB_TOKEN            = "op://Personal/github-token/credential"
ANTHROPIC_API_KEY       = "op://Engineering/Anthropic API/credential"
```

The URI format is `op://<vault>/<item>/<field>`. You can copy a
reference from the 1Password GUI: right-click an item → "Copy Secret
Reference".

#### Optional entries

Append `?optional` to the URI when the 1P item may legitimately not
exist yet — `iso` will then silently skip the var instead of failing
the whole session start:

```toml
GITHUB_TOKEN = "op://Employee/gh-altinity/password?optional"
```

Other failure modes (op not installed, session expired, network
error) remain fatal even with `?optional`. Opt-in only — a typo in
a non-optional URI still aborts the session, which is what you want.

`iso pf` is unaffected by these changes — it doesn't need secrets.

### 4. Install the sshd drop-in (only if you use `iso -s`)

If you use the SSH path (`iso -s <user> ...`), copy the `AcceptEnv`
drop-in into sshd's config dir so the server accepts the
`ISO_TOKEN_*` env vars sent by the client:

```bash
sudo cp /Users/admin/work/isolator/etc/sshd-isolator.conf \
        /etc/ssh/sshd_config.d/isolator.conf
sudo launchctl kickstart -k system/com.openssh.sshd
```

Without it, the SSH path silently drops all forwarded secret env vars
and the sandbox session has none of them set.

The sudo path (default `iso <user> ...`) doesn't need this.

## Vault layout

We use a single admin-owned vault, not per-sandbox-user vaults.
Reasons:

- Admin runs `iso`; admin authenticates to 1Password; admin is the
  only one who needs vault access.
- 1Password's permission model is per-vault, not per-item. Per-user
  vaults would multiply vault count without adding meaningful
  isolation, since the admin can read all of them anyway.
- Audit logs already record which item was accessed, when, by whom.
  That's enough granularity.

Recommended layout: one item per credential, named after the use case
(`claude-code`, `openai-api-key`, `github-token`, etc.). Field name is
typically `credential` for tokens, `password` for passwords, or a
custom field for structured values.

## SSH agent forwarding

isolator does **not** copy private SSH keys to sandbox users. Outbound
SSH operations from a sandbox (e.g., `git push`, `ssh foo@bar`) are
expected to use 1Password's SSH agent via forwarding.

### One-time setup on admin's machine

Enable 1Password's SSH agent in the 1Password app:
**Settings → Developer → Use the SSH agent**.

Confirm in your shell:

```bash
echo $SSH_AUTH_SOCK
# Should print something like:
# /Users/admin/Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock
```

Add to your shell rc if not already exported:

```bash
export SSH_AUTH_SOCK=~"/Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock"
```

Test that the agent has your keys:

```bash
ssh-add -l
# Should list your 1Password-stored keys
```

### How forwarding reaches the sandbox

`iso -s <user> ...` invokes ssh with `-A` (forward authentication
agent). ssh creates a UNIX socket on the sandbox side owned by the
sandbox UID; signing requests are tunneled back through the SSH
connection to admin's `op ssh agent`, which prompts for biometric
approval per signature.

Inside the sandbox:

```bash
$ env | grep SSH_AUTH_SOCK
SSH_AUTH_SOCK=/tmp/ssh-XXXXXX/agent.NNNN

$ ssh-add -l
# Same keys admin has, signing happens on admin's side
```

`git push` and similar Just Work.

### Sudo path limitation (use SSH for outbound git/ssh)

The sudo path (`iso <user> ...` without `-s`) preserves
`SSH_AUTH_SOCK` in the sandbox environment (`/etc/sudoers` already
has `Defaults env_keep += "SSH_AUTH_SOCK"`), but the socket file in
admin's home is mode 0600 — sandbox UID can't connect. Agent
forwarding via sudo is therefore **not functional** even with the
env var preserved.

Workaround: use `iso -s` for any task needing outbound SSH.

## Lifecycle, rotation, audit

**Lifecycle.** Secret values exist only while a sandbox process is
alive:

1. Admin runs `iso altinity claude`.
2. `iso` calls `op read` per URI in `[users.altinity.auth]` (one
   biometric prompt batch if session is fresh; cached otherwise).
3. Resolved values go into the parent's env, then into the sudo/ssh
   subprocess via `--preserve-env` / `SendEnv`.
4. The login shell loads them; child processes inherit.
5. When the shell exits, the values vanish from RAM.

**Rotation.** Edit the value in 1Password. The next `iso` invocation
reads the new value automatically. No code changes, no `iso create`
re-runs. Long-running processes hold stale values until restarted —
same as before.

**Audit.** Every `op read` is logged in 1Password's audit feed (Business
plan; personal/family plans don't have audit log access). Filter by
item to see "who read this credential when".

## Troubleshooting

**`FATAL: 'op' CLI not found`**: the 1Password CLI isn't installed.
`brew install --cask 1password-cli`.

**`FATAL: 1Password session not active. Run: eval $(op signin)`**: your
admin shell doesn't have an active op session. Run `eval $(op signin)`
and retry.

**Secret value is empty in the sandbox**: probably the SSH path
without the sshd drop-in. Check `/etc/ssh/sshd_config.d/isolator.conf`
exists and contains `AcceptEnv ISO_TOKEN_*`, then restart sshd
(`sudo launchctl kickstart -k system/com.openssh.sshd`). Run
`ssh -v ...` to confirm the env vars are being sent and accepted.

**`ssh-add -l` empty inside sandbox**: agent forwarding didn't work.
Check that admin's shell has `SSH_AUTH_SOCK` set, the socket exists,
and `iso -s` is the path being used (sudo path can't forward agent).

**`FATAL: auth value for <VAR> must be a 1Password URI`**: the value in
`[users.<name>.auth]` isn't an `op://...` URI. Only 1Password
references are accepted; the legacy file-path keyfile format was
removed.

## Threat model

What this design defends against:

| Attack | Defense |
|---|---|
| Plaintext on disk | No `.env` written; secrets only in process env |
| Forensic artifact post-session | Process exits, env vanishes |
| Direct `su altinity` reads secrets | The new shell has no parent env from `iso` |
| Stale token after rotation | Every `iso` call re-resolves from the vault |
| Long-lived SSH key on disk | No private keys copied; agent forwarding only |
| Compromised sandbox steals SSH key | Agent doesn't expose key material; signing is gated by biometric on admin's screen |

What it does **not** defend against:

| Risk | Why this design doesn't address it |
|---|---|
| Memory scraping of running sandbox process | env block is in process memory; same UID can read /proc/self/environ |
| `ps eww` showing env to same user | macOS exposes own-process env; this is normal |
| Compromised admin machine | If admin's account is compromised, attacker has 1Password session and can resolve any secret. Defense lives in 1Password (biometric, MFA), not isolator |
| Long-running sandbox processes | Hold stale values until restarted — same as file-based |
| Memory dumps | Crash dumps may include env vars; same as any env-based scheme |

## Note on the previous keyfile scheme

Earlier versions of isolator read auth values from
`/etc/isolator/keys/*` files and wrote them into `~/.env` at user
create time. That scheme has been removed entirely. To migrate:

1. For each `[users.<name>.auth]` entry, create a 1Password item with
   the credential value and update the config to the `op://` URI.
2. Run `iso create <name>` once — this wipes any stale `~/.env`.
3. Verify with `iso <name> bash` that `$SOME_VAR` is set.
4. Delete the keyfile from `/etc/isolator/keys/<name>` — it's no
   longer read.
