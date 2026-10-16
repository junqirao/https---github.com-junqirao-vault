package iscsiproxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"

	"vault/internal/iscsicache/iscsi"
)

// TestSessionBindsCacheFromSniffedIdentity 端到端锁定 v2 透传代理的身份嗅探链路：
// 发起端发出的 INQUIRY(VPD 0x80/0x83) 与 READ CAPACITY 响应被旁路采集齐之后，
// 目标必须真的建立并绑定 L1 缓存。这条链路此前从未被任何测试覆盖，而真机上
// "读 IOPS 有数、缓存占用恒 0"正是它没有走通的表现。
func TestSessionBindsCacheFromSniffedIdentity(t *testing.T) {
	bln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("后端监听失败：%v", err)
	}
	defer bln.Close()

	go func() {
		bc, err := bln.Accept()
		if err != nil {
			return
		}
		defer bc.Close()
		var hdr [iscsi.BHSLen]byte
		for {
			if _, err := io.ReadFull(bc, hdr[:]); err != nil {
				return
			}
			var h iscsi.BHS
			copy(h[:], hdr[:])
			if dsl := h.DataSegmentLength(); dsl > 0 {
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
					resp = iscsi.BuildDataIn(itt, iscsi.ReservedTag, 1, 1, 1, 0, 0,
						iscsi.VPDSerial(0x00, "VAULTSERIAL001"), true, true, iscsi.StatusGood, 0, false, false)
				case 0x83:
					resp = iscsi.BuildDataIn(itt, iscsi.ReservedTag, 1, 1, 1, 0, 0,
						iscsi.VPDDeviceID(0x00, iscsi.T10VendorDescriptor("VAULT", "VAULTSERIAL001")),
						true, true, iscsi.StatusGood, 0, false, false)
				default:
					continue
				}
			case iscsi.SCSIReadCapacity10:
				resp = iscsi.BuildDataIn(itt, iscsi.ReservedTag, 1, 1, 1, 0, 0,
					iscsi.BuildReadCapacity10(1023, 4096), true, true, iscsi.StatusGood, 0, false, false)
			default:
				continue
			}
			if err := iscsi.WritePDU(bc, resp); err != nil {
				return
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
		Cache:   CacheConfig{BlockSize: 4096, SectorSize: 512, L1Bytes: 4 << 20, FillWorkers: 1, FillQueueDepth: 16},
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

	sendCmd := func(itt uint32, cdb ...byte) {
		var h iscsi.BHS
		h[0] = iscsi.OpSCSICommand | 0x80 // F bit：命令在本 PDU 内结束
		h.SetITT(itt)
		copy(h[32:48], cdb)
		if err := iscsi.WritePDU(ic, &iscsi.PDU{Header: h}); err != nil {
			t.Fatalf("发送命令 0x%02x 失败：%v", cdb[0], err)
		}
	}
	sendCmd(1, iscsi.SCSIInquiry, 0x01, 0x80, 0x00, 0xff, 0x00)
	sendCmd(2, iscsi.SCSIInquiry, 0x01, 0x83, 0x00, 0xff, 0x00)
	sendCmd(3, iscsi.SCSIReadCapacity10, 0, 0, 0, 0, 0, 0, 0, 0, 0)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := p.Stats("k"); ok && st.BlockSize == 4096 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	st, _ := p.Stats("k")
	t.Fatalf("身份嗅探成功后缓存仍未绑定：BlockSize=%d Cache=%+v", st.BlockSize, st.Cache)
}

// TestRewriteTI locks the target→initiator sequence rewrite: every target PDU
// gains the injected answers on StatSN/ExpCmdSN/MaxCmdSN, and nothing else in
// the header moves.
//
// Data-In 不带 S 位时 StatSN 是保留字段（后端填 0），既不改写也不采集：加 offset
// 只会往保留字段里写一个假序号，而把它当序号读回来会让下一次命中代答的 StatSN 倒退
// （见 captureTargetSeq 与 TestInjectedHitKeepsStatSNForward）。
func TestRewriteTI(t *testing.T) {
	const offset = 3
	cases := []struct {
		name     string
		op       uint8
		flags    uint8
		wantAdd  bool // ExpCmdSN/MaxCmdSN 是否加 offset
		wantStat bool // StatSN 是否加 offset
	}{
		{"NOP-In", iscsi.OpNOPIn, 0, true, true},
		{"SCSIResponse", iscsi.OpSCSIResponse, 0, true, true},
		{"DataIn 带 S 位", iscsi.OpDataIn, iscsi.DataInFlagStatus, true, true},
		{"DataIn 不带 S 位时 StatSN 是保留字段", iscsi.OpDataIn, 0, true, false},
		{"R2T", iscsi.OpR2T, 0, true, true},
		{"AsyncMsg", iscsi.OpAsyncMsg, 0, true, true},
		{"Reject", iscsi.OpReject, 0, true, true},
		{"SCSICommand 是发起端方向，不该被 T→I 改写", iscsi.OpSCSICommand, 0, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var h iscsi.BHS
			h.SetOpcode(c.op)
			h.SetFlags(c.flags)
			h.SetStatSN(100)
			h.SetExpCmdSN(200)
			h.SetMaxCmdSN(220)
			// 目标方向的 ExpStatSN 在字节 36（请求方向的 accessor 落在 28，
			// 会盖掉 ExpCmdSN），这里直接落字节以校验它不被改写。
			binary.BigEndian.PutUint32(h[36:40], 300)
			rewriteTI(&h, offset)
			want := func(base uint32, add bool) uint32 {
				if add {
					return base + offset
				}
				return base
			}
			if h.StatSN() != want(100, c.wantStat) ||
				h.ExpCmdSN() != want(200, c.wantAdd) || h.MaxCmdSN() != want(220, c.wantAdd) {
				t.Fatalf("StatSN/ExpCmdSN/MaxCmdSN = %d/%d/%d，期望 %d/%d/%d",
					h.StatSN(), h.ExpCmdSN(), h.MaxCmdSN(),
					want(100, c.wantStat), want(200, c.wantAdd), want(220, c.wantAdd))
			}
			if got := binary.BigEndian.Uint32(h[36:40]); got != 300 {
				t.Fatalf("目标方向的 ExpStatSN 被误改：%d", got)
			}
		})
	}
}

