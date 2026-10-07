package store

import (
	"testing"

	"vault/internal/domain"
)

// TestDiskAccountedBytes 锁定"这块盘占了多少"的统一口径（与 accountedUsageSQL 同义）。
//
// 重点是 ready + physical=0：thin 差异盘刚派生时独占块本来就是 0（全部与母盘共享），
// 这是**实测值**而不是"没测到"。早先把它当成未采样、退回标称容量，导致新建的库
// 一上线就"已用 100%"（真实反馈）。
func TestDiskAccountedBytes(t *testing.T) {
	const size int64 = 16 << 30

	cases := []struct {
		name     string
		state    domain.DiskState
		physical int64
		want     int64
	}{
		{"有实测值就用实测值", domain.DiskStateReady, 3 << 30, 3 << 30},
		{"已建好的盘实测 0 就是 0", domain.DiskStateReady, 0, 0},
		{"已发布同理", domain.DiskStatePublished, 0, 0},
		{"建盘失败按 0 计（盘不存在）", domain.DiskStateError, 0, 0},
		{"建盘失败但已有实测值时不抹掉", domain.DiskStateError, 1 << 30, 1 << 30},
		{"尚未建好按标称容量预留", domain.DiskStateCreating, 0, size},
		{"回收中同样按标称容量预留", domain.DiskStateDeleting, 0, size},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := diskAccountedBytes(string(tc.state), tc.physical, size); got != tc.want {
				t.Fatalf("state=%s physical=%d：want %d，got %d", tc.state, tc.physical, tc.want, got)
			}
		})
	}
}
