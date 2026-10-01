//go:build linux

package linuxlvm

import (
	"context"
	"net/http"
	"strings"

	"vault/internal/apperr"
)

// cachePoolName 返回与 thin pool 配对的 cache pool 名。
func cachePoolName(thinPool string) string { return thinPool + "_cache" }

// validateCacheDevice 校验一个块设备能否被用作缓存。
//
// 拒绝：已挂载（会破坏在用文件系统）、已有文件系统（需用户先清空，绝不代为抹除）、
// 已属于**其它** VG（LVM 不支持 LVM-on-LVM，cache pool 必须与 origin 同 VG）。
// 已是本 VG 的 PV 或尚未加入任何 VG 的裸盘/未分配 PV 都放行。
func (m *Manager) validateCacheDevice(ctx context.Context, vg, dev string) error {
	dev = strings.TrimSpace(dev)
	if dev == "" {
		return apperr.InvalidParam("cache_device")
	}
	if mp, err := m.firstMountpoint(ctx, dev); err == nil && mp != "" {
		return apperr.New(apperr.CodeConflict, http.StatusConflict).
			WithArg("device", dev).WithArg("mountpoint", mp)
	}
	if fs, _ := m.blkidType(ctx, dev); fs != "" {
		return apperr.New(apperr.CodeConflict, http.StatusConflict).
			WithArg("device", dev).WithArg("existing_fs", fs)
	}
	if owner, ok := m.pvVG(ctx, dev); ok && owner != "" && owner != vg {
		return apperr.New(apperr.CodeConflict, http.StatusConflict).
			WithArg("device", dev).WithArg("vg", owner)
	}
	return nil
}

// ensureCachePool 把给定块设备接成 dm-cache 的 cache pool。
//
// LVM 不支持 LVM-on-LVM：cache pool 与 origin(thin pool) 必须同 VG，
// 因此不属于目标 VG 的设备先 pvcreate 再 vgextend 并入。
// 命令：lvcreate --type cache-pool -n <cpool> -l 100%FREE --chunksize <chunk> --cachepolicy <policy> <vg> <dev...>
func (m *Manager) ensureCachePool(ctx context.Context, vg, thinPool string, devices []string, chunk, policy string) error {
	if !lvNameRe.MatchString(vg) || !lvNameRe.MatchString(thinPool) {
		return apperr.InvalidParam("vg")
	}
	if chunk == "" {
		chunk = "256K"
	}
	if policy == "" {
		policy = "smq"
	}
	for _, dev := range devices {
		if err := m.ensurePV(ctx, vg, dev); err != nil {
			return err
		}
	}
	args := []string{
		"--type", "cache-pool",
		"-n", cachePoolName(thinPool),
		"-l", "100%FREE",
		"--chunksize", chunk,
		"--cachepolicy", policy,
		vg,
	}
	args = append(args, devices...)
	if _, err := m.run(ctx, "lvcreate", args...); err != nil {
		return err
	}
	m.logger.Info("已创建 cache pool", "cache_pool", vg+"/"+cachePoolName(thinPool),
		"devices", devices, "chunksize", chunk, "policy", policy)
	return nil
}

// ensurePV 确保设备已成为目标 VG 的 PV。
func (m *Manager) ensurePV(ctx context.Context, vg, dev string) error {
	dev = strings.TrimSpace(dev)
	if dev == "" {
		return apperr.InvalidParam("cache_device")
	}
	owner, isPV := m.pvVG(ctx, dev)
	switch {
	case isPV && owner == vg:
		return nil // 已在目标 VG，无需处理
	case isPV && owner == "":
		// 已是 PV 但未加入任何 VG：直接并入即可，无需（也不应）重复 pvcreate。
	case isPV:
		return apperr.New(apperr.CodeConflict, http.StatusConflict).
			WithArg("device", dev).WithArg("vg", owner)
	default:
		if _, err := m.run(ctx, "pvcreate", dev); err != nil {
			return err
		}
	}
	if _, err := m.run(ctx, "vgextend", vg, dev); err != nil {
		return err
	}
	return nil
}

// attachCache 把 cache pool 挂到 thin pool 上。
//
// ⚠️ 缓存实际插在 thin pool 的 _tdata 子 LV 上；但 lvconvert 挂载时传的是 **pool 名**，
// LVM 会自动落到 _tdata。而改 cachemode 必须针对 <pool>_tdata（见 setCacheMode），
// 对 pool 名操作会报错/无效——这是 dm-cache 最容易踩的坑。
func (m *Manager) attachCache(ctx context.Context, vg, thinPool string) error {
	_, err := m.run(ctx, "lvconvert",
		"--type", "cache",
		"--cachepool", vg+"/"+cachePoolName(thinPool),
		vg+"/"+thinPool,
	)
	if err != nil {
		return err
	}
	m.logger.Info("已挂载 dm-cache", "thin_pool", vg+"/"+thinPool, "cache_pool", vg+"/"+cachePoolName(thinPool))
	return nil
}

// setCacheMode 设置缓存模式；mode 只接受 writethrough / writeback。
//
// 命令针对 thin pool 的 **<pool>_tdata** 子 LV：对 pool 名操作会报错/无效。
// writeback 下 SSD 掉盘可能丢失未回写数据，故默认应保持 writethrough（由调用方决定）。
func (m *Manager) setCacheMode(ctx context.Context, vg, thinPool, mode string) error {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		return nil // 未指定：沿用 LVM 默认（writethrough），与 platform.CacheSpec 说明一致
	}
	switch mode {
	case "writethrough", "writeback":
	default:
		return apperr.InvalidParam("mode")
	}
	tdata := vg + "/" + thinPool + "_tdata"
	if _, err := m.run(ctx, "lvchange", "--cachemode", mode, tdata); err != nil {
		return err
	}
	m.logger.Info("已设置缓存模式", "target", tdata, "mode", mode)
	return nil
}

// firstMountpoint 返回设备（或其子分区）上的第一个挂载点。
func (m *Manager) firstMountpoint(ctx context.Context, dev string) (string, error) {
	out, err := m.run(ctx, "lsblk", "--noheadings", "-o", "MOUNTPOINTS", dev)
	if err != nil {
		return "", err
	}
	for _, f := range strings.Fields(out) {
		if f != "" {
			return f, nil
		}
	}
	return "", nil
}

// cacheAttached 判断 thin pool 上是否已挂载 dm-cache（lv_attr 含 'C'）。
func (m *Manager) cacheAttached(ctx context.Context, vg, thinPool string) bool {
	rows, err := m.lvsRows(ctx, "-a", "-o", "lv_name,pool_lv,lv_attr", vg)
	if err != nil {
		return false
	}
	for _, r := range rows {
		name := strings.TrimSpace(rowStr(r, "lv_name"))
		poolLV := strings.TrimSpace(rowStr(r, "pool_lv"))
		if name == thinPool || poolLV == thinPool {
			if strings.Contains(rowStr(r, "lv_attr"), "C") {
				return true
			}
		}
	}
	return false
}
