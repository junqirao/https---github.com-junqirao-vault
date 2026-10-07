package linuxlvm

import "testing"

// 本文件锁住"建池要给元数据留多少"这条口径：它是"自动填好的最大容量"能不能真填进去的前提。
//
// 真机反馈：两块 8G 盘（合计 16G 卷组）建池，前端自动填了 14 GB，提交却报"可用 11.6 GB"。
// 当时三处都不对：
//   - 前端不该自己估（现已改成问后端）；
//   - 后端只扣了**一份**元数据——`lvcreate --type thin-pool` 除了 <pool>_tmeta
//     还会建 VG 级的 <vg>_pmspare（大小与 metadata 相同），两份都从同一个卷组另划；
//   - 元数据大小被配置默认值钉死在 4G：16G 的盘上光元数据就要 4G + 4G = 8G，
//     池几乎无地自容。现在改成按池容量自适应（poolMetaSizeFor）。

// TestPoolMetaSizeForScalesWithPool：元数据随池容量伸缩，且夹在 [64 MiB, 4 GiB]。
func TestPoolMetaSizeForScalesWithPool(t *testing.T) {
	const gb = int64(1) << 30
	cases := []struct {
		name string
		data int64
		want int64
	}{
		{"16G 的盘取到下限 64M（而不是 4G）", 16 * gb, 64 << 20},
		{"100G 的池按 1/500 给元数据", 100 * gb, 100 * gb / 500},
		{"4T 的池取到上限 4G（与老默认值一致）", 4 << 40, 4 * gb},
		{"超大池仍封顶 4G", 100 << 40, 4 * gb},
		{"容量未知时取下限", 0, 64 << 20},
	}
	for _, c := range cases {
		if got := poolMetaSizeFor(c.data); got != c.want {
			t.Fatalf("%s：元数据 = %d，期望 %d", c.name, got, c.want)
		}
	}
}

// TestPoolMetaReserveCountsBothMetadataCopies：预留 = tmeta + pmspare + 10% + 对齐余量。
func TestPoolMetaReserveCountsBothMetadataCopies(t *testing.T) {
	const gb = int64(1) << 30
	explicit := func(meta int64) int64 { return 2*meta + 2*meta/10 + poolAlignSlackBytes }
	if got, want := poolMetaReserveBytes("4G", 16*gb), explicit(4*gb); got != want {
		t.Fatalf("显式 4G 元数据的预留 = %d，期望 %d", got, want)
	}
	// 元数据写得小，预留就小——这正是"把元数据填小些、能多建数据"的依据。
	if got, want := poolMetaReserveBytes("64M", 16*gb), explicit(64<<20); got != want {
		t.Fatalf("显式 64M 元数据的预留 = %d，期望 %d", got, want)
	}
	// 留空 = 自适应：16G 的盘取 64M 元数据，而不是曾经的 4G。
	if got, want := poolMetaReserveBytes("", 16*gb), explicit(64<<20); got != want {
		t.Fatalf("16G 盘的自适应预留 = %d，期望 %d", got, want)
	}
}

// TestPoolMaxBytesWithReservesMetadataBeforeData：上限必须是"扣掉预留之后"的数字，
// 而且扣不出数据时给 0（前端据此不给上限、交给服务端校验，而不是给个负数）。
func TestPoolMaxBytesWithReservesMetadataBeforeData(t *testing.T) {
	const gb = int64(1) << 30
	// 16G 的盘：自适应 64M 元数据，扣掉两份与对齐余量后仍有约 15.8G 可用——
	// 这才是"两块 8G 盘"应有的答案，而不是 11.6G（少扣了一份）或 7G（元数据 4G）。
	want := 16*gb - (2*(64<<20) + 2*(64<<20)/10 + poolAlignSlackBytes)
	if got := poolMaxBytesWith(16*gb, ""); got != want {
		t.Fatalf("自适应上限 = %d，期望 %d", got, want)
	}
	if got := poolMaxBytesWith(16*gb, ""); got < 15*gb {
		t.Fatalf("16G 的盘只给出 %d 字节（不足 15G）：元数据口径又变回大损耗了", got)
	}
	// 显式把元数据写成 4G（老配置的默认值）：真机上就是这么把池逼死的。
	if got := poolMaxBytesWith(16*gb, "4G"); got >= 8*gb {
		t.Fatalf("显式 4G 元数据本应只剩约 7G，实际 %d", got)
	}
	if got := poolMaxBytesWith(4*gb, "4G"); got != 0 {
		t.Fatalf("元数据比可用空间还大时应为 0，实际 %d", got)
	}
	if got := poolMaxBytesWith(0, ""); got != 0 {
		t.Fatalf("空余为 0 时应为 0，实际 %d", got)
	}
}

// 下面两个用例锁住"发命令前必须对齐扇区"这条口径：
// lvcreate / lvextend 的 -L/-V 只接受 512 的整数倍，而界面上两位小数的 GB 换算回字节必然带尾数。
//
// 真机反馈：自动填好的 15.67 GB 提交成 16825534382B，LVM 直接拒绝整条命令——
//
//	Size is not a multiple of 512. Try using 16825533952 or 16825534464.
//	Invalid argument for --size: 16825534382B
func TestAlignSectorDownAlignsUIByteCount(t *testing.T) {
	// 界面换算：两位小数的 15.67 GB × 1GiB，取整后就是真机上那个值。
	// 变量（非常量）才会在运行时做截断转换，所以先落成 float64。
	gbPerGB := float64(int64(1) << 30)
	fromUI := int64(15.67 * gbPerGB)
	if fromUI != 16825534382 {
		t.Fatalf("界面换算值 = %d，期望 16825534382（前提变了，本用例需重看）", fromUI)
	}
	if fromUI%lvmSectorBytes == 0 {
		t.Fatal("这个值本就对齐扇区，用例失去意义")
	}
	if got := alignSectorDown(fromUI); got != 16825533952 {
		t.Fatalf("alignSectorDown(%d) = %d，期望 16825533952", fromUI, got)
	}
}

// TestAlignSectorDownCases：只向下取整（向上会顶破后端刚校验过的上限），非法容量原样透出。
func TestAlignSectorDownCases(t *testing.T) {
	cases := []struct {
		name string
		in   int64
		want int64
	}{
		{"已是 512 的倍数则原样返回", 16825533952, 16825533952},
		{"零头不足一个扇区则归零", 100, 0},
		{"1 TiB 本就对齐", 1 << 40, 1 << 40},
		{"0 原样返回（合法性由调用方的前置校验兜底）", 0, 0},
		{"负数原样返回，不在这里当合法容量", -512, -512},
	}
	for _, c := range cases {
		if got := alignSectorDown(c.in); got != c.want {
			t.Fatalf("%s：alignSectorDown(%d) = %d，期望 %d", c.name, c.in, got, c.want)
		}
	}
	// 无论输入什么，输出都必须真的能被 LVM 接受。
	gbPerGB := float64(int64(1) << 30)
	for _, n := range []int64{int64(15.67 * gbPerGB), 1<<40 + 7, 512*3 + 511} {
		if got := alignSectorDown(n); got%lvmSectorBytes != 0 {
			t.Fatalf("alignSectorDown(%d) = %d，不是 512 的倍数", n, got)
		}
	}
}