// TestRewriteIT locks the initiator→target rewrite: ExpStatSN always loses the
// injected count; a non-immediate command that consumes a CmdSN loses it too;
// login and SNACK are left alone.
func TestRewriteIT(t *testing.T) {
	const offset = 2
	cases := []struct {
		name        string
		op          uint8
		immediate   bool
		wantCmdSN   uint32 // expected CmdSN after rewrite
		wantSkipAll bool   // header must be untouched
	}{
		{"SCSICommand 非立即", iscsi.OpSCSICommand, false, 50 - offset, false},
		{"SCSICommand 立即（不占 CmdSN）", iscsi.OpSCSICommand, true, 50, false},
		{"NOP-Out 非立即", iscsi.OpNOPOut, false, 50 - offset, false},
		{"TaskMgmtReq", iscsi.OpTaskMgmtReq, false, 50 - offset, false},
		{"TextReq", iscsi.OpTextReq, false, 50 - offset, false},
		{"LogoutReq", iscsi.OpLogoutReq, false, 50 - offset, false},
		{"DataOut 只改 ExpStatSN", iscsi.OpDataOut, false, 50, false},
		{"LoginReq 原样", iscsi.OpLoginReq, false, 50, true},
		{"SNACKReq 原样", iscsi.OpSNACKReq, false, 50, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var h iscsi.BHS
			h.SetOpcode(c.op)
			if c.immediate {
				h.SetImmediate(true)
			}
			h.SetCmdSN(50)
			h.SetExpStatSN(80)
			rewriteIT(&h, offset)
			if h.CmdSN() != c.wantCmdSN {
				t.Fatalf("CmdSN = %d，期望 %d", h.CmdSN(), c.wantCmdSN)
			}
			wantExp := uint32(80 - offset)
			if c.wantSkipAll {
				wantExp = 80
			}
			if h.ExpStatSN() != wantExp {
				t.Fatalf("ExpStatSN = %d，期望 %d", h.ExpStatSN(), wantExp)
			}
		})
	}
}

