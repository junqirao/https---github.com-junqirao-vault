// 本文件**刻意不加** //go:build linux。
//
// 这里放的是"如何把命令交给 targetcli、如何复验、如何处理插件路径别名"这套**驱动机制**：
// 它不碰任何 Linux 专有的东西，只用 os/exec 与字符串处理，因此可在任何平台编译。
// 这样拆分的直接好处是机制部分能被单元测试**真正执行**（见 runner_test.go），
// 而不是只能靠肉眼看代码——"自己写的实现不好使"的病根之一，正是这类逻辑无法在开发机验证。
//
// 与 configfs 路径相关的部分（Manager）留在 //go:build linux 的文件里。

package liotarget

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"vault/internal/apperr"
)

const (
	// defaultTargetCLI targetcli 可执行文件的默认名。
	defaultTargetCLI = "targetcli"
	// defaultBackstoreAlias targetcli 中 iblock 插件的路径末段默认值。
	defaultBackstoreAlias = "block"
	// maxOutputInError 回显 targetcli 输出时的截断长度。
	maxOutputInError = 400
	// tpgAlias TPG 1 在 targetcli 里的名字（configfs 中对应目录 tpgt_1）。
	tpgAlias = "tpg1"
	// tpgLunAlias TPG 级 lun0 在 targetcli 里的名字，也是 luns delete 接受的位置参数。
	//
	// configfs 中对应目录 lun/lun_0。UILUNs.ui_command_delete 会剥掉 "lun" 前缀
	// （`if lun.lower().startswith("lun"): lun = lun[3:]`），故 "lun0" 合法。
	tpgLunAlias = "lun0"
	// aclMappedLunNode ACL 下映射 LUN 在 **targetcli 路径**里的节点名。
	//
	// ⚠️ 名字陷阱：同一层对象在两个世界叫法不同，不能互换——
	//   - targetcli: mapped_lun0（UIMappedLUN.__init__ 用 "mapped_lun%d" 命名）；
	//   - configfs : lun_0     （rtslib MappedLUN 的 path 模板是 "%s/lun_%d"）。
	aclMappedLunNode = "mapped_lun0"
	// aclMappedLunIndex ACL 下 create / delete 接受的**位置参数**。
	//
	// 必须是整数：UINodeACL.ui_command_create 会做 int(mapped_lun)，传 "lun0" 直接报
	// "mapped_lun must be an integer"；ui_command_delete 把参数交给 MappedLUN()，同样
	// int() 解析，且**不会**像 UILUNs 那样剥 "lun" 前缀。
	aclMappedLunIndex = "0"
	// tclISCSIRoot targetcli 的 iscsi fabric 根路径。
	tclISCSIRoot = "/iscsi"
)

// backstoreAliasCandidates 是 iblock 插件在 targetcli 路径里的候选末段。
//
// configfs 里插件组名**恒为** iblock_0，而 targetcli（rtslib）把它映射成 CLI 路径
// /backstores/<别名>：较新版本用 block，部分版本/发行版打补丁后为 iblock，且并非都做了
// 别名兼容。硬编码任一名字都会在另一环境下全盘失败，因此按顺序尝试并用 configfs 复验，
// 命中后记住——把"版本差异"从编译期假设降级为运行期一次探测。
var backstoreAliasCandidates = []string{defaultBackstoreAlias, "iblock"}

// targetcliRunner 抽象"把一批命令交给 targetcli 执行"。
//
// 抽成接口是为了可测：真实实现走 exec，测试注入假实现，从而在没有 Linux/LIO 的开发机上
// 也能断言"该下发哪些命令、不该下发哪些命令、失败时是否如实报错"。
type targetcliRunner interface {
	// Run 在**同一个** targetcli 进程里按顺序执行 commands（每项一行）并返回合并输出。
	Run(ctx context.Context, commands []string) (string, error)
}

// execRunner 是 targetcliRunner 的真实实现。
type execRunner struct {
	bin     string
	timeout time.Duration
}

