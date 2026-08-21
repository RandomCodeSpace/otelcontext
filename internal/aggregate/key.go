// Package aggregate implements the aggregate-first accounting engine: every
// accepted telemetry point is reduced into a small number of series deltas
// before sampling, so aggregate counts describe traffic rather than the
// sampling rate.
//
// This file owns series identity. A series is identified by a SeriesKey — a
// fixed struct of dictionary IDs plus small bounded enums (ADR 0001, issue
// #159). The struct is directly comparable and therefore usable as a Go map
// key, and it is serialized field-wise with an explicit little-endian layout
// and a leading version byte. Raw struct memory is never persisted: padding,
// field reordering and endianness would all silently corrupt identity across
// builds.
package aggregate

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// Signal names the telemetry stream a series belongs to. It also selects the
// dictionary namespace that SeriesKey.NameID is resolved through; readers must
// never join NameIDs across namespaces.
type Signal uint8

// Signal values. Zero is reserved for "unspecified" so a zero-valued SeriesKey
// is never mistaken for a real series.
const (
	SignalUnspecified Signal = 0
	// SignalTraceOp is a per-operation trace series. NameID resolves through
	// KindOperation.
	SignalTraceOp Signal = 1
	// SignalServiceEdge is a caller/callee service edge series. NameID
	// resolves through KindOperation.
	SignalServiceEdge Signal = 2
	// SignalLog is a log series. NameID resolves through KindLogTemplate.
	SignalLog Signal = 3
	// SignalMetric is a native metric series. NameID resolves through
	// KindMetricName.
	SignalMetric Signal = 4

	signalMax = SignalMetric
)

// String implements fmt.Stringer. The lowercase forms double as metric label
// values, so they are part of the exported contract.
func (s Signal) String() string {
	switch s {
	case SignalTraceOp:
		return "trace_op"
	case SignalServiceEdge:
		return "service_edge"
	case SignalLog:
		return "log"
	case SignalMetric:
		return "metric"
	case SignalUnspecified:
		return "unspecified"
	default:
		return fmt.Sprintf("signal(%d)", uint8(s))
	}
}

// Valid reports whether s is one of the four defined signals.
func (s Signal) Valid() bool { return s >= SignalTraceOp && s <= signalMax }

// StatusClass is the per-signal status dimension of a SeriesKey. Its meaning
// depends on the Signal:
//
//	SignalTraceOp, SignalServiceEdge — OTLP span status: StatusUnset/OK/Error.
//	SignalLog                       — severity tier: SeverityTierTrace..Fatal.
//	SignalMetric                    — always 0.
type StatusClass uint8

// Span-status values of StatusClass, mirroring the OTLP status code numbering.
const (
	StatusUnset StatusClass = 0
	StatusOK    StatusClass = 1
	StatusError StatusClass = 2

	statusMax = StatusError
)

// Severity-tier values of StatusClass. OTLP severity numbers collapse into six
// tiers; the raw number never reaches series identity.
const (
	SeverityTierUnspecified StatusClass = 0
	SeverityTierTrace       StatusClass = 1
	SeverityTierDebug       StatusClass = 2
	SeverityTierInfo        StatusClass = 3
	SeverityTierWarn        StatusClass = 4
	SeverityTierError       StatusClass = 5
	SeverityTierFatal       StatusClass = 6

	severityTierMax = SeverityTierFatal
)

// TraceStatusFromCode maps an OTLP span status code onto a StatusClass.
// Unrecognized codes degrade to StatusUnset rather than inventing identity.
func TraceStatusFromCode(code int32) StatusClass {
	switch code {
	case 1:
		return StatusOK
	case 2:
		return StatusError
	default:
		return StatusUnset
	}
}

// SeverityTierFromNumber maps an OTLP severity number (1..24) onto a severity
// tier. Numbers outside the range yield SeverityTierUnspecified.
func SeverityTierFromNumber(sev int32) StatusClass {
	if sev < 1 || sev > 24 {
		return SeverityTierUnspecified
	}
	return StatusClass((sev-1)/4 + 1)
}

// HTTPClass is the HTTP status family of an operation. It carries the 4xx/5xx
// triage split separately from StatusClass because a 4xx is legitimately not a
// span ERROR.
type HTTPClass uint8