// TestRewriteZeroOffset 零偏移必须是空操作（登录全程与缓存未生效时都靠它）。
func TestRewriteZeroOffset(t *testing.T) {
	var h iscsi.BHS
	h.SetOpcode(iscsi.OpDataIn)
	h.SetStatSN(7)
	h.SetExpCmdSN(8)
	h.SetMaxCmdSN(9)
	binary.BigEndian.PutUint32(h[36:40], 10)
	rewriteTI(&h, 0)
	rewriteIT(&h, 0)
	if h.StatSN() != 7 || h.ExpCmdSN() != 8 || h.MaxCmdSN() != 9 || binary.BigEndian.Uint32(h[36:40]) != 10 {
		t.Fatalf("零偏移不应改动任何序号：%+v", h)
	}
}

// TestFrameRoundTrip 验证帧读写在不带 AHS/digest 时与协议编码逐字节一致。
func TestFrameRoundTrip(t *testing.T) {
	pdu := iscsi.BuildDataIn(0x1111, iscsi.ReservedTag, 5, 6, 7, 0, 0, []byte("hello"), true, true, iscsi.StatusGood, 0, false, false)
	want := pdu.Bytes()

	var f frame
	f.hdr = pdu.Header
	f.body = pdu.Data
	f.padLen = (4 - len(pdu.Data)%4) % 4

	var buf bytes.Buffer
	if err := writeFrame(&buf, &f); err != nil {
		t.Fatalf("writeFrame 失败：%v", err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("帧编码与协议不一致:\n got %x\nwant %x", buf.Bytes(), want)
	}

	fr := newFrameReader(bytes.NewReader(want), &digestFlags{})
	got, err := fr.read()
	if err != nil {
		t.Fatalf("read 失败：%v", err)
	}
	if !bytes.Equal(got.hdr[:], pdu.Header[:]) || !bytes.Equal(got.data(), pdu.Data) {
		t.Fatalf("读回的帧与原 PDU 不一致：hdr=%x data=%q", got.hdr[:], got.data())
	}
	if got.ahsLen != 0 || got.hasHD || got.hasDD {
		t.Fatalf("意外的 AHS/digest 标记：ahs=%d hd=%v dd=%v", got.ahsLen, got.hasHD, got.hasDD)
	}
}

// TestFrameReaderDigestAndAHS 锁定字段顺序：BHS → AHS → header digest → data →
// padding → data digest。顺序错了会把 digest 当数据，framing 直接错位。
func TestFrameReaderDigestAndAHS(t *testing.T) {
	ahs := []byte{0xde, 0xad, 0xbe, 0xef, 0x01, 0x02, 0x03, 0x04} // 8 bytes = 2 words
	data := []byte("abcdefg")                                     // 7 bytes → pad 1
	hd := [4]byte{0xa1, 0xa2, 0xa3, 0xa4}
	dd := [4]byte{0xb1, 0xb2, 0xb3, 0xb4}

	var f frame
	f.hdr.SetOpcode(iscsi.OpDataIn)
	f.hdr.SetDataSegmentLength(len(data))
	f.hdr[4] = uint8(len(ahs) / 4)
	f.ahsLen = len(ahs)
	f.body = append(append([]byte{}, ahs...), data...)
	f.hasHD = true
	f.hd = hd
	f.hasDD = true
	f.dd = dd
	f.padLen = (4 - len(data)%4) % 4

	var buf bytes.Buffer
	if err := writeFrame(&buf, &f); err != nil {
		t.Fatalf("writeFrame 失败：%v", err)
	}
	// 手工拼出期望的字节序，逐段落位校验。
	var want []byte
	want = append(want, f.hdr[:]...)
	want = append(want, ahs...)
	want = append(want, hd[:]...)
	want = append(want, data...)
	want = append(want, 0) // 对齐 padding
	want = append(want, dd[:]...)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("带 digest/AHS 的帧编码顺序不对:\n got %x\nwant %x", buf.Bytes(), want)
	}

	digests := &digestFlags{}
	digests.header.Store(true)
	digests.data.Store(true)
	got, err := newFrameReader(bytes.NewReader(want), digests).read()
	if err != nil {
		t.Fatalf("read 失败：%v", err)
	}
	if !bytes.Equal(got.data(), data) {
		t.Fatalf("data = %q，期望 %q", got.data(), data)
	}
	if got.ahsLen != len(ahs) || !got.hasHD || !got.hasDD || got.hd != hd || got.dd != dd {
		t.Fatalf("digest/AHS 解析错误：%+v", got)
	}
}

