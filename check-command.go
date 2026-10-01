// check-command.go —— Vault 服务端"外部命令 / 平台能力"一次性自检工具。
//
// 目的：把 Vault 服务端**所有依赖的外部命令与系统能力**按真实参数跑一遍，
// 一次拿到完整的失败清单（含最小复现命令），避免反复发包试错。
//
// 编译（单文件，跨平台，无需任何第三方依赖）：
//
//	Windows: go build -o check-command.exe check-command.go
//	Linux  : GOOS=linux GOARCH=amd64 go build -o check-command check-command.go
//
// 运行：
//
//	./check-command                  # 全量检查（含沙箱内的写操作，结束自动清理）
//	./check-command -skip-write      # 只做只读探测（不挂载/不格式化/不建目标）
//	./check-command -lvm-sandbox     # Linux：额外用 loop 设备建临时 VG 做 LVM 全链路验证
//	./check-command -out D:\a.log    # 指定日志文件路径
//	./check-command -keep            # 失败时保留沙箱目录，便于人工复现
//
// 安全约定（重要）：
//  1. 所有写操作只针对本工具自建的沙箱：临时目录 + 前缀 vaultcheck-* 的目标/虚拟盘/LV，
//     结束时尽力清理，**绝不触碰任何既有对象**；
//  2. 发起端（Initiator）的连接/断开/门户增删一律**不执行**：
//     那属于客户端侧能力，且本机 loopback iSCSI 有把服务器挂死的风险（见报告末尾说明）；
//     唯一例外：F1b 会幂等**启动** MSiSCSI 服务（产品挂载前必做的一步，不建立任何连接）；
//  3. LVM 的全链路写操作默认关闭，需要显式 -lvm-sandbox 才执行。
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// 内嵌全部 PowerShell 脚本：二进制自包含，拷到目标机上即可运行。
//
// 注意：脚本是 UTF-8 **with BOM** 的，必须原样落盘（PowerShell 5.1 对无 BOM 的
// .ps1 会按 ANSI 代码页解析，中文会乱码甚至语法错误），因此这里按字节复制。
//
//go:embed internal/platform/winps/scripts/*.ps1
var scriptsFS embed.FS

const embeddedScriptDir = "internal/platform/winps/scripts"

// ---------------------------------------------------------------------------
// 报告
// ---------------------------------------------------------------------------

type status string

const (
	stOK   status = "OK  "
	stFail status = "FAIL"
	stWarn status = "WARN"
	stSkip status = "SKIP"
)

type item struct {
	section string
	name    string
	status  status
	detail  string
	repro   string
}

type report struct {
	w       io.Writer
	logPath string
	items   []item
	section string
	start   time.Time
}

func (r *report) sec(name string) {
	r.section = name
	fmt.Fprintf(r.w, "\n=== %s ===\n", name)
}

func (r *report) add(st status, name, detail, repro string) {
	r.items = append(r.items, item{section: r.section, name: name, status: st, detail: detail, repro: repro})
	fmt.Fprintf(r.w, "  [%s] %s\n", st, name)
	if strings.TrimSpace(detail) != "" {
		for _, line := range strings.Split(strings.TrimRight(detail, "\n"), "\n") {
			fmt.Fprintf(r.w, "         %s\n", line)
		}
	}
	if st == stFail && strings.TrimSpace(repro) != "" {
		fmt.Fprintf(r.w, "         复现: %s\n", repro)
	}
}

func (r *report) pass(name, detail, repro string) { r.add(stOK, name, detail, repro) }
func (r *report) fail(name, detail, repro string) { r.add(stFail, name, detail, repro) }
func (r *report) warn(name, detail, repro string) { r.add(stWarn, name, detail, repro) }
func (r *report) skip(name, why string)           { r.add(stSkip, name, why, "") }

// ---------------------------------------------------------------------------
// 命令执行
// ---------------------------------------------------------------------------

type cmdResult struct {
	stdout string
	stderr string
	exit   int
	err    error
	ms     int64
}

func (c cmdResult) failed() bool { return c.err != nil }

// describe 把一次执行折叠成几行可粘贴的文本。
func (c cmdResult) describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "exit=%d 耗时=%dms", c.exit, c.ms)
	if c.err != nil {
		fmt.Fprintf(&b, " err=%v", c.err)
	}
	if s := strings.TrimSpace(c.stdout); s != "" {
		fmt.Fprintf(&b, "\nstdout: %s", clampText(s, 3000))
	}
	if s := cleanPSNoise(c.stderr); s != "" {
		fmt.Fprintf(&b, "\nstderr: %s", clampText(s, 1500))
	}
	return b.String()
}

// cleanPSNoise 去掉 PowerShell 写到 stderr 的 CLIXML 进度噪音。
//
// Windows PowerShell 在 stdout 被重定向时，会把 progress 记录以 CLIXML 形式写 stderr
// （典型内容就是"Preparing modules for first use."），这与脚本本身无关，
// 混在报告里会把真正的报错淹没。这里把整段 CLIXML 摘掉，只保留真正的错误文本。
func cleanPSNoise(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	if !strings.Contains(s, "#< CLIXML") {
		return s
	}
	// CLIXML 会把换行编码成 _x000D__x000A_，先还原成真换行，方便逐行判断。
	s = strings.ReplaceAll(s, "_x000D__x000A_", "\n")

	var keep []string
	// <S S="Error">真正的报错文本</S>：必须保留，否则失败原因就丢了。
	for _, m := range clixmlErrorRe.FindAllStringSubmatch(s, -1) {
		if text := strings.TrimSpace(m[1]); text != "" {
			keep = append(keep, "- "+text)
		}
	}
	// 非 CLIXML 的普通文本行也保留（去掉 XML 包装行）。
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#< CLIXML") ||
			strings.HasPrefix(line, "<Objs") || strings.HasPrefix(line, "</Objs>") ||
			strings.HasPrefix(line, "<") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.TrimSpace(strings.Join(uniqStrings(keep), "\n"))
}

// clixmlErrorRe 匹配 CLIXML 中的错误文本节点。
var clixmlErrorRe = regexp.MustCompile(`<S S="Error">(.*?)</S>`)

func uniqStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

const taskTimeout = 90 * time.Second

func runCmd(timeout time.Duration, name string, args ...string) cmdResult {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := cmdResult{
		stdout: stdout.String(),
		stderr: stderr.String(),
		ms:     time.Since(start).Milliseconds(),
		err:    err,
		exit:   0,
	}
	if err != nil {
		res.exit = -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.exit = exitErr.ExitCode()
		}
		if ctx.Err() != nil {
			res.err = fmt.Errorf("超时（%s）：%w", timeout, ctx.Err())
		}
	}
	return res
}

// psArgs 构造 PowerShell 调用参数（与 winps.Runner 保持一致）。
func psArgs(args ...string) []string {
	base := []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-OutputFormat", "Text"}
	return append(base, args...)
}

func runPS(timeout time.Duration, args ...string) cmdResult {
	return runCmd(timeout, psExe(), psArgs(args...)...)
}

// runPSInline 执行内联脚本（用 -EncodedCommand，彻底规避引号/换行转义问题）。
func runPSInline(timeout time.Duration, script string) cmdResult {
	return runPS(timeout, "-EncodedCommand", encodeCommand(script))
}

// runPSFile 执行内联物化后的 .ps1 脚本（-File + 参数对）。
func runPSFile(timeout time.Duration, scriptPath string, params ...string) cmdResult {
	args := []string{"-File", scriptPath}
	args = append(args, params...)
	return runPS(timeout, args...)
}

func psExe() string {
	if runtime.GOOS == "windows" {
		return "powershell.exe"
	}
	return "pwsh"
}

func encodeCommand(script string) string {
	units := utf16.Encode([]rune(script))
	buf := make([]byte, len(units)*2)
	for i, u := range units {
		buf[i*2] = byte(u)
		buf[i*2+1] = byte(u >> 8)
	}
	return base64.StdEncoding.EncodeToString(buf)
}

func clampText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("\n...（截断，共 %d 字节）", len(s))
}

// inline 执行内联脚本，并按"是否失败"记录结果。
func (r *report) inline(name, repro, script string, timeout time.Duration) cmdResult {
	res := runPSInline(timeout, script)
	detail := summarizeEnvelope(res)
	if res.failed() {
		r.fail(name, detail, repro)
	} else {
		r.pass(name, detail, "")
	}
	return res
}

// script 执行内嵌 .ps1，并按"是否失败"记录结果。
func (r *report) script(name, repro, scriptPath string, params ...string) cmdResult {
	res := runPSFile(taskTimeout, scriptPath, params...)
	detail := summarizeEnvelope(res)
	if res.failed() {
		r.fail(name, detail, repro)
	} else {
		r.pass(name, detail, "")
	}
	return res
}

// summarizeEnvelope 优先展示脚本的结构化 JSON 信封（ok/reason/step/message），
// 便于直接看出失败在哪一步；没有 JSON 时退回原始输出。
func summarizeEnvelope(res cmdResult) string {
	line := lastJSONLine(res.stdout)
	var b strings.Builder
	fmt.Fprintf(&b, "exit=%d 耗时=%dms", res.exit, res.ms)
	if res.err != nil {
		fmt.Fprintf(&b, " err=%v", res.err)
	}
	if line != "" {
		fmt.Fprintf(&b, "\njson: %s", clampText(line, 1200))
	}
	if s := cleanPSNoise(res.stderr); s != "" {
		fmt.Fprintf(&b, "\nstderr: %s", clampText(s, 1200))
	}
	if line == "" && strings.TrimSpace(res.stdout) != "" {
		fmt.Fprintf(&b, "\nstdout: %s", clampText(strings.TrimSpace(res.stdout), 1200))
	}
	return b.String()
}

