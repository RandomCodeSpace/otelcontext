package graphrag

import "testing"

// The event channel buffer is allocated up front from operator-influenced
// config; New must clamp nonsense values to the default rather than allocate
// gigabytes (config.Validate rejects out-of-range env values, this guards
// programmatic callers and is the allocation-site barrier CodeQL checks).
func TestNew_ClampsEventChannelSize(t *testing.T) {
	for _, bad := range []int{-1, maxChannelSize + 1} {
		cfg := DefaultConfig()
		cfg.ChannelSize = bad
		g := New(nil, nil, nil, cfg)
		if got := cap(g.eventCh); got != defaultChannelSize {
			t.Fatalf("ChannelSize %d: channel cap = %d, want default %d", bad, got, defaultChannelSize)
		}
	}
	cfg := DefaultConfig()
	cfg.ChannelSize = 64
	if got := cap(New(nil, nil, nil, cfg).eventCh); got != 64 {
		t.Fatalf("in-range ChannelSize not honored: cap = %d, want 64", got)
	}
}
