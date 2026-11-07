#!/usr/bin/env bash
# selftest.sh
#
# Tests the scripts here against throwaway git repositories in a temporary
# directory (under $TMPDIR), never the real repository, registry or daemon:
#   - bash -n on every script;
#   - land.sh: a conflict confined to docs/DECISIONS.md lands with both
#     entries whole and in order (above the Learned rules section when the
#     log ends with one), a commit that edits an old entry, writes into that
#     section or has another conflicted file stops, golden files name `go test ./<pkg> -update`, a
#     cherry-pick in progress is refused, --continue after a resolution by
#     hand picks the rest and skips the resolved commit, a re-pick never
#     duplicates an entry, and a migration the registry lacks is dry-run and
#     not built;
#   - restart.sh: its arguments and the drain it runs;
#   - eval-at.sh: its arguments, run directory, worktree, build and log, the
#     cleanup on a failure, the reuse of a stored replay with the same inputs
#     and Go files (and none with other Go files, an unknown commit or
#     --fresh), and the report with the points it prints;
#   - evidence.sh: against a registry with only the columns it reads, the rejected findings of claude-review
#     alone split into those matching a finding of its report and those matching a bullet of its Rejected
#     list (the self column and table), a nudge run whose twin is listed left out of the posted reviews, the
#     test-gap class per repository (its words matched in any case, a title's "(in the fix for" part not
#     read, the latest answer of each finding, the rounds of test gaps only), and the fixer trial line (the
#     replies to a fix run's own commits, those naming "also at", and the next round's tagged findings on the
#     answered round's paths, read from the round's first run when its nudge twin is the later one); against a
#     factory registry with only the columns it reads, its holds and alerts by their real kinds and phases, the time
#     per stage (after the publish left out), the waits on you by cause (a wait of several spans once), the take
#     rate of recommended answers by role, the liaison's notes (scrubbed, cut to their last lines), the factory's
#     own since (--factory-since: its tables, notes and report directories clipped to it), a since that is no time
#     refused, the report directories since a UTC since in any time zone, the scrub's patterns against the Go
#     files with a sample of each, a long dotted line scrubbed in seconds, and no notes read through a link, from a
#     file over 1 MiB or from a foreign folder.
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

# 4b. The log ends with the self-improving loop's `## Learned rules` section:
#     both sides insert their entries above it, and the landing keeps both
#     there, master's first, with the section as master has it (an entry the
#     loop added meanwhile included).
ops=$(<docs/DECISIONS.md)
section='## Learned rules

The loop appends these.'
commit_on "" "learned rules" "docs/DECISIONS.md=$ops

$section"
base=$(git rev-parse HEAD)
commit_on b6 "branch six" "docs/DECISIONS.md=$ops
- **Branch six.** text

$section" "b6.txt=b"
commit_on "" "master six" "docs/DECISIONS.md=$ops
- **Master six.** text

$section

- **A learned rule.** text"
printf '%s\n' "$ops" "- **Master six.** text" "- **Branch six.** text" "" "$section" "" "- **A learned rule.** text" >"$tmp/want"
run "$scripts/land.sh" b6
[[ $rc == 0 ]] || fail "land.sh b6 exits 0 (got $rc)"
cmp -s docs/DECISIONS.md "$tmp/want" || { diff "$tmp/want" docs/DECISIONS.md >&2 || true; fail "both entries above the Learned rules section, the section whole"; }
[[ -f b6.txt ]] && has "kept both sides of docs/DECISIONS.md" || fail "the commit landed with both sides of DECISIONS.md kept"
ok "entries both sides insert above the Learned rules section land with both kept there, master's first"
base=$(git rev-parse HEAD)
commit_on b7 "branch seven" "docs/DECISIONS.md=$(<docs/DECISIONS.md)
- **Into the section.** text"
commit_on "" "master seven" "docs/DECISIONS.md=+- **Another learned rule.** text"
before=$(git rev-parse HEAD)
run "$scripts/land.sh" b7
[[ $rc == 2 ]] && has "not merged by itself" || fail "a commit that writes into the Learned rules section stops with exit 2"
git cherry-pick --abort
[[ $(git rev-parse HEAD) == "$before" ]] || fail "nothing landed"
ok "a commit that writes into the Learned rules section is not merged by itself"

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

# 9. evidence.sh counts the rejected bullets of a claude-review report apart from the findings the judge
#    rejected, and leaves a nudge out of the posted reviews when the run it nudged is listed: a registry with
#    only the columns it reads, and one report.
ev=$tmp/ev
mkdir -p "$ev/reviews/example/app/1/abc123def456"
cat >"$ev/reviews/example/app/1/abc123def456/claude-review.md" <<'MD'
# Review of example/app#1

## 1. The cache key ignores the locale

- **Severity:** P2
- **Location:** `app/cache.rb:10`

A locale switch serves the page cached for the other language.

## Rejected

- The retry loop sleeps too long: no impact, the sleep is capped at one second.
- The token appears in the log: rejected, the log line holds only the id.
MD
# PR 2: a review whose two findings are test gaps (j1, with a nudge twin j1n that carries its review id and no
# findings, as a nudged round's runs do), a fix run that pushed nothing (fx0; its Needs you entry,
# written without its dash, starts with a comment id, which only the Pushed section would read as a commit) and one
# that pushed two commits (fx1), and the next round (j2), whose threads file holds the replies to fx1's commits (one
# naming "also at", one cut without the marker) and an older fix's reply.
mkdir -p "$ev/reviews/example/app/2/aaa111aaa111" "$ev/reviews/example/app/2/bbb222bbb222" "$ev/reviews/example/app/2/000000000000"
printf '%s\n' '<!-- magnum:run=fx0 -->' '## Pushed' 'nothing' '## Replied' '0 threads' '## Needs you' \
    '4100000102 P2 app/region.rb:8: not fixed. The region list is a product call.' \
    '## Other reviewers' 'none' >"$ev/reviews/example/app/2/000000000000/claude-fix.md"
