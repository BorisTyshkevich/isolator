# Isolator profile — sourced at end of slot user's login rc
# Layers overrides on top of the admin's shell config

# Auth keys
[[ -f "$HOME/.env" ]] && source "$HOME/.env"

# Global tools (read-only) + local installs in home
export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"
export PATH="$HOME/.local/bin:$HOME/.npm-global/bin:$PATH"

# npm — local installs, no postinstall hooks
export NPM_CONFIG_PREFIX="$HOME/.npm-global"
export NODE_PATH="/usr/local/lib/node_modules"
export NPM_CONFIG_ignore_scripts=true

# pip — user installs only
export PIP_USER=1
export PYTHONUSERBASE="$HOME/.local"
