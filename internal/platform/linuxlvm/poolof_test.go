//go:build linux

package linuxlvm

import "testing"

// PoolOf 是"差异盘必须与母盘同池"的判定入口（app 层 pickGuardInPool 用它把
// /dev/mapper/<vg>-<lv> 这类引用归类到卷组）。它必须做到两件事：
//
//   - 认得出磁盘引用，并取出卷组（差异盘是 thin 快照，只有同卷组才建得出来）；
//   - 认不出的形态老老实实返回空串 —— 调用方据此退回路径语义（Windows，或目录模式
//     落在宿主文件系统上），绝不猜一个卷组出来把差异盘建到错误的池里。
func TestPoolOfDiskRef(t *testing.T) {
	m := &Manager{}
	cases := []struct {
		ref  string
		want string
	}{
		// 真实现场的形状：母盘 /dev/mapper/test4-disks_parents_<id>_base
		{"/dev/mapper/test4-disks_parents_8bba0282_base", "test4"},
		{"/dev/mapper/data-disks_diffs_8bba0282_0715", "data"},
		{"/dev/mapper/vg0-lv1", "vg0"},
		// 空白与空串。
		{"", ""},
		{"   ", ""},
		// 形态不对：/dev/ 下但不是 device-mapper 引用。
		{"/dev/sda1", ""},
		// 缺少 '-' 分隔（切不出 vg/lv）。
		{"/dev/mapper/bad", ""},
		// 名字里含 '-'：device-mapper 会把它转义成 '--'，无法无歧义反解，只能判不出来
		// （本包创建的 VG/LV 名被 lvNameRe 限制在 [A-Za-z0-9_+.]，不会走到这里）。
		{"/dev/mapper/vg--x-lv", ""},
	}
	for _, c := range cases {
		if got := m.PoolOf(c.ref); got != c.want {
			t.Errorf("PoolOf(%q) = %q，期望 %q", c.ref, got, c.want)
		}
	}
}
