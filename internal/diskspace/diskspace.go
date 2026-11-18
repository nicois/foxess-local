// Package diskspace reports a filesystem's free space and the level
// below which foxess-local treats it as critical (the disk guard
// discards old raw data; an upgrade refuses to download).
package diskspace

import "errors"

// ErrUnsupported reports that this platform has no free-space query (the
// guard and the upgrade check are then skipped).
var ErrUnsupported = errors.New("diskspace: free space is not available on this platform")

// MiB is one mebibyte (the unit of [retention] min_free_mb and messages).
const MiB = 1 << 20

// criticalFraction caps the critical level at a tenth of the filesystem.
const criticalFraction = 10

// Threshold is the critical free-space level for a filesystem of total
// bytes: minFree, capped at a tenth of the filesystem — so a small flash
// device is not permanently "critical" while a large disk keeps a
// fixed floor.
func Threshold(minFree, total uint64) uint64 {
	return min(minFree, total/criticalFraction)
}
