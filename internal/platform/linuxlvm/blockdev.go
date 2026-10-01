//go:build linux

package linuxlvm

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"vault/internal/platform"
)

// lsblkNode 是 lsblk --json 输出的一个块设备节点。
//
// 字段刻意用 any/[]any 承载：ROTA、MOUNTPOINTS 等列在部分发行版/旧版 util-linux 上不存在，
// 声明为具体类型会因缺字段或类型差异导致整体解析失败；用 any 则"取不到就留零值"。
type lsblkNode struct {
	Name        string      `json:"name"`
	Path        string      `json:"path"`
	Type        string      `json:"type"`
	Size        any         `json:"size"`
	Rota        any         `json:"rota"`
	Model       any         `json:"model"`
	Mountpoints []any       `json:"mountpoints"`
	Mountpoint  any         `json:"mountpoint"`
	FSType      any         `json:"fstype"`
	Children    []lsblkNode `json:"children"`
}

// ListBlockDevices 枚举块设备供"缓存设备选择器"使用（仅 TYPE=="disk" 的顶层设备）。
//
// 分区不作为独立候选，但其挂载/文件系统/PV 信息会汇总到所属磁盘上，并计入 Partitions。
func (m *Manager) ListBlockDevices(ctx context.Context) ([]platform.BlockDevice, error) {
	nodes, err := m.lsblkTree(ctx)
	if err != nil {
		return nil, err
	}
	pvs := m.pvVGs(ctx)
	out := make([]platform.BlockDevice, 0, len(nodes))
	for i := range nodes {
		if !strings.EqualFold(nodes[i].Type, "disk") {
			continue
		}
		out = append(out, m.toBlockDevice(&nodes[i], pvs))
	}
	// 可选的 SSD（非机械、非系统盘）排前面，方便前端默认展示。
	sort.SliceStable(out, func(i, j int) bool {
		return deviceRank(out[i]) < deviceRank(out[j])
	})
	return out, nil
}

func deviceRank(d platform.BlockDevice) int {
	switch {
	case !d.Rotational && !d.System:
		return 0
	case !d.Rotational:
		return 1
	default:
		return 2
	}
}

// lsblkTree 执行 lsblk 并解析成设备树。
//
// 完整列集在个别实现上会被拒（未知列），此时退化为最小列集，对应字段留零值。
func (m *Manager) lsblkTree(ctx context.Context) ([]lsblkNode, error) {
	const full = "NAME,PATH,TYPE,SIZE,ROTA,MODEL,MOUNTPOINTS,FSTYPE"
	out, err := m.run(ctx, "lsblk", "--json", "-b", "-o", full)
	if err != nil {
		m.logger.Warn("lsblk 完整列集失败，退化为最小列集", "err", err.Error())
		out, err = m.run(ctx, "lsblk", "--json", "-b", "-o", "NAME,PATH,TYPE,SIZE,FSTYPE")
		if err != nil {
			return nil, err
		}
	}
	var doc struct {
		Blockdevices []lsblkNode `json:"blockdevices"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return nil, err
	}
	return doc.Blockdevices, nil
}

// pvVGs 返回 pv 设备路径 → 所属 VG 名（未加入 VG 的映射到空串）。
func (m *Manager) pvVGs(ctx context.Context) map[string]string {
	res := make(map[string]string)
	rows, err := m.pvsRows(ctx)
	if err != nil {
		m.logger.Warn("pvs 查询失败，PV/VG 信息留空", "err", err.Error())
		return res
	}
	for _, r := range rows {
		name := strings.TrimSpace(rowStr(r, "pv_name"))
		if name == "" {
			continue
		}
		res[name] = strings.TrimSpace(rowStr(r, "vg_name"))
	}
	return res
}

// toBlockDevice 汇总一块磁盘（含其全部子分区）的信息并判定可用性。
func (m *Manager) toBlockDevice(n *lsblkNode, pvs map[string]string) platform.BlockDevice {
	d := platform.BlockDevice{
		Name:       n.Name,
		Path:       n.Path,
		SizeBytes:  anyInt64(n.Size),
		Model:      anyStr(n.Model),
		Rotational: anyBool(n.Rota),
	}
	var parts int
	sys := false
	hasFS := false
	hasPV := false
	vg := ""
	var walk func(node *lsblkNode)
	walk = func(node *lsblkNode) {
		for _, mp := range node.mountPoints() {
			if isSystemMount(mp) {
				sys = true
			}
		}
		if anyStr(node.FSType) != "" {
			hasFS = true
		}
		if node.Type == "part" {
			parts++
		}
		if v, ok := pvs[node.Path]; ok {
			hasPV = true
			if vg == "" {
				vg = v
			}
		}
		for i := range node.Children {
			walk(&node.Children[i])
		}
	}
	walk(n)
	d.Partitions = parts
	d.System = sys
	d.HasFS = hasFS
	d.HasPV = hasPV
	d.VGName = vg
	d.Reason = unavailableReason(d, m.vg)
	return d
}

func (n *lsblkNode) mountPoints() []string {
	var out []string
	for _, mp := range n.Mountpoints {
		if s := anyStr(mp); s != "" {
			out = append(out, s)
		}
	}
	if s := anyStr(n.Mountpoint); s != "" { // 兼容只有单数 MOUNTPOINT 列的旧版
		out = append(out, s)
	}
	return out
}

// isSystemMount 判定承载根/引导/swap 的挂载点。
func isSystemMount(mp string) bool {
	switch mp {
	case "/", "/boot", "/boot/efi", "[SWAP]":
		return true
	}
	return strings.HasPrefix(mp, "/boot/")
}

// unavailableReason 按优先级给出不可用原因：系统盘 > 已挂载/有文件系统 > 属于其它 VG > 有分区。
func unavailableReason(d platform.BlockDevice, targetVG string) string {
	switch {
	case d.System:
		return "系统盘（承载根/引导/swap）"
	case d.HasFS:
		return "已挂载或已存在文件系统"
	case d.HasPV && d.VGName != "" && d.VGName != targetVG:
		return "已是卷组 " + d.VGName + " 的成员"
	case d.Partitions > 0:
		return "存在子分区"
	}
	return ""
}

// ---- any 取值辅助 ----

func anyStr(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	default:
		return ""
	}
}

func anyBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		b, _ := strconv.ParseBool(t)
		return b
	case float64:
		return t != 0
	default:
		return false
	}
}

func anyInt64(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case json.Number:
		n, _ := t.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		return n
	default:
		return 0
	}
}
