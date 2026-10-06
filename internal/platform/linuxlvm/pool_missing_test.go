//go:build linux

package linuxlvm

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"vault/internal/apperr"
)

// 本文件锁住"存储池缺失"这条路径的两个边界：
//
//  1. 池确实不存在时必须报**可执行**的错误（platform.pool_missing），
//     而不是软失败后让 lvcreate 甩出 `Volume group "vg0" not found` ——
//     真机上用户就是这样对着"创建存储失败"完全无从下手的；
//  2. 池存在、只是水位读不到（权限、字段不支持等）时必须**保持软失败**：
//     仍跳过闸门交给 lvcreate 兜底，不能在这里误报"池不存在"或"空间不足"。
//
// 判定逻辑经 exec 调用 lvs/vgs，因此这里用假的同名脚本来喂输入：
// 无需真实 LVM 也能在任意 Linux 机器（含 CI 容器）上跑。

// discardLogger 让被测代码的日志不污染测试输出。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// writeFake 在 dir 下生成一个假的 LVM 命令（/bin/sh 脚本）。
func writeFake(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("写假命令 %s 失败: %v", name, err)
	}
}

// fakeLVM 把假的 vgs/lvs 放进临时目录并置于 PATH 最前（优先于真机上的真命令）。
//
// 假 lvs 按参数分流，模拟真机行为：
//   - 带 data_percent 的调用（水位闸门）→ 照真机输出报错并 exit 5；
//   - 其余调用（存在性探测）→ 输出 lvsJSON。
//
// vgsFail=true 用来模拟"连卷组列表都读不到"（例如服务账号权限不足）。
func fakeLVM(t *testing.T, vgsJSON, lvsJSON string, vgsFail bool) {
	t.Helper()
	dir := t.TempDir()
	vgsBody := "printf '%s\\n' '" + vgsJSON + "'"
	if vgsFail {
		vgsBody = "echo '  Failed to read volume groups' >&2; exit 1"
	}
	writeFake(t, dir, "vgs", vgsBody)
	// JSON 里只有双引号，嵌在单引号里安全。
	writeFake(t, dir, "lvs", `case "$*" in
  *data_percent*)
    echo '  Volume group "vg0" not found' >&2
    echo '  Cannot process volume group vg0' >&2
    exit 5
    ;;
esac
printf '%s\n' '`+lvsJSON+`'`)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestPoolMissingKind(t *testing.T) {
	const (
		vgOnly   = `{"report":[{"vg":[{"vg_name":"vg0","vg_size":"1000","vg_free":"900"}]}]}`
		otherVG  = `{"report":[{"vg":[{"vg_name":"othervg","vg_size":"1000","vg_free":"900"}]}]}`
		poolOnly = `{"report":[{"lv":[{"lv_name":"vault"}]}]}`
		dataOnly = `{"report":[{"lv":[{"lv_name":"data1"}]}]}`
	)

	cases := []struct {
		name    string
		vgsJSON string
		lvsJSON string
		vgsFail bool
		want    string
	}{
		{"卷组不存在", otherVG, poolOnly, false, "vg"},
		{"卷组在但 thin pool 不存在", vgOnly, dataOnly, false, "pool"},
		{"卷组与 thin pool 都在", vgOnly, poolOnly, false, ""},
		{"卷组列表都读不到（无法判定）", "", "", true, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeLVM(t, tc.vgsJSON, tc.lvsJSON, tc.vgsFail)
			m := New(Options{VG: "vg0", ThinPool: "vault", Logger: discardLogger()})
			if got := m.poolMissingKind(context.Background(), "vg0", "vault"); got != tc.want {
				t.Fatalf("poolMissingKind = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// TestCheckWatermarkMissingPool：池不存在时必须给出可执行的错误码与参数，
// 而不是让后续 lvcreate 用裸报错收场。
func TestCheckWatermarkMissingPool(t *testing.T) {
	fakeLVM(t,
		`{"report":[{"vg":[{"vg_name":"othervg","vg_size":"1000","vg_free":"900"}]}]}`,
		`{"report":[{"lv":[]}]}`,
		false)

	m := New(Options{VG: "vg0", ThinPool: "vault", Logger: discardLogger()})
	err := m.checkWatermark(context.Background(), "vg0")
	if err == nil {
		t.Fatal("池不存在时必须报错，不能软失败后交给 lvcreate")
	}
	e, ok := apperr.As(err)
	if !ok {
		t.Fatalf("应为业务错误（可被前端翻译），实际: %v", err)
	}
	if e.Code != CodePoolMissing {
		t.Fatalf("错误码 = %s，期望 %s", e.Code, CodePoolMissing)
	}
	if e.HTTP != http.StatusServiceUnavailable {
		t.Fatalf("HTTP 状态 = %d，期望 %d", e.HTTP, http.StatusServiceUnavailable)
	}
	if e.Args["vg"] != "vg0" || e.Args["thin_pool"] != "vault" {
		t.Fatalf("参数应带上 vg/thin_pool 供前端插值，实际: %v", e.Args)
	}
}

// TestCheckWatermarkKeepsFailSoftWhenPoolExists：池存在、只是水位读不到时，
// 必须保持原有的软失败（跳过闸门，交给 lvcreate 兜底）。
func TestCheckWatermarkKeepsFailSoftWhenPoolExists(t *testing.T) {
	fakeLVM(t,
		`{"report":[{"vg":[{"vg_name":"vg0","vg_size":"1000","vg_free":"900"}]}]}`,
		`{"report":[{"lv":[{"lv_name":"vault"}]}]}`,
		false)

	m := New(Options{VG: "vg0", ThinPool: "vault", Logger: discardLogger()})
	if err := m.checkWatermark(context.Background(), "vg0"); err != nil {
		t.Fatalf("池存在时不应因读不到水位而拒绝创建，实际: %v", err)
	}
}
