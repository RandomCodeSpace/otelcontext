//go:build gate

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/RandomCodeSpace/otelcontext/test/gate/gatecore"
)

// Process supervision and confinement (Q2).
//
// The server runs inside a cgroup-v2 transient scope so the memory gate
// measures a process that was actually subjected to the production boundary.
// The load generator runs outside the scope: confining the thing that produces
// the load would measure the wrong system.

// cgroupRoot is the unified-hierarchy mount point.
const cgroupRoot = "/sys/fs/cgroup"

// serverProc is one supervised server incarnation.
type serverProc struct {
	label     string
	cmd       *exec.Cmd
	logPath   string
	logFile   *os.File
	unit      string
	scopePath string
	pid       int
	startedAt time.Time
	waitErr   chan error
}

// startServer launches the server under the configured confinement and
// returns once the process exists (not once it is ready).
func (g *gate) startServer(label string, mode gatecore.ConfinementMode) (*serverProc, error) {
	logPath := filepath.Join(g.cfg.WorkDir, "server-"+label+".log")
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) // #nosec G304 -- gate work dir
	if err != nil {
		return nil, err
	}

	unit := fmt.Sprintf("%s-%s-%s.scope", g.cfg.Confinement.UnitPrefix, g.cfg.RunID, label)
	var argv []string
	env := os.Environ()
	for k, v := range g.cfg.ServerEnv {
		env = append(env, k+"="+v)
	}

	switch mode {
	case gatecore.ConfinementCgroup:
		tool := systemTool("systemd-run")
		if tool == "" {
			_ = lf.Close()
			return nil, fmt.Errorf("systemd-run not found in %v", systemToolDirs)
		}
		argv = []string{
			tool, "--user", "--scope", "--collect", "--quiet",
			"--unit=" + unit,
			"-p", fmt.Sprintf("CPUQuota=%d%%", g.cfg.Confinement.CPUQuotaPercent),
			"-p", "MemoryMax=" + g.cfg.Confinement.MemoryMax,
			"--", g.abs(g.cfg.Binaries.Server),
		}
	case gatecore.ConfinementTaskset:
		tool := systemTool("taskset")
		if tool == "" {
			_ = lf.Close()
			return nil, fmt.Errorf("taskset not found in %v", systemToolDirs)
		}
		argv = []string{tool, "-c", g.cfg.Confinement.FallbackCPUs, g.abs(g.cfg.Binaries.Server)}
		env = append(env, fmt.Sprintf("GOMAXPROCS=%d", g.cfg.Confinement.FallbackGOMAXPROCS))
	default:
		_ = lf.Close()
		return nil, fmt.Errorf("unsupported confinement mode %q", mode)
	}

	cmd := exec.Command(argv[0], argv[1:]...) // #nosec G204 -- argv is built from the gate's own config
	cmd.Dir = g.cfg.WorkDir
	cmd.Env = env
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	started := time.Now()
	if err := cmd.Start(); err != nil {
		_ = lf.Close()
		return nil, fmt.Errorf("start server (%s): %w", strings.Join(argv, " "), err)
	}

	p := &serverProc{
		label: label, cmd: cmd, logPath: logPath, logFile: lf,
		unit: unit, pid: cmd.Process.Pid, startedAt: started,
		waitErr: make(chan error, 1),
	}
	go func() { p.waitErr <- cmd.Wait() }()

	g.recordCommand("server-"+label, argv, cmd.Dir, started, 0, -1, logPath, "")

	if mode == gatecore.ConfinementCgroup {
		scope, pid, err := waitForScope(unit, 30*time.Second)
		if err != nil {
			_ = g.stopServer(p)
			return nil, err
		}
		p.scopePath, p.pid = scope, pid
	}
	return p, nil
}

// waitForScope locates the transient scope's cgroup directory and the single
// process inside it.
func waitForScope(unit string, timeout time.Duration) (string, int, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		path, err := findScopePath(unit)
		if err == nil {
			body, rerr := os.ReadFile(filepath.Join(path, "cgroup.procs")) // #nosec G304 -- cgroupfs
			if rerr == nil {
				pids, perr := gatecore.ParseCgroupProcs(string(body))
				if perr == nil && len(pids) > 0 {
					return path, pids[0], nil
				}
				lastErr = perr
			} else {
				lastErr = rerr
			}
		} else {
			lastErr = err
		}
		time.Sleep(200 * time.Millisecond)
	}
	return "", 0, fmt.Errorf("transient scope %s never appeared under %s: %w", unit, cgroupRoot, lastErr)
}

// findScopePath walks the unified hierarchy for a directory named unit.
func findScopePath(unit string) (string, error) {
	var found string
	err := filepath.WalkDir(cgroupRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subtree is not this walk's problem.
			return nil //nolint:nilerr // permission-denied subtrees are skipped by design
		}
		if !d.IsDir() {
			return nil
		}
		if d.Name() == unit {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("no cgroup directory named %s", unit)
	}
	return found, nil
}

