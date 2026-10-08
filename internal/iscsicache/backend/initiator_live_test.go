package backend

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"testing"
	"time"

	"vault/internal/iscsicache/iscsi"
)

// Live tests run only when a reachable target is configured, e.g.
//
//	$env:VAULT_ISCSI_TEST_ADDR="172.18.28.200:3260"
//	$env:VAULT_ISCSI_TEST_IQN="iqn.1991-05.com.microsoft:win-0e595h11jss-test-target"
//	go test ./internal/iscsicache/backend -run Live -v
func liveConfig(t *testing.T) Config {
	t.Helper()
	addr := os.Getenv("VAULT_ISCSI_TEST_ADDR")
	iqn := os.Getenv("VAULT_ISCSI_TEST_IQN")
	if addr == "" || iqn == "" {
		t.Skip("live target not configured (VAULT_ISCSI_TEST_ADDR / VAULT_ISCSI_TEST_IQN)")
	}
	return Config{
		Address:      addr,
		TargetIQN:    iqn,
		InitiatorIQN: os.Getenv("VAULT_ISCSI_TEST_INITIATOR"),
		Auth:         "none",
		IOTimeout:    20 * time.Second,
	}
}

func TestLiveLoginAndDiscover(t *testing.T) {
	cfg := liveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	it, err := Dial(ctx, cfg)
	if err != nil {
		t.Fatalf("dial/login failed: %v", err)
	}
	defer it.Close()

	info := it.Info()
	t.Logf("device: vendor=%q product=%q rev=%q type=%#x", info.Vendor, info.Product, info.Revision, info.DeviceType)
	t.Logf("block_size=%d block_count=%d capacity=%d bytes", info.BlockSize, info.BlockCount, info.CapacityBytes())
	t.Logf("serial=%q wwid=%q target_max_recv_dsl=%d", info.Serial, info.WWID, info.TargetMaxRecvDSL)

	if info.BlockSize != 512 && info.BlockSize != 4096 {
		t.Errorf("unexpected block size %d", info.BlockSize)
	}
	if info.BlockCount == 0 {
		t.Errorf("unexpected zero block count")
	}
	if info.DeviceType != 0x00 {
		t.Errorf("expected direct-access device type 0x00, got %#x", info.DeviceType)
	}
}

func TestLiveReadAndCommandChecks(t *testing.T) {
	cfg := liveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	it, err := Dial(ctx, cfg)
	if err != nil {
		t.Fatalf("dial/login failed: %v", err)
	}
	defer it.Close()

	info := it.Info()

	// TEST UNIT READY
	res, err := it.Exec(ctx, []byte{iscsi.SCSITestUnitReady, 0, 0, 0, 0, 0}, nil, 0)
	if err != nil {
		t.Fatalf("TEST UNIT READY transport error: %v", err)
	}
	if !res.OK() {
		t.Fatalf("TEST UNIT READY status: %v", res.Error())
	}

	// READ(16) of 8 blocks at LBA 0, then again to confirm stability.
	blocks := uint32(8)
	want := int64(blocks) * int64(info.BlockSize)
	readLBA := func(lba uint64) []byte {
		cdb := make([]byte, 16)
		cdb[0] = iscsi.SCSIRead16
		binary.BigEndian.PutUint64(cdb[2:10], lba)
		binary.BigEndian.PutUint32(cdb[10:14], blocks)
		r, err := it.Exec(ctx, cdb, nil, int(want))
		if err != nil {
			t.Fatalf("READ(16) lba=%d transport error: %v", lba, err)
		}
		if !r.OK() {
			t.Fatalf("READ(16) lba=%d status: %v", lba, r.Error())
		}
		if len(r.Data) != int(want) {
			t.Fatalf("READ(16) lba=%d returned %d bytes, want %d", lba, len(r.Data), want)
		}
		return r.Data
	}

	a := readLBA(0)
	b := readLBA(0)
	if !bytes.Equal(a, b) {
		t.Errorf("two reads of the same LBA returned different data")
	}

	// Cross-check READ(10) against READ(16) on the same range.
	cdb10 := make([]byte, 10)
	cdb10[0] = iscsi.SCSIRead10
	binary.BigEndian.PutUint32(cdb10[2:6], 0)
	binary.BigEndian.PutUint16(cdb10[7:9], uint16(blocks))
	r10, err := it.Exec(ctx, cdb10, nil, int(want))
	if err != nil {
		t.Fatalf("READ(10) transport error: %v", err)
	}
	if !r10.OK() {
		t.Fatalf("READ(10) status: %v", r10.Error())
	}
	if !bytes.Equal(r10.Data, a) {
		t.Errorf("READ(10) and READ(16) disagree on LBA 0..%d", blocks-1)
	}

	// Out-of-range read must be rejected with CHECK CONDITION, not dropped.
	oorCDB := make([]byte, 16)
	oorCDB[0] = iscsi.SCSIRead16
	binary.BigEndian.PutUint64(oorCDB[2:10], info.BlockCount+1024)
	binary.BigEndian.PutUint32(oorCDB[10:14], 1)
	oor, err := it.Exec(ctx, oorCDB, nil, int(info.BlockSize))
	if err != nil {
		t.Fatalf("out-of-range READ transport error: %v", err)
	}
	if oor.OK() {
		t.Errorf("out-of-range READ unexpectedly returned GOOD")
	} else {
		t.Logf("out-of-range READ correctly rejected: %s", iscsi.SenseFromResult(oor.Sense))
	}
}

// TestLiveWriteReadback writes to the last LBA only (outside any filesystem
// likely in use) and restores nothing; it is opt-in.
func TestLiveWriteReadback(t *testing.T) {
	if os.Getenv("VAULT_ISCSI_TEST_ALLOW_WRITE") != "1" {
		t.Skip("set VAULT_ISCSI_TEST_ALLOW_WRITE=1 to run the destructive write test")
	}
	cfg := liveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	it, err := Dial(ctx, cfg)
	if err != nil {
		t.Fatalf("dial/login failed: %v", err)
	}
	defer it.Close()

	info := it.Info()
	lba := info.BlockCount - 1
	payload := make([]byte, info.BlockSize)
	for i := range payload {
		payload[i] = byte(i*7 + 3)
	}

	cdb := make([]byte, 16)
	cdb[0] = iscsi.SCSIWrite16
	binary.BigEndian.PutUint64(cdb[2:10], lba)
	binary.BigEndian.PutUint32(cdb[10:14], 1)
	wr, err := it.Exec(ctx, cdb, payload, 0)
	if err != nil {
		t.Fatalf("WRITE(16) transport error: %v", err)
	}
	if !wr.OK() {
		t.Fatalf("WRITE(16) status: %v", wr.Error())
	}

	rc := make([]byte, 16)
	rc[0] = iscsi.SCSIRead16
	binary.BigEndian.PutUint64(rc[2:10], lba)
	binary.BigEndian.PutUint32(rc[10:14], 1)
	rd, err := it.Exec(ctx, rc, nil, int(info.BlockSize))
	if err != nil {
		t.Fatalf("read-back transport error: %v", err)
	}
	if !rd.OK() {
		t.Fatalf("read-back status: %v", rd.Error())
	}
	if !bytes.Equal(rd.Data, payload) {
		t.Errorf("write-then-read mismatch at LBA %d", lba)
	}
}
