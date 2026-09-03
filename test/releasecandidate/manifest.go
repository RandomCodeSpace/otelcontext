package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// requiredLimitedProductionJobs are the release workflow jobs that must be
// present and green before the draft may be published. Reusable-workflow jobs
// are reported by GitHub as "<caller job> / <callee job>", so a job matches by
// exact name or by the suffix after the last " / ".
var requiredLimitedProductionJobs = []string{
	"source identity",
	"verify signed assets",
	"database proof · sqlite",
	"database proof · postgres-16",
	"database proof · mysql-8.4",
	"database proof · sqlserver-2022",
	"browser smoke · release binary",
	"systemd proof · release archive",
	"linux arm64 smoke",
}

type candidateManifest struct {
	SchemaVersion  string              `json:"schema_version"`
	Tag            string              `json:"tag"`
	Sha            string              `json:"sha"`
	Repository     string              `json:"repository"`
	Workflow       manifestWorkflow    `json:"workflow"`
	Source         json.RawMessage     `json:"source"`
	Actions        []manifestAction    `json:"actions"`
	ReleaseAssets  json.RawMessage     `json:"release_assets"`
	Jobs           []manifestJob       `json:"jobs"`
	ProofArtifacts []manifestArtifact  `json:"proof_artifacts"`
	Conclusions    manifestConclusions `json:"conclusions"`
}