printf '%s\n' '<!-- magnum:run=fx1 -->' '## Pushed' '1a2b3c4 Cover the locale guard (review comment 101)' \
    '5e6f7a8 Cover the region guard (review comment 102)' '## Replied' '2 threads (2 fixed)' '## Needs you' 'none' \
    '## Other reviewers' 'none' >"$ev/reviews/example/app/2/aaa111aaa111/claude-fix.md"
cat >"$ev/reviews/example/app/2/bbb222bbb222/review-threads.json" <<'JSON'
[
  {"id": "T1", "comment_id": 101, "url": "https://github.com/example/app/pull/2#discussion_r101", "finding": "**[P2] Regression test missing for the locale guard**",
   "location": "app/locale.rb:5", "review_id": 91, "created_at": "2026-10-11T10:00:00Z", "resolved": true, "outdated": false,
   "replies": [{"id": 201, "author": "alice", "class": "fixed",
     "body": "Fixed in 1a2b3c4: the locale guard has an example, also at app/locale.rb:20, `rspec spec/locale_spec.rb` passed\n\n<!-- magnum:fix -->"}]},
  {"id": "T2", "comment_id": 102, "url": "https://github.com/example/app/pull/2#discussion_r102", "finding": "**[P2] Region guard lacks regression coverage**",
   "location": "app/region.rb:8", "review_id": 91, "created_at": "2026-10-11T10:00:00Z", "resolved": true, "outdated": false,
   "replies": [{"id": 202, "author": "alice", "class": "fixed", "truncated": true,
     "body": "Fixed in 5e6f7a8: the region guard has an example, `rspec spec/region_spec.rb` passed"}]},
  {"id": "T3", "comment_id": 99, "url": "https://github.com/example/app/pull/2#discussion_r99", "finding": "**[P2] Old finding**",
   "location": "app/old.rb:1", "review_id": 90, "created_at": "2026-10-09T10:00:00Z", "resolved": true, "outdated": false,
   "replies": [{"id": 203, "author": "alice", "class": "fixed",
     "body": "Fixed in 9d8c7b6: an older fix, also at app/old.rb:4\n\n<!-- magnum:fix -->"}]}
]
JSON
sqlite3 "$ev/magnum.db" <<SQL
create table repos (id integer primary key, owner text, name text);
create table prs (id integer primary key, repo_id integer, number integer, gh_state text, state text, attempts integer, last_error text);
create table runs (id text primary key, pr_id integer, round integer, role text, kind text, state text, outcome text,
  report_path text, review_id integer, review_event text, review_url text, result_json text, prompt_text text default '',
  created_at text, submitted_at text, ended_at text);
create table findings (id integer primary key, run_id text, pr_id integer, round integer, finding_id text, severity text,
  path text, line integer, sources_json text, verdict text, reason_code text, created_at text, title text);
create table misses (class text, scope text, severity text, raised text, state text, created_at text, source_url text,
  title text, lesson text);
create table declines (id integer primary key, source_url text, finding_row_id integer, outcome text);
create table events (level text, kind text, at text);
create table requests (kind text, state text, created_at text);
create table kv (key text primary key, value text);
insert into repos values (1, 'example', 'app');
insert into prs values (1, 1, 1, 'OPEN', 'reviewed', 0, null), (2, 1, 2, 'OPEN', 'reviewed', 0, null);
insert into runs (id, pr_id, round, role, kind, state, outcome, report_path, review_id, review_event, created_at) values
  ('r1', 1, 1, 'codex-judge', 'initial', 'verified', 'posted', '$ev/reviews/example/app/1/abc123def456/codex-judge.json', 77, 'COMMENT', '2026-10-10T10:00:00Z'),
  ('r2', 1, 1, 'codex-judge', 'nudge', 'verified', 'posted', '$ev/reviews/example/app/1/abc123def456/codex-judge.json', 77, 'COMMENT', '2026-10-10T10:05:00Z'),
  ('r3', 1, 2, 'codex-judge', 'nudge', 'verified', 'posted', '$ev/reviews/example/app/1/abc123def456/codex-judge.json', 88, 'COMMENT', '2026-10-10T11:00:00Z'),
  ('fx0', 2, 1, 'claude-fix', 'fix', 'verified', 'needs_you', '$ev/reviews/example/app/2/000000000000/claude-fix.md', null, null, '2026-10-11T09:00:00Z'),
  ('j1', 2, 1, 'codex-judge', 'initial', 'verified', 'posted', '$ev/reviews/example/app/2/aaa111aaa111/codex-judge.json', 91, 'COMMENT', '2026-10-11T10:00:00Z'),
  ('j1n', 2, 1, 'codex-judge', 'nudge', 'verified', 'posted', '$ev/reviews/example/app/2/aaa111aaa111/codex-judge.json', 91, 'COMMENT', '2026-10-11T10:05:00Z'),
  ('fx1', 2, 1, 'claude-fix', 'fix', 'verified', 'pushed', '$ev/reviews/example/app/2/aaa111aaa111/claude-fix.md', null, null, '2026-10-11T10:30:00Z'),
  ('j2', 2, 2, 'codex-judge', 'rereview', 'verified', 'posted', '$ev/reviews/example/app/2/bbb222bbb222/codex-judge.json', 92, 'COMMENT', '2026-10-11T11:00:00Z');