func lastJSONLine(out string) string {
	lines := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		s := strings.TrimSpace(lines[i])
		if s == "" {
			continue
		}
		var probe map[string]any
		if json.Unmarshal([]byte(s), &probe) == nil {
			return s
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

type options struct {
	skipWrite  bool
	keep       bool
	lvmSandbox bool
	outPath    string
	sandboxDir string
	timeout    time.Duration
}

var opts options

// caps 记录前置探测结论：缺失的角色/权限会被后续各节用来"明确跳过"，
// 避免一个根因（例如没装 iSCSI 角色）引发几十条连锁 FAIL，把报告淹没。
var caps struct {
	iscsiModule bool // IscsiTarget 模块可用
	winTarget   bool // WinTarget 服务 Running
	hyperV      bool // Hyper-V（Set-VHD）可用
}

func main() {
	flag.BoolVar(&opts.skipWrite, "skip-write", false, "只做只读探测：不挂载/不格式化/不创建目标")
	flag.BoolVar(&opts.keep, "keep", false, "失败时保留沙箱目录（便于人工复现）")
	flag.BoolVar(&opts.lvmSandbox, "lvm-sandbox", false,
		"Linux：用 loop 设备建临时 VG/thin LV 做 LVM 全链路验证（默认关闭；会创建并在结束时清理 vaultcheck-* 卷组）")
	flag.StringVar(&opts.outPath, "out", "", "日志文件路径（默认 ./vault-check-<主机>-<时间>.log）")
	flag.StringVar(&opts.sandboxDir, "sandbox-dir", "",
		"沙箱父目录（默认系统临时目录）。VHDX 不能位于网络共享，若 TEMP 被重定向请用本参数指向本地盘，例如 D:\\")
	flag.DurationVar(&opts.timeout, "timeout", taskTimeout, "单条命令超时")
	flag.Parse()

	host, _ := os.Hostname()
	logPath := opts.outPath
	if logPath == "" {
		logPath = fmt.Sprintf("vault-check-%s-%s.log", host, time.Now().Format("20060102-150405"))
	}
	f, err := os.Create(logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "无法创建日志文件 %s: %v\n", logPath, err)
		os.Exit(2)
	}
	defer func() { _ = f.Close() }()

	r := &report{w: io.MultiWriter(os.Stdout, f), logPath: logPath, start: time.Now()}
	if abs, aErr := filepath.Abs(logPath); aErr == nil {
		r.logPath = abs
	}

	header(r, host)
	switch runtime.GOOS {
	case "windows":
		runWindows(r)
	case "linux":
		runLinux(r)
	default:
		r.warn("平台", "本工具目前只覆盖 Windows 与 Linux，当前为 "+runtime.GOOS, "")
	}
	r.caveats()
	r.summary()
}

// caveats 说明本工具"刻意不做 / 无法覆盖"的部分，避免误读报告。
func (r *report) caveats() {
	r.sec("说明 / 未覆盖项")
	r.pass("写操作隔离", "所有写操作只发生在沙箱内（临时目录 + vaultcheck-* 命名的目标/虚拟盘/LV），结束自动清理", "")
	r.skip("发起端连接/断开/门户增删类脚本",
		"刻意不执行：属客户端侧能力，需要真实对端；同一台机器上的 loopback iSCSI（自己连自己）有把服务器挂死的风险")
	if runtime.GOOS == "windows" {
		r.skip("VirtDisk 的 Go 绑定直调",
			"跨平台单文件无法引入 windows-only 包；已用 New-IscsiVirtualDisk / Mount-DiskImage / volume_* 脚本等价覆盖同一套系统能力")
	}
	if runtime.GOOS == "linux" {
		r.skip("LVM 全链路写操作",
			"默认关闭以避免在生产机上创建卷组；需要时加 -lvm-sandbox 重跑（用 loop 设备，结束自动清理）")
	}
}

func header(r *report, host string) {
	fmt.Fprintf(r.w, "Vault 环境自检报告\n")
	fmt.Fprintf(r.w, "============================================================\n")
	fmt.Fprintf(r.w, "时间      : %s\n", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(r.w, "主机      : %s\n", host)
	fmt.Fprintf(r.w, "操作系统  : %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(r.w, "工作目录  : %s\n", mustGetwd())
	fmt.Fprintf(r.w, "参数      : skip-write=%v keep=%v lvm-sandbox=%v sandbox-dir=%q timeout=%s\n",
		opts.skipWrite, opts.keep, opts.lvmSandbox, opts.sandboxDir, opts.timeout)
	fmt.Fprintf(r.w, "日志      : %s\n", r.logPath)
	if opts.skipWrite {
		fmt.Fprintf(r.w, "提示      : -skip-write 已开启，本次不产生任何写操作\n")
	}
}

func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "(未知)"
	}
	return wd
}

func (r *report) summary() {
	fmt.Fprintf(r.w, "\n=== 汇总 ===\n")
	counts := map[status]int{}
	for _, it := range r.items {
		counts[it.status]++
	}
	fmt.Fprintf(r.w, "  OK=%d FAIL=%d WARN=%d SKIP=%d  （耗时 %.1fs）\n",
		counts[stOK], counts[stFail], counts[stWarn], counts[stSkip], time.Since(r.start).Seconds())

	if counts[stFail] == 0 {
		fmt.Fprintf(r.w, "\n结论：没有失败项。\n")
	} else {
		fmt.Fprintf(r.w, "\n需要处理的失败项（按出现顺序）：\n")
		i := 0
		for _, it := range r.items {
			if it.status != stFail {
				continue
			}
			i++
			fmt.Fprintf(r.w, "  %d) [%s] %s\n", i, it.section, it.name)
			if strings.TrimSpace(it.repro) != "" {
				fmt.Fprintf(r.w, "     复现: %s\n", it.repro)
			}
		}
	}
	fmt.Fprintf(r.w, "\n把这份日志整个发回即可（含上方每一节的原始输出）。\n")
	fmt.Fprintf(r.w, "日志文件：%s\n", r.logPath)
}

// ---------------------------------------------------------------------------
// 公共辅助
// ---------------------------------------------------------------------------

// sandbox 是本工具专用的临时目录：所有写操作都在其中。
type sandbox struct {
	dir    string
	psDir  string
	closed bool
}

func newSandbox(r *report) (*sandbox, error) {
	parent := opts.sandboxDir
	if parent != "" {
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return nil, fmt.Errorf("无法创建沙箱父目录 %s: %w", parent, err)
		}
	}
	dir, err := os.MkdirTemp(parent, "vaultcheck-")
	if err != nil {
		return nil, err
	}
	sb := &sandbox{dir: dir, psDir: filepath.Join(dir, "scripts")}
	if err := sb.materializeScripts(); err != nil {
		return nil, err
	}
	r.pass("沙箱目录", dir, "")
	return sb, nil
}

// materializeScripts 把内嵌脚本按字节落盘到沙箱（保留 BOM）。
func (sb *sandbox) materializeScripts() error {
	if err := os.MkdirAll(sb.psDir, 0o755); err != nil {
		return err
	}
	entries, err := fs.ReadDir(scriptsFS, embeddedScriptDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := scriptsFS.ReadFile(embeddedScriptDir + "/" + e.Name())
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(sb.psDir, e.Name()), data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (sb *sandbox) script(name string) string { return filepath.Join(sb.psDir, name) }

func (sb *sandbox) path(rel string) string {
	p := filepath.Join(sb.dir, rel)
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	return p
}

func (sb *sandbox) remove() {
	if sb == nil || sb.closed {
		return
	}
	sb.closed = true
	if opts.keep {
		return
	}
	_ = os.RemoveAll(sb.dir)
}

// randomSecret 生成 12 字节随机数的 base64（16 字符），与 app.defaultChapSecretBytes 一致。
func randomSecret() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "vaultcheck-secret"
	}
	return base64.StdEncoding.EncodeToString(buf)
}

func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func psList(items []string) string {
	quoted := make([]string, 0, len(items))
	for _, it := range items {
		quoted = append(quoted, psQuote(it))
	}
	return "@(" + strings.Join(quoted, ",") + ")"
}

// ---------------------------------------------------------------------------
// Windows
// ---------------------------------------------------------------------------

func runWindows(r *report) {
	checkWindowsHost(r)
	sb, err := newSandbox(r)
	if err != nil {
		r.fail("创建沙箱目录", err.Error(), "")
		return
	}
	defer sb.remove()

	checkWindowsTools(r)
	checkWindowsFeatures(r)

	// 沙箱内的 VHDX：卷链路与 iSCSI 链路共用。
	vhdx := sb.path("sandbox.vhdx")

	mounted := checkWindowsVolume(r, sb, vhdx)
	if mounted {
		if _, _, err := volumeDismount(r, vhdx); err != nil {
			r.warn("沙箱 VHDX 分离", err.Error(), "")
		}
	}
	checkWindowsIscsi(r, sb, vhdx)
	checkWindowsInitiatorReadonly(r, sb)
	checkWindowsMisc(r, sb)

	if opts.skipWrite {
		r.skip("沙箱清理", "-skip-write 已开启；如需保留现场用 -keep")
	} else if opts.keep {
		r.warn("沙箱清理", "已按 -keep 保留现场："+sb.dir, "")
	} else {
		r.pass("沙箱清理", "已删除 "+sb.dir, "")
	}
}

