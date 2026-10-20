//go:build prefill

package main

import (
	"database/sql"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
)

func TestServiceIdentities(t *testing.T) {
	for _, tc := range []struct {
		n      int
		prefix string
	}{{1, "single-"}, {defaultServices, "checkout-svc-"}, {200, "loadsim-svc-"}, {numTraceSeries, "many-"}} {
		dicts, specs := buildIdentities(tc.n, tc.prefix, legacyLogProfile)
		services := make(map[uint32]string)
		for _, d := range dicts {
			if d.Kind == aggregate.KindService {
				services[d.ID] = string(d.Value)
				want := fmt.Sprintf("%s%03d", tc.prefix, len(services)-1)
				if string(d.Value) != want {
					t.Fatalf("service=%q want %q", d.Value, want)
				}
			}
			if d.Kind == aggregate.KindOperation && strings.HasPrefix(string(d.Value), "edge:") && !strings.HasPrefix(string(d.Value), "edge:"+tc.prefix) {
				t.Fatalf("edge uses a different prefix: %s", d.Value)
			}
		}
		if len(services) != tc.n || len(specs) != totalSeries {
			t.Fatalf("services=%d series=%d", len(services), len(specs))
		}
		seen := make(map[uint32]bool)
		for _, s := range specs {
			if err := s.key.Validate(); err != nil {
				t.Fatal(err)
			}
			if _, ok := services[s.key.ServiceID]; !ok {
				t.Fatalf("unresolved service %d", s.key.ServiceID)
			}
			seen[s.key.ServiceID] = true
		}
		if len(seen) != tc.n {
			t.Fatalf("only %d services have series", len(seen))
		}
	}
}

func TestPrefillEndWindow(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 3, 0, 0, time.UTC)
	got, err := prefillEndWindow("", now)
	if err != nil || got != aggregate.WindowStart(now) {
		t.Fatalf("default: %d %v", got, err)
	}
	got, err = prefillEndWindow("2026-10-18T12:00:00Z", now)
	if err != nil || got != time.Date(2026, 10, 18, 11, 55, 0, 0, time.UTC).Unix() {
		t.Fatalf("exclusive end: %d %v", got, err)
	}
	for _, raw := range []string{"bad", "2026-10-18T12:05:00Z", "2026-10-18T12:01:00Z", "2026-10-18T12:00:00.1Z"} {
		if _, err := prefillEndWindow(raw, now); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}

