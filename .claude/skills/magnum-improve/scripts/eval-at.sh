#!/usr/bin/env bash
# eval-at.sh [--run-dir DIR] <sha> <label> <case>...
#
# Measures the review skill and prompts of one commit with `magnum eval run`,
# the before/after proof of a skill or prompt change: a detached worktree of
# this repository at <sha> under <run-dir>/tmp/eval-<label-slug>, bin/magnum
# built there (a binary reads the prompts and the skill of its own checkout),
# `bin/magnum eval run --case <case>... --label <label>` with its output in
# <run-dir>/tmp/eval-<label-slug>.log, and the worktree removed at the end,
# also on a failure or an interrupt. The run directory is --run-dir, else
# $MAGNUM_IMPROVE_RUN. The results stay where `magnum eval list` finds them.
# Each case costs a review round of Codex usage; run it in the background.
set -euo pipefail

usage="usage: eval-at.sh [--run-dir DIR] <sha> <label> <case>..."
run_dir=${MAGNUM_IMPROVE_RUN:-}
sha=
label=
cases=()
while (($#)); do
    case $1 in
    --run-dir) [[ $# -ge 2 ]] || { echo "$usage" >&2; exit 2; }; run_dir=$2; shift 2 ;;
    --run-dir=*) run_dir=${1#*=}; shift ;;
    -h | --help) echo "$usage"; exit 0 ;;
    -*) echo "eval-at: unknown flag $1" >&2; echo "$usage" >&2; exit 2 ;;
    *)
        if [[ -z $sha ]]; then
            sha=$1
        elif [[ -z $label ]]; then
            label=$1
        else
            cases+=(--case "$1")
        fi
        shift
        ;;
    esac
done
((${#cases[@]} > 0)) || { echo "$usage" >&2; exit 2; }
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
echo "eval-at: \"$label\" at $short, $((${#cases[@]} / 2)) case(s) (log: $log)"

# The checkout's mise configuration names the Go version; the worktree's copy
# of it is the repository's own, trusted for this build only.
if ! (cd "$wt" && MISE_TRUSTED_CONFIG_PATHS="$wt${MISE_TRUSTED_CONFIG_PATHS:+:$MISE_TRUSTED_CONFIG_PATHS}" \
    mise exec -- go build -o bin/magnum ./cmd/magnum) >"$log" 2>&1; then
    echo "eval-at: the build failed at $short:" >&2
    tail -20 "$log" >&2
    exit 1
fi
if (cd "$wt" && bin/magnum eval run "${cases[@]}" --label "$label") >>"$log" 2>&1; then
    tail -n 15 "$log"
else
    status=$?
    echo "eval-at: magnum eval run failed (exit $status); the end of $log:" >&2
    tail -n 20 "$log" >&2
    exit "$status"
fi
