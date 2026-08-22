package gatecore

import (
	"fmt"
	"strconv"
	"strings"
)

// cgroup-v2 and /proc file parsing (Q2).
//
// Every one of these takes a string, so the gate's confinement verification is
// unit-tested against fixture contents instead of against whatever the CI
// runner's kernel happens to expose.

// CPUMax is a parsed cpu.max.
type CPUMax struct {
	Raw string `json:"raw"`
	// QuotaUsec is -1 when the quota is "max" (unbounded).
	QuotaUsec  int64   `json:"quota_usec"`
	PeriodUsec int64   `json:"period_usec"`
	Unbounded  bool    `json:"unbounded"`
	CPUs       float64 `json:"effective_cpus"`
}

// ParseCPUMax parses the contents of cpu.max: "<quota|max> <period>".
func ParseCPUMax(s string) (CPUMax, error) {
	c := CPUMax{Raw: strings.TrimSpace(s)}
	fields := strings.Fields(c.Raw)
	if len(fields) != 2 {
		return c, fmt.Errorf("cpu.max: want 2 fields, got %d in %q", len(fields), c.Raw)
	}
	period, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return c, fmt.Errorf("cpu.max period %q: %w", fields[1], err)
	}
	if period <= 0 {
		return c, fmt.Errorf("cpu.max period %d must be positive", period)
	}
	c.PeriodUsec = period

	if fields[0] == "max" {
		c.Unbounded = true
		c.QuotaUsec = -1
		return c, nil
	}
	quota, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return c, fmt.Errorf("cpu.max quota %q: %w", fields[0], err)
	}
	c.QuotaUsec = quota
	c.CPUs = float64(quota) / float64(period)
	return c, nil
}

// ParseMemoryMax parses memory.max. "max" returns (-1, false, nil): unbounded,
// which the gate treats as a failed confinement rather than a huge limit.
func ParseMemoryMax(s string) (bytes int64, bounded bool, err error) {
	t := strings.TrimSpace(s)
	if t == "max" {
		return -1, false, nil
	}
	v, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("memory.max %q: %w", t, err)
	}
	return v, true, nil
}

// ParseMemoryPeak parses memory.peak, the high-water mark of the cgroup.
func ParseMemoryPeak(s string) (int64, error) {
	t := strings.TrimSpace(s)
	v, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("memory.peak %q: %w", t, err)
	}
	return v, nil
}

// ParseMemoryEvents parses memory.events into its key/count pairs. The gate
// reads oom_kill out of it; a missing key is reported as absent, never as 0.
func ParseMemoryEvents(s string) (map[string]int64, error) {
	out := make(map[string]int64)
	for _, raw := range strings.Split(s, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("memory.events: want 'key value', got %q", line)
		}
		v, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("memory.events %q: %w", line, err)
		}
		out[fields[0]] = v
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("memory.events is empty")
	}
	return out, nil
}

// ParseVmHWM pulls the peak resident set size out of /proc/<pid>/status and
// returns it in bytes. The kernel reports it in kB.
func ParseVmHWM(status string) (int64, error) {
	for _, raw := range strings.Split(status, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "VmHWM:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "VmHWM:"))
		if len(fields) < 1 {
			return 0, fmt.Errorf("VmHWM line has no value: %q", line)
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("VmHWM %q: %w", fields[0], err)
		}
		return kb * 1024, nil
	}
	return 0, fmt.Errorf("no VmHWM line in /proc status (kernel too old, or the process exited)")
}

// ParseCgroupProcs parses a cgroup.procs file into PIDs.
func ParseCgroupProcs(s string) ([]int, error) {
	var out []int
	for _, raw := range strings.Split(s, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		pid, err := strconv.Atoi(line)
		if err != nil {
			return nil, fmt.Errorf("cgroup.procs %q: %w", line, err)
		}
		out = append(out, pid)
	}
	return out, nil
}

// ParseProcSelfCgroup returns the unified-hierarchy path from a
// /proc/<pid>/cgroup body. On cgroup-v2 the line is "0::/some/path".
func ParseProcSelfCgroup(s string) (string, error) {
	for _, raw := range strings.Split(s, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "0::") {
			continue
		}
		return strings.TrimPrefix(line, "0::"), nil
	}
	return "", fmt.Errorf("no cgroup-v2 (0::) line: this host is not on the unified hierarchy")
}

// ParseMemTotal pulls MemTotal out of /proc/meminfo, in bytes.
func ParseMemTotal(meminfo string) (int64, error) {
	for _, raw := range strings.Split(meminfo, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "MemTotal:"))
		if len(fields) < 1 {
			return 0, fmt.Errorf("MemTotal line has no value: %q", line)
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("MemTotal %q: %w", fields[0], err)
		}
		return kb * 1024, nil
	}
	return 0, fmt.Errorf("no MemTotal line in /proc/meminfo")
}