insert into findings (id, run_id, pr_id, round, finding_id, severity, path, line, sources_json, verdict, created_at, title) values
  (11, 'j1', 2, 1, 'F1', 'P2', 'app/locale.rb', 5, '["codex-review"]', 'posted', '2026-10-11T10:00:00Z', 'Regression test missing for the locale guard'),
  (12, 'j1', 2, 1, 'F2', 'P2', 'app/region.rb', 8, '["codex-review"]', 'posted', '2026-10-11T10:00:00Z', 'Region guard lacks regression coverage'),
  (13, 'j2', 2, 2, 'F1', 'P2', 'app/locale.rb', 9, '["codex-review"]', 'posted', '2026-10-11T11:00:00Z', 'Locale fallback skips the guard (missed earlier)'),
  (14, 'j2', 2, 2, 'F2', 'P1', 'app/cache.rb', 3, '["codex-review"]', 'posted', '2026-10-11T11:00:00Z', 'Cache key drops the region (in the fix for Region guard lacks regression coverage)'),
  (15, 'j2', 2, 2, 'F3', 'P3', 'app/locale.rb', 12, '["codex-review"]', 'posted', '2026-10-11T11:00:00Z', 'Fallback specs stay green without the guard');
insert into declines (id, source_url, finding_row_id, outcome) values
  (1, 'https://github.com/example/app/pull/2#discussion_r201', 11, 'fixed'),
  (2, 'https://github.com/example/app/pull/2#discussion_r202', 12, 'fixed'),
  (3, 'https://github.com/example/app/pull/2#discussion_r204', 12, 'declined');
insert into findings (run_id, pr_id, round, finding_id, severity, path, line, sources_json, verdict, reason_code, created_at, title) values
  ('r1', 1, 1, 'F1', 'P2', 'app/cache.rb', 10, '["claude-review"]', 'posted', null, '2026-10-10T10:00:00Z', 'Cache key ignores the locale'),
  ('r1', 1, 1, 'F2', 'P3', 'app/retry.rb', 4, '["claude-review"]', 'rejected', 'speculative', '2026-10-10T10:00:00Z', 'Retry loop sleeps too long'),
  ('r1', 1, 1, 'F3', 'P1', 'app/log.rb', 9, '["claude-review"]', 'rejected', 'not_reproducible', '2026-10-10T10:00:00Z', 'Token appears in the log'),
  ('r1', 1, 1, 'F4', 'P2', 'app/cache.rb', 10, '["claude-review"]', 'rejected', 'duplicate', '2026-10-10T10:00:00Z', 'Cache key ignores the locale setting'),
  ('r1', 1, 1, 'F5', 'P3', 'app/other.rb', 3, '["codex-review"]', 'rejected', 'speculative', '2026-10-10T10:00:00Z', 'Retry loop sleeps too long');
SQL
MAGNUM_DB=$ev/magnum.db MAGNUM_REVIEWS=$ev/reviews run "$scripts/evidence.sh" 2026-10-01T00:00:00Z "$ev/out"
[[ $rc == 0 ]] || fail "evidence.sh runs against a registry with only the columns it reads (rc $rc)"
origin=$ev/out/claude-review-origin.txt
class_of() { awk -F'\t' -v t="$1" '$9 == t { print $2 }' "$origin"; }
[[ $(class_of "Retry loop sleeps too long") == self && $(class_of "Token appears in the log") == self ]] ||
    fail "a title that matches a bullet of the Rejected list is self"
[[ $(class_of "Cache key ignores the locale setting") == reported ]] || fail "a title that matches a finding of the report is reported"
[[ $(grep -c . "$origin") == 4 ]] || fail "only the rejected rows of claude-review alone are listed (not the posted one, not codex-review's)"
ev_md=$ev/out/evidence.md
# The tables are sqlite3's markdown mode, whose column padding differs between sqlite versions (the CI runner's
# sqlite3 pads numbers on the other side than a current one), so each row is compared with its spaces squeezed.
rows=$(tr -s ' ' <"$ev_md")
grep -Fxq '| rejected | speculative | P3 | 2 | 1 |' <<<"$rows" &&
    grep -Fxq '| rejected | not_reproducible | P1 | 1 | 1 |' <<<"$rows" &&
    grep -Fxq '| rejected | duplicate | P2 | 1 | 0 |' <<<"$rows" ||
    fail "the findings table counts the self rows apart in its self column: $(sed -n '/^## Findings/,/^## Posted findings/p' "$ev_md" | tr -s ' ' | tr '\n' ';')"
grep -Fxq '| P2 | 1 | 1 | 0 | 0 | 0 |' <<<"$rows" &&
    grep -Fxq '| P3 | 0 | 0 | 1 | 0 | 0 |' <<<"$rows" ||
    fail "the claude-review table splits posted, reported, self and unmatched: $(sed -n '/^## claude-review alone/,/^## Role minutes/p' "$ev_md" | tr -s ' ' | tr '\n' ';')"
sed -n '/^## Posted reviews/,/^## Findings/p' "$ev_md" >"$tmp/out"
[[ $(grep -c '^| 2026.* example/app#1 ' "$tmp/out") == 2 && $(grep -c '| nudge ' "$tmp/out") == 1 ]] ||
    fail "the nudge whose twin is listed has no row; the one whose review id is its own stays"
ok "evidence.sh counts a claude-review report's own Rejected bullets apart and leaves out a nudge whose twin is listed"

# PR 1's posted finding is no test gap; of PR 2's five, the two of its first review (one opening with "Regression",
# the case of many real titles, so the words match the lowered title) and the one whose specs stay green are, and
# the one whose "(in the fix for" part names a test gap is not. Of those three, one was fixed, one
# fixed and then declined (the latest answer counts) and one has no answer; j1 posted only test gaps.
grep -Fxq '| example/app | 2 | 6 | 1 | 3 | 2/3 | 1 | 1 |' <<<"$rows" ||
    fail "the test-gap row per repository: $(sed -n '/^## Recurring posted classes/,/^## Fixer trial/p' "$ev_md" | tr -s ' ' | tr '\n' ';')"
