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

# acm-kube.sh writes the rcfile with values baked in via heredoc
# expansion at write time on the admin side. When the sandbox shell
# sources it, those literals overwrite anything iso resolved into our
# env. Append final-override exports so the last definition wins. The
# early `kubectl config use-context` calls in the rcfile still run
# with the (wrong) admin path and fail silently (>/dev/null 2>&1) but
# they're cosmetic — every alias passes --context/--namespace
# explicitly. Vars to override:
#   - KUBECONFIG: heredoc baked admin's path; we need the sandbox path
#   - ACM_API_KEY: heredoc baked admin's value (often empty after
#     migrating to 1Password); we need iso's resolved value
{
    printf '\n# iso-acm: re-export values that may have been shadowed\n'
    printf 'export KUBECONFIG=%q\n' "$KC"
    printf 'export ACM_API_KEY=%q\n' "${ACM_API_KEY:-}"
} >> "$RC"

unset KUBECONFIG_DATA ACM_RCFILE_CONTENT
export KUBECONFIG="$KC"

exec bash --rcfile "$RC" -i