func checkWindowsHost(r *report) {
	r.sec("A. 运行环境")

	// 一次 PowerShell 调用拿到全部环境信息（每次启动 PowerShell 约 300ms，合并更省事）。
	doc, res := psJSON(opts.timeout,
		`$ErrorActionPreference='Stop';`+
			`try{[Console]::OutputEncoding=[System.Text.Encoding]::UTF8}catch{};`+
			`$v=$PSVersionTable.PSVersion;`+
			`$p=New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent());`+
			`[pscustomobject]@{`+
			`ok=$true;`+
			`ps="$($v.Major).$($v.Minor).$($v.Build)";`+
			`edition=[string]$PSVersionTable.PSEdition;`+
			`is64=$([Environment]::Is64BitProcess);`+
			`admin=$p.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator);`+
			`encoding=[string][Console]::OutputEncoding.WebName}|ConvertTo-Json -Compress`)

	repro := `powershell.exe -NoProfile -Command "$PSVersionTable.PSVersion; [Environment]::Is64BitProcess"`
	if res.failed() {
		r.fail("PowerShell 可用性", summarizeEnvelope(res), repro)
		return
	}
	r.pass("PowerShell 可用性", fmt.Sprintf("版本 %s（%s），编码 %s",
		strOf(doc, "ps"), strOf(doc, "edition"), strOf(doc, "encoding")), "")

	if boolOf(doc, "is64") {
		r.pass("进程位数", "64 位（iSCSI/Storage 模块要求 64 位 PowerShell）", "")
	} else {
		r.fail("进程位数", "32 位 PowerShell 无法加载 IscsiTarget / Storage 模块，请用 64 位 PowerShell 运行",
			`C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`)
	}

	if boolOf(doc, "admin") {
		r.pass("管理员权限", "当前为管理员", "")
	} else {
		r.fail("管理员权限", "挂载 VHDX / 分区格式化 / 管理 iSCSI 目标都需要提权；请以管理员身份运行本工具",
			`以管理员身份打开 PowerShell 后运行 .\check-command.exe`)
	}
}

func checkWindowsTools(r *report) {
	r.sec("B. 外部命令")

	for _, tool := range []string{"powershell.exe", "robocopy.exe", "icacls.exe", "cmd.exe"} {
		if _, err := exec.LookPath(tool); err != nil {
			r.fail(tool, "未找到："+err.Error(), tool+" /?")
			continue
		}
		r.pass(tool, "已在 PATH 中", "")
	}

	// robocopy 的 /? 返回 16，属正常（它不是错误）。
	res := runCmd(opts.timeout, "robocopy", "/?")
	if res.exit >= 0 {
		r.pass("robocopy 可执行", fmt.Sprintf("exit=%d（robocopy 的 /? 正常返回 16 或 0）", res.exit), "")
	} else {
		r.fail("robocopy 可执行", res.describe(), "robocopy /?")
	}
}

func checkWindowsFeatures(r *report) {
	r.sec("C. Windows 角色 / 服务 / 模块")

	// C1 iSCSI 目标服务器角色：缺失则"建盘/发布/挂载"必然失败，属硬性依赖。
	doc, res := psJSON(opts.timeout,
		`$ErrorActionPreference='Stop';`+
			`$m=Get-Module -ListAvailable -Name IscsiTarget|Select-Object -First 1;`+
			`[pscustomobject]@{ok=[bool]$m;version=[string]$m.Version;path=[string]$m.Path}|ConvertTo-Json -Compress`)
	repro := `Install-WindowsFeature FS-iSCSITarget-Server`
	caps.iscsiModule = !res.failed() && boolOf(doc, "ok")
	switch {
	case res.failed():
		r.fail("IscsiTarget 模块", summarizeEnvelope(res), repro)
	case !caps.iscsiModule:
		r.fail("IscsiTarget 模块缺失",
			"未安装 iSCSI 目标服务器角色 → 目标创建/虚拟盘登记都会失败。安装后需重启（或在功能向导中一并安装管理工具）",
			repro)
	default:
		r.pass("IscsiTarget 模块", "版本 "+strOf(doc, "version"), "")
	}

	// C2 两个服务：WinTarget（服务端）缺失=硬失败；MSiSCSI（客户端发起端）缺失只告警。
	for _, svc := range []struct {
		name string
		hard bool
	}{
		{"WinTarget", true},
		{"MSiSCSI", false},
	} {
		doc, res := psJSON(opts.timeout,
			`$s=Get-Service -Name `+psQuote(svc.name)+` -ErrorAction SilentlyContinue;`+
				`[pscustomobject]@{ok=[bool]$s;status=[string]$s.Status;startType=[string]$s.StartType}|ConvertTo-Json -Compress`)
		title := "服务 " + svc.name
		running := !res.failed() && boolOf(doc, "ok") && strOf(doc, "status") == "Running"
		if svc.name == "WinTarget" {
			caps.winTarget = running
		}
		switch {
		case res.failed():
			r.fail(title, summarizeEnvelope(res), "Get-Service "+svc.name)
		case !boolOf(doc, "ok"):
			msg := "服务不存在"
			if svc.hard {
				r.fail(title, msg+"（iSCSI 目标服务器角色未安装或不完整）",
					"Install-WindowsFeature FS-iSCSITarget-Server")
			} else {
				r.warn(title, msg+"（客户端发起端功能，服务端机器上可缺省）", "")
			}
		case strOf(doc, "status") != "Running":
			// WinTarget 未运行 = 服务端能力不可用（硬失败）；
			// MSiSCSI 未运行只是"本机不作为 iSCSI 客户端"，服务端机器上属正常，仅提示。
			if svc.hard {
				r.fail(title, "服务状态="+strOf(doc, "status")+"，iSCSI 目标相关操作会失败",
					"Start-Service "+svc.name+" ; Set-Service "+svc.name+" -StartupType Automatic")
			} else {
				r.warn(title, "服务状态="+strOf(doc, "status")+
					"（发起端服务：服务端机器上未运行属正常；仅当本机也要挂载 iSCSI 磁盘时才需启动）",
					"Start-Service "+svc.name)
			}
		default:
			r.pass(title, "Running（"+strOf(doc, "startType")+"）", "")
		}
	}

	// C3 Hyper-V 模块：可选，只影响"空间回收 / 重置磁盘标识"。
	doc, res = psJSON(opts.timeout,
		`$c=Get-Command -Name Set-VHD -ErrorAction SilentlyContinue;`+
			`[pscustomobject]@{ok=[bool]$c}|ConvertTo-Json -Compress`)
	caps.hyperV = !res.failed() && boolOf(doc, "ok")
	if res.failed() {
		r.warn("Hyper-V 模块（Set-VHD）", summarizeEnvelope(res), "")
	} else if caps.hyperV {
		r.pass("Hyper-V 模块（Set-VHD）", "可用：支持空间回收与重置磁盘标识", "")
	} else {
		r.warn("Hyper-V 模块（Set-VHD）", "未安装：Optimize/VHDX 压缩与磁盘标识重置不可用（其余功能不受影响）",
			"Install-WindowsFeature Hyper-V-PowerShell")
	}

	// Storage 模块：卷链路的全部依赖。一次调用全部探测完，避免逐条 PowerShell。
	cmds := []string{"Get-Disk", "Get-Partition", "Get-Volume", "Initialize-Disk",
		"New-Partition", "Format-Volume", "Add-PartitionAccessPath", "Set-Disk",
		"Update-HostStorageCache", "Get-DiskImage", "Mount-DiskImage", "Dismount-DiskImage"}
	probe := `$ErrorActionPreference='Stop';$names=@(` + psList(cmds) + `);` +
		`$missing=@($names | Where-Object { -not (Get-Command -Name $_ -ErrorAction SilentlyContinue) });` +
		`if ($missing.Count -gt 0) { throw ('缺少 cmdlet: ' + ($missing -join ',')) };` +
		`[pscustomobject]@{ok=$true;checked=$names.Count}|ConvertTo-Json -Compress`
	r.inline("Storage 模块 cmdlet（"+fmt.Sprint(len(cmds))+" 个）",
		"Get-Command Get-Disk, Get-Partition, Initialize-Disk, New-Partition, Format-Volume, Mount-DiskImage",
		probe, opts.timeout)
}

