package iscsiproxy

import (
	"bufio"
	"io"
	"net"
	"sync/atomic"

	"vault/internal/iscsicache/iscsi"
)

// digestFlags are the digests negotiated by a session's login, shared by both
// pumps' frame readers. They are only ever flipped from off to on, right after
// the login response that negotiates them is seen, so a plain atomic load is
// enough.
type digestFlags struct {
	header atomic.Bool
	data   atomic.Bool
}

// frameReader splits a byte stream into iSCSI PDUs.
//
// It owns the buffers a frame points at, so a frame is only valid until the next
// read. Both forwarding pumps process a frame inline (copying out anything that
// must outlive it), which keeps the transparent hot path allocation free.
//
// The reader is digest aware. A PDU with a negotiated HeaderDigest carries four
// extra bytes between the AHS and the data segment, and one with a DataDigest
// carries four more after the data segment's padding. The flags are read after
// the BHS arrives, never before: the peer can only send a digest protected PDU
// after it has received the response that negotiated the digest, so by the time
// those bytes are in hand the flags are already visible.
type frameReader struct {
	r       *bufio.Reader
	f       frame
	buf     []byte
	digests *digestFlags
}

// frame is one iSCSI PDU: the Basic Header Segment, an optional AHS, the
// optional header digest, the data segment, its alignment padding and the
// optional data digest.
type frame struct {
	hdr    iscsi.BHS
	body   []byte // AHS bytes followed by the data segment, padding excluded
	ahsLen int    // leading AHS bytes inside body

	hd     [4]byte // header digest, when present
	hasHD  bool
	pad    [3]byte
	padLen int
	dd     [4]byte // data digest, when present
	hasDD  bool
}

// frameReadBuffer is the read-ahead window of a frameReader. PDU headers are
// read one syscall per frame without it; batching them into a buffered reader
// cuts the hot path to one syscall per window. Large data segments bypass the
// buffer (bufio reads them straight into the destination), so big transfers do
// not pay an extra copy.
const frameReadBuffer = 64 << 10

func newFrameReader(r io.Reader, digests *digestFlags) *frameReader {
	return &frameReader{r: bufio.NewReaderSize(r, frameReadBuffer), digests: digests}
}

// splice copies the rest of the stream to w verbatim. It is used once a session
// degrades to a pure pass-through: the buffered reader is drained first, so no
// bytes are lost, and from then on the session pays no per-PDU parsing at all.
func (fr *frameReader) splice(w io.Writer) {
	_, _ = io.Copy(w, fr.r)
}

// data returns the PDU's data segment (padding, AHS and digests excluded).
func (f *frame) data() []byte { return f.body[f.ahsLen:] }

// read reads exactly one PDU. The returned frame aliases the reader's buffers.
func (fr *frameReader) read() (*frame, error) {
	h, err := fr.readHeader()
	if err != nil {
		return nil, err
	}
	if err := fr.readBody(h); err != nil {
		return nil, err
	}
	return h, nil
}

// readHeader reads the BHS, any AHS and the header digest, stopping before the
// data segment. The frame's body holds only the AHS bytes (nil when none). The
// caller must then consume the data segment with readBody (buffered) or
// streamBody (cut-through); both also consume the trailing padding and digest.
func (fr *frameReader) readHeader() (*frame, error) {
	h := &fr.f
	h.hasHD, h.hasDD = false, false
	h.padLen = 0
	h.body = nil
	if _, err := io.ReadFull(fr.r, h.hdr[:]); err != nil {
		return nil, err
	}
	h.ahsLen = h.hdr.AHSLength() * 4
	// Field order per RFC 7143: BHS, AHS, header digest, data, padding, data digest.
	if h.ahsLen > 0 {
		if cap(fr.buf) < h.ahsLen {
			fr.buf = make([]byte, h.ahsLen)
		}
		h.body = fr.buf[:h.ahsLen]
		if _, err := io.ReadFull(fr.r, h.body); err != nil {
			return nil, err
		}
	}
	h.hasHD = fr.digests != nil && fr.digests.header.Load()
	if h.hasHD {
		if _, err := io.ReadFull(fr.r, h.hd[:]); err != nil {
			return nil, err
		}
	}
	return h, nil
}

// readBody reads the data segment of the PDU from readHeader into the frame body
// (after any AHS bytes), then consumes the padding and data digest.
func (fr *frameReader) readBody(h *frame) error {
	dataLen := h.hdr.DataSegmentLength()
	total := h.ahsLen + dataLen
	if cap(fr.buf) < total {
		fr.buf = make([]byte, total)
	}
	if h.ahsLen > 0 {
		copy(fr.buf[:h.ahsLen], h.body)
	}
	h.body = fr.buf[:total]
	if dataLen > 0 {
		if _, err := io.ReadFull(fr.r, h.body[h.ahsLen:]); err != nil {
			return err
		}
	}
	return fr.readTail(h, dataLen)
}

// readTail consumes the alignment padding and data digest that follow a data
// segment of dataLen bytes.
func (fr *frameReader) readTail(h *frame, dataLen int) error {
	h.padLen = (4 - dataLen%4) % 4
	if h.padLen > 0 {
		if _, err := io.ReadFull(fr.r, h.pad[:h.padLen]); err != nil {
			return err
		}
	}
	h.hasDD = fr.digests != nil && fr.digests.data.Load()
	if h.hasDD {
		if _, err := io.ReadFull(fr.r, h.dd[:]); err != nil {
			return err
		}
	}
	return nil
}

