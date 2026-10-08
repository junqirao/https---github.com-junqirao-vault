//go:build !windows

package platform

// stubMemLocker is the placeholder for platforms whose memory pinning has not
// been implemented yet. The intended Linux implementation wraps mlock/munlock,
// which share the same best effort semantics as VirtualLock.
type stubMemLocker struct{}

func defaultMemLocker() MemLocker { return stubMemLocker{} }

func (stubMemLocker) Name() string { return "stub" }

func (stubMemLocker) Supported() bool { return false }

func (stubMemLocker) Lock(b []byte) error { return ErrUnsupported }

func (stubMemLocker) Unlock(b []byte) error { return ErrUnsupported }