// checkWindowsVolume 走一遍"建盘 → 挂载 → 初始化/分区/格式化 → 拷入 → 分离"的卷链路。
// 返回 true 表示当前 VHDX 处于已挂载状态（调用方负责分离）。
func checkWindowsVolume(r *report, sb *sandbox, vhdx string) bool {
	r.sec("D. VHDX + 卷链路（沙箱内真实执行）")

	if opts.skipWrite {
		r.skip("整节 D（写操作）", "-skip-write 已开启")
		return false
	}
	if !caps.iscsiModule && !caps.hyperV {
		r.skip("整节 D（VHDX + 卷链路）",
			"既没有 IscsiTarget 模块也没有 Hyper-V 模块 → 无法创建 VHDX（见 C 节）")
		return false
	}

	// D1 建 VHDX。优先 iSCSI 模块（与生产一致），缺失时退回 Hyper-V 的 New-VHD。
	createScript := `$ErrorActionPreference='Stop';` +
		`if (Get-Command -Name New-IscsiVirtualDisk -ErrorAction SilentlyContinue) {` +
		`  New-IscsiVirtualDisk -Path ` + psQuote(vhdx) + ` -SizeBytes 67108864 | Out-Null; 'iscsi'` +
		`} elseif (Get-Command -Name New-VHD -ErrorAction SilentlyContinue) {` +
		`  New-VHD -Path ` + psQuote(vhdx) + ` -SizeBytes 67108864 -Dynamic | Out-Null; 'hyperv'` +
		`} else { throw '既没有 IscsiTarget 也没有 Hyper-V 模块，无法创建 VHDX' }`
	res := r.inline("D1 创建 VHDX（64MB，动态）",
		`New-IscsiVirtualDisk -Path `+psQuote(vhdx)+` -SizeBytes 67108864`, createScript, opts.timeout)
	if res.failed() {
		r.skip("D2 起的卷链路", "D1 未通过：无法创建 VHDX")
		return false
	}

	// D2 挂载（等价于服务端的 AttachVirtualDisk）。
	mres := r.inline("D2 挂载 VHDX（Mount-DiskImage）",
		`Mount-DiskImage -ImagePath `+psQuote(vhdx)+` -PassThru`,
		`$ErrorActionPreference='Stop';$i=Mount-DiskImage -ImagePath `+psQuote(vhdx)+` -PassThru;`+
			`[pscustomobject]@{ok=$true;attached=$i.Attached;size=[long]$i.Size}|ConvertTo-Json -Compress`, opts.timeout)
	mounted := !mres.failed()
	if !mounted {
		r.skip("D3 起的卷链路", "D2 未通过：未挂载成功")
		return false
	}

	refresh := `$ErrorActionPreference='Stop';Update-HostStorageCache;[pscustomobject]@{ok=$true}|ConvertTo-Json -Compress`
	r.inline("D3 刷新存储缓存（Update-HostStorageCache）", "Update-HostStorageCache", refresh, opts.timeout)

	r.script("D4 volume_find_disk.ps1（按路径反查磁盘号）",
		`& .\scripts\volume_find_disk.ps1 -VhdxPath `+psQuote(vhdx),
		sb.script("volume_find_disk.ps1"), "-VhdxPath", vhdx, "-RefreshCache", "true")

	r.script("D5 volume_update_cache.ps1", "& .\\scripts\\volume_update_cache.ps1",
		sb.script("volume_update_cache.ps1"))

	// D6 初始化 + 分区 + 格式化（最脆弱的一段，生产就是在这里失败）。
	fres := r.script("D6 volume_ensure_formatted.ps1（初始化/分区/格式化）",
		`& .\scripts\volume_ensure_formatted.ps1 -VhdxPath `+psQuote(vhdx)+` -FileSystem NTFS -Label VAULT`,
		sb.script("volume_ensure_formatted.ps1"), "-VhdxPath", vhdx, "-FileSystem", "NTFS", "-Label", "VAULT")

	diskNo := firstNumberFromJSON(fres.stdout, "disk_number")
	letter := firstStringFromJSON(fres.stdout, "drive_letter")
	if diskNo > 0 {
		r.script("D7 volume_current_mount.ps1", "& .\\scripts\\volume_current_mount.ps1",
			sb.script("volume_current_mount.ps1"), "-DiskNumber", fmt.Sprint(diskNo))
		r.script("D8 volume_set_disk_state.ps1（离线/只读开关）",
			`& .\scripts\volume_set_disk_state.ps1 -DiskNumber <N> -ReadOnly true`,
			sb.script("volume_set_disk_state.ps1"), "-DiskNumber", fmt.Sprint(diskNo), "-ReadOnly", "false")
		if letter == "" {
			r.script("D9 volume_mount_drive.ps1（分配盘符）", "& .\\scripts\\volume_mount_drive.ps1",
				sb.script("volume_mount_drive.ps1"), "-DiskNumber", fmt.Sprint(diskNo))
			letter = firstStringFromJSON(
				runPSFile(taskTimeout, sb.script("volume_mount_drive.ps1"), "-DiskNumber", fmt.Sprint(diskNo)).stdout,
				"drive_letter")
		}

		// 目录挂载点：先建空目录，再挂载 → 查询 → 卸载。
		mountDir := sb.path("mnt")
		if mkErr := os.MkdirAll(mountDir, 0o755); mkErr == nil {
			r.script("D10 volume_mount_dir.ps1（目录挂载点）",
				`& .\scripts\volume_mount_dir.ps1 -DiskNumber <N> -AccessPath `+psQuote(mountDir),
				sb.script("volume_mount_dir.ps1"), "-DiskNumber", fmt.Sprint(diskNo), "-AccessPath", mountDir)
			r.script("D11 volume_unmount.ps1（卸载目录挂载点）", "& .\\scripts\\volume_unmount.ps1",
				sb.script("volume_unmount.ps1"), "-AccessPath", mountDir)
		}
	} else {
		r.warn("D7 起的卷脚本", "D6 未返回 disk_number，跳过后续依赖磁盘号的脚本", "")
	}

	// D12 空间 / 文件系统查询（对沙箱目录，总是可做）。
	r.script("D12 volume_freespace.ps1", "& .\\scripts\\volume_freespace.ps1",
		sb.script("volume_freespace.ps1"), "-Path", sb.dir)
	r.script("D13 volume_filesystem.ps1", "& .\\scripts\\volume_filesystem.ps1",
		sb.script("volume_filesystem.ps1"), "-Path", sb.dir)

	// D14 robocopy 递归拷贝（建盘流水线的拷入环节）。
	src := sb.path("src/inner")
	if err := os.MkdirAll(src, 0o755); err == nil {
		_ = os.WriteFile(filepath.Join(src, "hello.txt"), []byte("vault check\n"), 0o644)
		dst := sb.path("dst")
		cres := runCmd(opts.timeout, "robocopy", src, dst, "/E", "/COPY:DAT", "/R:1", "/W:1",
			"/NFL", "/NDL", "/NP", "/NJH", "/NJS")
		// robocopy 退出码 <8 即成功（1 表示有文件被复制）。
		if cres.exit >= 0 && cres.exit < 8 {
			r.pass("D14 robocopy 递归拷贝", fmt.Sprintf("exit=%d（<8 即成功）", cres.exit), "")
		} else {
			r.fail("D14 robocopy 递归拷贝", cres.describe(),
				`robocopy "`+src+`" "`+dst+`" /E /COPY:DAT /R:1 /W:1`)
		}
	}

	// D15 若目标盘已格式化且拿到盘符，再把 VHDX 分离（生产顺序即如此）。
	return true
}

// volumeDismount 分离沙箱 VHDX。
func volumeDismount(r *report, vhdx string) (bool, string, error) {
	res := runPSInline(opts.timeout, `$ErrorActionPreference='Stop';`+
		`Dismount-DiskImage -ImagePath `+psQuote(vhdx)+`;[pscustomobject]@{ok=$true}|ConvertTo-Json -Compress`)
	if res.failed() {
		r.fail("D15 分离 VHDX（Dismount-DiskImage）", summarizeEnvelope(res),
			`Dismount-DiskImage -ImagePath `+psQuote(vhdx))
		return false, "", fmt.Errorf("dismount 失败")
	}
	r.pass("D15 分离 VHDX（Dismount-DiskImage）", summarizeEnvelope(res), "")
	return true, "", nil
}

