package backend

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
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

// TestParseCHAPChallenge locks the parsing of the target's security-stage
// challenge, including the "no challenge" case (a target that accepted None) and
// a malformed identifier.
func TestParseCHAPChallenge(t *testing.T) {
	text := iscsi.ParseText(iscsi.EncodeText(
		[2]string{"AuthMethod", "CHAP"},
		[2]string{"CHAP_A", "5"},
		[2]string{"CHAP_I", "42"},
		[2]string{"CHAP_C", "0x1234567890abcdef"},
	))
	c, ok, err := parseCHAPChallenge(text)
	if err != nil || !ok {
		t.Fatalf("parseCHAPChallenge: ok=%v err=%v, want true, nil", ok, err)
	}
	if c.algorithm != "5" || c.id != 42 || hex.EncodeToString(c.challenge) != "1234567890abcdef" {
		t.Fatalf("parsed challenge = %+v", c)
	}

	if _, ok, err := parseCHAPChallenge(map[string]string{"authmethod": "none"}); ok || err != nil {
		t.Fatalf("no challenge: ok=%v err=%v, want false, nil", ok, err)
	}
	if _, _, err := parseCHAPChallenge(map[string]string{"chap_i": "999"}); err == nil {
		t.Fatal("CHAP_I=999 should be rejected")
	}
}

// TestCHAPResponseKeys locks the digest formula. The expected value is computed
// independently as MD5(0x2a || "secret" || 0x1234567890abcdef): if the byte order
// (id, secret, challenge) ever changes, this fails instead of silently
// authenticating against a target that computes it differently.
func TestCHAPResponseKeys(t *testing.T) {
	it := &Initiator{cfg: Config{Username: "user", Secret: "secret"}}
	keys, err := it.chapResponseKeys(chapChallenge{
		algorithm: "5",
		id:        0x2a,
		challenge: mustHex(t, "1234567890abcdef"),
	})
	if err != nil {
		t.Fatalf("chapResponseKeys: %v", err)
	}
	got := map[string]string{}
	for _, kv := range keys {
		got[kv[0]] = kv[1]
	}
	if got["CHAP_N"] != "user" {
		t.Errorf("CHAP_N = %q, want user", got["CHAP_N"])
	}
	const want = "0x761c4e69e05c742497ade47d6bdaeb43"
	if got["CHAP_R"] != want {
		t.Errorf("CHAP_R = %q, want %q", got["CHAP_R"], want)
	}

	if _, err := it.chapResponseKeys(chapChallenge{algorithm: "1"}); err == nil {
		t.Fatal("unsupported CHAP algorithm should be rejected")
	}
}

// TestSecurityKeysOffersCHAP locks the wire offer: CHAP gets "CHAP,None" plus the
// MD5 algorithm id, plain auth keeps offering only None.
func TestSecurityKeysOffersCHAP(t *testing.T) {
	chap := (&Initiator{cfg: Config{Auth: "CHAP", InitiatorIQN: "iqn.t:i", TargetIQN: "iqn.t:t"}}).securityKeys()
	kv := map[string]string{}
	for _, p := range chap {
		kv[p[0]] = p[1]
	}
	if kv["AuthMethod"] != "CHAP,None" || kv["CHAP_A"] != "5" {
		t.Fatalf("chap offer = AuthMethod %q CHAP_A %q, want CHAP,None / 5", kv["AuthMethod"], kv["CHAP_A"])
	}

	none := (&Initiator{cfg: Config{Auth: "none", InitiatorIQN: "iqn.t:i", TargetIQN: "iqn.t:t"}}).securityKeys()
	for _, p := range none {
		if p[0] == "CHAP_A" {
			t.Fatal("plain auth must not advertise CHAP_A")
		}
	}
}

// TestDialRejectsCHAPWithoutCredentials checks the credential requirement is
// enforced before any network work.
func TestDialRejectsCHAPWithoutCredentials(t *testing.T) {
	_, err := Dial(context.Background(), Config{
		Address: "127.0.0.1:1", TargetIQN: "iqn.test:target", Auth: "chap", Username: "user",
	})
	if err == nil || !strings.Contains(err.Error(), "requires username and secret") {
		t.Fatalf("Dial chap without secret: err = %v, want credential error", err)
	}
	_, err = Dial(context.Background(), Config{
		Address: "127.0.0.1:1", TargetIQN: "iqn.test:target", Auth: "krb5",
	})
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("Dial unsupported auth: err = %v, want unsupported error", err)
	}
}

