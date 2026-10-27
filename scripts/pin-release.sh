#!/usr/bin/env bash
# Rewrites the reusable workflow's own `uses:` lines from @v0 to a release's
# tag, so the release's review.yml runs that release's actions and nothing a
# moving tag points at later (docs/design.md §3, "Releases";
# docs/releasing.md). scripts/release.sh runs it to write the release commit,
# and release.yml runs it again to check that commit.
#
# Usage: scripts/pin-release.sh <vX.Y.Z> [<repo root>]
#
# Only `uses:` lines naming SnapdragonPartners/buggrit change; comments don't.
# It fails if there's nothing to pin, or if any of buggrit's own references
# is left at a ref other than the release's.
set -euo pipefail

version=${1:?usage: pin-release.sh <vX.Y.Z> [<repo root>]}
root=${2:-.}
file="$root/.github/workflows/review.yml"

semver='^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'
[[ "$version" =~ $semver ]] || { echo "pin-release: $version is not a vX.Y.Z version" >&2; exit 1; }

# A step's `uses:` line, with or without the list dash, naming buggrit's
# repository (the root action or a path in it).
own='^[[:space:]]*(-[[:space:]]+)?uses:[[:space:]]*SnapdragonPartners/buggrit(/[^@[:space:]]*)?@'

n=$(grep -cE "${own}v0[[:space:]]*\$" "$file" || true)
[ "$n" -gt 0 ] || { echo "pin-release: no @v0 references to buggrit in $file" >&2; exit 1; }

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
sed -E "s#(${own})v0([[:space:]]*)\$#\\1${version}\\4#" "$file" > "$tmp"
cat "$tmp" > "$file"

if bad=$(grep -nE "$own" "$file" | grep -vE "@${version//./\\.}[[:space:]]*\$"); then
  echo "pin-release: references to buggrit not pinned to $version:" >&2
  echo "$bad" >&2
  exit 1
fi
echo "Pinned $n reference(s) in $file to $version."
