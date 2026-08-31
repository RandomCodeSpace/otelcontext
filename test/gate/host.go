//go:build gate

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/RandomCodeSpace/otelcontext/test/gate/gatecore"
)

// Host, filesystem and provenance facts. A report that cannot say what it ran
// on is not evidence.

// collectHost fills in the host block.
func collectHost(dataDir string) gatecore.HostInfo {
	h := gatecore.HostInfo{
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
		NumCPU:  runtime.NumCPU(),
		DataDir: dataDir,
	}
	h.Hostname, _ = os.Hostname()
	if b, err := os.ReadFile("/proc/version"); err == nil {
		h.Kernel = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		if v, err := gatecore.ParseMemTotal(string(b)); err == nil {
			h.TotalMemBytes = v
		}
	}
	if _, err := os.Stat(filepath.Join(cgroupRoot, "cgroup.controllers")); err == nil {
		h.CgroupV2 = true
	}
	h.DataDirDevice, h.DataDirMount, h.DataDirFSType = mountFor(dataDir)
	if total, free, err := statfs(dataDir); err == nil {
		h.DataDirTotal, h.DataDirFreeMin = total, free
	}
	return h
}

// statfs returns the volume's total and available bytes.
func statfs(path string) (total, free int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := int64(st.Bsize)
	return int64(st.Blocks) * bs, int64(st.Bavail) * bs, nil // #nosec G115 -- statfs counts fit in int64 on this platform
}

// mountFor resolves the mount point, device and filesystem type backing path
// by taking the longest matching prefix in /proc/self/mounts.
func mountFor(path string) (device, mount, fsType string) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	body, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return "", "", ""
	}
	best := -1
	for _, line := range strings.Split(string(body), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		mp := f[1]
		if mp != "/" && !strings.HasPrefix(abs, strings.TrimSuffix(mp, "/")+"/") && abs != mp {
			continue
		}
		if len(mp) > best {
			best = len(mp)
			device, mount, fsType = f[0], mp, f[2]
		}
	}
	return device, mount, fsType
}

// collectProvenance records exactly what was measured.
func collectProvenance(repoRoot string, binaries gatecore.Binaries, candidate candidateSpec) gatecore.Provenance {
	p := gatecore.Provenance{
		ExpectedCommitSHA:     candidate.expectedCommitSHA,
		CandidateTag:          candidate.tag,
		ExpectedServerSHA256:  candidate.expectedServerSHA256,
		ArchivePath:           candidate.archivePath,
		ExpectedArchiveSHA256: candidate.expectedArchiveSHA256,
		ConfigPath:            candidate.configPath,
		GoVersion:             runtime.Version(),
		BinarySHA256:          map[string]string{},
		BuiltAt:               time.Now().UTC(),
		OrchestratorPID:       os.Getpid(),
	}
	p.CommitSHA = gitOutput(repoRoot, "rev-parse", "HEAD")
	if candidate.tag != "" {
		p.TagCommitSHA = gitOutput(repoRoot, "rev-parse", candidate.tag+"^{commit}")
	}
	p.Branch = gitOutput(repoRoot, "rev-parse", "--abbrev-ref", "HEAD")
	status := gitOutput(repoRoot, "status", "--porcelain")
	if status != "" {
		p.DirtyTree = true
		for _, line := range strings.Split(status, "\n") {
			if s := strings.TrimSpace(line); s != "" {
				p.DirtyFiles = append(p.DirtyFiles, s)
			}
		}
	}
	for name, path := range map[string]string{
		"server": binaries.Server, "loadsim": binaries.Loadsim, "prefill": binaries.Prefill,
	} {
		if path == "" {
			continue
		}
		path = rootedPath(repoRoot, path)
		if sum, err := sha256File(path); err == nil {
			p.BinarySHA256[name] = sum
		} else {
			p.BinarySHA256[name] = "unreadable: " + err.Error()
		}
	}
	if exe, err := os.Executable(); err == nil {
		if sum, err := sha256File(exe); err == nil {
			p.BinarySHA256["gate"] = sum
		} else {
			p.BinarySHA256["gate"] = "unreadable: " + err.Error()
		}
	}
	if candidate.archivePath != "" {
		candidate.archivePath = rootedPath(repoRoot, candidate.archivePath)
		p.ArchivePath = candidate.archivePath
		if sum, err := sha256File(candidate.archivePath); err == nil {
			p.ArchiveSHA256 = sum
		} else {
			p.ArchiveSHA256 = "unreadable: " + err.Error()
		}
	}
	if candidate.configPath != "" {
		candidate.configPath = rootedPath(repoRoot, candidate.configPath)
		p.ConfigPath = candidate.configPath
		if sum, err := sha256File(candidate.configPath); err == nil {
			p.ConfigSHA256 = sum
		} else {
			p.ConfigSHA256 = "unreadable: " + err.Error()
		}
	}
	serverPath := rootedPath(repoRoot, binaries.Server)
	if out, err := exec.Command(serverPath, "--version").Output(); err == nil { // #nosec G204 -- operator-selected candidate path
		p.ServerVersion = strings.TrimSpace(strings.TrimPrefix(string(out), "OtelContext version "))
	}
	return p
}

