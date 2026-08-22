//go:build !linux && !darwin

package storage

import "errors"

// errStatfsUnsupported is returned on platforms without statfs. The watchdog
// treats an unreadable sample as "no new information" and holds its current
// state rather than shedding on a platform detail.
var errStatfsUnsupported = errors.New("statfs is not available on this platform")

func statfsUsage(string) (DiskUsage, error) { return DiskUsage{}, errStatfsUnsupported }
