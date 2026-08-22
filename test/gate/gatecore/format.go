package gatecore

import (
	"fmt"
	"math"
	"time"
)

// Formatting helpers shared by the assertion table and the Markdown renderer,
// so a threshold reads the same in both.

// HumanBytes renders a byte count with a binary unit.
func HumanBytes(b int64) string {
	const unit = 1024
	if b < 0 {
		return fmt.Sprintf("-%s", HumanBytes(-b))
	}
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit && exp < 4; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(b)/float64(div), "KMGTP"[exp])
}

// Ms renders a millisecond figure.
func Ms(v float64) string { return fmt.Sprintf("%.1f ms", v) }

// Pct renders a ratio as a percentage with enough digits to see 99.9%.
func Pct(v float64) string { return fmt.Sprintf("%.4f%%", v*100) }

// Rate renders a points-per-second figure.
func Rate(v float64) string { return fmt.Sprintf("%.0f pts/s", v) }

// Secs renders a duration in seconds.
func Secs(v float64) string { return fmt.Sprintf("%.1f s", v) }

// Dur renders a duration compactly.
func Dur(d time.Duration) string { return d.Truncate(time.Second).String() }

// Count renders an integer count.
func Count(v int64) string { return fmt.Sprintf("%d", v) }

// Float renders a float with two decimals.
func Float(v float64) string {
	if math.IsNaN(v) {
		return "NaN"
	}
	if math.IsInf(v, 0) {
		return "Inf"
	}
	return fmt.Sprintf("%.2f", v)
}
