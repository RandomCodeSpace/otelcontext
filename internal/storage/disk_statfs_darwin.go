//go:build darwin

package storage

import "syscall"

// statfsUsage reads filesystem capacity for the volume holding path. See the
// linux build for why Bavail rather than Bfree.
func statfsUsage(path string) (DiskUsage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return DiskUsage{}, err
	}
	bsize := int64(st.Bsize)
	return DiskUsage{
		TotalBytes: int64(st.Blocks) * bsize, // #nosec G115 -- block counts on a real volume are far below 2^63/bsize
		AvailBytes: int64(st.Bavail) * bsize, // #nosec G115 -- same
	}, nil
}
