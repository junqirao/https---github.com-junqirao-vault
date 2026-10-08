package iscsicache_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"vault/internal/iscsicache"
	"vault/internal/iscsicache/backend"
	"vault/internal/iscsicache/iscsi"
)

// TestLiveEndToEndThroughCache runs the whole module — frontend target, proxy,
// cache and a real remote backend — and asserts that a repeated read is served
// from the local cache without touching the backend again. It is opt-in:
//
//	$env:VAULT_ISCSI_TEST_ADDR="172.18.28.200:3260"
//	$env:VAULT_ISCSI_TEST_IQN="iqn.1991-05.com.microsoft:win-0e595h11jss-test-target"
//	go test ./internal/iscsicache -run Live -v
func TestLiveEndToEndThroughCache(t *testing.T) {
	addr := os.Getenv("VAULT_ISCSI_TEST_ADDR")
	iqn := os.Getenv("VAULT_ISCSI_TEST_IQN")
	if addr == "" || iqn == "" {
		t.Skip("live target not configured (VAULT_ISCSI_TEST_ADDR / VAULT_ISCSI_TEST_IQN)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const proxyIQN = "iqn.2024-01.local.vault:live-e2e"
	noScan := false

	cfg := iscsicache.Config{
		ListenAddr: "127.0.0.1:0",
		TargetIQN:  proxyIQN,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Backend: iscsicache.BackendConfig{
			Address: addr, TargetIQN: iqn, Auth: "none",
			DialTimeout: "10s", IOTimeout: "30s",
		},
		Cache: iscsicache.CacheConfig{
			Mode: "writearound", BlockSize: 65536, SectorSize: 4096,
		},
	}
	cfg.Cache.L1.SizeBytes = 32 << 20
	cfg.Cache.L2.Enabled = false
	cfg.Cache.ScanDetection.Enabled = &noScan

	svc, err := iscsicache.New(ctx, cfg)
	if err != nil {
		t.Fatalf("iscsicache.New: %v", err)
	}
	defer svc.Close()

	serveErr := make(chan error, 1)
	go func() { serveErr <- svc.Start(ctx) }()

	dev := svc.DeviceInfo()
	t.Logf("proxied device: vendor=%q product=%q serial=%q block_size=%d block_count=%d",
		dev.Vendor, dev.Product, dev.Serial, dev.BlockSize, dev.BlockCount)
	if dev.BlockSize == 0 || dev.BlockCount == 0 {
		t.Fatalf("proxy did not discover the backing geometry: %+v", dev)
	}

	// Connect to our own proxy exactly like a local Windows initiator would.
	it, err := backend.Dial(ctx, backend.Config{
		Address:   svc.Addr().String(),
		TargetIQN: proxyIQN,
		IOTimeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer it.Close()

	info := it.Info()
	if info.BlockSize != dev.BlockSize || info.BlockCount != dev.BlockCount {
		t.Fatalf("client saw %d/%d, proxy has %d/%d", info.BlockSize, info.BlockCount, dev.BlockSize, dev.BlockCount)
	}

	read := func(lba uint32, blocks uint16) []byte {
		t.Helper()
		cdb := make([]byte, 10)
		cdb[0] = iscsi.SCSIRead10
		binary.BigEndian.PutUint32(cdb[2:6], lba)
		binary.BigEndian.PutUint16(cdb[7:9], blocks)
		res, err := it.Exec(ctx, cdb, nil, int(blocks)*int(info.BlockSize))
		if err != nil {
			t.Fatalf("read lba=%d: %v", lba, err)
		}
		if !res.OK() {
			t.Fatalf("read lba=%d: %v", lba, res.Error())
		}
		if len(res.Data) != int(blocks)*int(info.BlockSize) {
			t.Fatalf("read lba=%d returned %d bytes", lba, len(res.Data))
		}
		return res.Data
	}

	first := read(0, 8)
	before := svc.Cache().Stats()
	second := read(0, 8)
	after := svc.Cache().Stats()

	if !bytes.Equal(first, second) {
		t.Fatal("cache returned different data for the same range")
	}
	if after.L1Hits <= before.L1Hits {
		t.Fatalf("second read was not served from L1: before=%+v after=%+v", before, after)
	}
	if after.BackendReads != before.BackendReads {
		t.Fatalf("second read reached the backend: before=%d after=%d", before.BackendReads, after.BackendReads)
	}
	t.Logf("cache stats: reads=%d l1_hits=%d backend_reads=%d fills=%d",
		after.Reads, after.L1Hits, after.BackendReads, after.Fills)

	if err := it.Close(); err != nil {
		t.Fatalf("initiator close: %v", err)
	}
	// Close the listener before waiting, otherwise Serve stays blocked in Accept.
	if err := svc.Close(); err != nil {
		t.Fatalf("service close: %v", err)
	}
	cancel()
	if err := <-serveErr; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("serve: %v", err)
	}
}
