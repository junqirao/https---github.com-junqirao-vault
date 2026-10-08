// 本文件**刻意不加** //go:build linux：它测的是 runner.go 里的驱动机制（输出压缩、脱敏、
// 复验与失败信息），这些逻辑与 configfs 无关，必须能在没有 LIO 的开发机上真正跑起来——
// 否则"自己写的实现不好使"这类问题永远只能靠肉眼在 Linux 机器上找。
//
// 背景（真实反馈）：prepare_repo 在 create_backstore 阶段连试 3 次失败，日志里只有
//
//	命令已下发但后置条件未满足；targetcli 输出: /usr/lib/python3/dist-packages/rtslib_fb/
//	root.py:174: UserWarning: Cannot set dbroot to ...  targetcli shell version 2.1.53 ...
//
// 即 400 字节的输出预算被 rtslib 的 dbroot 告警与 targetcli 横幅占满，真正的报错行被截掉。
// 下面的用例就是把这个现场钉住。

package liotarget

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"vault/internal/apperr"
)

// rtslibDbrootWarning 是现场日志里的开头两行（python 告警 + 它的续行）。
//
// 它每次启动 targetcli 都会出现，且**带着来源文件路径**，所以不属于 "Warning:" 开头的
// 常见形态——最初的无害告警过滤没认出它，于是它污染了 version 字段、又吃掉截断预算。
const rtslibDbrootWarning = "/usr/lib/python3/dist-packages/rtslib_fb/root.py:174: UserWarning: " +
	"Cannot set dbroot to /etc/rtslib-fb-target. Target drivers have already been registered.\n" +
	"  warn(...)\n"

// targetcliBanner 是 targetcli 每次启动固定打印的横幅。
const targetcliBanner = "targetcli shell version 2.1.53\n" +
	"Copyright 2011-2018 by Datera, Inc and others.\n" +
	"For help on commands, type 'help'.\n"

// TestTruncateOutputKeepsErrorAfterBanner 钉住本次故障：横幅必须让位给报错行。
//
// 断言的是"报错行的**结尾**能活下来"。旧实现只留头部 400 字节，横幅（约 300 字节）加提示符与
// 命令回显就把它占满了，报错句被切在半截——现场因此完全看不出该查什么。
func TestTruncateOutputKeepsErrorAfterBanner(t *testing.T) {
	ref := "/dev/mapper/vault-storages_1ee078b1_diffs"
	// 命令回显与 rtslib 的报错原文都是凭据无关的现场线索，必须都能留下。
	out := rtslibDbrootWarning + targetcliBanner +
		"\n/> /backstores/block create name=iqn.2026-01.com.vault_vault-1ee078b1 dev=" + ref + "\n" +
		"Storage object block/iqn.2026-01.com.vault_vault-1ee078b1: device " + ref + " is not usable\n"

	got := truncateOutput(out, maxOutputInError)

	if strings.Contains(got, "UserWarning") || strings.Contains(got, "targetcli shell version") {
		t.Errorf("横幅与 rtslib 告警不应进入输出，实际得到：%q", got)
	}
	if !strings.Contains(got, "create name=iqn.2026-01.com.vault_vault-1ee078b1 dev="+ref) {
		t.Errorf("命令回显应保留（它记录了下发的命令与插件路径），实际得到：%q", got)
	}
	if !strings.Contains(got, "is not usable") {
		t.Errorf("报错句的结尾应保留（旧实现正是切在这里丢掉了全部线索），实际得到：%q", got)
	}
}

// TestTruncateOutputKeepsHeadAndTail 长输出取头 + 尾两段，中间省略。
func TestTruncateOutputKeepsHeadAndTail(t *testing.T) {
	var b strings.Builder
	b.WriteString(rtslibDbrootWarning + targetcliBanner)
	line := "/> /backstores/block set attribute " + strings.Repeat("x", 60) + "\n"
	for i := 0; i < 40; i++ {
		b.WriteString(line)
	}
	b.WriteString("Storage object block/last: 最后一行才是结论\n")

	got := truncateOutput(b.String(), maxOutputInError)

	if !strings.Contains(got, "create") && !strings.Contains(got, "/backstores/block set attribute") {
		t.Errorf("应保留头部内容（命令回显），实际得到：%q", got)
	}
	if !strings.Contains(got, "最后一行才是结论") {
		t.Errorf("应保留尾部内容（结论常在末尾），实际得到：%q", got)
	}
	if !strings.Contains(got, "省略") {
		t.Errorf("截断处应有省略标记，实际得到：%q", got)
	}
	if len(got) > maxOutputInError+64 {
		t.Errorf("压缩后长度 %d 超出预算上限 %d 太多", len(got), maxOutputInError)
	}
}

