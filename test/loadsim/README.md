# loadsim — Multi-signal OTLP load simulator

A single Go binary that spins up N simulated services as goroutines and drives
sustained OTLP/gRPC traffic (spans, logs, metrics) into OtelContext. Used to verify
backend robustness and gate releases via `make loadtest`.

## What it does

- Launches `--services` (default 200) concurrent producers, each with its own
  trace, logs, and metrics exporters.
- Each producer emits:
  - `--rate` (default 50) spans/sec, cycling round-robin across 5 synthetic operations,
    durations in [5ms, 500ms], deterministic 5% error rate.
  - `--logs-rate` (default 0, disabled) logs/sec with severity mix ~70% INFO, 15% WARN,
    10% DEBUG, 5% ERROR; bodies drawn from ~20 templates with variable tokens for
    template mining.
  - `--metrics-rate` (default 0, disabled) metrics/sec: request counter (cumulative),
    queue depth gauge, and a counter that resets every ~5 minutes.
- Every 10th span is a parent with 1–3 children in the same trace.
- Producers come online linearly over `--warmup` (default 5s).
- `--profile aggregate-acceptance` applies acceptance test shape: 150 services, ~10k points/s
  sustained (75% spans, 15% logs, 10% metrics split across the services).
- `--burst multiplier x duration` (e.g. `2x30s`) temporarily multiplies all rates for the
  specified duration to exercise backpressure.
- Progress reported every 5s with per-signal counters; final summary on exit.

## Run

```bash
# Requires OtelContext running on the target endpoint.
make loadtest                           # full 200-service, 60s run
make loadtest-build                     # build-only → bin/loadsim
go test -tags loadtest ./test/loadsim/...  # unit tests

# Acceptance test (issue #173): 150 services, ~10k points/s sustained.
bin/loadsim -profile=aggregate-acceptance -duration=300s

# Backpressure test: 2× multiplier for 30s in the middle of a 60s run.
bin/loadsim -profile=aggregate-acceptance -burst=2x30s -duration=60s
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--endpoint` | `localhost:4317` | OTLP gRPC endpoint |
| `--services` | `200` | Number of simulated services |
| `--rate` | `50` | Spans per second per service |
| `--logs-rate` | `0` | Logs per second per service (0 = disabled) |
| `--metrics-rate` | `0` | Metrics per second per service (0 = disabled) |
| `--profile` | `""` | Predefined profile (e.g. `aggregate-acceptance`); overrides `--services/--rate/--logs-rate/--metrics-rate` |
| `--burst` | `""` | Burst spec (e.g. `2x30s` for 2× multiplier for 30s); sustains normal rates before/after |
| `--duration` | `60s` | Test duration |
| `--insecure` | `true` | Skip TLS verification |
| `--tenant-id` | `""` | Attach `x-tenant-id` metadata (empty = omit) |
| `--warmup` | `5s` | Linear producer ramp-up window |

## Output

Progress lines show per-signal counters, e.g.
`[T+10s] spans=5000 logs=1000 metrics=667 errors=250 rate=6667/s`

Final summary breaks down:
- Spans: count, error count, error rate %, effective rate
- Logs: (if enabled) count, error count, error rate %, effective rate
- Metrics: (if enabled) count, error count, error rate %, effective rate
- Combined: total signals, total errors, error rate %, combined effective rate

## What "healthy" looks like

- No OTLP `ResourceExhausted` or `Unavailable` errors in producer output.
- Backend `/ready` returns 200 throughout.
- `/metrics`: `OtelContext_retention_consecutive_failures` stays 0.
- p99 ingestion latency (`otelcontext_http_request_duration_seconds`) stays
  within 2× baseline; goroutine count levels off within 30s.

Caveat: this simulator does **not** start OtelContext — a live backend
must already be accepting gRPC on the target endpoint.
