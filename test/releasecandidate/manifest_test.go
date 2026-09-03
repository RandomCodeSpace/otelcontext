package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update", false, "regenerate testdata/release-candidate-v1.json")

const (
	dryRunTag = "v0.4.0-rc.1"
	dryRunSha = "4f3c0a9d1e7b2c6a8f5d3e1b9c7a4d2f6e8b0a1c"
)

// manifestArgs returns the manifest flags for the dry-run fixture with the
// given input overrides (flag name -> path).
func manifestArgs(t *testing.T, out string, overrides map[string]string) []string {
	t.Helper()
	fixture := filepath.Join("testdata", "dry-run")
	inputs := map[string]string{
		"source":        filepath.Join(fixture, "source-identity-v1.json"),
		"assets":        filepath.Join(fixture, "release-assets-v1.json"),
		"jobs":          filepath.Join(fixture, "jobs.json"),
		"evidence":      filepath.Join(fixture, "evidence"),
		"workflow-file": filepath.Join(fixture, "release.yml"),
	}
	for k, v := range overrides {
		inputs[k] = v
	}
	return []string{
		"--tag", dryRunTag,
		"--sha", dryRunSha,
		"--repository", "RandomCodeSpace/otelcontext",
		"--workflow-file", inputs["workflow-file"],
		"--workflow-ref", "refs/tags/" + dryRunTag,
		"--run-id", "17640023311",
		"--run-attempt", "1",
		"--run-url", "https://github.com/RandomCodeSpace/otelcontext/actions/runs/17640023311",
		"--started-at", "2026-09-03T10:02:00Z",
		"--finished-at", "2026-09-03T10:41:12Z",
		"--source", inputs["source"],
		"--assets", inputs["assets"],
		"--jobs", inputs["jobs"],
		"--evidence", inputs["evidence"],
		"--out", out,
	}
}

func TestManifestGolden(t *testing.T) {
	out := filepath.Join(t.TempDir(), "release-candidate-v1.json")
	if err := runManifest(manifestArgs(t, out, nil)); err != nil {
		t.Fatalf("runManifest: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "release-candidate-v1.json")
	if *updateGolden {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with -update to create it): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("manifest differs from %s (run with -update to regenerate)\n--- got ---\n%s", golden, got)
	}

	var m candidateManifest
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatal(err)
	}
	if !m.Conclusions.LimitedProduction.Approved {
		t.Fatalf("dry-run fixture must be approved, blocking: %v", m.Conclusions.LimitedProduction.BlockingFailures)
	}
	if m.Conclusions.AggregateProduction.Approved || m.Conclusions.AggregateProduction.Status != "not_run" {
		t.Fatalf("aggregate_production must be the fixed not_run block: %+v", m.Conclusions.AggregateProduction)
	}
	if m.Conclusions.LimitedProduction.Profiles.Postgres16Legacy != "approved" ||
		m.Conclusions.LimitedProduction.Profiles.SqliteLegacyBounded != "approved" {
		t.Fatalf("approved manifest must approve the legacy profiles: %+v", m.Conclusions.LimitedProduction.Profiles)
	}
	required := 0
	for _, job := range m.Jobs {
		if job.RequiredFor == "limited_production" {
			required++
		}
	}
	if required != len(requiredLimitedProductionJobs) {
		t.Fatalf("expected %d limited_production jobs, got %d", len(requiredLimitedProductionJobs), required)
	}
	if len(m.Actions) != 5 || m.Actions[0].Version != "v2.13.0" ||
		m.Actions[3].Version != "6f9f17788090df1f26f669e9d70d6ae9567deba6" || m.Actions[4].Version != "" {
		t.Fatalf("unexpected actions: %+v", m.Actions)
	}
}

// rewriteJSON loads a fixture, applies mutate, and writes the result to a
// temp file whose path is returned.
func rewriteJSON(t *testing.T, path string, mutate func(doc map[string]any)) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	mutate(doc)
	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(t.TempDir(), filepath.Base(path))
	if err := os.WriteFile(tmp, encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	return tmp
}

func mutateJobs(t *testing.T, mutate func(jobs []any) []any) string {
	t.Helper()
	return rewriteJSON(t, filepath.Join("testdata", "dry-run", "jobs.json"), func(doc map[string]any) {
		doc["jobs"] = mutate(doc["jobs"].([]any))
	})
}

