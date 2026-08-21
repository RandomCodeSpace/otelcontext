# Latency Sketch Selection for the Aggregate Engine

Resolution of issue #157 (part of #154). Supersedes the dense 40-64 bin proposal in
issue #153 section 5, whose worst-case relative error (~58% at 40 bins over 8 decades,
~19% at 64 bins over 1ms-60s) is not defensible for SLO reporting.

Date: 2026-08-21. All arithmetic in this document was computed for this ticket; sources
are cited inline.

## 1. Recommendation (summary)

Use a **DDSketch-style bucketed quantile sketch whose bucket mapping is the OTLP
exponential-histogram base-2 function at scale 4** (base gamma = 2^(1/16) ~= 1.04427,
worst-case relative error 2.17% with log-midpoint estimation), stored as a **dense
uint32 array bounded at 512 bins with collapse-lowest overflow**, serialized with a
**versioned delta/varint format**, and **implemented in-repo, stdlib-only** (~300 lines).

The single design decision that buys the most: by choosing the OTel base-2 bucket
boundaries instead of an arbitrary DDSketch gamma, merging OTLP ExponentialHistogram
data points becomes an exact integer index shift ("perfect subsetting" per the OTel
spec) instead of a lossy remap. The accuracy guarantee and merge algebra are exactly
DDSketch's (Masson, Rim, Lee, VLDB 2019); only the base changes.

## 2. Candidate bucket mappings

All log-bucketed sketches share the same math: bucket index `i = ceil(log_gamma(x))`,
bucket `B_i` covers `(gamma^(i-1), gamma^i]`, worst-case relative error with
log-midpoint estimation is `(gamma-1)/(gamma+1)`. Bucket count over a dynamic range
`R` is `ceil(ln(R)/ln(gamma))`. For the required 1 microsecond to 100 seconds range,
`R = 1e8`, `ln(R) = 18.4207`.

| Candidate | gamma | Rel. error | Buckets over 1us-100s |
|---|---|---|---|
| DDSketch alpha = 0.02 | (1.02/0.98) = 1.040816 | 2.00% | 461 |
| DDSketch alpha = 0.01 | (1.01/0.99) = 1.020202 | 1.00% | 922 |
| OTel scale 4 | 2^(1/16) = 1.044274 | 2.17% | 426 |
| OTel scale 5 | 2^(1/32) = 1.021897 | 1.08% | 851 |

Populated buckets on realistic unimodal latency distributions (a service's latencies
typically span 2-3 decades, e.g. 1ms-1s or 500us-30s):

| Candidate | 2 decades | 3 decades |
|---|---|---|
| alpha = 0.02 | 116 | 173 |
| OTel scale 4 | 107 | 160 |
| OTel scale 5 | 213 | 319 |
| alpha = 0.01 | 231 | 346 |

The DDSketch paper's own evaluation is consistent: on their real "span" latency
dataset at n = 1e10 the sketch used ~900 buckets against a 2048 max at alpha = 0.01;
halving accuracy roughly halves bucket usage.

**Verdict on parameters:** 2% error at p99 is more than sufficient for SLO burn-rate
reporting (a 200ms p99 reads as 196-204ms); 1% doubles memory and disk for no
operational benefit. Between alpha = 0.02 (2.00%) and OTel scale 4 (2.17%), the 0.17
percentage points of error buy exact OTLP interoperability. **Choose OTel scale 4.**

## 3. Bound and overflow policy

Theoretical bucket count for 1us-100s at scale 4 is 426; add headroom for outliers
(sub-microsecond clock artifacts, multi-minute timeouts) and bound at **m = 512 bins**.
512 bins at scale 4 spans a 2^32 dynamic range (~9.6 decades). Overflow uses DDSketch's
collapse-lowest-buckets strategy: when a value would require a 513th bin, the
lowest-indexed bins merge into one. The paper's guarantee: a q-quantile stays
alpha-accurate as long as `x_max <= x_q * gamma^(m-1)` — with m = 512 the tail
quantiles that SLOs care about are unconditionally safe; only the far-left tail
(fastest requests) can degrade, and nobody alerts on p1 latency.

Alongside the bins keep `zero_count` (uint64, for exact-zero and sub-threshold
values, mirroring the OTel zero bucket), `count` (uint64 total), and `sum` (float64,
for averages). Negative buckets are omitted: latency is non-negative by construction;
a negative observation is a bug and goes to `zero_count`.

## 4. Memory arithmetic

