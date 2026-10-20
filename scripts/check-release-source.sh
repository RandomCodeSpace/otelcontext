#!/usr/bin/env bash
# Validate the latest push-to-main CI run for an exact source commit.
# PR requirements are enforced at merge, not rerun as release preconditions.
# Print the verified check record as JSON; never create tags or releases here.
set -euo pipefail

sha="${1:?usage: check-release-source.sh SHA}"
[[ "$sha" =~ ^[0-9a-f]{40}$ ]] || { echo "invalid source SHA" >&2; exit 1; }
repo="${GITHUB_REPOSITORY:-$(gh repo view --json nameWithOwner --jq .nameWithOwner)}"

gh api "repos/$repo/branches/main" |
  jq -e '.protected == true and (.protection.required_status_checks.contexts | length) > 0' >/dev/null

# Do not fall back to an older successful run when a rerun is pending or failed.
run="$(gh api --method GET "repos/$repo/actions/workflows/ci.yml/runs" \
  -f branch=main -f event=push -f head_sha="$sha" -f per_page=100 |
  jq -ce --arg sha "$sha" '
    [.workflow_runs[] | select(.head_sha == $sha and .head_branch == "main" and
      .event == "push" and .path == ".github/workflows/ci.yml")]
    | max_by(.id) | select(. != null)')"
jq -e '.status == "completed" and .conclusion == "success"' <<<"$run" >/dev/null ||
  { echo "latest main CI run is not successful for $sha" >&2; exit 1; }
run_id="$(jq -r .id <<<"$run")"
attempt="$(jq -r .run_attempt <<<"$run")"
[[ "$run_id" =~ ^[0-9]+$ && "$attempt" =~ ^[1-9][0-9]*$ ]] ||
  { echo "invalid main run identity" >&2; exit 1; }

gh api --paginate "repos/$repo/actions/runs/$run_id/attempts/$attempt/jobs?per_page=100" \
  --jq '.jobs[]' |
  jq -sce --arg sha "$sha" --argjson run_id "$run_id" --argjson attempt "$attempt" '
    [.[] | select(.name == "main validation")] |
    if length == 1 and .[0].status == "completed" and .[0].conclusion == "success" then
      [.[0] | {name, conclusion, url:.html_url, sha:$sha,
        run_id:$run_id, run_attempt:$attempt, event:"push", branch:"main"}]
    else error("main validation must be present exactly once and successful") end'
