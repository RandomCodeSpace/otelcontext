#!/usr/bin/env bash
#
# Ingest baseline protocol (issue #293, decision #282).
#
# `run` starts the exact otelcontext binary in AGGREGATE_MODE=aggregate,
# pinned to two CPUs with GOMAXPROCS=2, drives it with `loadsim --direct`
# (profile aggregate-acceptance, ~10k points/s) for a settle window and a
# measured sustained window, takes a CPU profile and an allocation profile
# from the pprof listener in the middle of the sustained window, and writes
# `ingest-baseline-v1.json` (schema otelcontext.ingest-baseline.v1) next to
# the raw evidence (loadsim report, profiles, server log, /metrics dumps).
#
# `compare BASE CAND OUT` applies the #282 acceptance rule to two such
# files, writes `OUT` (schema otelcontext.ingest-ab.v1) and prints a
# Markdown table. A candidate is accepted only with identical acknowledged
# point totals AND (ACK p99 >= 10 % lower at equal throughput OR throughput
# >= 10 % higher at equal p99). "Equal" is within AB_TOLERANCE (default 5 %).
# AB_NOTES (free text) is recorded as `notes`.
# "Identical totals" is every offered point acknowledged on both sides
# (points_acked == points_sent, no RESOURCE_EXHAUSTED) with the offered totals within 0.01 %:
# loadsim's tick-based emitters vary the offered total by a few points per
# run, and that drift is the generator's, not the server's.
#
# Usage:
#   OTELCONTEXT_BIN=... LOADSIM_BIN=... OUT_DIR=... scripts/ingest-baseline.sh run
#   scripts/ingest-baseline.sh compare baseline.json candidate.json ab.json
#
# Knobs (run): SERVER_CPUS (0,1), LOADSIM_CPUS (2,3), SETTLE_SEC (30),
# SUSTAINED_SEC (120), PROFILE_SEC (60), PROFILE_OFFSET_SEC (30), HTTP_PORT
# (18080), GRPC_PORT (14317), PPROF_PORT (16060), WORK_DIR ($OUT_DIR/work),
# HARDWARE_NOTE (free text recorded in the JSON), READY_TIMEOUT_SEC (180),
# BOUNDARY_OFFSET_SEC (60): the sustained phase is started so that exactly one
# five-minute window boundary falls this many seconds into it. A boundary
# opens a fresh delta row for every active series in one group commit, which
# is the largest single ACK stall the steady state has; whether a 120 s
# window contains one or not moved p99 by 3x between otherwise identical
# runs, so the protocol pins it instead of leaving it to the wall clock.
#
set -euo pipefail

SCHEMA_RUN="otelcontext.ingest-baseline.v1"
SCHEMA_AB="otelcontext.ingest-ab.v1"

die() { echo "ingest-baseline: $*" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || die "missing tool: $1"; }

# prom_sum NAME FILE — sum every sample of a Prometheus metric across labels.
prom_sum() {
  awk -v name="$1" '
    index($0, name) == 1 && (substr($0, length(name)+1, 1) == "{" || substr($0, length(name)+1, 1) == " ") { s += $NF }
    END { printf "%.0f\n", s + 0 }' "$2"
}

