//go:build linux

package linuxlvm

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"vault/internal/apperr"
	"vault/internal/platform"
)

// devPathRe 限定"释放设备"的入参形态：只接受内核块设备路径。
//
// 该值最终会交给 wipefs/pvremove/partprobe 这类破坏性命令。虽然命令一律以参数切片
// 执行（不存在注入），也必须挡住 "/"、"/dev/.." 之类的输入——误擦的代价无法承受。
var devPathRe = regexp.MustCompile(`^/dev/[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ReleaseDevice 把一块盘清成"可被选作卷组设备"的干净状态（实现 platform.DeviceReleaser）。
//
// 动作顺序（与前端文案一致）：
//
//	umount → swapoff → wipefs -a（抹签名）→ pvremove（移出卷组）
//	→ 停用残留 md/dm 映射 → partprobe + udevadm settle（重扫）
//
// 过程性失败（设备忙、命令缺失等）**不返回错误**，而是逐条记进 report.Steps：
// 释放本就允许"部分成功"，逐步回报比一句"释放失败"有用得多。
// 只有入参非法、设备不存在、拒绝操作（系统盘）才返回 error。
func (m *Manager) ReleaseDevice(ctx context.Context, path string) (*platform.DeviceReleaseReport, error) {
	path = strings.TrimSpace(path)
	if !devPathRe.MatchString(path) {
		return nil, apperr.InvalidParam("path")
	}
	nodes, err := m.lsblkTree(ctx)
	if err != nil {
		return nil, err
	}
	disk := findDiskNode(nodes, path)
	if disk == nil {
		return nil, apperr.InvalidParam("path").WithArg("reason", "device_not_found")
	}
	// 系统盘一律不碰：即使调用方漏了校验，这里也要兜住。
	if deviceCarriesSystemMount(disk) {
		return nil, apperr.InvalidParam("path").WithArg("reason", "system_disk")
	}

	// 与其他写操作串行：释放期间不应有 lvcreate/lvremove 并发穿过。
	m.mu.Lock()
	defer m.mu.Unlock()

	pvs := m.pvVGs(ctx)
	rep := &platform.DeviceReleaseReport{Path: path}
	rep.Steps = append(rep.Steps, m.releaseUnmount(ctx, disk)...)
	rep.Steps = append(rep.Steps, m.releaseSwapoff(ctx, disk)...)
	rep.Steps = append(rep.Steps, m.releaseWipe(ctx, disk)...)
	rep.Steps = append(rep.Steps, m.releasePVRemove(ctx, disk, pvs)...)
	rep.Steps = append(rep.Steps, m.releaseHolders(ctx, nodes, path)...)
	rep.Steps = append(rep.Steps, m.releaseRescan(ctx, disk)...)

	// 复核以"重新枚举后的不可用原因"为准，而不是"我们自认为清干净了"：
	// 只有 lsblk 不再给出 reason，才算真的可选。
	if after, err := m.ListBlockDevices(ctx); err == nil {
		for _, d := range after {
			if d.Path == path {
				rep.Released = d.Reason == ""
				rep.Reason = d.Reason
				break
			}
		}
	}
	return rep, nil
}

// releaseUnmount 卸载磁盘及其后代上的一切挂载点。
//
// 常规 umount 失败（有进程占用）时退一步做懒卸载（umount -l）：先解挂、待进程退出后回收。
// 注意 [SWAP] 不是挂载点，umount 管不到，交给 swapoff。
func (m *Manager) releaseUnmount(ctx context.Context, disk *lsblkNode) []platform.DeviceReleaseStep {
	var steps []platform.DeviceReleaseStep
	for _, t := range subtreeTargets(disk) {
		for _, mp := range t.mounts {
			if mp == "" || strings.EqualFold(mp, "[SWAP]") {
				continue
			}
			out, err := m.run(ctx, "umount", mp)
			if err == nil {
				steps = append(steps, platform.DeviceReleaseStep{Step: "umount", Target: mp, OK: true})
				continue
			}
			if lazyOut, lazyErr := m.run(ctx, "umount", "-l", mp); lazyErr == nil {
				steps = append(steps, platform.DeviceReleaseStep{
					Step: "umount", Target: mp, OK: true, Detail: "lazy: " + stepDetail(lazyOut, lazyErr),
				})
				continue
			}
			steps = append(steps, platform.DeviceReleaseStep{
				Step: "umount", Target: mp, Detail: stepDetail(out, err),
			})
		}
	}
	return steps
}

// releaseSwapoff 关闭该盘（或其分区）上启用的 swap。
//
// 以 `swapon --show=NAME` 的活动列表为准：不在列表里的设备直接算跳过，
// 不去执行一条注定报 "Not a swap device" 的命令（那只会污染报告）。
func (m *Manager) releaseSwapoff(ctx context.Context, disk *lsblkNode) []platform.DeviceReleaseStep {
	if _, err := LookPath("swapon"); err != nil {
		return []platform.DeviceReleaseStep{{Step: "swapoff", Skipped: true}}
	}
	out, err := m.run(ctx, "swapon", "--show=NAME", "--noheadings")
	if err != nil {
		return []platform.DeviceReleaseStep{{Step: "swapoff", Skipped: true, Detail: stepDetail(out, err)}}
	}
	active := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			active[name] = true
		}
	}
	var steps []platform.DeviceReleaseStep
	for _, t := range subtreeTargets(disk) {
		if !active[t.path] {
			continue
		}
		out, err := m.run(ctx, "swapoff", t.path)
		if err != nil {
			steps = append(steps, platform.DeviceReleaseStep{
				Step: "swapoff", Target: t.path, Detail: stepDetail(out, err),
			})
			continue
		}
		steps = append(steps, platform.DeviceReleaseStep{Step: "swapoff", Target: t.path, OK: true})
	}
	return steps
}

// releaseWipe 抹掉磁盘与全部分区上的签名（含分区表）。
//
// 分区先于整盘（subtreeTargets 是叶子优先）：先抹整盘的话分区节点会先消失。
func (m *Manager) releaseWipe(ctx context.Context, disk *lsblkNode) []platform.DeviceReleaseStep {
	if _, err := LookPath("wipefs"); err != nil {
		return []platform.DeviceReleaseStep{{Step: "wipefs", Skipped: true}}
	}
	var steps []platform.DeviceReleaseStep
	for _, t := range subtreeTargets(disk) {
		out, err := m.run(ctx, "wipefs", "-a", t.path)
		if err != nil {
			steps = append(steps, platform.DeviceReleaseStep{
				Step: "wipefs", Target: t.path, Detail: stepDetail(out, err),
			})
			continue
		}
		steps = append(steps, platform.DeviceReleaseStep{Step: "wipefs", Target: t.path, OK: true})
	}
	return steps
}

// releasePVRemove 把该盘（或其分区）从所属卷组里移出。
//
// 只有当前确实是 PV 的设备才执行：对非 PV 执行 pvremove 只会得到
// "not a physical volume" 的噪声。标签已被上一步 wipefs 抹掉的情况在这里
// 表现为 pvremove 报错，归为"无需移出"（跳过）而不是失败。
func (m *Manager) releasePVRemove(ctx context.Context, disk *lsblkNode, pvs map[string]string) []platform.DeviceReleaseStep {
	if _, err := LookPath("pvremove"); err != nil {
		return []platform.DeviceReleaseStep{{Step: "pvremove", Skipped: true}}
	}
	var steps []platform.DeviceReleaseStep
	for _, t := range subtreeTargets(disk) {
		if _, ok := pvs[t.path]; !ok {
			continue
		}
		out, err := m.run(ctx, "pvremove", "-f", "-y", t.path)
		if err != nil {
			steps = append(steps, platform.DeviceReleaseStep{
				Step: "pvremove", Target: t.path, Skipped: isNotPV(out), Detail: stepDetail(out, err),
			})
			continue
		}
		steps = append(steps, platform.DeviceReleaseStep{Step: "pvremove", Target: t.path, OK: true})
	}
	return steps
}

// releaseHolders 停用仍然持有该盘的 md / dm 映射。
//
// lsblk 里 md0 / dm-crypt 这类设备是**独立顶层节点**，其子节点才是成员盘——
// 因此不能只顺着目标盘的子树找，必须从整棵树里挑出"子树包含目标盘"的映射节点。
// 顺序按层级从外到内：先摘掉最外层的映射，其下层才可能被停用。
func (m *Manager) releaseHolders(ctx context.Context, nodes []lsblkNode, diskPath string) []platform.DeviceReleaseStep {
	type layered struct {
		node  *lsblkNode
		depth int
	}
	var layers []layered
	var walk func(n *lsblkNode, depth int)
	walk = func(n *lsblkNode, depth int) {
		if n.Type != "disk" && nodePath(n) != diskPath && subtreeContainsPath(n, diskPath) {
			layers = append(layers, layered{node: n, depth: depth})
		}
		for i := range n.Children {
			walk(&n.Children[i], depth+1)
		}
	}
	for i := range nodes {
		walk(&nodes[i], 0)
	}
	if len(layers) == 0 {
		return nil // 没有上层映射：不出步骤，避免报告里多一条无意义的"跳过"
	}
	sort.SliceStable(layers, func(i, j int) bool { return layers[i].depth < layers[j].depth })

	var steps []platform.DeviceReleaseStep
	for _, l := range layers {
		target := nodePath(l.node)
		var tool string
		var args []string
		if strings.HasPrefix(strings.ToLower(l.node.Type), "raid") {
			tool, args = "mdadm", []string{"--stop", target}
		} else {
			// 不用 --retry：它会在映射仍被引用时长时间阻塞重试，这里只需"尽力摘除并如实回报"。
			tool, args = "dmsetup", []string{"remove", target}
		}
		if _, err := LookPath(tool); err != nil {
			steps = append(steps, platform.DeviceReleaseStep{Step: tool, Target: target, Skipped: true})
			continue
		}
		out, err := m.run(ctx, tool, args...)
		if err != nil {
			steps = append(steps, platform.DeviceReleaseStep{Step: tool, Target: target, Detail: stepDetail(out, err)})
			continue
		}
		steps = append(steps, platform.DeviceReleaseStep{Step: tool, Target: target, OK: true})
	}
	return steps
}

// releaseRescan 重扫分区表并等待 udev 收敛，让内核与上层看到"干净"的设备。
//
// 缺 partprobe（属 parted 包，精简系统上可能没有）时不算失败：udevadm settle 就够了。
func (m *Manager) releaseRescan(ctx context.Context, disk *lsblkNode) []platform.DeviceReleaseStep {
	path := nodePath(disk)
	var steps []platform.DeviceReleaseStep
	if _, err := LookPath("partprobe"); err == nil {
		// 上一步 wipefs 已经把签名（含分区表）抹掉了，此时 partprobe 面对一块**无标签**的设备
		// 会打印 `Error: <dev>: unrecognised disk label` 并以非 0 退出——那是"本来就没有表可
		// 重读"，属正常分支，故用 runQuiet；报成"跳过"而不是"失败"，否则一次完全成功的释放
		// 会在报告里挂一条红项（判定套路与 pvremove 的 isNotPV 一致）。
		out, err := m.runQuiet(ctx, "partprobe", path)
		switch {
		case err == nil:
			steps = append(steps, platform.DeviceReleaseStep{Step: "partprobe", Target: path, OK: true})
		case hasNoDiskLabel(out):
			steps = append(steps, platform.DeviceReleaseStep{Step: "partprobe", Target: path, Skipped: true, Detail: stepDetail(out, err)})
		default:
			steps = append(steps, platform.DeviceReleaseStep{Step: "partprobe", Target: path, Detail: stepDetail(out, err)})
		}
	}
	if _, err := LookPath("udevadm"); err == nil {
		out, err := m.run(ctx, "udevadm", "settle")
		if err != nil {
			steps = append(steps, platform.DeviceReleaseStep{Step: "udevadm", Detail: stepDetail(out, err)})
		} else {
			steps = append(steps, platform.DeviceReleaseStep{Step: "udevadm", OK: true})
		}
	}
	return steps
}

// ---- 设备树辅助 ----

// releaseTarget 是释放流程要处理的一个设备节点（盘自身或其分区/子设备）。
type releaseTarget struct {
	path   string
	mounts []string
}

// subtreeTargets 返回设备自身与全部后代，**叶子优先**（分区先于整盘）。
func subtreeTargets(n *lsblkNode) []releaseTarget {
	var out []releaseTarget
	var walk func(node *lsblkNode)
	walk = func(node *lsblkNode) {
		for i := range node.Children {
			walk(&node.Children[i])
		}
		out = append(out, releaseTarget{path: nodePath(node), mounts: node.mountPoints()})
	}
	walk(n)
	return out
}

// nodePath 取节点全路径；个别 lsblk 版本不给 PATH 时按 /dev/<Name> 兜底。
func nodePath(n *lsblkNode) string {
	if n.Path != "" {
		return n.Path
	}
	return "/dev/" + n.Name
}

// findDiskNode 在顶层设备里按全路径找一个整盘节点（找不到返回 nil）。
func findDiskNode(nodes []lsblkNode, path string) *lsblkNode {
	for i := range nodes {
		if !strings.EqualFold(nodes[i].Type, "disk") {
			continue
		}
		if nodePath(&nodes[i]) == path || "/dev/"+nodes[i].Name == path {
			return &nodes[i]
		}
	}
	return nil
}

// subtreeContainsPath 判断某节点的子树里是否有指定设备。
func subtreeContainsPath(n *lsblkNode, path string) bool {
	if nodePath(n) == path {
		return true
	}
	for i := range n.Children {
		if subtreeContainsPath(&n.Children[i], path) {
			return true
		}
	}
	return false
}

// deviceCarriesSystemMount 判断该盘（含分区）是否承载根/引导/swap。
func deviceCarriesSystemMount(n *lsblkNode) bool {
	for _, mp := range n.mountPoints() {
		if isSystemMount(mp) {
			return true
		}
	}
	for i := range n.Children {
		if deviceCarriesSystemMount(&n.Children[i]) {
			return true
		}
	}
	return false
}

// isNotPV 识别"该设备本来就不是物理卷"这类"无需执行"的报错。
func isNotPV(out string) bool {
	low := strings.ToLower(out)
	return strings.Contains(low, "not a physical volume") || strings.Contains(low, "no pv label")
}

// hasNoDiskLabel 识别"设备上本来就没有分区表"这类"无需重扫"的报错。
//
// wipefs 抹掉签名之后，parted/partprobe（libparted）对一块无标签的设备就是这么说的；
// 英式拼写是它的原文，美式只是兜个不同构建的底。
func hasNoDiskLabel(out string) bool {
	low := strings.ToLower(out)
	return strings.Contains(low, "unrecognised disk label") ||
		strings.Contains(low, "unrecognized disk label")
}

// stepDetail 取命令失败时的说明：优先用命令输出，输出为空时退回错误文本。
func stepDetail(out string, err error) string {
	if s := limitText(out); s != "" {
		return s
	}
	return err.Error()
}
