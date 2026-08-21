package aggregate

import (
	"bytes"
	"errors"
	"testing"
)

func traceKey() SeriesKey {
	return SeriesKey{
		TenantID:    0x01020304,
		ServiceID:   0x05060708,
		NameID:      0x090a0b0c,
		DimsID:      0x0d0e0f10,
		Signal:      SignalTraceOp,
		StatusClass: StatusError,
		HTTPClass:   HTTPClass5xx,
		Method:      MethodPost,
		Variant:     SpanKindServer,
	}
}

func TestSeriesKeyEncodingLayoutIsExplicit(t *testing.T) {
	got, err := traceKey().MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	want := []byte{
		SeriesKeyVersion,
		0x04, 0x03, 0x02, 0x01, // TenantID, little-endian
		0x08, 0x07, 0x06, 0x05, // ServiceID
		0x0c, 0x0b, 0x0a, 0x09, // NameID
		0x10, 0x0f, 0x0e, 0x0d, // DimsID
		byte(SignalTraceOp),
		byte(StatusError),
		byte(HTTPClass5xx),
		byte(MethodPost),
		byte(SpanKindServer),
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("encoding layout drifted\n got %#v\nwant %#v", got, want)
	}
	if len(got) != EncodedSeriesKeyLen {
		t.Fatalf("len = %d, want EncodedSeriesKeyLen %d", len(got), EncodedSeriesKeyLen)
	}
}

func TestSeriesKeyRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		key  SeriesKey
	}{
		{"trace", traceKey()},
		{"trace zero ids", SeriesKey{Signal: SignalTraceOp}},
		{"edge", SeriesKey{
			TenantID: 1, ServiceID: 2, NameID: 3, DimsID: 0,
			Signal: SignalServiceEdge, StatusClass: StatusOK,
			HTTPClass: HTTPClass2xx, Method: MethodGet, Variant: SpanKindClient,
		}},
		{"log", SeriesKey{
			TenantID: 7, ServiceID: 8, NameID: 9,
			Signal: SignalLog, StatusClass: SeverityTierFatal,
		}},
		{"metric", SeriesKey{
			TenantID: 4, ServiceID: 5, NameID: 6, DimsID: 99,
			Signal: SignalMetric,
		}},
		{"max ids", SeriesKey{
			TenantID: ^uint32(0), ServiceID: ^uint32(0), NameID: ^uint32(0), DimsID: ^uint32(0),
			Signal: SignalTraceOp, StatusClass: StatusUnset,
			HTTPClass: HTTPClassNone, Method: MethodOther, Variant: SpanKindConsumer,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			enc, err := tc.key.MarshalBinary()
			if err != nil {
				t.Fatalf("MarshalBinary: %v", err)
			}
			got, err := DecodeSeriesKey(enc)
			if err != nil {
				t.Fatalf("DecodeSeriesKey: %v", err)
			}
			if got != tc.key {
				t.Fatalf("round-trip mismatch\n got %s\nwant %s", got, tc.key)
			}
			var viaUnmarshal SeriesKey
			if err := viaUnmarshal.UnmarshalBinary(enc); err != nil {
				t.Fatalf("UnmarshalBinary: %v", err)
			}
			if viaUnmarshal != tc.key {
				t.Fatalf("UnmarshalBinary mismatch: got %s", viaUnmarshal)
			}
		})
	}
}

func TestAppendBinaryDoesNotAllocateWithCapacity(t *testing.T) {
	buf := make([]byte, 0, EncodedSeriesKeyLen)
	k := traceKey()
	allocs := testing.AllocsPerRun(100, func() {
		out, err := k.AppendBinary(buf[:0])
		if err != nil || len(out) != EncodedSeriesKeyLen {
			t.Fatalf("AppendBinary: len=%d err=%v", len(out), err)
		}
	})
	if allocs != 0 {
		t.Fatalf("AppendBinary allocated %v times per run, want 0", allocs)
	}
}

