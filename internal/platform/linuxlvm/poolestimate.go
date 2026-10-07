//go:build linux

package linuxlvm

import (
	"context"
	"strings"

	"vault/internal/apperr"
)

// EstimatePoolMaxBytes 估算"用这些设备新建卷组后，还能建多大的 thin pool"（字节）。
//
// 为什么需要它：卷组还不存在时没有 vgs 可读，`VolumeGroup.PoolMaxBytes` 无从谈起，
// 但用户此刻正要填池容量。若让前端拿磁盘总容量自己乘个系数，算出来的值必然与后端
// 校验对不上——真机上就是这么撞的：自动填好 14 GB，提交却报"可用 11.6 GB"。
// 这里按与 InitializePool 完全相同的 poolMaxBytesWith 算，前端直接用返回值即可。
//
// 设备总容量还要再留 1%：建 VG 时 LVM 会给每个 PV 划出 metadata area 并按 PE
// （默认 4 MiB）对齐，vg_free 略小于"磁盘容量之和"。留出这点余量，估算值才是
// 真填得进去的值；估得偏大就又回到"填好了却提交不了"。
func (m *Manager) EstimatePoolMaxBytes(ctx context.Context, devices []string, metaSize string) (int64, error) {
	if len(devices) == 0 {
		return 0, apperr.InvalidParam("hdd_devices")
	}
	listed, err := m.ListBlockDevices(ctx)
	if err != nil {
		return 0, err
	}
	want := make(map[string]struct{}, len(devices))
	for _, d := range devices {
		if p := strings.TrimSpace(d); p != "" {
			want[p] = struct{}{}
		}
	}
	var total int64
	for _, d := range listed {
		if _, ok := want[d.Path]; ok {
			total += d.SizeBytes
		}
	}
	if total <= 0 {
		// 一块都没匹配上（设备被拔掉、路径变了）：报参数错误，而不是回一个说不清
		// 来历的 0——前端会把容量输入框锁死在那个数字上。
		return 0, apperr.InvalidParam("hdd_devices")
	}
	meta := strings.TrimSpace(metaSize)
	if meta == "" {
		meta = strings.TrimSpace(m.metadataSize) // 与 InitializePool 一致：请求 > 配置 > 自适应
	}
	// 还留 1% 给卷组自身的 metadata area 与 PE 对齐——那部分不在池元数据预留口径里。
	return poolMaxBytesWith(total-total/100, meta), nil
}
