#!/bin/sh
# managed by magnum; `magnum hooks install` writes this file and overwrites it on an upgrade.
# MAGNUM_HOOK_ID={{agent}}
# MAGNUM_HOOK_VERSION=1
#
# Claude Code and Codex run this script on their Stop, StopFailure,
# PermissionRequest, SessionStart, SessionEnd and Interrupt hooks, with the
# agent and the event as arguments and the hook's payload on stdin. Outside a
# pane magnum started (no MAGNUM_HOOK_SOCKET in the env) it exits at once and
# reads nothing. In one, `magnum hook-event` sends the daemon that started the
# pane a hint that it should look at the pane now; it keeps a few fields of
# the payload, never the agent's messages or prompts, prints nothing and
# always exits 0 within 300 ms. The script exits 0 too when magnum cannot
# run (a rebuild in progress): a hint never fails the agent's hook.
[ -n "${MAGNUM_HOOK_SOCKET:-}" ] || exit 0
[ -x "${MAGNUM_HOOK_BIN:-}" ] || exit 0
"$MAGNUM_HOOK_BIN" hook-event "$1" "$2" >/dev/null 2>&1
exit 0
