package domain

import "testing"

// TestCalculateVHDXSizeUsesMBGranularity 锁定"按 MB 取整"的容量推导。
//
// 背景（真实反馈："我一点空间都没用就占了 1G"）：旧参数是 1GB 粒度 + 512MB 预留下限 +
// 1GB 空目录兜底，于是**空目录/小目录**也会推导出 1GB 级的 VHDX 标称容量。而分配时是
// **按母盘标称容量预留用量**的（差异盘建完后才按实测物理占用校正），于是每个分配一上来
// 就占 1GB 配额。VHDX 是动态盘，标称容量只是上限 —— 按 MB 取整才符合实际占用。
func TestCalculateVHDXSizeUsesMBGranularity(t *testing.T) {
	const (
		mib = int64(1) << 20
		gib = int64(1) << 30
	)
	s := DefaultSizing()

	cases := []struct {
		name      string
		dirBytes  int64
		requested int64
		want      int64
	}{
		{"空目录且未填 → 64MiB 兜底", 0, 0, 64 * mib},
		{"小目录 4MiB → 取整 4MiB + 预留下限 16MiB", 4 * mib, 0, 20 * mib},
		{"目录 500MiB → 500 + 10%（50MiB）", 500 * mib, 0, 550 * mib},
		{"用户填的比目录大 → 取用户值（并按 MB 取整）", 100 * mib, 2 * gib, 2 * gib},
		{"用户值不是整 MB → 向上取整到 1MiB", 0, 1500*mib + 1, 1501 * mib},
		{"低于硬下限 → 3MiB", 0, 1 * mib, 3 * mib},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := s.CalculateVHDXSize(c.dirBytes, c.requested); got != c.want {
				t.Fatalf("CalculateVHDXSize(%d, %d) = %d，期望 %d", c.dirBytes, c.requested, got, c.want)
			}
		})
	}
}

// TestCalculateVHDXSizeRegressionAgainstGBGranularity 把"旧 1GB 粒度"的坏结果钉成反例：
// 同一个 4MiB 小目录，旧参数会推导出 1GiB 级的标称容量（≈1.5GiB），这正是用户看到的 1G。
func TestCalculateVHDXSizeRegressionAgainstGBGranularity(t *testing.T) {
	const mib = int64(1) << 20
	const gib = int64(1) << 30

	legacy := Sizing{
		GranularityBytes:      1 << 30,   // 旧：1 GiB
		ReservePermille:       100,       // 10%
		ReserveFloorBytes:     512 << 20, // 旧：512 MiB
		MinSizeBytes:          3 << 20,
		EmptyDirFallbackBytes: 1 << 30, // 旧：1 GiB
	}
	if got := legacy.CalculateVHDXSize(4*mib, 0); got < gib {
		t.Fatalf("旧参数下小目录应推导成 GB 级（反例失效），实际 %d", got)
	}

	now := DefaultSizing().CalculateVHDXSize(4*mib, 0)
	if now >= 100*mib {
		t.Fatalf("小目录不应再推导成 %d 字节（应远小于 100MiB）", now)
	}
	if now < 4*mib {
		t.Fatalf("容量不得小于源目录大小：%d", now)
	}
}

// TestCalculateVHDXSizeFallsBackToDefaults 粒度非法（<=0）时回退默认参数，避免除零/负粒度。
func TestCalculateVHDXSizeFallsBackToDefaults(t *testing.T) {
	const mib = int64(1) << 20
	s := Sizing{GranularityBytes: 0}
	if got := s.CalculateVHDXSize(4*mib, 0); got != 20*mib {
		t.Fatalf("粒度非法时应回退默认参数，实际 %d", got)
	}
}
