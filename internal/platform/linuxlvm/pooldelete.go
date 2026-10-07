//go:build linux

package linuxlvm

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"vault/internal/apperr"
	"vault/internal/platform"
)

// DeletePool 删除存储池，可选连带删除卷组并把物理卷清回"可选"状态（实现 platform.PoolAdmin）。
//
// 破坏性说明：池里的 thin 卷正是各存储的底层卷、母盘与差异盘，删池就等于删它们。
// 因此本方法**先验后做**——所有拒绝条件都在动手之前判完，且拒绝理由带上"还剩什么、有多少"，
// 让用户知道下一步该删什么（这正是"没有管理入口、只能去敲 LVM 命令"要解决的痛点）：
//
//   - 默认池（配置 platform.lvm.vg/thin_pool）→ pool_protected.default_pool：
//     它是服务端所有建库/建存储的落脚点，删掉服务随即不可用，要删必须先改配置；
//   - 物理卷（含分区）上挂着根/引导/swap 的卷组 → pool_protected.system_vg：
//     "非系统盘"是这个功能的边界；
//   - 池里还有 thin 卷 → pool_in_use.thin_volumes（带数量与卷名）：
//     不替用户 `lvremove -f` 强删——删存储/删盘要先卸载、下线 LUN、清 LIO backstore，
//     那些收尾只有应用层做得到（同 thin.go 里 Delete 的说明）；
//   - 要连卷组删、而卷组里还有别的逻辑卷 → pool_in_use.volume_group。
//
// 锁只覆盖真正改元数据的命令：前面的读探测（lsblk/pvs/lvs）不需要锁，
// 而 vgs/lvs 都带超时，长时间持锁会拖住建库路径。
func (m *Manager) DeletePool(ctx context.Context, vg, thinPool string, opts platform.PoolDeleteOptions) (*platform.PoolDeleteReport, error) {
	vg, thinPool = strings.TrimSpace(vg), strings.TrimSpace(thinPool)
	if !lvNameRe.MatchString(vg) {
		return nil, apperr.InvalidParam("vg")
	}
	if thinPool != "" && !lvNameRe.MatchString(thinPool) {
		return nil, apperr.InvalidParam("thin_pool")
	}
	if thinPool == "" && !opts.RemoveVolumeGroup {
		// 只给卷组名、又不许删卷组：没有任何可执行的动作。拒绝比"静默成功"诚实——
		// 静默成功会让用户以为池没了，回头发现池还在。
		return nil, apperr.InvalidParam("thin_pool")
	}
	if vg == m.vg && (thinPool == "" || thinPool == m.thinPool) {
		return nil, apperr.PoolProtected("default_pool").
			WithArg("vg", vg).WithArg("thin_pool", thinPool)
	}

	if !m.vgExists(ctx, vg) {
		return nil, apperr.PoolNotFound(vg, thinPool)
	}
	if thinPool != "" && !m.poolExists(ctx, vg, thinPool) {
		return nil, apperr.PoolNotFound(vg, thinPool)
	}

	// 系统卷组：卷组的物理卷（含其分区）承载 / /boot swap 时一律不碰。
	// 判定复用"释放设备"那套（deviceCarriesSystemMount/isSystemMount），否则会出现
	// "某块盘能释放、但它所在的池却删得掉"这种自相矛盾的口径。
	pvPaths, err := m.vgPVPaths(ctx, vg)
	if err != nil {
		return nil, err
	}
	if mounts := m.systemMountsOnDisks(ctx, pvPaths); len(mounts) > 0 {
		return nil, apperr.PoolProtected("system_vg").
			WithArg("vg", vg).WithArg("mounts", strings.Join(mounts, ", "))
	}

	rep := &platform.PoolDeleteReport{VG: vg, ThinPool: thinPool}

	// ---- 前置校验（都不动元数据）----
	if thinPool != "" {
		vols, err := m.thinVolumesInPool(ctx, vg, thinPool)
		if err != nil {
			return nil, err
		}
		if len(vols) > 0 {
			return nil, apperr.PoolInUse("thin_volumes").
				WithArg("count", len(vols)).
				WithArg("names", joinLimit(vols))
		}
	}
	if opts.RemoveVolumeGroup {
		// 这一步必须在删池之前：否则会留下"池删了、卷组却因还有别的卷删不掉"的半截状态。
		lvs, err := m.contentLVsOfVG(ctx, vg, thinPool)
		if err != nil {
			return nil, err
		}
		if len(lvs) > 0 {
			return nil, apperr.PoolInUse("volume_group").
				WithArg("vg", vg).WithArg("names", joinLimit(lvs))
		}
	}

	// ---- 校验通过，开始动手 ----
	if thinPool != "" {
		m.mu.Lock()
		_, err := m.run(ctx, "lvremove", "-y", vg+"/"+thinPool)
		m.mu.Unlock()
		if err != nil {
			return nil, err
		}
		rep.Steps = append(rep.Steps, platform.DeviceReleaseStep{
			Step: "lvremove", Target: vg + "/" + thinPool, OK: true,
		})
		m.logger.Info("已删除存储池", "vg", vg, "thin_pool", thinPool)
	}
	if !opts.RemoveVolumeGroup {
		return rep, nil
	}

	m.mu.Lock()
	_, err = m.run(ctx, "vgremove", "-y", vg)
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	rep.RemovedVolumeGroup = true
	rep.Steps = append(rep.Steps, platform.DeviceReleaseStep{Step: "vgremove", Target: vg, OK: true})
	m.logger.Info("已删除卷组", "vg", vg, "devices", pvPaths)

	if opts.ReleaseDevices {
		rep.ReleasedDevices, rep.Steps = m.releasePoolPVs(ctx, pvPaths, rep.Steps)
	}
	return rep, nil
}

