#!/bin/bash
set -euo pipefail

# Isolator installer
# Usage: sudo ./install.sh

RED='\033[0;31m'
GREEN='\033[0;32m'
BLUE='\033[0;34m'
NC='\033[0m'

info()  { echo -e "${BLUE}==>${NC} $*"; }
ok()    { echo -e "${GREEN}  ✓${NC} $*"; }
warn()  { echo -e "${RED}  !${NC} $*"; }

if [[ $EUID -ne 0 ]]; then
    echo "Usage: sudo ./install.sh"
    exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ADMIN="${SUDO_USER:-$(whoami)}"

info "Installing Isolator for admin user: $ADMIN"

# 1. Config directory
info "Setting up /etc/isolator/"
mkdir -p /etc/isolator
cp "$SCRIPT_DIR/etc/profile" /etc/isolator/profile
cp "$SCRIPT_DIR/etc/CLAUDE.md" /etc/isolator/CLAUDE.md
cp "$SCRIPT_DIR/etc/open-browser" /etc/isolator/open-browser
chmod 644 /etc/isolator/profile /etc/isolator/CLAUDE.md
chmod 755 /etc/isolator/open-browser

# Config — only copy if not exists (don't overwrite user's config)
if [[ ! -f /etc/isolator/config.toml ]]; then
    cp "$SCRIPT_DIR/etc/config.toml" /etc/isolator/config.toml
    # Set admin user in config
    sed -i '' "s/admin = \"bvt\"/admin = \"$ADMIN\"/" /etc/isolator/config.toml
    ok "Created config.toml (admin = $ADMIN)"
else
    ok "config.toml already exists, skipping"
fi
chmod 600 /etc/isolator/config.toml
ok "Config installed"

# 2. Install iso and docker-proxy to PATH
info "Installing commands"
cp "$SCRIPT_DIR/bin/iso" /usr/local/bin/iso
cp "$SCRIPT_DIR/bin/docker-proxy" /usr/local/bin/docker-proxy
chmod 755 /usr/local/bin/iso /usr/local/bin/docker-proxy
ok "iso and docker-proxy installed to /usr/local/bin/"

# 3. Docker socket hardlink (for OrbStack)
info "Setting up Docker socket"
ORBSTACK_SOCK="/Users/$ADMIN/.orbstack/run/docker.sock"
if [[ -S "$ORBSTACK_SOCK" ]]; then
    cp "$SCRIPT_DIR/etc/com.isolator.docker-proxy.plist" /Library/LaunchDaemons/
    launchctl load /Library/LaunchDaemons/com.isolator.docker-proxy.plist 2>/dev/null || true
    ok "Docker socket launchd job installed"
else
    warn "OrbStack socket not found, skipping Docker setup"
fi

# 4. Workspaces directory
info "Setting up /Users/Workspaces/"
mkdir -p /Users/Workspaces
chown root:staff /Users/Workspaces
chmod 775 /Users/Workspaces
ok "Workspaces directory ready"

# 5. Check prerequisites
info "Checking prerequisites"

# Remote Login
if systemsetup -getremotelogin 2>/dev/null | grep -q "On"; then
    ok "Remote Login is enabled (SSH mode)"
else
    warn "Remote Login is OFF — enable it for SSH mode:"
    warn "  System Settings → General → Sharing → Remote Login → ON"
    warn "  (iso will fall back to sudo mode without it)"
fi

# Homebrew docker CLI (not OrbStack shim)
if [[ -x /opt/homebrew/bin/docker ]]; then
    ok "Homebrew Docker CLI found"
else
    warn "Homebrew Docker CLI not found. Install with: brew install docker"
    warn "  (needed for Docker proxy — OrbStack shim bypasses socket filtering)"
fi

# pam_reattach for iTerm Touch ID
if [[ -f /opt/homebrew/lib/pam/pam_reattach.so ]]; then
    ok "pam_reattach installed (Touch ID in iTerm)"
else
    warn "pam_reattach not found. Install for Touch ID in iTerm:"
    warn "  brew install pam-reattach"
fi

echo ""
info "Installation complete!"
echo ""
echo "  Next steps:"
echo "    1. Edit /etc/isolator/config.toml — set admin username and hosts"
echo "    2. Enable Remote Login if not already (for SSH mode)"
echo "    3. Create sandbox users:"
echo "       iso create acm --keychain"
echo "       iso create click --keychain"
echo "    4. Apply firewall rules (optional):"
echo "       iso pf"
echo "    5. Run:"
echo "       iso acm claude"
echo ""
