#!/usr/bin/env bash
# Downloads the buggrit release binary for this runner, checks it against the
# release's checksums.txt and installs it as $BIN_DIR/buggrit. The composite
# action (action.yml) runs it; it also works by hand for a local install.
#
# Environment:
#   GH_TOKEN    token that can read the release (the App token, for a private repo)
#   REPO        owner/name of the buggrit repository (default: from GITHUB_ACTION_PATH)
#   VERSION     release tag (v0.1.0); if empty, the release at REF
#   REF         ref the action was used at: v0, v0.1.0 or a commit SHA
#               (default: from GITHUB_ACTION_PATH)
#   RUNNER_OS, RUNNER_ARCH  as set by Actions (Linux|macOS, X64|ARM64)
#   BIN_DIR     where to install
#
# Needs gh, tar, and sha256sum or shasum.
set -euo pipefail

# github.action_repository and github.action_ref have come up empty inside
# composite actions (actions/runner#2473). The action's checkout lives at
# _actions/<owner>/<name>/<ref>, so fall back to that path.
if [ -n "${GITHUB_ACTION_PATH:-}" ]; then
  p="${GITHUB_ACTION_PATH%/}"
  REF="${REF:-${p##*/}}"
  p="${p%/*}"; name="${p##*/}"; p="${p%/*}"
  REPO="${REPO:-${p##*/}/$name}"
fi

: "${GH_TOKEN:?}" "${REPO:?}" "${BIN_DIR:?}" "${RUNNER_OS:?}" "${RUNNER_ARCH:?}"

case "$RUNNER_OS" in
  Linux) os=linux ;;
  macOS) os=darwin ;;
  *) echo "buggrit: no release binary for $RUNNER_OS runners" >&2; exit 1 ;;
esac
case "$RUNNER_ARCH" in
  X64) arch=amd64 ;;
  ARM64) arch=arm64 ;;
  *) echo "buggrit: no release binary for $RUNNER_ARCH runners" >&2; exit 1 ;;
esac

semver='^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'
asset="buggrit_${os}_${arch}.tar.gz"

# by_precedence sorts semver tags on stdin highest first, with a final
# release above its own pre-releases (v0.2.0 > v0.2.0-rc.1). Pre-releases
# of one version compare by sort -V, which is close enough for rc.N.
by_precedence() {
  awk '{
    v = substr($0, 2); pre = ""; final = 1
    i = index(v, "-"); if (i) { pre = substr(v, i + 1); v = substr(v, 1, i - 1); final = 0 }
    split(v, n, ".")
    print n[1], n[2], n[3], final, (pre == "" ? "-" : pre), $0
  }' | sort -k1,1nr -k2,2nr -k3,3nr -k4,4nr -k5,5Vr | awk '{ print $6 }'
}

# has_assets reports whether tag has a published release (drafts aren't
# found by tag) carrying this runner's archive and checksums.txt. Only a 404
# means "no release"; any other failure (a bad token, a rate limit, a 5xx)
# stops the install rather than quietly falling back to an older release.
has_assets() {
  local names
  if ! names=$(gh api "repos/$REPO/releases/tags/$1" --jq '.assets[].name' 2>&1); then
    case "$names" in
      *"(HTTP 404)"*) return 1 ;;
      *) echo "buggrit: reading the release for $1 failed: $names" >&2; exit 1 ;;
    esac
  fi
  grep -qxF "$asset" <<< "$names" && grep -qxF checksums.txt <<< "$names"
}

# The release to install. A moving tag (v0) or a commit SHA resolves to the
# highest tag on the same commit whose release is published with its assets,
# so @v0 runs the binary of the release v0 points at, an action pinned by SHA
# runs that release's, and a tag whose release is still building (or failed)
# is passed over.
tag="${VERSION:-}"
if [ -z "$tag" ]; then
  : "${REF:?set VERSION or REF}"
  if [[ "$REF" =~ $semver ]]; then
    tag="$REF"
  else
    sha=$(gh api "repos/$REPO/commits/$REF" --jq .sha)
    candidates=$(gh api --paginate "repos/$REPO/tags?per_page=100" \
                   --jq ".[] | select(.commit.sha == \"$sha\") | .name" |
                 { grep -E "$semver" || true; } | by_precedence)
    for t in $candidates; do
      if has_assets "$t"; then tag="$t"; break; fi
    done
    if [ -z "$tag" ]; then
      echo "buggrit: no published release at $REF ($sha); use a release tag, a SHA of one, or set the version input" >&2
      exit 1
    fi
  fi
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

echo "Installing buggrit $tag ($os/$arch) from $REPO"
gh release download "$tag" --repo "$REPO" --dir "$work" \
  --pattern "$asset" --pattern checksums.txt

# Exactly one line naming the asset, compared literally (sha256sum's binary
# mode marks the name with a leading *).
line=$(awk -v a="$asset" '$2 == a || $2 == "*" a' "$work/checksums.txt")
if [ -z "$line" ]; then
  echo "buggrit: $asset is not in the checksums of $tag" >&2
  exit 1
fi
if [ "$(wc -l <<< "$line")" -ne 1 ]; then
  echo "buggrit: $asset appears more than once in the checksums of $tag" >&2
  exit 1
fi
if command -v sha256sum >/dev/null; then
  (cd "$work" && echo "$line" | sha256sum -c -)
else
  (cd "$work" && echo "$line" | shasum -a 256 -c -)
fi

tar -xzf "$work/$asset" -C "$work" buggrit
mkdir -p "$BIN_DIR"
install -m 0755 "$work/buggrit" "$BIN_DIR/buggrit"
"$BIN_DIR/buggrit" version