// HTTPClass values.
const (
	HTTPClassNone HTTPClass = 0
	HTTPClass1xx  HTTPClass = 1
	HTTPClass2xx  HTTPClass = 2
	HTTPClass3xx  HTTPClass = 3
	HTTPClass4xx  HTTPClass = 4
	HTTPClass5xx  HTTPClass = 5

	httpClassMax = HTTPClass5xx
)

// String implements fmt.Stringer.
func (h HTTPClass) String() string {
	switch h {
	case HTTPClassNone:
		return "none"
	case HTTPClass1xx:
		return "1xx"
	case HTTPClass2xx:
		return "2xx"
	case HTTPClass3xx:
		return "3xx"
	case HTTPClass4xx:
		return "4xx"
	case HTTPClass5xx:
		return "5xx"
	default:
		return fmt.Sprintf("httpclass(%d)", uint8(h))
	}
}

// HTTPClassFromStatus maps an HTTP status code onto its family. Codes outside
// 100..599 yield HTTPClassNone.
func HTTPClassFromStatus(code int) HTTPClass {
	if code < 100 || code > 599 {
		return HTTPClassNone
	}
	return HTTPClass(code / 100)
}

// Method is the bounded HTTP method enum. It is bounded, not closed:
// unrecognized methods map to MethodOther so a hostile client cannot mint
// series identity.
type Method uint8

// Method values.
const (
	MethodNone    Method = 0
	MethodGet     Method = 1
	MethodPost    Method = 2
	MethodPut     Method = 3
	MethodDelete  Method = 4
	MethodPatch   Method = 5
	MethodHead    Method = 6
	MethodOptions Method = 7
	MethodTrace   Method = 8
	MethodConnect Method = 9
	MethodOther   Method = 10

	methodMax = MethodOther
)

// methodNames is indexed by Method.
var methodNames = [...]string{
	MethodNone:    "",
	MethodGet:     "GET",
	MethodPost:    "POST",
	MethodPut:     "PUT",
	MethodDelete:  "DELETE",
	MethodPatch:   "PATCH",
	MethodHead:    "HEAD",
	MethodOptions: "OPTIONS",
	MethodTrace:   "TRACE",
	MethodConnect: "CONNECT",
	MethodOther:   "_OTHER",
}

// String implements fmt.Stringer. Known methods render in their canonical
// uppercase form; MethodNone renders as the empty string.
func (m Method) String() string {
	if int(m) < len(methodNames) {
		return methodNames[m]
	}
	return fmt.Sprintf("method(%d)", uint8(m))
}

// LookupMethod resolves s against the known methods only. It reports ok=false
// for the empty string and for anything unrecognized; callers that want the
// bounded-enum degradation should use ParseMethod instead. Matching is
// case-insensitive so lowercase "get" from sloppy instrumentation still lands
// on MethodGet.
func LookupMethod(s string) (Method, bool) {
	// Exact match first: the overwhelmingly common case, and allocation-free.
	for m := MethodGet; m < MethodOther; m++ {
		if methodNames[m] == s {
			return m, true
		}
	}
	if s == "" {
		return MethodNone, false
	}
	for m := MethodGet; m < MethodOther; m++ {
		if strings.EqualFold(methodNames[m], s) {
			return m, true
		}
	}
	return MethodOther, false
}

// ParseMethod maps an HTTP method string onto the bounded enum. The empty
// string yields MethodNone; anything unrecognized yields MethodOther.
func ParseMethod(s string) Method {
	if s == "" {
		return MethodNone
	}
	if m, ok := LookupMethod(s); ok {
		return m
	}
	return MethodOther
}

// Variant is the signal-specific variant dimension. For traces and edges it is
// the OTLP SpanKind; every other signal pins it to zero.
type Variant uint8

// SpanKind values of Variant, mirroring the OTLP SpanKind numbering.
const (
	SpanKindUnspecified Variant = 0
	SpanKindInternal    Variant = 1
	SpanKindServer      Variant = 2
	SpanKindClient      Variant = 3
	SpanKindProducer    Variant = 4
	SpanKindConsumer    Variant = 5

	spanKindMax = SpanKindConsumer
)