// Run 以批处理模式执行：命令写到 targetcli 的 stdin，而不是 argv。
//
// 走 stdin 有两个硬理由：
//  1. CHAP 密钥会作为命令参数出现，放 argv 就会留在 /proc/<pid>/cmdline 里被同机任何
//     用户用 ps 看到；走 stdin 则只在管道里。日志同理只记条数、不记内容。
//  2. 一次收敛要下发十几到几十条命令，每条都启一次 python 解释器（数百毫秒）会让对账
//     明显变慢；合并成一个进程只剩一次启动开销。
//
// 注意：targetcli 批处理模式**不会因某条命令失败而中止**，也不保证逐条回报退出码。
// 所以"命令发出去了"绝不等于"配置生效了"——调用方必须复验（见 cliDriver.applyAliases）。
func (r *execRunner) Run(ctx context.Context, commands []string) (string, error) {
	lines := nonEmpty(commands)
	if len(lines) == 0 {
		return "", nil
	}
	cctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, r.bin)
	cmd.Stdin = strings.NewReader(strings.Join(lines, "\n") + "\n")
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err := cmd.Run()
	out := buf.String()
	if err != nil {
		if cctx.Err() != nil {
			return out, fmt.Errorf("targetcli 执行超时（%s）: %w", r.timeout, cctx.Err())
		}
		return out, fmt.Errorf("targetcli 执行失败: %w", err)
	}
	return out, nil
}

// cliDriver 封装 targetcli 的调用、复验与插件路径别名。
//
// 它故意不持有任何 configfs 路径知识：复验条件由调用方以闭包给出（读 configfs 判断
// 后置条件是否成立），驱动层只负责"该不该发、发什么、发完算不算成功"。
type cliDriver struct {
	runner targetcliRunner
	logger *slog.Logger
	// bin targetcli 可执行文件（用于报错与探测）。
	bin string
	// aliasOpt 配置里显式指定的插件路径末段；空表示运行时探测。
	aliasOpt string

	// aliasMu / alias 保护并缓存探测命中的插件路径末段（见 aliasCandidates）。
	aliasMu sync.Mutex
	alias   string

	// probedMu / probed 缓存"targetcli 已验证可用"。只缓存成功：失败不缓存，
	// 以便后续装上 targetcli 后无需重启服务即可自愈（LookPath 失败不启子进程，很便宜）。
	probedMu sync.Mutex
	probed   bool
}

// newCLIDriver 构造驱动层。runner 为 nil 时装配真实 exec 实现（测试注入假实现）。
func newCLIDriver(bin, aliasOpt string, timeout time.Duration, logger *slog.Logger, runner targetcliRunner) *cliDriver {
	if strings.TrimSpace(bin) == "" {
		bin = defaultTargetCLI
	}
	if runner == nil {
		runner = &execRunner{bin: bin, timeout: timeout}
	}
	return &cliDriver{
		runner:   runner,
		logger:   logger,
		bin:      bin,
		aliasOpt: strings.TrimSpace(aliasOpt),
	}
}

// probe 校验 targetcli 可执行文件存在且能正常运行。
//
// 单独一步探测的理由：targetcli 与 configfs 挂载是**两个**独立的就绪条件，合并成一个布尔量
// 会让"没装 targetcli"与"configfs 没挂上"这两种处置方式完全不同的故障在日志里长得一模一样。
// 成功结果永久缓存（读路径每次都要过这道门，不能每次启 python）；失败**不缓存**，以便装上后自动恢复。
//
// 它同时是 LIO 就绪的**行为判据**：targetcli 启动时会构造 RTSRoot() 读整棵 configfs 树，
// 能跑通就说明 configfs 与 LIO 树都可读——比断言某个 configfs 目录存在可靠得多。
func (d *cliDriver) probe(ctx context.Context) error {
	d.probedMu.Lock()
	probed := d.probed
	d.probedMu.Unlock()
	if probed {
		return nil
	}

	if _, err := exec.LookPath(d.bin); err != nil {
		return apperr.New(apperr.CodeUnavailable, http.StatusInternalServerError).
			WithArg("component", "targetcli").
			WithArg("binary", d.bin).
			WithArg("hint", "安装 targetcli（Debian/Ubuntu: apt install targetcli-fb；RHEL: dnf install targetcli）").
			WithCause(err)
	}
	// version 会一并打印 rtslib/configfs 路径与内核 target 版本，是排障时最有价值的一行。
	out, err := d.runner.Run(ctx, []string{"version"})
	if err != nil {
		return apperr.New(apperr.CodeUnavailable, http.StatusInternalServerError).
			WithArg("component", "targetcli").
			WithCause(fmt.Errorf("%w（输出: %s）", err, truncateOutput(out, maxOutputInError)))
	}

	d.probedMu.Lock()
	d.probed = true
	d.probedMu.Unlock()
	d.logger.Info("targetcli 可用", "binary", d.bin, "version", firstLine(out))
	return nil
}

