package winps

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

// 这两个用例扫的是**内嵌脚本源文件**（不执行 PowerShell，任何平台都能跑），
// 守的是两类"改一行代码就会踩、且只在真机上才炸"的坑。

// bomPrefix 是 UTF-8 BOM。见 embed.go 顶部说明：没有 BOM 时 Windows PowerShell 5.1
// 会按系统 ANSI 代码页（中文环境为 GBK）解析脚本，中文注释/字符串乱码甚至语法错误。
var bomPrefix = []byte{0xEF, 0xBB, 0xBF}

// convertFromJSONInPipe 匹配"把 ConvertFrom-Json 的结果直接接进管道"的写法。
//
// Windows PowerShell 5.1 的 ConvertFrom-Json 把 JSON 数组**整个当一个对象**输出（不展开），
// 因此 `@(ConvertFrom-Json -InputObject $json | Where-Object {...})` 拿到的 `$_` 是整个数组，
// 后续 `[int]` 转换直接抛 "无法将 System.Object[] 转换为 System.Int32"。
// 正确写法是先把结果落到变量，再在变量上用 @(...) 展开：
//
//	$decoded = ConvertFrom-Json -InputObject $json
//	foreach ($item in @($decoded)) { ... }
var convertFromJSONInPipe = regexp.MustCompile(`ConvertFrom-Json[^|]*\|`)

// TestEmbeddedScriptsAreUTF8WithBOM 防止脚本被编辑器/工具"顺手"存成无 BOM 的 UTF-8。
func TestEmbeddedScriptsAreUTF8WithBOM(t *testing.T) {
	for _, name := range embeddedScriptNames(t) {
		raw, err := scriptsFS.ReadFile(name)
		if err != nil {
			t.Fatalf("读取内嵌脚本 %s 失败: %v", name, err)
		}
		if !bytes.HasPrefix(raw, bomPrefix) {
			t.Errorf("%s 缺少 UTF-8 BOM：PowerShell 5.1 会按 ANSI 代码页解析，中文注释/字符串会乱码甚至语法错误", name)
		}
	}
}

// TestEmbeddedScriptsDoNotPipeConvertFromJSON 守住上面那个 PS 5.1 的数组解析坑。
//
// 真实事故：volume_find_iscsi_disk.ps1 这么写之后，**本机只要已经挂了一块盘**
// （UsedDiskNumbers 非空），后续每次挂载都在 find_disk 阶段失败
// （"无法将 System.Object[] 转换为 System.Int32"）。
func TestEmbeddedScriptsDoNotPipeConvertFromJSON(t *testing.T) {
	for _, name := range embeddedScriptNames(t) {
		raw, err := scriptsFS.ReadFile(name)
		if err != nil {
			t.Fatalf("读取内嵌脚本 %s 失败: %v", name, err)
		}
		for i, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
			// 注释行本身就是用来记录这个坑的（会原样引用错误写法），跳过。
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			if convertFromJSONInPipe.MatchString(line) {
				t.Errorf("%s:%d 把 ConvertFrom-Json 直接接进了管道；PS 5.1 不会展开 JSON 数组，"+
					"请先 `$decoded = ConvertFrom-Json -InputObject $x`，再 `foreach ($item in @($decoded))`：\n%s",
					name, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// embeddedScriptNames 返回全部内嵌脚本的相对路径（scripts/xxx.ps1）。
func embeddedScriptNames(t *testing.T) []string {
	t.Helper()
	entries, err := scriptsFS.ReadDir("scripts")
	if err != nil {
		t.Fatalf("枚举内嵌脚本失败: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".ps1") {
			continue
		}
		names = append(names, "scripts/"+e.Name())
	}
	if len(names) == 0 {
		t.Fatal("没有枚举到任何内嵌脚本，embed 配置可能坏了")
	}
	return names
}
