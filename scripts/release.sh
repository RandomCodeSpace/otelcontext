#!/usr/bin/env bash
#
# scripts/release.sh — cut a release tag whose git tree contains the built UI.
#
# Why this exists
# ---------------
# The binary embeds the React UI via `//go:embed all:dist` in internal/ui, but
# the built dist is intentionally NOT committed to main (main is source-only;
# only internal/ui/dist/.gitkeep is tracked). `go install <module>@<tag>` only
# compiles Go — it never runs vite — so for an installed binary to be
# UI-complete, the *tagged commit* must carry the built dist.
#
# This script builds the UI, creates a DETACHED release commit that includes
# internal/ui/dist, tags it, and pushes ONLY the tag. main is left untouched —
# the release commit is reachable solely through the tag, so no build artifact
# ever lands on the branch.
#
# Usage:
#   scripts/release.sh vX.Y.Z[-pre]            # build + push tag
#   scripts/release.sh vX.Y.Z[-pre] --release  # also create a GitHub pre-release
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

# --- Preconditions -----------------------------------------------------------
branch="$(git rev-parse --abbrev-ref HEAD)"
[ "$branch" = "main" ] || { echo "error: must be on main (currently '$branch')" >&2; exit 1; }
git diff --quiet && git diff --cached --quiet || { echo "error: working tree not clean" >&2; exit 1; }
git fetch origin main --tags --quiet
[ "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)" ] || { echo "error: local main is not in sync with origin/main" >&2; exit 1; }
if git rev-parse -q --verify "refs/tags/$VER" >/dev/null 2>&1 || git ls-remote --exit-code --tags origin "$VER" >/dev/null 2>&1; then
  echo "error: tag $VER already exists (local or remote)" >&2
  exit 1
fi

base="$(git rev-parse HEAD)"
# Always restore main to its source-only state, even on failure.
cleanup() { git reset --hard --quiet "$base"; git clean -fdq -- internal/ui/dist >/dev/null 2>&1 || true; }
trap cleanup EXIT

# --- Build the UI ------------------------------------------------------------
echo "▸ building UI (npm ci && npm run build)…"
( cd ui && npm ci && npm run build )
[ -f internal/ui/dist/index.html ] || { echo "error: UI build produced no dist/index.html" >&2; exit 1; }
touch internal/ui/dist/.gitkeep   # vite emptyOutDir wipes it; keep the placeholder

# --- Sanity: the binary compiles with the freshly built dist embedded --------
echo "▸ verifying the binary embeds the UI…"
go build -o /tmp/otelctx-release-check . && rm -f /tmp/otelctx-release-check

# --- Detached release commit carrying the built dist -------------------------
echo "▸ creating release commit + tag $VER…"
git add -f internal/ui/dist
git commit -q -m "release: $VER (built UI embedded; not merged to main)"
git tag -a "$VER" -m "$VER"

# --- Push ONLY the tag (its commit + dist travel with it; main stays clean) ---
git push origin "refs/tags/$VER"
echo "✓ pushed tag $VER -> $(git rev-parse --short "$VER")"

# (the EXIT trap now restores main to the source-only state)

# --- Optional GitHub pre-release --------------------------------------------
if [ "$MAKE_RELEASE" = "--release" ]; then
  prev="$(git describe --tags --abbrev=0 "${VER}^" 2>/dev/null || true)"
  range="${prev:+${prev}..}${VER}"
  notes="$(git log --pretty='- %s' "$range" 2>/dev/null | grep -vE '^- release:' || true)"
  gh release create "$VER" --prerelease --title "$VER" --notes \
"Install (UI embedded):
\`\`\`
go install github.com/RandomCodeSpace/otelcontext@$VER
\`\`\`

### Changes
$notes"
  echo "✓ created GitHub pre-release $VER"
fi
