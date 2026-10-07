//go:build linux

package linuxlvm

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vault/internal/apperr"
)

// 本文件锁住"建盘布局"的三条底线（用假命令喂输入，无需真机 LVM）：
//
//  1. 空盘 → GPT 分区表 + 单分区，分区的**类型**按目标文件系统写（客户端据此识别分区）；
//  2. 已有分区表/分区的盘 → 只读复用，**绝不 mklabel**（重写分区表会抹掉分区里的数据）；
//  3. 判不准的盘（设备看不见、探不到表却有子设备）→ 失败退出，绝不赌"它大概是个空盘"。
const (
	// layoutDev / layoutPartDev 是布局用例里的整盘 LV 与它的分区（取 lsblk 报的全路径）。
	layoutDev     = "/dev/mapper/vg0-lv"
	layoutPartDev = "/dev/mapper/vg0-lv1"
)

// 盘上没有任何分区（空盘，或升级前的整盘布局）。
const layoutTreeNoPart = `{"blockdevices":[{"name":"dm-3","path":"/dev/mapper/vg0-lv","type":"lvm","size":10737418240,"mountpoints":[],"fstype":null,"children":[]}]}`

// 盘上已有分区（GPT 布局建好之后的样子）。
const layoutTreeWithPart = `{"blockdevices":[{"name":"dm-3","path":"/dev/mapper/vg0-lv","type":"lvm","size":10737418240,"mountpoints":[],"fstype":null,"children":[{"name":"dm-4","path":"/dev/mapper/vg0-lv1","type":"part","size":10735304704,"fstype":"ntfs","mountpoints":[]}]}]}`

// 设备根本不在 lsblk 树里（映射未建立/已停用）：无法判断它是不是空盘。
const layoutTreeInvisible = `{"blockdevices":[{"name":"sda","path":"/dev/sda","type":"disk","size":1000,"mountpoints":[],"children":[]}]}`

// 同一块盘，lsblk 只给出内核名（PATH=/dev/dm-N、NAME=<vg>-<lv>）时的样子：
// 不同发行版/版本的 lsblk 写法不一，认不出来就会把"盘明明在"误判成"设备看不见"。
const layoutTreeKernelNameNoPart = `{"blockdevices":[{"name":"vg0-lv","path":"/dev/dm-3","type":"lvm","size":10737418240,"mountpoints":[],"children":[]}]}`

const layoutTreeKernelNameWithPart = `{"blockdevices":[{"name":"vg0-lv","path":"/dev/dm-3","type":"lvm","size":10737418240,"mountpoints":[],"children":[{"name":"vg0-lv1","path":"/dev/dm-4","type":"part","size":10735304704,"fstype":"ntfs","mountpoints":[]}]}]}`