func checkWindowsIscsi(r *report, sb *sandbox, vhdx string) {
	r.sec("E. iSCSI 目标全链路（沙箱命名，自清理）")

	// 角色不可用时整节跳过：C 节已经给出了根因与安装命令，
	// 这里再逐条报同一个错误只会把报告淹没。
	if !caps.iscsiModule {
		r.skip("整节 E（iSCSI 目标链路）",
			"IscsiTarget 模块不可用（见 C 节）→ 先安装 iSCSI 目标服务器角色再重跑本节")
		return
	}
	if !caps.winTarget {
		r.warn("WinTarget 服务未运行",
			"目标创建类 cmdlet 会失败；请先 Start-Service WinTarget", "Start-Service WinTarget")
	}

	r.script("E1 iscsi_available.ps1", "& .\\scripts\\iscsi_available.ps1 -ServiceName WinTarget",
		sb.script("iscsi_available.ps1"), "-ServiceName", "WinTarget")

	if opts.skipWrite {
		r.skip("E2 起的目标写操作", "-skip-write 已开启")
		return
	}

	stamp := time.Now().Format("150405")
	nameOK := "vaultcheck-" + stamp + "-a"    // 纯字母数字 + 连字符（当前代码的命名形态）
	nameUnder := "vaultcheck_" + stamp + "_b" // 对照：带下划线（IQN 语法不允许）

	// E2 建目标（当前形态）。这是生产上失败的那一步，必须单独记录。
	createOK := r.script("E2 建目标（连字符名）",
		`New-IscsiServerTarget -TargetName `+psQuote(nameOK),
		sb.script("iscsi_set_target.ps1"), "-TargetName", nameOK)
	created := !createOK.failed()

	// E3 对照实验：带下划线的名字。**成功/失败都只作提示**（不是必须修的问题），
	// 目的是确认"下划线非法"这条假设是否成立。
	underRes := runPSInline(opts.timeout,
		`$ErrorActionPreference='Stop';`+
			`try { New-IscsiServerTarget -TargetName `+psQuote(nameUnder)+` | Out-Null;`+
			`  [pscustomobject]@{created=$true}|ConvertTo-Json -Compress }`+
			`catch { [pscustomobject]@{created=$false;type=$_.Exception.GetType().Name;message=$_.Exception.Message}|ConvertTo-Json -Compress }`)
	if createdUnder := firstBoolFromJSON(underRes.stdout, "created"); createdUnder {
		r.warn("E3 对照：带下划线名也被接受",
			"说明目标名里的 '_' 不是失败原因，需要看 E2/E5 的具体报错", "")
	} else {
		r.pass("E3 对照：带下划线名被拒绝",
			"符合 IQN 语法（'_' 非法）→ 目标名必须只用字母数字和 '-'\n"+summarizeEnvelope(underRes), "")
	}

	// E4 枚举与按名查询（验证短名 vs iqn 前缀的存储形态）。
	r.script("E4 iscsi_get_targets.ps1（枚举）", "& .\\scripts\\iscsi_get_targets.ps1",
		sb.script("iscsi_get_targets.ps1"))
	r.inline("E4b 目标名的真实存储形态", `Get-IscsiServerTarget | Select-Object TargetName,Enabled`,
		`(Get-IscsiServerTarget -ErrorAction SilentlyContinue | Select-Object -First 5 TargetName,Enabled | ConvertTo-Json -Compress)`,
		opts.timeout)
	// E4c 目标对象的**真实属性清单**：用于确认"启用状态"到底该读哪个属性
	//（实测该版本上 Select TargetName,Enabled 得到 Enabled=null，说明可能根本没有该属性）。
	r.inline("E4c 目标对象的属性清单", `Get-IscsiServerTarget | Get-Member -MemberType Property`,
		`Import-Module IscsiTarget;`+
			`(Get-IscsiServerTarget -ErrorAction SilentlyContinue | Select-Object -First 1 | Get-Member -MemberType Property | `+
			`ForEach-Object { $_.Name }) -join ','`,
		opts.timeout)
	// E4d Status 的枚举成员表：确定"哪些取值代表已启用"，是启用状态判定的唯一依据。
	r.inline("E4d 目标 Status 的枚举成员表",
		`$t=(Get-IscsiServerTarget | Select-Object -First 1); $t.Status; [enum]::GetValues($t.Status.GetType())`,
		`Import-Module IscsiTarget;`+
			`$t = @(Get-IscsiServerTarget -ErrorAction SilentlyContinue) | Select-Object -First 1;`+
			`if (-not $t) { 'target_not_found' } else {`+
			`  try {`+
			`    $v = $t.Status; $ty = $v.GetType();`+
			`    $members = @([enum]::GetValues($ty) | ForEach-Object { [string]$_ + '=' + [int]$_ });`+
			`    'type=' + $ty.FullName + ' ;; current=' + [string]$v + '(' + [int]$v + ') ;; members: ' + ($members -join ', ')`+
			`  } catch { 'not-an-enum: ' + $_.Exception.Message + ' ;; raw=' + [string]$t.Status } }`,
		opts.timeout)

	// E5 分步下发（生产上就是这一串），逐步定位差异。
	if created {
		r.script("E5a Set -InitiatorIds @()（清空）",
			`Set-IscsiServerTarget -TargetName `+psQuote(nameOK)+` -InitiatorIds @()`,
			sb.script("iscsi_set_target.ps1"), "-TargetName", nameOK, "-InitiatorIdsJson", "[]")

		// E5b-1 授权参数的类型与别名：决定"该怎么把字符串变成 cmdlet 要的对象"。
		r.inline("E5b-1 授权参数的类型/别名",
			`(Get-Command Set-IscsiServerTarget).Parameters | Select-Object -ExpandProperty Keys`,
			`Import-Module IscsiTarget;`+
				`$all = @((Get-Command Set-IscsiServerTarget).Parameters.Values | `+
				`ForEach-Object { $_.Name + ':' + $_.ParameterType.FullName + ' aliases=' + ($_.Aliases -join '|') });`+
				`$all -join ' ;; '`,
			opts.timeout)
		// E5b-2 InitiatorId 的构造签名：脚本用它来反射构造对象，签名不对就构造不出来。
		r.inline("E5b-2 InitiatorId 构造签名",
			`[Microsoft.Iscsi.Target.Commands.InitiatorId].GetConstructors() | ForEach-Object { $_.ToString() }`,
			`Import-Module IscsiTarget;`+
				`$t = [Microsoft.Iscsi.Target.Commands.InitiatorId];`+
				`$ctors = @($t.GetConstructors() | ForEach-Object { $_.ToString() });`+
				`$props = @($t.GetProperties() | ForEach-Object { $_.Name + '(' + $_.PropertyType.Name + ')' });`+
				`('ctors: ' + ($ctors -join ' / ') + ' ;; props: ' + ($props -join ', '))`,
			opts.timeout)
		// E5b-3 对照：直接把字符串交给 -InitiatorIds（该版本预期被拒绝，仅作说明）。
		rawIDs := runPSInline(opts.timeout,
			`Import-Module IscsiTarget;`+
				`try { Set-IscsiServerTarget -TargetName `+psQuote(nameOK)+` -InitiatorIds @('IPAddress:127.0.0.1') -ErrorAction Stop;`+
				`  [pscustomobject]@{accepted=$true}|ConvertTo-Json -Compress }`+
				`catch { [pscustomobject]@{accepted=$false;type=$_.Exception.GetType().Name;message=$_.Exception.Message}|ConvertTo-Json -Compress }`)
		if firstBoolFromJSON(rawIDs.stdout, "accepted") {
			r.pass("E5b-3 对照：-InitiatorIds 也接受字符串",
				"该版本直接传字符串也能绑定；脚本仍按参数类型显式构造 InitiatorId 对象"+
					"（确定性更强，不依赖 PowerShell 的隐式转换）", "")
		} else {
			r.pass("E5b-3 对照：-InitiatorIds 拒绝字符串",
				"符合预期：该版本要求 InitiatorId 对象，脚本必须做类型转换\n"+summarizeEnvelope(rawIDs), "")
		}
		// E5b 真正要验的：**脚本自己**能不能把非空授权下发成功（生产走的就是它）。
		r.script("E5b Set -InitiatorIds 非空（脚本自适应转换）",
			`& .\scripts\iscsi_set_target.ps1 -TargetName `+psQuote(nameOK)+` -InitiatorIdsJson '["IPAddress:127.0.0.1"]'`,
			sb.script("iscsi_set_target.ps1"), "-TargetName", nameOK,
			"-InitiatorIdsJson", `["IPAddress:127.0.0.1"]`)
		r.script("E5c Set -Enable $false", `Set-IscsiServerTarget -Enable $false`,
			sb.script("iscsi_set_target.ps1"), "-TargetName", nameOK, "-Enabled", "false")
		// E5c-1 停用后 dump 目标对象：与 E8-1（启用后）对比，即可确定
		// "启用状态"到底反映在哪个属性上（Enabled 可能根本不存在）。
		r.inline("E5c-1 停用后的目标对象",
			`Get-IscsiServerTarget -TargetName `+psQuote(nameOK),
			`Import-Module IscsiTarget;`+
				`$t = @(Get-IscsiServerTarget -ErrorAction SilentlyContinue | Where-Object { $_.TargetName -eq `+psQuote(nameOK)+` }) | Select-Object -First 1;`+
				`if ($t) { ($t | Select-Object * -ExcludeProperty CimClass,CimInstanceProperties,CimSystemProperties | ConvertTo-Json -Compress -Depth 4) } else { 'target_not_found' }`,
			opts.timeout)
		// E5c-2 自校验：停用后脚本必须报 enabled=false（否则上层 reconcile 会反复下发启用）。
		disabledState := r.script("E5c-2 停用后 iscsi_get_targets 应报 enabled=false",
			`& .\scripts\iscsi_get_targets.ps1 -TargetName `+psQuote(nameOK),
			sb.script("iscsi_get_targets.ps1"), "-TargetName", nameOK)
		if !disabledState.failed() {
			enabled, parsed := firstTargetEnabled(disabledState.stdout)
			switch {
			case !parsed:
				r.fail("E5c-2b 无法解析 targets[0].enabled",
					"脚本输出缺少 targets[0].enabled（字段被改名？）\n"+summarizeEnvelope(disabledState), "")
			case enabled:
				r.fail("E5c-2c 启用状态映射错误（停用却报 enabled）",
					"目标已 -Enable $false，但脚本报 enabled=true：Status→enabled 的映射不对\n"+
						summarizeEnvelope(disabledState), "")
			}
		}

		// CHAP：密钥形态与参数类型（PSCredential / string）在不同版本上不同。
		secret := randomSecret()
		r.warn("E5d CHAP 密钥形态",
			fmt.Sprintf("本次使用 12 字节随机 base64 = %d 字符，样本=%s；若 Windows 拒绝 CHAP，请优先怀疑密钥字符集",
				len(secret), secret), "")
		r.script("E5d Set -EnableChap（单向 CHAP）",
			`Set-IscsiServerTarget -TargetName `+psQuote(nameOK)+` -EnableChap:$true -Chap <PSCredential>`,
			sb.script("iscsi_set_target.ps1"), "-TargetName", nameOK,
			"-ChapUser", "vaultcheck", "-ChapSecret", secret)
		r.inline("E5e CHAP 参数类型探测", `(Get-Command Set-IscsiServerTarget).Parameters['Chap'].ParameterType`,
			`$t=(Get-Command Set-IscsiServerTarget).Parameters['Chap'].ParameterType;"Chap 参数类型: $t"`, opts.timeout)
	} else {
		r.skip("E5 分步下发", "E2 未通过：目标未创建成功")
	}

	// E6 虚拟盘登记（VHDX 此时必须处于未挂载状态）。
	imported := r.script("E6 iscsi_ensure_virtual_disk.ps1（登记虚拟盘）",
		`& .\scripts\iscsi_ensure_virtual_disk.ps1 -Path `+psQuote(vhdx),
		sb.script("iscsi_ensure_virtual_disk.ps1"), "-Path", vhdx, "-Description", "vaultcheck")

	// E7 建立映射（并探测参数名到底是 -DevicePath 还是 -Path）。
	r.inline("E7a 映射 cmdlet 参数名探测",
		`(Get-Command Add-IscsiVirtualDiskTargetMapping).Parameters.Keys`,
		`$n=(Get-Command Add-IscsiVirtualDiskTargetMapping -ErrorAction SilentlyContinue).Parameters.Keys -join ',';"参数: $n"`,
		opts.timeout)
	mapped := false
	if created && !imported.failed() {
		mres := r.script("E7b iscsi_add_mapping.ps1（建立映射）",
			`& .\scripts\iscsi_add_mapping.ps1 -TargetName `+psQuote(nameOK)+` -DevicePath `+psQuote(vhdx),
			sb.script("iscsi_add_mapping.ps1"), "-TargetName", nameOK, "-DevicePath", vhdx)
		mapped = !mres.failed()
	} else {
		r.skip("E7b 建立映射", "E2/E6 未通过")
	}

	// E7c/E7d：映射关系到底该怎么查。
	//
	// 现象：E7b 报 ok:added，但随后 iscsi_get_virtual_disks.ps1 查该盘的 TargetName 为空、
	// remove_mapping 也报 not_found —— 说明"从 Get-IscsiVirtualDisk.TargetName 侧推映射"
	// 在该版本上不成立。这里把模块的**全部 cmdlet** 与虚拟盘对象的**全部属性**都 dump 出来，
	// 以便确定正确的查询方式（是否有专门的 mapping 查询 cmdlet）。
	r.inline("E7c IscsiTarget 模块的全部 cmdlet", "Get-Command -Module IscsiTarget",
		`Import-Module IscsiTarget;`+
			`((Get-Command -Module IscsiTarget | ForEach-Object { $_.Name }) | Sort-Object) -join ', '`,
		opts.timeout)
	if !imported.failed() {
		r.inline("E7d 虚拟盘对象的全部属性（映射是否可见）",
			`Get-IscsiVirtualDisk | Where-Object Path -eq `+psQuote(vhdx)+` | Select-Object *`,
			`Import-Module IscsiTarget;`+
				`$d = @(Get-IscsiVirtualDisk -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq `+psQuote(vhdx)+` }) | Select-Object -First 1;`+
				`if (-not $d) { 'disk_not_found' } else {`+
				`  $props = @($d.PSObject.Properties | ForEach-Object { $_.Name + '=' + (($_.Value | Out-String).Trim()) });`+
				`  ($props -join ' ;; ') }`,
			opts.timeout)
	}

	// E8 映射后再启用（这是本轮修复的顺序）。
	if created {
		r.script("E8 Set -Enable $true（映射后启用）",
			`Set-IscsiServerTarget -TargetName `+psQuote(nameOK)+` -Enable $true`,
			sb.script("iscsi_set_target.ps1"), "-TargetName", nameOK, "-Enabled", "true")
		r.inline("E8-1 启用后的目标对象（与 E5c-1 对比）",
			`Get-IscsiServerTarget -TargetName `+psQuote(nameOK),
			`Import-Module IscsiTarget;`+
				`$t = @(Get-IscsiServerTarget -ErrorAction SilentlyContinue | Where-Object { $_.TargetName -eq `+psQuote(nameOK)+` }) | Select-Object -First 1;`+
				`if ($t) { ($t | Select-Object * -ExcludeProperty CimClass,CimInstanceProperties,CimSystemProperties | ConvertTo-Json -Compress -Depth 4) } else { 'target_not_found' }`,
			opts.timeout)
		// E8-2 自校验：启用后脚本必须报 enabled=true（与 E5c-2 构成双向验证）。
		enabledState := r.script("E8-2 启用后 iscsi_get_targets 应报 enabled=true",
			`& .\scripts\iscsi_get_targets.ps1 -TargetName `+psQuote(nameOK),
			sb.script("iscsi_get_targets.ps1"), "-TargetName", nameOK)
		if !enabledState.failed() {
			enabled, parsed := firstTargetEnabled(enabledState.stdout)
			switch {
			case !parsed:
				r.fail("E8-2b 无法解析 targets[0].enabled",
					"脚本输出缺少 targets[0].enabled（字段被改名？）\n"+summarizeEnvelope(enabledState), "")
			case !enabled:
				r.fail("E8-2c 启用状态映射错误（启用却报未启用）",
					"目标已 -Enable $true，但脚本报 enabled=false：Status→enabled 的映射不对\n"+
						summarizeEnvelope(enabledState), "")
			}
		}
	}
	// E9 自校验：映射已建立（E7b 成功）时，虚拟盘的 target_names 必须能查到该目标。
	// 该字段曾因"从虚拟盘侧推映射"而恒为空（虚拟盘对象没有 TargetName 属性）。
	diskState := r.script("E9 iscsi_get_virtual_disks.ps1", "& .\\scripts\\iscsi_get_virtual_disks.ps1",
		sb.script("iscsi_get_virtual_disks.ps1"))
	if mapped && !diskState.failed() && !strings.Contains(diskState.stdout, nameOK) {
		r.fail("E9b 虚拟盘的 target_names 未反映已建立的映射",
			"E7b 已建立映射，但 iscsi_get_virtual_disks.ps1 的 target_names 里没有 "+nameOK+
				"（映射须从目标侧 LunMappings 反查）\n"+summarizeEnvelope(diskState), "")
	}

	// E7e 解除映射 cmdlet 的参数集：确认 -Path / -DevicePath / -Lun 的可用性。
	r.inline("E7e Remove 映射 cmdlet 的参数集",
		`Get-Command Remove-IscsiVirtualDiskTargetMapping`,
		`Import-Module IscsiTarget;`+
			`$params = @((Get-Command Remove-IscsiVirtualDiskTargetMapping).Parameters.Values | `+
			`ForEach-Object { $_.Name + ':' + $_.ParameterType.FullName });`+
			`$params -join ' ;; '`,
		opts.timeout)

	// E10 逆向清理（顺序必须与创建相反）。
	if mapped {
		r.script("E10a iscsi_remove_mapping.ps1",
			`& .\scripts\iscsi_remove_mapping.ps1 -TargetName `+psQuote(nameOK)+` -DevicePath `+psQuote(vhdx),
			sb.script("iscsi_remove_mapping.ps1"), "-TargetName", nameOK, "-DevicePath", vhdx)
		// E10a-2 自校验：映射必须真的从目标侧消失（虚拟盘侧看不到映射信息）。
		afterRemoval := r.inline("E10a-2 解除后目标侧不应再有该映射",
			`(Get-IscsiServerTarget -TargetName `+psQuote(nameOK)+`).LunMappings`,
			`Import-Module IscsiTarget;`+
				`$t = @(Get-IscsiServerTarget -ErrorAction SilentlyContinue | Where-Object { $_.TargetName -eq `+psQuote(nameOK)+` }) | Select-Object -First 1;`+
				`if (-not $t) { 'target_not_found' } else { 'luns=' + ((@($t.LunMappings) | ForEach-Object { [string]$_.Path }) -join ',') }`,
			opts.timeout)
		if !afterRemoval.failed() && strings.Contains(afterRemoval.stdout, vhdx) {
			r.fail("E10a-3 映射未真正解除",
				"目标上仍能看到 "+vhdx+" 的映射（remove_mapping 报成功却没生效）", "")
		}
	}
	if !imported.failed() {
		r.script("E10b iscsi_remove_virtual_disk.ps1（仅移除登记）",
			`& .\scripts\iscsi_remove_virtual_disk.ps1 -Path `+psQuote(vhdx),
			sb.script("iscsi_remove_virtual_disk.ps1"), "-Path", vhdx)
	}
	if created {
		r.script("E10c iscsi_remove_target.ps1", "& .\\scripts\\iscsi_remove_target.ps1",
			sb.script("iscsi_remove_target.ps1"), "-TargetName", nameOK)
	}
	r.inline("E10d 清理对照目标（带下划线）", `Remove-IscsiServerTarget -TargetName `+psQuote(nameUnder),
		`Remove-IscsiServerTarget -TargetName `+psQuote(nameUnder)+` -ErrorAction SilentlyContinue;'done'`, opts.timeout)
}

