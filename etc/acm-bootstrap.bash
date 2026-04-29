#!/usr/bin/env bash
# /etc/isolator/acm-bootstrap.bash — sandbox-side entry for sandboxed
# acm sessions. Invoked as `bash /etc/isolator/acm-bootstrap.bash`
# from `iso-acm-launcher` after iso transitions to the acm UID.
#
# Decodes the per-session payload (kubeconfig content, rcfile content)
# from env into ~/tmp/, then exec's an interactive bash with the
# rcfile. KUBECONFIG_DATA and ACM_RCFILE_CONTENT must be set by the
# caller; both are unset before exec so they don't leak into the user
# shell's environ.
set -euo pipefail

[ -n "${KUBECONFIG_DATA:-}" ]   || { echo "acm-bootstrap: KUBECONFIG_DATA missing" >&2; exit 1; }
[ -n "${ACM_RCFILE_CONTENT:-}" ] || { echo "acm-bootstrap: ACM_RCFILE_CONTENT missing" >&2; exit 1; }

mkdir -p "$HOME/tmp"
umask 077
KC="$HOME/tmp/acm-session.kubeconfig"
RC="$HOME/tmp/acm-session.bashrc"

printf '%s' "$KUBECONFIG_DATA" | base64 -d > "$KC"
printf '%s' "$ACM_RCFILE_CONTENT" > "$RC"

# acm-kube.sh writes the rcfile with `export KUBECONFIG="<admin-path>"`
# baked in (heredoc expansion at write time). When the sandbox shell
# sources it, that overrides our sandbox path. Append a final override
# so the last definition wins. The early `kubectl config use-context`
# calls in the rcfile still run with the admin path and fail silently
# (>/dev/null 2>&1), but they're cosmetic — every alias passes
# --context/--namespace explicitly.
printf '\nexport KUBECONFIG=%q\n' "$KC" >> "$RC"

unset KUBECONFIG_DATA ACM_RCFILE_CONTENT
export KUBECONFIG="$KC"

exec bash --rcfile "$RC" -i
