//go:build gate

package main

import (
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/RandomCodeSpace/otelcontext/test/gate/gatecore"
)

// The sampling loop.
//
// One goroutine owns every periodic measurement: the Prometheus scrape, the
// data-directory walk that feeds the projection, the free-space watermark, and
// the live incarnation's memory evidence. Keeping them on one clock means a
// disk sample and the metric sample it is reasoned about are never more than
// one tick apart.

type sampler struct {
	g        *gate
	interval time.Duration

	mu           sync.Mutex
	phase        string
	proc         *serverProc
	steadyStart  time.Time
	steadyActive bool

	samples     []gatecore.MetricSample
	diskSamples []gatecore.DiskSample
	freeMin     int64
	memory      map[string]*gatecore.MemoryIncarnate
	memOrder    []string
	missing     map[string]int

	// Where the live memory figures came from. Guarded by mu like everything
	// else here: the measurement phase reads them while the loop is still
	// ticking.
	peakSource  string
	oomSource   string
	oomObserved bool

	stop chan struct{}
	done chan struct{}
}

func newSampler(g *gate) *sampler {
	return &sampler{
		g:        g,
		interval: time.Duration(g.cfg.Sampling.IntervalSec * float64(time.Second)),
		phase:    "idle",
		freeMin:  -1,
		memory:   map[string]*gatecore.MemoryIncarnate{},
		missing:  map[string]int{},
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// setPhase labels every subsequent sample.
func (s *sampler) setPhase(name string) {
	s.mu.Lock()
	s.phase = name
	s.mu.Unlock()
}

// setProc points the memory sampler at the live incarnation.
func (s *sampler) setProc(p *serverProc) {
	s.mu.Lock()
	s.proc = p
	if p != nil {
		if _, ok := s.memory[p.label]; !ok {
			s.memory[p.label] = &gatecore.MemoryIncarnate{
				Label: p.label, PID: p.pid, ScopePath: p.scopePath,
			}
			s.memOrder = append(s.memOrder, p.label)
		}
	}
	s.mu.Unlock()
}

// beginSteady starts the projection's sample window. Samples before this are
// warm-up, not steady state.
func (s *sampler) beginSteady(at time.Time) {
	s.mu.Lock()
	s.steadyStart, s.steadyActive = at, true
	s.mu.Unlock()
}

func (s *sampler) endSteady() {
	s.mu.Lock()
	s.steadyActive = false
	s.mu.Unlock()
}

func (s *sampler) run() {
	defer close(s.done)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case now := <-t.C:
			s.tick(now)
		}
	}
}

func (s *sampler) shutdown() {
	close(s.stop)
	<-s.done
}

func (s *sampler) tick(now time.Time) {
	s.mu.Lock()
	phase, proc, steady, steadyStart := s.phase, s.proc, s.steadyActive, s.steadyStart
	s.mu.Unlock()

	// Prometheus.
	if body, err := s.g.scrape(); err == nil {
		if parsed, perr := gatecore.ParsePrometheusText(body); perr == nil {
			values := parsed.Flatten(s.g.cfg.Sampling.Metrics)
			s.mu.Lock()
			for _, req := range s.g.cfg.Sampling.RequiredMetrics {
				if _, ok := parsed.Sum(req); !ok {
					s.missing[req]++
				}
			}
			s.samples = append(s.samples, gatecore.MetricSample{At: now.UTC(), Phase: phase, Values: values})
			s.mu.Unlock()
		}
	}

	// Disk: free-space watermark always, projection samples only while steady.
	if _, free, err := statfs(s.g.cfg.DataDir); err == nil {
		s.mu.Lock()
		if s.freeMin < 0 || free < s.freeMin {
			s.freeMin = free
		}
		s.mu.Unlock()
	}
	if steady {
		if entries, err := walkDataDir(s.g.cfg.DataDir); err == nil {
			ds := gatecore.DiskSample{
				At:            now.UTC(),
				PhysicalBytes: gatecore.MainTierPhysicalBytes(entries, s.g.cfg.Classify.Spec()),
				Windows:       now.Sub(steadyStart).Seconds() / float64(gatecore.WindowSecs),
			}
			s.mu.Lock()
			ds.ChargedBytes = s.chargedLocked()
			s.diskSamples = append(s.diskSamples, ds)
			s.mu.Unlock()
		}
	}

	// Memory of the live incarnation.
	if proc != nil {
		snap := readMemory(proc, s.g.mode)
		s.mu.Lock()
		if m, ok := s.memory[proc.label]; ok {
			if snap.PeakBytes > m.PeakBytes {
				m.PeakBytes = snap.PeakBytes
			}
			if snap.VmHWMBytes > m.VmHWMBytes {
				m.VmHWMBytes = snap.VmHWMBytes
			}
			if snap.OOMKills > m.OOMKills {
				m.OOMKills = snap.OOMKills
			}
			s.peakSource = snap.PeakSource
			s.oomSource = snap.OOMSource
			if snap.OOMObserved {
				s.oomObserved = true
			}
		}
		s.mu.Unlock()
	}
}

// chargedLocked reads the optional logical charged-bytes counter out of the
// most recent scrape. Report-only; see gatecore.FitProjection.
func (s *sampler) chargedLocked() int64 {
	key := s.g.cfg.Sampling.ChargedBytesMetric
	if key == "" || len(s.samples) == 0 {
		return 0
	}
	last := s.samples[len(s.samples)-1]
	if v, ok := last.Values[key]; ok {
		return int64(v)
	}
	return 0
}

// samplerEvidence is everything the measurement phase reads back out of the
// sampling loop, copied under the lock so measure and the ticker never touch
// the same memory.
type samplerEvidence struct {
	Samples     []gatecore.MetricSample
	DiskSamples []gatecore.DiskSample
	FreeMin     int64
	Memory      []gatecore.MemoryIncarnate
	Missing     map[string]int
	PeakSource  string
	OOMSource   string
	OOMObserved bool
}

// snapshot copies out everything the report needs.
func (s *sampler) snapshot() samplerEvidence {
	s.mu.Lock()
	defer s.mu.Unlock()
	ms := make([]gatecore.MetricSample, len(s.samples))
	copy(ms, s.samples)
	ds := make([]gatecore.DiskSample, len(s.diskSamples))
	copy(ds, s.diskSamples)
	mem := make([]gatecore.MemoryIncarnate, 0, len(s.memOrder))
	for _, label := range s.memOrder {
		mem = append(mem, *s.memory[label])
	}
	missing := make(map[string]int, len(s.missing))
	for k, v := range s.missing {
		missing[k] = v
	}
	return samplerEvidence{
		Samples: ms, DiskSamples: ds, FreeMin: s.freeMin, Memory: mem, Missing: missing,
		PeakSource: s.peakSource, OOMSource: s.oomSource, OOMObserved: s.oomObserved,
	}
}

// scrape fetches the Prometheus exposition body.
func (g *gate) scrape() (string, error) {
	req, err := http.NewRequest(http.MethodGet, g.baseURL()+"/metrics/prometheus", nil)
	if err != nil {
		return "", err
	}
	g.authorize(req)
	resp, err := g.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET /metrics/prometheus: HTTP %d", resp.StatusCode)
	}
	return string(body), nil
}
