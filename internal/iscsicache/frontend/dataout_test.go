package frontend

import (
	"testing"

	"vault/internal/iscsicache/iscsi"
)

// dataOutHeader builds the header of a Data-Out PDU with the fields the
// placement decision reads.
func dataOutHeader(itt, ttt uint32, off uint32, dsl int) iscsi.BHS {
	var h iscsi.BHS
	h.SetOpcode(iscsi.OpDataOut)
	h.SetITT(itt)
	h.SetTTT(ttt)
	h.SetBufOffset(off)
	h.SetDataSegmentLength(dsl)
	return h
}

// TestDataOutWindowPlacesBurstInTheTransferBuffer covers the R2T path: a burst
// the pending transfer can hold is read straight into the buffer that holds the
// write payload, which is what saves the per-PDU allocation and copy. The
// window has to alias that buffer, not a copy of it.
func TestDataOutWindowPlacesBurstInTheTransferBuffer(t *testing.T) {
	buf := make([]byte, 4096)
	tx := &dataOut{buf: buf, base: 1024, want: 512}
	s := &session{pending: map[uint32]*dataOut{7: tx}, writes: map[uint32]*writeTransfer{}}

	const at = 1024 + 256
	h := dataOutHeader(0x42, 7, at, 128)
	got := s.dataOutWindow(&h)
	if got == nil {
		t.Fatal("a burst inside the R2T window must be placed in the transfer buffer")
	}
	if len(got) != 128 {
		t.Fatalf("window length = %d, want 128", len(got))
	}
	if &got[0] != &buf[at] {
		t.Fatal("the window does not alias the transfer buffer at the burst offset")
	}
	got[0] = 0xab
	if buf[at] != 0xab {
		t.Fatal("a write through the window did not land in the transfer buffer")
	}
}

// TestDataOutWindowFallsBackWhenItCannotPlace covers every PDU the header alone
// cannot place: it must return nil so readPDU reads a segment of its own and
// handleDataOut reports the burst exactly as it did before the optimisation.
// A placement miss is only ever a lost optimisation, never a lost burst.
func TestDataOutWindowFallsBackWhenItCannotPlace(t *testing.T) {
	buf := make([]byte, 4096)
	reg := func(s *session) *session {
		s.pending = map[uint32]*dataOut{7: {buf: buf, base: 1024, want: 512}}
		s.writes = map[uint32]*writeTransfer{9: {buf: buf, notify: make(chan struct{}, 1)}}
		return s
	}

	cases := []struct {
		name    string
		h       iscsi.BHS
		wantNil bool
	}{
		{name: "unknown transfer tag", h: dataOutHeader(0, 8, 1024, 128), wantNil: true},
		{name: "unknown task tag", h: dataOutHeader(0x11, iscsi.ReservedTag, 0, 128), wantNil: true},
		{name: "no data segment", h: dataOutHeader(0, 7, 1024, 0), wantNil: true},
		{name: "offset before the burst", h: dataOutHeader(0, 7, 512, 128), wantNil: true},
		{name: "burst past the window", h: dataOutHeader(0, 7, 1024+256, 512), wantNil: true},
		{name: "offset past the window", h: dataOutHeader(0, 7, 4096, 4), wantNil: true},
		{name: "unsolicited past the buffer", h: dataOutHeader(9, iscsi.ReservedTag, 4096, 4), wantNil: true},
		{name: "last byte of the window", h: dataOutHeader(0, 7, 1536-4, 4), wantNil: false},
		{name: "unsolicited last byte", h: dataOutHeader(9, iscsi.ReservedTag, 4096-4, 4), wantNil: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := reg(&session{})
			got := s.dataOutWindow(&tc.h)
			if tc.wantNil && got != nil {
				t.Fatalf("expected no placement, got a %d byte window", len(got))
			}
			if !tc.wantNil && got == nil {
				t.Fatal("expected a window inside the transfer buffer")
			}
			if got != nil && &got[0] != &buf[int(tc.h.BufOffset())] {
				t.Fatal("the window does not alias the transfer buffer")
			}
		})
	}
}

// TestFitsRejectsWrapAround pins the bound check the placement shares with
// handleDataOut: the offset and length come off the wire, so off+n must not be
// allowed to wrap into the window.
func TestFitsRejectsWrapAround(t *testing.T) {
	cases := []struct {
		off, n, size int
		want         bool
	}{
		{0, 0, 0, true},
		{0, 16, 16, true},
		{1, 16, 16, false},
		{-1, 4, 16, false},
		{0, -1, 16, false},
		{16, 0, 16, true},
		{1<<31 - 1, 1<<31 - 1, 16, false},
	}
	for _, tc := range cases {
		if got := fits(tc.off, tc.n, tc.size); got != tc.want {
			t.Fatalf("fits(%d, %d, %d) = %v, want %v", tc.off, tc.n, tc.size, got, tc.want)
		}
	}
}