// apply 下发一批与 backstore 无关的命令，并用 verify 复验后置条件。
//
// verify 为 nil 表示"无从复验"（仅用于删除类操作，此时以退出码为准）。verify 非 nil 时
// **复验结果是唯一判据**：targetcli 批处理不中止、不逐条回报，只看退出码会把"命令被拒绝
// 但进程正常退出"误判为成功。
func (d *cliDriver) apply(ctx context.Context, stage string, verify func() bool, commands ...string) error {
	return d.applyAliases(ctx, stage, verify, d.aliasCandidates(), func(string) []string { return commands })
}

// applyStorage 下发需要引用 backstore 对象路径的命令（创建/删除 LUN 映射等）。
//
// 与 apply 的唯一区别：命令要嵌入 /backstores/<别名>/<名字>，而别名在首次使用前未知，
// 故对每个候选别名各试一次，以复验为准判定命中。
func (d *cliDriver) applyStorage(ctx context.Context, stage string, verify func() bool, build func(alias string) []string) error {
	return d.applyAliases(ctx, stage, verify, d.aliasCandidates(), build)
}

// applyAliases 是 apply / applyStorage 的共同实现。
//
// 快路径：后置条件已满足时**一条命令都不下发**——幂等收敛里绝大多数步骤都是这种情形，
// 复验一次目录树远比启动一个 python 便宜。
func (d *cliDriver) applyAliases(
	ctx context.Context,
	stage string,
	verify func() bool,
	aliases []string,
	build func(alias string) []string,
) error {
	if verify != nil && verify() {
		return nil
	}

	var (
		lastErr error
		lastOut string
	)
	for _, alias := range aliases {
		cmds := nonEmpty(build(alias))
		if len(cmds) == 0 {
			return nil
		}
		out, err := d.runner.Run(ctx, cmds)
		lastErr, lastOut = err, out

		if verify == nil || verify() {
			if err != nil {
				// 退出码非 0 但后置条件已满足：批处理下退出码不可靠，以复验为准，但留痕便于追查。
				d.logger.Debug("targetcli 退出码非 0，但后置条件已满足", "stage", stage, "error", err)
			}
			if alias != "" {
				d.rememberAlias(alias)
			}
			d.logger.Debug("targetcli 已执行",
				"stage", stage, "commands", len(cmds), "output", truncateOutput(out, maxOutputInError))
			return nil
		}
		d.logger.Debug("targetcli 候选路径未生效，尝试下一个",
			"stage", stage, "alias", alias, "error", err,
			"output", truncateOutput(out, maxOutputInError))
	}
	return stageFailure(stage, lastErr, lastOut)
}

// aliasCandidates 返回本次可尝试的插件路径末段列表。
func (d *cliDriver) aliasCandidates() []string {
	d.aliasMu.Lock()
	defer d.aliasMu.Unlock()
	if d.alias != "" {
		return []string{d.alias}
	}
	if d.aliasOpt != "" {
		d.alias = d.aliasOpt
		return []string{d.aliasOpt}
	}
	return append([]string(nil), backstoreAliasCandidates...)
}

// rememberAlias 记住探测成功的插件路径末段。
func (d *cliDriver) rememberAlias(alias string) {
	d.aliasMu.Lock()
	defer d.aliasMu.Unlock()
	if d.alias == alias {
		return
	}
	if d.alias != "" {
		d.logger.Info("targetcli backstore 插件路径已切换",
			"old", d.alias, "new", alias, "hint", "若与预期不符请检查 targetcli 版本")
	}
	d.alias = alias
}

// stageFailure 把"命令已下发但配置未生效"整理成带阶段的业务错误。
func stageFailure(stage string, err error, out string) error {
	cause := err
	if cause == nil {
		cause = errors.New("命令已下发但后置条件未满足")
	}
	msg := cause.Error()
	if s := truncateOutput(strings.TrimSpace(out), maxOutputInError); s != "" {
		msg += "；targetcli 输出: " + s
	}
	return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
		WithArg("stage", stage).
		WithCause(errors.New(msg))
}

// nonEmpty 去掉空白项后返回新切片。
func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// truncateOutput 截断过长的命令输出，避免把整段 python 回溯塞进日志与 HTTP 响应体。
func truncateOutput(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "...（截断）"
}

