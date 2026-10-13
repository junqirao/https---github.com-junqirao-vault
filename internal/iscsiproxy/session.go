package iscsiproxy

import (
	"encoding/binary"
	"log/slog"
	"net"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"vault/internal/iscsicache/iscsi"
)

const (
	// defaultMRDSL is the RFC 7143 default MaxRecvDataSegmentLength.
	defaultMRDSL = 8192
	// maxInjectionSegment caps an injected Data-In segment regardless of the
	// negotiated value, bounding the work done before a send.
	maxInjectionSegment = 1 << 20
)

// pendingKind classifies an in-flight command whose response the proxy watches.
type pendingKind uint8

const (
	pendRead pendingKind = iota
	pendInquirySerial
	pendInquiryWWID
	pendCapacity
)

// pending is one command the proxy is collecting a response for: a read it will
// hand to the cache once complete, or an INQUIRY/READ CAPACITY probe feeding the
// LUN identity.
type pending struct {
	kind   pendingKind
	off    int64
	length int
	buf    []byte
	got    int
}

// session is one initiator TCP connection spliced to one backend connection.
//
// The proxy is transparent: it never terminates the iSCSI session, so the
// backend login, CHAP and negotiation all run end to end. It only watches the
// byte stream, rewrites sequence numbers when it answers a read locally and, on
// a miss, tees the returned data into the cache.
type session struct {
	tgt *target
	it  net.Conn
	bt  net.Conn
	log *slog.Logger

	digests digestFlags

	// outMu serialises every write to the initiator and guards offset/lastStat/
	// expCmd/maxCmd, which the T→I pump shares with the injected answers.
	outMu    sync.Mutex
	offset   uint32 // commands answered locally == injected Data-In sequences
	lastStat uint32 // last StatSN the initiator was sent
	expCmd   uint32 // latest ExpCmdSN the initiator was told
	maxCmd   uint32 // latest MaxCmdSN the initiator was told

	negMu         sync.Mutex
	degraded      atomic.Bool // 每帧都要读，走原子位而不是锁
	degradeReason string      // 仅在 negMu 下于置位前写一次
	loginDone     bool
	initMRDSL     int
	tgtMRDSL      int

	ittMu     sync.Mutex
	pending   map[uint32]*pending
	pendingN  atomic.Int32 // collectTI 的快速路径：为 0 时跳过锁与 map 查找
	closeOnce sync.Once
}

func newSession(t *target, it, bt net.Conn) *session {
	return &session{
		tgt:       t,
		it:        it,
		bt:        bt,
		log:       t.log,
		expCmd:    1,
		initMRDSL: defaultMRDSL,
		tgtMRDSL:  defaultMRDSL,
		pending:   make(map[uint32]*pending),
	}
}

// run pumps both directions until either side closes, then tears the session
// down.
func (s *session) run() {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.guardedPump("IT", s.pumpIT) }()
	go func() { defer wg.Done(); s.guardedPump("TI", s.pumpTI) }()
	wg.Wait()
	s.close()
}

// guardedPump 跑一个数据泵并**兜住 panic**。
//
// 为什么必须有：这两个泵是普通 goroutine，任何一处 panic（畸形 PDU、越界索引、nil 解引用…）
// 都会**终止整个代理进程**。而代理是 Electron 的子进程、stderr 被主进程丢弃
// （见 apps/client/electron/agent.ts 的 `child.stderr.on('data', () => undefined)`），
// 于是 panic 堆栈既进不了日志文件、界面也看不到 —— 真实故障现象正是"挂载到一半代理无声消失、
// 日志里什么都没有"。兜住之后：只断这一条会话（对向泵被 close 唤醒后随之退出），代理继续服务
// 其他分配，并把 panic 与堆栈写进日志文件供定位。
func (s *session) guardedPump(dir string, pump func()) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("iscsiproxy: 数据泵 panic，已断开该会话（代理继续运行）",
				"engine", Version, "key", s.tgt.key, "dir", dir,
				"panic", r, "stack", string(debug.Stack()))
			s.close()
		}
	}()
	pump()
}

func (s *session) close() {
	s.closeOnce.Do(func() {
		_ = s.it.Close()
		_ = s.bt.Close()
	})
}

func (s *session) isDegraded() bool { return s.degraded.Load() }

