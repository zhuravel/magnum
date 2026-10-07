#!/usr/bin/env bash
# land.sh <branch-or-sha>
#
# Lands an agent's finished work on master in the main checkout: a
# fast-forward when master is an ancestor, else a cherry-pick of the commits
# master lacks. Before the gate it dry-runs every migration the range adds on
# a copy of the live registry. Then the gate, a push, and a build unless the
# range adds a migration (scripts/restart.sh --migration builds right before
# the restart, so the board keeps working until then). It stops at a
# conflict or a failure and says what to do. After a resolved conflict, a
# resolved commit no longer matches its original patch, so land.sh <branch>
# would pick it again: land.sh HEAD runs only the gate, the push and the build.
set -euo pipefail

target=${1:?usage: land.sh <branch-or-sha>}
repo=$(cd "$(dirname "$0")/../../../.." && pwd)
db=${MAGNUM_DB:-$HOME/.local/share/magnum/magnum.db}
cd "$repo"

[[ $(git rev-parse --abbrev-ref HEAD) == master ]] || { echo "land: the checkout is not on master" >&2; exit 1; }
if [[ -n $(git status --porcelain --untracked-files=no) ]]; then
	echo "land: the checkout has uncommitted changes; commit them or ask their owner first" >&2
	git status --short --untracked-files=no >&2
	exit 1
fi

sha=$(git rev-parse --verify "$target^{commit}")
before=$(git rev-parse HEAD)
migrations=$(git diff --name-only --diff-filter=A "$before" "$sha" -- internal/store/migrations/ | sort)

if [[ -n $migrations ]]; then
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	sqlite3 -readonly "$db" ".backup '$tmp/copy.db'"
	for m in $migrations; do
		{ echo "PRAGMA foreign_keys=ON; BEGIN;"; git show "$sha:$m"; echo "COMMIT; PRAGMA foreign_key_check;"; } |
			sqlite3 "$tmp/copy.db" >"$tmp/out" 2>&1 || { echo "land: $m fails on a copy of the registry:" >&2; cat "$tmp/out" >&2; exit 1; }
		[[ -s $tmp/out ]] && { echo "land: $m leaves foreign key problems:" >&2; cat "$tmp/out" >&2; exit 1; }
		echo "land: $m applies cleanly to a copy of the registry"
	done
fi

if git merge-base --is-ancestor "$before" "$sha"; then
	git merge --ff-only --quiet "$sha"
	echo "land: fast-forwarded master to $(git rev-parse --short HEAD)"
else
	commits=$(git rev-list --reverse --cherry-pick --right-only --no-merges "$before...$sha")
	[[ -n $commits ]] || { echo "land: master already has every commit of $target"; exit 0; }
	for c in $commits; do
		if ! git cherry-pick "$c" >/dev/null; then
			echo "land: conflict in $(git rev-parse --short "$c"); resolve it, run git cherry-pick --continue, then land.sh $target again (the rest of the range) or land.sh HEAD (gate, push and build only)" >&2
			git status --short | grep -E '^(UU|AA|DU|UD) ' >&2 || true
			exit 2
		fi
	done
	echo "land: cherry-picked $(echo "$commits" | wc -l | tr -d ' ') commit(s); master is $(git rev-parse --short HEAD)"
fi

gate=$(mktemp "${TMPDIR:-/tmp}/magnum-land-gate.XXXXXX")
if ! mise exec -- make test >"$gate" 2>&1; then
	echo "land: the gate failed on master; fix it before pushing (log: $gate)" >&2
	tail -20 "$gate" >&2
	exit 1
fi
rm -f "$gate"
echo "land: gate green"

git push --quiet origin master
echo "land: pushed $(git rev-parse --short HEAD)"

if [[ -n $migrations ]]; then
	echo "land: new migration(s); restart with scripts/restart.sh --migration (it builds right before the restart)"
else
	make build >/dev/null
	echo "land: built bin/magnum; restart with scripts/restart.sh"
fi