// releasePoolPVs 把卷组原来的物理卷标签清掉，让磁盘回到"可再次被选作卷组设备"。
//
// 只做 pvremove，不做 wipefs：卷组已经删了，盘上残留的 LVM 痕迹就是 PV 标签本身；
// 再深的清理（分区表、文件系统签名）属于"释放设备"的职责，那里有逐步骤回报，
// 用户能看清到底动了什么——两处混在一起会让"删池"这种高频操作变得不可预期。
func (m *Manager) releasePoolPVs(ctx context.Context, pvPaths []string, steps []platform.DeviceReleaseStep) ([]string, []platform.DeviceReleaseStep) {
	if len(pvPaths) == 0 {
		return nil, steps
	}
	if _, err := LookPath("pvremove"); err != nil {
		steps = append(steps, platform.DeviceReleaseStep{
			Step: "pvremove", Skipped: true, Detail: "缺少 pvremove（未安装 lvm2？），磁盘仍带 PV 标签",
		})
		return nil, steps
	}
	var freed []string
	for _, p := range pvPaths {
		out, err := m.run(ctx, "pvremove", "-f", "-y", p)
		switch {
		case err == nil:
			freed = append(freed, p)
			steps = append(steps, platform.DeviceReleaseStep{Step: "pvremove", Target: p, OK: true})
		case isNotPV(out):
			// 标签已随 vgremove 一起消失（LVM 有时会自己清）：算释放成功，不该报失败。
			freed = append(freed, p)
			steps = append(steps, platform.DeviceReleaseStep{
				Step: "pvremove", Target: p, Skipped: true, Detail: "已经不再是 PV",
			})
		default:
			steps = append(steps, platform.DeviceReleaseStep{
				Step: "pvremove", Target: p, Detail: stepDetail(out, err),
			})
		}
	}
	if _, err := LookPath("udevadm"); err == nil {
		out, err := m.run(ctx, "udevadm", "settle")
		if err != nil {
			steps = append(steps, platform.DeviceReleaseStep{
				Step: "udevadm", Detail: stepDetail(out, err),
			})
		} else {
			steps = append(steps, platform.DeviceReleaseStep{Step: "udevadm", OK: true})
		}
	}
	return freed, steps
}

