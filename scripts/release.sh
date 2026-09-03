#!/usr/bin/env bash
#
# Cut and push an OtelContext release tag. The browser UI is committed source
# embedded by Go, so releases do not require a frontend build or detached
# release commit.
#
# The tag is created only when HEAD equals origin/main and every status check
# that branch protection requires on main reports "success" for that commit.
# The tag is annotated, pushed once, and never moved or reused; a failed
# candidate gets a new version. `--release` creates a DRAFT GitHub release
# with notes and no artifacts. The release workflow (.github/workflows/
# release.yml) runs at the tag ref, adds the signed artifacts to that draft,
# proves the candidate, and publishes it only when limited production is
# approved.
#
# Usage:
#   scripts/release.sh vX.Y.Z[-pre] [--release] [--dry-run]
#
#   --release   also create the draft GitHub (pre-)release with notes
#   --dry-run   run every check and print what would happen; do not tag,
#               push, or create a release
#
set -euo pipefail

usage() {
  echo "usage: scripts/release.sh vX.Y.Z[-pre] [--release] [--dry-run]" >&2
  exit 2
}

VER="${1:-}"
if [[ ! "$VER" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  usage
fi
shift
MAKE_RELEASE=0
DRY_RUN=0
for arg in "$@"; do
  case "$arg" in
    --release) MAKE_RELEASE=1 ;;
    --dry-run) DRY_RUN=1 ;;
    *) usage ;;
  esac
done

fail() { echo "error: $*" >&2; exit 1; }

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

branch="$(git rev-parse --abbrev-ref HEAD)"
[ "$branch" = "main" ] || fail "must be on main (currently '$branch')"
tracked_status="$(git status --porcelain --untracked-files=no)"
[ -z "$tracked_status" ] || fail "working tree has tracked changes"
git fetch origin main --tags --quiet
SHA="$(git rev-parse HEAD)"
[ "$SHA" = "$(git rev-parse origin/main)" ] || fail "local main is not in sync with origin/main"
if git rev-parse -q --verify "refs/tags/$VER" >/dev/null 2>&1 || git ls-remote --exit-code --tags origin "$VER" >/dev/null 2>&1; then
  fail "tag $VER already exists (local or remote); tags are never moved or reused"
fi

REPO="$(gh repo view --json nameWithOwner --jq .nameWithOwner)" || fail "cannot resolve the GitHub repository (is gh authenticated?)"

# Every context branch protection requires on main must report "success" for
# HEAD. Workflow jobs report as check-runs; external analyzers (SonarCloud)
# report as commit statuses. Missing, pending, skipped, or failed refuses.
echo "▸ checking required status checks for $SHA on $REPO…"
required_raw="$(gh api "repos/$REPO/branches/main/protection/required_status_checks" --jq '.contexts[]')" \
  || fail "cannot read required status checks for main"
mapfile -t required <<<"$required_raw"
[ -n "${required[0]:-}" ] || fail "main has no required status checks; refusing to tag unprotected source"
runs_raw="$(gh api "repos/$REPO/commits/$SHA/check-runs?per_page=100" --paginate \
  --jq '.check_runs[] | "\(.name)\t\(.status)\t\(.conclusion // "none")"')" \
  || fail "cannot list check-runs for $SHA"
statuses_raw="$(gh api "repos/$REPO/commits/$SHA/status" \
  --jq '.statuses[] | "\(.context)\t\(.state)"')" \
  || fail "cannot read commit statuses for $SHA"
mapfile -t runs <<<"$runs_raw"
mapfile -t statuses <<<"$statuses_raw"

blocked=()
for ctx in "${required[@]}"; do
  found=0
  verdict=""
  for line in "${runs[@]}"; do
    [ -n "$line" ] || continue
    IFS=$'\t' read -r name status conclusion <<<"$line"
    [ "$name" = "$ctx" ] || continue
    found=1
    if [ "$status" != "completed" ]; then
      verdict="$status"
    elif [ "$conclusion" != "success" ]; then
      verdict="$conclusion"
    fi
  done
  for line in "${statuses[@]}"; do
    [ -n "$line" ] || continue
    IFS=$'\t' read -r name state <<<"$line"
    [ "$name" = "$ctx" ] || continue
    found=1
    [ "$state" = "success" ] || verdict="$state"
  done
  if [ "$found" -eq 0 ]; then
    verdict="missing"
  fi
  if [ -z "$verdict" ]; then
    echo "  ✓ $ctx"
  else
    echo "  ✗ $ctx ($verdict)"
    blocked+=("$ctx ($verdict)")
  fi
done
if [ "${#blocked[@]}" -gt 0 ]; then
  fail "refusing to tag $SHA: required check not successful: ${blocked[*]}"
fi
echo "✓ all ${#required[@]} required checks succeeded for $SHA"

CHECK_BIN="$(mktemp "${TMPDIR:-/tmp}/otelcontext-release-check.XXXXXX")"
trap 'rm -f "$CHECK_BIN"' EXIT

echo "▸ verifying embedded UI and release build…"
CGO_ENABLED=0 go build -o "$CHECK_BIN" .

prerelease_flag=()
kind="release"
if [[ "$VER" == *-* ]]; then
  prerelease_flag=(--prerelease)
  kind="pre-release"
fi

if [ "$DRY_RUN" -eq 1 ]; then
  echo "dry run: would create annotated tag $VER at $SHA and push refs/tags/$VER once"
  if [ "$MAKE_RELEASE" -eq 1 ]; then
    echo "dry run: would create draft GitHub $kind $VER with generated notes"
  fi
  echo "dry run: release.yml at refs/tags/$VER would then build, sign, prove, and publish the draft"
  exit 0
fi

git tag -a "$VER" -m "$VER"
git push origin "refs/tags/$VER"
echo "✓ pushed tag $VER -> $(git rev-parse --short "$VER")"

if [ "$MAKE_RELEASE" -eq 1 ]; then
  prev="$(git describe --tags --abbrev=0 "${VER}^" 2>/dev/null || true)"
  range="${prev:+${prev}..}${VER}"
  notes="$(git log --pretty='- %s' "$range" 2>/dev/null || true)"
  body="Install:

    go install github.com/RandomCodeSpace/otelcontext@$VER

### Changes
$notes"
  # Draft on purpose: release.yml adds the signed artifacts to this draft
  # (GoReleaser release.use_existing_draft) and publishes it only after the
  # limited-production proofs pass.
  gh release create "$VER" --draft "${prerelease_flag[@]}" --title "$VER" --notes "$body"
  echo "✓ created draft GitHub $kind $VER; release.yml publishes it after the candidate is approved"
fi