// degrade marks the session as a permanent pure byte splice. The first reason is
// kept for the log. The reason is written under negMu before the atomic flag is
// published, so a reader that observes the flag can safely read the reason under
// the same lock.
func (s *session) degrade(reason string) {
	if s.degraded.Load() {
		return
	}
	s.negMu.Lock()
	if !s.degraded.Load() {
		s.degradeReason = reason
		s.degraded.Store(true)
	}
	s.negMu.Unlock()
}

// pumpIT forwards initiator→backend. It observes commands to invalidate on
// writes and to answer reads from the cache; everything else is spliced after
// the sequence numbers are pulled back.
func (s *session) pumpIT() {
	defer s.close()
	fr := newFrameReader(s.it, &s.digests)
	scratch := make([]byte, writeCoalesce+8)
	for {
		f, err := fr.read()
		if err != nil {
			return
		}
		s.observeLoginIT(f)
		if s.isDegraded() {
			// 降级会话的余下流量不再逐帧解析：当前帧发走后直接裸拷贝。
			if err := writeFrame(s.bt, f, scratch); err != nil {
				return
			}
			fr.splice(s.bt)
			return
		}
		if s.handleCommandIT(f) {
			continue // answered locally, never reaches the backend
		}
		rewriteIT(&f.hdr, s.offset)
		s.tgt.stats.passthrough.Add(1)
		if err := writeFrame(s.bt, f, scratch); err != nil {
			return
		}
	}
}

// pumpTI forwards backend→initiator. It rewrites the target's sequence numbers,
// watches responses for the tee and the LUN identity, and serialises writes with
// the injection path.
func (s *session) pumpTI() {
	defer s.close()
	fr := newFrameReader(s.bt, &s.digests)
	scratch := make([]byte, writeCoalesce+8)
	for {
		f, err := fr.read()
		if err != nil {
			return
		}
		s.observeLoginTI(f)
		if s.isDegraded() {
			if err := writeFrame(s.it, f, scratch); err != nil {
				return
			}
			fr.splice(s.it)
			return
		}
		s.collectTI(f)
		s.outMu.Lock()
		rewriteTI(&f.hdr, s.offset)
		s.captureTargetSeq(&f.hdr)
		err = writeFrame(s.it, f, scratch)
		s.outMu.Unlock()
		if err != nil {
			return
		}
	}
}

// captureTargetSeq records the target's sequence numbers so an injected cache hit
// can continue them. It must run under outMu, right where the PDU is written.
//
// StatSN is only taken from a PDU that actually carries one. A Data-In PDU without
// the S bit has a reserved StatSN (the backend fills 0, RFC 7143 §10.7.4 wants
// 0xffffffff), so taking it would twist the counter backwards. That is not
// theoretical: one 1MiB passthrough read arrives as a dozen Data-In PDUs of which
// only the last has the S bit, and at queue depth ≥2 a locally answered hit lands
// in the middle of that burst. The injected StatSN was then computed from the
// reserved value (offset+1 and friends) — the initiator saw StatSN move backwards,
// reset the connection and re-logged in. Measured on a real machine: sequential
// QD1 reads ran at 711MB/s, QD8 collapsed to 39MB/s with endless reconnects, while
// 4K reads (single PDU, always with the S bit) stayed healthy.
func (s *session) captureTargetSeq(h *iscsi.BHS) {
	if h.Opcode() < 0x20 {
		return
	}
	if h.Opcode() != iscsi.OpDataIn || h.Flags()&iscsi.DataInFlagStatus != 0 {
		s.lastStat = h.StatSN()
	}
	s.expCmd = h.ExpCmdSN()
	s.maxCmd = h.MaxCmdSN()
}