// systemToolDirs are the fixed, root-owned directories the gate resolves its
// external tools from. $PATH is deliberately ignored (sonar go:S4036): the
// provenance record depends on running the real git and the confinement
// teardown depends on running the real systemctl, and a writable $PATH entry
// would let either be substituted without the report noticing.
var systemToolDirs = []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin", "/usr/local/bin"}

// systemTool resolves an external tool to an absolute path under
// systemToolDirs, or returns "" when it is not installed in any of them.
func systemTool(name string) string {
	for _, dir := range systemToolDirs {
		abs := filepath.Join(dir, name)
		fi, err := os.Stat(abs)
		if err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return abs
		}
	}
	return ""
}

// cgroupProbe reports whether delegated cgroup control is usable here, so the
// gate can pick its confinement mode before it commits to a three-hour run.
func (g *gate) cgroupProbe() error {
	if _, err := os.Stat(filepath.Join(cgroupRoot, "cgroup.controllers")); err != nil {
		return fmt.Errorf("no cgroup-v2 unified hierarchy at %s: %w", cgroupRoot, err)
	}
	tool := systemTool("systemd-run")
	if tool == "" {
		return fmt.Errorf("systemd-run not found in %v", systemToolDirs)
	}
	unit := fmt.Sprintf("%s-%s-probe.scope", g.cfg.Confinement.UnitPrefix, g.cfg.RunID)
	argv := []string{
		tool, "--user", "--scope", "--collect", "--quiet", "--unit=" + unit,
		"-p", fmt.Sprintf("CPUQuota=%d%%", g.cfg.Confinement.CPUQuotaPercent),
		"-p", "MemoryMax=" + g.cfg.Confinement.MemoryMax,
		"--", "/bin/true",
	}
	cmd := exec.Command(argv[0], argv[1:]...) // #nosec G204 -- argv is built from the gate's own config
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("cgroup scope probe failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// readConfinement reads the boundary back out of the kernel.
func readConfinement(p *serverProc, mode gatecore.ConfinementMode, cfg gatecore.ConfinementConfig) (gatecore.Confinement, error) {
	c := gatecore.Confinement{Mode: mode, Unit: p.unit}
	if mode == gatecore.ConfinementTaskset {
		c.TasksetCPUs = cfg.FallbackCPUs
		c.GOMAXPROCS = cfg.FallbackGOMAXPROCS
		c.EffectiveCPUs = float64(len(strings.Split(cfg.FallbackCPUs, ",")))
		c.Note = gatecore.TasksetNote
		return c, nil
	}

	c.ScopePath = p.scopePath
	c.Note = gatecore.CgroupNote

	raw, err := os.ReadFile(filepath.Join(p.scopePath, "cpu.max")) // #nosec G304 -- cgroupfs
	if err != nil {
		return c, fmt.Errorf("read cpu.max: %w", err)
	}
	cpu, err := gatecore.ParseCPUMax(string(raw))
	if err != nil {
		return c, err
	}
	c.CPUMaxRaw, c.CPUQuotaUsec, c.CPUPeriodUsec, c.EffectiveCPUs =
		cpu.Raw, cpu.QuotaUsec, cpu.PeriodUsec, cpu.CPUs

	raw, err = os.ReadFile(filepath.Join(p.scopePath, "memory.max")) // #nosec G304 -- cgroupfs
	if err != nil {
		return c, fmt.Errorf("read memory.max: %w", err)
	}
	c.MemoryMaxRaw = strings.TrimSpace(string(raw))
	bytes, bounded, err := gatecore.ParseMemoryMax(c.MemoryMaxRaw)
	if err != nil {
		return c, err
	}
	if !bounded {
		return c, errors.New("memory.max is 'max': the scope carries no memory bound, so the memory gate would measure nothing")
	}
	c.MemoryMaxByte = bytes
	return c, nil
}

// memorySnapshot is one read of a live incarnation's memory evidence.
type memorySnapshot struct {
	PeakBytes   int64
	VmHWMBytes  int64
	OOMKills    int64
	OOMObserved bool
	PeakSource  string
	OOMSource   string
}

// readMemory samples the live incarnation. In cgroup mode memory.peak and
// memory.events are the load-bearing numbers; VmHWM is secondary evidence and
// is read in both modes.
func readMemory(p *serverProc, mode gatecore.ConfinementMode) memorySnapshot {
	var s memorySnapshot
	if body, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", p.pid)); err == nil { // #nosec G304 -- procfs
		if v, err := gatecore.ParseVmHWM(string(body)); err == nil {
			s.VmHWMBytes = v
		}
	}
	if mode != gatecore.ConfinementCgroup || p.scopePath == "" {
		s.PeakBytes = s.VmHWMBytes
		s.PeakSource = fmt.Sprintf("/proc/%d/status VmHWM", p.pid)
		s.OOMSource = "unavailable in taskset-fallback"
		return s
	}
	s.PeakSource = filepath.Join(p.scopePath, "memory.peak")
	s.OOMSource = filepath.Join(p.scopePath, "memory.events")
	if body, err := os.ReadFile(s.PeakSource); err == nil { // #nosec G304 -- cgroupfs
		if v, err := gatecore.ParseMemoryPeak(string(body)); err == nil {
			s.PeakBytes = v
		}
	}
	if body, err := os.ReadFile(s.OOMSource); err == nil { // #nosec G304 -- cgroupfs
		if ev, err := gatecore.ParseMemoryEvents(string(body)); err == nil {
			if v, ok := ev["oom_kill"]; ok {
				s.OOMKills, s.OOMObserved = v, true
			}
		}
	}
	return s
}

// kill9 sends SIGKILL to the server process itself — the crash the gate is
// here to survive.
func kill9(p *serverProc) error {
	if p.pid <= 0 {
		return errors.New("no server pid to kill")
	}
	return syscall.Kill(p.pid, syscall.SIGKILL)
}

// stopServer tears down an incarnation and everything it spawned: the process
// group first, then the transient scope, so nothing survives into the next
// phase and quietly competes for the same ports.
func (g *gate) stopServer(p *serverProc) error {
	if p == nil {
		return nil
	}
	var firstErr error
	if p.cmd.Process != nil {
		if err := syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			firstErr = err
		}
	}
	select {
	case <-p.waitErr:
	case <-time.After(time.Duration(g.cfg.ShutdownTimeoutSec) * time.Second):
		if p.cmd.Process != nil {
			_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		}
		<-p.waitErr
	}
	if sc := systemTool("systemctl"); p.unit != "" && sc != "" {
		stop := exec.Command(sc, "--user", "stop", p.unit) // #nosec G204 -- absolute path, gate-generated unit
		_ = stop.Run()
	}
	if p.logFile != nil {
		_ = p.logFile.Close()
	}
	return firstErr
}

