#!/usr/bin/env bash
# Thin herdr plugin launcher: every verb delegates to the magnum binary. No logic lives here.
set -euo pipefail

PLUGIN_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PLUGIN_ID="${HERDR_PLUGIN_ID:-zhuravel.magnum}"
HERDR="${HERDR_BIN_PATH:-herdr}"
export MAGNUM_HOME="${MAGNUM_HOME:-$PLUGIN_ROOT}"

resolve_magnum() {
  if [ -n "${MAGNUM_BIN:-}" ] && [ -x "$MAGNUM_BIN" ]; then echo "$MAGNUM_BIN"; return; fi
  if [ -x "$PLUGIN_ROOT/bin/magnum" ]; then echo "$PLUGIN_ROOT/bin/magnum"; return; fi
  if command -v go >/dev/null 2>&1; then
    (cd "$PLUGIN_ROOT" && go build -o bin/magnum ./cmd/magnum) >/dev/null 2>&1 && echo "$PLUGIN_ROOT/bin/magnum" && return
  fi
  echo "magnum binary not found; run: cd $PLUGIN_ROOT && go build -o bin/magnum ./cmd/magnum" >&2
  exit 1
}
M="$(resolve_magnum)"

ctx() { printf '%s' "${HERDR_PLUGIN_CONTEXT_JSON:-{\}}" | jq -r "$1 // empty" 2>/dev/null || true; }
toast() { "$HERDR" notification show "magnum" --body "$1" >/dev/null 2>&1 || true; }
popup() { exec "$M" ui open "$1"; }

case "${1:-}" in
  on-startup)     "$M" kick reconcile >/dev/null 2>&1 || true ;;
  picker)         popup picker ;;
  picker-link)    MAGNUM_PICK_QUERY="${HERDR_PLUGIN_CLICKED_URL:-}" popup picker ;;
  _picker)        exec "$M" pick --query "${MAGNUM_PICK_QUERY:-}" ;;
  attention)      out="$("$M" attention 2>&1)" || toast "$out" ;;
  here)           verb="${2:?verb}"; out="$("$M" "$verb" --workspace "$(ctx .workspace_id)" --cwd "$(ctx '.workspace_cwd // .focused_pane_cwd')" 2>&1)" || true; toast "$out" ;;
  popup)          popup "${2:?pane}" ;;
  _status)        exec "$M" status --watch ;;
  _cleanup)       "$M" cleanup || true; printf '\npress any key to close'; read -rsn1 ;;
  _doctor)        "$M" doctor || true; printf '\npress any key to close'; read -rsn1 ;;
  daemon-restart) "$M" daemon-restart && toast "daemon restarted" ;;
  build)          (cd "$PLUGIN_ROOT" && go build -o bin/magnum ./cmd/magnum) ;;
  *) echo "usage: magnum-ctl.sh {on-startup|picker|picker-link|attention|here <verb>|popup <pane>|daemon-restart|build}" >&2; exit 2 ;;
esac
