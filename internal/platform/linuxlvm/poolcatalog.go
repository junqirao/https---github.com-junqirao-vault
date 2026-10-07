//go:build linux

package linuxlvm

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"vault/internal/apperr"
	"vault/internal/platform"
)

// 本文件实现"多存储池"：一台机器上可以有多个卷组（各自一个 thin pool），
// 每个存储（storages.path）落在其中一个池上。
//
// 关键约束（务必先读再改）：
//
//	一个卷组只允许一个 thin pool。
//
// 原因是**磁盘引用里只有 VG、没有池名**（ref = /dev/mapper/<vg>-<lv>）：
// 磁盘的 thin pool 只能由 VG 反查（resolvePool）得到唯一答案。
// 允许同 VG 建第二个池会让"这块盘落在哪个池"失去唯一解，因此 InitializePool 显式拒绝
// （同池重复初始化仍然是幂等跳过）。

// DefaultPool 返回后端配置的默认存储池（VG + thin pool）。
func (m *Manager) DefaultPool() (vg, thinPool string) {
	return strings.TrimSpace(m.vg), strings.TrimSpace(m.thinPool)
}

// PoolStatusOf 查询指定池的现状；池（或 VG）不存在时返回 Exists=false 而非错误。
func (m *Manager) PoolStatusOf(ctx context.Context, vg, thinPool string) (*platform.PoolStatus, error) {
	vg, thinPool = strings.TrimSpace(vg), strings.TrimSpace(thinPool)
	if !lvNameRe.MatchString(vg) {
		return nil, apperr.InvalidParam("vg")
	}
	if !lvNameRe.MatchString(thinPool) {
		return nil, apperr.InvalidParam("thin_pool")
	}
	st, err := m.poolStatus(ctx, vg, thinPool)
	if err != nil {
		return nil, err
	}
	st.Default = vg == m.vg && thinPool == m.thinPool
	return st, nil
}

// ListPools 列出全部存储池（每个 VG 的 thin pool 一项），默认池排在最前。
func (m *Manager) ListPools(ctx context.Context) ([]platform.PoolStatus, error) {
	groups, err := m.ListVolumeGroups(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]platform.PoolStatus, 0, len(groups))
	for _, g := range groups {
		for _, pool := range g.ThinPools {
			st, err := m.poolStatus(ctx, g.Name, pool)
			if err != nil {
				// 单个池读不到不应让整个列表失败（与 lvsAllRows 的容错口径一致）。
				m.logger.Warn("读取存储池状态失败，跳过该池",
					"vg", g.Name, "thin_pool", pool, "err", err.Error())
				continue
			}
			out = append(out, *st)
		}
	}
	// 默认池排最前：前端拿它作为"选择或新建存储池"的预选项。
	sort.SliceStable(out, func(i, j int) bool { return out[i].Default && !out[j].Default })
	return out, nil
}

// ListVolumeGroups 列出全部卷组及其中的 thin pool（供"在已有卷组里新建池 / 新建卷组"选择）。
func (m *Manager) ListVolumeGroups(ctx context.Context) ([]platform.VolumeGroup, error) {
	vgRows, err := m.vgsRows(ctx)
	if err != nil {
		return nil, err
	}
	pools, err := m.thinPoolsOf(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]platform.VolumeGroup, 0, len(vgRows))
	for _, r := range vgRows {
		name := strings.TrimSpace(rowStr(r, "vg_name"))
		if name == "" {
			continue
		}
		free := rowInt(r, "vg_free")
		out = append(out, platform.VolumeGroup{
			Name:      name,
			SizeBytes: rowInt(r, "vg_size"),
			FreeBytes: free,
			PVCount:   int(rowInt(r, "pv_count")),
			ThinPools: pools[name],
			// 前端建池容量输入框的 max：与 InitializePool 的前置校验同口径。
			PoolMaxBytes: m.poolMaxBytes(free, len(pools[name])),
		})
	}
	return out, nil
}

// thinPoolsOf 返回"卷组 → 该组里的 thin pool 名"（跨全部卷组，一次 lvs）。
//
// 判定依据是 lv_attr 首字符：'t' = thin pool，'V' = thin 卷（大写 V，别混）。
// 因此不需要 -a，内部子 LV 也不会被列进来。
func (m *Manager) thinPoolsOf(ctx context.Context) (map[string][]string, error) {
	rows, err := m.lvsRows(ctx, "-o", "vg_name,lv_name,lv_attr")
	if err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(rows))
	for _, r := range rows {
		vg := strings.TrimSpace(rowStr(r, "vg_name"))
		name := strings.TrimSpace(rowStr(r, "lv_name"))
		if vg == "" || name == "" {
			continue
		}
		if !strings.HasPrefix(strings.TrimSpace(rowStr(r, "lv_attr")), "t") {
			continue
		}
		out[vg] = append(out[vg], name)
	}
	return out, nil
}