// TestParseCapacity 覆盖 READ CAPACITY(10/16) 两种响应长度。
func TestParseCapacity(t *testing.T) {
	// RC(10)：last LBA(4) + block size(4)。last=0xff → 256 blocks。
	rc10 := []byte{0x00, 0x00, 0x00, 0xff, 0x00, 0x00, 0x02, 0x00}
	if bs, bc, ok := parseCapacity(rc10); !ok || bs != 512 || bc != 256 {
		t.Fatalf("RC(10) = (%d,%d,%v)，期望 (512,256,true)", bs, bc, ok)
	}
	// RC(16)：last LBA(8) + block size(4) + 保留，取前 12 字节。last=9 → 10 blocks。
	rc16 := make([]byte, 32)
	rc16[7] = 9
	rc16[8], rc16[9], rc16[10], rc16[11] = 0x00, 0x00, 0x10, 0x00
	if bs, bc, ok := parseCapacity(rc16); !ok || bs != 4096 || bc != 10 {
		t.Fatalf("RC(16) = (%d,%d,%v)，期望 (4096,10,true)", bs, bc, ok)
	}
	// block size 为 0 视为不可用。
	if _, _, ok := parseCapacity(make([]byte, 8)); ok {
		t.Fatal("block size 为 0 应判为无效")
	}
	if _, _, ok := parseCapacity([]byte{1, 2, 3}); ok {
		t.Fatal("长度不足应判为无效")
	}
}

// TestScanDetector 锁定顺序扫描探测：连续读超过阈值才判为扫描。
func TestScanDetector(t *testing.T) {
	const bs = 64 << 10
	d := newScanDetector(true, 3)
	if d.scan(0, bs) || d.scan(bs, bs) {
		t.Fatal("扫描阈值之前不应判为扫描")
	}
	if !d.scan(2*bs, bs) {
		t.Fatal("连续第 3 次读应判为扫描")
	}
	// 跳读打断连续：run 归零。
	if d.scan(100*bs, bs) {
		t.Fatal("非连续读不应判为扫描")
	}
	// 关闭时永远不判扫描。
	off := newScanDetector(false, 1)
	if off.scan(0, bs) || off.scan(bs, bs) {
		t.Fatal("未启用时不应判为扫描")
	}
}

// TestHexIdentity 保证 VPD 0x83 载荷转成稳定的十六进制标识。
func TestHexIdentity(t *testing.T) {
	if got := hexIdentity([]byte{0x0a, 0xff}); got != "0aff" {
		t.Fatalf("hexIdentity = %q，期望 0aff", got)
	}
	if got := hexIdentity(nil); got != "" {
		t.Fatalf("空载荷应为空串，实际 %q", got)
	}
}

// TestApplyLoginDegradesOnNegotiatedValuesOnly 锁定降级只看目标**协商后**的值，
// 不看发起端的报价。真实教训：MS iSCSI 发起端每个 LoginReq 都报
// MaxConnections=32 / ErrorRecoveryLevel=2，目标才回落到 1 / 0；若按报价降级，
// 每个正常会话都会被误判为多连接而退化为纯透传，缓存永远不生效。
func TestApplyLoginDegradesOnNegotiatedValuesOnly(t *testing.T) {
	newSess := func() *session { return &session{pending: map[uint32]*pending{}} }

	// 发起端报价（MS 发起端真实报价），不应触发任何降级。
	offer := map[string]string{
		"maxconnections":     "32",
		"errorrecoverylevel": "2",
		"headerdigest":       "None,CRC32C",
		"datadigest":         "None,CRC32C",
		"sessiontype":        "Normal",
	}
	s := newSess()
	s.applyLogin(offer, false)
	if s.isDegraded() {
		t.Fatalf("发起端报价不应触发降级，实际 reason=%q", s.degradeReason)
	}
	// 目标协商回落到 1 / 0 → 仍不应降级（缓存必须生效）。
	resp := map[string]string{
		"maxconnections":     "1",
		"errorrecoverylevel": "0",
		"sessiontype":        "Normal",
	}
	s.applyLogin(resp, true)
	if s.isDegraded() {
		t.Fatalf("目标协商为 1/0 时不应降级，实际 reason=%q", s.degradeReason)
	}

	// 目标真的协商出多连接 / ERL>0 → 必须降级。
	s2 := newSess()
	s2.applyLogin(map[string]string{"maxconnections": "2"}, true)
	if !s2.isDegraded() {
		t.Fatal("MaxConnections 协商为 2 时必须降级")
	}
	s3 := newSess()
	s3.applyLogin(map[string]string{"errorrecoverylevel": "1"}, true)
	if !s3.isDegraded() {
		t.Fatal("ErrorRecoveryLevel 协商为 1 时必须降级")
	}
	// 响应方向协商出 digest != None → 降级并置位 digest 解析标志。
	s4 := newSess()
	s4.applyLogin(map[string]string{"headerdigest": "CRC32C"}, true)
	if !s4.isDegraded() || !s4.digests.header.Load() {
		t.Fatal("HeaderDigest 协商为 CRC32C 时必须降级并启用 digest 解析")
	}
	// discovery 会话两个方向都要降级。
	s5 := newSess()
	s5.applyLogin(map[string]string{"sessiontype": "Discovery"}, false)
	if !s5.isDegraded() {
		t.Fatal("discovery 会话必须降级")
	}
}