grep -Fxq '| example/app | fixed | 1 |' <<<"$rows" && grep -Fxq '| example/app | declined | 1 |' <<<"$rows" &&
    grep -Fxq '| example/app | no answer | 1 |' <<<"$rows" ||
    fail "the answers to the test gaps: $(sed -n '/^## Recurring posted classes/,/^## Fixer trial/p' "$ev_md" | tr -s ' ' | tr '\n' ';')"
ok "evidence.sh counts the test-gap class per repository, its first-review share, its answers and its rounds"
# fx0 pushed nothing; fx1's two commits have a reply each in j2's threads file (the one cut without the marker
# too), the older fix's reply is not fx1's, and of j2's three findings one is missed earlier on a path j1 named
# (j1n, the run before fx1, is j1's twin and names no path of its own) and one is in the fix for another, on a
# path j1 did not name.
grep -Fxq 'Fixer trial: 2 fix runs, 1 pushed. Fixed-in replies to their commits: 2, 1 naming "also at". The next posted rounds (1): 3 findings, 1 "(missed earlier)" and 1 "(in the fix for", 1 of those on a path a finding of the answered round named. Without a posted round after them: 0.' "$ev_md" ||
    fail "the fixer trial line: $(grep '^Fixer trial' "$ev_md" || echo none)"
grep -Fxq '| example/app#2 | fx1 | 2026-10-11T10:30 | 2 | 2 | 1 | j2 (r2) | 3 | 1 | 1 | 1 |' "$ev_md" ||
    fail "the fixer trial's row of fx1: $(sed -n '/^## Fixer trial/,/^## Role minutes/p' "$ev_md" | tr '\n' ';')"
! grep -q '^| example/app#2 | fx0 ' "$ev_md" || fail "a fix run that pushed nothing has no row"
ok "evidence.sh's fixer trial counts the replies to a fix run's commits, \"also at\" and the next round's tagged findings"

# 10. evidence.sh reads the factory's registry and its tasks' local review rounds: the PRs the factory published
# (whose comments by people are misses of its local review) and the rounds' report directories; without a factory
# registry it writes no factory section.
fdb=$ev/factory.db
sqlite3 "$fdb" <<'SQL'
create table tasks (id integer primary key, jira_key text, repo text, kind text, state text, stage text, pr_number integer,
  created_at text, updated_at text);
create table pr_links (task_id integer, repo text, number integer, opened_head text, opened_at text);
create table review_cycles (task_id integer, n integer, exit text, head_after text, tokens integer, started_at text);
create table review_findings (task_id integer, priority text, class text, verdict text, outcome text, blocking integer,
  first_cycle integer);
create table events (id integer primary key, at text, task_id integer, kind text, phase text, text text);
create table operator_inputs (at text, kind text, surface text);
create table task_spans (id integer primary key, task_id integer, to_state text, stage text, waits_who text, waits_what text,
  on_you integer, at text);
create table questions (id integer primary key, task_id integer, raised_by text, state text, recommended integer,
  took_recommendation integer, flag text, asked_at text, answered_at text);
create table decisions (id integer primary key, task_id integer, kind text, choice text, recommendation_shown integer,
  recommendation_followed integer, at text);
insert into tasks values (7, 'PR-12345', 'example/app', 'bug', 'published', 'publish', 4512, '2026-10-11T09:00:00Z', '2026-10-11T12:00:00Z'),
  (8, '', 'example/app', 'feature', 'needs_you', 'local_review', 0, '2026-10-11T08:00:00Z', '2026-10-11T09:45:00Z');
insert into pr_links values (7, 'example/app', 4512, 'abc123abc123abc1', '2026-10-11T11:00:00Z');
insert into review_cycles values (7, 1, 'clean', 'abc123abc123abc1', 0, '2026-10-11T10:00:00Z');
insert into review_findings values (7, 'P3', 'simplification', 'posted', 'open', 0, 1);
insert into events (at, task_id, kind, phase, text) values
  ('2026-10-11T09:30:00Z', 7, 'claim', 'fail', 'the copy is busy'),
  ('2026-10-11T09:51:00Z', null, 'hold', 'begin', 'infra: MySQL does not answer; every stage holds; tries again at 10:05'),
  ('2026-10-11T09:52:00Z', 8, 'alert', 'info', 'task 8: you: 3 questions before judge goes on'),
  ('2026-10-11T09:53:00Z', 8, 'alert_held', 'info', 'task 8 questions: held, you are on the screen'),
  ('2026-10-11T09:54:00Z', null, 'intake', 'info', 'an info event of another kind'),
  ('2026-10-11T09:55:00Z', null, 'hold', 'ok', 'infra: lifted after 4 minutes (MySQL does not answer)');
