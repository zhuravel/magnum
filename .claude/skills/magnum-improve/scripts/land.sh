#!/usr/bin/env bash
# land.sh [--continue] <branch-or-sha>
#
# Lands an agent's finished work on master in the main checkout: a
# fast-forward when master is an ancestor, else a cherry-pick of the commits
# master lacks. A conflicted docs/DECISIONS.md, where the commit only appends
# entries, is resolved by keeping both sides: master's file, then the entries
# the commit appends (none dropped, no markers). A conflict in any other file
# stops with what to do (DECISIONS.md already resolved and staged). Before
# landing it dry-runs, on a copy of the live registry, every migration master
# or the target has that the registry has not applied. Then the gate, a push,
# and a build unless such a migration exists (scripts/restart.sh --migration
# builds right before the restart, so the board keeps working until then).
#
# --continue: after a conflict resolved by hand and `git cherry-pick
# --continue`, picks the rest of the range, skipping the commits whose subject
# master already has (a resolved commit no longer matches its original patch,
# so it would be picked again), then the gate, the push and the build.
set -euo pipefail

usage="usage: land.sh [--continue] <branch-or-sha>"
resume=false
target=
for arg in "$@"; do
    case $arg in
    --continue) resume=true ;;
    -*) echo "land: unknown flag $arg" >&2; echo "$usage" >&2; exit 2 ;;
    *) [[ -z $target ]] || { echo "$usage" >&2; exit 2; }; target=$arg ;;
    esac
done
[[ -n $target ]] || { echo "$usage" >&2; exit 2; }

repo=$(cd "$(dirname "$0")/../../../.." && pwd)
db=${MAGNUM_DB:-$HOME/.local/share/magnum/magnum.db}
decisions=docs/DECISIONS.md
cd "$repo"

[[ $(git rev-parse --abbrev-ref HEAD) == master ]] || { echo "land: the checkout is not on master" >&2; exit 1; }
if git rev-parse -q --verify CHERRY_PICK_HEAD >/dev/null; then
    echo "land: a cherry-pick is in progress; resolve it, git add the files, git cherry-pick --continue, then land.sh --continue $target" >&2
    exit 1
fi
if [[ -n $(git status --porcelain --untracked-files=no) ]]; then
    echo "land: the checkout has uncommitted changes; commit them or ask their owner first" >&2
    git status --short --untracked-files=no >&2
    exit 1
fi

sha=$(git rev-parse --verify "$target^{commit}")
before=$(git rev-parse HEAD)
work=$(mktemp -d "${TMPDIR:-/tmp}/magnum-land.XXXXXX")
trap 'rm -rf "$work"' EXIT