func TestLogProfileIdentities(t *testing.T) {
	legacyDicts, legacy := buildIdentities(200, "loadsim-svc-", legacyLogProfile)
	denseDicts, dense := buildIdentities(200, "loadsim-svc-", denseLogProfile)
	if !reflect.DeepEqual(legacyDicts, denseDicts) {
		t.Fatal("corrected density changed the retained dictionary")
	}
	for _, tc := range []struct {
		name  string
		specs []seriesSpec
		logs  int
	}{{legacyLogProfile, legacy, 500}, {denseLogProfile, dense, 9000}} {
		t.Run(tc.name, func(t *testing.T) {
			dicts := map[uint32]aggregate.DictRow{}
			for _, d := range legacyDicts {
				if d.ID == 0 || dicts[d.ID].ID != 0 || string(d.Value) == aggregate.OtherValue {
					t.Fatalf("zero, duplicate, or overflow dictionary identity: %+v", d)
				}
				dicts[d.ID] = d
			}
			keys := map[aggregate.SeriesKey]bool{}
			ids := map[aggregate.SeriesID]bool{}
			counts := map[aggregate.Signal]int{}
			names := map[uint32]bool{}
			perService := map[uint32]map[uint32]map[aggregate.StatusClass]bool{}
			system, counters := 0, 0
			for _, sp := range tc.specs {
				k := sp.key
				if err := k.Validate(); err != nil {
					t.Fatal(err)
				}
				if sp.id == 0 || ids[sp.id] || keys[k] {
					t.Fatalf("zero or duplicate series identity: %+v", sp)
				}
				ids[sp.id], keys[k] = true, true
				nameKind, _ := aggregate.NameKind(k.Signal)
				for id, kind := range map[uint32]aggregate.Kind{k.TenantID: aggregate.KindTenant, k.ServiceID: aggregate.KindService, k.NameID: nameKind} {
					if id == 0 || dicts[id].Kind != kind {
						t.Fatalf("unresolved dictionary identity %d for %+v", id, k)
					}
				}
				if dicts[k.ServiceID].TenantID != k.TenantID || dicts[k.NameID].TenantID != k.TenantID {
					t.Fatalf("cross-tenant identity: %+v", k)
				}
				counts[k.Signal]++
				if k.Signal == aggregate.SignalMetric && k.DimsID != 0 {
					system++
				}
				if sp.kind == kindCounter {
					counters++
				}
				if k.Signal != aggregate.SignalLog {
					continue
				}
				if sp.kind != kindLog || k.DimsID != 0 {
					t.Fatalf("log shape changed: %+v", sp)
				}
				names[k.NameID] = true
				if perService[k.ServiceID] == nil {
					perService[k.ServiceID] = map[uint32]map[aggregate.StatusClass]bool{}
				}
				if perService[k.ServiceID][k.NameID] == nil {
					perService[k.ServiceID][k.NameID] = map[aggregate.StatusClass]bool{}
				}
				perService[k.ServiceID][k.NameID][k.StatusClass] = true
			}
			wantCounts := map[aggregate.Signal]int{aggregate.SignalTraceOp: 2400, aggregate.SignalServiceEdge: 500, aggregate.SignalLog: tc.logs, aggregate.SignalMetric: 2600}
			if !reflect.DeepEqual(counts, wantCounts) || len(tc.specs) != 5500+tc.logs || system != 200 || counters != 1200 || len(names) != 500 {
				t.Fatalf("counts=%v series=%d system=%d counters=%d retained log names=%d", counts, len(tc.specs), system, counters, len(names))
			}
			if tc.name == legacyLogProfile {
				for i, sp := range tc.specs[2900:3400] {
					if string(dicts[sp.key.NameID].Value) != fmt.Sprintf("template-%04d: request <*> failed with <*> after <*> ms", i) ||
						string(dicts[sp.key.ServiceID].Value) != fmt.Sprintf("loadsim-svc-%03d", i%200) || sp.key.StatusClass != aggregate.StatusClass(i%6+1) ||
						sp.id != aggregate.SeriesID(2901+i) || sp.baseRate != 20+(i*11)%181 {
						t.Fatalf("legacy log %d changed: %+v", i, sp)
					}
				}
				return
			}
			if len(perService) != 200 {
				t.Fatalf("log services=%d", len(perService))
			}
			wantTiers := map[aggregate.StatusClass]int{aggregate.SeverityTierInfo: 20, aggregate.SeverityTierDebug: 10, aggregate.SeverityTierWarn: 10, aggregate.SeverityTierError: 5}
			for service, templates := range perService {
				tiers := map[aggregate.StatusClass]int{}
				for _, set := range templates {
					if !set[aggregate.SeverityTierInfo] {
						t.Fatal("template missing INFO key")
					}
					for tier := range set {
						tiers[tier]++
					}
				}
				if len(templates) != 20 || !reflect.DeepEqual(tiers, wantTiers) {
					t.Fatalf("service %d: templates=%d tiers=%v", service, len(templates), tiers)
				}
			}
		})
	}
	// Added log series shift later series IDs, but no non-log identity or
	// distribution parameters may change.
	nonLogs := func(specs []seriesSpec) []seriesSpec {
		var out []seriesSpec
		for _, sp := range specs {
			if sp.key.Signal != aggregate.SignalLog {
				sp.id = 0
				out = append(out, sp)
			}
		}
		return out
	}
	if !reflect.DeepEqual(nonLogs(legacy), nonLogs(dense)) {
		t.Fatal("non-log specs changed")
	}
	if int64(len(dense))*totalWindows != 29232000 || int64(len(legacy))*totalWindows != 12096000 {
		t.Fatal("seven-day row expectation changed")
	}
}

func TestPrefillValidation(t *testing.T) {
	for _, tc := range []struct {
		profile  string
		services int
		prefix   string
		windows  int
		wantErr  string
	}{
		{legacyLogProfile, 1, "one-", 1, ""},
		{legacyLogProfile, 2400, "many-", totalWindows, ""},
		{denseLogProfile, 200, "custom-", 2, ""},
		{denseLogProfile, defaultServices, "svc-", 1, "requires -services 200"},
		{denseLogProfile, 201, "svc-", 1, "requires -services 200"},
		{"unknown", 200, "svc-", 1, "unknown log-profile"},
		{"", 200, "svc-", 1, "unknown log-profile"},
		{legacyLogProfile, 0, "svc-", 1, "services must be"},
		{denseLogProfile, 2401, "svc-", 1, "services must be"},
		{denseLogProfile, 200, "", 1, "service-prefix nonempty"},
		{denseLogProfile, 200, "svc-", 0, "windows positive"},
		{denseLogProfile, 200, "svc-", -1, "windows positive"},
	} {
		err := validatePrefill(tc.profile, tc.services, tc.prefix, tc.windows)
		if (tc.wantErr == "" && err != nil) || (tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr))) {
			t.Errorf("%+v: %v", tc, err)
		}
	}
}