func setJobConclusion(name, conclusion string) func(jobs []any) []any {
	return func(jobs []any) []any {
		for _, j := range jobs {
			job := j.(map[string]any)
			if job["name"] == name {
				job["conclusion"] = conclusion
			}
		}
		return jobs
	}
}

func TestManifestBlocked(t *testing.T) {
	fixture := filepath.Join("testdata", "dry-run")
	cases := []struct {
		name      string
		overrides func(t *testing.T) map[string]string
		want      string
	}{
		{
			name: "missing required job",
			overrides: func(t *testing.T) map[string]string {
				return map[string]string{"jobs": mutateJobs(t, func(jobs []any) []any {
					kept := jobs[:0]
					for _, j := range jobs {
						if j.(map[string]any)["name"] != "database lifecycle gate / database proof · mysql-8.4" {
							kept = append(kept, j)
						}
					}
					return kept
				})}
			},
			want: `required job "database proof · mysql-8.4" missing`,
		},
		{
			name: "skipped required job",
			overrides: func(t *testing.T) map[string]string {
				return map[string]string{"jobs": mutateJobs(t, setJobConclusion("browser smoke · release binary", "skipped"))}
			},
			want: `required job "browser smoke · release binary" conclusion skipped`,
		},
		{
			name: "failed required check",
			overrides: func(t *testing.T) map[string]string {
				return map[string]string{"source": rewriteJSON(t, filepath.Join(fixture, "source-identity-v1.json"), func(doc map[string]any) {
					for _, c := range doc["required_checks"].([]any) {
						check := c.(map[string]any)
						if check["name"] == "SonarCloud Code Analysis" {
							check["conclusion"] = "failure"
						}
					}
				})}
			},
			want: `required check "SonarCloud Code Analysis" conclusion failure`,
		},
		{
			name: "signature not verified",
			overrides: func(t *testing.T) map[string]string {
				return map[string]string{"assets": rewriteJSON(t, filepath.Join(fixture, "release-assets-v1.json"), func(doc map[string]any) {
					doc["signature_verified"] = false
				})}
			},
			want: "release assets: signature not verified",
		},
		{
			name: "failed assets check",
			overrides: func(t *testing.T) map[string]string {
				return map[string]string{"assets": rewriteJSON(t, filepath.Join(fixture, "release-assets-v1.json"), func(doc map[string]any) {
					for _, c := range doc["checks"].([]any) {
						check := c.(map[string]any)
						if check["name"] == "archives" {
							check["passed"] = false
						}
					}
				})}
			},
			want: `release assets: check "archives" failed`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "release-candidate-v1.json")
			err := runManifest(manifestArgs(t, out, tc.overrides(t)))
			var exit exitError
			if !asExit(err, &exit) || exit.code != 3 {
				t.Fatalf("want exitError code 3, got %v", err)
			}
			data, err := os.ReadFile(out)
			if err != nil {
				t.Fatalf("manifest must be written even when blocked: %v", err)
			}
			var m candidateManifest
			if err := json.Unmarshal(data, &m); err != nil {
				t.Fatal(err)
			}
			lp := m.Conclusions.LimitedProduction
			if lp.Approved {
				t.Fatal("approved must be false")
			}
			if lp.Profiles.Postgres16Legacy != "blocked" || lp.Profiles.SqliteLegacyBounded != "blocked" {
				t.Fatalf("legacy profiles must be blocked: %+v", lp.Profiles)
			}
			if len(lp.BlockingFailures) != 1 || lp.BlockingFailures[0] != tc.want {
				t.Fatalf("blocking_failures = %q, want exactly [%q]", lp.BlockingFailures, tc.want)
			}
		})
	}
}

func TestManifestInputErrors(t *testing.T) {
	out := filepath.Join(t.TempDir(), "release-candidate-v1.json")
	err := runManifest(manifestArgs(t, out, map[string]string{"jobs": filepath.Join(t.TempDir(), "absent.json")}))
	if err == nil {
		t.Fatal("missing jobs file must fail")
	}
	var exit exitError
	if asExit(err, &exit) {
		t.Fatalf("input errors must be plain errors, got exit code %d", exit.code)
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatal("no manifest may be written on an input error")
	}

	args := manifestArgs(t, out, nil)
	for i, a := range args {
		if a == "--sha" {
			args[i+1] = strings.Repeat("g", 40)
		}
	}
	if err := runManifest(args); err == nil || !strings.Contains(err.Error(), "--sha") {
		t.Fatalf("malformed sha must be refused, got %v", err)
	}
}
