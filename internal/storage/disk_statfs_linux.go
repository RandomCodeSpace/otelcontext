//go:build linux

package storage

import "syscall"

// statfsUsage reads filesystem capacity for the volume holding path.
//
// Avail (not Free) is the enforcement number: the reserved-for-root blocks in
// Free are not usable by this process, and counting them as headroom is how a
// watchdog reports 8% free while writes are already returning ENOSPC.
func statfsUsage(path string) (DiskUsage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return DiskUsage{}, err
	}
	// Bsize is already int64 on linux; darwin's is uint32 and converts there.
	bsize := st.Bsize
	return DiskUsage{
		TotalBytes: int64(st.Blocks) * bsize, // #nosec G115 -- block counts on a real volume are far below 2^63/bsize
		AvailBytes: int64(st.Bavail) * bsize, // #nosec G115 -- same
	}, nil
}
