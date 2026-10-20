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

# The release to install. A moving tag (v0) or a commit SHA resolves to the
# newest release tag on the same commit, so @v0 runs the binary of the
# release v0 points at, and an action pinned by SHA runs that release's.
tag="${VERSION:-}"
if [ -z "$tag" ]; then
  : "${REF:?set VERSION or REF}"
  if [[ "$REF" =~ $semver ]]; then
    tag="$REF"
  else
    sha=$(gh api "repos/$REPO/commits/$REF" --jq .sha)
    tag=$(gh api --paginate "repos/$REPO/tags?per_page=100" \
            --jq ".[] | select(.commit.sha == \"$sha\") | .name" |
          { grep -E "$semver" || true; } | sort -V | tail -n1)
    if [ -z "$tag" ]; then
      echo "buggrit: no release tag at $REF ($sha); use a release tag, a SHA of one, or set the version input" >&2
      exit 1
    fi
  fi
fi

asset="buggrit_${os}_${arch}.tar.gz"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

echo "Installing buggrit $tag ($os/$arch) from $REPO"
gh release download "$tag" --repo "$REPO" --dir "$work" \
  --pattern "$asset" --pattern checksums.txt

line=$(grep -E "[[:space:]]\*?$asset\$" "$work/checksums.txt" || true)
if [ -z "$line" ]; then
  echo "buggrit: $asset is not in the checksums of $tag" >&2
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
