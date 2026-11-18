//go:build !linux

package diskspace

// Free is unsupported off Linux (foxess-local runs on Linux; this keeps
// development builds elsewhere compiling).
func Free(string) (free, total uint64, err error) {
	return 0, 0, ErrUnsupported
}