// TestReadBufPoolRoundTrip 锁定缓冲池的尺寸分类与归还语义：取出的缓冲区
// len 等于请求值、cap 等于其尺寸类，归还后同尺寸类的请求必须复用它。
func TestReadBufPoolRoundTrip(t *testing.T) {
	for _, n := range []int{1, 4096, 64 << 10, 200 << 10, 1 << 20, maxTeeBytes} {
		b := readBufGet(n)
		if len(b) != n {
			t.Fatalf("readBufGet(%d) 长度 = %d", n, len(b))
		}
		cl := readBufClass(n)
		if cap(b) != readBufClassSizes[cl] {
			t.Fatalf("readBufGet(%d) 容量 %d 不属于尺寸类 %d", n, cap(b), readBufClassSizes[cl])
		}
		readBufPut(b)
		again := readBufGet(n)
		if cap(again) != cap(b) {
			t.Fatalf("归还后同尺寸类应复用：cap %d -> %d", cap(b), cap(again))
		}
		readBufPut(again)
	}
}

// TestInjectBufPoolRoundTrip 锁定注入缓冲池的尺寸分类与归还语义：取出的缓冲
// cap 等于其尺寸类、归还后同尺寸类的请求必须复用它 —— 一次 4KiB 命中不该占用
// 为最大读准备的缓冲。
func TestInjectBufPoolRoundTrip(t *testing.T) {
	for _, n := range []int{0, 48, 4096, 64 << 10, 200 << 10, 1 << 20, maxTeeBytes} {
		b := injectBufGet(n)
		if len(b) != n {
			t.Fatalf("injectBufGet(%d) 长度 = %d", n, len(b))
		}
		cl := injectBufClass(n)
		if cap(b) != injectBufClasses[cl] {
			t.Fatalf("injectBufGet(%d) 容量 %d 不属于尺寸类 %d", n, cap(b), injectBufClasses[cl])
		}
		injectBufPut(b)
		again := injectBufGet(n)
		if cap(again) != cap(b) {
			t.Fatalf("归还后同尺寸类应复用：cap %d -> %d", cap(b), cap(again))
		}
		injectBufPut(again)
	}
}

// TestInjectBufNeedFitsTheBound 锁定容量上界的推算前提：只要 MRDSL 不低于
// RFC 7143 的下限 512，任何一次注入的组装长度都不会超过 injectBufCap，分档后
// 的最大尺寸类也就装得下，组装循环不会越界写。
func TestInjectBufNeedFitsTheBound(t *testing.T) {
	for _, mrdsl := range []int{512, 4096, 64 << 10, 256 << 10, 1 << 20} {
		for _, n := range []int{0, 512, 4096, 64 << 10, 1 << 20, maxTeeBytes} {
			need := injectBufNeed(n, mrdsl)
			if need > injectBufCap {
				t.Fatalf("MRDSL %d 下 %d 字节的组装需要 %d，超过上界 %d", mrdsl, n, need, injectBufCap)
			}
			if need < n {
				t.Fatalf("MRDSL %d 下 %d 字节的组装长度算成 %d", mrdsl, n, need)
			}
		}
	}
	if last := injectBufClasses[len(injectBufClasses)-1]; last < injectBufCap {
		t.Fatalf("最大尺寸类 %d 小于上界 %d", last, injectBufCap)
	}
}