// thinPoolsOfVG 返回单个卷组里的 thin pool 名（按 VG 收窄查询，比全量扫描便宜）。
func (m *Manager) thinPoolsOfVG(ctx context.Context, vg string) []string {
	rows, err := m.lvsRows(ctx, "-o", "lv_name,lv_attr", vg)
	if err != nil {
		return nil
	}
	var out []string
	for _, r := range rows {
		if !strings.HasPrefix(strings.TrimSpace(rowStr(r, "lv_attr")), "t") {
			continue
		}
		if name := strings.TrimSpace(rowStr(r, "lv_name")); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// resolvePool 返回某卷组上的 thin pool 名（磁盘引用只带 VG，池必须由 VG 反查）。
//
// 回退口径（重要）：判定不出来时**回退到配置的池名**，保持"读不到就交给 lvcreate 兜底"
// 的软失败语义（见 checkWatermark 注释）——绝不在这里猜一个会误报"池不存在"的答案：
//   - lvs 读不到（权限/老版本）：回退配置池名；
//   - 该 VG 里一个 thin pool 都没有：回退配置池名（由 requirePool / lvcreate 收场）；
//   - 有多个（人为绕过 InitializePool 建的第二池）：告警后回退配置池名。
func (m *Manager) resolvePool(ctx context.Context, vg string) string {
	pools := m.thinPoolsOfVG(ctx, vg)
	switch len(pools) {
	case 1:
		return pools[0]
	case 0:
		return strings.TrimSpace(m.thinPool)
	default:
		m.logger.Warn("卷组内有多个 thin pool，无法判定该用哪个，回退配置池名",
			"vg", vg, "pools", pools, "fallback", m.thinPool)
		return strings.TrimSpace(m.thinPool)
	}
}

// requirePool 校验池确实存在（VG + thin pool），否则给出可执行的 pool_missing 错误。
//
// 判不出来（例如 vgs/lvs 自己都读不到）时**不报错**：那种情况下报"池不存在"会把运维
// 引向错误方向，留给后续 lvcreate 兜底（与 poolMissingKind 的口径一致）。
func (m *Manager) requirePool(ctx context.Context, vg, pool string) error {
	if _, err := poolPath(vg, pool); err != nil {
		return err
	}
	if m.poolExists(ctx, vg, pool) {
		return nil
	}
	kind := m.poolMissingKind(ctx, vg, pool)
	if kind == "" {
		return nil
	}
	m.logger.Error("存储池不存在，拒绝创建（请先初始化存储池）",
		"vg", vg, "thin_pool", pool, "missing", kind)
	return apperr.New(CodePoolMissing, http.StatusServiceUnavailable).
		WithArg("vg", vg).
		WithArg("thin_pool", pool)
}

// storagePoolFor 决定一个存储卷该用哪个 thin pool。
//
//   - poolRef 非空（用户在创建存储时选/建了池）：校验键合法、VG 与 ref 一致（不允许跨池建卷）、
//     池确实存在；
//   - poolRef 为空（历史数据 / 目录模式）：按 ref 的 VG 反查，并要求池存在。
func (m *Manager) storagePoolFor(ctx context.Context, vg, poolRef string) (string, error) {
	if key := strings.TrimSpace(poolRef); key != "" {
		pvg, ppool, err := platform.SplitPoolKey(key)
		if err != nil {
			return "", apperr.InvalidParam("pool_ref")
		}
		if pvg != vg {
			return "", apperr.InvalidParam("pool_ref")
		}
		if err := m.requirePool(ctx, pvg, ppool); err != nil {
			return "", err
		}
		return ppool, nil
	}
	pool := m.resolvePool(ctx, vg)
	if err := m.requirePool(ctx, vg, pool); err != nil {
		return "", err
	}
	return pool, nil
}

// checkSameVGPool 拒绝在同一卷组里建第二个 thin pool（见本文件顶部约束说明）。
func (m *Manager) checkSameVGPool(ctx context.Context, vg, pool string) error {
	others := m.thinPoolsOfVG(ctx, vg)
	for _, o := range others {
		if o != pool {
			return apperr.New(apperr.CodeConflict, http.StatusConflict).
				WithArg("vg", vg).
				WithArg("thin_pool", o).
				WithArg("requested", pool)
		}
	}
	return nil
}
