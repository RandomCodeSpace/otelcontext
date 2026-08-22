package gatecore

import (
	"fmt"
	"math"
	"time"
)

// The main-tier projection (Q4).
//
// One thing is calculated, once: PHYSICAL allocated bytes per completed
// five-minute window, fitted over the steady portion of the run, multiplied by
// the 576-window two-day horizon.
//
// The slope is already physical — it is the difference between two filesystem
// measurements of the same files, so it contains the indexes, the FTS shadow
// tables, the WAL, the SHM and the free pages. Multiplying it by an
// amplification factor would charge the indexes twice. Amplification is
// therefore computed for the report and never fed back into the projection.

// DiskSample is one main-tier measurement taken during the steady portion.
type DiskSample struct {
	At time.Time `json:"at"`
	// PhysicalBytes is the allocated size of every file in the tier: main DB
	// plus its -wal and -shm sidecars. Free pages inside the file count,
	// because the operator's volume pays for them.
	PhysicalBytes int64 `json:"physical_bytes"`
	// ChargedBytes is the logical byte count the server says it wrote, if a
	// counter for it was configured. Optional; report-only.
	ChargedBytes int64 `json:"charged_bytes,omitempty"`
	// Windows is completed five-minute windows since the steady portion began.
	Windows float64 `json:"windows"`
}

// Projection is the labelled two-day main-tier estimate.
type Projection struct {
	Evaluated  bool   `json:"evaluated"`
	Label      string `json:"label"`
	Error      string `json:"error,omitempty"`
	MetricNote string `json:"metric_note,omitempty"`

	Samples      []DiskSample `json:"samples"`
	SampleCount  int          `json:"sample_count"`
	FirstAt      time.Time    `json:"first_sample_at"`
	LastAt       time.Time    `json:"last_sample_at"`
	ObservedMin  int64        `json:"observed_min_bytes"`
	ObservedMax  int64        `json:"observed_max_bytes"`
	WindowsSpan  float64      `json:"windows_observed"`
	HorizonWinds int          `json:"horizon_windows"`

	Fit                Fit     `json:"fit"`
	BytesPerWindow     float64 `json:"physical_bytes_per_window"`
	UpperBytesPerWinds float64 `json:"physical_bytes_per_window_upper"`
	ZScore             float64 `json:"upper_estimate_z"`

	ProjectedBytes      int64 `json:"projected_bytes"`
	ProjectedUpperBytes int64 `json:"projected_upper_bytes"`

	// AmplificationFactor is physical growth / charged growth over the same
	// samples. Report-only: it is NEVER multiplied into the projection.
	AmplificationMeasured bool    `json:"amplification_measured"`
	ChargedBytesPerWindow float64 `json:"charged_bytes_per_window,omitempty"`
	AmplificationFactor   float64 `json:"amplification_factor,omitempty"`
}

// FitProjection turns steady-portion samples into the labelled projection.
//
// horizonWindows is the retention horizon of the tier in completed windows
// (576 for the two-day main tier). z is how many slope standard errors the
// conservative upper estimate is pushed out by.
func FitProjection(samples []DiskSample, horizonWindows int, z float64, minSamples int) Projection {
	p := Projection{
		Samples:      samples,
		SampleCount:  len(samples),
		HorizonWinds: horizonWindows,
		ZScore:       z,
		Label: fmt.Sprintf("PROJECTION — not a measurement. Physical bytes/window fitted over the steady "+
			"portion and multiplied by the %d-window (two-day) horizon.", horizonWindows),
	}
	if len(samples) < minSamples {
		p.Error = fmt.Sprintf("%d steady-portion samples, need at least %d", len(samples), minSamples)
		return p
	}

	pts := make([]Point, 0, len(samples))
	chargedPts := make([]Point, 0, len(samples))
	haveCharged := true
	for _, s := range samples {
		pts = append(pts, Point{X: s.Windows, Y: float64(s.PhysicalBytes)})
		if s.ChargedBytes <= 0 {
			haveCharged = false
		}
		chargedPts = append(chargedPts, Point{X: s.Windows, Y: float64(s.ChargedBytes)})
	}

	f, err := FitLine(pts, minSamples)
	if err != nil {
		p.Error = err.Error()
		return p
	}

	p.Evaluated = true
	p.Fit = f
	p.FirstAt = samples[0].At
	p.LastAt = samples[len(samples)-1].At
	p.ObservedMin = int64(f.YMin)
	p.ObservedMax = int64(f.YMax)
	p.WindowsSpan = f.XSpan
	p.BytesPerWindow = f.Slope
	p.UpperBytesPerWinds = f.UpperSlope(z)

	// A tier that shrank over the steady portion projects to zero growth, not
	// to a negative footprint.
	p.ProjectedBytes = int64(math.Max(0, f.Slope*float64(horizonWindows)))
	p.ProjectedUpperBytes = int64(math.Max(0, p.UpperBytesPerWinds*float64(horizonWindows)))

	if haveCharged {
		if cf, cerr := FitLine(chargedPts, minSamples); cerr == nil && cf.Slope > 0 {
			p.AmplificationMeasured = true
			p.ChargedBytesPerWindow = cf.Slope
			p.AmplificationFactor = f.Slope / cf.Slope
		}
	}
	if !p.AmplificationMeasured {
		p.MetricNote = "no logical charged-bytes counter was configured for the main tier, so the " +
			"physical/charged amplification factor is not measured. The projection does not need it."
	}
	return p
}
