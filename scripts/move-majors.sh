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
# so a patch to an older line never moves a tag backward, and running it
# again is harmless.
#
# Environment:
#   REPO    owner/name on GitHub (default SnapdragonPartners/buggrit)
#   REMOTE  git remote for that repository (default origin)
set -euo pipefail

repo=${REPO:-SnapdragonPartners/buggrit}
remote=${REMOTE:-origin}

cd "$(git rev-parse --show-toplevel)"
git fetch --quiet --force --tags "$remote"
finals=$(gh release list --repo "$repo" --limit 1000 \
           --exclude-drafts --exclude-pre-releases --json tagName --jq '.[].tagName' |
         { grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' || true; } | sort -V)
[ -n "$finals" ] || { echo "move-majors: no published final releases in $repo" >&2; exit 1; }
for major in $(sed 's/\..*//' <<< "$finals" | sort -u); do
  newest=$(grep -E "^${major}\." <<< "$finals" | tail -n1)
  target=$(git rev-list -n1 "refs/tags/$newest")
  if [ "$(git rev-list -n1 "refs/tags/$major" 2>/dev/null || true)" = "$target" ]; then
    echo "$major already points at $newest."
    continue
  fi
  git tag -f "$major" "$target" >/dev/null
  git push --force "$remote" "refs/tags/$major"
  echo "$major now points at $newest."
done