// firstLine 取文本里第一行**有信息量**的内容（用于把 targetcli version 的整段输出压成一行日志）。
//
// 跳过空行与 rtslib 的无害告警：targetcli 首次运行会先把
//
//	Warning: Could not load preferences file /root/.targetcli/prefs.bin.
//
// 写到 stderr，而它排在版本号前面——不过滤的话日志里的 version 字段就变成这句警告了。
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line == "" || isNoiseLine(line) {
			continue
		}
		return line
	}
	return ""
}

// isNoiseLine 判断一行输出是否只是无害告警（rtslib 的 prefs、Django 风格弃用提示等）。
func isNoiseLine(line string) bool {
	l := strings.ToLower(line)
	return strings.HasPrefix(l, "warning:") || strings.HasPrefix(l, "warn:") ||
		strings.HasPrefix(l, "deprecationwarning:")
}

// ============================================================================
// targetcli 命令形式 —— 全包**唯一**依赖 targetcli CLI 语法的地方。
//
// configfs 侧的路径由内核固定（tpgt_1 / lun_0 / iblock_0），可靠；而 CLI 侧的名字由
// rtslib 决定，且和 configfs **并不一一对应**——最坑的一处是 ACL 下的映射 LUN：
// targetcli 路径里叫 mapped_lun0，configfs 目录却叫 lun_0。全收在这里，是为了这类
// 名字集中在唯一一处，出问题只改一个地方。
//
// 已按上游源码逐条核对（targetcli-fb src/targetcli/ui_target.py、ui_root.py、shell.py
// 与 rtslib-fb rtslib/target.py），不要凭记忆改动这里的字面量。
//
// 兜底：每个写操作都回读 configfs 复验，失败时把 targetcli 原始输出塞进错误。因此
// "名字猜错了"表现为一条**带 CLI 报错原文**的显式错误（而非静默无效），贴回输出即可定位。
// ============================================================================

// tclISCSIPath 目标对象路径。
func tclISCSIPath(iqn string) string { return tclISCSIRoot + "/" + iqn }

// tclTpgPath 目标 TPG 路径。
func tclTpgPath(iqn string) string { return tclISCSIPath(iqn) + "/" + tpgAlias }

// tclAclsPath TPG 下 ACL 容器路径。
func tclAclsPath(iqn string) string { return tclTpgPath(iqn) + "/acls" }

// tclACLPath 单个 ACL 节点路径。
func tclACLPath(iqn, ini string) string { return tclAclsPath(iqn) + "/" + ini }

// tclLunsPath TPG 下的 LUN 容器路径。
func tclLunsPath(iqn string) string { return tclTpgPath(iqn) + "/luns" }

// tclACLMappedLunPath ACL 内映射 LUN 的路径（write_protect 落在这一层）。
func tclACLMappedLunPath(iqn, ini string) string {
	return tclACLPath(iqn, ini) + "/" + aclMappedLunNode
}

// cmdCreateTarget 创建目标对象（rtslib 会随之建立 tpg1 及其 enable/attrib/acls/luns）。
func cmdCreateTarget(iqn string) string { return tclISCSIRoot + " create " + iqn }

// cmdDeleteTarget 删除目标对象（连同其 TPG / ACL / LUN 映射一并拆除）。
func cmdDeleteTarget(iqn string) string { return tclISCSIRoot + " delete " + iqn }

// cmdSetTpgAttr 设置一条 TPG 属性。
//
// 逐条下发（而非一条命令带多个 key=value）是刻意的：不同内核属性集不同，批处理下某条
// 属性不被识别时其余属性仍能生效，等价于"尽力而为"；合并成一条则一条不认识就整条被拒。
func cmdSetTpgAttr(iqn, name, value string) string {
	return tclTpgPath(iqn) + " set attribute " + name + "=" + value
}

// cmdSetEnabled 启用 / 停用 TPG。
func cmdSetEnabled(iqn string, enabled bool) string {
	if enabled {
		return tclTpgPath(iqn) + " enable"
	}
	return tclTpgPath(iqn) + " disable"
}

// cmdCreateBackstore 创建 iblock backstore。
//
// 用 name=/dev= 具名形式而非位置参数：位置参数在不同插件下含义不同（fileio 的首个位置
// 参数是文件名而非设备），具名形式对 iblock 明确且不易误传。
func cmdCreateBackstore(alias, name, ref string) string {
	return "/backstores/" + alias + " create name=" + name + " dev=" + ref
}

