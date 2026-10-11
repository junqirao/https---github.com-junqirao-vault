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
	h := &fr.f
	h.hasHD, h.hasDD = false, false
	if _, err := io.ReadFull(fr.r, h.hdr[:]); err != nil {
		return nil, err
	}
	h.ahsLen = h.hdr.AHSLength() * 4
	dataLen := h.hdr.DataSegmentLength()
	n := h.ahsLen + dataLen
	if cap(fr.buf) < n {
		fr.buf = make([]byte, n)
	}
	h.body = fr.buf[:n]
	// Field order per RFC 7143: BHS, AHS, header digest, data, padding, data digest.
	if h.ahsLen > 0 {
		if _, err := io.ReadFull(fr.r, h.body[:h.ahsLen]); err != nil {
			return nil, err
		}
	}
	h.hasHD = fr.digests != nil && fr.digests.header.Load()
	if h.hasHD {
		if _, err := io.ReadFull(fr.r, h.hd[:]); err != nil {
			return nil, err
		}
	}
	if dataLen > 0 {
		if _, err := io.ReadFull(fr.r, h.body[h.ahsLen:]); err != nil {
			return nil, err
		}
	}
	h.padLen = (4 - dataLen%4) % 4
	if h.padLen > 0 {
		if _, err := io.ReadFull(fr.r, h.pad[:h.padLen]); err != nil {
			return nil, err
		}
	}
	h.hasDD = fr.digests != nil && fr.digests.data.Load()
	if h.hasDD {
		if _, err := io.ReadFull(fr.r, h.dd[:]); err != nil {
			return nil, err
		}
	}
	return h, nil
}

// writeCoalesce caps the frame size flattened into a single Write. Go's
// net.Buffers has no scatter-gather write on Windows, so a multi-slice frame
// costs one syscall per slice; below this size a memcpy into the pump's scratch
// buffer is cheaper than the extra syscalls. Larger frames (bulk data segments)
// take the two-Write path instead: a syscall is far cheaper than copying MiBs.
const writeCoalesce = 16 << 10

// writeFrame writes a PDU, preserving the field order of RFC 7143 (BHS, AHS,
// header digest, data, padding, data digest). scratch is a per-pump buffer used
// to flatten small digest-free frames into a single Write; nil is allowed.
func writeFrame(w io.Writer, f *frame, scratch []byte) error {
	data := f.data()
	if !f.hasHD && !f.hasDD {
		total := 48 + len(f.body) + f.padLen
		if total <= writeCoalesce && len(scratch) >= total {
			b := scratch[:total]
			copy(b, f.hdr[:])
			copy(b[48:], f.body)
			copy(b[48+len(f.body):], f.pad[:f.padLen])
			_, err := w.Write(b)
			return err
		}
		if f.ahsLen == 0 && f.padLen == 0 {
			if _, err := w.Write(f.hdr[:]); err != nil {
				return err
			}
			if len(data) > 0 {
				_, err := w.Write(data)
				return err
			}
			return nil
		}
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
	if len(data) > 0 {
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