// batchTestWriter 记录 Write 调用次数，用来验证攒批确实把多次写并成了一次。
type batchTestWriter struct {
	buf    bytes.Buffer
	writes int
}

func (w *batchTestWriter) Write(p []byte) (int, error) {
	w.writes++
	return w.buf.Write(p)
}

// batchTestFrame 造一帧 Data-In。payload 不同则字节内容不同，于是逐字节比较
// 既能证明内容一致，也能证明顺序没乱。
func batchTestFrame(payload int, withAHS, withHD, withDD bool) *frame {
	data := make([]byte, payload)
	for i := range data {
		data[i] = byte(i*7 + payload)
	}
	var f frame
	f.hdr.SetOpcode(iscsi.OpDataIn)
	f.hdr.SetDataSegmentLength(payload)
	if withAHS {
		ahs := []byte{0xde, 0xad, 0xbe, 0xef}
		f.ahsLen = len(ahs)
		f.hdr[4] = uint8(len(ahs) / 4)
		f.body = append(append([]byte{}, ahs...), data...)
	} else {
		f.body = data
	}
	if withHD {
		f.hasHD = true
		f.hd = [4]byte{0xa1, 0xa2, 0xa3, 0xa4}
	}
	if withDD {
		f.hasDD = true
		f.dd = [4]byte{0xb1, 0xb2, 0xb3, 0xb4}
	}
	f.padLen = (4 - payload%4) % 4
	for i := 0; i < f.padLen; i++ {
		f.pad[i] = 0xcc
	}
	return &f
}

// TestFrameBatchMatchesWriteFrame 是攒批的正确性底线：一帧无论走批还是直发，
// 写出的字节必须与逐帧写完全一致 —— 攒批只该改变系统调用的次数。
func TestFrameBatchMatchesWriteFrame(t *testing.T) {
	cases := []struct {
		name                    string
		payload                 int
		withAHS, withHD, withDD bool
	}{
		{"empty", 0, false, false, false},
		{"small-pad", 7, false, false, false},
		{"4k", 4096, false, false, false},
		{"over-copy-limit", frameCopyLimit + 512, false, false, false},
		{"at-batch-cap", frameBatchCap - 48, false, false, false},
		{"over-batch-cap", frameBatchCap + 4096, false, false, false},
		{"ahs", 100, true, false, false},
		{"digests", 100, true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := batchTestFrame(tc.payload, tc.withAHS, tc.withHD, tc.withDD)

			var want batchTestWriter
			if err := writeFrame(&want, f); err != nil {
				t.Fatalf("writeFrame 失败：%v", err)
			}
			// 这里不校验 Write 次数：writeFrame 的"一次系统调用"来自
			// net.Buffers.WriteTo 命中 TCP 连接的 writeBuffers 接口，而
			// bytes.Buffer 不在其列，会退化成逐 slice 写。攒批的 Write 次数
			// 由 frameBatch.flush 保证，与目标类型无关（见下一个测试）。

			var got batchTestWriter
			b := newFrameBatch(&got)
			if err := b.append(f); err != nil {
				t.Fatalf("append 失败：%v", err)
			}
			if err := b.flush(); err != nil {
				t.Fatalf("flush 失败：%v", err)
			}
			if !bytes.Equal(got.buf.Bytes(), want.buf.Bytes()) {
				t.Fatalf("攒批写出的字节与逐帧写不一致:\n got %x\nwant %x", got.buf.Bytes(), want.buf.Bytes())
			}
		})
	}
}