// writeFrame writes a PDU, preserving the field order of RFC 7143 (BHS, AHS,
// header digest, data, padding, data digest).
//
// It always hands the whole PDU to net.Buffers, which is **one** send syscall no
// matter how many slices the frame is made of: internal/poll.(*FD).Writev packs
// every slice into a single WSABuf array and issues one WSASend on Windows
// (writev on Unix). An earlier version instead flattened small frames through a
// memcpy into a per-pump scratch buffer and wrote bulk frames with two separate
// Writes, on the theory that net.Buffers had no scatter-gather on Windows; both
// only added work. Batching several frames into a single Write is a different
// trade and still a win — that is what frameBatch does.
func writeFrame(w io.Writer, f *frame) error {
	// 纯头帧（响应、命令、无载荷的 Data-In）占 4K 随机负载的一半，直接写省掉
	// net.Buffers 的接口断言与 WSABuf 构造，依然是一次 Write。
	if !f.hasHD && !f.hasDD && f.ahsLen == 0 && f.padLen == 0 && len(f.body) == 0 {
		_, err := w.Write(f.hdr[:])
		return err
	}
	var segs [6][]byte
	bufs := net.Buffers(segs[:0])
	bufs = append(bufs, f.hdr[:])
	if f.ahsLen > 0 {
		bufs = append(bufs, f.body[:f.ahsLen])
	}
	if f.hasHD {
		bufs = append(bufs, f.hd[:])
	}
	if data := f.data(); len(data) > 0 {
		bufs = append(bufs, data)
	}
	if f.padLen > 0 {
		bufs = append(bufs, f.pad[:f.padLen])
	}
	if f.hasDD {
		bufs = append(bufs, f.dd[:])
	}
	_, err := bufs.WriteTo(w)
	return err
}

const (
	// frameBatchCap 是一个方向上的写批大小。取一个读窗口的大小：读满一窗
	// （frameReadBuffer 字节，通常已含多个完整 PDU）正好一次 Write 送走。
	frameBatchCap = frameReadBuffer
	// frameCopyLimit 是"空批时愿意为合并而拷贝"的帧大小上限。比它大的帧在批
	// 为空时直接写（writeFrame 本身就是一次 gather 写，不拷也只是一次 syscall），
	// 拷贝它只有在下一帧恰好已在读缓冲里时才回本，不值得赌。
	frameCopyLimit = 16 << 10
)

// frameBatch 把同一方向上连续到达的若干帧攒进一个缓冲，凑成一次 Write。
//
// 为什么要攒：每个 PDU 至少一次发送系统调用，而系统调用就是代理的每 PDU
// 大头 —— 写路径的 CPU profile 里 61% 的样本落在 WSASend 上。QD≥2 时读者
// 的窗口里本来就躺着好几个完整 PDU，把它们拼进一个缓冲再一次写出，可把 N
// 次系统调用降成 1 次。
//
// 延迟不会增加：只有读者手里**已经**没有下一个字节时才 flush（见两个泵），
// 绝不为了凑批去等还没到的数据。攒批期间帧内容一个字节都不变，只是发送时机
// 推迟到本批末尾。
type frameBatch struct {
	w   io.Writer
	out []byte
	n   int
}

func newFrameBatch(w io.Writer) *frameBatch {
	return &frameBatch{w: w, out: make([]byte, frameBatchCap)}
}

// append 把一帧并入批。放不下（或帧太大不宜拷）时先 flush。
//
// 帧指向读者的缓冲，下一帧就会覆盖它，所以这里必须把字节拷走；不拷贝的大帧
// 只有"批为空且帧本身不小"这一种情况，那时直接写出去同样是零拷贝一次系统调用。
func (b *frameBatch) append(f *frame) error {
	if f.hasHD || f.hasDD {
		// 协商到摘要的会话本来就降级为纯透传，收不到这里；真收到也不值得为它
		// 破例：flush 之后按原样写一帧。
		if err := b.flush(); err != nil {
			return err
		}
		return writeFrame(b.w, f)
	}
	total := 48 + len(f.body) + f.padLen
	if total > len(b.out) {
		// 比整个批还大的帧（透传帧的数据段不受注入上限约束）。
		if err := b.flush(); err != nil {
			return err
		}
		return writeFrame(b.w, f)
	}
	if b.n == 0 && total > frameCopyLimit {
		return writeFrame(b.w, f)
	}
	if b.n+total > len(b.out) {
		if err := b.flush(); err != nil {
			return err
		}
	}
	dst := b.out[b.n : b.n+total]
	copy(dst, f.hdr[:])
	copy(dst[48:], f.body)
	copy(dst[48+len(f.body):], f.pad[:f.padLen])
	b.n += total
	return nil
}

// flush 把已攒的字节一次写出。n 先清零再写：写失败时调用方都会终止会话，而
// 重试一批已经部分写出的字节会重复发送。
func (b *frameBatch) flush() error {
	if b.n == 0 {
		return nil
	}
	buf := b.out[:b.n]
	b.n = 0
	_, err := b.w.Write(buf)
	return err
}

// write 把一段已经装配好的连续字节（本地代答的一整串 Data-In）并入批，从而与
// 同方向攒下的透传帧保持先后次序 —— 注入直接写连接就会插到批的前面，StatSN 立刻
// 倒退。调用方负责在注入后 flush：注入的响应不能等下次攒批才发出去。
//
// 大块不拷贝：比整个批还大的注入先 flush 掉已攒的帧，再零拷贝写出去。
func (b *frameBatch) write(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	if len(p) > len(b.out) {
		if err := b.flush(); err != nil {
			return err
		}
		_, err := b.w.Write(p)
		return err
	}
	if b.n+len(p) > len(b.out) {
		if err := b.flush(); err != nil {
			return err
		}
	}
	b.n += copy(b.out[b.n:], p)
	return nil
}
