package aggregate

import (
	"errors"
	"fmt"
	"io/fs"
	"syscall"
	"testing"
)

// The classifier decides whether a failed authoritative commit is a disk-full
// failure, which is the one condition that MUST fail an Export (#201 Q5). A
// false negative acknowledges data that was never stored.
func TestIsDiskFull(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("constraint violation"), false},
		{"bare ENOSPC", syscall.ENOSPC, true},
		{"wrapped ENOSPC", fmt.Errorf("commit deltas: %w", syscall.ENOSPC), true},
		{"path error", &fs.PathError{Op: "write", Path: "/data/aggregate.db", Err: syscall.ENOSPC}, true},
		{"wrapped path error", fmt.Errorf("group commit: %w", &fs.PathError{Op: "write", Path: "/data/aggregate.db", Err: syscall.ENOSPC}), true},
		{"quota exceeded", fmt.Errorf("write wal: %w", syscall.EDQUOT), true},
		{"SQLITE_FULL text", errors.New("database or disk is full (13)"), true},
		{"ENOSPC text", errors.New("write /data/aggregate.db-wal: no space left on device"), true},
		{"mixed case", errors.New("SQL logic error: Database Or Disk Is Full"), true},
		// Adjacent failures that are NOT disk-full: misclassifying them would
		// turn an ordinary commit error into a different gRPC code.
		{"disk io error", errors.New("disk I/O error"), false},
		{"readonly", errors.New("attempt to write a readonly database"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsDiskFull(tc.err); got != tc.want {
				t.Fatalf("IsDiskFull(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
