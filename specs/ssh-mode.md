# SSH Mode for Sandboxed Users

Status: **Experimental**

## Problem

Using `sudo -u <user> -i` to switch to sandboxed users causes several macOS issues:

1. **Keychain Not Found** — Chrome and other apps can't find the user's keychain
2. **Safari as default browser** — Launch Services ignores plist for non-GUI users
3. **Workspace trust prompt** — Claude Code asks every time because session context is different
4. **No GUI context** — sandboxed user doesn't get a proper login session
5. **Chrome zombies** — processes get stuck in uninterruptible wait

These happen because `sudo -u` doesn't create a real macOS user session — it just switches UID.

## Solution: SSH to localhost

SSH via `ssh user@localhost` creates a proper login session with:
- Full Launch Services context (default browser works)
- Keychain access (login keychain auto-unlocked by SSH)
- Persistent session state (trust dialogs stick)
- Clean process lifecycle

## Implementation

### 1. Prerequisites

Enable Remote Login (one-time):
```
System Settings → General → Sharing → Remote Login → ON
```

Add sandboxed users to the SSH service ACL group:
```bash
sudo dscl . -append /Groups/com.apple.access_ssh GroupMembership <username>
```
Without this, macOS PAM denies SSH login ("failed service ACL check").

### 2. Single keypair for all sandbox users

One Ed25519 keypair for the admin. Public key installed to every sandbox user on `iso create`.

```
~/.ssh/isolator              ← private key (admin, generated once)
~/.ssh/isolator.pub          ← public key
/Users/acm/.ssh/authorized_keys    ← same public key
/Users/click/.ssh/authorized_keys  ← same public key
/Users/otel/.ssh/authorized_keys   ← same public key
```

Generate once (first `iso create`):
```bash
ssh-keygen -t ed25519 -f ~/.ssh/isolator -N "" -q -C "isolator@$(hostname -s)"
```

Install on each `iso create`:
```python
def setup_ssh_key(name, admin):
    """Install admin's isolator public key for passwordless SSH."""
    admin_home = Path(f"/Users/{admin}")
    user_home = Path(f"/Users/{name}")
    
    priv_key = admin_home / ".ssh" / "isolator"
    
    # Generate keypair if it doesn't exist yet
    if not priv_key.exists():
        run(["ssh-keygen", "-t", "ed25519", "-f", str(priv_key),
             "-N", "", "-q", "-C", f"isolator@{socket.gethostname()}"])
        run(["chmod", "600", str(priv_key)])
    
    pub_key = priv_key.with_suffix(".pub").read_text().strip()
    
    # Install public key in sandbox user's authorized_keys
    user_ssh = user_home / ".ssh"
    user_ssh.mkdir(exist_ok=True)
    auth_keys = user_ssh / "authorized_keys"
    auth_keys.write_text(pub_key + "\n")
    
    run(["chown", "-R", f"{name}:staff", str(user_ssh)])
    run(["chmod", "700", str(user_ssh)])
    run(["chmod", "600", str(auth_keys)])
    
    # Add to SSH service ACL group
    run(["dscl", ".", "-append", "/Groups/com.apple.access_ssh",
         "GroupMembership", name], check=False)
```

To revoke access for one user: remove their `authorized_keys` or remove from `com.apple.access_ssh`.

### 3. cmd_run via SSH

```python
def cmd_run(user, command, args):
    extra = COMMAND_FLAGS.get(command, [])
    admin = os.environ.get("USER", "bvt")
    priv_key = Path(f"/Users/{admin}/.ssh/isolator")
    
    if priv_key.exists():
        # Unlock keychain via sudo (needs root to read password file)
        unlock_keychain(user)
        
        # SSH mode: proper login session
        ssh_cmd = ["ssh", "-t",
                   "-i", str(priv_key),
                   "-o", "StrictHostKeyChecking=no",
                   "-o", "UserKnownHostsFile=/dev/null",
                   "-o", "LogLevel=ERROR",
                   f"{user}@127.0.0.1",
                   command] + extra + args
        os.execvp("ssh", ssh_cmd)
    else:
        # Fallback: sudo mode
        unlock_keychain(user)
        os.execvp("sudo", ["sudo", "-u", user, "-i", command] + extra + args)
```

Keychain unlock still uses sudo (reads root-only password file), then SSH for the session.

## Comparison

| Aspect | sudo mode (current) | SSH mode (proposed) |
|--------|-------------------|-------------------|
| Login session | Partial (no GUI context) | Full macOS login session |
| Keychain | Needs manual unlock | Better session support |
| Launch Services | Broken (Safari default) | Works (proper session) |
| Trust dialog | Doesn't persist | Should persist |
| Setup | Nothing extra | Enable Remote Login |
| Speed | Instant | ~200ms SSH handshake |
| Keychain unlock | sudo reads root file | sudo unlock + SSH session |
| Works remote | No (local sudo only) | Yes (SSH over network) |

## Risks

1. **Remote Login exposure** — SSH daemon listens on all interfaces. Mitigate: restrict to localhost via `sshd_config` or pf.
2. **Key management** — private keys in admin's `~/.ssh/`. If admin home is compromised, all sandbox access is compromised. But admin already has sudo, so this isn't worse.
3. **SSH brute force** — Ed25519 key-only auth, no password. Low risk.
4. **sshd_config changes** — may need `AllowUsers` or `Match` blocks for sandboxed users.

## Migration path

1. Start with one user (`otel`) as experiment
2. Keep sudo as fallback (auto-detect if SSH key exists)
3. If SSH works better, make it the default for new users
4. Document Remote Login requirement in README

## Open questions

1. Does SSH login session fix the Chrome zombie issue?
2. Does Launch Services properly register default browser via SSH?
3. Does Claude Code's workspace trust persist across SSH sessions?
4. Performance impact of SSH handshake per invocation?
5. Do we need `AcceptEnv` for passing `DOCKER_NETWORK` and other vars?
