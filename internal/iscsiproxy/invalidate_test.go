package iscsiproxy

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"vault/internal/iscsicache/iscsi"
)

const invBlockSize = 4096

// invBackend 是一个最小的后端目标：它回答身份嗅探需要的 INQUIRY(VPD 0x80/0x83) 与
// READ CAPACITY(10)，对 READ(10) 返回固定长度的数据，并且记录每一次读的 LBA。
// 测试靠它证明某个区间到底有没有回头端。
type invBackend struct {
	ln net.Listener

	mu    sync.Mutex
	reads []uint64
}

func newInvBackend(t *testing.T) *invBackend {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("后端监听失败：%v", err)
	}
	b := &invBackend{ln: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go b.serve()
	return b
}

func (b *invBackend) serve() {
	for {
		bc, err := b.ln.Accept()
		if err != nil {
			return
		}
		go b.handle(bc)
	}
}

func (b *invBackend) handle(bc net.Conn) {
	defer bc.Close()
	var hdr [iscsi.BHSLen]byte
	for {
		if _, err := io.ReadFull(bc, hdr[:]); err != nil {
			return
		}
		var h iscsi.BHS
		copy(h[:], hdr[:])
		if dsl := h.DataSegmentLength(); dsl > 0 {
			// 连同对齐填充一起读掉：UNMAP 的参数列表也走这里。
			if _, err := io.CopyN(io.Discard, bc, int64(dsl+(4-dsl%4)%4)); err != nil {
				return
			}
		}
		cdb, itt := h.CDB(), h.ITT()
		var resp *iscsi.PDU
		switch cdb[0] {
		case iscsi.SCSIInquiry:
			switch cdb[2] {
			case 0x80:
				resp = invDataIn(itt, iscsi.VPDSerial(0x00, "VAULTSERIAL001"))
			case 0x83:
				resp = invDataIn(itt, iscsi.VPDDeviceID(0x00, iscsi.T10VendorDescriptor("VAULT", "VAULTSERIAL001")))
			default:
				continue
			}
		case iscsi.SCSIReadCapacity10:
			resp = invDataIn(itt, iscsi.BuildReadCapacity10(1023, invBlockSize))
		case iscsi.SCSIRead10:
			lba := uint32(binary.BigEndian.Uint32(cdb[2:6]))
			b.record(lba)
			resp = invDataIn(itt, make([]byte, int(binary.BigEndian.Uint16(cdb[7:9]))*invBlockSize))
		default:
			resp = invSCSIResponse(itt)
		}
		if err := iscsi.WritePDU(bc, resp); err != nil {
			return
		}
	}
}

func (b *invBackend) record(lba uint32) {
	b.mu.Lock()
	b.reads = append(b.reads, uint64(lba))
	b.mu.Unlock()
}

func (b *invBackend) readCount(lba uint64) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, v := range b.reads {
		if v == lba {
			n++
		}
	}
	return n
}

func (b *invBackend) totalReads() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.reads)
}

func invDataIn(itt uint32, payload []byte) *iscsi.PDU {
	return iscsi.BuildDataIn(itt, iscsi.ReservedTag, 1, 1, 1, 0, 0,
		payload, true, true, iscsi.StatusGood, 0, false, false)
}

func invSCSIResponse(itt uint32) *iscsi.PDU {
	var h iscsi.BHS
	h.SetOpcode(iscsi.OpSCSIResponse)
	h.SetFlags(0x80)
	h.SetITT(itt)
	h[3] = iscsi.StatusGood
	return &iscsi.PDU{Header: h}
}

// invSend 发一条 SCSI 命令（F 位带上，命令在一个 PDU 内结束）。
func invSend(t *testing.T, w io.Writer, itt uint32, cdb ...byte) {
	t.Helper()
	var h iscsi.BHS
	h[0] = iscsi.OpSCSICommand | 0x80
	h.SetITT(itt)
	copy(h[32:48], cdb)
	if err := iscsi.WritePDU(w, &iscsi.PDU{Header: h}); err != nil {
		t.Fatalf("发送命令 0x%02x 失败：%v", cdb[0], err)
	}
}

func invWait(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}

func invStartPortal(t *testing.T, be *invBackend, snapshot string) *Portal {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p, err := NewPortal(PortalConfig{Logger: log})
	if err != nil {
		t.Fatalf("NewPortal 失败：%v", err)
	}
	if err := p.Add(context.Background(), TargetConfig{
		Key:     "k",
		Backend: BackendConfig{Address: be.ln.Addr().String(), TargetIQN: "iqn.2026-01.com.vault:test"},
		Cache: CacheConfig{
			BlockSize: invBlockSize, SectorSize: 512, L1Bytes: 4 << 20,
			L1Snapshot: snapshot, FillWorkers: 1, FillQueueDepth: 16,
		},
	}); err != nil {
		p.Close()
		t.Fatalf("Add 失败：%v", err)
	}
	return p
}