func rootedPath(root, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(root, path)
}

func gitOutput(dir string, args ...string) string {
	git := systemTool("git")
	if git == "" {
		return ""
	}
	cmd := exec.Command(git, args...) // #nosec G204 -- absolute path, fixed git subcommands
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path) // #nosec G304 -- gate-configured binary path
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeDigestManifest(path string, entries map[string]string) error {
	names := make([]string, 0, len(entries))
	for name, filePath := range entries {
		if filePath != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var body strings.Builder
	for _, name := range names {
		digest, err := sha256File(entries[name])
		if err != nil {
			return fmt.Errorf("digest %s: %w", name, err)
		}
		fmt.Fprintf(&body, "%s  %s\n", digest, name)
	}
	return os.WriteFile(path, []byte(body.String()), 0o600)
}

// parsePrefillOutput pulls the deterministic seeded counts out of the prefill
// binary's stdout. Those numbers are the only exact totals the prefill tier
// publishes, so the gate reads them rather than assuming them.
type prefillFacts struct {
	WindowsFinalized int
	BucketRows       int64
	DeltaRows        int64
	FirstWindow      int64
	LastWindow       int64
	Series           int
	Services         int
	Requests         int64
	RequestErrors    int64
	Spans            int64
	SpanErrors       int64
	Logs             int64
}

func parsePrefillOutput(out string) (prefillFacts, error) {
	var f prefillFacts
	var seen int
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "windows_finalized:"):
			if _, err := fmt.Sscanf(line, "windows_finalized: %d", &f.WindowsFinalized); err == nil {
				seen++
			}
		case strings.HasPrefix(line, "bucket_rows_written:"):
			if _, err := fmt.Sscanf(line, "bucket_rows_written: %d", &f.BucketRows); err == nil {
				seen++
			}
		case strings.HasPrefix(line, "delta_rows_incorporated:"):
			if _, err := fmt.Sscanf(line, "delta_rows_incorporated: %d", &f.DeltaRows); err == nil {
				seen++
			}
		case strings.HasPrefix(line, "first_window:"):
			if _, err := fmt.Sscanf(line, "first_window: %d  last_window: %d", &f.FirstWindow, &f.LastWindow); err == nil {
				seen++
			}
		case strings.HasPrefix(line, "series_total:"):
			if _, err := fmt.Sscanf(line, "series_total: %d", &f.Series); err == nil {
				seen++
			}
		case strings.HasPrefix(line, "services_total:"):
			if _, err := fmt.Sscanf(line, "services_total: %d", &f.Services); err == nil {
				seen++
			}
		case strings.HasPrefix(line, "dashboard_requests:"):
			if _, err := fmt.Sscanf(line, "dashboard_requests: %d", &f.Requests); err == nil {
				seen++
			}
		case strings.HasPrefix(line, "dashboard_request_errors:"):
			if _, err := fmt.Sscanf(line, "dashboard_request_errors: %d", &f.RequestErrors); err == nil {
				seen++
			}
		case strings.HasPrefix(line, "dashboard_spans:"):
			if _, err := fmt.Sscanf(line, "dashboard_spans: %d", &f.Spans); err == nil {
				seen++
			}
		case strings.HasPrefix(line, "dashboard_span_errors:"):
			if _, err := fmt.Sscanf(line, "dashboard_span_errors: %d", &f.SpanErrors); err == nil {
				seen++
			}
		case strings.HasPrefix(line, "dashboard_logs:"):
			if _, err := fmt.Sscanf(line, "dashboard_logs: %d", &f.Logs); err == nil {
				seen++
			}
		}
	}
	if seen < 11 {
		return f, fmt.Errorf("prefill output did not carry the eleven seeded-count lines (found %d)", seen)
	}
	return f, nil
}