insert into operator_inputs values ('2026-10-11T09:40:00Z', 'answer', 'screen');
-- Task 7 went from intake through coding (a precheck wait, then one on you) to its ready card and the publish; task 8
-- waited on you twice at its questions (one wait in two spans), at the judge's questions, and waits on you now.
insert into task_spans (task_id, to_state, stage, waits_who, waits_what, on_you, at) values
  (7, 'active', '', '', '', 0, '2026-10-11T09:00:00Z'),
  (7, 'active', 'coding', '', '', 0, '2026-10-11T09:10:00Z'),
  (7, 'active', 'coding', 'precheck', 'precheck: specs', 0, '2026-10-11T09:30:00Z'),
  (7, 'needs_you', 'coding', 'you', 'precheck: precheck.lint `bin/lint` exited 1', 1, '2026-10-11T09:40:00Z'),
  (7, 'active', 'coding', '', '', 0, '2026-10-11T09:50:00Z'),
  (7, 'needs_you', 'ready', 'you', 'ready to push (card 1): PR-12345 a title', 1, '2026-10-11T10:00:00Z'),
  (7, 'active', 'publish', '', '', 0, '2026-10-11T10:30:00Z'),
  (7, 'published', 'publish', '', '', 0, '2026-10-11T11:00:00Z'),
  (8, 'active', '', '', '', 0, '2026-10-11T08:00:00Z'),
  (8, 'needs_you', 'questions', 'you', '2 questions before coding', 1, '2026-10-11T08:20:00Z'),
  (8, 'needs_you', 'questions', 'you', '1 question before coding', 1, '2026-10-11T08:50:00Z'),
  (8, 'active', 'judge', '', '', 0, '2026-10-11T09:00:00Z'),
  (8, 'needs_you', 'judge', 'you', '3 questions before judge goes on', 1, '2026-10-11T09:20:00Z'),
  (8, 'active', 'local_review', '', '', 0, '2026-10-11T09:35:00Z'),
  (8, 'needs_you', 'local_review', 'you', 'round 1 of the local review left 1 tracked change in the copy (Gemfile.lock)', 1,
   '2026-10-11T09:45:00Z');
insert into questions (task_id, raised_by, state, recommended, took_recommendation, flag, asked_at, answered_at) values
  (7, 'fx-judge', 'answered', 0, 1, '', '2026-10-11T09:20:00Z', '2026-10-11T09:25:00Z'),
  (7, 'fx-judge', 'answered', 1, 1, '', '2026-10-11T09:20:00Z', '2026-10-11T09:25:00Z'),
  (7, 'fx-judge', 'answered', 0, 0, '', '2026-10-11T09:20:00Z', '2026-10-11T10:05:00Z'),
  (7, 'fx-judge', 'answered', -1, 0, '', '2026-10-11T09:20:00Z', '2026-10-11T09:25:00Z'),
  (7, 'fx-judge', 'voided', 0, 0, '', '2026-10-11T09:20:00Z', null),
  (8, 'fx-points', 'answered', 2, 1, 'needless', '2026-10-11T08:20:00Z', '2026-10-11T08:40:00Z');
insert into decisions (task_id, kind, choice, recommendation_shown, recommendation_followed, at) values
  (7, 'ready_push', 'push', 0, 0, '2026-10-11T10:29:00Z');
SQL
round=$ev/factory-tasks/7/local-review/1-local-aaa/reviews/example/app/900071/abc123abc123
mkdir -p "$round" "$ev/factory-tasks/8"
TZ=UTC touch -t 202610111000 "$round"
# Task 7's liaison notes: 85 lines, the last three with a secret, a password and an email; task 8's are older.
{
    for i in $(seq 1 82); do echo "- 2026-10-11 10:05 filler note $i"; done
    echo '- 2026-10-11 10:10 the operator asked back what "precheck" means (report.md)'
    echo '- 2026-10-11 10:11 a token ghp_abcdefghijklmnopqrstuvwxyz0123456789 and password=hunter2 in precheck.md'
    echo '- 2026-10-11 10:12 mail alice@example.com about it'
} >"$ev/factory-tasks/7/notes.md"
echo '- 2026-10-11 09:00 an older note' >"$ev/factory-tasks/8/notes.md"
TZ=UTC touch -t 202610110900 "$ev/factory-tasks/8/notes.md"
# Task 9's notes hold one sample of every pattern of the factory's scrub (execx.Redact and runtime.Scrub, most of them
# from those files' Go tests; a key shape a scanner knows is built from two pieces) and a dotted line of 20,000
# characters. Task 10's notes.md links to a file outside, task 11 is a linked folder, task 12's notes.md is over
# 1 MiB, and a folder named with a non-ASCII digit lies beside them.
mkdir -p "$ev/factory-tasks/9" "$ev/factory-tasks/10" "$ev/factory-tasks/12" "$ev/outside/11" "$ev/factory-tasks/²"
jwt="eyJhbGciOiJIUzI1NiJ9"".eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
leaks=(ghs_abcDEF123_x eyJzdWIiOiIxMjM0NTY3ODkwIn0 opaqueTOKEN123 dXNlcjpodW50ZXIy MIIEsecretbody MIIEtruncat
    AKIAABCDEFGHIJKLMNOP xoxb-123456789012-abcdefghijkl sk-abcdefghijklmnopqrstuvwxyz123456
    "sk-""ant-abcdefghijklmnopqrstuv" glpat-abcdefghij0123456789 "rk_""live_abcdefghij0123456789"
    "AI""zaSyA-abcdefghijklmnopqrstuvwxyz01234" wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY hunter3 s3cr3t-value
    bob@example.org db1.internal.example.com 10.0.4.12)
{
    echo "- token ${leaks[0]} here, and a JWT $jwt"
    echo "- Authorization: Bearer ${leaks[2]}, then Authorization: Basic ${leaks[3]}"
    echo "- ${leaks[6]} and aws_secret_access_key=${leaks[13]}"
    echo "- token ${leaks[7]} and ${leaks[8]} and ${leaks[9]} and ${leaks[10]} and ${leaks[11]} and ${leaks[12]}"
    echo "- password=${leaks[14]} client_secret: \"${leaks[15]}\""
    echo "- mail ${leaks[16]} about ${leaks[17]} and ${leaks[18]}"
    printf -- '- %s\n' "$(printf 'a.%.0s' $(seq 1 10000))"
    printf '%s\n' '-----BEGIN RSA PRIVATE KEY-----' "${leaks[4]}" '-----END RSA PRIVATE KEY-----' '- the cut key follows'
    printf '%s\n' '-----BEGIN EC PRIVATE KEY-----' "${leaks[5]}"
} >"$ev/factory-tasks/9/notes.md"
echo '- LINKED-FILE-NOTE' >"$ev/outside/notes.md"
ln -s "$ev/outside/notes.md" "$ev/factory-tasks/10/notes.md"
echo '- LINKED-FOLDER-NOTE' >"$ev/outside/11/notes.md"
ln -s "$ev/outside/11" "$ev/factory-tasks/11"
{ echo '- BIG-NOTE-LINE'; head -c 1100000 /dev/zero | tr '\0' 'x'; echo; } >"$ev/factory-tasks/12/notes.md"
echo '- FOREIGN-FOLDER-NOTE' >"$ev/factory-tasks/²/notes.md"
started=$SECONDS
MAGNUM_DB=$ev/magnum.db MAGNUM_REVIEWS=$ev/reviews MAGNUM_FACTORY_DB=$fdb MAGNUM_FACTORY_TASKS=$ev/factory-tasks \
    run "$scripts/evidence.sh" 2026-10-01T00:00:00Z "$ev/fout"
