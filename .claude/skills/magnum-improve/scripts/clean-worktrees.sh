#!/usr/bin/env bash
# clean-worktrees.sh
#
# Removes the agent worktrees under .claude/worktrees whose work master
# already has (merged, or cherry-picked as equivalent patches) and that hold
# no uncommitted changes, with their branches. Lists the others.
set -euo pipefail

repo=$(cd "$(dirname "$0")/../../../.." && pwd)
cd "$repo"

git worktree list --porcelain | awk '/^worktree /{p=$2} /^branch /{print p" "$2}' |
	grep "/.claude/worktrees/" | while read -r path ref; do
	branch=${ref#refs/heads/}
	if [[ -n $(git -C "$path" status --porcelain 2>/dev/null) ]]; then
		echo "kept $branch: uncommitted changes in $path"
		continue
	fi
	if git cherry master "$branch" | grep -q '^+'; then
		echo "kept $branch: commits master does not have"
		continue
	fi
	git worktree remove "$path"
	git branch -D "$branch" >/dev/null
	echo "removed $branch"
done
git worktree prune
