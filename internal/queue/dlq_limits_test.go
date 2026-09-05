package queue

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testMiB = 1024 * 1024

func TestDLQDiskLimitRejectsOversizedEnvelopeWithoutEviction(t *testing.T) {
	for _, tc := range []struct {
		name     string
		seedFile bool
	}{
		{name: "empty queue"},
		{name: "nonempty queue", seedFile: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			oldPath := filepath.Join(dir, "batch_1_old.json")
			oldData := []byte(`{"old":"recoverable"}`)
			if tc.seedFile {
				if err := os.WriteFile(oldPath, oldData, 0o600); err != nil {
					t.Fatal(err)
				}
			}

			q, err := NewDLQWithLimits(dir, time.Hour, func([]byte) error { return nil }, 0, 1, 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(q.Stop)
			enqueued := 0
			q.SetMetrics(func() { enqueued++ }, nil, nil, nil)

			// JSON string encoding adds two quote bytes.
			oversized := strings.Repeat("x", testMiB-1)
			if err := q.Enqueue(oversized); err == nil {
				t.Fatal("Enqueue oversized envelope succeeded")
			}
			if got := q.EvictedCount(); got != 0 {
				t.Fatalf("EvictedCount=%d, want 0", got)
			}
			if got := q.EvictedBytesCount(); got != 0 {
				t.Fatalf("EvictedBytesCount=%d, want 0", got)
			}
			if enqueued != 0 {
				t.Fatalf("enqueue callback count=%d, want 0", enqueued)
			}

			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			wantFiles := 0
			if tc.seedFile {
				wantFiles = 1
			}
			if len(entries) != wantFiles {
				t.Fatalf("files=%d, want %d", len(entries), wantFiles)
			}
			if tc.seedFile {
				got, err := os.ReadFile(oldPath)
				if err != nil {
					t.Fatalf("read old envelope: %v", err)
				}
				if !bytes.Equal(got, oldData) {
					t.Fatalf("old envelope changed: got %q, want %q", got, oldData)
				}
			}
		})
	}
}

func TestDLQDiskLimitAcceptsExactBoundary(t *testing.T) {
	dir := t.TempDir()
	q, err := NewDLQWithLimits(dir, time.Hour, func([]byte) error { return nil }, 0, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.Stop)

	// JSON string encoding adds two quote bytes.
	if err := q.Enqueue(strings.Repeat("x", testMiB-2)); err != nil {
		t.Fatalf("Enqueue exact-boundary envelope: %v", err)
	}
	if got := q.DiskBytes(); got != testMiB {
		t.Fatalf("DiskBytes=%d, want %d", got, testMiB)
	}
}

func TestDLQDiskLimitEvictsOldestFile(t *testing.T) {
	dir := t.TempDir()
	oldestPath := filepath.Join(dir, "batch_1_oldest.json")
	newerPath := filepath.Join(dir, "batch_2_newer.json")
	if err := os.WriteFile(oldestPath, bytes.Repeat([]byte("a"), 700*1024), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newerPath, bytes.Repeat([]byte("b"), 300*1024), 0o600); err != nil {
		t.Fatal(err)
	}

	q, err := NewDLQWithLimits(dir, time.Hour, func([]byte) error { return nil }, 0, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.Stop)

	// The incoming JSON string is exactly 100 KiB. Removing the 700 KiB
	// oldest file leaves the newer 300 KiB file and the incoming envelope.
	if err := q.Enqueue(strings.Repeat("x", 100*1024-2)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := os.Stat(oldestPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oldest file still exists or stat failed: %v", err)
	}
	if _, err := os.Stat(newerPath); err != nil {
		t.Fatalf("newer file was not preserved: %v", err)
	}
	if got := q.EvictedCount(); got != 1 {
		t.Fatalf("EvictedCount=%d, want 1", got)
	}
	if got := q.EvictedBytesCount(); got != 700*1024 {
		t.Fatalf("EvictedBytesCount=%d, want %d", got, 700*1024)
	}
	if got := q.DiskBytes(); got != 400*1024 {
		t.Fatalf("DiskBytes=%d, want %d", got, 400*1024)
	}
}

func TestDLQDiskLimitRemovalFailureRefusesIncoming(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "batch_1_old.json")
	oldData := bytes.Repeat([]byte("a"), 700*1024)
	if err := os.WriteFile(oldPath, oldData, 0o600); err != nil {
		t.Fatal(err)
	}

	q, err := NewDLQWithLimits(dir, time.Hour, func([]byte) error { return nil }, 0, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.Stop)
	removeErr := errors.New("injected remove failure")
	q.removeFile = func(string) error { return removeErr }

	if err := q.Enqueue(strings.Repeat("x", 400*1024-2)); !errors.Is(err, removeErr) {
		t.Fatalf("Enqueue error=%v, want removal failure", err)
	}
	if got := q.EvictedCount(); got != 0 {
		t.Fatalf("EvictedCount=%d, want 0", got)
	}
	if got := q.EvictedBytesCount(); got != 0 {
		t.Fatalf("EvictedBytesCount=%d, want 0", got)
	}
	got, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatalf("read old envelope: %v", err)
	}
	if !bytes.Equal(got, oldData) {
		t.Fatal("old envelope changed after removal failure")
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
		t.Fatalf("files after removal failure=%d, err=%v; want one old file", len(entries), err)
	}
}

func TestDLQDiskLimitDirectoryReadFailureRefusesIncoming(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "dlq")
	q, err := NewDLQWithLimits(dir, time.Hour, func([]byte) error { return nil }, 0, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.Stop)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}

	if err := q.Enqueue("fits"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Enqueue error=%v, want missing-directory error", err)
	}
	if got := q.EvictedCount(); got != 0 {
		t.Fatalf("EvictedCount=%d, want 0", got)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("DLQ directory was recreated or stat failed: %v", err)
	}
}

func TestDLQDiskLimitZeroIsUnlimited(t *testing.T) {
	dir := t.TempDir()
	q, err := NewDLQWithLimits(dir, time.Hour, func([]byte) error { return nil }, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.Stop)

	if err := q.Enqueue(strings.Repeat("x", testMiB)); err != nil {
		t.Fatalf("Enqueue with unlimited disk: %v", err)
	}
	if got := q.DiskBytes(); got != testMiB+2 {
		t.Fatalf("DiskBytes=%d, want %d", got, testMiB+2)
	}
}