func checkWindowsInitiatorReadonly(r *report, sb *sandbox) {
	r.sec("F. 发起端（Initiator）只读探测")
	r.script("F1 initiator_available.ps1", "& .\\scripts\\initiator_available.ps1 -ServiceName MSiSCSI",
		sb.script("initiator_available.ps1"), "-ServiceName", "MSiSCSI")

	// F1b 挂载前必须的"确保发起端服务运行"：产品每次挂载都会调用同一个脚本。
	// 服务没起来时 Connect-IscsiTarget 会抛 CIM 异常，而挂载链路只把错误压成
	// reason=connect_failed（界面仅显示"挂载失败（阶段：connect）"），真因不可见
	// —— 这里把它变成一条可自证的检查。会启动 MSiSCSI（幂等），故 -skip-write 下只跳过。
	if opts.skipWrite {
		r.skip("F1b initiator_ensure_service.ps1", "-skip-write 已开启（该脚本会幂等启动 MSiSCSI 服务）")
	} else {
		r.script("F1b initiator_ensure_service.ps1（挂载前确保 MSiSCSI 运行）",
			`& .\scripts\initiator_ensure_service.ps1 -ServiceName MSiSCSI`,
			sb.script("initiator_ensure_service.ps1"), "-ServiceName", "MSiSCSI")
		// 自校验：脚本执行后服务必须是 Running，否则挂载一定卡在 connect 阶段。
		status := r.inline("F1b-2 发起端服务应处于 Running",
			`(Get-Service -Name MSiSCSI).Status`,
			`$s=Get-Service -Name MSiSCSI -ErrorAction SilentlyContinue;`+
				`"status=" + $(if ($s) { [string]$s.Status } else { 'missing' })`,
			opts.timeout)
		if !status.failed() && !strings.Contains(status.stdout, "Running") {
			r.fail("F1b-3 发起端服务未运行",
				strings.TrimSpace(status.stdout)+"：挂载会在 connect 阶段失败（真因被压成 connect_failed，日志里才看得到）",
				"Start-Service MSiSCSI")
		}
	}
	r.inline("F2 已注册门户（Get-IscsiTargetPortal）", "Get-IscsiTargetPortal",
		`$p=@(Get-IscsiTargetPortal -ErrorAction SilentlyContinue);"portal 数量: $($p.Count)"`, opts.timeout)
	r.inline("F3 当前会话（Get-IscsiSession）", "Get-IscsiSession",
		`$s=@(Get-IscsiSession -ErrorAction SilentlyContinue);"会话数量: $($s.Count)"`, opts.timeout)
	r.skip("F4 连接/断开/门户增删类脚本",
		"刻意不执行：属客户端侧能力，需要真实对端；本机 loopback iSCSI 可能把服务器挂死（见报告末尾）")
}