// Run the real CLI in a child so flag parsing and fatal exits are covered.
// The synctest clock holds the rollup worker's startup timer until main closes
// the store. No product API or shared database is needed to disable rollups.
func TestPrefillCommand(t *testing.T) {
	if os.Getenv("AGGPREFILL_TEST_COMMAND") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
			break
		}
	}
	flag.CommandLine = flag.NewFlagSet("aggprefill", flag.ExitOnError)
	synctest.Test(t, func(t *testing.T) { main() })
}

func prefillCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], append([]string{"-test.run=^TestPrefillCommand$", "--"}, args...)...)
	cmd.Env = append(os.Environ(), "AGGPREFILL_TEST_COMMAND=1")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestPrefillCommandRejectsProfile(t *testing.T) {
	for _, profile := range []string{"unknown", denseLogProfile} {
		db := filepath.Join(t.TempDir(), "not-created", "aggregate.db")
		out, err := prefillCommand(t, "-db", db, "-log-profile", profile)
		if err == nil || !strings.Contains(out, "log-profile") {
			t.Fatalf("accepted invalid profile combination: %v\n%s", err, out)
		}
		if _, err := os.Stat(filepath.Dir(db)); !os.IsNotExist(err) {
			t.Fatalf("invalid profile created output directory: %v", err)
		}
	}
}