# The migrations the registry has not applied (above its user_version) that
# master or the target has, oldest first. Without a registry there is none.
pending=
if [[ -f $db ]]; then
    schema=$(sqlite3 -readonly "$db" 'PRAGMA user_version')
    for m in $({ git ls-tree --name-only HEAD internal/store/migrations/; git ls-tree --name-only "$sha" internal/store/migrations/; } | sort -u); do
        n=${m##*/}
        n=${n%%_*}
        if [[ $n =~ ^[0-9]+$ ]] && ((10#$n > schema)); then
            pending+="$m "
        fi
    done
fi
if [[ -n $pending ]]; then
    sqlite3 -readonly "$db" ".backup '$work/copy.db'"
    for m in $pending; do
        { echo "PRAGMA foreign_keys=ON; BEGIN;"; git show "HEAD:$m" 2>/dev/null || git show "$sha:$m"; echo "COMMIT; PRAGMA foreign_key_check;"; } |
            sqlite3 "$work/copy.db" >"$work/out" 2>&1 || { echo "land: $m fails on a copy of the registry:" >&2; cat "$work/out" >&2; exit 1; }
        [[ -s $work/out ]] && { echo "land: $m leaves foreign key problems:" >&2; cat "$work/out" >&2; exit 1; }
        echo "land: $m applies cleanly to a copy of the registry"
    done
fi

# keep_both_decisions resolves a conflicted docs/DECISIONS.md when the commit
# only appends to it (its parent's file is a prefix of its own): master's file,
# then the text the commit appends, unless master already has that text.
keep_both_decisions() {
    local size
    git show ":1:$decisions" >"$work/base" 2>/dev/null &&
        git show ":2:$decisions" >"$work/ours" 2>/dev/null &&
        git show ":3:$decisions" >"$work/theirs" 2>/dev/null || return 1
    size=$(wc -c <"$work/base" | tr -d ' ')
    head -c "$size" "$work/theirs" | cmp -s - "$work/base" || return 1
    tail -c +"$((size + 1))" "$work/theirs" >"$work/added"
    [[ -s $work/added ]] || return 1
    ! grep -qE '^(<<<<<<<|>>>>>>>)' "$work/ours" "$work/added" || return 1
    if [[ $(<"$work/ours") == *"$(<"$work/added")"* ]]; then
        cp "$work/ours" "$decisions"
    else
        {
            cat "$work/ours"
            [[ -z $(tail -c 1 "$work/ours") ]] || echo
            cat "$work/added"
        } >"$decisions"
    fi
}

# stop explains a cherry-pick that did not go through and exits.
stop() {
    local c=$1 conflicted=$2 short subject p
    short=$(git rev-parse --short "$c")
    if [[ -z $conflicted ]]; then
        echo "land: $short did not apply:" >&2
        cat "$work/pick.out" >&2
        echo "fix: when master already has its change (an empty pick), git cherry-pick --skip; then land.sh --continue $target" >&2
        exit 2
    fi
    subject=$(git log -1 --format=%s "$c")
    ((${#subject} <= 80)) || subject="${subject:0:77}..."
    echo "land: conflict in $short ($subject):" >&2
    printf '  %s\n' $conflicted >&2
    if grep -qxF "$decisions" <<<"$conflicted"; then
        echo "  ($decisions: the commit changes more than the end of the file, so it is not merged by itself)" >&2
    fi
    echo "resolve it ($decisions: keep both entries; the config comments: keep both sides' keys; SKILL.md: keep both" \
        "rules and raise its size cap by the minimum), git add the files, git cherry-pick --continue, then" \
        "land.sh --continue $target (the rest of the range, then the gate, the push and the build)" >&2
    for p in $(printf '%s\n' $conflicted | sed -n -E 's#^(.*)/testdata/[^/]+\.golden$#\1#p' | sort -u); do
        echo "golden files of ./$p conflict: resolve the files they are rendered from first, then regenerate them with" \
            "\`go test ./$p -update\` and git add them before git cherry-pick --continue" >&2
    done
    exit 2
}

# pick cherry-picks one commit. A conflicted docs/DECISIONS.md is resolved by
# keeping both sides (and staged); with no other conflicted file the pick goes
# on, else the landing stops with the rest to resolve by hand.
pick() {
    local c=$1 conflicted
    git cherry-pick "$c" >"$work/pick.out" 2>&1 && return 0
    conflicted=$(git diff --name-only --diff-filter=U)
    if grep -qxF "$decisions" <<<"$conflicted" && keep_both_decisions; then
        git add -- "$decisions"
        echo "land: $(git rev-parse --short "$c"): kept both sides of $decisions (master's entries, then the commit's)"
        conflicted=$(git diff --name-only --diff-filter=U)
        if [[ -z $conflicted ]]; then
            GIT_EDITOR=true git cherry-pick --continue >"$work/pick.out" 2>&1 || stop "$c" ""
            return 0
        fi
    fi
    stop "$c" "$conflicted"
}

if git merge-base --is-ancestor "$before" "$sha"; then
    git merge --ff-only --quiet "$sha"
    echo "land: fast-forwarded master to $(git rev-parse --short HEAD)"
else
    commits=$(git rev-list --reverse --cherry-pick --right-only --no-merges "$before...$sha")
    if $resume && [[ -n $commits ]]; then
        landed=$(git log --format=%s "$(git merge-base "$before" "$sha")..$before")
        rest=
        for c in $commits; do
            if grep -qxF -- "$(git log -1 --format=%s "$c")" <<<"$landed"; then
                echo "land: skipped $(git rev-parse --short "$c"): master has a commit with its subject"
            else
                rest+="$c "
            fi
        done
        commits=$rest
    fi
    if [[ -z $commits ]]; then
        $resume || { echo "land: master already has every commit of $target"; exit 0; }
        echo "land: master has every commit of $target"
    else
        n=0
        for c in $commits; do
            pick "$c"
            n=$((n + 1))
        done
        echo "land: cherry-picked $n commit(s); master is $(git rev-parse --short HEAD)"
    fi
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

if [[ -n $pending ]]; then
    echo "land: the registry lacks migration(s) $pending; restart with scripts/restart.sh --migration (it builds right before the restart)"
else
    make build >/dev/null
    echo "land: built bin/magnum; restart with scripts/restart.sh"
fi
