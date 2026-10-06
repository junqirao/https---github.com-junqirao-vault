//go:build linux

package linuxlvm

import (
	"context"
	"net/http"
	"strings"

	"vault/internal/apperr"
	"vault/internal/platform"
)

// coreLVFields 是所有 LVM 版本都稳定支持的字段。
const coreLVFields = "lv_name,lv_size,data_percent,metadata_percent,lv_attr,pool_lv"

// cacheLVFields 是缓存相关字段（老版本可能不认识，需要容错）。
const cacheLVFields = "cache_mode,chunksize,cache_policy,cache_dirty_blocks," +
	"cache_read_hits,cache_read_misses,cache_write_hits,cache_write_misses,devices"

// Status 返回存储池与缓存的现状快照。
func (m *Manager) Status(ctx context.Context) (*platform.PoolStatus, error) {
	st := &platform.PoolStatus{Kind: platform.KindLinux, VG: m.vg, ThinPool: m.thinPool}

	// VG 容量。
	vgFound := false
	vgs, err := m.vgsRows(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range vgs {
		if strings.TrimSpace(rowStr(r, "vg_name")) != m.vg {
			continue
		}
		vgFound = true
		st.SizeBytes = rowInt(r, "vg_size")
		st.FreeBytes = rowInt(r, "vg_free")
		break
	}

	rows, err := m.lvsAllRows(ctx)
	if err != nil {
		return nil, err
	}
	poolFound := false
	for _, r := range rows {
		name := strings.TrimSpace(rowStr(r, "lv_name"))
		poolLV := strings.TrimSpace(rowStr(r, "pool_lv"))
		if name == m.thinPool {
			poolFound = true
			st.DataPercent = rowFloat(r, "data_percent")
			st.MetadataPercent = rowFloat(r, "metadata_percent")
		}
		if name != m.thinPool && poolLV != m.thinPool {
			continue
		}
		attr := rowStr(r, "lv_attr")
		if strings.Contains(attr, "C") {
			st.CacheAttached = true
		}
		if strings.Contains(attr, "Fail") {
			st.CacheHealth = "Fail"
		}
		// 缓存指标只取第一行带 cache_mode 的（通常是池主行或 _tdata 行）。
		if st.CacheMode == "" && rowStr(r, "cache_mode") != "" {
			st.CacheMode = rowStr(r, "cache_mode")
			st.CacheChunkSize = rowStr(r, "chunksize")
			st.CachePolicy = rowStr(r, "cache_policy")
			st.CacheDirtyBlocks = rowInt(r, "cache_dirty_blocks")
			st.CacheReadHits = rowInt(r, "cache_read_hits")
			st.CacheReadMisses = rowInt(r, "cache_read_misses")
			st.CacheWriteHits = rowInt(r, "cache_write_hits")
			st.CacheWriteMisses = rowInt(r, "cache_write_misses")
		}
	}
	// 缓存设备取自 cache pool 行的 devices 字段。
	for _, r := range rows {
		if strings.TrimSpace(rowStr(r, "lv_name")) == cachePoolName(m.thinPool) {
			st.CacheDevices = parseDevices(rowStr(r, "devices"))
			break
		}
	}
	// dmsetup status 补充 needs_check（lvs 看不到该状态）。
	if st.CacheAttached {
		if out, err := m.run(ctx, "dmsetup", "status", m.vg+"-"+m.thinPool); err == nil {
			if strings.Contains(out, "needs_check") {
				st.CacheHealth = "needs_check"
			}
		}
	}
	st.Exists = vgFound && poolFound
	return st, nil
}

// lvsAllRows 读取 -a（含子 LV）的 lvs 报告；缓存字段不被支持时退化为核心字段。
//
// 容错策略：先按"核心 + 缓存"字段集查询；若某个发行版的 LVM 不认识其中任一字段，
// 命令会整体非 0 退出，此时只取核心字段重试，并告警——绝不让缺字段导致 Status 整体失败。
func (m *Manager) lvsAllRows(ctx context.Context) ([]lvsRow, error) {
	rows, err := m.lvsRows(ctx, "-a", "-o", coreLVFields+","+cacheLVFields)
	if err != nil {
		m.logger.Warn("lvs 含缓存字段查询失败，退化为核心字段", "err", err.Error())
		return m.lvsRows(ctx, "-a", "-o", coreLVFields)
	}
	return rows, nil
}

// InitializePool 幂等地初始化存储池（含可选的缓存设备）。已存在的一律跳过，绝不覆盖既有数据。
//
// 顺序：① 校验名 → ② 建 VG（需要时）→ ③ 建 thin pool → ④ 建/挂 cache pool。
func (m *Manager) InitializePool(ctx context.Context, spec platform.PoolSpec) error {
	vg := strings.TrimSpace(spec.VG)
	if vg == "" {
		vg = m.vg
	}
	pool := strings.TrimSpace(spec.ThinPool)
	if pool == "" {
		pool = m.thinPool
	}
	if !lvNameRe.MatchString(vg) {
		return apperr.InvalidParam("vg")
	}
	if !lvNameRe.MatchString(pool) {
		return apperr.InvalidParam("thin_pool")
	}
	chunk := strings.TrimSpace(spec.ChunkSize)
	if chunk == "" {
		chunk = m.chunkSize
	}
	if chunk == "" {
		chunk = "256K"
	}
	meta := strings.TrimSpace(spec.MetadataSize)
	if meta == "" {
		meta = m.metadataSize
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// ② 卷组。
	if m.vgExists(ctx, vg) {
		m.logger.Info("跳过卷组创建（已存在）", "vg", vg)
	} else {
		if len(spec.HDDDevices) == 0 {
			return apperr.InvalidParam("hdd_devices")
		}
		if err := m.createVG(ctx, vg, spec.HDDDevices); err != nil {
			return err
		}
	}

	// ③ thin pool。
	if m.poolExists(ctx, vg, pool) {
		m.logger.Info("跳过 thin pool 创建（已存在）", "thin_pool", vg+"/"+pool)
	} else {
		args := []string{"--type", "thin-pool", "-n", pool, "--chunksize", chunk}
		if meta != "" {
			args = append(args, "--poolmetadatasize", meta)
		}
		args = append(args, vg)
		if _, err := m.run(ctx, "lvcreate", args...); err != nil {
			return err
		}
		m.logger.Info("已创建 thin pool", "thin_pool", vg+"/"+pool, "chunksize", chunk, "metadata_size", meta)
	}

	// ④ dm-cache。
	if spec.Cache != nil && len(spec.Cache.Devices) > 0 {
		if m.cacheAttached(ctx, vg, pool) {
			m.logger.Info("跳过缓存挂载（已挂载）", "thin_pool", vg+"/"+pool)
		} else {
			for _, dev := range spec.Cache.Devices {
				if err := m.validateCacheDevice(ctx, vg, dev); err != nil {
					return err
				}
			}
			if err := m.ensureCachePool(ctx, vg, pool, spec.Cache.Devices, spec.Cache.ChunkSize, spec.Cache.Policy); err != nil {
				return err
			}
			if err := m.attachCache(ctx, vg, pool); err != nil {
				return err
			}
			if err := m.setCacheMode(ctx, vg, pool, spec.Cache.Mode); err != nil {
				return err
			}
		}
	}
	return nil
}

// createVG 用给定设备创建卷组（已是 PV 的跳过 pvcreate，属于其它 VG 的拒绝）。
func (m *Manager) createVG(ctx context.Context, vg string, devices []string) error {
	pvs := make([]string, 0, len(devices))
	for _, dev := range devices {
		dev = strings.TrimSpace(dev)
		if dev == "" {
			continue
		}
		owner, isPV := m.pvVG(ctx, dev)
		switch {
		case isPV && owner == "":
			// 已是 PV 但未加入 VG：直接并入。
		case isPV && owner == vg:
			// 理论上 VG 尚不存在，不会走到这里；稳妥起见放行。
		case isPV:
			return apperr.New(apperr.CodeConflict, http.StatusConflict).
				WithArg("device", dev).WithArg("vg", owner)
		default:
			if _, err := m.run(ctx, "pvcreate", dev); err != nil {
				return err
			}
		}
		pvs = append(pvs, dev)
	}
	if len(pvs) == 0 {
		return apperr.InvalidParam("hdd_devices")
	}
	args := append([]string{vg}, pvs...)
	if _, err := m.run(ctx, "vgcreate", args...); err != nil {
		return err
	}
	m.logger.Info("已创建卷组", "vg", vg, "devices", pvs)
	return nil
}

// vgExists 判断卷组是否存在。
func (m *Manager) vgExists(ctx context.Context, vg string) bool {
	rows, err := m.vgsRows(ctx)
	if err != nil {
		return false
	}
	return rowsHaveValue(rows, "vg_name", vg)
}

// poolExists 判断 thin pool 是否存在。
func (m *Manager) poolExists(ctx context.Context, vg, pool string) bool {
	rows, err := m.lvsRows(ctx, "-o", "lv_name", vg)
	if err != nil {
		return false
	}
	return rowsHaveValue(rows, "lv_name", pool)
}

// poolMissingKind 判定存储池缺失的部位："vg" / "pool"；两者都在、或**判不出来**时返回 ""。
//
// 只在读水位失败之后调用（失败路径才多一次只读探测，正常路径零开销）。
// 判不出来（vgs 自己也失败，例如服务账号权限不足）刻意返回 ""：
// 那种情况下报"池不存在"会把运维引向错误的方向，不如保留原有的兜底行为。
func (m *Manager) poolMissingKind(ctx context.Context, vg, pool string) string {
	rows, err := m.vgsRows(ctx)
	if err != nil {
		return ""
	}
	if !rowsHaveValue(rows, "vg_name", vg) {
		return "vg"
	}
	lvs, err := m.lvsRows(ctx, "-o", "lv_name", vg)
	if err != nil {
		return ""
	}
	if !rowsHaveValue(lvs, "lv_name", pool) {
		return "pool"
	}
	return ""
}

// rowsHaveValue 判断行集中是否存在某字段等于 v 的行（比较前 TrimSpace）。
func rowsHaveValue(rows []lvsRow, field, v string) bool {
	for _, r := range rows {
		if strings.TrimSpace(rowStr(r, field)) == v {
			return true
		}
	}
	return false
}

// parseDevices 解析 lvs 的 devices 字段（形如 "/dev/sdb1(0),/dev/sdc1(0)"）。
func parseDevices(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if i := strings.IndexByte(part, '('); i >= 0 {
			part = strings.TrimSpace(part[:i])
		}
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
