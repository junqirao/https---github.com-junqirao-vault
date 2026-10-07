//go:build linux

// Package liotarget 实现 iSCSI target 后端（platform.IscsiBackend 的 Linux 版本），
// 底层是 LIO（默认挂载点 /sys/kernel/config/target）。
//
// # 读走 configfs，写走 targetcli
//
// 这两件事分开做是刻意的：
//
//   - **写**交给 `targetcli`（即 rtslib）。configfs 的同一套语义在不同内核/发行版上
//     表示并不统一（LUN 映射是"目录里放符号链接"还是"直接符号链接"、TPG 属性集随版本
//     增减、ACL 层的映射目录 configfs 叫 lun_0 而 CLI 叫 mapped_lun0）。此前自行拼
//     mkdir/写属性文件，为覆盖这些差异写了大量"先成功者为准"的兼容分支，既脆弱又无法
//     在本机验证——而这些差异恰恰是 rtslib 用版本感知代码一直在维护的东西。交给它，
//     就是把"我要兼容所有内核版本"这个不可能完成的承诺转交出去。
//
//   - **读**继续直接查 configfs 目录树。因为 targetcli 的输出是给人看的（对齐、缩写、
//     彩色），而 configfs 是结构化的：读目录 = 枚举对象，读属性文件 = 取字段值。
//     且读路径本来就是可靠的——出问题的从来不是"看"，是"写"。保留读路径还带来两个好处：
//     不解析命令输出（少一整类脆弱点），以及 targetcli 缺失时状态查询仍可降级工作。
//
// # 下发即复验
//
// targetcli 的批处理模式**不会因某条命令失败而中止**，也不保证逐条回报退出码。
// 因此本包所有写操作都遵循"算差量 → 下发 → 回读 configfs 复验后置条件"：
// 复验不通过就报错并附上 targetcli 原始输出，绝不把"命令发出去了"当成"配置生效了"。
//
// # 为什么用 stdin 而不是 argv
//
// CHAP 密钥会作为命令参数出现。走 argv 会让它留在 /proc/<pid>/cmdline 里被 ps 看到，
// 走 stdin 则只在管道内；日志同样只记命令条数与输出，不记命令内容。
//
// # 并发
//
// configfs 的目录写入不是并发安全的（并发 mkdir/rmdir 会得到 EBUSY/EINVAL 甚至半成品
// 目录树），targetcli 亦非可重入。这里用一把大锁把所有读写串行化——iSCSI 目标配置
// 不是高频操作，简单优先。
package liotarget

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"vault/internal/apperr"
)

const (
	// defaultConfigFSRoot configfs 中 target 子系统的默认挂载点。
	defaultConfigFSRoot = "/sys/kernel/config/target"
	// defaultIQNPrefix 当调用方给的 name 不是合法 IQN 时用于拼接的默认前缀。
	defaultIQNPrefix = "iqn.2026-01.com.vault"
	// defaultTimeout 单次文件操作/外部命令的超时。
	defaultTimeout = 30 * time.Second

	// tpgName 固定使用第一个 target portal group（targetcli 默认 tpg1）。
	tpgName = "tpgt_1"
	// iblockPlugin 块设备 backstore 的后端插件名（core 子系统下的 iblock_0 组）。
	iblockPlugin = "iblock_0"
)

// Options 是 Manager 的构造参数。
type Options struct {
	// Logger 日志器；nil 时使用 slog.Default()。
	Logger *slog.Logger
	// ConfigFSRoot configfs 中 target 子系统的根；空时使用 /sys/kernel/config/target。
	ConfigFSRoot string
	// IQNPrefix 备用前缀：调用方给的 name 不以 "iqn." 开头时用它拼成合法 IQN；
	// 空时使用 iqn.2026-01.com.vault。
	IQNPrefix string
	// Timeout 单次外部命令（targetcli / modprobe）超时；空时使用 30s。
	Timeout time.Duration
	// TargetCLI targetcli 可执行文件路径或名字；空时使用 "targetcli"（走 PATH）。
	//
	// 留成可配是为了应付 targetcli 装在非标准路径（源码部署 / 虚拟环境）的环境。
	TargetCLI string
	// BackstoreAlias 强制指定 iblock 插件在 targetcli 路径里的末段（"block" / "iblock"）。
	//
	// 空（推荐）表示运行时探测：按候选顺序尝试并用 configfs 复验，命中后记住。
	// 仅当探测结果与现场实际不符时才需要显式指定。
	BackstoreAlias string
	// runner 仅供测试注入假执行器；生产路径保持 nil，由 New 装配真实 exec 实现。
	runner targetcliRunner
}

