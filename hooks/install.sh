#!/bin/sh
set -e

# Ask git where hooks live: this handles linked worktrees (where .git is a
# file) and a configured core.hooksPath.
HOOKS_DIR="$(git rev-parse --path-format=absolute --git-path hooks)"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

mkdir -p "$HOOKS_DIR"
cp "$SCRIPT_DIR/pre-push" "$HOOKS_DIR/pre-push"
chmod +x "$HOOKS_DIR/pre-push"

echo "Git hooks installed in $HOOKS_DIR."
