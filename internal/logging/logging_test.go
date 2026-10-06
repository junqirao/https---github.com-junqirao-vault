package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestReadDayAndPathForDay 锁定日志模块的核心：PathForDay("") 指向当天文件、ReadDay 只回
// 末尾且不漏最后几条。
func TestReadDayAndPathForDay(t *testing.T) {
	dir := t.TempDir()
	l, err := New(Options{Dir: dir, FileName: "Vault-Agent", KeepDays: 7})
	if err != nil {
		t.Fatalf("构造 Logger 失败：%v", err)
	}
	defer l.Close()

	l.Info("line one", "k", "v1")
	l.Info("line two", "k", "v2")
	l.Warn("line three")

	today := time.Now().Format("2006-01-02")
	path := l.PathForDay("")
	if !strings.HasPrefix(path, filepath.Join(dir, "Vault-Agent-")) || !strings.HasSuffix(path, ".log") {
		t.Fatalf("PathForDay 形态不对：%s", path)
	}
	if path != l.PathForDay(today) {
		t.Fatalf("PathForDay(\"\") 应等于当天文件：%s vs %s", path, l.PathForDay(today))
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("日志文件不存在：%v", err)
	}

	data, err := l.ReadDay("", 1<<20)
	if err != nil {
		t.Fatalf("ReadDay 失败：%v", err)
	}
	text := string(data)
	for _, want := range []string{"line one", "line two", "line three"} {
		if !strings.Contains(text, want) {
			t.Fatalf("日志尾部缺少 %q：%s", want, text)
		}
	}

	// 小 tail：只回末尾，且最后一条必须完整落在里面。
	small, err := l.ReadDay("", 200)
	if err != nil {
		t.Fatalf("ReadDay(200) 失败：%v", err)
	}
	if len(small) > 200 {
		t.Fatalf("ReadDay(200) 返回 %d 字节，超过上限", len(small))
	}
	if !strings.Contains(string(small), "line three") {
		t.Fatalf("ReadDay(200) 应包含最后一条：%s", string(small))
	}

	// 当天文件已存在：Days 必须列出它。
	if days := l.Days(); len(days) != 1 || days[0] != today {
		t.Fatalf("Days 应为 [%s]，实际 %v", today, days)
	}
}

// TestDaysAndReadDayHistorical 历史日志按天查询：手工造出昨天的文件后，Days 列出两天、
// ReadDay 能读到昨天那份。
func TestDaysAndReadDayHistorical(t *testing.T) {
	dir := t.TempDir()
	l, err := New(Options{Dir: dir, FileName: "Vault-Agent", KeepDays: 7})
	if err != nil {
		t.Fatalf("构造 Logger 失败：%v", err)
	}
	defer l.Close()

	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	old := filepath.Join(dir, "Vault-Agent-"+yesterday+".log")
	if err := os.WriteFile(old, []byte("{\"level\":\"INFO\",\"msg\":\"old line\"}\n"), 0o644); err != nil {
		t.Fatalf("写入历史日志失败：%v", err)
	}
	// 干扰项：命名不合法（不是日期）的文件不能出现在可查日期里。
	if err := os.WriteFile(filepath.Join(dir, "Vault-Agent-notaday.log"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("写入干扰文件失败：%v", err)
	}

	days := l.Days()
	want := []string{yesterday, time.Now().Format("2006-01-02")}
	if len(days) != len(want) || days[0] != want[0] || days[1] != want[1] {
		t.Fatalf("Days 应为 %v，实际 %v", want, days)
	}

	data, err := l.ReadDay(yesterday, 1<<20)
	if err != nil {
		t.Fatalf("ReadDay(昨天) 失败：%v", err)
	}
	if !strings.Contains(string(data), "old line") {
		t.Fatalf("昨天日志内容不对：%s", string(data))
	}

	// 非法日期必须被拒，且不能把它当路径拼出去。
	if path := l.PathForDay("../evil"); path != "" {
		t.Fatalf("非法日期的 PathForDay 应为空，实际 %q", path)
	}
	if _, err := l.ReadDay("../evil", 1024); err == nil {
		t.Fatal("非法日期的 ReadDay 应报错")
	}
}

// TestReadDayWithoutDaily 未启用文件日志时，PathForDay 为空、ReadDay 报 not exist、Days 为空。
func TestReadDayWithoutDaily(t *testing.T) {
	l, err := New(Options{AlsoConsole: true})
	if err != nil {
		t.Fatalf("构造 Logger 失败：%v", err)
	}
	defer l.Close()
	if path := l.PathForDay(""); path != "" {
		t.Fatalf("未启用文件日志时 PathForDay 应为空，实际 %q", path)
	}
	if days := l.Days(); len(days) != 0 {
		t.Fatalf("未启用文件日志时 Days 应为空，实际 %v", days)
	}
	if _, err := l.ReadDay("", 1024); err == nil {
		t.Fatal("未启用文件日志时 ReadDay 应报错")
	}
}