type manifestWorkflow struct {
	File       string `json:"file"`
	Ref        string `json:"ref"`
	RunID      int64  `json:"run_id"`
	RunAttempt int    `json:"run_attempt"`
	RunURL     string `json:"run_url"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
}

type manifestAction struct {
	Uses    string `json:"uses"`
	Version string `json:"version"`
}

type manifestJob struct {
	Name        string `json:"name"`
	ID          int64  `json:"id"`
	Conclusion  string `json:"conclusion"`
	URL         string `json:"url"`
	StartedAt   string `json:"started_at"`
	CompletedAt string `json:"completed_at"`
	RequiredFor string `json:"required_for"`
}

type manifestArtifact struct {
	Name  string         `json:"name"`
	Files []manifestFile `json:"files"`
}

type manifestFile struct {
	Path      string `json:"path"`
	Sha256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

type manifestConclusions struct {
	LimitedProduction   limitedProductionConclusion   `json:"limited_production"`
	AggregateProduction aggregateProductionConclusion `json:"aggregate_production"`
}

type limitedProductionConclusion struct {
	Approved         bool            `json:"approved"`
	BlockingFailures []string        `json:"blocking_failures"`
	Profiles         limitedProfiles `json:"profiles"`
}

type limitedProfiles struct {
	Postgres16Legacy    string `json:"postgres-16-legacy"`
	SqliteLegacyBounded string `json:"sqlite-legacy-bounded"`
	Mysql84             string `json:"mysql-8.4"`
	Sqlserver2022       string `json:"sqlserver-2022"`
	Aggregate           string `json:"aggregate"`
	AggregateShadow     string `json:"aggregate-shadow"`
}

type aggregateProductionConclusion struct {
	Approved bool   `json:"approved"`
	Status   string `json:"status"`
	Workflow string `json:"workflow"`
	Note     string `json:"note"`
}

// sourceIdentity holds the fields of source-identity-v1.json the approval
// rule reads. The document itself is embedded verbatim.
type sourceIdentity struct {
	Tag            string `json:"tag"`
	Sha            string `json:"sha"`
	RefType        string `json:"ref_type"`
	ProtectedMain  bool   `json:"protected_main"`
	CleanTree      bool   `json:"clean_tree"`
	RequiredChecks []struct {
		Name       string `json:"name"`
		Conclusion string `json:"conclusion"`
	} `json:"required_checks"`
}

// releaseAssetsSummary holds the fields of release-assets-v1.json the approval
// rule reads. The document itself is embedded verbatim.
type releaseAssetsSummary struct {
	Tag               string `json:"tag"`
	Sha               string `json:"sha"`
	SignatureVerified bool   `json:"signature_verified"`
	Checks            []struct {
		Name   string `json:"name"`
		Passed bool   `json:"passed"`
	} `json:"checks"`
}

// githubJobs is the GitHub Actions jobs API shape
// (GET /repos/{repo}/actions/runs/{run_id}/attempts/{attempt}/jobs).
type githubJobs struct {
	Jobs []struct {
		ID          int64  `json:"id"`
		Name        string `json:"name"`
		Conclusion  string `json:"conclusion"`
		Status      string `json:"status"`
		HTMLURL     string `json:"html_url"`
		StartedAt   string `json:"started_at"`
		CompletedAt string `json:"completed_at"`
	} `json:"jobs"`
}

var (
	shaPattern  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	usesPattern = regexp.MustCompile(`^\s*(?:-\s*)?uses:\s*(\S+)\s*(?:#\s*(.*?)\s*)?$`)
)

// runManifest assembles release-candidate-v1.json from the workflow evidence.
// It returns nil when the manifest was written and limited production is
// approved, an exitError with code 3 when the manifest was written but not
// approved, and a plain error for input or I/O failures.
func runManifest(args []string) error {
	flags := flag.NewFlagSet("manifest", flag.ContinueOnError)
	var (
		tag          = flags.String("tag", "", "release tag (vX.Y.Z[-pre])")
		sha          = flags.String("sha", "", "40-hex candidate commit")
		repository   = flags.String("repository", "", "owner/name")
		workflowFile = flags.String("workflow-file", "", "workflow file path (parsed for action versions)")
		workflowRef  = flags.String("workflow-ref", "", "workflow ref (refs/tags/<tag>)")
		runID        = flags.Int64("run-id", 0, "workflow run id")
		runAttempt   = flags.Int("run-attempt", 0, "workflow run attempt")
		runURL       = flags.String("run-url", "", "workflow run URL")
		startedAt    = flags.String("started-at", "", "run start time, RFC3339")
		finishedAt   = flags.String("finished-at", "", "run finish time, RFC3339")
		sourcePath   = flags.String("source", "", "source-identity-v1.json")
		assetsPath   = flags.String("assets", "", "release-assets-v1.json")
		jobsPath     = flags.String("jobs", "", "jobs.json from the GitHub Actions jobs API")
		evidenceDir  = flags.String("evidence", "", "directory with one subdirectory per downloaded artifact")
		out          = flags.String("out", "", "output path for release-candidate-v1.json")
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("manifest: unexpected arguments %q", flags.Args())
	}
	required := map[string]string{
		"tag": *tag, "sha": *sha, "repository": *repository, "workflow-file": *workflowFile,
		"workflow-ref": *workflowRef, "run-url": *runURL, "started-at": *startedAt,
		"finished-at": *finishedAt, "source": *sourcePath, "assets": *assetsPath,
		"jobs": *jobsPath, "evidence": *evidenceDir, "out": *out,
	}
	for _, name := range []string{"tag", "sha", "repository", "workflow-file", "workflow-ref", "run-url",
		"started-at", "finished-at", "source", "assets", "jobs", "evidence", "out"} {
		if required[name] == "" {
			return fmt.Errorf("manifest: --%s is required", name)
		}
	}
	if !shaPattern.MatchString(*sha) {
		return fmt.Errorf("manifest: --sha %q is not a 40-character lowercase hex commit", *sha)
	}
	if *runID <= 0 || *runAttempt <= 0 {
		return errors.New("manifest: --run-id and --run-attempt must be positive")
	}
	for name, value := range map[string]string{"started-at": *startedAt, "finished-at": *finishedAt} {
		if _, err := time.Parse(time.RFC3339, value); err != nil {
			return fmt.Errorf("manifest: --%s %q is not RFC3339: %w", name, value, err)
		}
	}

	sourceRaw, err := readJSONDocument(*sourcePath)
	if err != nil {
		return fmt.Errorf("manifest: source: %w", err)
	}
	var source sourceIdentity
	if err := json.Unmarshal(sourceRaw, &source); err != nil {
		return fmt.Errorf("manifest: source %s: %w", *sourcePath, err)
	}
	assetsRaw, err := readJSONDocument(*assetsPath)
	if err != nil {
		return fmt.Errorf("manifest: assets: %w", err)
	}
	var assets releaseAssetsSummary
	if err := json.Unmarshal(assetsRaw, &assets); err != nil {
		return fmt.Errorf("manifest: assets %s: %w", *assetsPath, err)
	}
	jobsRaw, err := os.ReadFile(*jobsPath)
	if err != nil {
		return fmt.Errorf("manifest: jobs: %w", err)
	}
	var jobs githubJobs
	if err := json.Unmarshal(jobsRaw, &jobs); err != nil {
		return fmt.Errorf("manifest: jobs %s: %w", *jobsPath, err)
	}
	actions, err := parseWorkflowActions(*workflowFile)
	if err != nil {
		return fmt.Errorf("manifest: workflow file: %w", err)
	}
	artifacts, err := collectEvidence(*evidenceDir)
	if err != nil {
		return fmt.Errorf("manifest: evidence: %w", err)
	}

	manifest := candidateManifest{
		SchemaVersion: "otelcontext.release-candidate.v1",
		Tag:           *tag,
		Sha:           *sha,
		Repository:    *repository,
		Workflow: manifestWorkflow{
			File:       *workflowFile,
			Ref:        *workflowRef,
			RunID:      *runID,
			RunAttempt: *runAttempt,
			RunURL:     *runURL,
			StartedAt:  *startedAt,
			FinishedAt: *finishedAt,
		},
		Source:         sourceRaw,
		Actions:        actions,
		ReleaseAssets:  assetsRaw,
		Jobs:           make([]manifestJob, 0, len(jobs.Jobs)),
		ProofArtifacts: artifacts,
	}
	for _, job := range jobs.Jobs {
		requiredFor := "informational"
		if _, ok := requiredJobName(job.Name); ok {
			requiredFor = "limited_production"
		}
		manifest.Jobs = append(manifest.Jobs, manifestJob{
			Name:        job.Name,
			ID:          job.ID,
			Conclusion:  job.Conclusion,
			URL:         job.HTMLURL,
			StartedAt:   job.StartedAt,
			CompletedAt: job.CompletedAt,
			RequiredFor: requiredFor,
		})
	}
	sort.Slice(manifest.Jobs, func(i, j int) bool {
		if manifest.Jobs[i].Name != manifest.Jobs[j].Name {
			return manifest.Jobs[i].Name < manifest.Jobs[j].Name
		}
		return manifest.Jobs[i].ID < manifest.Jobs[j].ID
	})

	blocking := limitedProductionFailures(*tag, *sha, source, assets, manifest.Jobs)
	approved := len(blocking) == 0
	profile := func(ok string) string {
		if approved {
			return ok
		}
		return "blocked"
	}
	manifest.Conclusions = manifestConclusions{
		LimitedProduction: limitedProductionConclusion{
			Approved:         approved,
			BlockingFailures: blocking,
			Profiles: limitedProfiles{
				Postgres16Legacy:    profile("approved"),
				SqliteLegacyBounded: profile("approved"),
				Mysql84:             "preview",
				Sqlserver2022:       "experimental",
				Aggregate:           "available-unapproved",
				AggregateShadow:     "available-unapproved",
			},
		},
		AggregateProduction: aggregateProductionConclusion{
			Approved: false,
			Status:   "not_run",
			Workflow: ".github/workflows/aggregate-release-gate.yml",
			Note:     "Certified separately against the same signed Linux amd64 archive; a failure never changes limited_production.",
		},
	}

	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("manifest: encode: %w", err)
	}
	encoded = append(encoded, '\n')
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	if err := os.WriteFile(*out, encoded, 0o644); err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	if !approved {
		return exitError{code: 3, msg: fmt.Sprintf("manifest: written to %s; limited production not approved (%d blocking failures)", *out, len(blocking))}
	}
	return nil
}

