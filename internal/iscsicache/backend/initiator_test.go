package backend

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"

	"vault/internal/iscsicache/iscsi"
)

// TestHandleNOPInDoesNotConsumeCmdSN locks the fix for a session wedge that the
// backing Microsoft target punishes silently: a NOP-Out sent as the answer to a
// target NOP-In must not consume a CmdSN, because the target does not count it.
// Consuming one makes every later command carry a CmdSN the target is not
// waiting for, and it then holds those commands forever without an error.
func TestHandleNOPInDoesNotConsumeCmdSN(t *testing.T) {
	cli, srv := net.Pipe()
	defer cli.Close()
	defer srv.Close()

	it := &Initiator{
		conn:      cli,
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		cmdSN:     7,
		expStatSN: 9,
	}

	nopIn := iscsi.NewPDU(iscsi.OpNOPIn, []byte("ping"))
	nopIn.Header.SetITT(0xabcd)
	nopIn.Header.SetTTT(0x1234)

	errCh := make(chan error, 1)
	go func() { errCh <- it.handleNOPIn(context.Background(), &nopIn.Header, nopIn.Data) }()

	reply, err := iscsi.ReadPDU(srv)
	if err != nil {
		t.Fatalf("read NOP-Out reply: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("handleNOPIn: %v", err)
	}
	if reply.Header.Opcode() != iscsi.OpNOPOut {
		t.Fatalf("reply opcode = %s, want NOP-Out", iscsi.OpcodeName(reply.Header.Opcode()))
	}
	if reply.Header.ITT() != 0xabcd || reply.Header.TTT() != 0x1234 {
		t.Fatalf("NOP-Out must echo the NOP-In ITT/TTT: itt=%#x ttt=%#x",
			reply.Header.ITT(), reply.Header.TTT())
	}
	if it.cmdSN != 7 {
		t.Fatalf("handleNOPIn consumed a CmdSN: want 7, got %d", it.cmdSN)
	}
}

// TestAdoptSequenceTracksTarget checks that the initiator re-reads the sequence
// numbers the target advertises, so a drift corrects itself instead of wedging
// the session.
func TestAdoptSequenceTracksTarget(t *testing.T) {
	it := &Initiator{cmdSN: 5, expStatSN: 6}

	var h iscsi.BHS
	h.SetStatSN(11)
	h.SetExpCmdSN(12)
	h.SetMaxCmdSN(40)
	it.adoptSequence(&h)

	if it.expStatSN != 12 {
		t.Errorf("expStatSN = %d, want 12", it.expStatSN)
	}
	if it.cmdSN != 12 {
		t.Errorf("cmdSN = %d, want 12", it.cmdSN)
	}
	if it.maxCmdSN != 40 {
		t.Errorf("maxCmdSN = %d, want 40", it.maxCmdSN)
	}
}
