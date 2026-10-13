package iscsiproxy

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"vault/internal/iscsicache/iscsi"
)

// TestInjectedHitKeepsStatSNForward 锁定真机事故：命中代答的 StatSN 必须接着目标的序号走。
//
// 后端一个 1MiB 读要拆成十几段 Data-In，只有末段带 S 位，其余段的 StatSN 是保留字段
// （后端实测填 0）。队列深度 ≥2 时命中代答正好会插在这些段中间：数据泵若把无 S 位段的保留
// 值也当成序号采下来，注入响应的 StatSN 就退回 1 这类旧值，发起端判为乱序 —— 立刻 RST
// 断链再重登录。真机表现正是本题：顺序读 QD8 从 ~800MB/s 掉到 39MB/s 并反复断线，而
// QD1 顺序读与 4K 随机读（单段、永远带 S 位）完好。
func TestInjectedHitKeepsStatSNForward(t *testing.T) {
	const blockSize = 4096
	bln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("后端监听失败：%v", err)
	}
	defer bln.Close()

	seg1Sent := make(chan struct{})
	releaseSeg2 := make(chan struct{}, 1)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { releaseSeg2 <- struct{}{} }) }
	t.Cleanup(release)

	// 假后端：每发一次带 S 位的响应 StatSN 加一；1MiB 读拆两段，首段不带 S 位（保留值 0）
	// 发完就停住，等测试放行 —— 这就是真机上命中代答插进来的那个空档。
	go func() {
		bc, err := bln.Accept()
		if err != nil {
			return
		}
		defer bc.Close()
		var statSN uint32
		for {
			h, err := iscsi.ReadBHS(bc)
			if err != nil {
				return
			}
			if dsl := h.DataSegmentLength(); dsl > 0 {
				if _, err := io.CopyN(io.Discard, bc, int64(dsl+(4-dsl%4)%4)); err != nil {
					return
				}
			}
			cdb, itt := h.CDB(), h.ITT()
			exp, max := h.CmdSN()+1, h.CmdSN()+64
			send := func(data []byte, last, withStatus bool, sn, dataSN, bufOffset uint32) {
				_ = iscsi.WritePDU(bc, iscsi.BuildDataIn(itt, iscsi.ReservedTag, sn, exp, max,
					dataSN, bufOffset, data, last, withStatus, iscsi.StatusGood, 0, false, false))
			}
			switch cdb[0] {
			case iscsi.SCSIInquiry:
				var payload []byte
				switch cdb[2] {
				case 0x80:
					payload = iscsi.VPDSerial(0x00, "VAULTSERIAL001")
				case 0x83:
					payload = iscsi.VPDDeviceID(0x00, iscsi.T10VendorDescriptor("VAULT", "VAULTSERIAL001"))
				default:
					continue
				}
				statSN++
				send(payload, true, true, statSN, 0, 0)
			case iscsi.SCSIReadCapacity10:
				statSN++
				send(iscsi.BuildReadCapacity10(1<<20, blockSize), true, true, statSN, 0, 0)
			case iscsi.SCSIRead10:
				// 传输长度以 LUN 块（READ CAPACITY 报的 4096）为单位。
				blocks := int(binary.BigEndian.Uint16(cdb[7:9]))
				if blocks != 1<<20/blockSize {
					statSN++
					send(make([]byte, blocks*blockSize), true, true, statSN, 0, 0)
					continue
				}
				seg := make([]byte, 512<<10)
				send(seg, false, false, 0, 0, 0) // 首段不带 S 位：StatSN 是保留字段
				close(seg1Sent)
				<-releaseSeg2
				statSN++
				send(seg, true, true, statSN, 1, 512<<10)
			}
		}
	}()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p, err := NewPortal(PortalConfig{Logger: log})
	if err != nil {
		t.Fatalf("NewPortal 失败：%v", err)
	}
	defer p.Close()
	if err := p.Add(context.Background(), TargetConfig{
		Key:     "k",
		Backend: BackendConfig{Address: bln.Addr().String(), TargetIQN: "iqn.2026-01.com.vault:test"},
		Cache: CacheConfig{
			BlockSize: blockSize, SectorSize: 512, L1Bytes: 8 << 20,
			FillWorkers: 1, FillQueueDepth: 16,
		},
	}); err != nil {
		t.Fatalf("Add 失败：%v", err)
	}

	host, port, ok := p.TargetAddr("k")
	if !ok {
		t.Fatal("TargetAddr 未返回代理地址")
	}
	ic, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("连接代理失败：%v", err)
	}
	defer ic.Close()

	type pduSeen struct {
		itt     uint32
		statSN  uint32
		hasStat bool
		dsl     int
	}
	var mu sync.Mutex
	var seen []pduSeen
	go func() {
		for {
			pdu, err := iscsi.ReadPDU(ic)
			if err != nil {
				return
			}
			h := pdu.Header
			rec := pduSeen{itt: h.ITT(), statSN: h.StatSN(), dsl: len(pdu.Data)}
			rec.hasStat = h.Opcode() == iscsi.OpDataIn && h.Flags()&iscsi.DataInFlagStatus != 0
			mu.Lock()
			seen = append(seen, rec)
			mu.Unlock()
		}
	}()
	snapshot := func() []pduSeen {
		mu.Lock()
		defer mu.Unlock()
		return append([]pduSeen(nil), seen...)
	}
	// statusOf 返回某个 ITT 上带 S 位响应的 StatSN。
	statusOf := func(recs []pduSeen, itt uint32) (uint32, bool) {
		for _, r := range recs {
			if r.itt == itt && r.hasStat {
				return r.statSN, true
			}
		}
		return 0, false
	}
	// lastStatusStatSN 是发起端看到的最新一个带 S 位响应的 StatSN。
	lastStatusStatSN := func() (uint32, bool) {
		recs := snapshot()
		for i := len(recs) - 1; i >= 0; i-- {
			if recs[i].hasStat {
				return recs[i].statSN, true
			}
		}
		return 0, false
	}
	waitFor := func(timeout time.Duration, msg string, cond func([]pduSeen) bool) {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for {
			recs := snapshot()
			if cond(recs) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s：超时（已收到 %d 个 PDU）", msg, len(recs))
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	cmdSN := uint32(0)
	sendCmd := func(itt uint32, cdb ...byte) {
		t.Helper()
		cmdSN++
		var h iscsi.BHS
		h[0] = iscsi.OpSCSICommand | 0x80 // F bit：命令在本 PDU 内结束
		h.SetITT(itt)
		h.SetCmdSN(cmdSN)
		copy(h[32:48], cdb)
		if err := iscsi.WritePDU(ic, &iscsi.PDU{Header: h}); err != nil {
			t.Fatalf("发送命令 0x%02x 失败：%v", cdb[0], err)
		}
	}
	readCDB := func(lba uint32, blocks uint16) []byte {
		cdb := make([]byte, 10)
		cdb[0] = iscsi.SCSIRead10
		binary.BigEndian.PutUint32(cdb[2:6], lba)
		binary.BigEndian.PutUint16(cdb[7:9], blocks)
		return cdb
	}

	sendCmd(1, iscsi.SCSIInquiry, 0x01, 0x80, 0x00, 0xff, 0x00)
	sendCmd(2, iscsi.SCSIInquiry, 0x01, 0x83, 0x00, 0xff, 0x00)
	sendCmd(3, iscsi.SCSIReadCapacity10, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	waitFor(3*time.Second, "等身份嗅探绑定缓存", func([]pduSeen) bool {
		st, ok := p.Stats("k")
		return ok && st.BlockSize == blockSize
	})

	// 预热 4KiB 区间：回填是异步的，反复读直到代理本地代答一次为止。
	itt := uint32(16)
	for i := 0; ; i++ {
		if i == 200 {
			st, _ := p.Stats("k")
			t.Fatalf("反复读同一 4KiB 区间后缓存仍未命中：Cache=%+v Proxy=%+v", st.Cache, st.Proxy)
		}
		itt++
		sendCmd(itt, readCDB(0, 1)...)
		waitFor(3*time.Second, "等预热读的响应", func(recs []pduSeen) bool {
			_, ok := statusOf(recs, itt)
			return ok
		})
		if st, ok := p.Stats("k"); ok && st.Proxy.LocalCmds > 0 {
			break
		}
	}
	base, ok := lastStatusStatSN()
	if !ok {
		t.Fatal("没有观察到任何带 S 位的响应")
	}

	// 事故现场：1MiB 穿透读（首段不带 S 位）停在半路，此时插入一次命中代答。
	sendCmd(100, readCDB(1<<20/blockSize, 1<<20/blockSize)...)
	waitFor(5*time.Second, "等 1MiB 读的首段到达发起端", func(recs []pduSeen) bool {
		for _, r := range recs {
			if r.itt == 100 && !r.hasStat && r.dsl > 0 {
				return true
			}
		}
		return false
	})
	sendCmd(101, readCDB(0, 1)...)
	waitFor(5*time.Second, "等插入的命中代答", func(recs []pduSeen) bool {
		_, ok := statusOf(recs, 101)
		return ok
	})
	if got, _ := statusOf(snapshot(), 101); got != base+1 {
		t.Fatalf("命中代答的 StatSN 没接着目标序号走：got=%d want=%d。"+
			"目标刚发过不带 S 位的段（保留值 0），把保留值当序号用会让发起端判乱序并 RST 断链", got, base+1)
	}

	// 会话必须还活着：放行 1MiB 读的末段，随后同一 4KiB 区间仍要由缓存本地代答。
	release()
	waitFor(5*time.Second, "等 1MiB 读的末段补齐", func(recs []pduSeen) bool {
		total := 0
		for _, r := range recs {
			if r.itt == 100 {
				total += r.dsl
			}
		}
		return total == 1<<20
	})
	before, _ := p.Stats("k")
	sendCmd(102, readCDB(0, 1)...)
	waitFor(5*time.Second, "等放行后的命中代答", func(recs []pduSeen) bool {
		_, ok := statusOf(recs, 102)
		return ok
	})
	after, _ := p.Stats("k")
	if after.Proxy.LocalCmds <= before.Proxy.LocalCmds {
		t.Fatal("插入代答之后缓存不再命中，会话可能已被断开")
	}
	if after.Proxy.Degraded != 0 {
		t.Fatalf("会话被降级为纯透传：Degraded=%d", after.Proxy.Degraded)
	}
}
