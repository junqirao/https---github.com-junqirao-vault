// Package proxy assembles the read-cache proxy: it implements the frontend
// Handler (the SCSI command executor the local iSCSI target calls) on top of
// the L1/L2 cache and the backing iSCSI initiator.
//
// Command routing:
//
//	local      TEST UNIT READY, INQUIRY, REPORT LUNS, READ CAPACITY,
//	           SYNCHRONIZE CACHE, REQUEST SENSE, START STOP UNIT
//	cached     READ(10/16): served from L1/L2, misses filled from the backend
//	writearound WRITE(10/16), UNMAP: forwarded to the backend, then the
//	           affected cache ranges are invalidated before GOOD is returned
//	pass       MODE SENSE/SELECT and anything else the cache cannot answer
//
// Everything the proxy cannot handle locally is rejected with ILLEGAL REQUEST
// rather than being forwarded blindly, so the target never claims support for a
// command it does not implement.
package proxy

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"

	"vault/internal/iscsicache/backend"
	"vault/internal/iscsicache/cache"
	"vault/internal/iscsicache/frontend"
	"vault/internal/iscsicache/iscsi"
)

// Config configures a Proxy.
type Config struct {
	// Backend is the backing iSCSI target. Required.
	Backend backend.Backend
	// Cache is the read cache. When nil every read goes straight to the
	// backend.
	Cache *cache.Cache

	// Vendor/Product/Revision override the strings reported to local
	// initiators; empty fields fall back to the backend INQUIRY data.
	Vendor   string
	Product  string
	Revision string

	// TargetIQN is reported in the VPD page 0x83 SCSI name string descriptor.
	TargetIQN string

	Logger *slog.Logger
}

// Stats counts what the executor did.
type Stats struct {
	Reads         atomic.Int64
	Writes        atomic.Int64
	Invalidations atomic.Int64
	LocalCmds     atomic.Int64
	Passthrough   atomic.Int64
	Unsupported   atomic.Int64
}

// StatsSnapshot is a plain copy of Stats.
type StatsSnapshot struct {
	Reads, Writes, Invalidations, LocalCmds, Passthrough, Unsupported int64
}

// Proxy is the frontend handler backed by a cache over an iSCSI initiator.
type Proxy struct {
	be  backend.Backend
	c   *cache.Cache
	log *slog.Logger

	dev frontend.DeviceInfo
	iqn string

	// execMu guards cache coherence across sessions. Reads only share the cache,
	// which has its own per-shard locking and single-flight fill dedup, so they
	// take the shared lock and run concurrently: an initiator keeps many commands
	// outstanding and collapsing them to one wastes its queue depth. Everything
	// else takes the exclusive lock, because a write's backend update plus
	// invalidation must not interleave with a read fill of the same range.
	execMu sync.RWMutex

	stats Stats
}

var _ frontend.Handler = (*Proxy)(nil)

