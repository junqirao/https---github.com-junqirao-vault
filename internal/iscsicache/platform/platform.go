// Package platform abstracts the operating system services the iSCSI cache
// proxy needs: pinning cache memory and (on Windows) reporting the physical
// sector alignment used by the L2 store.
//
// Only the Windows implementation is real; other platforms return
// ErrUnsupported so the proxy can fall back to its non-pinned configuration.
package platform

import "errors"

// ErrUnsupported is returned when a platform cannot provide a service.
var ErrUnsupported = errors.New("platform: operation not supported on this platform")

// MemLocker pins byte slices in physical memory.
type MemLocker interface {
	// Name identifies the implementation, for logs.
	Name() string
	// Supported reports whether Lock/Unlock can succeed at all.
	Supported() bool
	// Lock pins b. It may fail when the process lacks the required privilege;
	// callers are expected to degrade gracefully.
	Lock(b []byte) error
	// Unlock releases a previous Lock.
	Unlock(b []byte) error
}

// DefaultMemLocker returns the memory locker for the running platform.
func DefaultMemLocker() MemLocker { return defaultMemLocker() }