// limitedProductionFailures evaluates the approval rule and returns every
// reason the candidate is blocked, in a fixed order: source identity, required
// checks, release assets, then the required jobs in their declared order.
func limitedProductionFailures(tag, sha string, source sourceIdentity, assets releaseAssetsSummary, jobs []manifestJob) []string {
	failures := []string{}
	if source.Tag != tag {
		failures = append(failures, fmt.Sprintf("source: tag %q does not match candidate tag %q", source.Tag, tag))
	}
	if source.Sha != sha {
		failures = append(failures, fmt.Sprintf("source: sha %q does not match candidate sha %q", source.Sha, sha))
	}
	if !source.ProtectedMain {
		failures = append(failures, "source: protected_main is false")
	}
	if !source.CleanTree {
		failures = append(failures, "source: clean_tree is false")
	}
	if source.RefType != "tag" {
		failures = append(failures, fmt.Sprintf("source: ref_type %q is not \"tag\"", source.RefType))
	}
	for _, check := range source.RequiredChecks {
		if check.Conclusion != "success" {
			failures = append(failures, fmt.Sprintf("required check %q conclusion %s", check.Name, orMissing(check.Conclusion)))
		}
	}
	if assets.Tag != tag {
		failures = append(failures, fmt.Sprintf("release assets: tag %q does not match candidate tag %q", assets.Tag, tag))
	}
	if assets.Sha != sha {
		failures = append(failures, fmt.Sprintf("release assets: sha %q does not match candidate sha %q", assets.Sha, sha))
	}
	if !assets.SignatureVerified {
		failures = append(failures, "release assets: signature not verified")
	}
	for _, check := range assets.Checks {
		if !check.Passed {
			failures = append(failures, fmt.Sprintf("release assets: check %q failed", check.Name))
		}
	}
	for _, name := range requiredLimitedProductionJobs {
		found := false
		for _, job := range jobs {
			matched, ok := requiredJobName(job.Name)
			if !ok || matched != name {
				continue
			}
			found = true
			if job.Conclusion != "success" {
				failures = append(failures, fmt.Sprintf("required job %q conclusion %s", name, orMissing(job.Conclusion)))
			}
		}
		if !found {
			failures = append(failures, fmt.Sprintf("required job %q missing", name))
		}
	}
	return failures
}