func TestDecodeSeriesKeyRejectsUnknownVersion(t *testing.T) {
	enc, err := traceKey().MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	for _, bad := range []uint8{0, SeriesKeyVersion + 1, 0xff} {
		enc[0] = bad
		_, err := DecodeSeriesKey(enc)
		var verr *VersionError
		if !errors.As(err, &verr) {
			t.Fatalf("version %d: err = %v, want *VersionError", bad, err)
		}
		if verr.Got != bad || verr.Want != SeriesKeyVersion {
			t.Fatalf("version %d: got %+v", bad, *verr)
		}
	}
}

func TestDecodeSeriesKeyLengthErrors(t *testing.T) {
	enc, err := traceKey().MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	if _, err := DecodeSeriesKey(enc[:len(enc)-1]); !errors.Is(err, ErrKeyTruncated) {
		t.Fatalf("short buffer: err = %v, want ErrKeyTruncated", err)
	}
	if _, err := DecodeSeriesKey(nil); !errors.Is(err, ErrKeyTruncated) {
		t.Fatalf("nil buffer: err = %v, want ErrKeyTruncated", err)
	}
	if _, err := DecodeSeriesKey(append(enc, 0)); !errors.Is(err, ErrKeyTrailingBytes) {
		t.Fatalf("long buffer: err = %v, want ErrKeyTrailingBytes", err)
	}
}

func TestSeriesKeyValidate(t *testing.T) {
	cases := []struct {
		name      string
		key       SeriesKey
		wantField string // "" = valid
	}{
		{"unspecified signal", SeriesKey{}, "Signal"},
		{"unknown signal", SeriesKey{Signal: 9}, "Signal"},
		{"http class out of range", SeriesKey{Signal: SignalTraceOp, HTTPClass: 6}, "HTTPClass"},
		{"method out of range", SeriesKey{Signal: SignalTraceOp, Method: 11}, "Method"},
		{"trace status out of range", SeriesKey{Signal: SignalTraceOp, StatusClass: 3}, "StatusClass"},
		{"span kind out of range", SeriesKey{Signal: SignalTraceOp, Variant: 6}, "Variant"},
		{"log severity out of range", SeriesKey{Signal: SignalLog, StatusClass: 7}, "StatusClass"},
		{"log carries http class", SeriesKey{Signal: SignalLog, HTTPClass: HTTPClass2xx}, "HTTPClass"},
		{"log carries method", SeriesKey{Signal: SignalLog, Method: MethodGet}, "Method"},
		{"log carries variant", SeriesKey{Signal: SignalLog, Variant: SpanKindServer}, "Variant"},
		{"metric carries status", SeriesKey{Signal: SignalMetric, StatusClass: StatusError}, "StatusClass"},
		{"metric carries method", SeriesKey{Signal: SignalMetric, Method: MethodGet}, "Method"},
		{"metric carries variant", SeriesKey{Signal: SignalMetric, Variant: SpanKindClient}, "Variant"},
		{"valid trace", traceKey(), ""},
		{"valid log", SeriesKey{Signal: SignalLog, StatusClass: SeverityTierWarn}, ""},
		{"valid metric", SeriesKey{Signal: SignalMetric}, ""},
		{"valid edge", SeriesKey{Signal: SignalServiceEdge, StatusClass: StatusOK, Variant: SpanKindClient}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.key.Validate()
			if tc.wantField == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			var ferr *FieldError
			if !errors.As(err, &ferr) {
				t.Fatalf("Validate() = %v, want *FieldError", err)
			}
			if ferr.Field != tc.wantField {
				t.Fatalf("Validate() field = %q, want %q", ferr.Field, tc.wantField)
			}
		})
	}
}