// TestFrameBatchCoalescesWrites 验证收益本身：同一窗口里的多个小帧并成一次 Write，
// 且字节顺序不变。
func TestFrameBatchCoalescesWrites(t *testing.T) {
	const frames = 8

	var want batchTestWriter
	for i := 0; i < frames; i++ {
		if err := writeFrame(&want, batchTestFrame(1024+i*512, false, false, false)); err != nil {
			t.Fatalf("writeFrame 失败：%v", err)
		}
	}

	var got batchTestWriter
	b := newFrameBatch(&got)
	for i := 0; i < frames; i++ {
		if err := b.append(batchTestFrame(1024+i*512, false, false, false)); err != nil {
			t.Fatalf("append 失败：%v", err)
		}
	}
	if err := b.flush(); err != nil {
		t.Fatalf("flush 失败：%v", err)
	}

	// bytes.Buffer 不实现 net 的 writeBuffers，所以 writeFrame 在这里退化成
	// "一帧两个 slice 两次 Write"；真实连接是 *net.TCPConn，同一帧只发一次
	// WSASend。无论哪种目标类型，攒批都把那 2×frames 次写并成了 1 次。
	if want.writes != frames*2 {
		t.Fatalf("逐帧写应触发 %d 次 Write，实际 %d 次", frames*2, want.writes)
	}
	if got.writes != 1 {
		t.Fatalf("%d 个小帧应合并为 1 次 Write，实际 %d 次", frames, got.writes)
	}
	if !bytes.Equal(got.buf.Bytes(), want.buf.Bytes()) {
		t.Fatalf("攒批后的字节流与逐帧写不一致:\n got %x\nwant %x", got.buf.Bytes(), want.buf.Bytes())
	}
}

// countingConn 只把 Write 转给计数器：被测路径在测试里只写不读，其余 net.Conn
// 方法碰不到，一旦真的碰到就 panic，不会静默放过。
type countingConn struct {
	net.Conn
	w *batchTestWriter
}

func (c *countingConn) Write(p []byte) (int, error) { return c.w.Write(p) }

// TestInjectedHitQueuesBehindForwardedFrames 钉住 initiator 方向"唯一写出口"这条
// 不变量：本地代答必须排在**已经攒下、但还没写出**的透传帧之后。
//
// 真机上这就是 StatSN 倒退导致 RST 断链的那一类故障：代答若直接写连接，就会插到
// 攒批的前面，序号先大后小。同时代答必须立刻把自己的响应送出去 —— 它回答的是
// 一条正在等待的命令，不能等下一次攒批。
func TestInjectedHitQueuesBehindForwardedFrames(t *testing.T) {
	w := &batchTestWriter{}
	it := &countingConn{w: w}
	s := newSession(&target{}, it, it)

	// T→I 泵攒下一帧后端响应，但读者手里还有数据，flush 被推迟 —— 这正是批里
	// 压着未写出字节的那个窗口。
	// 零载荷：一个 48 字节的头，第二个 PDU 的头紧跟其后，无需跳过数据段。
	forwarded := batchTestFrame(0, false, false, false)
	forwarded.hdr.SetOpcode(iscsi.OpSCSIResponse)
	forwarded.hdr.SetStatSN(41)
	if err := s.tiBatch.append(forwarded); err != nil {
		t.Fatalf("append 失败：%v", err)
	}

	// 紧接着一条可以在本地代答的命中读。
	var cmd iscsi.BHS
	cmd.SetITT(7)
	cmd.SetCmdSN(9)
	s.lastStat = 41
	if err := s.injectLocked(&cmd, []byte("cached-hit")); err != nil {
		t.Fatalf("注入失败：%v", err)
	}

	if w.writes != 1 {
		t.Fatalf("透传帧与注入应在一次 Write 里按序写出，实际写了 %d 次", w.writes)
	}
	r := bytes.NewReader(w.buf.Bytes())
	first, err := iscsi.ReadBHS(r)
	if err != nil {
		t.Fatalf("读第一个 PDU 头失败：%v", err)
	}
	if first.Opcode() != iscsi.OpSCSIResponse || first.StatSN() != 41 {
		t.Fatalf("第一个 PDU 应是排在注入之前的透传响应，实际 opcode=%#x statSN=%d",
			first.Opcode(), first.StatSN())
	}
	second, err := iscsi.ReadBHS(r)
	if err != nil {
		t.Fatalf("读第二个 PDU 头失败：%v", err)
	}
	if second.Opcode() != iscsi.OpDataIn {
		t.Fatalf("第二个 PDU 应是注入的 Data-In，实际 opcode=%#x", second.Opcode())
	}
	if got, want := second.StatSN(), uint32(42); got != want {
		t.Fatalf("注入的 StatSN 应接着透传序号走，实际 %d，期望 %d", got, want)
	}
}
