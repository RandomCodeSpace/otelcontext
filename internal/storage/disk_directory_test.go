package storage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiskWatchdogSharedFilesystemBudgetAndFreeSpace(t *testing.T) {
	const gib = int64(1 << 30)
	usage := DiskUsage{TotalBytes: 400 * gib, AvailBytes: 100 * gib}
	dataBytes := gib
	w := NewDiskWatchdog(DiskWatchdogConfig{Path: "/data", BudgetBytes: 8 * gib,
		Stat:      func(string) (DiskUsage, error) { return usage, nil },
		DataUsage: func(string) (int64, error) { return dataBytes, nil },
	})
	if got := w.Sample(); got != SheddingNone {
		t.Fatalf("unrelated 300GiB filesystem use consumed app budget: %s", got)
	}
	dataBytes = 8 * gib
	if got := w.Sample(); got != SheddingRawOff {
		t.Fatalf("application exhausted its budget: %s", got)
	}
	dataBytes = gib
	if got := w.Sample(); got != SheddingNone {
		t.Fatalf("application budget recovered: %s", got)
	}
	usage.AvailBytes = 100 << 20
	if got := w.Sample(); got != SheddingRawOff {
		t.Fatalf("filesystem has insufficient free headroom: %s", got)
	}
	usage.AvailBytes = 2 * gib
	if got := w.Sample(); got != SheddingNone {
		t.Fatalf("filesystem free space recovered: %s", got)
	}
}

func TestDirectoryDataBytesIncludesSidecarsWithoutFollowingLinks(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "dlq"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, size := range map[string]int{"main.db": 50, "main.db-wal": 20, "aggregate.db": 30, "dlq/batch": 10} {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(dir, "main.db"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	got, err := DirectoryDataBytes(dir)
	if err != nil || got != 110 {
		t.Fatalf("directory bytes=%d err=%v, want110", got, err)
	}
	if _, err := DirectoryDataBytes(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing data path reported as empty")
	}
}

func TestDiskWatchdogDataWalkFailureHoldsPressure(t *testing.T) {
	w := NewDiskWatchdog(DiskWatchdogConfig{Path: "/data", BudgetBytes: testBudget,
		Stat:      func(string) (DiskUsage, error) { return DiskUsage{TotalBytes: testBudget, AvailBytes: testBudget}, nil },
		DataUsage: func(string) (int64, error) { return testBudget, nil },
	})
	if w.Sample() != SheddingRawOff {
		t.Fatal("setup failed")
	}
	w.dataUsage = func(string) (int64, error) { return 0, os.ErrPermission }
	if w.Sample() != SheddingRawOff {
		t.Fatal("failed usage measurement cleared pressure")
	}
}

func TestDiskWatchdogDataWalkFailureStillDetectsFullFilesystem(t *testing.T) {
	available := testBudget
	w := NewDiskWatchdog(DiskWatchdogConfig{Path: "/data", BudgetBytes: testBudget,
		Stat:      func(string) (DiskUsage, error) { return DiskUsage{TotalBytes: testBudget, AvailBytes: available}, nil },
		DataUsage: func(string) (int64, error) { return 0, os.ErrPermission },
	})
	if w.Sample() != SheddingNone {
		t.Fatal("initial state")
	}
	available = 0
	if w.Sample() != SheddingRawOff || w.Healthy() {
		t.Fatal("directory error hid physical exhaustion")
	}
	available = testBudget
	if w.Sample() != SheddingRawOff {
		t.Fatal("failed sample cleared pressure")
	}
	w.dataUsage = func(string) (int64, error) { return 0, nil }
	if w.Sample() != SheddingNone {
		t.Fatal("successful sample did not recover")
	}
}

func TestApplicationDataBytesCountsExternalDataOnce(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	if err := os.MkdirAll(filepath.Join(data, "dlq"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, size := range map[string]int{"data/aggregate.db": 30, "data/dlq/batch": 10, "OtelContext.db": 50, "OtelContext.db-wal": 20} {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(dir, "linked")
	if err := os.Symlink(data, link); err != nil {
		t.Fatal(err)
	}
	got, err := ApplicationDataBytes(link, filepath.Join(data, "dlq"), filepath.Join(dir, "OtelContext.db"), filepath.Join(dir, "OtelContext.db-wal"), filepath.Join(dir, "OtelContext.db-shm"), data)
	if err != nil || got != 110 {
		t.Fatalf("bytes=%d err=%v want 110", got, err)
	}
}

func TestDirectoryDataBytesSkipsPVCRecoveryDirectory(t *testing.T) {
	dir := t.TempDir()
	recovery := filepath.Join(dir, "lost+found")
	if err := os.Mkdir(recovery, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(recovery, "recovered"), make([]byte, 100), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.db"), make([]byte, 20), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(recovery, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(recovery, 0700) })
	got, err := DirectoryDataBytes(dir)
	if err != nil || got != 20 {
		t.Fatalf("bytes=%d err=%v", got, err)
	}
}