// Manager 是 Linux LIO iSCSI target 后端。
//
// 结构上分两层：本文件持有 **configfs 路径知识**（读状态、复验后置条件），
// tcl（cliDriver，见 runner.go）持有 **targetcli 驱动机制**（下发、复验判定、别名探测）。
// 后者不依赖任何 Linux 专有设施，因此可被单元测试执行。
type Manager struct {
	root      string
	iqnPrefix string
	timeout   time.Duration
	logger    *slog.Logger

	// tcl targetcli 驱动层。
	tcl *cliDriver

	// mu 串行化所有读写操作（见包注释）。读操作也一并加锁，避免读到 rmdir 过程中的半成品。
	mu sync.Mutex
}

// New 构造 Manager。
func New(opt Options) *Manager {
	logger := opt.Logger
	if logger == nil {
		logger = slog.Default()
	}
	root := strings.TrimSpace(opt.ConfigFSRoot)
	if root == "" {
		root = defaultConfigFSRoot
	}
	prefix := strings.TrimSpace(opt.IQNPrefix)
	if prefix == "" {
		prefix = defaultIQNPrefix
	}
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	bin := strings.TrimSpace(opt.TargetCLI)
	if bin == "" {
		bin = defaultTargetCLI
	}
	return &Manager{
		root:      strings.TrimSuffix(root, "/"),
		iqnPrefix: strings.TrimSuffix(prefix, ":"),
		timeout:   timeout,
		logger:    logger,
		tcl:       newCLIDriver(bin, opt.BackstoreAlias, timeout, logger, opt.runner),
	}
}

// ---- 路径拼接：configfs 内部一律用 '/' 分隔，故用 path.Join（而非 filepath.Join，
// 后者取决于构建时的 GOOS，显式用 path 可读性更好且不会因交叉编译产生歧义）。----

func (m *Manager) iscsiRoot() string            { return path.Join(m.root, "iscsi") }
func (m *Manager) coreRoot() string             { return path.Join(m.root, "core", iblockPlugin) }
func (m *Manager) targetPath(iqn string) string { return path.Join(m.iscsiRoot(), iqn) }
func (m *Manager) tpgPath(iqn string) string    { return path.Join(m.targetPath(iqn), tpgName) }
func (m *Manager) aclsPath(iqn string) string   { return path.Join(m.tpgPath(iqn), "acls") }

// tpgAuthDir 返回 TPG 级 CHAP 凭据目录（configfs 的 tpgt_1/auth，内核里是 tpg_demo_auth）。
//
// 别与 acls/<iqn>/auth 混为一谈：两者目录名与文件名都相同（userid / password），但落点与
// 服务对象不同——这个只给**动态 ACL** 用，那个只给显式 ACL 用（见 Manager.setTpgAuth）。
func (m *Manager) tpgAuthDir(iqn string) string { return path.Join(m.tpgPath(iqn), "auth") }
func (m *Manager) tpgLunDir(iqn string) string {
	return path.Join(m.tpgPath(iqn), "lun", "lun_0")
}
func (m *Manager) aclMappedLunDir(iqn, ini string) string {
	// configfs 里这一层叫 lun_0（rtslib MappedLUN 的 path 模板是 "%s/lun_%d"），
	// 尽管 targetcli 路径里它叫 mapped_lun0。别按 CLI 的名字来找目录。
	return path.Join(m.aclsPath(iqn), ini, "lun_0")
}
func (m *Manager) backstorePath(name string) string { return path.Join(m.coreRoot(), name) }

