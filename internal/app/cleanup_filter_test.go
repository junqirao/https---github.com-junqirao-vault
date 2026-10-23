package app

import (
	"testing"

	"vault/internal/domain"
)

// TestFilterDiffsOlderThan 锁定"退出维护后的自动清理"边界：
// 只删旧版本，绝不能把清理任务排队期间刚派生出来的新版本盘一起删掉。
func TestFilterDiffsOlderThan(t *testing.T) {
	disks := []domain.Disk{
		{ID: "v0", ParentVersion: 0},
		{ID: "v1", ParentVersion: 1},
		{ID: "v2", ParentVersion: 2},
	}

	// 版本刚 +1（新版本 = 1）：只应删 v0。
	got := filterDiffsOlderThan(disks, 1)
	if len(got) != 1 || got[0].ID != "v0" {
		t.Fatalf("OnlyOlderThan=1 应只剩 v0，实际 %+v", got)
	}

	// 版本为 2 时：v0/v1 都是旧的。
	got = filterDiffsOlderThan(disks, 2)
	if len(got) != 2 || got[0].ID != "v0" || got[1].ID != "v1" {
		t.Fatalf("OnlyOlderThan=2 应保留 v0/v1，实际 %+v", got)
	}

	// 0 表示不筛选（手动"清空差异盘"的语义）。
	if got = filterDiffsOlderThan(disks, 0); len(got) != 3 {
		t.Fatalf("OnlyOlderThan=0 不应筛选，实际 %+v", got)
	}
}
