#!/usr/bin/env bash
# eval-at.sh [--run-dir DIR] [--fresh] <sha> <label> <case>...
#
# Measures the review skill and prompts of one commit with `magnum eval run`,
# the before/after proof of a skill or prompt change: a detached worktree of
# this repository at <sha> under <run-dir>/tmp/eval-<label-slug>, bin/magnum
# built there (a binary reads the prompts and the skill of its own checkout),
# and only the cases given, the ones the change targets. A case that a stored
# run already replayed with the same inputs is reused, not replayed:
# `bin/magnum eval baseline` names the stored replays of each case at the head
# the corpus pins whose skill, prompts and role config hash (run.json's
# inputs) match; the newest whose magnum commit has the same Go files as <sha>
# stands for this one (0 points), else the newest, with a note that magnum's
# code differs. The inputs leave the code out: measure a code change with
# --fresh, which replays every case. The rest
# run with `bin/magnum eval run --case <case>... --label <label>`, output in
# <run-dir>/tmp/eval-<label-slug>.log; the report it ends with is printed,
# with the Codex points each case used. The worktree is removed at the end,
# also on a failure or an interrupt. The run directory is --run-dir, else
# $MAGNUM_IMPROVE_RUN. The results stay where `magnum eval list` finds them.
# Each replayed case costs a review round of Codex usage; run it in the
# background.
set -euo pipefail

usage="usage: eval-at.sh [--run-dir DIR] [--fresh] <sha> <label> <case>..."
run_dir=${MAGNUM_IMPROVE_RUN:-}
fresh=0
sha=
label=
names=()
while (($#)); do
    case $1 in
    --run-dir) [[ $# -ge 2 ]] || { echo "$usage" >&2; exit 2; }; run_dir=$2; shift 2 ;;
    --run-dir=*) run_dir=${1#*=}; shift ;;
    --fresh) fresh=1; shift ;;
    -h | --help) echo "$usage"; exit 0 ;;
    -*) echo "eval-at: unknown flag $1" >&2; echo "$usage" >&2; exit 2 ;;
    *)
        if [[ -z $sha ]]; then
            sha=$1
        elif [[ -z $label ]]; then
            label=$1
        else
            names+=("$1")
        fi
        shift
        ;;
    esac
done
((${#names[@]} > 0)) || { echo "$usage" >&2; exit 2; }
[[ -n $run_dir ]] || { echo "eval-at: no run directory: pass --run-dir DIR or set MAGNUM_IMPROVE_RUN" >&2; exit 2; }
[[ -d $run_dir ]] || { echo "eval-at: the run directory $run_dir does not exist" >&2; exit 2; }
slug=$(printf '%s' "$label" | tr 'A-Z' 'a-z' | tr -cs 'a-z0-9' '-' | sed -e 's/^-*//' -e 's/-*$//')
[[ -n $slug ]] || { echo "eval-at: the label needs a letter or a digit" >&2; exit 2; }

repo=$(cd "$(dirname "$0")/../../../.." && pwd)
commit=$(git -C "$repo" rev-parse -q --verify "$sha^{commit}") || { echo "eval-at: $sha is not a commit of $repo" >&2; exit 2; }
run_dir=$(cd "$run_dir" && pwd)
wt=$run_dir/tmp/eval-$slug
log=$run_dir/tmp/eval-$slug.log
[[ ! -e $wt ]] || { echo "eval-at: $wt exists (an eval with this label runs or was cut short); pick another label, or remove it with git worktree remove --force" >&2; exit 2; }
mkdir -p "$run_dir/tmp"

cleanup() {
    git -C "$repo" worktree remove --force "$wt" >/dev/null 2>&1 || rm -rf "$wt"
    git -C "$repo" worktree prune
}
trap cleanup EXIT
trap 'exit 130' INT TERM HUP
git -C "$repo" worktree add --quiet --detach "$wt" "$commit"
short=$(git -C "$wt" rev-parse --short HEAD)
echo "eval-at: \"$label\" at $short, ${#names[@]} case(s) (log: $log)"

# The checkout's mise configuration names the Go version; the worktree's copy
# of it is the repository's own, trusted for this build only.
if ! (cd "$wt" && MISE_TRUSTED_CONFIG_PATHS="$wt${MISE_TRUSTED_CONFIG_PATHS:+:$MISE_TRUSTED_CONFIG_PATHS}" \
    mise exec -- go build -o bin/magnum ./cmd/magnum) >"$log" 2>&1; then
    echo "eval-at: the build failed at $short:" >&2
    tail -20 "$log" >&2
    exit 1
fi

# same_code <commit>: magnum at <commit> has the Go files of <sha>, so a
# replay it made with the same inputs reviewed as <sha> would ("-": unknown).
same_code() {
    [[ $1 != - ]] && git -C "$repo" rev-parse -q --verify "$1^{commit}" >/dev/null &&
        git -C "$repo" diff --quiet "$1" "$commit" -- '*.go' go.mod go.sum
}

todo=()
if ((fresh)); then
    todo=("${names[@]}")
else
    baseline_args=()
    for name in "${names[@]}"; do baseline_args+=(--case "$name"); done
    if ! baseline=$(cd "$wt" && bin/magnum eval baseline "${baseline_args[@]}" 2>>"$log"); then
        echo "eval-at: magnum eval baseline failed; the end of $log:" >&2
        tail -n 20 "$log" >&2
        exit 1
    fi
    for name in "${names[@]}"; do
        reused=
        while IFS=$'\t' read -r kind case_name run_id run_commit score; do
            [[ $kind == reuse && $case_name == "$name" ]] || continue
            if same_code "$run_commit"; then
                reused="run $run_id (magnum $run_commit, found $score): same skill, prompts, roles and Go code"
                break
            fi
            [[ -n $reused ]] || reused="run $run_id (magnum $run_commit, found $score): same skill, prompts and roles, but other Go code than $short (--fresh replays)"
        done <<<"$baseline"
        if [[ -n $reused ]]; then
            echo "eval-at: $name: reused $reused; not replayed, 0 points"
        else
            todo+=("$name")
        fi
    done
fi
if ((${#todo[@]} == 0)); then
    echo "eval-at: every case reused; nothing replayed"
    exit 0
fi

run_args=()
for name in "${todo[@]}"; do run_args+=(--case "$name"); done
if (cd "$wt" && bin/magnum eval run "${run_args[@]}" --label "$label") >>"$log" 2>&1; then
    # The report the run ends with: from its header ("run <id> ...") on.
    id=$(sed -n 's/^eval run \([0-9][0-9-]*\): .*/\1/p' "$log" | tail -n 1)
    if [[ -n $id ]] && grep -q "^run $id" "$log"; then
        sed -n "/^run $id/,\$p" "$log"
    else
        tail -n 15 "$log"
    fi
else
    status=$?
    echo "eval-at: magnum eval run failed (exit $status); the end of $log:" >&2
    tail -n 20 "$log" >&2
    exit "$status"
fi