run1_secs=$((SECONDS - started))
[[ $rc == 0 ]] || fail "evidence.sh runs against a factory registry with only the columns it reads (rc $rc)"
grep -q '^## Factory PRs' "$ev/fout/evidence.md" && tr -s ' ' <"$ev/fout/evidence.md" | grep -q '| 7 | example/app#4512 |' ||
    fail "the factory section lists the PR the task published: $(sed -n '/^## Factory PRs/,/^## Factory local/p' "$ev/fout/evidence.md" | tr '\n' ';')"
[[ $(cat "$ev/fout/factory-reports.txt") == "$round" ]] || fail "factory-reports.txt lists the local review round's report directory"
MAGNUM_DB=$ev/magnum.db MAGNUM_REVIEWS=$ev/reviews MAGNUM_FACTORY_DB=$ev/none.db MAGNUM_FACTORY_TASKS=$ev/none \
    run "$scripts/evidence.sh" 2026-10-01T00:00:00Z "$ev/nout"
[[ $rc == 0 ]] && ! grep -q '^## Factory' "$ev/nout/evidence.md" || fail "without a factory registry there is no factory section (rc $rc)"
ok "evidence.sh lists the factory's tasks, the PRs they published and their local review rounds' reports"

# 11. The factory's holds and alerts are events of kind hold and alert* (phases begin, ok, info): they show beside the
#     failures, and an info event of another kind does not.
frows=$(tr -s ' ' <"$ev/fout/evidence.md")
fsect() { sed -n "/^## $1/,/^## /p" "$2" | tr -s ' ' | tr '\n' ';'; }
grep -Fxq '| hold | begin | 1 | 2026-10-11T09:51 |' <<<"$frows" && grep -Fxq '| hold | ok | 1 | 2026-10-11T09:55 |' <<<"$frows" &&
    grep -Fxq '| alert | info | 1 | 2026-10-11T09:52 |' <<<"$frows" && grep -Fxq '| alert_held | info | 1 | 2026-10-11T09:53 |' <<<"$frows" &&
    grep -Fxq '| claim | fail | 1 | 2026-10-11T09:30 |' <<<"$frows" && ! grep -q '^| intake | info ' <<<"$frows" ||
    fail "the factory's waits and failures list its holds, alerts and failures: $(fsect 'Factory waits and failures' "$ev/fout/evidence.md")"
grep -Fxq '| 2026-10-11T09:51 | begin | infra: MySQL does not answer; every stage holds; tries again at 10:05 |' <<<"$frows" &&
    grep -Fxq '| 2026-10-11T09:55 | ok | infra: lifted after 4 minutes (MySQL does not answer) |' <<<"$frows" ||
    fail "the factory's holds are listed with their words: $(fsect 'Factory holds' "$ev/fout/evidence.md")"
ok "evidence.sh shows the factory's holds and alerts with the registry's own kinds and phases"

# 12. The time per stage: each span runs to the next span of its task, the spans after the publish are left out, and the
#     time splits into at work, on you and on others (named with their minutes).
grep -Fxq '| - | 2 | 30.0 | 30.0 | 0.0 | 0.0 | - |' <<<"$frows" &&
    grep -Fxq '| questions | 1 | 40.0 | 0.0 | 40.0 | 0.0 | - |' <<<"$frows" &&
    grep -Fxq '| judge | 1 | 35.0 | 20.0 | 15.0 | 0.0 | - |' <<<"$frows" &&
    grep -Fxq '| coding | 1 | 50.0 | 30.0 | 10.0 | 10.0 | precheck 10.0 |' <<<"$frows" &&
    grep -Fxq '| ready | 1 | 30.0 | 0.0 | 30.0 | 0.0 | - |' <<<"$frows" &&
    grep -Fxq '| publish | 1 | 30.0 | 30.0 | 0.0 | 0.0 | - |' <<<"$frows" && grep -q '^| local_review | 1 | ' <<<"$frows" ||
    fail "the factory's time per stage: $(fsect 'Factory time per stage' "$ev/fout/evidence.md")"
ok "evidence.sh sums the factory's time per stage: at work, on you and on others by who"