func invDial(t *testing.T, p *Portal) net.Conn {
	t.Helper()
	host, port, ok := p.TargetAddr("k")
	if !ok {
		t.Fatal("TargetAddr 未返回代理地址")
	}
	ic, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("连接代理失败：%v", err)
	}
	return ic
}

// invSniff 走完身份嗅探，等缓存真的绑定上几何。
func invSniff(t *testing.T, p *Portal, ic net.Conn) {
	t.Helper()
	invSend(t, ic, 1, iscsi.SCSIInquiry, 0x01, 0x80, 0x00, 0xff, 0x00)
	invSend(t, ic, 2, iscsi.SCSIInquiry, 0x01, 0x83, 0x00, 0xff, 0x00)
	invSend(t, ic, 3, iscsi.SCSIReadCapacity10, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	invWait(t, "缓存绑定几何", func() bool {
		st, ok := p.Stats("k")
		return ok && st.BlockSize == invBlockSize
	})
}

// TestInvalidateCommandsDropOnlyTheirRanges 锁定"缓存不清理"的核心行为：UNMAP 与
// WRITE SAME 只失效它们真正覆盖的范围。
//
// 这两种命令以前整盘 Drop：Windows 的"优化驱动器"（TRIM，走的就是 UNMAP）发一次，
// 攒了半天的热数据就全没了 —— 那正是"用着用着缓存归零"。
func TestInvalidateCommandsDropOnlyTheirRanges(t *testing.T) {
	be := newInvBackend(t)
	p := invStartPortal(t, be, "")
	defer p.Close()

	ic := invDial(t, p)
	defer ic.Close()
	invSniff(t, p, ic)

	l1Used := func() int64 {
		st, _ := p.Stats("k")
		return st.Cache.L1UsedBytes
	}
	read := func(itt, lba uint32) {
		cdb := make([]byte, 10)
		cdb[0] = iscsi.SCSIRead10
		binary.BigEndian.PutUint32(cdb[2:6], lba)
		binary.BigEndian.PutUint16(cdb[7:9], 1) // 一个逻辑块 = 一个缓存块
		invSend(t, ic, itt, cdb...)
	}

	// 两个块各读一次，等异步回填落定。
	read(10, 0)
	read(11, 1)
	invWait(t, "两块进入 L1", func() bool { return l1Used() >= 2*invBlockSize })
	if got := be.totalReads(); got != 2 {
		t.Fatalf("后端读数 = %d，期望 2", got)
	}

	// 再读一次是命中，不该回头端。
	read(12, 0)
	read(13, 1)
	time.Sleep(150 * time.Millisecond)
	if got := be.totalReads(); got != 2 {
		t.Fatalf("命中后后端读数 = %d，期望 2", got)
	}

	// UNMAP 只解除 LBA 0。命令本身不带范围：范围在紧随其后的 Data-Out 参数列表里。
	unmap := make([]byte, 10)
	unmap[0] = iscsi.SCSIUnmap
	binary.BigEndian.PutUint16(unmap[7:9], 24) // 8 字节参数头 + 一个 16 字节目录
	invSend(t, ic, 20, unmap...)
	list := make([]byte, 8+16)
	binary.BigEndian.PutUint16(list[0:2], 16)
	binary.BigEndian.PutUint64(list[8:16], 0)  // LBA 0
	binary.BigEndian.PutUint32(list[16:20], 1) // 一个块
	out := iscsi.NewPDU(iscsi.OpDataOut, list)
	out.Header.SetITT(20)
	if err := iscsi.WritePDU(ic, out); err != nil {
		t.Fatalf("发送 UNMAP 参数列表失败：%v", err)
	}
	invWait(t, "UNMAP 只让 LBA 0 掉出 L1", func() bool {
		return l1Used() >= invBlockSize && l1Used() < 2*invBlockSize
	})

	// LBA 1 没被解除映射，必须还是命中：整盘 Drop 会把它一起清掉。
	read(21, 1)
	time.Sleep(150 * time.Millisecond)
	if got := be.readCount(1); got != 1 {
		t.Fatalf("UNMAP 波及了没被解除映射的区间：LBA 1 的后端读 = %d，期望 1", got)
	}

	// 被解除映射的 LBA 0 必须回头端拿新数据。
	read(22, 0)
	invWait(t, "LBA 0 重新回头端", func() bool { return be.readCount(0) >= 2 })
	invWait(t, "LBA 0 重新进入 L1", func() bool { return l1Used() >= 2*invBlockSize })

	// WRITE SAME：范围就在 CDB 里（LBA + 块数），同样只失效这一段。
	ws := make([]byte, 10)
	ws[0] = iscsi.SCSIWriteSame10
	binary.BigEndian.PutUint32(ws[2:6], 1) // LBA 1
	binary.BigEndian.PutUint16(ws[7:9], 1) // 一个块
	invSend(t, ic, 30, ws...)
	invWait(t, "WRITE SAME 只让 LBA 1 掉出 L1", func() bool {
		return l1Used() >= invBlockSize && l1Used() < 2*invBlockSize
	})

	read(31, 1)
	invWait(t, "被 WRITE SAME 覆盖的 LBA 1 回头端", func() bool { return be.readCount(1) >= 2 })

	// 而没被写到的 LBA 0 仍然是命中。
	reads0 := be.readCount(0)
	read(32, 0)
	time.Sleep(150 * time.Millisecond)
	if got := be.readCount(0); got != reads0 {
		t.Fatalf("WRITE SAME 波及了没被写到的区间：LBA 0 的后端读 = %d，期望 %d", got, reads0)
	}
}

// TestL1SnapshotSurvivesRestart 锁定用户诉求本身：正常收尾时 L1 落盘，下一次启动
// （新的 Portal、新的 target）把它装回内存。所以"停止代理"或"重新挂载"之后，热数据
// 不用再从零预热。
func TestL1SnapshotSurvivesRestart(t *testing.T) {
	be := newInvBackend(t)
	snapshot := filepath.Join(t.TempDir(), "l1.bin")

	first := invStartPortal(t, be, snapshot)
	ic1 := invDial(t, first)
	invSniff(t, first, ic1)

	read := func(ic net.Conn, itt, lba uint32) {
		cdb := make([]byte, 10)
		cdb[0] = iscsi.SCSIRead10
		binary.BigEndian.PutUint32(cdb[2:6], lba)
		binary.BigEndian.PutUint16(cdb[7:9], 1)
		invSend(t, ic, itt, cdb...)
	}
	read(ic1, 10, 0)
	invWait(t, "读进 L1", func() bool {
		st, _ := first.Stats("k")
		return st.Cache.L1UsedBytes >= invBlockSize
	})
	if got := be.totalReads(); got != 1 {
		t.Fatalf("后端读数 = %d，期望 1", got)
	}
	// 收尾：close 会先等会话退出，再把 L1 落盘。
	if err := first.Close(); err != nil {
		t.Fatalf("关闭第一个 Portal 失败：%v", err)
	}
	_ = ic1.Close()

	reads := be.totalReads()
	second := invStartPortal(t, be, snapshot)
	defer second.Close()
	ic2 := invDial(t, second)
	defer ic2.Close()
	invSniff(t, second, ic2)

	st, _ := second.Stats("k")
	if st.L1Restored == 0 {
		t.Fatalf("重启后没有从 L1 快照恢复任何块：%+v", st)
	}
	read(ic2, 11, 0)
	time.Sleep(200 * time.Millisecond)
	if got := be.totalReads(); got != reads {
		t.Fatalf("快照恢复的块仍然回头端了：后端读 %d，期望 %d", got, reads)
	}
}

// invUnmapCDB 造一条 UNMAP CDB，参数列表长度为 listLen。
func invUnmapCDB(listLen int) []byte {
	cdb := make([]byte, 10)
	cdb[0] = iscsi.SCSIUnmap
	binary.BigEndian.PutUint16(cdb[7:9], uint16(listLen))
	return cdb
}

// invUnmapList 造一份只解除 LBA lba 一个块的参数列表。
func invUnmapList(lba uint64) []byte {
	list := make([]byte, 8+16)
	binary.BigEndian.PutUint16(list[0:2], 16) // 一个描述符
	binary.BigEndian.PutUint64(list[8:16], lba)
	binary.BigEndian.PutUint32(list[16:20], 1) // 一个块
	return list
}

// invReadL1 读一个块并等它落进 L1，返回当前的 L1 用量读取函数。
func invReadL1(t *testing.T, p *Portal, ic net.Conn, be *invBackend, lba uint32) func() int64 {
	t.Helper()
	l1Used := func() int64 {
		st, _ := p.Stats("k")
		return st.Cache.L1UsedBytes
	}
	cdb := make([]byte, 10)
	cdb[0] = iscsi.SCSIRead10
	binary.BigEndian.PutUint32(cdb[2:6], lba)
	binary.BigEndian.PutUint16(cdb[7:9], 1)
	invSend(t, ic, 1000+lba, cdb...)
	invWait(t, "读进 L1", func() bool { return l1Used() >= int64((lba+1))*invBlockSize })
	return l1Used
}

// TestUnmapParamsAsImmediateData 锁定 immediate data 形态的 UNMAP：参数列表作为命令
// PDU 的数据段直接跟来，**没有 Data-Out 帧**（登录协商 InitialR2T=No 且
// ImmediateData=Yes 时 Windows 会这么做）。
//
// 以前只认 Data-Out，这类命令会在 unmapPending 里留下一个永远等不到载荷的条目：
// 攒够 maxUnmapPending 之后每条 UNMAP 都走整盘失效，缓存就会突然归零且此后一直是
// 几 KB。这里要求按范围失效，且一次整盘失效都不允许发生。
func TestUnmapParamsAsImmediateData(t *testing.T) {
	be := newInvBackend(t)
	p := invStartPortal(t, be, "")
	defer p.Close()

	ic := invDial(t, p)
	defer ic.Close()
	invSniff(t, p, ic)

	l1Used := invReadL1(t, p, ic, be, 0)
	invReadL1(t, p, ic, be, 1)
	invWait(t, "两块进入 L1", func() bool { return l1Used() >= 2*invBlockSize })
	if got := be.totalReads(); got != 2 {
		t.Fatalf("后端读数 = %d，期望 2", got)
	}

	list := invUnmapList(0)
	cmd := iscsi.NewPDU(iscsi.OpSCSICommand, list)
	cmd.Header.SetITT(20)
	cmd.Header.SetFlags(0x80)
	copy(cmd.Header[32:48], invUnmapCDB(len(list)))
	if err := iscsi.WritePDU(ic, cmd); err != nil {
		t.Fatalf("发送带 immediate data 的 UNMAP 失败：%v", err)
	}

	invWait(t, "immediate data 的 UNMAP 只让 LBA 0 掉出 L1", func() bool {
		return l1Used() >= invBlockSize && l1Used() < 2*invBlockSize
	})
	if st, _ := p.Stats("k"); st.WholeDrops != 0 {
		t.Fatalf("整盘失效计数 = %d，期望 0：immediate data 不该被当成看不懂的命令", st.WholeDrops)
	}

	// 没被解除映射的 LBA 1 必须仍然是命中。
	read := func(itt, lba uint32) {
		cdb := make([]byte, 10)
		cdb[0] = iscsi.SCSIRead10
		binary.BigEndian.PutUint32(cdb[2:6], lba)
		binary.BigEndian.PutUint16(cdb[7:9], 1)
		invSend(t, ic, itt, cdb...)
	}
	read(21, 1)
	time.Sleep(150 * time.Millisecond)
	if got := be.readCount(1); got != 1 {
		t.Fatalf("UNMAP 波及了没被解除映射的区间：LBA 1 的后端读 = %d，期望 1", got)
	}
}

// TestUnmapPendingFullDropsOnceAndRecovers 锁定"表满不再雪崩"：一批命令的参数列表
// 始终没等到时，整盘失效一次是必要的（安全性），但表必须同时清空。
//
// 不清表的话表会一直满着，此后**每一条** UNMAP 都再清一次缓存，缓存永远暖不起来
// —— 用户看到的正是"缓存掉到几 KB 之后就不再涨回去"。
func TestUnmapPendingFullDropsOnceAndRecovers(t *testing.T) {
	be := newInvBackend(t)
	p := invStartPortal(t, be, "")
	defer p.Close()

	ic := invDial(t, p)
	defer ic.Close()
	invSniff(t, p, ic)

	l1Used := invReadL1(t, p, ic, be, 0)
	invReadL1(t, p, ic, be, 1)
	invWait(t, "两块进入 L1", func() bool { return l1Used() >= 2*invBlockSize })

	// 这些 UNMAP 只登记、不带载荷（模拟"形态不认识"的那一批）。
	cdb := invUnmapCDB(24)
	for i := 0; i <= maxUnmapPending; i++ {
		invSend(t, ic, uint32(100+i), cdb...)
	}
	invWait(t, "表满后整盘失效一次", func() bool {
		st, _ := p.Stats("k")
		return st.WholeDrops >= 1
	})
	invWait(t, "L1 被清空", func() bool { return l1Used() < 2*invBlockSize })

	st, _ := p.Stats("k")
	if st.WholeDrops != 1 {
		t.Fatalf("整盘失效计数 = %d，期望恰好 1", st.WholeDrops)
	}

	// 关键：再发一条不该再整盘失效，否则缓存永远暖不起来。
	invSend(t, ic, 999, cdb...)
	time.Sleep(200 * time.Millisecond)
	if st, _ := p.Stats("k"); st.WholeDrops != 1 {
		t.Fatalf("表满之后仍在持续整盘失效：计数 = %d，期望仍是 1", st.WholeDrops)
	}
}
