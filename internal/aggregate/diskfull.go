package aggregate

import (
	"errors"
	"io/fs"
	"strings"
	"syscall"
)

// Disk-exhaustion classification for the authoritative commit path (#201 Q5).
//
// Raw exemplar shedding is explicitly forbidden from turning a successful
// aggregate Export into a retryable failure — the aggregate numbers are the
// dataset, the exemplars are diagnostics, and refusing telemetry because a
// diagnostic could not be stored is the wrong trade in every direction.
//
// There is exactly one exception, and it is not really about shedding: if the
// AUTHORITATIVE aggregate commit itself fails because the device is out of
// space, the Export must fail. Under the durable-ACK contract (#160) a success
// response asserts the deltas are in a committed transaction. Answering OK for
// data that hit ENOSPC is data loss with better branding, and the client's
// retry is the only thing that can still save it.
//
// Detection is by error inspection rather than by asking the filesystem,
// because the only reading that matters is the one the write actually got.

// sqliteFullMessages are the message fragments SQLITE_FULL (13) and its ENOSPC
// cousins surface through the pure-Go driver, which does not export a typed
// error the way mattn/go-sqlite3 does. Matched case-insensitively.
var sqliteFullMessages = []string{
	"database or disk is full", // SQLITE_FULL
	"no space left on device",  // ENOSPC surfaced as text
	"disk is full",
}

// IsDiskFull reports whether err is a device-out-of-space failure.
//
// It checks the typed errno first (errors.Is unwraps *fs.PathError and any
// wrapping the storage layer added) and falls back to the driver's message
// text, which is the only channel the pure-Go SQLite driver offers for
// SQLITE_FULL.
func IsDiskFull(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		return true
	}
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) && errors.Is(pathErr.Err, syscall.ENOSPC) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, frag := range sqliteFullMessages {
		if strings.Contains(msg, frag) {
			return true
		}
	}
	return false
}
