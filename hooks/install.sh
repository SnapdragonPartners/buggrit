#!/bin/sh
set -e

HOOKS_DIR="$(git rev-parse --show-toplevel)/.git/hooks"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

cp "$SCRIPT_DIR/pre-push" "$HOOKS_DIR/pre-push"
chmod +x "$HOOKS_DIR/pre-push"

echo "Git hooks installed."
