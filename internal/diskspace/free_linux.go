//go:build linux

package diskspace

import (
	"fmt"
	"syscall"
)

// Free returns the bytes available to an unprivileged writer and the
// total size of the filesystem holding dir.
func Free(dir string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, fmt.Errorf("diskspace: statfs %s: %w", dir, err)
	}
	bs := uint64(st.Bsize) //nolint:gosec // G115: a block size is positive.
	return st.Bavail * bs, st.Blocks * bs, nil
}
