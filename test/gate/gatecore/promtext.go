package gatecore

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// A small Prometheus text-exposition parser.
//
// The gate needs a handful of counters and gauges out of /metrics/prometheus
// and must fail loudly when one of them is absent. Pulling in a parsing
// dependency for that would be the wrong trade, and the format the gate reads
// is the stable, documented subset: one sample per line, optional labels, one
// float value, comments ignored.

// PromSample is one exposed series.
type PromSample struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
	Value  float64           `json:"value"`
}

// PromSamples is a scrape.
type PromSamples []PromSample

// ParsePrometheusText parses an exposition-format body.
//
// Malformed lines are an error, not a shrug: a scrape the gate cannot read is
// a scrape the gate must not score.
func ParsePrometheusText(body string) (PromSamples, error) {
	var out PromSamples
	for lineNo, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		s, err := parsePromLine(line)
		if err != nil {
			return nil, fmt.Errorf("prometheus line %d: %w", lineNo+1, err)
		}
		out = append(out, s)
	}
	return out, nil
}

func parsePromLine(line string) (PromSample, error) {
	var s PromSample
	nameEnd := strings.IndexAny(line, "{ \t")
	if nameEnd < 0 {
		return s, fmt.Errorf("no value in %q", line)
	}
	s.Name = line[:nameEnd]
	rest := line[nameEnd:]

	if strings.HasPrefix(rest, "{") {
		close := strings.LastIndex(rest, "}")
		if close < 0 {
			return s, fmt.Errorf("unterminated label set in %q", line)
		}
		labels, err := parsePromLabels(rest[1:close])
		if err != nil {
			return s, err
		}
		s.Labels = labels
		rest = rest[close+1:]
	}

	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return s, fmt.Errorf("no value in %q", line)
	}
	v, err := parsePromValue(fields[0])
	if err != nil {
		return s, fmt.Errorf("value %q: %w", fields[0], err)
	}
	s.Value = v
	return s, nil
}

func parsePromValue(tok string) (float64, error) {
	switch tok {
	case "+Inf":
		return math.Inf(1), nil
	case "-Inf":
		return math.Inf(-1), nil
	case "NaN":
		return math.NaN(), nil
	}
	return strconv.ParseFloat(tok, 64)
}

// parsePromLabels parses the inside of a label set: k="v",k2="v2".
func parsePromLabels(s string) (map[string]string, error) {
	labels := make(map[string]string)
	i := 0
	for i < len(s) {
		for i < len(s) && (s[i] == ' ' || s[i] == ',') {
			i++
		}
		if i >= len(s) {
			break
		}
		eq := strings.IndexByte(s[i:], '=')
		if eq < 0 {
			return nil, fmt.Errorf("label without '=' in %q", s)
		}
		key := strings.TrimSpace(s[i : i+eq])
		i += eq + 1
		if i >= len(s) || s[i] != '"' {
			return nil, fmt.Errorf("label %q value is not quoted", key)
		}
		i++
		var b strings.Builder
		closed := false
		for i < len(s) {
			c := s[i]
			if c == '\\' && i+1 < len(s) {
				i++
				switch s[i] {
				case 'n':
					b.WriteByte('\n')
				case 't':
					b.WriteByte('\t')
				default:
					b.WriteByte(s[i])
				}
				i++
				continue
			}
			if c == '"' {
				i++
				closed = true
				break
			}
			b.WriteByte(c)
			i++
		}
		if !closed {
			return nil, fmt.Errorf("label %q value is unterminated", key)
		}
		labels[key] = b.String()
	}
	return labels, nil
}

// Sum totals every series with the given name. found is false when the metric
// is absent entirely — which the gate treats as a failure, not as zero.
func (ps PromSamples) Sum(name string) (total float64, found bool) {
	for _, s := range ps {
		if s.Name == name {
			total += s.Value
			found = true
		}
	}
	return total, found
}

// Get returns the value of the series matching every supplied label.
func (ps PromSamples) Get(name string, labels map[string]string) (float64, bool) {
	for _, s := range ps {
		if s.Name != name || !matchesLabels(s.Labels, labels) {
			continue
		}
		return s.Value, true
	}
	return 0, false
}

func matchesLabels(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// ByLabel indexes one metric's series by the value of one label, summing
// duplicates.
func (ps PromSamples) ByLabel(name, label string) map[string]float64 {
	out := make(map[string]float64)
	for _, s := range ps {
		if s.Name != name {
			continue
		}
		out[s.Labels[label]] += s.Value
	}
	return out
}

// Flatten renders the scrape as the flat key/value map a MetricSample carries,
// keeping only the requested metric names. Keys are `name` for unlabelled
// series and `name{k="v",...}` for labelled ones, with labels sorted so the
// key is stable across scrapes.
func (ps PromSamples) Flatten(names []string) map[string]float64 {
	want := make(map[string]struct{}, len(names))
	for _, n := range names {
		want[n] = struct{}{}
	}
	out := make(map[string]float64, len(names))
	for _, s := range ps {
		if _, ok := want[s.Name]; !ok {
			continue
		}
		out[s.Key()] = s.Value
	}
	return out
}

// Key renders the sample's stable identity.
func (s PromSample) Key() string {
	if len(s.Labels) == 0 {
		return s.Name
	}
	keys := make([]string, 0, len(s.Labels))
	for k := range s.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(s.Name)
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString(`="`)
		b.WriteString(s.Labels[k])
		b.WriteString(`"`)
	}
	b.WriteByte('}')
	return b.String()
}
