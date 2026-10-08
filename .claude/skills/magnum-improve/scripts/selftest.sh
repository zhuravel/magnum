#!/usr/bin/env bash
# selftest.sh
#
# Tests the scripts here against throwaway git repositories in a temporary
# directory (under $TMPDIR), never the real repository, registry or daemon:
#   - bash -n on every script;
#   - land.sh: a conflict confined to docs/DECISIONS.md lands with both
#     entries whole and in order, a commit that edits an old entry or another
#     conflicted file stops, golden files name `go test ./<pkg> -update`, a
#     cherry-pick in progress is refused, --continue after a resolution by
#     hand picks the rest and skips the resolved commit, a re-pick never
#     duplicates an entry, and a migration the registry lacks is dry-run and
#     not built;
#   - restart.sh: its arguments and the drain it runs;
#   - eval-at.sh: its arguments, run directory, worktree, build and log, the
#     cleanup on a failure, the reuse of a stored replay with the same inputs
#     and Go files (and none with other Go files, an unknown commit or
#     --fresh), and the report with the points it prints.
# git, make and sqlite3 are real; mise, go and bin/magnum are fakes on PATH.
# It prints one line per check and stops at the first failure.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
tmp_root=${TMPDIR:-/tmp}
tmp=$(mktemp -d "${tmp_root%/}/magnum-selftest.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

checks=0
ok() { checks=$((checks + 1)); echo "ok    $*"; }
fail() {
    echo "FAIL  $*" >&2
    [[ ! -s $tmp/out ]] || { echo "----- output:" >&2; cat "$tmp/out" >&2; }
    exit 1
}
# run <cmd...>: runs it with its output in $tmp/out and its exit code in $rc.
run() {
    rc=0
    "$@" >"$tmp/out" 2>&1 || rc=$?
}
has() { grep -qF -- "$1" "$tmp/out"; }
count() { grep -cxF -- "$1" "$2" || true; }

for s in "$here"/*.sh; do
    bash -n "$s" || fail "bash -n $(basename "$s")"
done
ok "bash -n on every script"

# Git without the operator's configuration (hooks, signing, templates).
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=$tmp/gitconfig
: >"$GIT_CONFIG_GLOBAL"
export GIT_AUTHOR_NAME=selftest GIT_AUTHOR_EMAIL=selftest@example.com
export GIT_COMMITTER_NAME=selftest GIT_COMMITTER_EMAIL=selftest@example.com

# Fakes: mise runs what follows "--", go build writes the fake magnum, which
# records its arguments and prints what a drain prints, the stored replays in
# $FAKE_BASELINE for `eval baseline`, and a run's first line and report for
# `eval run`.
mkdir -p "$tmp/bin"
cat >"$tmp/bin/mise" <<'EOF'
#!/bin/sh
while [ $# -gt 0 ] && [ "$1" != "--" ]; do shift; done
[ $# -gt 0 ] && shift
exec "$@"
EOF
cat >"$tmp/bin/go" <<'EOF'
#!/bin/sh
[ "$1" = build ] || exit 1
out=
while [ $# -gt 0 ]; do [ "$1" = -o ] && out=$2; shift; done
pwd >"$FAKE_GO_PWD"
mkdir -p "$(dirname "$out")" && cp "$FAKE_MAGNUM" "$out" && chmod +x "$out"
EOF
cat >"$tmp/fake-magnum" <<'EOF'
#!/bin/sh
for a in "$@"; do printf '%s\n' "$a"; done >>"$FAKE_MAGNUM_ARGS"
case $1 in
daemon-restart)
    echo "draining (pid 1): no new rounds start; waiting for 1 round(s) in flight (at most 30m0s): example/app#1 (reviewing)"
    echo "draining: 1 round(s) in flight after 15s: example/app#1 (reviewing)"
    echo "drained after 20s: no round in flight"
    echo "restarted the daemon" ;;
eval)
    if [ "$2" = baseline ]; then
        [ -z "${FAKE_BASELINE:-}" ] || cat "$FAKE_BASELINE"
        exit 0
    fi
    echo "fake magnum: $*"
    echo "eval run 20261008-120000: 1 case(s), corpus eval.toml, results in state"
    echo
    echo "run 20261008-120000  \"label\""
    echo "codex: +1 points (case-b +1)" ;;
esac
exit "${FAKE_MAGNUM_EXIT:-0}"
EOF
chmod +x "$tmp/bin/mise" "$tmp/bin/go" "$tmp/fake-magnum"
export PATH=$tmp/bin:$PATH FAKE_MAGNUM=$tmp/fake-magnum FAKE_MAGNUM_ARGS=$tmp/magnum-args FAKE_GO_PWD=$tmp/go-pwd
export MAGNUM_DB=$tmp/no-registry.db

# The throwaway repository: the scripts at their place in the tree, a gate and
# a build that do nothing real, a DECISIONS.md, and a bare origin.
repo=$tmp/repo
scripts=$repo/.claude/skills/magnum-improve/scripts
git init -q --bare -b master "$tmp/origin.git"
git init -q -b master "$repo"
mkdir -p "$scripts" "$repo/docs" "$repo/internal/agents/testdata"
cp "$here"/*.sh "$scripts/"
printf 'test:\n\t@true\nbuild:\n\t@mkdir -p bin && cp "$$FAKE_MAGNUM" bin/magnum\n' >"$repo/Makefile"
printf 'bin/\n' >"$repo/.gitignore"
cat >"$repo/docs/DECISIONS.md" <<'EOF'
# Decisions

## Runtime

- **First.** One line.
  Second line.
EOF
echo base >"$repo/x.txt"
echo golden >"$repo/internal/agents/testdata/judge.golden"
cd "$repo"
git add -A
git commit -q -m "base"
git remote add origin "$tmp/origin.git"
git push -q -u origin master
base=$(git rev-parse HEAD)

# Entries share a blank line and a last line, as real ones do: a line-based
# merge would interleave them.
master_entry='- **Master entry.** Its text.

  A second paragraph.
  No new dependencies.'
branch_entry='- **Branch entry.** Other text.

  Another paragraph.
  No new dependencies.'

# commit_on <branch-or-""> <message> <path=content>...: one commit, on a new
# branch from $base when a branch is named; content "+text" appends.
commit_on() {
    local b=$1 msg=$2 kv
    shift 2
    if [[ -n $b ]]; then git checkout -q -b "$b" "$base"; fi
    for kv in "$@"; do
        mkdir -p "$(dirname "${kv%%=*}")"
        if [[ ${kv#*=} == +* ]]; then
            printf '%s\n' "${kv#*=+}" >>"${kv%%=*}"
        else
            printf '%s\n' "${kv#*=}" >"${kv%%=*}"
        fi
    done
    git add -A
    git commit -q -m "$msg"
    if [[ -n $b ]]; then git checkout -q master; fi
}

# 1. Both sides append to DECISIONS.md: it lands with both entries whole.
commit_on b1 "branch one" "docs/DECISIONS.md=+$branch_entry" "b.txt=b"
commit_on "" "master one" "docs/DECISIONS.md=+$master_entry" "a.txt=a"
{ git show "$base:docs/DECISIONS.md"; printf '%s\n%s\n' "$master_entry" "$branch_entry"; } >"$tmp/want"
run "$scripts/land.sh" b1
[[ $rc == 0 ]] || fail "land.sh b1 exits 0 (got $rc)"
cmp -s docs/DECISIONS.md "$tmp/want" || { diff "$tmp/want" docs/DECISIONS.md >&2 || true; fail "DECISIONS.md holds master's entry, then the branch's, whole"; }
ok "a DECISIONS.md-only conflict lands with both entries whole, master's first"
! grep -qE '^(<<<<<<<|=======|>>>>>>>)' docs/DECISIONS.md || fail "no conflict markers are left"
[[ -f b.txt && -z $(git status --porcelain --untracked-files=no) ]] || fail "the commit's other file landed and the tree is clean"
has "kept both sides of docs/DECISIONS.md" || fail "land.sh says it kept both sides"
ok "no markers, the commit's other files landed, the merge is announced"
[[ $(git rev-parse origin/master) == $(git rev-parse HEAD) && -x bin/magnum ]] || fail "land.sh pushed and built"
ok "the gate ran, master was pushed and bin/magnum built"

# 2. A commit that edits an old entry is not merged by itself.
base=$(git rev-parse HEAD)
commit_on b2 "branch two" "docs/DECISIONS.md=# Decisions, edited"
commit_on "" "master two" "docs/DECISIONS.md=+- **Another.** text"
before=$(git rev-parse HEAD)
run "$scripts/land.sh" b2
[[ $rc == 2 ]] && has "not merged by itself" && has "land.sh --continue b2" || fail "an edit of an old entry stops with exit 2"
git cherry-pick --abort
[[ $(git rev-parse HEAD) == "$before" ]] || fail "nothing landed"
ok "a commit that changes more than the end of DECISIONS.md stops"

# 3. Another conflicted file stops; a cherry-pick in progress is refused; a
#    re-pick without --continue duplicates no entry; --continue picks the rest.
base=$(git rev-parse HEAD)
git checkout -q -b b3 "$base"
printf '%s\n' theirs >x.txt
printf '%s\n' "- **Entry D1.** text" >>docs/DECISIONS.md
git add -A
git commit -q -m "branch three, part one"
printf '%s\n' "- **Entry D2.** text" >>docs/DECISIONS.md
echo y >y.txt
git add -A
git commit -q -m "branch three, part two"
git checkout -q master
commit_on "" "master three" "x.txt=ours" "docs/DECISIONS.md=+- **Entry M3.** text"
run "$scripts/land.sh" b3
[[ $rc == 2 ]] && has "x.txt" && has "land.sh --continue b3" || fail "a conflict in x.txt stops with exit 2 and names --continue"
{ git show "$base:docs/DECISIONS.md"; printf '%s\n' "- **Entry M3.** text" "- **Entry D1.** text"; } >"$tmp/want"
[[ $(git diff --name-only --diff-filter=U) == x.txt ]] || fail "only x.txt is left to resolve"
cmp -s docs/DECISIONS.md "$tmp/want" || fail "DECISIONS.md holds master's entry, then the commit's"
[[ -z $(git diff -- docs/DECISIONS.md) ]] || fail "DECISIONS.md is staged"
ok "a conflict in another file stops and says land.sh --continue, with DECISIONS.md already resolved and staged"
run "$scripts/land.sh" --continue b3
[[ $rc == 1 ]] && has "cherry-pick is in progress" || fail "land.sh refuses while a cherry-pick is in progress"
ok "a cherry-pick in progress is refused"
echo theirs >x.txt
git add x.txt
GIT_EDITOR=true git cherry-pick --continue >/dev/null
resolved=$(git rev-parse HEAD)
run "$scripts/land.sh" b3
[[ $rc == 2 && $(count "- **Entry D1.** text" docs/DECISIONS.md) == 1 ]] ||
    fail "a re-pick of the resolved commit stops and leaves its entry once"
ok "a re-pick of a resolved commit (land.sh without --continue) stops and duplicates no entry"
if git rev-parse -q --verify CHERRY_PICK_HEAD >/dev/null; then git cherry-pick --skip >/dev/null 2>&1; fi
[[ $(git rev-parse HEAD) == "$resolved" ]] || fail "the re-pick committed nothing"
run "$scripts/land.sh" --continue b3
[[ $rc == 0 ]] || fail "land.sh --continue b3 exits 0 (got $rc)"
has "skipped" || fail "--continue skips the resolved commit"
[[ $(git log --format=%s "$base..HEAD" | grep -cx "branch three, part one") == 1 ]] || fail "the resolved commit is on master once"
[[ -f y.txt && $(count "- **Entry D2.** text" docs/DECISIONS.md) == 1 && $(count "- **Entry D1.** text" docs/DECISIONS.md) == 1 ]] ||
    fail "the rest of the range landed, each entry once"
[[ $(git rev-parse origin/master) == $(git rev-parse HEAD) ]] || fail "--continue pushed"
ok "--continue skips the resolved commit by subject, picks the rest, gates, pushes and builds"

# 4. Golden files in a conflict: the message says how to regenerate them.
base=$(git rev-parse HEAD)
commit_on b4 "branch four" "internal/agents/testdata/judge.golden=theirs"
commit_on "" "master four" "internal/agents/testdata/judge.golden=ours"
run "$scripts/land.sh" b4
[[ $rc == 2 ]] && has 'go test ./internal/agents -update' || fail "a golden conflict names go test ./internal/agents -update"
git cherry-pick --abort
ok "a conflict in golden files says to regenerate them with go test ./internal/agents -update"

# 5. A migration the registry lacks is dry-run on a copy and not built.
if command -v sqlite3 >/dev/null; then
    sqlite3 "$tmp/registry.db" "create table t1 (x); pragma user_version = 1;"
    git checkout -q -b b5
    commit_on "" "migration two" "internal/store/migrations/0002_two.sql=create table t2 (y);" \
        "internal/store/migrations/0001_one.sql=create table t1 (x);"
    git checkout -q master
    rm -f bin/magnum
    MAGNUM_DB=$tmp/registry.db run "$scripts/land.sh" b5
    [[ $rc == 0 ]] && has "0002_two.sql applies cleanly" && has "restart.sh --migration" && [[ ! -e bin/magnum ]] ||
        fail "a new migration is dry-run and bin/magnum is not built"
    ! has "0001_one.sql" || fail "a migration the registry has is not dry-run"
    ok "a migration above the registry's user_version is dry-run on a copy, and nothing is built"
fi

# 6. restart.sh: its arguments and the drain.
run "$scripts/restart.sh" --max-wait soon
[[ $rc == 2 ]] && has "minutes or hours" || fail "restart.sh refuses --max-wait soon"
run "$scripts/restart.sh" --now
[[ $rc == 2 ]] || fail "restart.sh refuses an unknown argument"
: >"$FAKE_MAGNUM_ARGS"
run "$scripts/restart.sh"
[[ $rc == 0 && $(tr '\n' ' ' <"$FAKE_MAGNUM_ARGS") == "daemon-restart --drain --timeout 30m " ]] ||
    fail "restart.sh runs daemon-restart --drain --timeout 30m"
has "draining (pid 1)" && has "restarted the daemon" && ! has "draining: 1 round(s)" ||
    fail "restart.sh passes the drain's lines but its 15-second progress"
: >"$FAKE_MAGNUM_ARGS"
run "$scripts/restart.sh" --max-wait 2h
[[ $rc == 0 && $(tr '\n' ' ' <"$FAKE_MAGNUM_ARGS") == "daemon-restart --drain --timeout 2h " ]] || fail "--max-wait 2h is the drain's timeout"
ok "restart.sh drains with --max-wait as the bound (default 30m) and refuses bad arguments"

# 7. eval-at.sh.
run_dir=$tmp/run
mkdir -p "$run_dir"
unset MAGNUM_IMPROVE_RUN
run "$scripts/eval-at.sh" HEAD label case-a
[[ $rc == 2 ]] && has "no run directory" || fail "eval-at.sh needs a run directory"
export MAGNUM_IMPROVE_RUN=$run_dir
run "$scripts/eval-at.sh" HEAD label
[[ $rc == 2 ]] && has "usage:" || fail "eval-at.sh needs a case"
run "$scripts/eval-at.sh" --cases HEAD label case-a
[[ $rc == 2 ]] && has "unknown flag" || fail "eval-at.sh refuses an unknown flag"
run "$scripts/eval-at.sh" nosuchref label case-a
[[ $rc == 2 ]] && has "is not a commit" || fail "eval-at.sh refuses a sha that is not a commit"
run "$scripts/eval-at.sh" --run-dir "$tmp/missing" HEAD label case-a
[[ $rc == 2 ]] && has "does not exist" || fail "eval-at.sh refuses a missing run directory"
ok "eval-at.sh refuses missing or bad arguments"

: >"$FAKE_MAGNUM_ARGS"
sha=$(git rev-parse --short HEAD~1)
run "$scripts/eval-at.sh" "$sha" "After: miss rules (c107627)" case-a case-b
[[ $rc == 0 ]] || fail "eval-at.sh exits 0 (got $rc)"
wt=$run_dir/tmp/eval-after-miss-rules-c107627
[[ $(cat "$FAKE_GO_PWD") == "$wt" ]] || fail "the build ran in $wt (it ran in $(cat "$FAKE_GO_PWD"))"
printf '%s\n' eval baseline --case case-a --case case-b eval run --case case-a --case case-b --label "After: miss rules (c107627)" >"$tmp/want"
cmp -s "$FAKE_MAGNUM_ARGS" "$tmp/want" || fail "bin/magnum eval baseline, then eval run got --case case-a --case case-b --label <label>"
grep -qF "fake magnum: eval run" "$wt.log" || fail "the eval's output is in $wt.log"
[[ ! -e $wt && $(git worktree list --porcelain | grep -c '^worktree ') == 1 ]] || fail "the worktree is removed"
ok "eval-at.sh builds in a detached worktree at the sha, runs eval run with the cases and label, logs, removes it"
has 'run 20261008-120000  "label"' && has "codex: +1 points (case-b +1)" && ! has "fake magnum: eval run" ||
    fail "eval-at.sh prints the run's report, from its header on, with the points"
ok "eval-at.sh prints the report the run ends with, the Codex points included"

: >"$FAKE_MAGNUM_ARGS"
run_dir2=$tmp/run2
mkdir -p "$run_dir2"
FAKE_MAGNUM_EXIT=3 run "$scripts/eval-at.sh" --run-dir "$run_dir2" HEAD before case-a
[[ $rc == 3 ]] || fail "a failing eval run exits with its code (got $rc)"
[[ -f $run_dir2/tmp/eval-before.log && ! -e $run_dir2/tmp/eval-before && ! -e $run_dir/tmp/eval-before.log ]] ||
    fail "--run-dir wins over MAGNUM_IMPROVE_RUN, and the worktree is removed after a failure"
[[ $(git worktree list --porcelain | grep -c '^worktree ') == 1 ]] || fail "no worktree is left"
ok "--run-dir wins over MAGNUM_IMPROVE_RUN; a failing eval exits with its code and leaves no worktree"

mkdir -p "$run_dir/tmp/eval-busy"
run "$scripts/eval-at.sh" HEAD busy case-a
[[ $rc == 2 ]] && has "exists" || fail "eval-at.sh refuses a worktree path that exists"
ok "eval-at.sh refuses a label whose worktree exists"
rmdir "$run_dir/tmp/eval-busy"

# 8. eval-at.sh reuses a stored replay with the same inputs (`eval baseline`),
#    one whose commit has the same Go files as the sha first, says when the
#    code differs, and replays the rest; --fresh replays everything.
commit_on "" "go code" "x.go=package x"
go_a=$(git rev-parse --short HEAD)
commit_on "" "docs only" "docs/notes.md=text"
go_b=$(git rev-parse --short HEAD)
commit_on "" "go change" "x.go=package x // changed"
go_c=$(git rev-parse --short HEAD)
printf 'inputs\tabc123def456\nreuse\tcase-a\t20261007-100000\t%s\t1/2\nrun\tcase-b\n' "$go_a" >"$tmp/baseline"
export FAKE_BASELINE=$tmp/baseline
: >"$FAKE_MAGNUM_ARGS"
run "$scripts/eval-at.sh" "$go_b" reuse-docs case-a case-b
[[ $rc == 0 ]] && has "case-a: reused run 20261007-100000 (magnum $go_a, found 1/2): same skill, prompts, roles and Go code" ||
    fail "case-a is reused at a commit with the same Go files"
printf '%s\n' eval baseline --case case-a --case case-b eval run --case case-b --label reuse-docs >"$tmp/want"
cmp -s "$FAKE_MAGNUM_ARGS" "$tmp/want" || fail "only case-b is replayed"
ok "a stored replay with the same inputs is reused; a case with none is replayed"

: >"$FAKE_MAGNUM_ARGS"
run "$scripts/eval-at.sh" "$go_c" reuse-code case-a
printf '%s\n' eval baseline --case case-a >"$tmp/want"
[[ $rc == 0 ]] && has "case-a: reused run 20261007-100000 (magnum $go_a, found 1/2): same skill, prompts and roles, but other Go code than $go_c (--fresh replays)" &&
    has "every case reused" && cmp -s "$FAKE_MAGNUM_ARGS" "$tmp/want" || fail "a replay at other Go code is reused with a note"
ok "a stored replay whose commit has other Go files is reused and says so; nothing runs when every case is reused"

printf 'inputs\tabc123def456\nreuse\tcase-a\t20261008-090000\t%s\t2/2\nreuse\tcase-a\t20261007-100000\t%s\t1/2\nreuse\tcase-b\t20261007-100000\t-\t1/1\nrun\tcase-c\n' \
    "$go_c" "$go_a" >"$tmp/baseline"
run "$scripts/eval-at.sh" "$go_b" reuse-same case-a case-b
[[ $rc == 0 ]] && has "case-a: reused run 20261007-100000 (magnum $go_a" && has "case-b: reused run 20261007-100000 (magnum -, found 1/1): same skill, prompts and roles, but other Go code" ||
    fail "the newest replay at the same Go code wins over a newer one; an unknown commit is reused with the note"
ok "a replay at the same Go code wins over a newer one at other code; an unknown commit gets the note"

: >"$FAKE_MAGNUM_ARGS"
run "$scripts/eval-at.sh" --fresh "$go_b" fresh case-a
printf '%s\n' eval run --case case-a --label fresh >"$tmp/want"
[[ $rc == 0 ]] && cmp -s "$FAKE_MAGNUM_ARGS" "$tmp/want" || fail "--fresh replays without asking for a baseline"
ok "--fresh replays every case"
unset FAKE_BASELINE

echo "selftest: $checks checks passed"
