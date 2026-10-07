//go:build linux

package linuxlvm

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"vault/internal/apperr"
)

// 本文件锁住"释放设备"的三条边界（用假命令喂输入，无需真机 LVM）：
//
//  1. 步骤顺序与"叶子优先"：挂载点先卸载、分区先于整盘被抹签名；
//  2. 完成判据是**重新枚举后的 reason 为空**，而不是"我们自认为清干净了"；
//  3. 危险输入必须被挡在命令之前（系统盘、非 /dev 路径）。
const (
	// sdb 上有一个 ext4 分区，该分区是 vg0 的 PV 且被挂载在 /mnt/data。
	releaseDirtyTree = `{"blockdevices":[{"name":"sdb","path":"/dev/sdb","type":"disk","size":10737418240,"rota":true,"model":"FakeDisk","mountpoints":[],"fstype":null,"children":[{"name":"sdb1","path":"/dev/sdb1","type":"part","size":10736369664,"fstype":"ext4","mountpoints":["/mnt/data"]}]}]}`
	// 释放之后应有的样子：分区没了、没有任何签名。
	releaseCleanTree = `{"blockdevices":[{"name":"sdb","path":"/dev/sdb","type":"disk","size":10737418240,"rota":true,"model":"FakeDisk","mountpoints":[],"fstype":null,"children":[]}]}`
)

