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
func collectProvenance(repoRoot string, binaries gatecore.Binaries) gatecore.Provenance {
	p := gatecore.Provenance{
		GoVersion:       runtime.Version(),
		BinarySHA256:    map[string]string{},
		BuiltAt:         time.Now().UTC(),
		OrchestratorPID: os.Getpid(),
	}
	p.CommitSHA = gitOutput(repoRoot, "rev-parse", "HEAD")
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
		if sum, err := sha256File(path); err == nil {
			p.BinarySHA256[name+" ("+filepath.Base(path)+")"] = sum
		} else {
			p.BinarySHA256[name+" ("+filepath.Base(path)+")"] = "unreadable: " + err.Error()
		}
	}
	return p
}

func gitOutput(dir string, args ...string) string {
	cmd := exec.Command("git", args...) // #nosec G204 -- fixed git subcommands
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

// parsePrefillOutput pulls the deterministic seeded counts out of the prefill
// binary's stdout. Those numbers are the only exact totals the prefill tier
// publishes, so the gate reads them rather than assuming them.
type prefillFacts struct {
	WindowsFinalized int
	BucketRows       int64
	DeltaRows        int64
	FirstWindow      int64
	LastWindow       int64
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
		}
	}
	if seen < 4 {
		return f, fmt.Errorf("prefill output did not carry the four seeded-count lines (found %d)", seen)
	}
	return f, nil
}
