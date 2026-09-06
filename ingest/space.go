//go:build darwin || linux

package ingest

import "syscall"

// freeSpace returns the bytes available to this user on the filesystem
// holding path.
func statfsFree(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(uint64(st.Bavail) * uint64(st.Bsize)), nil
}