// TestTruncateOutputOnlyBannerFallsBack 输出里只剩横幅时退回原文。
//
// 为什么不能返回空串：空串在错误信息里读起来像"targetcli 完全没有输出"，会把现场往
// "命令没发出去"的方向带，而真相是"发了、也没报错、但配置没生效"。
func TestTruncateOutputOnlyBannerFallsBack(t *testing.T) {
	got := truncateOutput(rtslibDbrootWarning+targetcliBanner, maxOutputInError)
	if got == "" {
		t.Fatal("只剩横幅时不应返回空串")
	}
	if !strings.Contains(got, "UserWarning") {
		t.Errorf("退回的应是原文，实际得到：%q", got)
	}
}

// TestTruncateOutputMasksChapSecret CHAP 密钥绝不能被回显带进日志与错误信息。
//
// 密钥只经 stdin 下发（不进 argv），但 targetcli 的批处理回显会把收到的命令原样打出来；
// 本函数是它进日志/进错误信息前的最后一道闸门。密钥**允许含空格**，所以掩到行尾而非空白。
func TestTruncateOutputMasksChapSecret(t *testing.T) {
	out := "/> /iscsi/iqn.2026-01.com.vault:t1/tpg1/acls/iqn.2026-01.com.vault:i1 " +
		"set auth userid=vault password=s3cr3t with space\n" +
		"/> /iscsi/iqn.2026-01.com.vault:t1/tpg1 set auth password=another\n"

	got := truncateOutput(out, maxOutputInError)

	if strings.Contains(got, "s3cr3t") || strings.Contains(got, "another") {
		t.Fatalf("密钥泄漏进输出：%q", got)
	}
	if strings.Count(got, "password=***") != 2 {
		t.Errorf("两处密钥都应被掩掉，实际得到：%q", got)
	}
	if !strings.Contains(got, "userid=vault") {
		t.Errorf("同一条命令里的非密钥参数不应被牵连，实际得到：%q", got)
	}
}

// TestFirstLineSkipsRtslibDbrootWarning version 探针字段要拿到真版本号，而不是那句告警。
func TestFirstLineSkipsRtslibDbrootWarning(t *testing.T) {
	if got := firstLine(rtslibDbrootWarning + targetcliBanner); got != "targetcli shell version 2.1.53" {
		t.Errorf("firstLine = %q，期望 targetcli 版本号", got)
	}
}

// fakeRunner 注入假输出，避免测试依赖真实的 targetcli。
type fakeRunner struct {
	output string
	err    error
	calls  [][]string
}

func (f *fakeRunner) Run(_ context.Context, commands []string) (string, error) {
	f.calls = append(f.calls, commands)
	return f.output, f.err
}

func testDriver(r targetcliRunner) *cliDriver {
	return newCLIDriver("targetcli", "", time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)), r)
}

// TestStageFailureCarriesStageAndAlias 失败信息必须说清"哪个阶段、用了哪个插件路径"。
//
// 为什么这么较真：这一阶段的失败只有两种可能——别名探测没走对，或 rtslib 真的拒绝了；
// 两者的处置完全不同，而仅看 targetcli 输出分不出来（回显被截断时更是如此）。
// 别名记录的是**最后一次实际下发**的那个，不是候选列表里的第一个。
func TestStageFailureCarriesStageAndAlias(t *testing.T) {
	fr := &fakeRunner{output: rtslibDbrootWarning + targetcliBanner + "Storage object block/x: 打不开设备\n"}
	d := testDriver(fr)

	err := d.applyAliases(context.Background(), "create_backstore",
		func() bool { return false }, // 复验恒失败：模拟"命令已下发但后置条件未满足"
		[]string{"block", "iblock"},
		func(alias string) []string {
			return []string{"/backstores/" + alias + " create name=x dev=/dev/mapper/vault-x"}
		})
	if err == nil {
		t.Fatal("后置条件不满足时必须报错，不能吞掉")
	}

	var ae *apperr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("应是带错误码的业务错误，实际为 %T", err)
	}
	if got := ae.Args["stage"]; got != "create_backstore" {
		t.Errorf("stage = %v，期望 create_backstore", got)
	}
	if got := ae.Args["backstore_alias"]; got != "iblock" {
		t.Errorf("backstore_alias = %v，期望最后一次实际下发的 iblock", got)
	}
	if len(fr.calls) != 2 {
		t.Errorf("两个候选别名都应尝试过，实际下发 %d 次", len(fr.calls))
	}
	if ae.Cause == nil || !strings.Contains(ae.Cause.Error(), "打不开设备") {
		t.Errorf("rtslib 的报错原文应带进 Cause，实际为 %v", ae.Cause)
	}
}