// handleCommandIT dispatches a SCSI command. It returns true when the command
// was answered locally and must not be forwarded.
func (s *session) handleCommandIT(f *frame) bool {
	if f.hdr.Opcode() != iscsi.OpSCSICommand {
		return false
	}
	cdb := f.hdr.CDB()
	switch cdb[0] {
	case iscsi.SCSIRead10, iscsi.SCSIRead16:
		// 计在**命令观测**层，而不是缓存的命中层：这样只要代理看到读流量，
		// 读 IOPS 就有数（缓存尚未绑定/首次未命中也不例外）。命中率另用 RequestHits。
		s.tgt.stats.reads.Add(1)
		return s.handleRead(f)
	case iscsi.SCSIInquiry:
		s.handleInquiry(f)
	case iscsi.SCSIReadCapacity10:
		s.watchCapacity(f.hdr.ITT(), 8)
	case iscsi.SCSIServiceActionIn16:
		if len(cdb) >= 2 && cdb[1] == 0x10 { // READ CAPACITY(16)
			s.watchCapacity(f.hdr.ITT(), 32)
		}
	case iscsi.SCSIWrite10, iscsi.SCSIWrite16:
		s.invalidateWrite(cdb)
	case iscsi.SCSIUnmap:
		s.dropAll()
	case 0x41, 0x93: // WRITE SAME(10) / WRITE SAME(16)
		s.dropAll()
	}
	return false
}

// readBufPools recycle the buffers handleRead uses to stage read data. Read
// sizes cluster at a few values (4KiB random, 64KiB MRDSL segments, 1MiB
// benchmark reads), so the pool is sharded into size classes: without that a
// 4KiB read would pin a 4MiB buffer.
var readBufPools [4]sync.Pool

// readBufClassSizes are the capacities of readBufPools. A buffer's class is the
// smallest size that fits it, so readBufPut can recover the class from cap.
var readBufClassSizes = [4]int{64 << 10, 256 << 10, 1 << 20, maxTeeBytes}

func readBufClass(n int) int {
	for i, sz := range readBufClassSizes {
		if n <= sz {
			return i
		}
	}
	return len(readBufClassSizes) - 1
}

// readBufGet returns a buffer of n bytes whose cap is its class size.
func readBufGet(n int) []byte {
	cl := readBufClass(n)
	if v := readBufPools[cl].Get(); v != nil {
		return v.([]byte)[:n]
	}
	return make([]byte, readBufClassSizes[cl])[:n]
}

// readBufPut returns a buffer obtained from readBufGet; the caller must not
// touch it afterwards.
func readBufPut(b []byte) {
	readBufPools[readBufClass(cap(b))].Put(b[:cap(b)])
}

// handleRead serves a read from the cache when the whole range is present in L1
// or L2, otherwise registers it for a tee fill and lets it pass through.
func (s *session) handleRead(f *frame) bool {
	t := s.tgt
	bs := t.lunBlockSize()
	if bs == 0 {
		return false
	}
	c := t.getCache()
	if c == nil {
		return false
	}
	p, err := iscsi.ParseReadWrite(f.hdr.CDB())
	if err != nil {
		return false
	}
	length := int64(p.Blocks) * int64(bs)
	if length <= 0 || length > maxTeeBytes {
		return false
	}
	off := int64(p.LBA) * int64(bs)
	// 扫描准入先于分配：被判定为顺序扫描的读既不回填也不该白分配一个大缓冲区。
	scanned := t.scan.scan(off, length)
	buf := readBufGet(int(length))
	// L1 或 L2 全命中才本地代答：L2 命中的扇区会顺带提升进 L1（见 ReadCachedBoth）。
	if c.ReadCachedBoth(off, buf) {
		s.outMu.Lock()
		err := s.injectLocked(&f.hdr, buf)
		s.outMu.Unlock()
		readBufPut(buf)
		if err != nil {
			s.log.Warn("iscsiproxy: 注入命中响应失败，断开会话", "key", t.key, "error", err)
			s.close()
			return true
		}
		t.stats.localCmds.Add(1)
		return true
	}
	if scanned {
		readBufPut(buf)
		return false
	}
	// Miss: register the read so its Data-In sequence is teed into the cache.
	// buf 的所有权移交给 pending：它的子切片会被 FillObserved 挂进 fill 队列
	// 由 worker 异步消费，此处绝不能归还池中。
	s.watchRead(f.hdr.ITT(), off, buf)
	return false
}

// watchRead registers a read whose Data-In sequence is teed into the cache once
// it completes. buf is handed to the pending and filled from the response's
// BufOffset-tagged segments.
func (s *session) watchRead(itt uint32, off int64, buf []byte) {
	s.registerPending(itt, &pending{kind: pendRead, off: off, length: len(buf), buf: buf})
}

