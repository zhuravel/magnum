#!/usr/bin/env bash
# Cut a release: tag the pushed master as vX.Y.Z, publish the GitHub release and point the Homebrew
# tap's formula (github.com/zhuravel/homebrew-tap) at it. Usage: scripts/release.sh v0.2.0
set -euo pipefail

# The private-name guard (make check-private) reads its patterns from the gitignored .mise.local.toml:
# without them a release would skip it, so run under mise when they are not in the environment.
if [ -z "${MAGNUM_PRIVATE_PATTERNS+set}" ] && command -v mise >/dev/null 2>&1; then
  exec mise exec -C "$(git rev-parse --show-toplevel)" -- "$0" "$@"
fi

v="${1:?usage: scripts/release.sh vX.Y.Z}"
[[ "$v" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "release: the version must look like v1.2.3" >&2; exit 2; }
cd "$(git rev-parse --show-toplevel)"
[ -z "$(git status --porcelain)" ] || { echo "release: the working tree has changes; commit them first" >&2; exit 1; }
[ "$(git rev-parse --abbrev-ref HEAD)" = master ] || { echo "release: release from master" >&2; exit 1; }
git fetch -q origin master
[ "$(git rev-parse HEAD)" = "$(git rev-parse origin/master)" ] || { echo "release: push master first (HEAD is not origin/master)" >&2; exit 1; }
! git rev-parse -q --verify "refs/tags/$v" >/dev/null || { echo "release: $v exists" >&2; exit 1; }

make test
rev="$(git rev-parse HEAD)"
git tag -a "$v" -m "magnum $v"
git push -q origin "$v"
gh release create "$v" --verify-tag --generate-notes --title "magnum $v"

tap="$(mktemp -d)"
trap 'rm -rf "$tap"' EXIT
gh repo clone zhuravel/homebrew-tap "$tap" -- -q
mkdir -p "$tap/Formula"
sed -e "s/@VERSION@/$v/" -e "s/@REVISION@/$rev/" packaging/homebrew/magnum.rb > "$tap/Formula/magnum.rb"
git -C "$tap" add Formula/magnum.rb
git -C "$tap" commit -q -m "magnum $v"
git -C "$tap" push -q
echo "released $v: brew install zhuravel/tap/magnum (or brew upgrade magnum)"