func TestDecodeSeriesKeyRejectsInvalidFields(t *testing.T) {
	// A metric key carrying a span kind must not survive a decode: the signal
	// contract says only traces and edges have one.
	enc, err := SeriesKey{Signal: SignalMetric}.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	enc[21] = byte(SpanKindServer)
	_, err = DecodeSeriesKey(enc)
	var ferr *FieldError
	if !errors.As(err, &ferr) {
		t.Fatalf("DecodeSeriesKey = %v, want *FieldError", err)
	}
	if ferr.Field != "Variant" || ferr.Signal != SignalMetric {
		t.Fatalf("unexpected field error %+v", *ferr)
	}
}

func TestSeriesKeyMapSemantics(t *testing.T) {
	base := traceKey()
	m := map[SeriesKey]int{}
	m[base]++
	m[traceKey()]++ // Independently built, structurally identical.
	if len(m) != 1 || m[base] != 2 {
		t.Fatalf("identical keys did not collapse: len=%d count=%d", len(m), m[base])
	}

	variants := map[string]SeriesKey{}
	for name, mutate := range map[string]func(k *SeriesKey){
		"TenantID":    func(k *SeriesKey) { k.TenantID++ },
		"ServiceID":   func(k *SeriesKey) { k.ServiceID++ },
		"NameID":      func(k *SeriesKey) { k.NameID++ },
		"DimsID":      func(k *SeriesKey) { k.DimsID++ },
		"Signal":      func(k *SeriesKey) { k.Signal = SignalServiceEdge },
		"StatusClass": func(k *SeriesKey) { k.StatusClass = StatusOK },
		"HTTPClass":   func(k *SeriesKey) { k.HTTPClass = HTTPClass4xx },
		"Method":      func(k *SeriesKey) { k.Method = MethodGet },
		"Variant":     func(k *SeriesKey) { k.Variant = SpanKindClient },
	} {
		k := base
		mutate(&k)
		if k == base {
			t.Fatalf("%s mutation produced an identical key", name)
		}
		variants[name] = k
		m[k]++
	}
	if want := 1 + len(variants); len(m) != want {
		t.Fatalf("map has %d keys, want %d — a field is not part of identity", len(m), want)
	}

	// Every single-field variant must also encode differently.
	baseEnc, err := base.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	for name, k := range variants {
		enc, err := k.MarshalBinary()
		if err != nil {
			t.Fatalf("%s: MarshalBinary: %v", name, err)
		}
		if bytes.Equal(enc, baseEnc) {
			t.Fatalf("%s: encoding is identical to the base key", name)
		}
	}
}

