package membudget

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadCgroupBytes(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		content string
		want    int64
		wantOK  bool
	}{
		{"concrete limit", "2147483648\n", 2147483648, true},
		{"unlimited max", "max\n", 0, false},
		{"empty", "", 0, false},
		{"non-numeric", "garbage", 0, false},
		{"zero", "0", 0, false},
		{"v1 near-max sentinel", "9223372036854771712", 0, false}, // ~8 EiB "unlimited"
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(dir, tc.name)
			if err := os.WriteFile(p, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			got, ok := readCgroupBytes(p)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("readCgroupBytes(%q)=(%d,%v) want (%d,%v)", tc.content, got, ok, tc.want, tc.wantOK)
			}
		})
	}
	if _, ok := readCgroupBytes(filepath.Join(dir, "does-not-exist")); ok {
		t.Fatal("missing file should return ok=false")
	}
}

func TestReadMemTotal(t *testing.T) {
	dir := t.TempDir()
	meminfo := "MemFree:         1000 kB\nMemTotal:       16384000 kB\nBuffers:          500 kB\n"
	p := filepath.Join(dir, "meminfo")
	if err := os.WriteFile(p, []byte(meminfo), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok := readMemTotal(p)
	if !ok || got != 16384000*1024 {
		t.Fatalf("readMemTotal=(%d,%v) want (%d,true)", got, ok, int64(16384000)*1024)
	}
	if _, ok := readMemTotal(filepath.Join(dir, "nope")); ok {
		t.Fatal("missing meminfo should return ok=false")
	}
}

func TestDetect(t *testing.T) {
	// On the Linux test host at least one source (meminfo) must resolve to a
	// positive budget; the function must never return a negative value.
	b, src := Detect()
	if b < 0 {
		t.Fatalf("budget must not be negative, got %d", b)
	}
	if b > 0 && src == "" {
		t.Fatal("positive budget must carry a source label")
	}
}
