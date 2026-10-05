#!/usr/bin/env bash
# Thin herdr plugin launcher: every verb delegates to the magnum binary. No logic lives here.
set -euo pipefail

# The plugin is either a magnum checkout (linked for development) or the copy
# `magnum install --plugin` writes from an installed binary. Either way the
# binary finds its own files (MAGNUM_HOME is left to the caller).
PLUGIN_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PLUGIN_ID="${HERDR_PLUGIN_ID:-zhuravel.magnum}"
HERDR="${HERDR_BIN_PATH:-herdr}"

resolve_magnum() {
  if [ -n "${MAGNUM_BIN:-}" ] && [ -x "$MAGNUM_BIN" ]; then echo "$MAGNUM_BIN"; return; fi
  if [ -x "$PLUGIN_ROOT/bin/magnum" ]; then echo "$PLUGIN_ROOT/bin/magnum"; return; fi
  if [ ! -f "$PLUGIN_ROOT/go.mod" ]; then # an installed plugin: the installed binary
    for m in "$(command -v magnum 2>/dev/null || true)" /opt/homebrew/bin/magnum /usr/local/bin/magnum "$HOME/.local/bin/magnum"; do
      if [ -n "$m" ] && [ -x "$m" ]; then echo "$m"; return; fi
    done
    echo "magnum binary not found; install it: brew install zhuravel/tap/magnum" >&2
    exit 1
  fi
  if command -v go >/dev/null 2>&1; then
    (cd "$PLUGIN_ROOT" && go build -o bin/magnum ./cmd/magnum) >/dev/null 2>&1 && echo "$PLUGIN_ROOT/bin/magnum" && return
  fi
  echo "magnum binary not found; run: cd $PLUGIN_ROOT && go build -o bin/magnum ./cmd/magnum" >&2
  exit 1
}
M="$(resolve_magnum)"

ctx() { printf '%s' "${HERDR_PLUGIN_CONTEXT_JSON:-{\}}" | jq -r "$1 // empty" 2>/dev/null || true; }
toast() { "$HERDR" notification show "magnum" --body "$1" >/dev/null 2>&1 || true; }

# capture runs "$@" out of sight (an action has no terminal), keeping the last line it printed in OUT and
# the last line it wrote to stderr in ERR; it returns the command's exit status.
capture() {
  local errf rc=0
  errf="$(mktemp "${TMPDIR:-/tmp}/magnum-ctl.XXXXXX")"
  OUT="$("$@" 2>"$errf")" || rc=$?
  OUT="$(printf '%s\n' "$OUT" | awk 'NF { l = $0 } END { print l }')"
  ERR="$(awk 'NF { l = $0 } END { print l }' "$errf")"
  rm -f "$errf"
  return "$rc"
}
# failed toasts why an action failed: its last stderr line, else its last output line.
failed() { toast "${ERR:-${OUT:-magnum $1 failed}}"; }
popup() { capture "$M" ui open "$1" || failed "ui open"; }
# hold keeps a popup open after its command failed, so the error stays on screen (an exec would close the
# popup with it); a command that worked, or that ctrl+c stopped, closes it. It returns the command's status.
hold() {
  if [ "$1" -ne 0 ] && [ "$1" -ne 130 ]; then printf '\npress any key to close'; read -rsn1 || true; fi
  return "$1"
}

case "${1:-}" in
  on-startup)     "$M" kick >/dev/null 2>&1 || true ;;
  picker)         popup picker ;;
  picker-link)    MAGNUM_PICK_QUERY="${HERDR_PLUGIN_CLICKED_URL:-}" popup picker ;;
  _picker)        rc=0; "$M" pick --query "${MAGNUM_PICK_QUERY:-}" || rc=$?; hold "$rc" ;;
  attention)      capture "$M" attention || failed attention ;;
  here)           verb="${2:?verb}"
                  if capture "$M" "$verb" --workspace "$(ctx .workspace_id)" --cwd "$(ctx '.workspace_cwd // .focused_pane_cwd')"; then
                    toast "${OUT:-${ERR:-magnum $verb: done}}"
                  else
                    failed "$verb"
                  fi ;;
  popup)          popup "${2:?pane}" ;;
  _status)        rc=0; "$M" status --watch || rc=$?; hold "$rc" ;;
  _cleanup)       "$M" cleanup || true; printf '\npress any key to close'; read -rsn1 ;;
  _doctor)        "$M" doctor || true; printf '\npress any key to close'; read -rsn1 ;;
  # The restart waits, holding nothing, until no round is in flight; its last line is toasted either way.
  daemon-restart) if capture "$M" daemon-restart --when-idle; then toast "${OUT:-daemon restarted}"; else failed daemon-restart; fi ;;
  build)          if capture sh -c 'cd "$1" && go build -o bin/magnum ./cmd/magnum' build "$PLUGIN_ROOT"; then
                    toast "built bin/magnum; the daemon runs it after a restart (Magnum: restart daemon)"
                  else
                    failed build
                  fi ;;
  *) echo "usage: magnum-ctl.sh {on-startup|picker|picker-link|attention|here <verb>|popup <pane>|daemon-restart|build}" >&2; exit 2 ;;
esac
