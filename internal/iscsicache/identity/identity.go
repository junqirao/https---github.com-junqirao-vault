// Package identity derives the cache namespace that binds a cache to one
// backing LUN.
//
// Binding matters because a target may keep serving the same IQN after its
// backing disk was replaced. Without a namespace the proxy could hit stale
// blocks from the previous disk and silently corrupt data.
package identity

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strings"
)

// Descriptor is the set of backing LUN properties that must stay stable for a
// cache to remain valid.
type Descriptor struct {
	// TargetIQN is the IQN of the backing target.
	TargetIQN string
	// Serial is VPD page 0x80 of the backing LUN.
	Serial string
	// WWID is the best effort VPD page 0x83 identifier of the backing LUN.
	WWID string
	// BlockCount and BlockSize describe the LUN geometry.
	BlockCount uint64
	BlockSize  uint32
}

// Namespace hashes the descriptor into a stable cache namespace.
func (d Descriptor) Namespace() string {
	h := sha256.New()
	writeField(h, d.TargetIQN)
	writeField(h, d.Serial)
	writeField(h, d.WWID)
	var num [12]byte
	binary.BigEndian.PutUint64(num[0:8], d.BlockCount)
	binary.BigEndian.PutUint32(num[8:12], d.BlockSize)
	h.Write(num[:])
	return hex.EncodeToString(h.Sum(nil))
}

// Full reports whether the descriptor carries enough identity to be trusted.
// A LUN that reports neither a serial nor a WWID cannot be told apart from a
// replacement disk, so callers should warn when this is false.
func (d Descriptor) Full() bool {
	return strings.TrimSpace(d.Serial) != "" || strings.TrimSpace(d.WWID) != ""
}

func writeField(h interface{ Write([]byte) (int, error) }, s string) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(s)))
	h.Write(n[:])
	h.Write([]byte(s))
}