// fakeReleaseEnv 构造一套假的块设备工具链并置于 PATH 最前，返回目录以便按需覆盖某个命令。
//
// 假 lsblk 用标记文件实现"第一次返回 dirty、之后返回 clean"：
// 这样既能走完释放流程，又能验证复核读的是**重新枚举**的结果。
func fakeReleaseEnv(t *testing.T, dirty, clean string) string {
	t.Helper()
	dir := t.TempDir()
	writeFake(t, dir, "lsblk", `if [ -e "$0.done" ]; then
  printf '%s\n' '`+clean+`'
else
  : > "$0.done"
  printf '%s\n' '`+dirty+`'
fi`)
	writeFake(t, dir, "pvs", `printf '%s\n' '{"report":[{"pv":[{"pv_name":"/dev/sdb1","vg_name":"vg0"}]}]}'`)
	writeFake(t, dir, "umount", "exit 0")
	writeFake(t, dir, "swapon", "exit 0")
	writeFake(t, dir, "wipefs", "exit 0")
	writeFake(t, dir, "pvremove", "exit 0")
	writeFake(t, dir, "partprobe", "exit 0")
	writeFake(t, dir, "udevadm", "exit 0")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestReleaseDeviceSteps(t *testing.T) {
	fakeReleaseEnv(t, releaseDirtyTree, releaseCleanTree)
	m := New(Options{VG: "vg0", ThinPool: "vault", Logger: discardLogger()})

	rep, err := m.ReleaseDevice(context.Background(), "/dev/sdb")
	if err != nil {
		t.Fatalf("ReleaseDevice 失败: %v", err)
	}

	want := "umount,wipefs,wipefs,pvremove,partprobe,udevadm"
	var got []string
	for _, s := range rep.Steps {
		got = append(got, s.Step)
	}
	if strings.Join(got, ",") != want {
		t.Fatalf("步骤序列 = %v，期望 %s", got, want)
	}
	// 叶子优先：分区先于整盘被抹签名（否则整盘一擦分区节点就没了）。
	if rep.Steps[1].Target != "/dev/sdb1" || rep.Steps[2].Target != "/dev/sdb" {
		t.Fatalf("wipefs 顺序 = %s,%s，期望分区在前", rep.Steps[1].Target, rep.Steps[2].Target)
	}
	for _, s := range rep.Steps {
		if !s.OK {
			t.Fatalf("步骤 %s(%s) 未成功: skipped=%v detail=%s", s.Step, s.Target, s.Skipped, s.Detail)
		}
	}
	if !rep.Released {
		t.Fatalf("复核应判定为已可选，实际 released=false reason=%q", rep.Reason)
	}
}

// TestReleaseDeviceToleratesNoPartitionTable：wipefs 之后设备上已经没有分区表了，
// 此时真机 partprobe 会打印 `Error: /dev/sdb: unrecognised disk label` 并以非 0 退出。
//
// 那是"本来就没有表可重读"，属于正常分支：既不该记一条 ERROR 日志，也不该在报告里挂一条
// 失败项——否则一次完全成功的释放看起来像失败了（真机反馈："所有操作都成功了，
// 但是有这个日志"）。
func TestReleaseDeviceToleratesNoPartitionTable(t *testing.T) {
	dir := fakeReleaseEnv(t, releaseDirtyTree, releaseCleanTree)
	writeFake(t, dir, "partprobe", `printf 'Error: %s: unrecognised disk label\n' "$1"
exit 1`)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m := New(Options{VG: "vg0", ThinPool: "vault", Logger: logger})

	rep, err := m.ReleaseDevice(context.Background(), "/dev/sdb")
	if err != nil {
		t.Fatalf("ReleaseDevice 失败: %v", err)
	}

	var seen bool
	for _, s := range rep.Steps {
		if s.Step != "partprobe" {
			continue
		}
		seen = true
		if !s.OK && !s.Skipped {
			t.Fatalf("「无分区表可重读」应记为跳过而非失败，实际 ok=%v skipped=%v detail=%s", s.OK, s.Skipped, s.Detail)
		}
		if s.Detail == "" {
			t.Fatal("跳过也要带上原因（命令输出），否则用户看不懂这一步为什么没做")
		}
	}
	if !seen {
		t.Fatal("没有走到 partprobe 这一步，用例失去意义")
	}
	if !rep.Released {
		t.Fatalf("释放应判定为已可选，实际 released=false reason=%q", rep.Reason)
	}
	if logs := buf.String(); strings.Contains(logs, "level=ERROR") {
		t.Fatalf("预期内的非 0 退出不该记 ERROR，实际日志:\n%s", logs)
	}
}

// TestReleaseDeviceStillBlocked：清不掉时必须如实报告"仍不可用"，
// 而不是因为命令都返回了 0 就宣布成功。
func TestReleaseDeviceStillBlocked(t *testing.T) {
	fakeReleaseEnv(t, releaseDirtyTree, releaseDirtyTree)
	m := New(Options{VG: "vg0", ThinPool: "vault", Logger: discardLogger()})

	rep, err := m.ReleaseDevice(context.Background(), "/dev/sdb")
	if err != nil {
		t.Fatalf("ReleaseDevice 失败: %v", err)
	}
	if rep.Released {
		t.Fatal("重新枚举仍给出不可用原因时，不得判定为已释放")
	}
	if rep.Reason == "" {
		t.Fatal("仍不可用时应带上原因，供前端展示")
	}
}

func TestReleaseDeviceRejectsSystemDisk(t *testing.T) {
	const sysTree = `{"blockdevices":[{"name":"sda","path":"/dev/sda","type":"disk","size":1,"rota":false,"mountpoints":[],"children":[{"name":"sda1","path":"/dev/sda1","type":"part","fstype":"ext4","mountpoints":["/"]}]}]}`
	fakeReleaseEnv(t, sysTree, sysTree)
	m := New(Options{VG: "vg0", ThinPool: "vault", Logger: discardLogger()})

	_, err := m.ReleaseDevice(context.Background(), "/dev/sda")
	if err == nil {
		t.Fatal("系统盘必须被拒绝")
	}
	e, ok := apperr.As(err)
	if !ok || e.Code != apperr.CodeInvalidParam || e.Args["reason"] != "system_disk" {
		t.Fatalf("错误 = %v，期望 %s（reason=system_disk）", err, apperr.CodeInvalidParam)
	}
}

func TestReleaseDeviceRejectsBadPath(t *testing.T) {
	fakeReleaseEnv(t, releaseDirtyTree, releaseCleanTree)
	m := New(Options{VG: "vg0", ThinPool: "vault", Logger: discardLogger()})

	for _, p := range []string{"", "sdb", "/dev/", "/dev/sda/../../etc/passwd", "/etc/passwd", "/dev/sdb sdb1"} {
		if _, err := m.ReleaseDevice(context.Background(), p); err == nil {
			t.Fatalf("路径 %q 必须被拒绝（它会被交给 wipefs/pvremove）", p)
		}
	}
}
