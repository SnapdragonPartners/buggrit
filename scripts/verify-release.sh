#!/usr/bin/env bash
# Checks that a release tag is what scripts/release.sh makes: one commit on
# top of a commit on main, whose only change is scripts/pin-release.sh's
# rewrite for that tag (docs/releasing.md). release.yml runs it before
# building, so a tag pushed any other way publishes nothing.
#
# Usage: scripts/verify-release.sh <vX.Y.Z> [<main ref>]
#
# The main ref defaults to origin/main; fetch it first. Run it from the
# repository's top level.
set -euo pipefail

tag=${1:?usage: verify-release.sh <vX.Y.Z> [<main ref>]}
main=${2:-origin/main}
here=$(cd "$(dirname "$0")" && pwd)

fail() { echo "verify-release: $tag: $1" >&2; exit 1; }

semver='^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'
[[ "$tag" =~ $semver ]] || fail "not a vX.Y.Z tag"
commit=$(git rev-parse -q --verify "refs/tags/$tag^{commit}") || fail "no such tag"

read -r _ rest <<< "$(git rev-list --parents -n1 "$commit")"
read -r -a parents <<< "${rest:-}"
[ "${#parents[@]}" -eq 1 ] || fail "the release commit must have exactly one parent"
parent=${parents[0]}
git merge-base --is-ancestor "$parent" "$main" || fail "its parent $parent isn't on $main"

# Redo the rewrite on the parent and compare whole trees, so any other
# change in the release commit fails too.
work=$(mktemp -d)
trap 'git worktree remove --force "$work" >/dev/null 2>&1 || rm -rf "$work"' EXIT
git worktree add --quiet --detach "$work" "$parent"
"$here/pin-release.sh" "$tag" "$work" >/dev/null
git -C "$work" add -A
want=$(git -C "$work" write-tree)
got=$(git rev-parse "$commit^{tree}")
if [ "$want" != "$got" ]; then
  git diff --stat "$want" "$got" >&2 || true
  fail "the release commit isn't exactly its parent plus pin-release.sh's rewrite"
fi
echo "$tag is $parent on $main plus the pinned review.yml."