// New validates the geometry and builds a Proxy.
func New(cfg Config) (*Proxy, error) {
	if cfg.Backend == nil {
		return nil, fmt.Errorf("proxy: backend is required")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	info := cfg.Backend.Info()
	if info.BlockSize == 0 {
		return nil, fmt.Errorf("proxy: backend reported a zero block size")
	}
	if cfg.Cache != nil {
		bs := int(info.BlockSize)
		sec := cfg.Cache.SectorSize()
		blk := cfg.Cache.BlockSize()
		if sec%bs != 0 {
			return nil, fmt.Errorf("proxy: cache sector size %d is not a multiple of the backend block size %d", sec, bs)
		}
		if blk%bs != 0 {
			return nil, fmt.Errorf("proxy: cache block size %d is not a multiple of the backend block size %d", blk, bs)
		}
	}

	dev := frontend.DeviceInfo{
		Vendor:     firstNonEmpty(cfg.Vendor, info.Vendor),
		Product:    firstNonEmpty(cfg.Product, info.Product),
		Revision:   firstNonEmpty(cfg.Revision, info.Revision),
		Serial:     info.Serial,
		DeviceType: info.DeviceType,
		BlockSize:  info.BlockSize,
		BlockCount: info.BlockCount,
	}
	if dev.DeviceType == 0 {
		dev.DeviceType = 0x00 // direct access block device
	}
	if dev.Vendor == "" {
		dev.Vendor = "VAULT"
	}
	if dev.Product == "" {
		dev.Product = "iSCSI Read Cache"
	}
	if dev.Revision == "" {
		dev.Revision = "0001"
	}

	return &Proxy{be: cfg.Backend, c: cfg.Cache, log: log, dev: dev, iqn: cfg.TargetIQN}, nil
}

// Device implements frontend.Handler.
func (p *Proxy) Device() frontend.DeviceInfo { return p.dev }

// Stats returns a snapshot of the executor counters.
func (p *Proxy) Stats() StatsSnapshot {
	return StatsSnapshot{
		Reads: p.stats.Reads.Load(), Writes: p.stats.Writes.Load(),
		Invalidations: p.stats.Invalidations.Load(), LocalCmds: p.stats.LocalCmds.Load(),
		Passthrough: p.stats.Passthrough.Load(), Unsupported: p.stats.Unsupported.Load(),
	}
}

// Execute implements frontend.Handler.
func (p *Proxy) Execute(ctx context.Context, cdb []byte, dataOut []byte, inLen int) (frontend.Result, error) {
	if len(cdb) == 0 {
		return illegal(iscsi.ASCInvalidFieldInCDB), nil
	}
	// Reads only share the cache, whose per-shard locks and single-flight fill
	// dedup make concurrent reads safe, so they take the shared lock: an
	// initiator keeps several commands outstanding and collapsing them to one
	// wastes its queue depth. Everything else takes the exclusive lock, because
	// a write's backend update plus invalidation must not interleave with a read
	// fill of the same range.
	if cdb[0] == iscsi.SCSIRead10 || cdb[0] == iscsi.SCSIRead16 {
		p.execMu.RLock()
		defer p.execMu.RUnlock()
		return p.read(ctx, cdb, inLen)
	}
	p.execMu.Lock()
	defer p.execMu.Unlock()
	switch cdb[0] {
	case iscsi.SCSITestUnitReady:
		return p.local(good())
	case iscsi.SCSIStartStopUnit:
		// The backing target must keep running for the cache, so power
		// management is a no-op rather than a passthrough.
		return p.local(good())
	case iscsi.SCSIRequestSense:
		return p.local(frontend.Result{Status: iscsi.StatusGood, Data: iscsi.FixedSense(0, 0, 0)})

	case iscsi.SCSIInquiry:
		return p.inquiry(cdb, inLen)
	case iscsi.SCSIReportLuns:
		return p.local(withData(iscsi.BuildReportLuns(0), inLen))
	case iscsi.SCSIReadCapacity10:
		return p.readCapacity10()
	case iscsi.SCSIServiceActionIn16:
		if cdb[1]&0x1f == 0x10 {
			return p.readCapacity16()
		}
		return p.unsupported(cdb)

	case iscsi.SCSIWrite10, iscsi.SCSIWrite16:
		return p.write(ctx, cdb, dataOut)

	case iscsi.SCSISynchronizeCache10, 0x91:
		// Writes are writearound: the cache never holds dirty data, so
		// flushing it is a no-op.
		return p.local(good())

	case iscsi.SCSIUnmap:
		return p.unmap(ctx, cdb, dataOut)
	case iscsi.SCSIModeSense6, iscsi.SCSIModeSense10, iscsi.SCSIModeSelect6, iscsi.SCSIModeSelect10:
		return p.passthrough(ctx, cdb, dataOut, inLen)
	default:
		return p.unsupported(cdb)
	}
}

// Local commands --------------------------------------------------------------------

func good() frontend.Result { return frontend.Result{Status: iscsi.StatusGood} }

func illegal(asc uint8) frontend.Result {
	return frontend.Result{Status: iscsi.StatusCheckCondition, Sense: iscsi.IllegalRequestSense(asc)}
}

func withData(data []byte, inLen int) frontend.Result {
	if inLen > 0 && len(data) > inLen {
		data = data[:inLen]
	}
	return frontend.Result{Status: iscsi.StatusGood, Data: data}
}

func (p *Proxy) local(r frontend.Result) (frontend.Result, error) {
	p.stats.LocalCmds.Add(1)
	return r, nil
}

func (p *Proxy) inquiry(cdb []byte, inLen int) (frontend.Result, error) {
	params, err := iscsi.ParseInquiry(cdb)
	if err != nil {
		return p.unsupported(cdb)
	}
	devType := p.dev.DeviceType
	var data []byte
	if params.EVPD {
		switch params.PageCode {
		case 0x00:
			data = iscsi.VPDPage(devType, 0x00, iscsi.VPDSupportedPages(0x00, 0x80, 0x83))
		case 0x80:
			data = iscsi.VPDSerial(devType, p.dev.Serial)
		case 0x83:
			data = iscsi.VPDDeviceID(devType,
				iscsi.T10VendorDescriptor(p.dev.Vendor, p.dev.Serial),
				iscsi.SCSINameDescriptor(firstNonEmpty(p.iqn, p.dev.Serial)))
		default:
			return p.unsupported(cdb)
		}
	} else {
		if params.PageCode != 0 {
			return p.unsupported(cdb)
		}
		data = iscsi.StandardInquiry(devType, false, p.dev.Vendor, p.dev.Product, p.dev.Revision)
	}
	return p.local(withData(data, inLen))
}

func (p *Proxy) readCapacity10() (frontend.Result, error) {
	last := uint64(0)
	if p.dev.BlockCount > 0 {
		last = p.dev.BlockCount - 1
	}
	if last > 0xffffffff {
		// Signal that READ CAPACITY(16) must be used.
		last = 0xffffffff
	}
	return p.local(frontend.Result{
		Status: iscsi.StatusGood,
		Data:   iscsi.BuildReadCapacity10(uint32(last), p.dev.BlockSize),
	})
}

func (p *Proxy) readCapacity16() (frontend.Result, error) {
	last := uint64(0)
	if p.dev.BlockCount > 0 {
		last = p.dev.BlockCount - 1
	}
	return p.local(frontend.Result{
		Status: iscsi.StatusGood,
		Data:   iscsi.BuildReadCapacity16(last, p.dev.BlockSize, 1),
	})
}

// Reads -----------------------------------------------------------------------------

func (p *Proxy) read(ctx context.Context, cdb []byte, inLen int) (frontend.Result, error) {
	params, err := iscsi.ParseReadWrite(cdb)
	if err != nil {
		return p.unsupported(cdb)
	}
	params.BlockSize = p.dev.BlockSize
	length := params.TransferLength()
	if length == 0 {
		return p.local(good())
	}
	if res, bad := p.bounds(params, length); bad {
		return res, nil
	}

	buf := make([]byte, length)
	if p.c == nil {
		if err := p.source().ReadAt(ctx, int64(params.LBA)*int64(p.dev.BlockSize), buf); err != nil {
			return frontend.Result{}, err
		}
	} else if err := p.c.Read(ctx, int64(params.LBA)*int64(p.dev.BlockSize), buf, p.source()); err != nil {
		return frontend.Result{}, err
	}
	p.stats.Reads.Add(1)
	if inLen > 0 && len(buf) > inLen {
		buf = buf[:inLen]
	}
	return frontend.Result{Status: iscsi.StatusGood, Data: buf}, nil
}

// bounds rejects a CDB whose LBA range leaves the LUN.
func (p *Proxy) bounds(params iscsi.ReadWriteParams, length int64) (frontend.Result, bool) {
	end := int64(params.LBA)*int64(p.dev.BlockSize) + length
	if end > p.dev.CapacityBytes() || params.LBA > p.dev.BlockCount {
		return illegal(iscsi.ASCLogicalBlockAddressOutOfRange), true
	}
	return frontend.Result{}, false
}

// Writes ----------------------------------------------------------------------------

func (p *Proxy) write(ctx context.Context, cdb []byte, dataOut []byte) (frontend.Result, error) {
	params, err := iscsi.ParseReadWrite(cdb)
	if err != nil {
		return p.unsupported(cdb)
	}
	params.BlockSize = p.dev.BlockSize
	length := params.TransferLength()
	if length == 0 {
		return p.local(good())
	}
	if res, bad := p.bounds(params, length); bad {
		return res, nil
	}
	if int64(len(dataOut)) != length {
		return illegal(iscsi.ASCInvalidFieldInCDB), nil
	}

	res, err := p.be.Exec(ctx, cdb, dataOut, 0)
	if err != nil {
		return frontend.Result{}, err
	}
	if !res.OK() {
		return frontend.Result{Status: res.Status, Sense: res.Sense}, nil
	}
	p.stats.Writes.Add(1)
	// The cache must be consistent before the write is acknowledged.
	if p.c != nil {
		p.c.Invalidate(int64(params.LBA)*int64(p.dev.BlockSize), length)
		p.stats.Invalidations.Add(1)
	}
	return good(), nil
}

// unmap forwards UNMAP and drops the unmapped ranges from the cache.
func (p *Proxy) unmap(ctx context.Context, cdb []byte, dataOut []byte) (frontend.Result, error) {
	res, err := p.be.Exec(ctx, cdb, dataOut, 0)
	if err != nil {
		return frontend.Result{}, err
	}
	if !res.OK() {
		return frontend.Result{Status: res.Status, Sense: res.Sense}, nil
	}
	if p.c != nil {
		for _, r := range parseUnmapRanges(dataOut, p.dev.BlockSize) {
			p.c.Invalidate(r.off, r.length)
		}
	}
	p.stats.Passthrough.Add(1)
	return good(), nil
}

type byteRange struct{ off, length int64 }

// parseUnmapRanges decodes the UNMAP parameter list (SBC-3 5.30): an 8 byte
// header followed by 16 byte block descriptors of {LBA, block count}.
func parseUnmapRanges(list []byte, blockSize uint32) []byteRange {
	if len(list) < 8 {
		return nil
	}
	n := int(binary.BigEndian.Uint16(list[0:2]))
	if n > len(list)-8 {
		n = len(list) - 8
	}
	var out []byteRange
	for off := 8; off+16 <= 8+n; off += 16 {
		lba := binary.BigEndian.Uint64(list[off : off+8])
		blocks := binary.BigEndian.Uint32(list[off+8 : off+12])
		if blocks == 0 {
			continue
		}
		out = append(out, byteRange{
			off:    int64(lba) * int64(blockSize),
			length: int64(blocks) * int64(blockSize),
		})
	}
	return out
}

// Passthrough -----------------------------------------------------------------------

func (p *Proxy) passthrough(ctx context.Context, cdb []byte, dataOut []byte, inLen int) (frontend.Result, error) {
	res, err := p.be.Exec(ctx, cdb, dataOut, inLen)
	if err != nil {
		return frontend.Result{}, err
	}
	p.stats.Passthrough.Add(1)
	out := frontend.Result{Status: res.Status, Sense: res.Sense, Residual: res.Residual}
	if inLen > 0 && len(res.Data) > inLen {
		out.Data = res.Data[:inLen]
	} else {
		out.Data = res.Data
	}
	return out, nil
}

func (p *Proxy) unsupported(cdb []byte) (frontend.Result, error) {
	p.stats.Unsupported.Add(1)
	p.log.Debug("proxy: unsupported command", "opcode", fmt.Sprintf("0x%02x", cdb[0]))
	return illegal(iscsi.ASCInvalidCommandOperationCode), nil
}

// Source ----------------------------------------------------------------------------

// source adapts the backend into the cache's Source interface. Reads are always
// sector aligned by the cache, so the CDB can be built directly from the byte
// range.
func (p *Proxy) source() source { return source{p: p} }

type source struct{ p *Proxy }

func (s source) ReadAt(ctx context.Context, off int64, dst []byte) error {
	p := s.p
	bs := int64(p.dev.BlockSize)
	if off < 0 || off%bs != 0 {
		return fmt.Errorf("proxy: unaligned read offset %d", off)
	}
	if int64(len(dst))%bs != 0 {
		return fmt.Errorf("proxy: read length %d is not a multiple of the block size %d", len(dst), bs)
	}
	lba := uint64(off / bs)
	blocks := uint64(len(dst)) / uint64(bs)

	cdb := make([]byte, 16)
	if lba <= 0xffffffff && blocks <= 0xffff {
		cdb[0] = iscsi.SCSIRead10
		binary.BigEndian.PutUint32(cdb[2:6], uint32(lba))
		binary.BigEndian.PutUint16(cdb[7:9], uint16(blocks))
		cdb = cdb[:10]
	} else {
		cdb[0] = iscsi.SCSIRead16
		binary.BigEndian.PutUint64(cdb[2:10], lba)
		binary.BigEndian.PutUint32(cdb[10:14], uint32(blocks))
	}

	res, err := p.be.Exec(ctx, cdb, nil, len(dst))
	if err != nil {
		return err
	}
	if !res.OK() {
		return fmt.Errorf("proxy: backend read at %d failed: status 0x%02x %s",
			off, res.Status, iscsi.SenseFromResult(res.Sense))
	}
	if len(res.Data) < len(dst) {
		return fmt.Errorf("proxy: backend read returned %d of %d bytes: %w", len(res.Data), len(dst), io.ErrUnexpectedEOF)
	}
	copy(dst, res.Data[:len(dst)])
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
