//go:build linux

package linuxlvm

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"vault/internal/apperr"
	"vault/internal/platform"
)

// coreLVFields 是所有 LVM 版本都稳定支持的字段。
//
// 含 vg_name：一台机器上可以有多个卷组（= 多个存储池），
// 只按 LV 名匹配会串号（不同 VG 里的同名池），必须先按 VG 归位。
const coreLVFields = "vg_name,lv_name,lv_size,data_percent,metadata_percent,lv_attr,pool_lv"

// cacheLVFields 是缓存相关字段（老版本可能不认识，需要容错）。
const cacheLVFields = "cache_mode,chunksize,cache_policy,cache_dirty_blocks," +
	"cache_read_hits,cache_read_misses,cache_write_hits,cache_write_misses,devices"

// Status 返回**配置的默认池**与缓存的现状快照（多存储池的完整列表见 ListPools）。
func (m *Manager) Status(ctx context.Context) (*platform.PoolStatus, error) {
	st, err := m.poolStatus(ctx, m.vg, m.thinPool)
	if err != nil {
		return nil, err
	}
	st.Default = true
	return st, nil
}

// poolStatus 组装指定池的状态快照（Status / PoolStatusOf / ListPools 共用）。
func (m *Manager) poolStatus(ctx context.Context, vg, pool string) (*platform.PoolStatus, error) {
	st := &platform.PoolStatus{Kind: platform.KindLinux, VG: vg, ThinPool: pool}

	// VG 容量。
	vgFound := false
	vgs, err := m.vgsRows(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range vgs {
		if strings.TrimSpace(rowStr(r, "vg_name")) != vg {
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
		// 老 LVM 可能没有 vg_name 字段：查不到就不做跨卷组过滤（保持原有行为）。
		if got := strings.TrimSpace(rowStr(r, "vg_name")); got != "" && got != vg {
			continue
		}
		name := strings.TrimSpace(rowStr(r, "lv_name"))
		poolLV := strings.TrimSpace(rowStr(r, "pool_lv"))
		if name == pool {
			poolFound = true
			st.DataPercent = rowFloat(r, "data_percent")
			st.MetadataPercent = rowFloat(r, "metadata_percent")
		}
		if name != pool && poolLV != pool {
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
	// 池自身的数据容量 = 该池上新建存储卷的逻辑容量上限（供前端预填"容量"并做 max）。
	//
	// 不能在上面那次 lvs 里顺手取：那次报告没有 --units b，lv_size 形如 "15.60g"，
	// 按字节解析会得到 0（与 vgsRows 注释里同一个坑），因此这里单独按字节查一次。
	// 读不到就留 0：前端按"未知"处理，只是不预填、不加上限，绝不因此让整个状态查询失败。
	if poolFound {
		if n, err := m.lvSizeBytes(vg, pool); err == nil {
			st.MaxStorageVolumeBytes = n
		}
	}
	// 缓存设备取自 cache pool 行的 devices 字段。
	for _, r := range rows {
		if strings.TrimSpace(rowStr(r, "lv_name")) == cachePoolName(pool) {
			st.CacheDevices = parseDevices(rowStr(r, "devices"))
			break
		}
	}
	// dmsetup status 补充 needs_check（lvs 看不到该状态）。
	if st.CacheAttached {
		if out, err := m.run(ctx, "dmsetup", "status", vg+"-"+pool); err == nil {
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
	// 同一块盘不能既作容量盘又作缓存盘：两者最终都在同一个卷组里，
	// 一块 PV 不可能既承载 thin pool 又承载 cache pool——真走到 LVM 只会
	// 得到一句看不懂的报错。前端已互斥勾选，这里兜住直连 API 的情况。
	if spec.Cache != nil {
		if dev := firstSharedPath(spec.HDDDevices, spec.Cache.Devices); dev != "" {
			return apperr.CacheDeviceOverlap(dev)
		}
	}
	chunk := strings.TrimSpace(spec.ChunkSize)
	if chunk == "" {
		chunk = m.chunkSize
	}
	if chunk == "" {
		chunk = "256K"
	}
	// 元数据大小：请求 > 配置 > 自适应（留空即自适应，见 poolMetaSizeFor）。
	// 不再像以前那样给配置兜一个 4G 的默认值——那个默认值在 16G 的盘上要吃掉
	// 4G(tmeta) + 4G(pmspare)，用户想建的池直接建不出来（真机反馈）。
	meta := strings.TrimSpace(spec.MetadataSize)
	if meta == "" {
		meta = strings.TrimSpace(m.metadataSize)
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
		// 一个卷组只允许一个 thin pool（磁盘引用只带 VG，池由 VG 反查，见 poolcatalog.go）。
		if err := m.checkSameVGPool(ctx, vg, pool); err != nil {
			return err
		}
		// ⚠️ 容量必须先于 lvcreate 校验（真机反馈）。请求超过卷组剩余空间时 LVM 会打印
		//
		//	Volume group "vg0" has insufficient free space (3070 extents): 262144 required.
		//	Do you really want to remove and DISCARD logical volume vg0/lvol0_pmspare? [y/n]:
		//
		// 也就是在**没有 tty 的服务进程**里问"要不要抹掉 pmspare 卷"，默认取 n 后以
		// exit 5 失败——用户只看到一句看不懂的英文；而一旦有人为了"别卡住"补上 -y，
		// pmspare 就真被抹了。这里提前拦住，并把可用容量一并告诉前端。
		if spec.SizeBytes > 0 {
			if free, ok := m.vgFreeBytes(ctx, vg); ok {
				usable := free - poolMetaReserveBytes(meta, spec.SizeBytes)
				if usable < 0 {
					usable = 0
				}
				if spec.SizeBytes > usable {
					return apperr.PoolSizeExceeded(usable, spec.SizeBytes)
				}
			}
		}
		// ⚠️ thin pool **必须**给出容量：`lvcreate --type thin-pool` 缺少 -L/-l 时
		// LVM 只会甩一句 "No command with matching syntax recognised"，用户完全无从下手
		// （真机反馈）。这里的口径是：
		//   - SizeBytes > 0：按指定容量建池（-L <n>B）；
		//   - SizeBytes <= 0：占满 VG 当前剩余空间（-l 100%FREE），元数据由 LVM 自行预留。
		//
		// 元数据大小：显式给了就用它；留空则按池容量自适应（poolMetaSizeFor），
		// 并且**总是显式传给 LVM**——预留计算必须与实际占用一致，交给 LVM 自己挑的话，
		// 它挑多大我们算不出来，就又回到"自动填好的值一提交就失败"。
		metaArg := meta
		if metaArg == "" {
			dataForMeta := spec.SizeBytes
			if dataForMeta <= 0 {
				if free, ok := m.vgFreeBytes(ctx, vg); ok {
					dataForMeta = free
				}
			}
			metaArg = strconv.FormatInt(poolMetaSizeFor(dataForMeta), 10) + "B"
		}
		args := []string{"--type", "thin-pool", "-n", pool, "--chunksize", chunk}
		if spec.SizeBytes > 0 {
			// 对齐扇区：界面上两位小数的 GB 换算回字节必然带尾数，直接交给 -L 会被 LVM 拒绝，
			// 报 "Size is not a multiple of 512"（见 alignSectorDown）。
			args = append(args, "-L", strconv.FormatInt(alignSectorDown(spec.SizeBytes), 10)+"B")
		} else {
			args = append(args, "-l", "100%FREE")
		}
		args = append(args, "--poolmetadatasize", metaArg, vg)
		if _, err := m.run(ctx, "lvcreate", args...); err != nil {
			return err
		}
		m.logger.Info("已创建 thin pool", "thin_pool", vg+"/"+pool, "size_bytes", spec.SizeBytes,
			"chunksize", chunk, "metadata_size", metaArg)
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

// vgFreeBytes 读取卷组当前剩余空间（字节）。
//
// 读不到时返回 ok=false：宁可放行让 LVM 自己去报错，也不要因为一次读取失败
// 就误拒一个本来合法的请求。"读到了但没读懂"（空值 / 解析失败）同样算读不到——
// 把它当成 0 字节，前置校验就会拒绝一切容量（真机反馈：容量改小也照样超限）。
func (m *Manager) vgFreeBytes(ctx context.Context, vg string) (int64, bool) {
	rows, err := m.vgsRows(ctx)
	if err != nil {
		return 0, false
	}
	for _, r := range rows {
		if strings.TrimSpace(rowStr(r, "vg_name")) == vg {
			return rowIntOK(r, "vg_free")
		}
	}
	return 0, false
}

// poolMaxBytes 该卷组还能新建的 thin pool 数据容量上限（<=0 表示不能新建）。
//
// 与 InitializePool 的前置校验共用同一套预留口径，前端据此给出 max 与提示，
// 避免"前端放行、后端拒绝"的割裂。
func (m *Manager) poolMaxBytes(freeBytes int64, existingPools int) int64 {
	if existingPools > 0 {
		return 0 // 一个卷组只允许一个 thin pool
	}
	return poolMaxBytesWith(freeBytes, m.metadataSize)
}

// firstSharedPath 返回两个设备列表里第一块重复出现的设备（无重叠时返回空串）。
//
// 用于"容量盘不得同时作缓存盘"这类互斥判定：只关心有没有交集，
// 报出来的那一块设备给用户定位用。
func firstSharedPath(a, b []string) string {
	if len(a) == 0 || len(b) == 0 {
		return ""
	}
	seen := make(map[string]struct{}, len(a))
	for _, dev := range a {
		seen[strings.TrimSpace(dev)] = struct{}{}
	}
	for _, dev := range b {
		dev = strings.TrimSpace(dev)
		if _, ok := seen[dev]; ok && dev != "" {
			return dev
		}
	}
	return ""
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