# 13. The waits on you by cause: one task's consecutive spans of one cause and stage are one wait, the cause is the
#     wait's words as the alerts shorten them (numbers as N), and a wait that still runs is open.
grep -Fxq '| questions | questions, judge | 2 | 1 | 55.0 | 40.0 | 0 |' <<<"$frows" &&
    grep -Fxq '| ready to push | ready | 1 | 1 | 30.0 | 30.0 | 0 |' <<<"$frows" &&
    grep -Fxq '| precheck | coding | 1 | 1 | 10.0 | 10.0 | 0 |' <<<"$frows" &&
    grep -q '^| round N of the local review left N tracked change in the copy | local_review | 1 | 1 | .* | 1 |$' <<<"$frows" ||
    fail "the factory's waits on you by cause: $(fsect 'Factory waits on you' "$ev/fout/evidence.md")"
ok "evidence.sh counts the factory's waits on you by cause, a wait of several spans once"

# 14. The take rate of recommended answers by role, of the questions asked or answered in the window, and the cards'.
grep -Fxq '| fx-judge | 5 | 4 | 3 | 2 | 67% | 1 | 0 |' <<<"$frows" &&
    grep -Fxq '| fx-points | 1 | 1 | 1 | 1 | 100% | 0 | 1 |' <<<"$frows" && grep -Fxq '| all | 6 | 5 | 4 | 3 | 75% | 1 | 1 |' <<<"$frows" ||
    fail "the take rate by role: $(fsect 'Factory questions' "$ev/fout/evidence.md")"
grep -Fq 'Card decisions: 1 (ready_push push 1); a recommendation was shown on 0 and followed on 0.' "$ev/fout/evidence.md" ||
    fail "the card decisions' line: $(fsect 'Factory questions' "$ev/fout/evidence.md")"
ok "evidence.sh gives the take rate of the recommended answers by the role that raised the question"

# 15. The liaison's notes of each task folder changed in the window, scrubbed as the factory scrubs what it keeps, the
#     last 80 lines of each.
f_md=$ev/fout/evidence.md
grep -Fxq '### Task 7 (PR-12345, example/app)' "$f_md" && grep -Fxq '### Task 8 (task 8, example/app)' "$f_md" &&
    grep -Fxq '> - 2026-10-11 10:10 the operator asked back what "precheck" means (report.md)' "$f_md" &&
    grep -Fxq '> - 2026-10-11 10:11 a token <redacted> and password=<redacted> in precheck.md' "$f_md" &&
    grep -Fxq '> - 2026-10-11 10:12 mail <email> about it' "$f_md" && grep -Fq '(5 earlier lines left out)' "$f_md" &&
    ! grep -Fxq '> - 2026-10-11 10:05 filler note 5' "$f_md" && grep -Fxq '> - 2026-10-11 10:05 filler note 6' "$f_md" ||
    fail "the liaison's notes: $(fsect 'Factory liaison notes' "$f_md")"
! grep -Eq 'ghp_|hunter2|alice@' "$f_md" || fail "evidence.md holds no secret, password or email of the notes"
ok "evidence.sh prints the liaison's notes, scrubbed and cut to their last lines"

# 16. The factory has a since of its own (--factory-since): its tables and factory-reports.txt read from it, clipped to
#     it, while the review's tables read from the review's since; a since that is no time is refused.
MAGNUM_DB=$ev/magnum.db MAGNUM_REVIEWS=$ev/reviews MAGNUM_FACTORY_DB=$fdb MAGNUM_FACTORY_TASKS=$ev/factory-tasks \
    run "$scripts/evidence.sh" --factory-since 2026-10-11T09:45:00Z 2026-11-01T00:00:00Z "$ev/f2out"
[[ $rc == 0 ]] || fail "evidence.sh --factory-since runs (rc $rc)"
f2=$ev/f2out/evidence.md
f2rows=$(tr -s ' ' <"$f2")
grep -q '^# magnum since 2026-11-01T00:00:00Z' "$f2" && grep -q '^## Factory tasks (updated since 2026-10-11T09:45:00Z)' "$f2" ||
    fail "the review's tables read from its since, the factory's from --factory-since: $(grep '^#' "$f2" | tr '\n' ';')"
[[ $(cat "$ev/f2out/factory-reports.txt") == "$round" ]] ||
    fail "factory-reports.txt reads from the factory's since"
grep -Fxq '| coding | 1 | 15.0 | 10.0 | 5.0 | 0.0 | - |' <<<"$f2rows" && grep -Fxq '| precheck | coding | 1 | 1 | 5.0 | 5.0 | 0 |' <<<"$f2rows" &&
    ! grep -q '^| questions |' <<<"$f2rows" ||
    fail "the stage times and waits are clipped to the factory's since: $(fsect 'Factory time per stage' "$f2")"
grep -Fxq '| fx-judge | 1 | 1 | 1 | 0 | 0% | 0 | 0 |' <<<"$f2rows" && grep -Fxq '| all | 1 | 1 | 1 | 0 | 0% | 0 | 0 |' <<<"$f2rows" ||
    fail "the take rate counts the questions answered since the factory's since: $(fsect 'Factory questions' "$f2")"
grep -Fxq '### Task 7 (PR-12345, example/app)' "$f2" && ! grep -q '^### Task 8 ' "$f2" ||
    fail "only the notes changed since the factory's since are listed: $(fsect 'Factory liaison notes' "$f2")"
MAGNUM_DB=$ev/magnum.db MAGNUM_REVIEWS=$ev/reviews MAGNUM_FACTORY_DB=$fdb MAGNUM_FACTORY_TASKS=$ev/factory-tasks \
    run "$scripts/evidence.sh" --factory-since 3d 2026-10-01T00:00:00Z "$ev/f3out"
[[ $rc == 0 ]] && grep -Eq '^## Factory tasks \(updated since [0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:]{8}Z\)' "$ev/f3out/evidence.md" ||
    fail "--factory-since 3d is three days ago (rc $rc)"
