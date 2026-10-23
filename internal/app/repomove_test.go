package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCopyFileStreamCopiesContent(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.vhdx")
	dst := filepath.Join(dir, "nested", "dst.vhdx")
	data := append(bytes.Repeat([]byte{0}, 1<<20), []byte("tail-data")...)
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	var last int64
	copied, err := copyFileStream(context.Background(), src, dst, int64(len(data)), func(done int64) {
		last = done
	})
	if err != nil {
		t.Fatalf("copyFileStream: %v", err)
	}
	if copied != int64(len(data)) || last != int64(len(data)) {
		t.Fatalf("unexpected progress: copied=%d last=%d want=%d", copied, last, len(data))
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("copied content differs: got %d bytes, want %d", len(got), len(data))
	}
}

func TestCopyFileStreamStopsOnContextCancel(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.vhdx")
	dst := filepath.Join(dir, "dst.vhdx")
	if err := os.WriteFile(src, bytes.Repeat([]byte{7}, 32<<10), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := copyFileStream(ctx, src, dst, 32<<10, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestMovePercent(t *testing.T) {
	cases := []struct {
		done, total int64
		want        int
	}{
		{0, 0, 0},
		{0, 100, 0},
		{50, 100, 50},
		{100, 100, 100},
		{150, 100, 100},
	}
	for _, c := range cases {
		if got := movePercent(c.done, c.total); got != c.want {
			t.Fatalf("movePercent(%d, %d) = %d, want %d", c.done, c.total, got, c.want)
		}
	}
}
