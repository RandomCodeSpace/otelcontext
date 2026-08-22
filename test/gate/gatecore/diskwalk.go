package gatecore

import (
	"path"
	"sort"
	"strings"
)

// Data-directory attribution (Q3 disk assertions).
//
// The gate asserts on its own filesystem walk rather than on the server's
// gauges: the gauges are the thing under test. The exported
// otelcontext_disk_component_bytes series is recorded alongside as
// corroboration, and a disagreement between the two is itself a finding.

// Tier names. These are the partitions the frozen contract assigns budgets to.
const (
	TierMain        = "main"
	TierAggregate   = "aggregate"
	TierDLQ         = "dlq"
	TierWALTempTLS  = "wal_temp_tls"
	TierUnclassifid = "unclassified"
)

// FileEntry is one file found under the data directory.
type FileEntry struct {
	// RelPath is slash-separated and relative to the data directory root.
	RelPath string `json:"rel_path"`
	Bytes   int64  `json:"bytes"`
}

// ClassifySpec names the files and directories that anchor the tiers.
type ClassifySpec struct {
	// MainDBFile and AggregateDBFile are base names, e.g. "otelcontext.db".
	MainDBFile      string
	AggregateDBFile string
	// DLQDir and TLSDir are directory names relative to the data root.
	DLQDir string
	TLSDir string
}

// DefaultClassifySpec matches the layout the gate configures the server with.
func DefaultClassifySpec() ClassifySpec {
	return ClassifySpec{
		MainDBFile:      "otelcontext.db",
		AggregateDBFile: "aggregate.db",
		DLQDir:          "dlq",
		TLSDir:          "tls",
	}
}

// Classification is the walk reduced to tiers.
type Classification struct {
	Bytes             map[string]int64
	Files             map[string][]string
	Total             int64
	Unclassified      int64
	UnclassifiedFiles []string
}

// sidecarSuffixes are the SQLite sidecars and scratch files that belong to the
// WAL/temp tier rather than to the database they hang off.
var sidecarSuffixes = []string{"-wal", "-shm", "-journal", ".tmp", ".temp"}

// Classify partitions a data-directory walk into the contract's tiers.
//
// Anything the spec does not account for lands in the unclassified bucket: it
// still counts toward the data-directory total (the volume pays for it either
// way) but it is named in the report instead of being quietly folded into a
// tier that happens to have headroom.
func Classify(entries []FileEntry, spec ClassifySpec) Classification {
	c := Classification{
		Bytes: map[string]int64{
			TierMain:       0,
			TierAggregate:  0,
			TierDLQ:        0,
			TierWALTempTLS: 0,
		},
		Files: make(map[string][]string),
	}
	for _, e := range entries {
		tier := classifyOne(e.RelPath, spec)
		c.Total += e.Bytes
		if tier == TierUnclassifid {
			c.Unclassified += e.Bytes
			c.UnclassifiedFiles = append(c.UnclassifiedFiles, e.RelPath)
			continue
		}
		c.Bytes[tier] += e.Bytes
		c.Files[tier] = append(c.Files[tier], e.RelPath)
	}
	for k := range c.Files {
		sort.Strings(c.Files[k])
	}
	sort.Strings(c.UnclassifiedFiles)
	return c
}

func classifyOne(rel string, spec ClassifySpec) string {
	clean := strings.TrimPrefix(path.Clean(rel), "./")
	base := path.Base(clean)
	top := clean
	if i := strings.IndexByte(clean, '/'); i >= 0 {
		top = clean[:i]
	}

	switch {
	case spec.DLQDir != "" && top == spec.DLQDir:
		return TierDLQ
	case spec.TLSDir != "" && top == spec.TLSDir:
		return TierWALTempTLS
	case isSidecar(base):
		return TierWALTempTLS
	case spec.MainDBFile != "" && base == spec.MainDBFile:
		return TierMain
	case spec.AggregateDBFile != "" && base == spec.AggregateDBFile:
		return TierAggregate
	case strings.HasPrefix(base, "etilqs_"):
		// SQLite's own temporary-file prefix.
		return TierWALTempTLS
	default:
		return TierUnclassifid
	}
}

func isSidecar(base string) bool {
	for _, s := range sidecarSuffixes {
		if strings.HasSuffix(base, s) {
			return true
		}
	}
	return false
}

// MainTierPhysicalBytes is the projection's measure of the main tier (Q4):
// the main database file plus its own -wal and -shm sidecars, which together
// hold the tables, the indexes, the FTS shadow tables and the free pages.
//
// This deliberately overlaps the wal_temp_tls disk tier, because Q3 and Q4
// draw the line in different places: Q3 budgets sidecars as their own
// partition, Q4 counts them as part of the physical footprint whose growth is
// projected. The report states both, and neither number is derived from the
// other.
func MainTierPhysicalBytes(entries []FileEntry, spec ClassifySpec) int64 {
	if spec.MainDBFile == "" {
		return 0
	}
	var total int64
	for _, e := range entries {
		base := path.Base(path.Clean(e.RelPath))
		if base == spec.MainDBFile ||
			base == spec.MainDBFile+"-wal" ||
			base == spec.MainDBFile+"-shm" ||
			base == spec.MainDBFile+"-journal" {
			total += e.Bytes
		}
	}
	return total
}
