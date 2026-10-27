#!/usr/bin/env bash
# Cuts a buggrit release (docs/releasing.md). Run it from an up-to-date main
# with a clean worktree:
#
#   make release RELEASE=v0.5.0
#
# It checks that nothing blocks the release, writes a release commit on top
# of main (not on any branch) whose review.yml names this release in its
# own `uses:` lines, tags it, and pushes the tag with your credentials. The
# tag push starts release.yml, which checks the commit again, then builds
# and publishes the release. This script waits for the release to be
# published, then moves v0 to it (scripts/move-majors.sh).
#
# A workflow can't do either push: each changes what a workflow file says at
# that ref, and GitHub refuses that from any token without the workflows
# permission (ADR 0001, ADR 0002).
#
# Environment:
#   REPO            owner/name on GitHub (default SnapdragonPartners/buggrit)
#   REMOTE          git remote for that repository (default origin)
#   RELEASE_CONFIRM yes skips the prompt before the push
#   RELEASE_WAIT    seconds to wait for the release to publish (default 1200)
#   RELEASE_POLL    seconds between checks (default 15)
set -euo pipefail

version=${1:?usage: release.sh <vX.Y.Z>}
repo=${REPO:-SnapdragonPartners/buggrit}
remote=${REMOTE:-origin}
here=$(cd "$(dirname "$0")" && pwd)

fail() { echo "release: $1" >&2; exit 1; }

semver='^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'
[[ "$version" =~ $semver ]] || fail "$version is not a vX.Y.Z version"

cd "$(git rev-parse --show-toplevel)"
[ -z "$(git status --porcelain)" ] || fail "the worktree isn't clean"

git fetch --quiet "$remote" main
head=$(git rev-parse HEAD)
[ "$head" = "$(git rev-parse "$remote/main")" ] || fail "HEAD isn't $remote/main; check out main and pull"
git rev-parse -q --verify "refs/tags/$version" >/dev/null && fail "tag $version already exists locally"
[ -z "$(git ls-remote --tags "$remote" "refs/tags/$version")" ] || fail "tag $version already exists on $remote"

# Nothing may be waiting on this release (CLAUDE.md, "Releasing").
blockers=$(gh issue list --repo "$repo" --label release-blocker --state open \
             --json number,title --jq '.[] | "  #\(.number) \(.title)"')
[ -z "$blockers" ] || fail "open release-blocker issues:"$'\n'"$blockers"

# The protections the pinned references rely on (docs/releasing.md, "One-time
# setup"): immutable releases lock the tag and its assets once published, and
# the tag ruleset stops anyone else creating, moving or deleting v* tags.
[ "$(gh api "repos/$repo/immutable-releases" --jq .enabled)" = true ] ||
  fail "immutable releases are off for $repo"
# Only admins bypass that ruleset (ADR 0002), so a non-admin can't push the
# tag; say so now rather than at the push.
[ "$(gh api "repos/$repo" --jq .permissions.admin)" = true ] ||
  fail "only an admin of $repo can push a release tag (the \"Release tags\" ruleset)"
rulesets=$(gh api "repos/$repo/rulesets" --jq '.[] | select(.target == "tag" and .enforcement == "active") | .name')
grep -qxF "Release tags" <<< "$rulesets" || fail "$repo has no active \"Release tags\" ruleset"

# Write the release commit in a throwaway worktree, so this one stays on main.
work=$(mktemp -d)
tagged=
pushed=
# Whatever stops the script before the push succeeds (a refusal, a declined
# or unreadable prompt, a failed push) deletes the local tag, so a rerun
# starts clean. If a failed push did reach the remote, the rerun's check of
# the remote catches it.
cleanup() {
  git worktree remove --force "$work" >/dev/null 2>&1 || rm -rf "$work"
  if [ -n "$tagged" ] && [ -z "$pushed" ]; then
    git tag -d "$version" >/dev/null 2>&1 || true
    echo "release: the local tag $version is deleted" >&2
  fi
}
trap cleanup EXIT
git worktree add --quiet --detach "$work" "$head"
"$here/pin-release.sh" "$version" "$work"
git -C "$work" commit --quiet -am "Release $version" \
  -m "Pins review.yml's own actions to $version (docs/releasing.md)."
commit=$(git -C "$work" rev-parse HEAD)
git tag "$version" "$commit"
tagged=1
"$here/verify-release.sh" "$version" "$remote/main" || fail "the release commit failed its own check"

git --no-pager show --stat --format='%H%n%s' "$commit"
git --no-pager diff "$head" "$commit"
if [ "${RELEASE_CONFIRM:-}" != yes ]; then
  # EOF or no terminal counts as no.
  answer=
  read -r -p "Push tag $version ($commit) to $remote? Published releases can't be changed. [y/N] " answer < /dev/tty || answer=
  [ "$answer" = y ] || [ "$answer" = Y ] || fail "not pushed"
fi
git push "$remote" "refs/tags/$version" || fail "pushing $version failed; check $remote before trying again"
pushed=1
echo "Pushed $version. release.yml is building it: gh run list --repo $repo --workflow release.yml"

# Move v0 only once the release is published with its assets: the action
# installs the release at the commit @v0 names, and a release still building
# has none.
wait=${RELEASE_WAIT:-1200}
poll=${RELEASE_POLL:-15}
deadline=$((SECONDS + wait))
until gh api "repos/$repo/releases/tags/$version" --jq '.assets[].name' 2>/dev/null | grep -qxF checksums.txt; do
  [ "$SECONDS" -lt "$deadline" ] ||
    fail "$version isn't published after ${wait}s; once release.yml has published it, run make move-majors"
  sleep "$poll"
done
echo "$version is published."
"$here/move-majors.sh" || fail "moving the major tags failed; fix that and run make move-majors"