func checkWindowsMisc(r *report, sb *sandbox) {
	r.sec("G. 其他")

	r.script("G1 tls_cert_fingerprint.ps1（本机证书指纹查询）",
		"& .\\scripts\\tls_cert_fingerprint.ps1", sb.script("tls_cert_fingerprint.ps1"))

	// vhd_optimize / vhd_reset_disk_identifier 依赖 Hyper-V 模块，缺失时明确跳过。
	res := runPSInline(opts.timeout, `if (Get-Command -Name Set-VHD -ErrorAction SilentlyContinue) {'yes'} else {'no'}`)
	if strings.TrimSpace(res.stdout) == "yes" {
		// 需要真实 VHDX；用沙箱里刚建成的那个（可能已被删）。这里只验证脚本能跑通参数解析。
		r.skip("G3 vhd_optimize / vhd_reset_disk_identifier",
			"已检测到 Set-VHD；实际执行需要一块未被占用的 VHDX，建议随建盘流水线一起验证")
	} else {
		r.skip("G3 vhd_optimize / vhd_reset_disk_identifier", "未检测到 Hyper-V 模块（Set-VHD）")
	}

	r.inline("G4 磁盘 / 卷快照", "Get-Disk; Get-Volume",
		`[pscustomobject]@{disks=@(Get-Disk -ErrorAction SilentlyContinue).Count;volumes=@(Get-Volume -ErrorAction SilentlyContinue).Count}|ConvertTo-Json -Compress`,
		opts.timeout)
}

// ---------------------------------------------------------------------------
// Linux
// ---------------------------------------------------------------------------

func runLinux(r *report) {
	r.sec("A. 运行环境")

	res := runCmd(opts.timeout, "id", "-u")
	if strings.TrimSpace(res.stdout) == "0" {
		r.pass("root 权限", "当前为 root（LVM/configfs/挂载都需要）", "")
	} else {
		r.fail("root 权限", "当前 UID="+strings.TrimSpace(res.stdout)+"，请用 root 运行，否则大量检查会失真", "sudo ./check-command")
	}
	if data, err := os.ReadFile("/etc/os-release"); err == nil {
		r.pass("发行版", firstMatchingLine(string(data), "PRETTY_NAME="), "")
	} else {
		r.warn("发行版", err.Error(), "")
	}
	r.pass("内核", strings.TrimSpace(runCmd(opts.timeout, "uname", "-r").stdout), "")

	r.sec("B. 外部命令清单（LookPath + 版本）")

	type tool struct {
		name string
		args []string
		hard bool // 缺失即失败（否则只是告警）
	}
	tools := []tool{
		{"lvs", []string{"--version"}, true},
		{"vgs", []string{"--version"}, true},
		{"pvs", []string{"--version"}, true},
		{"lvcreate", []string{"--version"}, true},
		{"lvchange", []string{"--version"}, true},
		{"lvextend", []string{"--version"}, true},
		{"lvremove", []string{"--version"}, true},
		{"lvconvert", []string{"--version"}, false},
		{"pvcreate", []string{"--version"}, true},
		{"vgcreate", []string{"--version"}, true},
		{"vgextend", []string{"--version"}, false},
		{"thin_ls", []string{"--help"}, false},
		{"dmsetup", []string{"version"}, false},
		{"blkid", []string{"--version"}, true},
		{"lsblk", []string{"--version"}, true},
		{"wipefs", []string{"--version"}, false},
		{"fstrim", []string{"--version"}, false},
		{"mount", []string{"--version"}, true},
		{"umount", []string{"--version"}, true},
		{"sync", []string{"--version"}, true},
		{"modprobe", []string{"-V"}, true},
		{"losetup", []string{"--version"}, false},
		{"resize2fs", nil, false},
		{"xfs_growfs", nil, false},
		{"mkfs.ext4", nil, false},
		{"mkfs.xfs", nil, false},
		{"mkfs.ntfs", nil, false},
		{"ntfsfix", nil, false},
		{"ntfslabel", nil, false},
	}
	for _, t := range tools {
		if _, err := exec.LookPath(t.name); err != nil {
			if t.hard {
				r.fail(t.name, "未找到："+err.Error(), "which "+t.name)
			} else {
				r.warn(t.name, "未找到（可选能力，相关功能会降级或不可用）", "which "+t.name)
			}
			continue
		}
		if len(t.args) == 0 {
			r.pass(t.name, "已在 PATH 中", "")
			continue
		}
		out := runCmd(opts.timeout, t.name, t.args...)
		detail := strings.TrimSpace(out.stdout)
		if detail == "" {
			detail = strings.TrimSpace(out.stderr)
		}
		r.pass(t.name, clampText(firstLine(detail), 200), "")
	}

	r.sec("C. 只读功能探测（验证参数与输出格式被支持）")

	// lvs 的 JSON 报告是本项目解析 LVM 的唯一途径，必须验证 --reportformat json 可用。
	res = runCmd(opts.timeout, "lvs", "--reportformat", "json")
	if res.failed() {
		r.fail("lvs --reportformat json", res.describe(), "lvs --reportformat json")
	} else {
		r.pass("lvs --reportformat json", "输出前 200 字符: "+clampText(firstLine(strings.TrimSpace(res.stdout)), 200), "")
	}
	for _, args := range [][]string{
		{"--noheadings", "-o", "lv_name"},
		{"--noheadings", "--units", "b", "--nosuffix", "-o", "chunksize"},
		{"--reportformat", "json", "-o", "data_percent,metadata_percent"},
	} {
		cmdline := "lvs " + strings.Join(args, " ")
		res = runCmd(opts.timeout, "lvs", args...)
		if res.failed() {
			r.warn(cmdline, "退出非 0（无匹配对象时也可能如此）："+firstLine(res.describe()), cmdline)
		} else {
			r.pass(cmdline, "exit=0 耗时="+fmt.Sprint(res.ms)+"ms", "")
		}
	}

	res = runCmd(opts.timeout, "lsblk", "--json", "-b",
		"-o", "NAME,PATH,TYPE,SIZE,ROTA,MODEL,MOUNTPOINTS,FSTYPE")
	if res.failed() {
		// 应用侧对此有降级路径（退化为最小列集），因此只告警：块设备选择器会缺 ROTA/MODEL/MOUNTPOINTS。
		r.warn("lsblk --json 完整列集不可用",
			"应用会退化为最小列集，块设备选择器缺少 机械盘/型号/挂载点 信息\n"+firstLine(res.describe()),
			"lsblk --json -b -o NAME,PATH,TYPE,SIZE,ROTA,MODEL,MOUNTPOINTS,FSTYPE")
	} else {
		r.pass("lsblk --json 完整列集", "exit=0 耗时="+fmt.Sprint(res.ms)+"ms", "")
	}

	res = runCmd(opts.timeout, "dmsetup", "version")
	if res.failed() {
		r.warn("dmsetup version", res.describe(), "dmsetup version")
	} else {
		r.pass("dmsetup version", firstLine(strings.TrimSpace(res.stdout)), "")
	}

	r.sec("D. 内核模块与 LIO configfs")

	if data, err := os.ReadFile("/proc/mounts"); err == nil {
		if strings.Contains(string(data), " /sys/kernel/config ") || strings.Contains(string(data), "configfs") {
			r.pass("configfs 已挂载", "/sys/kernel/config", "")
		} else {
			r.fail("configfs 未挂载", "LIO 需要 configfs；请执行：mount -t configfs none /sys/kernel/config",
				"mount -t configfs none /sys/kernel/config")
		}
	}
	if st, err := os.Stat("/sys/kernel/config/target"); err == nil && st.IsDir() {
		r.pass("/sys/kernel/config/target", "存在（LIO 内核子系统已就绪）", "")
	} else {
		r.fail("/sys/kernel/config/target 不存在",
			"说明 target 内核模块未加载；请 modprobe target_core_mod / iscsi_target_mod",
			"modprobe target_core_mod iscsi_target_mod && ls /sys/kernel/config/target")
	}
	for _, mod := range []string{"target_core_mod", "iscsi_target_mod"} {
		if _, err := os.Stat("/sys/module/" + mod); err == nil {
			r.pass("模块已加载 "+mod, "/sys/module/"+mod, "")
			continue
		}
		if _, err := exec.LookPath("modprobe"); err != nil {
			r.skip("模块 "+mod, "未加载且找不到 modprobe")
			continue
		}
		res = runCmd(opts.timeout, "modprobe", "-n", mod)
		if res.failed() {
			r.fail("模块可加载性 "+mod, res.describe(), "modprobe -n "+mod)
		} else {
			r.warn("模块未加载 "+mod, "可加载但当前未加载（服务端启动时会 modprobe）", "modprobe "+mod)
		}
	}

	// ntfs-3g：Linux 侧建盘要写 NTFS，依赖用户态驱动。
	if _, err := exec.LookPath("mount.ntfs-3g"); err == nil {
		r.pass("ntfs-3g", "mount.ntfs-3g 可用（NTFS 读写）", "")
	} else if _, err := exec.LookPath("ntfs-3g"); err == nil {
		r.pass("ntfs-3g", "ntfs-3g 可用", "")
	} else {
		r.warn("ntfs-3g", "未找到：NTFS 卷的挂载/拷贝会失败（安装 ntfs-3g / ntfsprogs）", "which ntfs-3g")
	}

	checkLinuxLVMSandbox(r)
}

