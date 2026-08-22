package gatecore

import (
	"encoding/json"
	"fmt"
	"time"
)

// Generic JSON inspection for the completeness checks.
//
// The aggregate query surfaces do not carry a uniform `truncated` field: the
// engine pages every store read to completion, so truncation never reaches the
// wire on those endpoints, and the only responses that carry the flag are the
// exemplar-backed ones. Rather than assert a field that may not exist, the
// gate scans the whole decoded document for any `truncated` key and records
// both whether one was present and whether any was true. Absence is reported;
// a true value fails.

// ScanTruncated walks a decoded JSON document for `truncated` keys.
func ScanTruncated(v any) (found bool, isTrue bool) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if k == "truncated" {
				found = true
				if b, ok := val.(bool); ok && b {
					isTrue = true
				}
				continue
			}
			f, tr := ScanTruncated(val)
			found = found || f
			isTrue = isTrue || tr
		}
	case []any:
		for _, val := range t {
			f, tr := ScanTruncated(val)
			found = found || f
			isTrue = isTrue || tr
		}
	}
	return found, isTrue
}

// FindStringField returns the first value of a named string key anywhere in a
// decoded document.
func FindStringField(v any, key string) (string, bool) {
	switch t := v.(type) {
	case map[string]any:
		if raw, ok := t[key]; ok {
			if s, ok := raw.(string); ok {
				return s, true
			}
		}
		for _, val := range t {
			if s, ok := FindStringField(val, key); ok {
				return s, true
			}
		}
	case []any:
		for _, val := range t {
			if s, ok := FindStringField(val, key); ok {
				return s, true
			}
		}
	}
	return "", false
}

// TopLevelScalars pulls the requested numeric fields out of an object
// response. Missing keys are simply absent from the result, so the caller can
// tell "zero" from "not reported".
func TopLevelScalars(v any, keys []string) map[string]float64 {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]float64, len(keys))
	for _, k := range keys {
		if raw, ok := obj[k]; ok {
			if f, ok := raw.(float64); ok {
				out[k] = f
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// WindowPoint is one entry of a per-window traffic response.
type WindowPoint struct {
	Timestamp     time.Time `json:"timestamp"`
	Count         int64     `json:"count"`
	ErrorCount    int64     `json:"error_count"`
	Requests      int64     `json:"requests"`
	RequestErrors int64     `json:"request_errors"`
	Spans         int64     `json:"spans"`
	SpanErrors    int64     `json:"span_errors"`
}

// ParseWindowPoints decodes the bare-array traffic response.
func ParseWindowPoints(body []byte) ([]WindowPoint, error) {
	var pts []WindowPoint
	if err := json.Unmarshal(body, &pts); err != nil {
		return nil, fmt.Errorf("parse per-window response: %w", err)
	}
	return pts, nil
}

// WindowTotals indexes per-window points by aligned window start, summing any
// duplicates the surface happens to emit for one window.
func WindowTotals(pts []WindowPoint, field string, windowSecs int64) map[int64]int64 {
	out := make(map[int64]int64, len(pts))
	for _, p := range pts {
		start := WindowStartFor(p.Timestamp, windowSecs)
		switch field {
		case "spans":
			out[start] += p.Spans
		case "requests":
			out[start] += p.Requests
		case "count":
			out[start] += p.Count
		case "span_errors":
			out[start] += p.SpanErrors
		}
	}
	return out
}

// WindowCoverage compares the window starts a surface returned against the
// window starts that were expected. Returned windows outside the expected set
// are counted as extra rather than silently ignored.
func WindowCoverage(pts []WindowPoint, expected []int64, windowSecs int64) (returned, missing, extra int) {
	have := make(map[int64]struct{}, len(pts))
	for _, p := range pts {
		have[WindowStartFor(p.Timestamp, windowSecs)] = struct{}{}
	}
	want := make(map[int64]struct{}, len(expected))
	for _, w := range expected {
		want[w] = struct{}{}
	}
	for w := range want {
		if _, ok := have[w]; ok {
			returned++
		} else {
			missing++
		}
	}
	for w := range have {
		if _, ok := want[w]; !ok {
			extra++
		}
	}
	return returned, missing, extra
}