run() {
  need jq; need curl; need taskset; need awk; need sha256sum
  : "${OTELCONTEXT_BIN:?set OTELCONTEXT_BIN}"
  : "${LOADSIM_BIN:?set LOADSIM_BIN}"
  : "${OUT_DIR:?set OUT_DIR}"
  SERVER_CPUS="${SERVER_CPUS:-0,1}"
  LOADSIM_CPUS="${LOADSIM_CPUS:-2,3}"
  SETTLE_SEC="${SETTLE_SEC:-30}"
  SUSTAINED_SEC="${SUSTAINED_SEC:-120}"
  PROFILE_SEC="${PROFILE_SEC:-60}"
  PROFILE_OFFSET_SEC="${PROFILE_OFFSET_SEC:-30}"
  HTTP_PORT="${HTTP_PORT:-18080}"
  GRPC_PORT="${GRPC_PORT:-14317}"
  PPROF_PORT="${PPROF_PORT:-16060}"
  READY_TIMEOUT_SEC="${READY_TIMEOUT_SEC:-180}"
  WORK_DIR="${WORK_DIR:-$OUT_DIR/work}"
  HARDWARE_NOTE="${HARDWARE_NOTE:-}"
  BOUNDARY_OFFSET_SEC="${BOUNDARY_OFFSET_SEC:-60}"
  if (( PROFILE_OFFSET_SEC + PROFILE_SEC > SUSTAINED_SEC )); then
    die "PROFILE_OFFSET_SEC + PROFILE_SEC must fit inside SUSTAINED_SEC"
  fi
  if (( BOUNDARY_OFFSET_SEC < 0 || BOUNDARY_OFFSET_SEC >= SUSTAINED_SEC || SUSTAINED_SEC - BOUNDARY_OFFSET_SEC > 300 || BOUNDARY_OFFSET_SEC > 300 )); then
    die "BOUNDARY_OFFSET_SEC must place exactly one 5-minute boundary inside SUSTAINED_SEC"
  fi

  # Every run starts from an empty store: a recovered delta log or a warm
  # dictionary from a previous run is a different experiment.
  rm -rf "$WORK_DIR/data"
  mkdir -p "$OUT_DIR" "$WORK_DIR/data"
  local data_dir="$WORK_DIR/data"
  local server_log="$OUT_DIR/server.log"
  local loadsim_log="$OUT_DIR/loadsim.log"
  local report="$OUT_DIR/loadsim-report.json"
  local cpu_pprof="$OUT_DIR/cpu.pprof"
  local allocs_pprof="$OUT_DIR/allocs.pprof"
  local before="$OUT_DIR/metrics-before.prom"
  local after="$OUT_DIR/metrics-after.prom"
  rm -f "$server_log" "$loadsim_log" "$report" "$cpu_pprof" "$allocs_pprof" "$before" "$after"

  # Server environment. The gate's server_env (test/gate/gate.config.json)
  # minus the disk-budget: hosted runners sit near a full disk, and a raw_off
  # shed would turn /ready into 503 for reasons unrelated to ingest.
  local -a server_env=(
    "AGGREGATE_MODE=aggregate"
    "AGGREGATE_SYNCHRONOUS=NORMAL"
    "AGGREGATE_DB_PATH=$data_dir/aggregate.db"
    "API_KEY="
    "API_RATE_LIMIT_RPS=0"
    "APP_ENV=development"
    "DATA_DISK_BUDGET_MB=1000000"
    "DATA_DISK_PATH=$data_dir"
    "DB_DRIVER=sqlite"
    "DB_DSN=$data_dir/otelcontext.db"
    "DLQ_PATH=$data_dir/dlq"
    "EXEMPLAR_RETENTION_DAYS=2"
    "GOMAXPROCS=2"
    "GRPC_PORT=$GRPC_PORT"
    "HOT_RETENTION_DAYS=8"
    "HTTP_PORT=$HTTP_PORT"
    "INGEST_ASYNC_ENABLED=true"
    "LOG_FTS_ENABLED=true"
    "MCP_ENABLED=true"
    "OTELCONTEXT_ALLOW_SQLITE_PROD=false"
    "PPROF_ADDR=127.0.0.1:$PPROF_PORT"
    "SAMPLING_RATE=1.0"
    "TLS_AUTO_SELFSIGNED=false"
    "TLS_CACHE_DIR=$data_dir/tls"
    "TMPDIR=$WORK_DIR"
  )

  echo "ingest-baseline: starting server on cpus $SERVER_CPUS (http :$HTTP_PORT grpc :$GRPC_PORT pprof :$PPROF_PORT)"
  env -i PATH="$PATH" HOME="${HOME:-/tmp}" "${server_env[@]}" \
    taskset -c "$SERVER_CPUS" "$OTELCONTEXT_BIN" >"$server_log" 2>&1 &
  local server_pid=$!
  # shellcheck disable=SC2064
  trap "kill -TERM $server_pid 2>/dev/null || true" EXIT

  local ready_deadline=$(( $(date +%s) + READY_TIMEOUT_SEC ))
  until [[ "$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$HTTP_PORT/ready" || true)" == "200" ]]; do
    kill -0 "$server_pid" 2>/dev/null || { tail -n 40 "$server_log" >&2; die "server exited before ready"; }
    (( $(date +%s) < ready_deadline )) || { curl -s "http://127.0.0.1:$HTTP_PORT/ready" >&2 || true; die "server not ready within ${READY_TIMEOUT_SEC}s"; }
    sleep 1
  done
  curl -s "http://127.0.0.1:$HTTP_PORT/metrics/prometheus" >"$before"

  # Align: loadsim starts at (boundary - BOUNDARY_OFFSET_SEC - SETTLE_SEC),
  # where boundary is the next 5-minute mark that leaves room for the lead.
  local now boundary lead wait_s
  lead=$(( SETTLE_SEC + BOUNDARY_OFFSET_SEC ))
  now=$(date +%s)
  boundary=$(( (now / 300 + 1) * 300 ))
  while (( boundary - lead < now )); do boundary=$(( boundary + 300 )); done
  wait_s=$(( boundary - lead - now ))
  echo "ingest-baseline: waiting ${wait_s}s so the window boundary at $(date -u -d @"$boundary" +%H:%M:%SZ) falls ${BOUNDARY_OFFSET_SEC}s into the sustained phase"
  sleep "$wait_s"

  echo "ingest-baseline: loadsim on cpus $LOADSIM_CPUS, settle ${SETTLE_SEC}s, sustained ${SUSTAINED_SEC}s"
  local started_at
  started_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  taskset -c "$LOADSIM_CPUS" "$LOADSIM_BIN" \
    --direct --profile aggregate-acceptance \
    --endpoint "127.0.0.1:$GRPC_PORT" \
    --settle "${SETTLE_SEC}s" --duration "${SUSTAINED_SEC}s" \
    --report "$report" >"$loadsim_log" 2>&1 &
  local loadsim_pid=$!

  # Profile the middle of the sustained window: the settle window carries
  # connection setup and cold dictionaries, and the last seconds carry the
  # emitters draining.
  sleep $(( SETTLE_SEC + PROFILE_OFFSET_SEC ))
  kill -0 "$loadsim_pid" 2>/dev/null || { tail -n 40 "$loadsim_log" >&2; die "loadsim exited before the profile window"; }
  echo "ingest-baseline: CPU profile for ${PROFILE_SEC}s"
  curl -s -f -o "$cpu_pprof" "http://127.0.0.1:$PPROF_PORT/debug/pprof/profile?seconds=$PROFILE_SEC" \
    || die "cpu profile fetch failed"
  curl -s -f -o "$allocs_pprof" "http://127.0.0.1:$PPROF_PORT/debug/pprof/allocs" \
    || die "allocs profile fetch failed"

  local loadsim_rc=0
  wait "$loadsim_pid" || loadsim_rc=$?
  local ended_at
  ended_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  curl -s "http://127.0.0.1:$HTTP_PORT/metrics/prometheus" >"$after"
  kill -TERM "$server_pid" 2>/dev/null || true
  wait "$server_pid" || true
  trap - EXIT
  (( loadsim_rc == 0 )) || { tail -n 40 "$loadsim_log" >&2; die "loadsim exited $loadsim_rc"; }
  [[ -s "$report" ]] || die "loadsim wrote no report"

  delta() { echo $(( $(prom_sum "$1" "$after") - $(prom_sum "$1" "$before") )); }

  local git_sha="unknown" git_dirty=false
  git_sha="$(git -C "$(dirname "$0")" rev-parse HEAD 2>/dev/null || echo unknown)"
  # A candidate is usually measured before its commit exists: git_sha is
  # then the parent and git_dirty says so; binary_sha256 is the identity.
  git -C "$(dirname "$0")" diff --quiet HEAD -- . 2>/dev/null || git_dirty=true
  local cpu_model
  cpu_model="$(awk -F': ' '/^model name/ {print $2; exit}' /proc/cpuinfo 2>/dev/null || echo unknown)"
  local mem_bytes
  mem_bytes="$(awk '/^MemTotal/ {printf "%.0f", $2 * 1024}' /proc/meminfo 2>/dev/null || echo 0)"

  jq -n \
    --arg schema "$SCHEMA_RUN" \
    --arg generated_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg started_at "$started_at" --arg ended_at "$ended_at" \
    --arg git_sha "$git_sha" --argjson git_dirty "$git_dirty" \
    --arg go_version "$(go version 2>/dev/null || echo unknown)" \
    --arg binary_sha256 "$(sha256sum "$OTELCONTEXT_BIN" | cut -d' ' -f1)" \
    --arg loadsim_sha256 "$(sha256sum "$LOADSIM_BIN" | cut -d' ' -f1)" \
    --arg cpu_model "$cpu_model" --argjson cpus_total "$(nproc)" --argjson mem_bytes "$mem_bytes" \
    --arg kernel "$(uname -r)" --arg hw_note "$HARDWARE_NOTE" \
    --arg server_cpus "$SERVER_CPUS" --arg loadsim_cpus "$LOADSIM_CPUS" \
    --argjson settle "$SETTLE_SEC" --argjson sustained "$SUSTAINED_SEC" \
    --argjson profile_sec "$PROFILE_SEC" --argjson profile_offset "$PROFILE_OFFSET_SEC" \
    --argjson boundary_offset "$BOUNDARY_OFFSET_SEC" --argjson boundary "$boundary" \
    --argjson server_env "$(printf '%s\n' "${server_env[@]}" | grep -v -E '^(AGGREGATE_DB_PATH|DATA_DISK_PATH|DB_DSN|DLQ_PATH|TLS_CACHE_DIR|TMPDIR|HTTP_PORT|GRPC_PORT|PPROF_ADDR)=' | jq -R -s 'split("\n") | map(select(length > 0) | capture("^(?<k>[^=]+)=(?<v>.*)$") | {key: .k, value: .v}) | from_entries')" \
    --slurpfile report "$report" \
    --argjson input_points "$(delta otelcontext_aggregate_input_points_total)" \
    --argjson commits "$(delta otelcontext_aggregate_commits_total)" \
    --argjson deltas "$(delta otelcontext_aggregate_deltas_total)" \
    --argjson admission_rejected "$(delta otelcontext_aggregate_admission_rejected_total)" \
    --argjson late_points "$(delta otelcontext_aggregate_late_points_total)" \
    --argjson pipeline_dropped "$(delta otelcontext_ingest_pipeline_dropped_total)" \
    --argjson identity_overflow "$(delta otelcontext_aggregate_identity_overflow_total)" \
    '
    ($report[0].phases | map(select(.phase == "sustained")) | first) as $s |
    {
      schema_version: $schema,
      generated_at: $generated_at,
      git_sha: $git_sha,
      git_dirty: $git_dirty,
      go_version: $go_version,
      binary_sha256: $binary_sha256,
      loadsim_sha256: $loadsim_sha256,
      hardware: {
        cpu_model: $cpu_model,
        cpus_total: $cpus_total,
        memory_total_bytes: $mem_bytes,
        kernel: $kernel,
        confinement: "taskset",
        server_cpus: $server_cpus,
        server_gomaxprocs: 2,
        loadsim_cpus: $loadsim_cpus,
        is_gate_box: false,
        note: $hw_note
      },
      protocol: {
        loadsim_profile: "aggregate-acceptance",
        services: ($report[0].config.services // 150),
        offered_points_per_sec: ($report[0].config | (.services * (.span_rate_per_svc + .log_rate_per_svc + .metric_rate_per_svc))),
        batch_interval_ms: ($report[0].config.batch_interval_ms // null),
        settle_sec: $settle,
        sustained_sec: $sustained,
        profile_offset_sec: $profile_offset,
        profile_sec: $profile_sec,
        window_boundary_unix: $boundary,
        window_boundary_offset_sec: $boundary_offset,
        started_at: $started_at,
        ended_at: $ended_at
      },
      server_env: $server_env,
      sustained: {
        duration_sec: $s.duration_sec,
        points_sent: $s.points_sent,
        points_acked: $s.points_acked,
        points_acked_per_sec: $s.points_acked_per_sec,
        requests_ok: $s.requests_ok,
        requests_err: $s.requests_err,
        resource_exhausted: $s.resource_exhausted,
        ack_latency_all_signals: $s.ack_latency_all_signals,
        ack_latency_by_signal: $s.ack_latency_by_signal
      },
      server_counters_delta: {
        aggregate_input_points_total: $input_points,
        aggregate_commits_total: $commits,
        aggregate_deltas_total: $deltas,
        aggregate_admission_rejected_total: $admission_rejected,
        aggregate_late_points_total: $late_points,
        aggregate_identity_overflow_total: $identity_overflow,
        ingest_pipeline_dropped_total: $pipeline_dropped
      },
      artifacts: {
        loadsim_report: "loadsim-report.json",
        cpu_profile: "cpu.pprof",
        allocs_profile: "allocs.pprof",
        server_log: "server.log",
        loadsim_log: "loadsim.log",
        metrics_before: "metrics-before.prom",
        metrics_after: "metrics-after.prom"
      }
    }' >"$OUT_DIR/ingest-baseline-v1.json"

  jq -r '"ingest-baseline: \(.sustained.points_acked_per_sec | floor) pts/s acked, p50 \(.sustained.ack_latency_all_signals.p50_ms) ms, p99 \(.sustained.ack_latency_all_signals.p99_ms) ms, acked \(.sustained.points_acked)/\(.sustained.points_sent), exhausted \(.sustained.resource_exhausted)"' \
    "$OUT_DIR/ingest-baseline-v1.json"
}

compare() {
  need jq
  local base="${1:?baseline json}" cand="${2:?candidate json}" out="${3:?output json}"
  local tol="${AB_TOLERANCE:-0.05}"
  jq -n --slurpfile b "$base" --slurpfile c "$cand" --argjson tol "$tol" --arg schema "$SCHEMA_AB" \
    --arg base_file "$(basename "$base")" --arg cand_file "$(basename "$cand")" \
    --arg notes "${AB_NOTES:-}" '
    def pick: {
      git_sha, git_dirty, generated_at,
      points_sent: .sustained.points_sent,
      points_acked: .sustained.points_acked,
      points_acked_per_sec: .sustained.points_acked_per_sec,
      ack_p50_ms: .sustained.ack_latency_all_signals.p50_ms,
      ack_p99_ms: .sustained.ack_latency_all_signals.p99_ms,
      resource_exhausted: .sustained.resource_exhausted,
      cpu_model: .hardware.cpu_model
    };
    ($b[0] | pick) as $B | ($c[0] | pick) as $C |
    ($C.points_acked_per_sec / $B.points_acked_per_sec) as $tp |
    ($C.ack_p99_ms / $B.ack_p99_ms) as $p99 |
    ($C.points_acked == $C.points_sent and $B.points_acked == $B.points_sent
      and $C.resource_exhausted == 0 and $B.resource_exhausted == 0
      and ((($C.points_sent - $B.points_sent) | fabs) <= $B.points_sent * 0.0001)) as $identical |
    (($tp - 1 | fabs) <= $tol) as $tp_equal |
    (($p99 - 1 | fabs) <= $tol) as $p99_equal |
    ($p99 <= 0.9 and $tp_equal) as $p99_win |
    ($tp >= 1.1 and $p99_equal) as $tp_win |
    ($B.cpu_model == $C.cpu_model) as $same_hw |
    {
      schema_version: $schema,
      rule: "identical acknowledged-point totals AND (ACK p99 <= 0.9x baseline at throughput within tolerance OR throughput >= 1.1x baseline at p99 within tolerance)",
      tolerance: $tol,
      baseline: ($B + {file: $base_file}),
      candidate: ($C + {file: $cand_file}),
      throughput_ratio: $tp,
      ack_p99_ratio: $p99,
      acknowledged_identical: $identical,
      same_hardware: $same_hw,
      p99_improvement: $p99_win,
      throughput_improvement: $tp_win,
      verdict: (if ($same_hw | not) then "incomparable"
                elif ($identical | not) then "revert"
                elif ($p99_win or $tp_win) then "keep"
                else "revert" end),
      notes: $notes
    }' >"$out"
  jq -r '
    "| | baseline | candidate | ratio |",
    "|---|---:|---:|---:|",
    "| commit | `\(.baseline.git_sha[0:12])`\(if .baseline.git_dirty then " (dirty)" else "" end) | `\(.candidate.git_sha[0:12])`\(if .candidate.git_dirty then " (dirty)" else "" end) | |",
    "| points acked / sent | \(.baseline.points_acked) / \(.baseline.points_sent) | \(.candidate.points_acked) / \(.candidate.points_sent) | identical: \(.acknowledged_identical) |",
    "| throughput (pts/s) | \(.baseline.points_acked_per_sec | floor) | \(.candidate.points_acked_per_sec | floor) | \(.throughput_ratio * 1000 | round / 1000) |",
    "| ACK p50 (ms) | \(.baseline.ack_p50_ms) | \(.candidate.ack_p50_ms) | |",
    "| ACK p99 (ms) | \(.baseline.ack_p99_ms) | \(.candidate.ack_p99_ms) | \(.ack_p99_ratio * 1000 | round / 1000) |",
    "| resource exhausted | \(.baseline.resource_exhausted) | \(.candidate.resource_exhausted) | |",
    "",
    "**Verdict: \(.verdict)** (rule: \(.rule); tolerance \(.tolerance); same hardware: \(.same_hardware))"
  ' "$out"
}

case "${1:-}" in
  run) run ;;
  compare) shift; compare "$@" ;;
  *) sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