func TestPrefillPersistedCounts(t *testing.T) {
	for _, tc := range []struct {
		profile string
		windows int
		series  int64
		logs    int64
	}{{legacyLogProfile, 1, 6000, 500}, {denseLogProfile, 2, 14500, 9000}} {
		t.Run(tc.profile, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "aggregate.db")
			args := []string{"-db", path, "-services", "200", "-service-prefix", "custom-", "-workers", "2", "-windows", strconv.Itoa(tc.windows), "-end-time", "2000-01-01T00:00:00Z"}
			if tc.profile != legacyLogProfile {
				args = append(args, "-log-profile", tc.profile)
			}
			out, err := prefillCommand(t, args...)
			if err != nil {
				t.Fatalf("prefill: %v\n%s", err, out)
			}
			if !strings.Contains(out, "=== PREFILL DONE ===") || !strings.Contains(out, "log_profile: "+tc.profile) {
				t.Fatalf("missing completion/profile summary:\n%s", out)
			}
			summary := map[string]int64{}
			for _, line := range strings.Split(out, "\n") {
				key, value, ok := strings.Cut(line, ": ")
				if n, err := strconv.ParseInt(value, 10, 64); ok && err == nil {
					summary[key] = n
				}
			}
			windows := int64(tc.windows)
			for key, want := range map[string]int64{
				"windows_expected": windows, "windows_finalized": windows,
				"bucket_rows_expected": tc.series * windows, "bucket_rows_written": tc.series * windows,
				"delta_rows_incorporated": tc.series * windows, "series_total": tc.series,
				"services_total": 200, "log_series_total": tc.logs, "log_template_names": 500,
				"sketches_encoded": 2900 * windows,
			} {
				if got, ok := summary[key]; !ok || got != want {
					t.Errorf("summary %s=%d present=%t want=%d", key, got, ok, want)
				}
			}
			// Read the closed fixture without reopening the aggregate store and
			// starting another background worker.
			u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
			db, err := sql.Open("sqlite", u.String())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			check := func(query string, want int64, args ...any) {
				t.Helper()
				var got int64
				if err := db.QueryRow(query, args...).Scan(&got); err != nil {
					t.Fatal(err)
				}
				if got != want {
					t.Errorf("%s: got %d want %d", query, got, want)
				}
			}
			check("SELECT COUNT(*) FROM aggregate_series", tc.series)
			check("SELECT COUNT(*) FROM aggregate_buckets", tc.series*windows)
			check("SELECT COUNT(*) FROM aggregate_delta_log", 0)
			check("SELECT COUNT(*) FROM aggregate_baseline", 1200)
			check("SELECT COUNT(*) FROM aggregate_hourly", 0)
			check("SELECT COUNT(*) FROM aggregate_rollup_hours", 0)
			check("SELECT COUNT(*) FROM aggregate_dict WHERE kind=?", 500, aggregate.KindLogTemplate)
			check("SELECT COUNT(*) FROM aggregate_dict WHERE id=0 OR value=?", 0, []byte(aggregate.OtherValue))
			check(`SELECT COUNT(*) FROM aggregate_series s
				LEFT JOIN aggregate_dict t ON t.id=s.tenant_id
				LEFT JOIN aggregate_dict v ON v.id=s.service_id
				LEFT JOIN aggregate_dict n ON n.id=s.name_id
				WHERE s.id=0 OR t.id IS NULL OR v.id IS NULL OR n.id IS NULL`, 0)
			check("SELECT COUNT(*) FROM aggregate_dict WHERE kind=? AND CAST(value AS TEXT) GLOB 'custom-[0-9][0-9][0-9]'", 200, aggregate.KindService)
			check("SELECT COUNT(*) FROM (SELECT series_id FROM aggregate_buckets GROUP BY series_id HAVING COUNT(*) != ?)", 0, tc.windows)
			for signal, count := range map[aggregate.Signal]int64{aggregate.SignalTraceOp: 2400, aggregate.SignalServiceEdge: 500, aggregate.SignalLog: tc.logs, aggregate.SignalMetric: 2600} {
				check("SELECT COUNT(*) FROM aggregate_series WHERE signal=?", count, signal)
			}
			check("SELECT COUNT(*) FROM aggregate_series WHERE signal=? AND dims_id!=0", 200, aggregate.SignalMetric)
			check(`SELECT COUNT(*) FROM aggregate_buckets b JOIN aggregate_series s ON s.id=b.series_id
				WHERE s.signal=? AND (b.sketch IS NOT NULL OR b.log_count!=b.point_count OR b.duration_count!=0
				OR b.error_count!=CASE WHEN s.severity>=5 THEN b.log_count ELSE 0 END)`, 0, aggregate.SignalLog)
			check("SELECT COUNT(*) FROM aggregate_buckets WHERE sketch IS NOT NULL", summary["sketches_encoded"])
			for column, key := range map[string]string{"request_count": "dashboard_requests", "error_request_count": "dashboard_request_errors", "point_count": "dashboard_spans", "error_count": "dashboard_span_errors"} {
				check("SELECT SUM(b."+column+") FROM aggregate_buckets b JOIN aggregate_series s ON s.id=b.series_id WHERE s.signal=?", summary[key], aggregate.SignalTraceOp)
				if summary[key] == 0 {
					t.Errorf("empty dashboard summary: %s", key)
				}
			}
			check("SELECT SUM(log_count) FROM aggregate_buckets", summary["dashboard_logs"])
			check("SELECT SUM(point_count) FROM aggregate_buckets", summary["total_observations_all_signals"])
			end := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
			check("SELECT MIN(window_start) FROM aggregate_buckets", end-windows*windowSecs)
			check("SELECT MAX(window_start) FROM aggregate_buckets", end-windowSecs)
			if !strings.Contains(out, fmt.Sprintf("first_window: %d  last_window: %d", end-windows*windowSecs, end-windowSecs)) {
				t.Fatal("summary window range differs from persisted rows")
			}
			if tc.profile == denseLogProfile {
				check("SELECT COUNT(*) FROM (SELECT service_id FROM aggregate_series WHERE signal=? GROUP BY service_id HAVING COUNT(*)=45 AND COUNT(DISTINCT name_id)=20)", 200, aggregate.SignalLog)
				for tier, count := range map[aggregate.StatusClass]int64{aggregate.SeverityTierInfo: 20, aggregate.SeverityTierDebug: 10, aggregate.SeverityTierWarn: 10, aggregate.SeverityTierError: 5} {
					check("SELECT COUNT(*) FROM (SELECT service_id FROM aggregate_series WHERE signal=? AND severity=? GROUP BY service_id HAVING COUNT(*)=?)", 200, aggregate.SignalLog, tier, count)
				}
			}
			t.Logf("persisted %d series, %d windows, %d buckets, %d log keys; dashboard totals match; rollup rows=0", tc.series, windows, tc.series*windows, tc.logs)
		})
	}
}