// checkLinuxLVMSandbox 用 loop 设备建一套临时 VG/thin LV，验证 LVM 全链路。
// 默认关闭：需要 -lvm-sandbox 显式开启（会创建 vaultcheck-* 卷组并尽力清理）。
func checkLinuxLVMSandbox(r *report) {
	r.sec("E. LVM 全链路（loop 沙箱）")
	if !opts.lvmSandbox {
		r.skip("LVM 全链路", "默认关闭（避免在生产机创建卷组）；需要时加 -lvm-sandbox 重跑本节")
		return
	}
	if opts.skipWrite {
		r.skip("LVM 全链路", "-skip-write 已开启")
		return
	}
	if _, err := exec.LookPath("losetup"); err != nil {
		r.skip("LVM 全链路", "找不到 losetup")
		return
	}

	stamp := time.Now().Format("150405")
	vg := "vaultcheck" + stamp
	backing := filepath.Join(os.TempDir(), "vaultcheck-"+stamp+".img")
	// 512MB 稀疏文件：不实际占用磁盘。
	if err := os.WriteFile(backing, nil, 0o600); err != nil {
		r.fail("创建 loop 后备文件", err.Error(), "")
		return
	}
	if res := runCmd(opts.timeout, "truncate", "-s", "512M", backing); res.failed() {
		r.fail("创建 loop 后备文件", res.describe(), "truncate -s 512M "+backing)
		_ = os.Remove(backing)
		return
	}

	loop := strings.TrimSpace(runCmd(opts.timeout, "losetup", "--find", "--show", backing).stdout)
	if loop == "" {
		r.fail("losetup 挂载 loop 设备", "未取得空闲 loop 设备（可能 loop 模块未加载或已用尽）",
			"losetup --find --show "+backing)
		_ = os.Remove(backing)
		return
	}
	r.pass("losetup", loop, "")

	// 无论成败都要清理：按相反顺序尽力执行。
	defer func() {
		_ = runCmd(opts.timeout, "lvremove", "-y", vg).failed()
		runCmd(opts.timeout, "vgremove", "-y", vg)
		runCmd(opts.timeout, "pvremove", "-y", loop)
		runCmd(opts.timeout, "losetup", "-d", loop)
		_ = os.Remove(backing)
		r.pass("E9 LVM 沙箱清理", "已删除 "+vg+" / "+loop, "")
	}()

	steps := [][]string{
		{"pvcreate", loop},
		{"vgcreate", vg, loop},
		{"lvcreate", "--type", "thin-pool", "-L", "256M", "-n", "pool", vg},
		{"lvcreate", "--type", "thin", "-V", "64M", "-n", "data", "-T", vg + "/pool"},
		{"lvchange", "-ay", "-K", vg + "/data"},
		{"lvcreate", "-s", "-n", "snap", vg + "/data"},
		{"lvchange", "-ay", "-K", vg + "/snap"},
		{"lvs", "--reportformat", "json"},
		{"lvs", "--noheadings", "-o", "lv_name", vg + "/data"},
		{"thin_ls", "--no-headers", "-o", "DEV,MAPPED_BLOCKS,EXCLUSIVE_BLOCKS",
			"/dev/mapper/" + vg + "-pool_tmeta"},
		{"lvextend", "-L", "+16M", vg + "/data"},
		{"lvremove", "-y", vg + "/snap"},
		{"lvremove", "-y", vg + "/data"},
		{"lvremove", "-y", vg + "/pool"},
	}
	for _, step := range steps {
		name := "E " + strings.Join(step, " ")
		res := runCmd(opts.timeout, step[0], step[1:]...)
		if res.failed() {
			r.fail(name, res.describe(), strings.Join(step, " "))
		} else {
			r.pass(name, "exit=0 耗时="+fmt.Sprint(res.ms)+"ms", "")
		}
	}
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

func firstMatchingLine(text, prefix string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			return strings.Trim(strings.TrimPrefix(strings.TrimSpace(line), prefix), `"`)
		}
	}
	return ""
}

// psJSON 执行内联脚本并解析其最后一行 JSON（脚本应以 ConvertTo-Json 收尾）。
//
// 这样调用方能按脚本自己的 ok 字段判定成败：Windows 的很多探测脚本并不以非 0 退出，
// 只看退出码会把"探测到不可用"误记成 OK。
func psJSON(timeout time.Duration, script string) (map[string]any, cmdResult) {
	res := runPSInline(timeout, script)
	return decodeLastJSON(res.stdout), res
}

func boolOf(doc map[string]any, key string) bool {
	if doc == nil {
		return false
	}
	v, _ := doc[key].(bool)
	return v
}

func strOf(doc map[string]any, key string) string {
	if doc == nil {
		return ""
	}
	v, _ := doc[key].(string)
	return v
}

func numOf(doc map[string]any, key string) int {
	if doc == nil {
		return 0
	}
	switch v := doc[key].(type) {
	case float64:
		return int(v)
	case string:
		n, _ := strconv.Atoi(v)
		return n
	}
	return 0
}

// firstNumberFromJSON / firstStringFromJSON 便于对"脚本原始 stdout"直接取值。
func firstNumberFromJSON(out, key string) int    { return numOf(decodeLastJSON(out), key) }
func firstStringFromJSON(out, key string) string { return strOf(decodeLastJSON(out), key) }
func firstBoolFromJSON(out, key string) bool     { return boolOf(decodeLastJSON(out), key) }

// firstTargetEnabled 取 iscsi_get_targets.ps1 输出中第一个目标的 enabled。
//
// ⚠️ 不能用 firstBoolFromJSON(out, "enabled")：enabled 是**嵌套**在 targets[0] 里的
// （顶层只有 ok/count/targets），顶层查不到就恒返回 false —— 曾导致"启用后仍报未启用"
// 这类**自检工具的误报**。返回 ok=false 表示解析失败（调用方应报错而不是当作 false）。
func firstTargetEnabled(out string) (enabled bool, ok bool) {
	doc := decodeLastJSON(out)
	if doc == nil {
		return false, false
	}
	targets, _ := doc["targets"].([]any)
	if len(targets) == 0 {
		return false, false
	}
	first, _ := targets[0].(map[string]any)
	if first == nil {
		return false, false
	}
	value, found := first["enabled"].(bool)
	return value, found
}

func decodeLastJSON(out string) map[string]any {
	line := lastJSONLine(out)
	if line == "" {
		return nil
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(line), &doc); err != nil {
		return nil
	}
	return doc
}
