#!/usr/bin/env bash
# OtelContext chaos simulator — bash port of run_simulation.ps1.
#
# Builds 7 test microservices, starts them in the background, then hammers
# their endpoints with N parallel HTTP workers. Each test service exports
# OTLP to localhost:4317 — start the otelcontext binary first.
#
# Env knobs (all optional):
#   WORKERS       parallel HTTP workers (default 10)
#   DELAY_MS      ms between requests per worker (default 10)
#   DURATION_SEC  stop after N seconds (default 0 = run until SIGINT)
#   LOG_DIR       service stdout/stderr directory (default ../tmp/logs)
#
# Usage:
#   ./test/run_simulation.sh                       # run forever
#   DURATION_SEC=600 ./test/run_simulation.sh      # 10-minute run

set -euo pipefail

WORKERS=${WORKERS:-10}
DELAY_MS=${DELAY_MS:-10}
DURATION_SEC=${DURATION_SEC:-0}

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
ROOT_DIR=$(cd "$SCRIPT_DIR/.." && pwd)
TMP_DIR="$ROOT_DIR/tmp"
LOG_DIR="${LOG_DIR:-$TMP_DIR/logs}"
STATS_DIR="$TMP_DIR/stats"

# Services: name port
SERVICES=(
  "orderservice 9001"
  "paymentservice 9002"
  "inventoryservice 9003"
  "authservice 9004"
  "userservice 9005"
  "shippingservice 9006"
  "notificationservice 9007"
)

# Endpoints with weights — replicates the ps1 PickList expansion.
ENDPOINTS=(
  "POST http://localhost:9001/order 6"
  "POST http://localhost:9002/pay 2"
  "POST http://localhost:9003/check 2"
  "POST http://localhost:9004/validate 1"
  "POST http://localhost:9007/notify 1"
)

PICK_LIST=()
for ep in "${ENDPOINTS[@]}"; do
  read -r method url weight <<< "$ep"
  for ((i=0; i<weight; i++)); do
    PICK_LIST+=("$method $url")
  done
done
PICK_COUNT=${#PICK_LIST[@]}

mkdir -p "$TMP_DIR" "$LOG_DIR" "$STATS_DIR"
rm -f "$STATS_DIR"/*.cnt "$STATS_DIR/.run"

echo ""
echo "======================================"
echo "  OtelContext Chaos Simulation (bash)"
echo "======================================"
echo "  Workers  : $WORKERS"
echo "  Delay    : ${DELAY_MS}ms / worker"
if (( DURATION_SEC > 0 )); then
  echo "  Duration : ${DURATION_SEC}s"
else
  echo "  Duration : continuous (Ctrl+C to stop)"
fi
echo ""

# ── Build ────────────────────────────────────────────────────────────────────
echo "[1/3] Building test services..."
for entry in "${SERVICES[@]}"; do
  read -r name port <<< "$entry"
  printf "  %-26s " "$name"
  (cd "$ROOT_DIR" && go build -o "$TMP_DIR/$name" "./test/$name") >/dev/null
  echo "built"
done
echo "  All services built."

# ── Start services ───────────────────────────────────────────────────────────
echo ""
echo "[2/3] Starting services..."
declare -a SVC_PIDS=()
for entry in "${SERVICES[@]}"; do
  read -r name port <<< "$entry"
  "$TMP_DIR/$name" > "$LOG_DIR/$name.stdout" 2> "$LOG_DIR/$name.stderr" &
  pid=$!
  SVC_PIDS+=("$pid")
  printf "  %-26s PID %6d  :%s\n" "$name" "$pid" "$port"
done

cleanup() {
  echo ""
  echo "[cleanup] Stopping services..."
  rm -f "$STATS_DIR/.run"
  for pid in "${SVC_PIDS[@]}"; do
    kill -TERM "$pid" 2>/dev/null || true
  done
  wait "${SVC_PIDS[@]}" 2>/dev/null || true
  echo "  Done. Logs in: $LOG_DIR"
}
trap cleanup EXIT INT TERM

echo "  Waiting 4s for services to bind ports..."
sleep 4

# ── Workers ──────────────────────────────────────────────────────────────────
echo ""
echo "[3/3] Running load..."
echo ""

START=$(date +%s)
DEADLINE=0
if (( DURATION_SEC > 0 )); then
  DEADLINE=$(( START + DURATION_SEC ))
fi

worker() {
  local id=$1
  local cnt_file="$STATS_DIR/$id.cnt"
  local total=0 ok=0 fail=0
  local sleep_s
  sleep_s=$(awk "BEGIN {printf \"%.3f\", $DELAY_MS/1000}")

  while [[ -f "$STATS_DIR/.run" ]]; do
    local idx=$((RANDOM % PICK_COUNT))
    local ep="${PICK_LIST[$idx]}"
    local method url
    read -r method url <<< "$ep"

    if curl -fsS -X "$method" -m 8 -o /dev/null "$url" 2>/dev/null; then
      ok=$((ok+1))
    else
      fail=$((fail+1))
    fi
    total=$((total+1))

    # Flush every 10 requests to amortise file IO.
    if (( total % 10 == 0 )); then
      printf "%d %d %d\n" "$total" "$ok" "$fail" > "$cnt_file"
    fi

    sleep "$sleep_s"
  done
  printf "%d %d %d\n" "$total" "$ok" "$fail" > "$cnt_file"
}

touch "$STATS_DIR/.run"

declare -a WORKER_PIDS=()
for ((i=0; i<WORKERS; i++)); do
  worker "$i" &
  WORKER_PIDS+=("$!")
done

LAST_TOTAL=0
while true; do
  sleep 1
  NOW=$(date +%s)
  ELAPSED=$((NOW - START))

  TOTAL=0; OK=0; FAIL=0
  shopt -s nullglob
  for f in "$STATS_DIR"/*.cnt; do
    read -r t o fa < "$f"
    TOTAL=$((TOTAL + t))
    OK=$((OK + o))
    FAIL=$((FAIL + fa))
  done
  shopt -u nullglob

  DELTA=$((TOTAL - LAST_TOTAL))
  LAST_TOTAL=$TOTAL
  RPS=$(( ELAPSED > 0 ? TOTAL / ELAPSED : 0 ))
  ERR_PCT=$(( TOTAL > 0 ? FAIL * 100 / TOTAL : 0 ))

  printf "  %6ds | Total: %7d | OK: %7d | Fail: %5d | Err: %4d%% | %5d req/s | +%d/s\n" \
    "$ELAPSED" "$TOTAL" "$OK" "$FAIL" "$ERR_PCT" "$RPS" "$DELTA"

  if (( DEADLINE > 0 && NOW >= DEADLINE )); then
    break
  fi
done

rm -f "$STATS_DIR/.run"
for pid in "${WORKER_PIDS[@]}"; do
  wait "$pid" 2>/dev/null || true
done

ERR_COL=
if   (( ERR_PCT > 20 )); then ERR_COL="!"
elif (( ERR_PCT >  5 )); then ERR_COL="~"
else                          ERR_COL=" "
fi

echo ""
echo "======================================"
echo "  Simulation Complete"
echo "======================================"
echo "  Duration  : ${ELAPSED}s"
echo "  Requests  : $TOTAL"
echo "  Success   : $OK"
echo "  Failed    : $FAIL"
echo "  Error Rate: ${ERR_PCT}% ${ERR_COL}"
echo "  Avg RPS   : $RPS"
echo ""
