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
//
// 容量必须显式要字节：LVM 报告的默认形态是 "12.00g" 这类带单位后缀的人类可读串，
// 直接 ParseFloat 会失败并退化成 0——"读到了"却被当成"0 字节可用"，
// 于是容量前置校验变成"填什么容量都超限"（真机反馈：容量改小照样报超限）。
func (m *Manager) vgsRows(ctx context.Context) ([]lvsRow, error) {
	out, err := m.run(ctx, "vgs", "--reportformat", "json", "--units", "b", "--nosuffix",
		"-o", "vg_name,vg_size,vg_free")
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
	v, _ := rowIntOK(r, key)
	return v
}

// rowIntOK 与 rowInt 同源，但把"字段缺失 / 解析不出来"如实报成 ok=false。
//
// 容量类字段必须用它：读不到就要能区分"确实是 0 字节"和"根本没读懂"，
// 否则一次格式变化就会被当成 0，把合法请求误判成"容量超限"。
func rowIntOK(r lvsRow, key string) (int64, bool) {
	s := strings.TrimSpace(rowStr(r, key))
	if s == "" {
		return 0, false
	}
	// JSON 里的数字会被解码成 float64，用 ParseFloat 再取整可兼容 "1.07e+09" 这类科学计数。
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return int64(f), true
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
