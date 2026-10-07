//go:build linux

package linuxlvm

import (
	"context"
	"os"
	"testing"
)

// 本文件锁住"存储卷还剩多少可用"这条口径（SpaceUsageOf 的 FreeBytes）：
//
// 真机反馈：16G 的存储卷 + 16G 的 thin pool，界面显示"可用 48M"，新建存储库一律报
// storage.low_free_space。原因是当时拿**卷组剩余（vg_free）**当上限，而池建好时就把卷组空间
// 整块划走了，vg_free 只剩 PE 对齐与 pmspare 的零头。正确口径是"这个 thin 卷还能从**池**里
// 写到多少"：池数据容量 × (100 − data_percent)/100。
//
// 判定逻辑经 exec 调用 lvs，因此这里用假的同名脚本来喂输入（见 fakeLVM 的说明）。

// TestThinPoolFreeBytesOf：只有"落在 thin 卷上的 /dev/mapper 设备"才按池算，
// 其余形态一律 ok=false —— 让调用方退回"只看文件系统"，绝不因此报出 0。
func TestThinPoolFreeBytesOf(t *testing.T) {
	cases := []struct {
		name    string
		dev     string
		lvsBody string
		want    int64
		wantOK  bool
	}{
		{
			name:    "thin 卷：按池容量 × 未用比例",
			dev:     "/dev/mapper/data-storages_953370c7_3392_4066_b20c_df90549428f7",
			lvsBody: `printf '%s\n' '{"report":[{"lv":[{"pool_lv":"vault","lv_size":"1000000","data_percent":"25.00"}]}]}'`,
			want:    750000,
			wantOK:  true,
		},
		{
			name:    "厚卷（pool_lv 为空）：没有池可算",
			dev:     "/dev/mapper/data-plain",
			lvsBody: `printf '%s\n' '{"report":[{"lv":[{"pool_lv":"","lv_size":"1000000"}]}]}'`,
			wantOK:  false,
		},
		{
			name:    "不是 mapper 设备：连 lvs 都不该调用",
			dev:     "/dev/sda2",
			lvsBody: `echo '不该被调用' >&2; exit 5`,
			wantOK:  false,
		},
		{
			name:    "lvs 查不到这个卷（已删除 / 权限不足）",
			dev:     "/dev/mapper/data-gone",
			lvsBody: `echo '  Failed to find logical volume "data/gone"' >&2; exit 5`,
			wantOK:  false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFake(t, dir, "lvs", c.lvsBody)
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

			m := New(Options{VG: "data", ThinPool: "vault", Logger: discardLogger()})
			got, ok := m.thinPoolFreeBytesOf(context.Background(), c.dev)
			if ok != c.wantOK {
				t.Fatalf("ok = %v，期望 %v（free = %d）", ok, c.wantOK, got)
			}
			if ok && got != c.want {
				t.Fatalf("池剩余 = %d，期望 %d", got, c.want)
			}
		})
	}
}

// TestSplitMapperRef：ref 形如 /dev/mapper/<vg>-<lv>，只按**第一个**连字符拆
// （本包的 VG/LV 名都不含 '-'，见包注释）；其它形态必须明确报"拆不出来"，
// 否则会把宿主分区当成 LVM 卷去查。
func TestSplitMapperRef(t *testing.T) {
	cases := []struct {
		name string
		dev  string
		vg   string
		lv   string
		ok   bool
	}{
		{"存储卷（名字里全是下划线）", "/dev/mapper/data-storages_953370c7_3392_4066_b20c_df90549428f7", "data", "storages_953370c7_3392_4066_b20c_df90549428f7", true},
		{"普通名字", "/dev/mapper/vg0-vault", "vg0", "vault", true},
		{"宿主分区不是 mapper 设备", "/dev/sda2", "", "", false},
		{"普通目录", "/var/lib/vault", "", "", false},
		{"没有分隔符", "/dev/mapper/data", "", "", false},
		{"分隔符在末尾（LV 名为空）", "/dev/mapper/data-", "", "", false},
		{"VG 名为空", "/dev/mapper/-lv", "", "", false},
		{"空串", "", "", "", false},
	}
	for _, c := range cases {
		vg, lv, ok := splitMapperRef(c.dev)
		if ok != c.ok || vg != c.vg || lv != c.lv {
			t.Fatalf("%s：splitMapperRef(%q) = (%q, %q, %v)，期望 (%q, %q, %v)",
				c.name, c.dev, vg, lv, ok, c.vg, c.lv, c.ok)
		}
	}
}