// cmdDeleteBackstore 删除 iblock backstore（**不删除底层块设备数据**）。
func cmdDeleteBackstore(alias, name string) string {
	return "/backstores/" + alias + " delete " + name
}

// cmdSetBackstoreAttr 设置一条 backstore（存储对象）属性。
//
// ⚠️ 作用对象是 **backstore 而不是 TPG**：emulate_tpu / emulate_tpws / is_nonrot 都是
// SE_device 属性，configfs 里位于 core/<插件>/<名字>/attrib/ 下
// （targetcli 侧即 /backstores/<别名>/<名字> 节点的 attribute 组，
// rtslib 的 ui_desc_attributes 把 emulate_tpu 列在存储对象上）。
// TPG 的 attribute 组里**没有**这几项——写到 TPG 上会被内核拒绝，且因为批处理不报错，
// 会变成"看着配了其实没生效"。
func cmdSetBackstoreAttr(alias, name, attr, value string) string {
	return "/backstores/" + alias + "/" + name + " set attribute " + attr + "=" + value
}

// cmdCreateACL 创建 ACL（即"授权一个 initiator"）。
//
// 显式带 add_mapped_luns=false。targetcli 建 ACL 时默认会把该 TPG 已有的 LUN 全部自动
// 映射进来（且是 rw），开关有两个合法位置——targetcli 的全局偏好 auto_add_mapped_luns
// （写 ~/.targetcli，会污染别人的交互式会话，不该动）与 create 的本命令参数。用后者，
// 才能让"映射哪些 LUN、是否只读"完全由本包显式决定、可复验。
//
// 注意：内核 TPG 属性里**没有** auto_add_mapped_luns 这一项，它不是 configfs 属性，
// 不能用 set attribute 去改（见 target.go 的 applyTpgAttribs）。
func cmdCreateACL(iqn, ini string) string {
	return tclAclsPath(iqn) + " create " + ini + " add_mapped_luns=false"
}

// cmdDeleteACL 删除 ACL 节点（连同其 mapped lun）。
func cmdDeleteACL(iqn, ini string) string { return tclAclsPath(iqn) + " delete " + ini }

// cmdSetACLAuth 设置单向 CHAP 凭据。
//
// ⚠️ 含密钥：调用方必须保证它只经 stdin 下发，绝不进日志、绝不进 argv。
func cmdSetACLAuth(iqn, ini, user, secret string) string {
	return tclACLPath(iqn, ini) + " set auth userid=" + user + " password=" + secret
}

// cmdCreateLun 在 TPG 的 LUN 容器里创建 lun0，指向 backstore 对象。
func cmdCreateLun(iqn, objPath string) string { return tclLunsPath(iqn) + " create " + objPath }

// cmdDeleteLun 删除 TPG 的 lun0。
func cmdDeleteLun(iqn string) string { return tclLunsPath(iqn) + " delete " + tpgLunAlias }

// cmdCreateMappedLun 在 ACL 内创建映射 LUN，把它指向 backstore 对象。
//
// 位置参数依次是 mapped_lun 与 tpg_lun_or_backstore（UINodeACL.ui_command_create
// 的签名），两个都是**位置参数**、第一个必须是整数。传 backstore 对象路径时 targetcli
// 会顺带把 TPG 级 LUN 建好，所以本指令不依赖"先建 TPG LUN"——但本包仍然显式建，
// 以保证复验口径固定。
func cmdCreateMappedLun(iqn, ini, objPath string) string {
	return tclACLPath(iqn, ini) + " create " + aclMappedLunIndex + " " + objPath
}

// cmdDeleteMappedLun 删除 ACL 内的映射 LUN。
//
// 参数是整数索引，不是节点名：UINodeACL.ui_command_delete 把参数交给 MappedLUN() 做
// int() 解析，且**不会**像 UILUNs 那样剥掉 "lun" 前缀，故不能传 mapped_lun0。
func cmdDeleteMappedLun(iqn, ini string) string {
	return tclACLPath(iqn, ini) + " delete " + aclMappedLunIndex
}

// cmdSetWriteProtect 设置 ACL 内映射 LUN 的只读属性。
func cmdSetWriteProtect(iqn, ini string, readOnly bool) string {
	v := "0"
	if readOnly {
		v = "1"
	}
	return tclACLMappedLunPath(iqn, ini) + " set write_protect=" + v
}