// VariantFromSpanKind maps an OTLP SpanKind number onto a Variant. Out-of-range
// kinds degrade to SpanKindUnspecified.
func VariantFromSpanKind(kind int32) Variant {
	if kind < 0 || kind > int32(spanKindMax) {
		return SpanKindUnspecified
	}
	return Variant(kind)
}

// SeriesKey is the complete identity of one aggregate series. Every field is a
// dictionary ID or a bounded enum, which makes the struct comparable and
// therefore directly usable as a Go map key with zero collision risk.
//
// DimsID is 0 when no operator-configured dimensions apply. NameID is resolved
// through the dictionary kind named by NameKind(Signal).
type SeriesKey struct {
	TenantID    uint32
	ServiceID   uint32
	NameID      uint32
	DimsID      uint32
	Signal      Signal
	StatusClass StatusClass
	HTTPClass   HTTPClass
	Method      Method
	Variant     Variant
}

// SeriesKeyVersion is the current encoding version. It is the first byte of
// every encoded key; decoders reject anything else rather than guessing a
// layout.
const SeriesKeyVersion uint8 = 1

// EncodedSeriesKeyLen is the exact wire size of an encoded SeriesKey: one
// version byte, four little-endian uint32 fields, five enum bytes.
const EncodedSeriesKeyLen = 1 + 4*4 + 5

// Decoding errors.
var (
	// ErrKeyTruncated reports a buffer shorter than EncodedSeriesKeyLen.
	ErrKeyTruncated = errors.New("aggregate: encoded series key truncated")
	// ErrKeyTrailingBytes reports a buffer longer than EncodedSeriesKeyLen.
	ErrKeyTrailingBytes = errors.New("aggregate: encoded series key has trailing bytes")
)

// VersionError reports an encoded key whose version byte is not one this build
// understands.
type VersionError struct {
	Got  uint8
	Want uint8
}

func (e *VersionError) Error() string {
	return fmt.Sprintf("aggregate: unsupported series key version %d (want %d)", e.Got, e.Want)
}

// FieldError reports a SeriesKey field whose value is outside the range the
// enum (or the signal) permits.
type FieldError struct {
	Field  string
	Value  uint8
	Signal Signal
}

func (e *FieldError) Error() string {
	return fmt.Sprintf("aggregate: series key field %s=%d invalid for signal %s", e.Field, e.Value, e.Signal)
}

// NameKind returns the dictionary kind that NameID is resolved through for the
// given signal. It reports ok=false for an unspecified or unknown signal.
func NameKind(s Signal) (Kind, bool) {
	switch s {
	case SignalTraceOp, SignalServiceEdge:
		return KindOperation, true
	case SignalLog:
		return KindLogTemplate, true
	case SignalMetric:
		return KindMetricName, true
	default:
		return 0, false
	}
}

// Validate reports whether the key is internally consistent: every enum is in
// range and the signal-specific constraints from #159 hold. Metric and log
// series carry no HTTP identity; only traces and edges carry a span kind.
func (k SeriesKey) Validate() error {
	if !k.Signal.Valid() {
		return &FieldError{Field: "Signal", Value: uint8(k.Signal), Signal: k.Signal}
	}
	if k.HTTPClass > httpClassMax {
		return &FieldError{Field: "HTTPClass", Value: uint8(k.HTTPClass), Signal: k.Signal}
	}
	if k.Method > methodMax {
		return &FieldError{Field: "Method", Value: uint8(k.Method), Signal: k.Signal}
	}
	switch k.Signal {
	case SignalTraceOp, SignalServiceEdge:
		if k.StatusClass > statusMax {
			return &FieldError{Field: "StatusClass", Value: uint8(k.StatusClass), Signal: k.Signal}
		}
		if k.Variant > spanKindMax {
			return &FieldError{Field: "Variant", Value: uint8(k.Variant), Signal: k.Signal}
		}
	case SignalLog:
		if k.StatusClass > severityTierMax {
			return &FieldError{Field: "StatusClass", Value: uint8(k.StatusClass), Signal: k.Signal}
		}
		if err := k.requireNoHTTPIdentity(); err != nil {
			return err
		}
		if k.Variant != SpanKindUnspecified {
			return &FieldError{Field: "Variant", Value: uint8(k.Variant), Signal: k.Signal}
		}
	case SignalMetric:
		if k.StatusClass != 0 {
			return &FieldError{Field: "StatusClass", Value: uint8(k.StatusClass), Signal: k.Signal}
		}
		if err := k.requireNoHTTPIdentity(); err != nil {
			return err
		}
		if k.Variant != SpanKindUnspecified {
			return &FieldError{Field: "Variant", Value: uint8(k.Variant), Signal: k.Signal}
		}
	}
	return nil
}

