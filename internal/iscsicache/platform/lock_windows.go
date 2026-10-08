//go:build windows

package platform

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

type winMemLocker struct{}

func defaultMemLocker() MemLocker { return winMemLocker{} }

func (winMemLocker) Name() string { return "windows-virtual-lock" }

// Supported reports true: VirtualLock exists, but it still needs the
// SeLockMemoryPrivilege to succeed for a non trivial amount of memory, which is
// why callers treat a failure as a graceful degradation rather than an error.
func (winMemLocker) Supported() bool { return true }

func (winMemLocker) Lock(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	return windows.VirtualLock(uintptr(unsafe.Pointer(unsafe.SliceData(b))), uintptr(len(b)))
}

func (winMemLocker) Unlock(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	return windows.VirtualUnlock(uintptr(unsafe.Pointer(unsafe.SliceData(b))), uintptr(len(b)))
}