// fakeLayoutEnv 构造布局流程用到的假命令工具链（置于 PATH 最前），并记录每次调用。
//
//	table  —— blkid -s PTTYPE 与 parted print 的答案（两者本就该一致；空串 = 没有分区表）
//	fsType —— blkid -s TYPE 的答案（空串 = 没有文件系统）
//	before / after —— lsblk 的输出：**盘上真的有分区**（parted mkpart 跑过）时给 after，
//	                  否则给 before。这样"建分区之后才看得见分区"与"本来就有分区"都能演，
//	                  而不会让代码凭"第几次调用"取巧通过。
//
// 返回的函数读出全部调用记录，每行形如 "parted -s /dev/mapper/vg0-lv mklabel gpt"。
func fakeLayoutEnv(t *testing.T, table, fsType, before, after string) func() []string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	// 盘上"已经有分区"的状态文件：由假 parted 的 mkpart 创建（真实世界里分区是它建的）。
	partedDone := filepath.Join(dir, "parted.mkpart")

	partedTable := table
	if partedTable == "" {
		partedTable = "unknown"
	}
	// 每条假命令都先记一行调用（命令名 + 原样透传的参数），再给出自己的答案。
	rec := "printf '%s %s\\n' \"$(basename \"$0\")\" \"$*\" >> '" + logPath + "'\n"

	writeFake(t, dir, "blkid", rec+`for a in "$@"; do
  case "$a" in
    PTTYPE) if [ -n '`+table+`' ]; then printf '%s\n' '`+table+`'; exit 0; fi; exit 2;;
    TYPE)   if [ -n '`+fsType+`' ]; then printf '%s\n' '`+fsType+`'; exit 0; fi; exit 2;;
  esac
done
exit 2`)
	writeFake(t, dir, "parted", rec+`for a in "$@"; do
  case "$a" in
    mklabel) exit 0;;
    mkpart) : > '`+partedDone+`'; exit 0;;
    print) printf 'Partition Table: %s\n' '`+partedTable+`'; exit 0;;
  esac
done
exit 0`)
	writeFake(t, dir, "lsblk", rec+`if [ -e '`+partedDone+`' ]; then
  printf '%s\n' '`+after+`'
else
  printf '%s\n' '`+before+`'
fi`)
	// 其余工具：本用例只关心"有没有被调用、参数是什么"，一律成功即可。
	for _, name := range []string{"wipefs", "partx", "partprobe", "udevadm", "mkfs.ntfs", "mkfs.ext4"} {
		writeFake(t, dir, name, rec+"exit 0")
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return func() []string {
		t.Helper()
		data, err := os.ReadFile(logPath)
		if err != nil {
			// 一条命令都没执行时日志文件还不存在（例如入参校验就被挡下），这不是失败。
			if os.IsNotExist(err) {
				return nil
			}
			t.Fatalf("读取命令日志失败: %v", err)
		}
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
}

// hasCall 判断命令日志里是否出现过某条完整调用。
func hasCall(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}

// layoutManager 造一个只用于布局探测的 Manager（日志丢弃，避免刷测试输出）。
func layoutManager() *Manager {
	return New(Options{VG: "vg0", ThinPool: "vault", Logger: discardLogger()})
}

func TestDiskFileSystem(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"ntfs", "ntfs"},
		{" NTFS ", "ntfs"},
		{"ext4", "ext4"},
		{"Ext4", "ext4"},
	} {
		got, err := diskFileSystem(tc.in)
		if err != nil {
			t.Fatalf("diskFileSystem(%q) 报错: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("diskFileSystem(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
	// 任意字符串会被拼进 mkfs.<name> 与 parted 的参数，必须一律拒绝。
	for _, in := range []string{"", "  ", "vfat", "ntfs; rm -rf /", "ntfs3"} {
		if _, err := diskFileSystem(in); err == nil {
			t.Fatalf("diskFileSystem(%q) 应被拒绝", in)
		}
	}
}

// TestEnsureDiskLayoutCreatesGPTPartition：空盘 → GPT + 单分区，分区类型按文件系统写。
func TestEnsureDiskLayoutCreatesGPTPartition(t *testing.T) {
	for _, tc := range []struct{ fs, partedType string }{
		{"ntfs", "ntfs"},
		{"ext4", "ext4"},
	} {
		t.Run(tc.fs, func(t *testing.T) {
			calls := fakeLayoutEnv(t, "", "", layoutTreeNoPart, layoutTreeWithPart)

			part, layout, err := layoutManager().ensureDiskLayout(context.Background(), layoutDev, tc.fs)
			if err != nil {
				t.Fatalf("ensureDiskLayout 失败: %v", err)
			}
			if part != layoutPartDev {
				t.Fatalf("数据设备 = %q，期望分区 %q", part, layoutPartDev)
			}
			if layout != layoutEmpty {
				t.Fatalf("布局 = %q，期望 %q（本次刚建好）", layout, layoutEmpty)
			}

			got := calls()
			// 分区类型必须按文件系统写：客户端按这个 GUID 识别分区（写错会显示成"未知分区"）。
			want := "parted -s -a optimal " + layoutDev + " mkpart primary " + tc.partedType + " 1MiB 100%"
			if !hasCall(got, want) {
				t.Fatalf("缺少建分区调用 %q，实际调用：\n%s", want, strings.Join(got, "\n"))
			}
			if want := "parted -s " + layoutDev + " mklabel gpt"; !hasCall(got, want) {
				t.Fatalf("空盘应先写 GPT 分区表，缺少 %q，实际调用：\n%s", want, strings.Join(got, "\n"))
			}
			if want := "wipefs -a " + layoutDev; !hasCall(got, want) {
				t.Fatalf("写表前应清残留签名，缺少 %q", want)
			}
		})
	}
}

// TestEnsureDiskLayoutFindsDeviceByKernelName：lsblk 对 dm 设备可能只给 /dev/dm-N + dm 名
// （不同发行版/版本写法不一），这时仍要认得出这块盘，否则会把它误判成"设备看不见"而拒绝建盘。
func TestEnsureDiskLayoutFindsDeviceByKernelName(t *testing.T) {
	calls := fakeLayoutEnv(t, "", "", layoutTreeKernelNameNoPart, layoutTreeKernelNameWithPart)

	part, layout, err := layoutManager().ensureDiskLayout(context.Background(), layoutDev, "ntfs")
	if err != nil {
		t.Fatalf("ensureDiskLayout 失败（lsblk 只给内核名时应照样认得出这块盘）: %v", err)
	}
	if part != "/dev/dm-4" {
		t.Fatalf("数据设备 = %q，期望 lsblk 报出的分区 %q", part, "/dev/dm-4")
	}
	if layout != layoutEmpty {
		t.Fatalf("布局 = %q，期望 %q", layout, layoutEmpty)
	}
	if want := "parted -s " + layoutDev + " mklabel gpt"; !hasCall(calls(), want) {
		t.Fatalf("缺少 %q", want)
	}
}

// TestEnsureDiskLayoutReusesExistingPartition：已有分区的盘只读复用，绝不动分区表。
func TestEnsureDiskLayoutReusesExistingPartition(t *testing.T) {
	calls := fakeLayoutEnv(t, "gpt", "ntfs", layoutTreeWithPart, layoutTreeWithPart)

	part, layout, err := layoutManager().ensureDiskLayout(context.Background(), layoutDev, "ntfs")
	if err != nil {
		t.Fatalf("ensureDiskLayout 失败: %v", err)
	}
	if part != layoutPartDev {
		t.Fatalf("数据设备 = %q，期望复用已有分区 %q", part, layoutPartDev)
	}
	if layout != layoutGPT {
		t.Fatalf("布局 = %q，期望 %q", layout, layoutGPT)
	}

	got := calls()
	if hasCall(got, "parted -s "+layoutDev+" mklabel gpt") {
		t.Fatalf("盘上已有分区表：绝不能重新 mklabel（会抹掉分区与数据），实际调用：\n%s", strings.Join(got, "\n"))
	}
	if hasCall(got, "parted -s -a optimal "+layoutDev+" mkpart primary ntfs 1MiB 100%") {
		t.Fatal("已有分区时不应再建分区")
	}
	if hasCall(got, "wipefs -a "+layoutDev) {
		t.Fatal("已有数据的盘不该被 wipefs 抹签名")
	}
}

// TestEnsureDiskLayoutKeepsLegacyWholeDisk：升级前的整盘布局（裸 NTFS 直接在整盘上）原样使用。
//
// 客户端挂不上是已知代价，但盘里的数据仍然可用；NTFS 无法无损搬进分区，转换就等于丢数据。
func TestEnsureDiskLayoutKeepsLegacyWholeDisk(t *testing.T) {
	calls := fakeLayoutEnv(t, "", "ntfs", layoutTreeNoPart, layoutTreeNoPart)

	part, layout, err := layoutManager().ensureDiskLayout(context.Background(), layoutDev, "ntfs")
	if err != nil {
		t.Fatalf("ensureDiskLayout 失败: %v", err)
	}
	if part != layoutDev {
		t.Fatalf("数据设备 = %q，期望整盘 %q（旧布局不做转换）", part, layoutDev)
	}
	if layout != layoutLegacy {
		t.Fatalf("布局 = %q，期望 %q", layout, layoutLegacy)
	}

	// 探测（parted print / blkid）可以有，改动不行。
	assertNoLayoutChange(t, calls())
}

// TestEnsureDiskLayoutRebuildsMissingPartitionWithoutMklabel：
// 有分区表却看不到分区（映射未建立，或上次建盘在 mklabel 后中断）→ 重扫后补建，**不 mklabel**。
func TestEnsureDiskLayoutRebuildsMissingPartitionWithoutMklabel(t *testing.T) {
	calls := fakeLayoutEnv(t, "gpt", "", layoutTreeNoPart, layoutTreeWithPart)

	part, layout, err := layoutManager().ensureDiskLayout(context.Background(), layoutDev, "ntfs")
	if err != nil {
		t.Fatalf("ensureDiskLayout 失败: %v", err)
	}
	if part != layoutPartDev {
		t.Fatalf("数据设备 = %q，期望补建出的分区 %q", part, layoutPartDev)
	}
	if layout != layoutGPT {
		t.Fatalf("布局 = %q，期望 %q（分区表本来就在）", layout, layoutGPT)
	}

	got := calls()
	if want := "parted -s -a optimal " + layoutDev + " mkpart primary ntfs 1MiB 100%"; !hasCall(got, want) {
		t.Fatalf("应补建分区，缺少 %q，实际调用：\n%s", want, strings.Join(got, "\n"))
	}
	if hasCall(got, "parted -s "+layoutDev+" mklabel gpt") {
		t.Fatalf("补建分区时不得重新 mklabel，实际调用：\n%s", strings.Join(got, "\n"))
	}
}

// TestEnsureDiskLayoutRefusesInvisibleDevice：设备不在 lsblk 树里时判不准它是不是空盘，必须失败退出。
func TestEnsureDiskLayoutRefusesInvisibleDevice(t *testing.T) {
	calls := fakeLayoutEnv(t, "", "", layoutTreeInvisible, layoutTreeInvisible)

	_, _, err := layoutManager().ensureDiskLayout(context.Background(), layoutDev, "ntfs")
	e, ok := apperr.As(err)
	if !ok || e.Code != CodeVHDFailed || e.Args["reason"] != "device_not_found" {
		t.Fatalf("错误 = %v，期望 %s（reason=device_not_found）", err, CodeVHDFailed)
	}
	assertNoLayoutChange(t, calls())
}

// TestEnsureDiskLayoutRefusesUnreadableTable：探不到分区表、却看得见子分区
// （说明探表结果不可信）→ 绝不当成空盘去 mklabel。
func TestEnsureDiskLayoutRefusesUnreadableTable(t *testing.T) {
	calls := fakeLayoutEnv(t, "", "", layoutTreeWithPart, layoutTreeWithPart)

	_, _, err := layoutManager().ensureDiskLayout(context.Background(), layoutDev, "ntfs")
	e, ok := apperr.As(err)
	if !ok || e.Code != CodeVHDFailed || e.Args["reason"] != "partition_table_unreadable" {
		t.Fatalf("错误 = %v，期望 %s（reason=partition_table_unreadable）", err, CodeVHDFailed)
	}
	assertNoLayoutChange(t, calls())
}

// TestEnsureDiskLayoutRejectsUnknownFileSystem：文件系统名会被拼进 mkfs.<name>，
// 非法取值必须在动手之前挡掉（不能等到 mkfs 才发现）。
func TestEnsureDiskLayoutRejectsUnknownFileSystem(t *testing.T) {
	calls := fakeLayoutEnv(t, "", "", layoutTreeNoPart, layoutTreeWithPart)

	if _, _, err := layoutManager().ensureDiskLayout(context.Background(), layoutDev, "vfat"); err == nil {
		t.Fatal("未知文件系统必须被拒绝")
	}
	assertNoLayoutChange(t, calls())
}

// TestPartitionTableTypeQuietOnNoLabel：设备上没有分区表时 parted 会打印整块设备信息后
// **以非 0 退出**（`Error: <dev>: unrecognised disk label`）。这是探测的**正常答案**，不是故障：
// 空盘每次探测都走到这条兜底（blkid 不认 PTTYPE 时），按 ERROR 记会让日志里满是红字。
//
// 真机反馈："所有操作都成功了，但是有这个日志" —— 日志里的红字会把一次成功的操作读成失败。
func TestPartitionTableTypeQuietOnNoLabel(t *testing.T) {
	dir := t.TempDir()
	// blkid 不认 PTTYPE（老 util-linux）：退出 2，把问题交给 parted 兜底。
	writeFake(t, dir, "blkid", "exit 2")
	// parted print 与真机一致：先打印设备信息，再以非 0 退出。
	writeFake(t, dir, "parted", `printf 'Error: %s: unrecognised disk label\n' "$2"
printf 'Model: Linux device-mapper (thin) (dm)\n'
printf 'Disk %s: 4295MB\n' "$2"
printf 'Partition Table: unknown\n'
printf 'Disk Flags:\n'
exit 1`)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m := New(Options{VG: "vg0", ThinPool: "vault", Logger: logger})

	if got := m.partitionTableType(context.Background(), layoutDev); got != "" {
		t.Fatalf("设备没有分区表时 = %q，期望空串", got)
	}
	logs := buf.String()
	// 先确认假命令真的按"非 0 退出"跑了（否则下面的断言会白通过）。
	if !strings.Contains(logs, "命令返回非 0") {
		t.Fatalf("假 parted 应非 0 退出并留 Debug 痕迹，实际日志：\n%s", logs)
	}
	if strings.Contains(logs, "level=ERROR") {
		t.Fatalf("探测「没有分区表」是正常答案，不该记 ERROR，实际日志：\n%s", logs)
	}
}

// assertNoLayoutChange 断言这次调用**没有**碰过盘上的布局（没写表、没建分区、没抹签名）。
func assertNoLayoutChange(t *testing.T, calls []string) {
	t.Helper()
	for _, c := range calls {
		switch {
		case strings.Contains(c, " mklabel "), strings.Contains(c, " mkpart "), strings.HasPrefix(c, "wipefs "):
			t.Fatalf("本次调用不该改动盘上的布局，实际调用：%s", c)
		}
	}
}