// reap waits for an already-killed incarnation and closes its log.
func reap(p *serverProc) {
	if p == nil {
		return
	}
	select {
	case <-p.waitErr:
	case <-time.After(30 * time.Second):
	}
	if sc := systemTool("systemctl"); p.unit != "" && sc != "" {
		_ = exec.Command(sc, "--user", "stop", p.unit).Run() // #nosec G204 -- absolute path, gate-generated unit
	}
	if p.logFile != nil {
		_ = p.logFile.Close()
	}
}

// bgCommand is a helper started in the background so the orchestrator can
// drive phase boundaries and sampling while it runs.
type bgCommand struct {
	phase   string
	argv    []string
	cmd     *exec.Cmd
	logPath string
	buf     *strings.Builder
	started time.Time
	done    chan error
}

// startCommand launches a helper and returns immediately.
func (g *gate) startCommand(phase string, argv []string, logName string) (*bgCommand, error) {
	logPath := filepath.Join(g.cfg.WorkDir, logName)
	lf, err := os.Create(logPath) // #nosec G304 -- gate work dir
	if err != nil {
		return nil, err
	}

	b := &bgCommand{phase: phase, argv: argv, logPath: logPath, buf: &strings.Builder{}, done: make(chan error, 1)}
	cmd := exec.Command(argv[0], argv[1:]...) // #nosec G204 -- argv is built from the gate's own config
	cmd.Dir = g.cfg.WorkDir
	cmd.Stdout = &teeWriter{a: lf, b: b.buf}
	cmd.Stderr = &teeWriter{a: lf, b: b.buf}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	b.cmd = cmd
	b.started = time.Now()
	if err := cmd.Start(); err != nil {
		_ = lf.Close()
		return nil, fmt.Errorf("start %s: %w", strings.Join(argv, " "), err)
	}
	go func() {
		err := cmd.Wait()
		_ = lf.Close()
		b.done <- err
	}()
	return b, nil
}

// waitCommand blocks until the helper exits, then records the invocation.
func (g *gate) waitCommand(b *bgCommand) (string, error) {
	runErr := <-b.done
	exit := 0
	msg := ""
	if runErr != nil {
		exit = -1
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			exit = ee.ExitCode()
		}
		msg = runErr.Error()
	}
	g.recordCommand(b.phase, b.argv, b.cmd.Dir, b.started, time.Since(b.started).Seconds(), exit, b.logPath, msg)
	return b.buf.String(), runErr
}

// killCommand tears down a helper and everything it spawned.
func killCommand(b *bgCommand) {
	if b == nil || b.cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-b.cmd.Process.Pid, syscall.SIGKILL)
	select {
	case <-b.done:
	case <-time.After(10 * time.Second):
	}
}

// runCommand starts a helper and waits for it.
func (g *gate) runCommand(phase string, argv []string, logName string) (string, error) {
	b, err := g.startCommand(phase, argv, logName)
	if err != nil {
		return "", err
	}
	return g.waitCommand(b)
}

// teeWriter fans one stream out to two sinks.
type teeWriter struct {
	a *os.File
	b *strings.Builder
}

func (t *teeWriter) Write(p []byte) (int, error) {
	_, _ = t.a.Write(p)
	return t.b.Write(p)
}

// walkDataDir lists every regular file under root, relative to it.
func walkDataDir(root string) ([]gatecore.FileEntry, error) {
	var out []gatecore.FileEntry
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, gatecore.FileEntry{RelPath: filepath.ToSlash(rel), Bytes: info.Size()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RelPath < out[j].RelPath })
	return out, nil
}