Per-sketch layouts (426 theoretical / 512 bounded bins, uint32 counters):

| Layout | Bytes/sketch | 15,000 sketches in RAM (5,000 series x 3 windows) |
|---|---|---|
| Dense array, 426 bins | 1,704 + ~64 header | 24.4 MiB |
| Dense array, 512 bins (bounded) | 2,048 + ~64 header | 29.3 MiB |
| Sparse Go map, ~100 populated | ~4,800 (~48 B/entry incl. map overhead) | 68.7 MiB |
| Sparse Go map, ~150 populated | ~7,200 | 103 MiB |

A Go `map[int16]uint32` costs ~48 bytes per entry once bucket headers, tophash, and
padding are counted — the "sparse" representation is 2-3x *worse* than the dense array
at realistic occupancy, plus GC pressure from 15,000 maps. **In RAM: dense
fixed-size `[512]uint32` array, offset-based like the OTel dense layout.** ~30 MiB
total against the in-RAM budget. Contiguous, allocation-free on the hot path,
merge is a vectorizable loop.

## 5. Disk arithmetic (8 GiB emptyDir, 2,016 windows x 5,000 series)

10,080,000 sketch snapshots worst case (in practice far fewer — series absent from a
window write nothing).

| Encoding | Bytes/sketch | Total (10.08M sketches) | Fits 8 GiB? |
|---|---|---|---|
| Raw dense, 426 x uint32 | 1,704 | 16.0 GiB | No |
| Raw dense, 512 x uint32 | 2,048 | 19.2 GiB | No |
| Sparse + delta/varint, ~100 populated | ~316 (16 B header + ~3 B/bin) | 2.97 GiB | Yes |
| Sparse + delta/varint, ~150 populated | ~466 | 4.37 GiB | Yes |
| Same, pessimistic 4 B/bin | ~616 | 5.78 GiB | Yes |

The ~3 B/bin estimate: index deltas in a populated unimodal region are almost always 1
(one varint byte); counts in the body of a latency distribution fit 1-3 varint bytes.
Raw dense storage blows the budget by 2x; **the delta/varint sparse encoding is
mandatory, not an optimization**, and lands at 3-4.5 GiB with headroom for WAL-style
slack. Zstd (already in `internal/compress/`) over window files would roughly halve
that again but is not required to fit.

## 6. Merge semantics

Merge of two sketches with the same mapping is bin-wise addition, `B_i += B'_i`, plus
`zero_count`, `count`, `sum` addition and min/max of bounds — associative, commutative,
order-independent (the DDSketch mergeability property). If the union exceeds 512 bins,
collapse-lowest until it fits, same as insertion overflow.

**Different gamma/scale: do not merge.** Remapping buckets between incommensurate
bases smears counts across boundaries and silently voids the error bound. The platform
fixes **scale = 4 platform-wide**, encoded in every serialized sketch header; a merge
between mismatched parameter bytes is a hard error. If a future scale change is ever
needed, base-2 scales have an escape hatch DDSketch gammas lack: a scale-5 sketch
downscales exactly to scale 4 by index shift (section 8). Arbitrary-gamma sketches
have no such path — one more reason to prefer the OTel mapping.

## 7. Serialization format (deterministic, versioned)

Little-endian throughout. Bins sorted ascending by index; identical sketch state
always produces identical bytes.

```
offset  field
0       version        u8   = 0x01
1       mapping        u8   = 0x04 (OTel base-2 scale, value is the scale itself)
2       flags          u8   = 0x00 (reserved; bit0 reserved for "collapsed" marker)
3       zero_count     uvarint
.       count          uvarint        (total, including zero_count)
.       sum            f64 LE         (8 bytes, IEEE 754)
.       num_bins       uvarint
.       first_index    svarint        (zigzag; bucket indexes are signed)
.       repeated num_bins times:
          index_delta  uvarint        (>= 1 from previous index; first bin delta 0)
          bin_count    uvarint
```

Header is 3 + ~4 + ~4 + 8 + ~2 + ~2 ~= 16-23 bytes; each populated bin is typically
2-4 bytes. `varint` is Go's `encoding/binary` Uvarint/Varint — stdlib. The `mapping`
byte doubles as the parameter version: a reader refuses any sketch whose version or
mapping byte it does not know, which is the versioning story in one byte each.

## 8. Counter width and saturation