MAGNUM_DB=$ev/magnum.db MAGNUM_REVIEWS=$ev/reviews run "$scripts/evidence.sh" --factory-since yesterday 2026-10-01T00:00:00Z "$ev/f4out"
[[ $rc == 2 ]] && has "is not a since" && [[ ! -e $ev/f4out ]] || fail "a factory since that is no time is refused (rc $rc)"
MAGNUM_DB=$ev/magnum.db MAGNUM_REVIEWS=$ev/reviews run "$scripts/evidence.sh" "2026-10-01'" "$ev/f5out"
[[ $rc == 2 ]] && has "is not a since" || fail "a since that is no time is refused (rc $rc)"
ok "evidence.sh reads the factory from a since of its own, --factory-since, and refuses a since that is no time"

# 17. A since is UTC in any time zone: on a machine nine hours east of UTC a report directory or a local review round
#     45 minutes older than the since is not listed, and one 15 minutes newer is.
early=$ev/factory-tasks/8/local-review/1-local-bbb/reviews/example/app/900081/def456def456
mkdir -p "$early"
TZ=UTC touch -t 202610110900 "$early"
for d in "$ev"/reviews/example/app/*/*; do TZ=UTC touch -t 202610110900 "$d"; done
TZ=UTC touch -t 202610111000 "$ev/reviews/example/app/2/bbb222bbb222"
TZ=Asia/Tokyo MAGNUM_DB=$ev/magnum.db MAGNUM_REVIEWS=$ev/reviews MAGNUM_FACTORY_DB=$fdb MAGNUM_FACTORY_TASKS=$ev/factory-tasks \
    run "$scripts/evidence.sh" --factory-since 2026-10-11T09:45:00Z 2026-10-11T09:45:00Z "$ev/f6out"
[[ $rc == 0 && $(cat "$ev/f6out/reports.txt") == "$ev/reviews/example/app/2/bbb222bbb222" &&
    $(cat "$ev/f6out/factory-reports.txt") == "$round" ]] ||
    fail "the report directories since a UTC since, in Asia/Tokyo: $(tr '\n' ';' <"$ev/f6out/reports.txt") / $(tr '\n' ';' <"$ev/f6out/factory-reports.txt")"
ok "evidence.sh lists the report directories since its UTC since in any time zone"

# 18. The scrub of the notes is the factory's own: every pattern of execx.Redact and runtime.Scrub stands in
#     evidence.sh word for word (Go's end of text, $, is Python's \Z), and no sample of them is left in evidence.md.
repo_root=$(cd "$here/../../../.." && pwd)
run python3 - "$repo_root/internal/execx/execx.go" "$repo_root/internal/factory/runtime/scrub.go" "$scripts/evidence.sh" <<'PY'
import re, sys

*gos, port = sys.argv[1:]
text = open(port).read()
pats = []
for g in gos:
    src = open(g).read()
    for m in re.finditer(r"regexp\.MustCompile\(", src):
        i, depth, raw = m.end(), 1, False
        while depth:
            c = src[i]
            raw = not raw if c == "`" else raw
            depth += 0 if raw else (c == "(") - (c == ")")
            i += 1
        pats += re.findall(r"`([^`]*)`", src[m.end():i])
missing = [p for p in pats if p.replace("|$)", r"|\Z)") not in text]
print("%d patterns of the Go scrub, missing from evidence.sh: %s" % (len(pats), missing))
sys.exit(1 if len(pats) < 13 or missing else 0)
PY
[[ $rc == 0 ]] || fail "evidence.sh holds every pattern of the factory's scrub"
for leak in "${leaks[@]}"; do
    ! grep -Fq -- "$leak" "$ev/fout/evidence.md" || fail "the notes' sample $leak is scrubbed: $(grep -F -- "$leak" "$ev/fout/evidence.md" | cut -c1-200)"
done
grep -Fxq '### Task 9 (task 9, -)' "$ev/fout/evidence.md" || fail "task 9's notes are listed"
ok "evidence.sh scrubs the notes with every pattern of the factory's scrub, and no sample of them is left"

# 19. A long dotted line, which Python's regex engine (unlike Go's) backtracks on for minutes, is scrubbed in seconds.
[[ $run1_secs -lt 20 ]] || fail "evidence.sh with a 20,000-character dotted line in the notes took ${run1_secs}s"
grep -Fxq '> - <host>' "$ev/fout/evidence.md" || fail "the long dotted line is masked whole: $(grep -c '^> - a\.a\.' "$ev/fout/evidence.md")"
ok "evidence.sh scrubs a 20,000-character dotted line in the notes in ${run1_secs}s"

# 20. The notes are read only from a plain notes.md of 1 MiB at most, in a task folder that is no link and is named
#     with ASCII digits.
f_md=$ev/fout/evidence.md
! grep -Eq 'LINKED-FILE-NOTE|LINKED-FOLDER-NOTE|BIG-NOTE-LINE|FOREIGN-FOLDER-NOTE' "$f_md" && ! grep -q '^### Task 11 ' "$f_md" &&
    grep -A2 -Fx '### Task 10 (task 10, -)' "$f_md" | grep -Fxq '(notes.md is not a plain file: left out)' &&
    grep -A2 -Fx '### Task 12 (task 12, -)' "$f_md" | grep -Fxq '(notes.md is over 1 MiB: left out)' &&
    grep -Fxq '### Task 7 (PR-12345, example/app)' "$f_md" ||
    fail "the notes skip links, files over 1 MiB and foreign folders: $(fsect 'Factory liaison notes' "$f_md" | cut -c1-600)"
ok "evidence.sh reads no notes.md that is a link or over 1 MiB, nor one in a linked or foreign folder"


echo "selftest: $checks checks passed"
