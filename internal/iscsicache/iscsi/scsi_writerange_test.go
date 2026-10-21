package iscsi

import (
	"encoding/binary"
	"testing"
)

// TestParseWriteRange 锁定"哪些写入命令必须让读缓存失效"：
// 少认一种（尤其 WRITE(6)/COMPARE AND WRITE/FORMAT UNIT），写后立刻读回就会拿到旧数据。
func TestParseWriteRange(t *testing.T) {
	// WRITE(6)：LBA 21 位、块数 1 字节（0 = 256）。
	w6 := []byte{SCSIWrite6, 0x00, 0x12, 0x34, 0x08, 0x00}
	if lba, blocks, ok := ParseWriteRange(w6); !ok || lba != 0x1234 || blocks != 8 {
		t.Fatalf("WRITE(6) = %d,%d,%v", lba, blocks, ok)
	}
	w6[4] = 0
	if _, blocks, ok := ParseWriteRange(w6); !ok || blocks != 256 {
		t.Fatalf("WRITE(6) 块数 0 应为 256，实际 %d", blocks)
	}

	// WRITE(10)。
	w10 := make([]byte, 10)
	w10[0] = SCSIWrite10
	binary.BigEndian.PutUint32(w10[2:6], 0x1000)
	binary.BigEndian.PutUint16(w10[7:9], 64)
	if lba, blocks, ok := ParseWriteRange(w10); !ok || lba != 0x1000 || blocks != 64 {
		t.Fatalf("WRITE(10) = %d,%d,%v", lba, blocks, ok)
	}

	// WRITE(16) / COMPARE AND WRITE(16) / WRITE ATOMIC(16) 同布局。
	for _, op := range []byte{SCSIWrite16, SCSICompareAndWrite, SCSIWriteAtomic16} {
		cdb := make([]byte, 16)
		cdb[0] = op
		binary.BigEndian.PutUint64(cdb[2:10], 0x2000)
		binary.BigEndian.PutUint32(cdb[10:14], 32)
		if lba, blocks, ok := ParseWriteRange(cdb); !ok || lba != 0x2000 || blocks != 32 {
			t.Fatalf("op 0x%02x = %d,%d,%v", op, lba, blocks, ok)
		}
	}

	// 非写命令不应被当成写。
	if _, _, ok := ParseWriteRange([]byte{SCSIRead10, 0, 0, 0, 0, 0, 0, 0, 0, 0}); ok {
		t.Fatal("READ(10) 不应被识别为写命令")
	}
	// WRITE SAME / UNMAP 由各自解析器处理，这里不重复认领。
	if _, _, ok := ParseWriteRange([]byte{SCSIWriteSame10, 0, 0, 0, 0, 0, 0, 0, 0, 0}); ok {
		t.Fatal("WRITE SAME 不应由 ParseWriteRange 处理")
	}
}
