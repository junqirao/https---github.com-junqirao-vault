package iscsiproxy

import "vault/internal/iscsicache/cache"

// Version labels the v2 engine. It is logged when a cache target comes up so an
// operator can tell at a glance which cache implementation the client runs.
const Version = "v2"

// StatsSnapshot holds the forwarding counters of one cache target.
type StatsSnapshot struct {
	// Reads counts read commands observed on the wire (READ(10)/READ(16)),
	// whether or not the cache could answer them. It is the IOPS numerator: the
	// cache's own Reads only advances once a LUN is bound and a read is looked
	// up, so before binding (or after a degraded session) it would read 0 even
	// though the proxy is forwarding traffic.
	Reads int64
	// Writes counts write commands observed on the wire (WRITE/WRITE SAME/UNMAP).
	Writes int64
	// Invalidations counts the cache ranges a write dropped.
	Invalidations int64
	// LocalCmds counts read commands answered from the cache without touching the
	// backend.
	LocalCmds int64
	// Passthrough counts commands forwarded to the backend unchanged.
	Passthrough int64
	// Degraded counts sessions that fell back to a pure byte splice.
	Degraded int64
}

// Stats aggregates one target's cache and forwarding counters. Its shape mirrors
// the v1 aggregate so the agent's status endpoint is untouched.
type Stats struct {
	Cache     cache.Snapshot
	Proxy     StatsSnapshot
	L2Slots   int
	L2Used    int
	BlockSize int
	// L2Restored/L2Unverified say how the L2 file was reopened: how many slots
	// the snapshot held and how many of them still have to be confirmed against
	// their slot stamp. Both are zero after a clean stop, and Unverified falls
	// as the entries are read.
	L2Restored   int
	L2Unverified int
	// L2Dropped counts restored entries the stamps disproved: reads that would
	// have been answered from a slot another write had taken over. A value that
	// keeps growing means the index is not flushed often enough.
	L2Dropped int
}