func (s *session) handleInquiry(f *frame) {
	if s.tgt.identitySeen() {
		return
	}
	ip, err := iscsi.ParseInquiry(f.hdr.CDB())
	if err != nil || !ip.EVPD {
		return
	}
	var kind pendingKind
	switch ip.PageCode {
	case 0x80:
		kind = pendInquirySerial
	case 0x83:
		kind = pendInquiryWWID
	default:
		return
	}
	n := int(ip.AllocLength)
	if n <= 4 {
		n = 255
	}
	if n > 4096 {
		n = 4096
	}
	s.registerPending(f.hdr.ITT(), &pending{kind: kind, buf: make([]byte, n)})
}

func (s *session) watchCapacity(itt uint32, n int) {
	if s.tgt.identitySeen() {
		return
	}
	s.registerPending(itt, &pending{kind: pendCapacity, buf: make([]byte, n)})
}

func (s *session) invalidateWrite(cdb []byte) {
	t := s.tgt
	t.stats.writes.Add(1)
	bs := t.lunBlockSize()
	if bs == 0 {
		return
	}
	c := t.getCache()
	if c == nil {
		return
	}
	p, err := iscsi.ParseReadWrite(cdb)
	if err != nil {
		return
	}
	length := int64(p.Blocks) * int64(bs)
	if length <= 0 {
		return
	}
	// 写路径只把 L1 失效同步做掉（廉价、纯内存，且是"写完再读不能读到旧数据"的
	// 正确性底线），L2 的失效交给缓存自己的后台 worker 异步做。同步失效里真正贵的
	// 是等待在飞的 L2 回填落盘（fence）与遍历 L2 索引 —— 把它放在 I→T 泵里会直接
	// 拖慢每一次写命令的转发。缓存在 L2 失效落地前用 l2InvN 门控读路径，保证读不会
	// 从 L2 拿到刚被写入失效的旧扇区（见 Cache.InvalidateAsync）。
	c.InvalidateAsync(int64(p.LBA)*int64(bs), length)
	t.stats.invalidations.Add(1)
}

func (s *session) dropAll() {
	t := s.tgt
	t.stats.writes.Add(1)
	if c := t.getCache(); c != nil {
		c.Drop()
		t.stats.invalidations.Add(1)
	}
}

// injectBufCap is the largest assembled injection: maxTeeBytes of data plus one
// 48-byte BHS and up to 3 bytes of padding per segment. MRDSL is never below
// 512 (RFC 7143), so the segment count is bounded by maxTeeBytes/512.
const injectBufCap = maxTeeBytes + (maxTeeBytes/512+2)*51

// injectBufPool holds the assembly buffers for injectLocked. The whole Data-In
// sequence is flattened and sent with a single Write: a 1MiB hit at 64KiB MRDSL
// otherwise costs 32 socket sends (header+body per segment), which dominates
// the hit path's CPU once the cache is warm.
var injectBufPool = sync.Pool{New: func() any { return make([]byte, injectBufCap) }}

var zeroPad [3]byte

// injectLocked writes a full Data-In sequence for a cached read. The caller
// holds outMu.
func (s *session) injectLocked(cmd *iscsi.BHS, data []byte) error {
	exp := s.expCmd
	if exp <= cmd.CmdSN() {
		exp = cmd.CmdSN() + 1
	}
	max := s.maxCmd
	if max < exp {
		max = exp
	}
	statSN := s.lastStat + 1
	mrdsl := s.maxSendSegment()
	itt, lun := cmd.ITT(), cmd.LUN()

	out := injectBufPool.Get().([]byte)
	defer injectBufPool.Put(out)
	pos := 0
	seg, dataSN, bufOffset := 0, uint32(0), uint32(0)
	n := len(data)
	for {
		chunk := n - seg
		if chunk > mrdsl {
			chunk = mrdsl
		}
		last := seg+chunk >= n
		var stat uint32
		if last {
			stat = statSN
		}
		// 非末段不带 S 位，StatSN 是保留字段，保持与后端同一个填法（0），
		// 它不参与序号推进 —— pumpTI 也据此不采集它。
		var hdr iscsi.BHS
		iscsi.FillDataInHeader(&hdr, itt, iscsi.ReservedTag, stat, exp, max,
			dataSN, bufOffset, chunk, last, true, iscsi.StatusGood, 0, false, false)
		hdr.SetLUN(lun)
		pos += copy(out[pos:], hdr[:])
		pos += copy(out[pos:], data[seg:seg+chunk])
		if pad := (4 - chunk%4) % 4; pad > 0 {
			pos += copy(out[pos:], zeroPad[:pad])
		}
		seg += chunk
		dataSN++
		bufOffset += uint32(chunk)
		if last {
			break
		}
	}
	if _, err := s.it.Write(out[:pos]); err != nil {
		return err
	}
	s.lastStat = statSN
	s.offset++
	return nil
}

