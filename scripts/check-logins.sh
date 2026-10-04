#!/bin/sh
# check-logins: this repository is public, and the people whose pull requests
# magnum reviews never agreed to appear in it. Fail when a tracked file names
# a human GitHub login from the local registry (PR authors, assignees,
# requested reviewers), except the logins in MAGNUM_OWN_LOGINS and
# MAGNUM_LOGIN_ALLOW (comma or space separated, set in the gitignored
# .mise.local.toml). Tests use placeholders (alice, bob, rev-ann). Without a
# registry (a fresh clone, CI) there is nothing to check.
set -eu
db="${MAGNUM_DB:-}"
if [ -z "$db" ]; then # the checkout's registry, else the installed one (magnum migrate-home)
  for c in state/magnum.db "${XDG_DATA_HOME:-$HOME/.local/share}/magnum/magnum.db"; do
    if [ -f "$c" ]; then db="$c"; break; fi
  done
fi
if [ ! -f "$db" ] || ! command -v sqlite3 >/dev/null 2>&1; then
  echo "check-logins: no registry at $db (or no sqlite3): skipped"
  exit 0
fi
list="$(mktemp)"
trap 'rm -f "$list"' EXIT
sqlite3 -readonly "$db" "
  SELECT author_login FROM prs WHERE author_login IS NOT NULL AND coalesce(author_type, 'User') = 'User'
  UNION SELECT value FROM prs, json_each(prs.assignees_json)
  UNION SELECT value FROM prs, json_each(prs.requested_reviewers_json) WHERE value NOT LIKE 'team:%'" |
  sed -e 's/\[bot\]$//' -e '/^$/d' | sort -u > "$list.all"
skip="$(printf '%s %s' "${MAGNUM_OWN_LOGINS:-}" "${MAGNUM_LOGIN_ALLOW:-}" | tr ', ' '\n\n' | sed '/^$/d')"
if [ -n "$skip" ]; then
  printf '%s\n' "$skip" | /usr/bin/grep -vixF -f - "$list.all" > "$list" || true
else
  cp "$list.all" "$list"
fi
rm -f "$list.all"
[ -s "$list" ] || exit 0
if git grep -qwiIF -f "$list" -- . ':!config.local.toml'; then
  echo "tracked files name people from the registry (use placeholders; MAGNUM_LOGIN_ALLOW exempts a login):"
  git grep -nwiIF -f "$list" -- . ':!config.local.toml' | cut -c1-200
  exit 1
fi