func TestParseMethod(t *testing.T) {
	cases := []struct {
		in   string
		want Method
	}{
		{"", MethodNone},
		{"GET", MethodGet},
		{"POST", MethodPost},
		{"PUT", MethodPut},
		{"DELETE", MethodDelete},
		{"PATCH", MethodPatch},
		{"HEAD", MethodHead},
		{"OPTIONS", MethodOptions},
		{"TRACE", MethodTrace},
		{"CONNECT", MethodConnect},
		{"get", MethodGet},
		{"PoSt", MethodPost},
		{"PROPFIND", MethodOther},
		{"FROBNICATE", MethodOther},
		{"GET ", MethodOther},
		{"\x00", MethodOther},
	}
	for _, tc := range cases {
		if got := ParseMethod(tc.in); got != tc.want {
			t.Errorf("ParseMethod(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestLookupMethod(t *testing.T) {
	if m, ok := LookupMethod("DELETE"); !ok || m != MethodDelete {
		t.Fatalf("LookupMethod(DELETE) = %v %v", m, ok)
	}
	if _, ok := LookupMethod(""); ok {
		t.Fatal("LookupMethod(\"\") reported ok")
	}
	if _, ok := LookupMethod("PROPFIND"); ok {
		t.Fatal("LookupMethod(PROPFIND) reported ok")
	}
	if got := MethodOther.String(); got != "_OTHER" {
		t.Fatalf("MethodOther.String() = %q", got)
	}
	if got := MethodNone.String(); got != "" {
		t.Fatalf("MethodNone.String() = %q", got)
	}
}

func TestSeverityTierFromNumber(t *testing.T) {
	cases := []struct {
		in   int32
		want StatusClass
	}{
		{0, SeverityTierUnspecified},
		{-1, SeverityTierUnspecified},
		{25, SeverityTierUnspecified},
		{1, SeverityTierTrace}, {4, SeverityTierTrace},
		{5, SeverityTierDebug}, {8, SeverityTierDebug},
		{9, SeverityTierInfo}, {12, SeverityTierInfo},
		{13, SeverityTierWarn}, {16, SeverityTierWarn},
		{17, SeverityTierError}, {20, SeverityTierError},
		{21, SeverityTierFatal}, {24, SeverityTierFatal},
	}
	for _, tc := range cases {
		if got := SeverityTierFromNumber(tc.in); got != tc.want {
			t.Errorf("SeverityTierFromNumber(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestHTTPClassFromStatus(t *testing.T) {
	cases := []struct {
		in   int
		want HTTPClass
	}{
		{0, HTTPClassNone}, {99, HTTPClassNone}, {600, HTTPClassNone}, {-1, HTTPClassNone},
		{100, HTTPClass1xx}, {200, HTTPClass2xx}, {204, HTTPClass2xx},
		{301, HTTPClass3xx}, {404, HTTPClass4xx}, {503, HTTPClass5xx}, {599, HTTPClass5xx},
	}
	for _, tc := range cases {
		if got := HTTPClassFromStatus(tc.in); got != tc.want {
			t.Errorf("HTTPClassFromStatus(%d) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestTraceStatusAndVariantMapping(t *testing.T) {
	if got := TraceStatusFromCode(0); got != StatusUnset {
		t.Errorf("TraceStatusFromCode(0) = %d", got)
	}
	if got := TraceStatusFromCode(1); got != StatusOK {
		t.Errorf("TraceStatusFromCode(1) = %d", got)
	}
	if got := TraceStatusFromCode(2); got != StatusError {
		t.Errorf("TraceStatusFromCode(2) = %d", got)
	}
	if got := TraceStatusFromCode(99); got != StatusUnset {
		t.Errorf("TraceStatusFromCode(99) = %d", got)
	}
	for kind := int32(0); kind <= 5; kind++ {
		if got := VariantFromSpanKind(kind); got != Variant(kind) {
			t.Errorf("VariantFromSpanKind(%d) = %d", kind, got)
		}
	}
	if got := VariantFromSpanKind(6); got != SpanKindUnspecified {
		t.Errorf("VariantFromSpanKind(6) = %d", got)
	}
	if got := VariantFromSpanKind(-1); got != SpanKindUnspecified {
		t.Errorf("VariantFromSpanKind(-1) = %d", got)
	}
}

func TestNameKindNamespaces(t *testing.T) {
	cases := []struct {
		signal Signal
		want   Kind
		ok     bool
	}{
		{SignalTraceOp, KindOperation, true},
		{SignalServiceEdge, KindOperation, true},
		{SignalLog, KindLogTemplate, true},
		{SignalMetric, KindMetricName, true},
		{SignalUnspecified, 0, false},
		{Signal(200), 0, false},
	}
	for _, tc := range cases {
		got, ok := NameKind(tc.signal)
		if got != tc.want || ok != tc.ok {
			t.Errorf("NameKind(%v) = %v %v, want %v %v", tc.signal, got, ok, tc.want, tc.ok)
		}
	}
}

func TestSignalString(t *testing.T) {
	cases := map[Signal]string{
		SignalTraceOp:     "trace_op",
		SignalServiceEdge: "service_edge",
		SignalLog:         "log",
		SignalMetric:      "metric",
		SignalUnspecified: "unspecified",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("Signal(%d).String() = %q, want %q", uint8(s), got, want)
		}
	}
	if got := Signal(99).String(); got != "signal(99)" {
		t.Errorf("Signal(99).String() = %q", got)
	}
	if got := HTTPClass4xx.String(); got != "4xx" {
		t.Errorf("HTTPClass4xx.String() = %q", got)
	}
}