// TestLoginCHAPHandshake drives the full login state machine against a stub
// target: CHAP offer -> challenge -> answer -> transit to full feature.
func TestLoginCHAPHandshake(t *testing.T) {
	cli, srv := net.Pipe()
	defer cli.Close()

	const (
		challenge = "0x1234567890abcdef"
		wantResp  = "0x761c4e69e05c742497ade47d6bdaeb43"
	)
	// The stub reports through a channel rather than t: it runs on its own
	// goroutine, and a login that returns nil proves it finished its script.
	stubErr := make(chan error, 1)
	go func() { stubErr <- runCHAPStub(srv, wantResp, challenge) }()

	it := &Initiator{
		cfg: Config{
			TargetIQN:                "iqn.1991-05.com.microsoft:test-target",
			InitiatorIQN:             "iqn.2024-01.local.vault-iscsi-cache:test",
			Auth:                     "chap",
			Username:                 "user",
			Secret:                   "secret",
			MaxRecvDataSegmentLength: DefaultMaxRecvDataSegmentLength,
			MaxBurstLength:           DefaultMaxBurstLength,
		},
		conn:             cli,
		log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		cmdSN:            1,
		ourMaxRecvDSL:    DefaultMaxRecvDataSegmentLength,
		targetMaxRecvDSL: DefaultMaxRecvDataSegmentLength,
		maxBurstLength:   DefaultMaxBurstLength,
	}
	if err := it.login(context.Background()); err != nil {
		t.Fatalf("login: %v", err)
	}
	if err := <-stubErr; err != nil {
		t.Fatalf("stub target: %v", err)
	}
}

// runCHAPStub answers the three login exchanges a CHAP session needs and checks
// what the initiator sent. It closes the connection on the way out so a failed
// handshake cannot wedge the initiator on a blocked write.
func runCHAPStub(conn net.Conn, wantResp, challenge string) error {
	defer conn.Close()

	// Exchange 1: the initiator offers CHAP and advertises MD5.
	req, err := readLoginReq(conn)
	if err != nil {
		return err
	}
	if got := iscsi.ParseText(req.Data)["authmethod"]; got != "CHAP,None" {
		return fmt.Errorf("AuthMethod offer = %q, want CHAP,None", got)
	}
	if err := writeLoginResp(conn, req, iscsi.StageSecurity, iscsi.StageOperational, false, [][2]string{
		{"AuthMethod", "CHAP"}, {"CHAP_A", "5"}, {"CHAP_I", "42"}, {"CHAP_C", challenge},
	}, 0, 0); err != nil {
		return err
	}

	// Exchange 2: the initiator answers the challenge.
	req, err = readLoginReq(conn)
	if err != nil {
		return err
	}
	text := iscsi.ParseText(req.Data)
	if text["chap_n"] != "user" {
		return fmt.Errorf("CHAP_N = %q, want user", text["chap_n"])
	}
	if text["chap_r"] != wantResp {
		return fmt.Errorf("CHAP_R = %q, want %q", text["chap_r"], wantResp)
	}
	if err := writeLoginResp(conn, req, iscsi.StageSecurity, iscsi.StageOperational, true, [][2]string{
		{"AuthMethod", "CHAP"},
	}, 0, 0); err != nil {
		return err
	}

	// Exchange 3: operational keys, then grant full feature.
	req, err = readLoginReq(conn)
	if err != nil {
		return err
	}
	return writeLoginResp(conn, req, iscsi.StageOperational, iscsi.StageFullFeature, true, nil, 0, 0)
}

// readLoginReq reads one Login Request from the stub target's side.
func readLoginReq(conn net.Conn) (*iscsi.PDU, error) {
	pdu, err := iscsi.ReadPDU(conn)
	if err != nil {
		return nil, fmt.Errorf("read login request: %w", err)
	}
	if pdu.Header.Opcode() != iscsi.OpLoginReq {
		return nil, fmt.Errorf("got %s, want Login Request", iscsi.OpcodeName(pdu.Header.Opcode()))
	}
	return pdu, nil
}

// writeLoginResp writes a Login Response mirroring the header layout the
// frontend target uses.
func writeLoginResp(conn net.Conn, req *iscsi.PDU, csg, nsg uint8, transit bool, keys [][2]string, class, detail uint8) error {
	resp := iscsi.NewPDU(iscsi.OpLoginResp, iscsi.EncodeText(keys...))
	var flags uint8 = (csg << iscsi.LoginFlagCSGShift) | (nsg & iscsi.LoginFlagNSGMask)
	if transit {
		flags |= iscsi.LoginFlagTransit
	}
	resp.Header.SetFlags(flags)
	resp.Header.SetITT(req.Header.ITT())
	resp.Header.SetStatSN(1)
	resp.Header.SetExpCmdSN(1)
	resp.Header.SetMaxCmdSN(8)
	resp.Header[36] = class
	resp.Header[37] = detail
	if err := iscsi.WritePDU(conn, resp); err != nil {
		return fmt.Errorf("write login response: %w", err)
	}
	return nil
}

// mustHex decodes a hex string in a test, failing the test on a bad literal.
func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex literal %q: %v", s, err)
	}
	return b
}
