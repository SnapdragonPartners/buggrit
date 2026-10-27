#!/usr/bin/env bash
# Moves each major tag (v0) to its major's newest published final release
# (docs/releasing.md). scripts/release.sh runs it once the release it pushed
# is published; run it by hand (make move-majors) if that step didn't
# finish.
#
# It runs with the releaser's credentials, not in a workflow: moving v0 to a
# release commit changes what review.yml says at v0, and GitHub refuses that
# from any token without the workflows permission, App tokens included
# (ADR 0002).
#
# It doesn't move a tag to a particular release. For every major, it reads
# the published final releases fresh and moves the major tag to the newest,
# so a patch to an older line never moves a tag backward, a concurrent run
# can't move one back past it (each push is fenced), and running it
# again is harmless.
#
# Environment:
#   REPO    owner/name on GitHub (default SnapdragonPartners/buggrit)
#   REMOTE  git remote for that repository (default origin)
set -euo pipefail

repo=${REPO:-SnapdragonPartners/buggrit}
remote=${REMOTE:-origin}

cd "$(git rev-parse --show-toplevel)"

fail() { echo "move-majors: $1" >&2; exit 1; }

# move_once reads the remote's major tags, then the published releases, and
# moves each major whose newest final release is elsewhere. Each push is
# fenced on the tag as first read, so if another mover changes it meanwhile
# the push is refused and nothing moves backward. The tags are read before
# the releases: a mover only moves a tag once its release is published, so
# a move that lands before our read comes with a release our read of the
# releases includes.
move_once() {
  local observed finals major newest target current
  observed=$(git ls-remote --tags "$remote" 'refs/tags/v*') || return 1
  git fetch --quiet --force --tags "$remote" || return 1
  finals=$(gh release list --repo "$repo" --limit 1000 \
             --exclude-drafts --exclude-pre-releases --json tagName --jq '.[].tagName') || return 1
  finals=$({ grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' || true; } <<< "$finals" | sort -V)
  [ -n "$finals" ] || fail "no published final releases in $repo"
  for major in $(sed 's/\..*//' <<< "$finals" | sort -u); do
    newest=$(grep -E "^${major}\." <<< "$finals" | tail -n1)
    target=$(git rev-list -n1 "refs/tags/$newest") || return 1
    current=$(awk -v ref="refs/tags/$major" '$2 == ref { print $1 }' <<< "$observed")
    if [ "$current" = "$target" ]; then
      echo "$major already points at $newest."
      continue
    fi
    git tag -f "$major" "$target" >/dev/null || return 1
    # An empty lease means the tag must not exist yet.
    git push --force-with-lease="refs/tags/$major:$current" "$remote" "refs/tags/$major" || return 1
    echo "$major now points at $newest."
  done
}

for attempt in 1 2 3; do
  move_once && exit 0
  echo "move-majors: attempt $attempt failed; reading the tags and releases again" >&2
done
fail "couldn't move the major tags; see the errors above"