// vgPVPaths 返回卷组的全部物理卷路径（排序，保证报告与日志稳定可读）。
func (m *Manager) vgPVPaths(ctx context.Context, vg string) ([]string, error) {
	rows, err := m.pvsRows(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rows {
		if strings.TrimSpace(rowStr(r, "vg_name")) != vg {
			continue
		}
		if p := strings.TrimSpace(rowStr(r, "pv_name")); p != "" {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}

// systemMountsOnDisks 返回这些设备（含其分区）上的系统挂载点（/、/boot、swap）。
//
// 读不到设备树时返回空（放行）：本方法是"多一道保险"，不是唯一防线——
// 真正的边界在应用层（默认池、存储引用），而误报会把"想删自己建的池"变成不可能。
func (m *Manager) systemMountsOnDisks(ctx context.Context, paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	nodes, err := m.lsblkTree(ctx)
	if err != nil {
		m.logger.Warn("lsblk 失败，无法确认卷组是否含系统盘", "err", err.Error())
		return nil
	}
	var out []string
	for _, p := range paths {
		if n := findNodeByPath(nodes, p); n != nil {
			out = append(out, systemMountsOf(n)...)
		}
	}
	sort.Strings(out)
	return out
}

// systemMountsOf 收集该节点及其后代上的系统挂载点。
func systemMountsOf(n *lsblkNode) []string {
	var out []string
	for _, mp := range n.mountPoints() {
		if isSystemMount(mp) {
			out = append(out, mp)
		}
	}
	for i := range n.Children {
		out = append(out, systemMountsOf(&n.Children[i])...)
	}
	return out
}

// findNodeByPath 在设备树里按全路径找任意节点（整盘或分区）。
//
// 与 blockdev.go 的整盘遍历不同：物理卷既可能是整盘（/dev/sdb）也可能是分区（/dev/sdb1），
// 判断"是否系统盘"必须两种都能命中。
func findNodeByPath(nodes []lsblkNode, path string) *lsblkNode {
	for i := range nodes {
		if n := findNodeInSubtree(&nodes[i], path); n != nil {
			return n
		}
	}
	return nil
}

func findNodeInSubtree(n *lsblkNode, path string) *lsblkNode {
	if nodePath(n) == path {
		return n
	}
	for i := range n.Children {
		if found := findNodeInSubtree(&n.Children[i], path); found != nil {
			return found
		}
	}
	return nil
}

// thinVolumesInPool 返回池里的 thin 卷名（= 存储底层卷、母盘、差异盘）。
//
// 判定是 lv_attr 首字符 'V'（thin 卷）**且** pool_lv 指向该池：
// 池自身的 '_tdata/_tmeta' 首字符是 't'，据此不会被误判成"池里还有内容"，
// 否则删任何一个空池都会失败（那种错法最难查：明明空着却说不空）。
func (m *Manager) thinVolumesInPool(ctx context.Context, vg, pool string) ([]string, error) {
	rows, err := m.lvsRows(ctx, "-o", "lv_name,lv_attr,pool_lv", vg)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rows {
		if !strings.HasPrefix(strings.TrimSpace(rowStr(r, "lv_attr")), "V") {
			continue
		}
		if strings.TrimSpace(rowStr(r, "pool_lv")) != pool {
			continue
		}
		if name := strings.TrimSpace(rowStr(r, "lv_name")); name != "" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// contentLVsOfVG 返回"删掉这个池之后，卷组里仍属于用户内容的逻辑卷"。
//
// 排除的是**会随这次删除一起消失的卷**，而不是"看起来像内部卷的名字"：
//
//   - 池自己的 LV（pool）：它就是本次要 lvremove 的目标，判据又是在动手之前跑的，
//     不计入的话"删池 + 删卷组"将永远被拒（池明明是自己要删的，却被当成还有内容）；
//   - 池的 _tdata/_tmeta：随池消失；
//   - _pmspare：LVM 给卷组留的元数据备用卷，随卷组消失。
//
// 若把它们计入，删空卷组永远失败——"池空着、卷组里也没别的东西，却报还有内容"
// 是最难查的一类错法。（不带 -a 的 lvs 本就不列隐藏卷，这里再兜一层，防老版本行为差异。）
func (m *Manager) contentLVsOfVG(ctx context.Context, vg, pool string) ([]string, error) {
	rows, err := m.lvsRows(ctx, "-o", "lv_name", vg)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rows {
		name := strings.TrimSpace(rowStr(r, "lv_name"))
		if name == "" || goesWithPool(name, pool) {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// goesWithPool 判断某个逻辑卷是否会随"删除这个池（含卷组）"一起消失。
//
// 名字规则由 LVM 保证（见其 *_{tdata,tmeta,pmspare} 约定）；pool 为空表示本次只删卷组，
// 此时池相关的排除项不适用——卷组里真住着别的池，正是该报出来的内容。
func goesWithPool(name, pool string) bool {
	if strings.HasSuffix(name, "_pmspare") {
		return true
	}
	if pool == "" {
		return false
	}
	return name == pool || name == pool+"_tdata" || name == pool+"_tmeta"
}

// poolNameLimit 是错误参数里最多带几个卷名。
//
// 池里可能有上百个差异盘，把名字全塞进响应既没意义（用户不会逐个看）又难读；
// 给前几个就足够定位，真实数量由 count/names 末尾的"等 N 个"体现。
const poolNameLimit = 3

// joinLimit 把卷名拼成"前几个 + 等 N 个"，用于错误参数与日志。
func joinLimit(names []string) string {
	if len(names) <= poolNameLimit {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s 等 %d 个", strings.Join(names[:poolNameLimit], ", "), len(names))
}
