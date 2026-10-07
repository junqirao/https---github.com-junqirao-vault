//go:build linux

package linuxlvm

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// 本文件锁住"挂载缓存时不能让 LVM 提问"这条约束（真机反馈）：
//
// 服务进程没有 tty，LVM 的 y/n 提问只能拿到默认值；而"要不要抹掉 cache pool 已有元数据"
// 这一问的默认值是 n，于是整条 lvconvert 以 exit 5 中止，用户只看到"创建带缓存的存储池
// 失败"：
//
//	Do you want wipe existing metadata of cache pool test4/vault_cache? [y/n]: [n]
//	  Conversion aborted.
//
// 修法是把意图写进命令行（--zero y，也正是 LVM 对 cache pool 的默认意图），提问便不再出现。
// 判定逻辑经 exec 调用 lvconvert，因此这里用假的同名脚本来喂输入，无需真机 LVM。

// TestAttachCacheNeverLeavesPrompt：假 lvconvert 复刻真机行为——**没给 --zero y 就提问并
// exit 5**；给了就静默通过。
//
// 用例同时钉住参数值必须是 y（抹掉既有元数据）而不是 n：n 正是 LVM 警告里的那条路
// （"Reusing mismatched cache pool metadata MAY DESTROY YOUR DATA!"），而本流程的 cache pool
// 是 initializePool 里刚 lvcreate 出来的（同名 LV 已存在时 lvcreate 必然失败），
// 没有任何值得保留的缓存内容。
func TestAttachCacheNeverLeavesPrompt(t *testing.T) {
	dir := t.TempDir()
	// 只有 --zero 后面紧跟着 y 才放行；否则照真机提问、中止并以 exit 5 失败。
	writeFake(t, dir, "lvconvert", `prev=
for a in "$@"; do
  if [ "$prev" = "--zero" ] && [ "$a" = "y" ]; then exit 0; fi
  prev="$a"
done
printf 'Do you want wipe existing metadata of cache pool vg0/vault_cache? [y/n]: [n]\n'
printf '  Conversion aborted.\n'
printf '  To preserve cache metadata add option "--zero n".\n'
exit 5`)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m := New(Options{VG: "vg0", ThinPool: "vault", Logger: logger})

	if err := m.attachCache(context.Background(), "vg0", "vault"); err != nil {
		t.Fatalf("attachCache 失败（非 tty 下把提问留给 LVM 必然走默认值 n 中止）: %v\n日志:\n%s", err, buf.String())
	}
	if logs := buf.String(); strings.Contains(logs, "level=ERROR") {
		t.Fatalf("正常挂载缓存不该有 ERROR 日志，实际:\n%s", logs)
	}
}
