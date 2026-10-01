//go:build linux

package linuxlvm

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
)

// lvsRow 是 lvs/vgs/pvs --reportformat json 输出里的一行。
//
// 之所以用 map 而非强类型结构体：不同 LVM 版本支持的字段集不同，
// 少一个字段不应该让整次解析失败——这里"取不到的字段留零值"。
type lvsRow map[string]any

// parseReportRows 解析 LVM 的 --reportformat json 输出，返回指定 section（"lv"/"vg"/"pv"）的行。
func parseReportRows(out, section string) ([]lvsRow, error) {
	var doc struct {
		Report []map[string][]lvsRow `json:"report"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return nil, err
	}
	var rows []lvsRow
	for _, r := range doc.Report {
		rows = append(rows, r[section]...)
	}
	return rows, nil
}

// lvsRows 执行 lvs 并返回其 JSON 报告的行集合（args 为 lvs 的其余参数）。
func (m *Manager) lvsRows(ctx context.Context, args ...string) ([]lvsRow, error) {
	full := append([]string{"--reportformat", "json"}, args...)
	out, err := m.run(ctx, "lvs", full...)
	if err != nil {
		return nil, err
	}
	return parseReportRows(out, "lv")
}

// vgsRows 读取全部卷组（名称 + 总容量 + 剩余容量）。
func (m *Manager) vgsRows(ctx context.Context) ([]lvsRow, error) {
	out, err := m.run(ctx, "vgs", "--reportformat", "json", "-o", "vg_name,vg_size,vg_free")
	if err != nil {
		return nil, err
	}
	return parseReportRows(out, "vg")
}

// pvsRows 读取全部物理卷及其所属卷组。
func (m *Manager) pvsRows(ctx context.Context) ([]lvsRow, error) {
	out, err := m.run(ctx, "pvs", "--reportformat", "json", "-o", "pv_name,vg_name")
	if err != nil {
		return nil, err
	}
	return parseReportRows(out, "pv")
}

// pvVG 查询某设备是否为 PV 及其所属 VG；第二个返回值为是否为 PV。
// 是 PV 但 vg 为空串表示"尚未加入任何 VG"。
func (m *Manager) pvVG(ctx context.Context, dev string) (string, bool) {
	rows, err := m.pvsRows(ctx)
	if err != nil {
		return "", false
	}
	for _, r := range rows {
		if strings.TrimSpace(rowStr(r, "pv_name")) == dev {
			return strings.TrimSpace(rowStr(r, "vg_name")), true
		}
	}
	return "", false
}

// blkidType 探测设备上已有的文件系统类型；无文件系统时 blkid 以非 0 退出，这里归为"空"。
func (m *Manager) blkidType(ctx context.Context, dev string) (string, error) {
	out, err := m.run(ctx, "blkid", "-o", "value", "-s", "TYPE", dev)
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(out), nil
}

// ---- JSON 字段取值辅助（容忍字符串/数字/Bool 三种编码） ----

func rowStr(r lvsRow, key string) string {
	if v, ok := r[key]; ok {
		return scalarStr(v)
	}
	return ""
}

func rowFloat(r lvsRow, key string) float64 {
	s := strings.TrimSpace(rowStr(r, key))
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}

func rowInt(r lvsRow, key string) int64 {
	s := strings.TrimSpace(rowStr(r, key))
	if s == "" {
		return 0
	}
	// JSON 里的数字会被解码成 float64，用 ParseFloat 再取整可兼容 "1.07e+09" 这类科学计数。
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int64(f)
}

func scalarStr(v any) string {
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