func orMissing(conclusion string) string {
	if conclusion == "" {
		return "missing"
	}
	return conclusion
}

// requiredJobName maps a GitHub job name to the required job it satisfies,
// matching either the exact name or the suffix after the last " / ".
func requiredJobName(jobName string) (string, bool) {
	short := jobName
	if i := strings.LastIndex(jobName, " / "); i >= 0 {
		short = jobName[i+3:]
	}
	for _, name := range requiredLimitedProductionJobs {
		if jobName == name || short == name {
			return name, true
		}
	}
	return "", false
}

// readJSONDocument reads a file and checks it is a JSON object so the raw
// bytes can be embedded verbatim.
func readJSONDocument(path string) (json.RawMessage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return json.RawMessage(data), nil
}

// parseWorkflowActions extracts every `uses:` line from a workflow file. The
// version is the trailing `# ... vX.Y.Z` comment when present (last field of
// the comment), otherwise the ref after "@", otherwise empty for local
// reusable workflows.
func parseWorkflowActions(path string) ([]manifestAction, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	actions := []manifestAction{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		m := usesPattern.FindStringSubmatch(scanner.Text())
		if m == nil {
			continue
		}
		uses := strings.Trim(m[1], `"'`)
		version := ""
		if comment := strings.TrimSpace(m[2]); comment != "" {
			fields := strings.Fields(comment)
			version = fields[len(fields)-1]
		} else if i := strings.LastIndex(uses, "@"); i >= 0 {
			version = uses[i+1:]
		}
		actions = append(actions, manifestAction{Uses: uses, Version: version})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return actions, nil
}

// collectEvidence records every file under each top-level subdirectory of
// the evidence directory. Each subdirectory is one downloaded artifact.
func collectEvidence(dir string) ([]manifestArtifact, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	artifacts := []manifestArtifact{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		root := filepath.Join(dir, entry.Name())
		artifact := manifestArtifact{Name: entry.Name(), Files: []manifestFile{}}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.Type().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			sum, size, err := hashFile(path)
			if err != nil {
				return err
			}
			artifact.Files = append(artifact.Files, manifestFile{Path: filepath.ToSlash(rel), Sha256: sum, SizeBytes: size})
			return nil
		})
		if err != nil {
			return nil, err
		}
		sort.Slice(artifact.Files, func(i, j int) bool { return artifact.Files[i].Path < artifact.Files[j].Path })
		artifacts = append(artifacts, artifact)
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Name < artifacts[j].Name })
	return artifacts, nil
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("%s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}