// ---- configfs 原语 ----

// readAttrEquals 判断属性文件的值是否等于期望值（用于下发 targetcli 命令后的复验）。
//
// 读不到（不存在/权限不足）一律视为**不相等**：复验的语义是"确认已生效"，读不到就无法
// 确认，只能按未生效处理——此时快路径会下发命令，真有问题则由 CLI 报错暴露出来，
// 好过把"读不到"当成"已生效"而静默跳过。
func (m *Manager) readAttrEquals(p, want string) bool {
	got, err := m.readAttr(p)
	return err == nil && got == want
}

// readAttr 读取属性文件内容（去除首尾空白）。
func (m *Manager) readAttr(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// listDirs 列出目录下的**子目录**名（跳过普通文件与符号链接），结果已排序。
// 目录不存在时返回 nil（等价于"空"），便于把"无会话/无 ACL"表达为不报错。
func (m *Manager) listDirs(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// isDir 判断路径存在且是目录。
func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// lioModules 是 LIO 服务端所需的内核模块，顺序即加载顺序。
//
// target_core_mod 提供 core 子系统（backstore 根），iscsi_target_mod 提供 iscsi fabric，
// target_core_iblock 提供 iblock 块设备 backstore 插件——**三个都要**：
// 少了 iblock，服务端与就绪判定都没问题，但每次建盘都会失败。
// 先加载 core 再加载 iscsi（iscsi_target_mod 依赖 target_core_mod）。
var lioModules = []string{"target_core_mod", "iscsi_target_mod", "target_core_iblock"}

// moduleLoaded 判断内核模块是否已加载。
//
// 读 /proc/modules 而不是调 lsmod：少一层进程开销，且 launcher/容器里不保证有 lsmod。
func moduleLoaded(name string) bool {
	b, err := os.ReadFile("/proc/modules")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, name+" "); ok && rest != "" {
			return true
		}
	}
	return false
}

