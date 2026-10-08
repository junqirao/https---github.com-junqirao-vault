// Package backend implements a pure-Go iSCSI initiator used as the proxy's
// backing store. It performs login, text negotiation, LUN discovery and SCSI
// command execution against a remote iSCSI target.
//
// The package is platform independent; there is no CGO and no dependency on
// the platform iSCSI initiator shipped with Windows.
package backend

import (
	"context"
	"errors"
	"fmt"
)

// ErrClosed is returned when the backend session is no longer usable.
var ErrClosed = errors.New("backend: session closed")

// DeviceInfo describes the backing LUN discovered during login.
type DeviceInfo struct {
	Vendor     string // INQUIRY vendor identification
	Product    string // INQUIRY product identification
	Revision   string // INQUIRY product revision level
	Serial     string // VPD page 0x80 unit serial number
	WWID       string // best effort VPD page 0x83 identifier (hex)
	DeviceType uint8  // INQUIRY peripheral device type
	BlockSize  uint32 // logical block size in bytes
	BlockCount uint64 // number of logical blocks

	// TargetIQN is the IQN of the backing target as configured by the caller.
	TargetIQN string
	// TargetMaxRecvDSL is the MaxRecvDataSegmentLength announced by the target;
	// it caps the size of Data-Out PDUs this initiator may send.
	TargetMaxRecvDSL int
}

// CapacityBytes returns the LUN capacity in bytes.
func (d DeviceInfo) CapacityBytes() uint64 { return d.BlockCount * uint64(d.BlockSize) }

// Result is the outcome of a single SCSI command.
type Result struct {
	Status   uint8
	Sense    []byte
	Data     []byte
	Residual uint32

	// Overflow/Underflow mirror the residual flags returned by the target.
	Overflow  bool
	Underflow bool
}

// OK reports whether the command completed with GOOD status.
func (r *Result) OK() bool { return r.Status == 0x00 }

// Error renders a non-GOOD result as an error.
func (r *Result) Error() error {
	if r.OK() {
		return nil
	}
	return fmt.Errorf("backend: SCSI status 0x%02x (%s)", r.Status, senselessString(r))
}

func senselessString(r *Result) string {
	if len(r.Sense) >= 14 {
		return fmt.Sprintf("sense key=0x%02x asc=0x%02x ascq=0x%02x", r.Sense[2]&0x0f, r.Sense[12], r.Sense[13])
	}
	return "no sense data"
}

// Backend is the storage side of the proxy. Implementations must be safe for
// concurrent use; commands may be serialised internally.
type Backend interface {
	// Info returns the discovered LUN parameters.
	Info() DeviceInfo
	// Exec issues a single SCSI command.
	//
	// dataOut (if non-empty) is transferred to the target, inLen is the number
	// of bytes expected back. Direction flags are derived from these values.
	Exec(ctx context.Context, cdb []byte, dataOut []byte, inLen int) (*Result, error)
	// Close terminates the session.
	Close() error
}