**In RAM and on disk: uint32 per bin, uint64 for `count`.** A uint32 bin saturates at
4.29e9 observations; over a 5-minute window that is a sustained 14.3M events/sec into
a single bucket of a single series — three orders of magnitude beyond the platform's
ingest ceiling. Policy: **saturating add** (clamp at MaxUint32, never wrap) on both
insert and merge, and increment a Prometheus counter
(`otelcontext_sketch_bin_saturations_total`) when it happens. Quantile estimates from
a saturated sketch are degraded, not corrupted; wrapping would be corruption.
`count`/`zero_count` stay uint64 because they aggregate across all bins and windows.

## 9. Merging OTLP histogram points without expanding observations

**ExponentialHistogram (the good case).** OTLP exponential histograms use the same
base-2 mapping family (`base = 2^(2^-scale)`, dense offset+counts layout — OTel
metrics data model). The spec's "perfect subsetting" property: buckets at scale s map
exactly onto buckets at any scale s' < s via `index' = index >> (s - s')`. So an
incoming point at scale >= 4 merges by index shift and bin-wise add — exact, zero
observation expansion, O(source bins). An incoming point at scale < 4 (rare; SDKs
default to scale 20 with downscale-on-demand) cannot be upscaled; merge it by treating
each coarse bucket like a fixed-boundary bucket (below), or downscale our copy for
that series. This exactness is unavailable if we pick DDSketch alpha = 0.02.

**Fixed-boundary Histogram (the lossy case).** Explicit-bounds histograms carry no
sub-bucket information, so any merge inherits the *source's* boundary error, not our
2.17% — unavoidable without raw data. Remap each source bucket `[lb, ub)` by assigning
its count to the sketch bin containing the geometric midpoint `sqrt(lb*ub)` (log-space
midpoint minimizes worst-case relative error; degenerate first/last buckets use the
finite edge). O(source buckets) per point, typically 10-20. Flag such series in the
API (`accuracy: "source-limited"`) rather than pretending the alpha bound holds.

## 10. Implement vs depend

| Library | License | Activity (checked 2026-08-21) | Footprint | Fit |
|---|---|---|---|---|
| DataDog/sketches-go | Apache-2.0 | v1.4.8, 2026-02-20 | requires `google.golang.org/protobuf` | Reference-quality DDSketch, but drags protobuf into the module for a proto serialization we would not use, and its gamma mapping forfeits exact OTLP merging |
| HdrHistogram/hdrhistogram-go | MIT | v1.3.0, 2026-07-07 | stdlib-only | Fixed integer range + significant digits model; linear-in-range memory; merge requires identical configuration; wrong shape for relative-error log bucketing |
| openhistogram/circonusllhist | Apache-2.0 | last commit 2025-01-28 | small | Log-linear base-10, ~5% worst-case error, fixed; low maintenance activity |

**Recommendation: implement in-repo (`internal/sketch/`), stdlib-only.** One line of
justification: the entire algorithm is a `[512]uint32` array, a `math.Log2`-based index
function, a saturating add, and `encoding/binary` varints — ~300 lines including the
serializer — and no candidate dependency provides the OTel-scale-4 mapping plus our
serialization anyway, so a dependency would be wrapped, not used. This satisfies the
repo policy (stdlib first; a dependency must earn its place — none does here). Port
DDSketch's published test vectors and the paper's collapse-lowest semantics; fuzz the
codec round-trip.

## Sources

- Masson, Rim, Lee. "DDSketch: A Fast and Fully-Mergeable Quantile Sketch with
  Relative-Error Guarantees." VLDB 2019. https://arxiv.org/abs/1908.10693
  (gamma = (1+alpha)/(1-alpha), i = ceil(log_gamma(x)), collapse-lowest guarantee
  `x_max <= x_q * gamma^(m-1)`, ~900 buckets observed at n=1e10 on real span data)
- OpenTelemetry Metrics Data Model, ExponentialHistogram.
  https://opentelemetry.io/docs/specs/otel/metrics/data-model/#exponentialhistogram
  (base = 2^(2^-scale), dense offset layout, perfect subsetting across scales)
- DataDog/sketches-go. https://github.com/DataDog/sketches-go (Apache-2.0; go.mod
  requires google.golang.org/protobuf v1.36.11; maxNumBins = 2048 example covers
  80us-1yr)
- HdrHistogram/hdrhistogram-go. https://github.com/HdrHistogram/hdrhistogram-go
  (MIT, pure Go, fixed-range/significant-figures model)
- openhistogram/circonusllhist. https://github.com/openhistogram/circonusllhist
  (Apache-2.0, log-linear OpenHistogram bucketing)