// requireNoHTTPIdentity enforces the "no HTTP identity" rule shared by logs and
// metrics.
func (k SeriesKey) requireNoHTTPIdentity() error {
	if k.HTTPClass != HTTPClassNone {
		return &FieldError{Field: "HTTPClass", Value: uint8(k.HTTPClass), Signal: k.Signal}
	}
	if k.Method != MethodNone {
		return &FieldError{Field: "Method", Value: uint8(k.Method), Signal: k.Signal}
	}
	return nil
}

// AppendBinary appends the field-wise encoding of k to dst and returns the
// extended slice. It never allocates when dst has EncodedSeriesKeyLen bytes of
// spare capacity. It implements encoding.BinaryAppender.
func (k SeriesKey) AppendBinary(dst []byte) ([]byte, error) {
	dst = append(dst, SeriesKeyVersion)
	dst = binary.LittleEndian.AppendUint32(dst, k.TenantID)
	dst = binary.LittleEndian.AppendUint32(dst, k.ServiceID)
	dst = binary.LittleEndian.AppendUint32(dst, k.NameID)
	dst = binary.LittleEndian.AppendUint32(dst, k.DimsID)
	dst = append(dst,
		uint8(k.Signal),
		uint8(k.StatusClass),
		uint8(k.HTTPClass),
		uint8(k.Method),
		uint8(k.Variant),
	)
	return dst, nil
}

// MarshalBinary implements encoding.BinaryMarshaler. It never returns an error;
// validation is the decoder's job so a caller can round-trip a key it built
// itself without paying for the check twice.
func (k SeriesKey) MarshalBinary() ([]byte, error) {
	return k.AppendBinary(make([]byte, 0, EncodedSeriesKeyLen))
}

// UnmarshalBinary implements encoding.BinaryUnmarshaler. b must be exactly
// EncodedSeriesKeyLen bytes.
func (k *SeriesKey) UnmarshalBinary(b []byte) error {
	decoded, err := DecodeSeriesKey(b)
	if err != nil {
		return err
	}
	*k = decoded
	return nil
}

// DecodeSeriesKey decodes exactly one encoded SeriesKey. It rejects short
// buffers (ErrKeyTruncated), long buffers (ErrKeyTrailingBytes), unknown
// versions (*VersionError) and out-of-range enum values (*FieldError).
func DecodeSeriesKey(b []byte) (SeriesKey, error) {
	if len(b) < EncodedSeriesKeyLen {
		return SeriesKey{}, ErrKeyTruncated
	}
	if len(b) > EncodedSeriesKeyLen {
		return SeriesKey{}, ErrKeyTrailingBytes
	}
	if b[0] != SeriesKeyVersion {
		return SeriesKey{}, &VersionError{Got: b[0], Want: SeriesKeyVersion}
	}
	k := SeriesKey{
		TenantID:    binary.LittleEndian.Uint32(b[1:5]),
		ServiceID:   binary.LittleEndian.Uint32(b[5:9]),
		NameID:      binary.LittleEndian.Uint32(b[9:13]),
		DimsID:      binary.LittleEndian.Uint32(b[13:17]),
		Signal:      Signal(b[17]),
		StatusClass: StatusClass(b[18]),
		HTTPClass:   HTTPClass(b[19]),
		Method:      Method(b[20]),
		Variant:     Variant(b[21]),
	}
	if err := k.Validate(); err != nil {
		return SeriesKey{}, err
	}
	return k, nil
}

// String renders a key for logs and test failures. It is diagnostic output,
// not a wire format — never parse it.
func (k SeriesKey) String() string {
	return fmt.Sprintf("SeriesKey{signal=%s tenant=%d service=%d name=%d dims=%d status=%d http=%s method=%s variant=%d}",
		k.Signal, k.TenantID, k.ServiceID, k.NameID, k.DimsID, k.StatusClass, k.HTTPClass, k.Method, k.Variant)
}
