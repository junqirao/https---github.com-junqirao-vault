package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadTailAndCurrentPath 锁定日志模块的核心：CurrentPath 指向当天文件、ReadTail 只回
// 末尾且不漏最后几条。
func TestReadTailAndCurrentPath(t *testing.T) {
	dir := t.TempDir()
	l, err := New(Options{Dir: dir, FileName: "Vault-Agent", KeepDays: 7})
	if err != nil {
		t.Fatalf("构造 Logger 失败：%v", err)
	}
	defer l.Close()

	l.Info("line one", "k", "v1")
	l.Info("line two", "k", "v2")
	l.Warn("line three")

	path := l.CurrentPath()
	if !strings.HasPrefix(path, filepath.Join(dir, "Vault-Agent-")) || !strings.HasSuffix(path, ".log") {
		t.Fatalf("CurrentPath 形态不对：%s", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("日志文件不存在：%v", err)
	}

	data, err := l.ReadTail(1 << 20)
	if err != nil {
		t.Fatalf("ReadTail 失败：%v", err)
	}
	text := string(data)
	for _, want := range []string{"line one", "line two", "line three"} {
		if !strings.Contains(text, want) {
			t.Fatalf("日志尾部缺少 %q：%s", want, text)
		}
	}

	// 小 tail：只回末尾，且最后一条必须完整落在里面。
	small, err := l.ReadTail(200)
	if err != nil {
		t.Fatalf("ReadTail(200) 失败：%v", err)
	}
	if len(small) > 200 {
		t.Fatalf("ReadTail(200) 返回 %d 字节，超过上限", len(small))
	}
	if !strings.Contains(string(small), "line three") {
		t.Fatalf("ReadTail(200) 应包含最后一条：%s", string(small))
	}
}

// TestReadTailWithoutDaily 未启用文件日志时，CurrentPath 为空、ReadTail 报 not exist。
func TestReadTailWithoutDaily(t *testing.T) {
	l, err := New(Options{AlsoConsole: true})
	if err != nil {
		t.Fatalf("构造 Logger 失败：%v", err)
	}
	defer l.Close()
	if path := l.CurrentPath(); path != "" {
		t.Fatalf("未启用文件日志时 CurrentPath 应为空，实际 %q", path)
	}
	if _, err := l.ReadTail(1024); err == nil {
		t.Fatal("未启用文件日志时 ReadTail 应报错")
	}
}
