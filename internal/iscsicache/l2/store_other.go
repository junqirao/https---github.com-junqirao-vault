//go:build !windows

package l2

// openStore is the placeholder for platforms whose unbuffered L2 store is not
// implemented yet. The intended Linux implementation opens the file with
// O_DIRECT and shares the same bounce-buffer shape as the Windows store.
func openStore(path string, size int64, maxIO int) (Store, error) {
	return nil, ErrUnsupported
}