func (s *session) maxSendSegment() int {
	s.negMu.Lock()
	m := s.initMRDSL
	s.negMu.Unlock()
	if m <= 0 {
		m = defaultMRDSL
	}
	if m > maxInjectionSegment {
		m = maxInjectionSegment
	}
	return m
}

// registerPending adds a watched command, dropping it when the table is full.
func (s *session) registerPending(itt uint32, p *pending) {
	s.ittMu.Lock()
	if len(s.pending) < maxPending {
		s.pending[itt] = p
		s.pendingN.Store(int32(len(s.pending)))
	}
	s.ittMu.Unlock()
}

// collectTI tees a Data-In segment into the buffer of the command it belongs to
// and completes it on the final PDU.
func (s *session) collectTI(f *frame) {
	if f.hdr.Opcode() != iscsi.OpDataIn || s.pendingN.Load() == 0 {
		return
	}
	itt := f.hdr.ITT()
	s.ittMu.Lock()
	p := s.pending[itt]
	s.ittMu.Unlock()
	if p == nil {
		return
	}
	if data := f.data(); len(data) > 0 {
		if off := int(f.hdr.BufOffset()); off >= 0 && off+len(data) <= len(p.buf) {
			copy(p.buf[off:], data)
			if off+len(data) > p.got {
				p.got = off + len(data)
			}
		}
	}
	// 序列的最后一段由 Data-In 的 S 位标识（RFC 7143：带 S 的 PDU 同时携带
	// SCSI Status）。绝不能拿 BHS 字节 0 的 F 位判断：Data-In 的字节 0 只有
	// opcode，该位恒为 0，会把每一段都当成"非最后一段"，pending 永不出队、
	// 缓存既不回填也不绑定（真机表现即"读 IOPS 有数、缓存占用恒 0"）。
	// 这与后端发起端 backend/initiator.go 的完成判据保持一致。
	if f.hdr.Flags()&iscsi.DataInFlagStatus == 0 {
		return
	}
	s.ittMu.Lock()
	delete(s.pending, itt)
	s.pendingN.Store(int32(len(s.pending)))
	s.ittMu.Unlock()
	if f.hdr[3] != iscsi.StatusGood {
		return
	}
	s.finishPending(p)
}

func (s *session) finishPending(p *pending) {
	t := s.tgt
	switch p.kind {
	case pendRead:
		if p.got < p.length {
			return
		}
		if c := t.getCache(); c != nil {
			c.FillObserved(p.off, p.buf)
		}
	case pendInquirySerial, pendInquiryWWID:
		if p.got < 4 {
			return
		}
		page := p.buf[1]
		// VPD 页长度字段是**后端自己声明的**，可能大于我们按发起端 AllocLength 申请的缓冲。
		// 真机实测：INQUIRY 缓冲 255 字节，后端回一个自报长度 404 的页 —— 旧代码直接拿
		// 未夹取的 4+n 去切片（`p.buf[4:4+n]`），越界 panic "slice bounds out of range
		// [:408] with capacity 255"，而该 panic 在修复前会带走**整个代理进程**（见 guardedPump）。
		// 因此 payload 必须夹在 [4, min(4+n, got)] 内；序列号/WWID 只需页头若干字节，截断到
		// 已收集长度不影响识别。
		n := int(binary.BigEndian.Uint16(p.buf[2:4]))
		end := 4 + n
		if end > p.got {
			end = p.got
		}
		if end < 4 {
			return
		}
		payload := p.buf[4:end]
		if p.kind == pendInquirySerial && page == 0x80 {
			t.setSerial(strings.TrimSpace(string(payload)))
			t.maybeBind()
		}
		if p.kind == pendInquiryWWID && page == 0x83 {
			t.setWWID(hexIdentity(payload))
			t.maybeBind()
		}
	case pendCapacity:
		if p.got < 8 {
			return
		}
		if bs, bc, ok := parseCapacity(p.buf[:p.got]); ok {
			t.setCapacity(bs, bc)
			t.maybeBind()
		}
	}
}