// loadModules 加载缺失的 LIO 内核模块（已加载的是空操作）。
//
// 返回第一条错误仅供日志参考——模块可能因依赖关系或已存在而部分"失败"。
func (m *Manager) loadModules(ctx context.Context) error {
	var firstErr error
	for _, mod := range lioModules {
		if err := m.modprobe(ctx, mod); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// modprobe 执行 modprobe 加载内核模块（带超时）。
func (m *Manager) modprobe(ctx context.Context, module string) error {
	cctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, "modprobe", module).CombinedOutput()
	if err != nil {
		m.logger.Debug("modprobe 失败", "module", module, "error", err, "output", strings.TrimSpace(string(out)))
		return err
	}
	return nil
}

// mountConfigFS 挂载 configfs 到 <root> 的父目录（与 sysdeps 的修复动作一致）。
//
// 用 mount(8) 而不是 syscall.Mount：受限于容器/挂载命名空间时 mount(8) 的报错远比 errno
// 可读，而这里失败的信息是要写进错误 hint 交给运维的。
func (m *Manager) mountConfigFS(ctx context.Context) error {
	mp := path.Dir(m.root)
	if err := os.MkdirAll(mp, 0o755); err != nil {
		return fmt.Errorf("创建 configfs 挂载点 %s 失败: %w", mp, err)
	}
	cctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	if out, err := exec.CommandContext(cctx, "mount", "-t", "configfs", "none", mp).CombinedOutput(); err != nil {
		return fmt.Errorf("mount -t configfs none %s 失败: %s", mp, strings.TrimSpace(string(out)))
	}
	return nil
}

// repairLIO 按需挂载 configfs 并加载缺失的内核模块；全部动作幂等。
//
// 只做**幂等且无破坏性**的事，刻意不做 sysdeps 那种"modprobe -r 再 modprobe"的模块重载：
// 在服务进程里重载 iscsi_target_mod 会把**正在对外提供服务的整棵 iSCSI 目标配置抹掉**
// （它承载着客户端正在用的盘），代价远大于收益。需要重载时由运维手工执行，并接受短暂中断。
func (m *Manager) repairLIO(ctx context.Context) error {
	if !isDir(m.root) {
		if err := m.mountConfigFS(ctx); err != nil {
			return err
		}
	}
	// 判据是「有没有模块没加载」，而**不是**「configfs 里某个目录在不在」。
	//
	// 那些目录（<root>/iscsi、<root>/core/iblock_0…）是 rtslib 按需创建、内核注册时机也随版本不同的
	// 内部细节，一度被当成就绪判据，结果是把好机器判成故障：每次调用都白跑一遍 modprobe，
	// 现场还被告知"模块没注册"，实际模块明明都在。模块缺失才是 modprobe 的唯一理由。
	if modulesMissing() {
		if err := m.loadModules(ctx); err != nil {
			return err
		}
	}
	return nil
}

// modulesMissing 判断 lioModules 里是否有模块尚未加载。
func modulesMissing() bool {
	for _, mod := range lioModules {
		if !moduleLoaded(mod) {
			return true
		}
	}
	return false
}

// configfsReady 判断**内核侧**的 LIO 是否已就绪，不就绪时给出可区分的错误。
//
// 只断言一件事：configfs 挂在 <root> 的父目录上（没有它，连 targetcli/rtslib 都起不来）。
//
// 刻意**不**断言 <root>/iscsi、<root>/core/iblock_0 这些子目录存在。原因：
//   - 它们是 rtslib 按需创建 / 内核注册时机随版本不同的内部细节，不是"LIO 可不可用"的判据；
//   - 把它们当门禁会把**只读操作**（列目标、查状态）一起锁死——现场表现就是"一大批功能
//     全不可用"，而其中大多数操作本来能正常工作、只是看到空列表；
//   - 真缺目录时，rtslib/targetcli 自己会在对应操作上报出准确错误（"Cannot create…"），
//     比我们预先编造一句 iscsi_fabric 缺失更贴近事实，也不会把现场引去折腾 CLI。
//
// 这里是**探测**，不负责修复（修复见 repairLIO）；自愈试过了才走到这，hint 里如实说明。
func (m *Manager) configfsReady() error {
	if isDir(m.root) {
		return nil
	}
	return apperr.New(apperr.CodeUnavailable, http.StatusInternalServerError).
		WithArg("component", "configfs").
		WithArg("root", m.root).
		WithArg("hint", "configfs 未挂载，且自动挂载（mount -t configfs none "+
			path.Dir(m.root)+"）未成功；请确认进程以 root 运行、且未被容器挂载命名空间隔离")
}

// ensureReady 确保**内核侧 configfs** 与**用户态 targetcli** 都可用；不就绪时先自愈，再按组件区分报错。
//
// 探测顺序是内核在前、CLI 在后：configfs 没挂上时 CLI 也跑不起来，先报出来才不会误导。
func (m *Manager) ensureReady(ctx context.Context) error {
	if err := m.repairLIO(ctx); err != nil {
		// 不在这里返回：结论统一由下面的探测给出（带 component/hint），
		// 避免"自愈失败"与"未自愈"报出两个成分不同的错误、让现场多猜一轮。
		m.logger.Warn("LIO 自愈未成功，继续探测以给出可区分的结论", "error", err)
	}
	if err := m.configfsReady(); err != nil {
		return err
	}
	return m.targetcliProbe(ctx)
}

// normalizeTargetName 归一化调用方给的目标名：
//   - 去除首尾空白并**转全小写**（configfs 目录名即 IQN，内核只接受全小写 IQN，
//     传入大写 IQN 会导致创建路径与读取路径不一致甚至创建失败）；
//   - name 不是合法 IQN（不以 "iqn." 开头）时，用 IQNPrefix 拼成合法 IQN。
func (m *Manager) normalizeTargetName(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if strings.HasPrefix(n, "iqn.") {
		return n
	}
	return strings.ToLower(m.iqnPrefix + ":" + n)
}

// ctxErr 把 context 取消/超时统一包装成业务错误（错误统一走 apperr）。
func ctxErr(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).WithCause(err)
	}
	return nil
}
