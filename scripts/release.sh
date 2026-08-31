#!/usr/bin/env bash
#
# Cut and push an OtelContext release tag. The browser UI is committed source
# embedded by Go, so releases do not require a frontend build or detached
# release commit.
#
# Usage:
#   scripts/release.sh vX.Y.Z[-pre]
#   scripts/release.sh vX.Y.Z[-pre] --release
#
set -euo pipefail

VER="${1:-}"
if [[ ! "$VER" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "usage: scripts/release.sh vX.Y.Z[-pre] [--release]" >&2
  exit 2
fi
MAKE_RELEASE="${2:-}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

branch="$(git rev-parse --abbrev-ref HEAD)"
[ "$branch" = "main" ] || { echo "error: must be on main (currently '$branch')" >&2; exit 1; }
tracked_status="$(git status --porcelain --untracked-files=no)"
[ -z "$tracked_status" ] || { echo "error: working tree has tracked changes" >&2; exit 1; }
git fetch origin main --tags --quiet
[ "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)" ] || { echo "error: local main is not in sync with origin/main" >&2; exit 1; }
if git rev-parse -q --verify "refs/tags/$VER" >/dev/null 2>&1 || git ls-remote --exit-code --tags origin "$VER" >/dev/null 2>&1; then
  echo "error: tag $VER already exists (local or remote)" >&2
  exit 1
fi

CHECK_BIN="$(mktemp "${TMPDIR:-/tmp}/otelcontext-release-check.XXXXXX")"
trap 'rm -f "$CHECK_BIN"' EXIT

echo "▸ verifying embedded UI and release build…"
CGO_ENABLED=0 go build -o "$CHECK_BIN" .

git tag -a "$VER" -m "$VER"
git push origin "refs/tags/$VER"
echo "✓ pushed tag $VER -> $(git rev-parse --short "$VER")"

if [ "$MAKE_RELEASE" = "--release" ]; then
  prev="$(git describe --tags --abbrev=0 "${VER}^" 2>/dev/null || true)"
  range="${prev:+${prev}..}${VER}"
  notes="$(git log --pretty='- %s' "$range" 2>/dev/null || true)"
  body="Install:

    go install github.com/RandomCodeSpace/otelcontext@$VER

### Changes
$notes"
  if [[ "$VER" == *-* ]]; then
    gh release create "$VER" --prerelease --title "$VER" --notes "$body"
    echo "✓ created GitHub pre-release $VER"
  else
    gh release create "$VER" --title "$VER" --notes "$body"
    echo "✓ created GitHub release $VER"
  fi
fi