// Login negotiation observation ----------------------------------------------------

func (s *session) observeLoginIT(f *frame) {
	if f.hdr.Opcode() != iscsi.OpLoginReq {
		return
	}
	s.applyLogin(iscsi.ParseText(f.data()), false)
}

func (s *session) observeLoginTI(f *frame) {
	if f.hdr.Opcode() != iscsi.OpLoginResp {
		return
	}
	s.applyLogin(iscsi.ParseText(f.data()), true)
	if f.hdr.LoginTransit() && f.hdr.LoginNSG() == iscsi.StageFullFeature && f.hdr[36] == 0 {
		s.finishLogin()
	}
}

func (s *session) applyLogin(keys map[string]string, resp bool) {
	if len(keys) == 0 {
		return
	}
	s.negMu.Lock()
	if v, ok := keys["maxrecvdatasegmentlength"]; ok {
		if n := atoiSafe(v); n > 0 {
			if resp {
				s.tgtMRDSL = n
			} else {
				s.initMRDSL = n
			}
		}
	}
	s.negMu.Unlock()

	// 降级判据只看**目标确认后的协商值**（resp 方向），绝不看发起端的报价。
	//
	// 真实教训：MS iSCSI 发起端在每个 LoginReq 里都报 MaxConnections=32、ErrorRecoveryLevel=2
	// （这是它的**报价范围**），目标才回落到 1 / 0。若按报价降级，**每个**正常会话都会被判成
	// "多连接 / 带 ERL"而退化为纯透传，缓存永远不生效（真实现象：客户端缓存用量恒为 0、
	// 日志出现 reason=MaxConnections>1）。RFC 7143 规定这两个键缺省即 1 / 0，故 resp 未带该键时
	// 视为已协商为默认值，不降级。
	if resp {
		if v, ok := keys["maxconnections"]; ok && atoiSafe(v) > 1 {
			s.degrade("MaxConnections>1")
		}
		if v, ok := keys["errorrecoverylevel"]; ok && atoiSafe(v) > 0 {
			s.degrade("ErrorRecoveryLevel>0")
		}
	}
	// SessionType 是两个方向都会声明的会话属性（发起端与目标必然一致），任一方向判定都成立。
	if v, ok := keys["sessiontype"]; ok && strings.EqualFold(strings.TrimSpace(v), "discovery") {
		s.degrade("discovery session")
	}
	if resp {
		if v, ok := keys["headerdigest"]; ok && !isNone(v) {
			s.digests.header.Store(true)
			s.degrade("HeaderDigest=" + v)
		}
		if v, ok := keys["datadigest"]; ok && !isNone(v) {
			s.digests.data.Store(true)
			s.degrade("DataDigest=" + v)
		}
	}
}

func (s *session) finishLogin() {
	s.negMu.Lock()
	if s.loginDone {
		s.negMu.Unlock()
		return
	}
	s.loginDone = true
	degraded, reason := s.degraded.Load(), s.degradeReason
	mrdsl := s.tgtMRDSL
	if mrdsl == 0 {
		mrdsl = s.initMRDSL
	}
	s.negMu.Unlock()
	if degraded {
		s.tgt.stats.degraded.Add(1)
		s.log.Info("iscsiproxy: 会话降级为纯透传",
			"engine", Version, "key", s.tgt.key, "reason", reason)
		return
	}
	// 健康会话此前是**完全静默**的：一旦客户端指标全 0，就无法区分"发起端根本没连到
	// 代理"和"连到了却没绑定缓存"。这条日志把"代理确实参与了这个会话"钉死，
	// 并顺带报出协商结果与缓存是否已绑定。
	s.log.Info("iscsiproxy: iSCSI 会话已建立，缓存代理参与转发",
		"engine", Version, "key", s.tgt.key,
		"max_send_segment", mrdsl, "cache_bound", s.tgt.getCache() != nil)
}

func atoiSafe(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0
	}
	return n
}

// isNone reports whether an iSCSI negotiation value is None (case-insensitive).
func isNone(v string) bool {
	return strings.EqualFold(strings.TrimSpace(v), "none")
}
