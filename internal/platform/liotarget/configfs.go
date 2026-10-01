//go:build linux

// Package liotarget 通过**直接读写 LIO 的 configfs**（默认 /sys/kernel/config/target）
// 实现 iSCSI target 后端（platform.IscsiBackend 的 Linux 版本）。
//
// 为什么零 cgo、零外部依赖：LIO 暴露给用户态的配置接口本身就是一棵 configfs 目录树，
// 语义与普通文件操作一一对应——
//
//	创建对象   = mkdir
//	删除对象   = rmdir（os.Remove，rmdir 语义，**绝不能**用 os.RemoveAll）
//	设置属性   = 往属性文件里写字符串
//	建立 LUN   = 在映射目录里建一个指向 backstore 的符号链接
//
// 因此既不需要 cgo，也不需要调用 targetcli（避免引入 Python 运行时依赖）。
//
// 并发：configfs 的目录/属性写入不是并发安全的，多个 goroutine 同时写同一个目标
// 会出现竞态（内核返回 EBUSY/EINVAL 甚至产生半成品目录树）。这里用一把大锁把
// 所有写操作串行化——简单优先，iSCSI 目标配置本身也不是高频操作。
package liotarget

import (
	"context"
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
	// Timeout 单次文件操作/外部命令（如 modprobe）超时；空时使用 30s。
	Timeout time.Duration
}

// Manager 是 Linux LIO iSCSI target 后端。
type Manager struct {
	root      string
	iqnPrefix string
	timeout   time.Duration
	logger    *slog.Logger

	// mu 串行化所有写操作（见包注释）。读操作也一并加锁，避免读到 rmdir 过程中的半成品。
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
	return &Manager{
		root:      strings.TrimSuffix(root, "/"),
		iqnPrefix: strings.TrimSuffix(prefix, ":"),
		timeout:   timeout,
		logger:    logger,
	}
}

// ---- 路径拼接：configfs 内部一律用 '/' 分隔，故用 path.Join（而非 filepath.Join，
// 后者取决于构建时的 GOOS，显式用 path 可读性更好且不会因交叉编译产生歧义）。----

func (m *Manager) iscsiRoot() string            { return path.Join(m.root, "iscsi") }
func (m *Manager) coreRoot() string             { return path.Join(m.root, "core", iblockPlugin) }
func (m *Manager) targetPath(iqn string) string { return path.Join(m.iscsiRoot(), iqn) }
func (m *Manager) tpgPath(iqn string) string    { return path.Join(m.targetPath(iqn), tpgName) }
func (m *Manager) aclsPath(iqn string) string   { return path.Join(m.tpgPath(iqn), "acls") }
func (m *Manager) tpgLunDir(iqn string) string {
	return path.Join(m.tpgPath(iqn), "lun", "lun_0")
}
func (m *Manager) aclMappedLunDir(iqn, ini string) string {
	return path.Join(m.aclsPath(iqn), ini, "mapped_lun_0")
}
func (m *Manager) backstorePath(name string) string { return path.Join(m.coreRoot(), name) }

// ---- configfs 原语 ----

// mkdirIfMissing 幂等地创建目录（configfs 里 mkdir 即"创建内核对象"）。
func (m *Manager) mkdirIfMissing(p string) error {
	if fi, err := os.Stat(p); err == nil {
		if !fi.IsDir() {
			return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
				WithArg("path", p).WithCause(os.ErrExist)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).WithCause(err)
	}
	if err := os.Mkdir(p, 0o755); err != nil && !os.IsExist(err) {
		return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).WithCause(err)
	}
	return nil
}

// writeAttr 往 configfs 属性文件写入一个值。
//
// os.WriteFile 使用 O_WRONLY|O_CREATE|O_TRUNC，与 shell 的 `echo x > attr` 语义一致，
// 这正是 configfs 属性所期望的写法。
func (m *Manager) writeAttr(p, value string) error {
	return os.WriteFile(p, []byte(value), 0o644)
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

// configfsReady 判断 configfs 是否已就绪：<root> 可访问，且 <root>/iscsi 存在、可写。
//
// 可写性用权限位近似判断（本服务以 root 运行，owner 位即可代表实际可写性），
// 这样无需引入 syscall.Access 之类的平台调用。
func (m *Manager) configfsReady() bool {
	if fi, err := os.Stat(m.root); err != nil || !fi.IsDir() {
		return false
	}
	fi, err := os.Stat(m.iscsiRoot())
	if err != nil || !fi.IsDir() {
		return false
	}
	return fi.Mode().Perm()&0o200 != 0
}

// loadModules 加载 LIO 所需内核模块。
//
// target_core_mod 提供 core 子系统（backstore），iscsi_target_mod 提供 iscsi 子系统。
// 先加载 core 再加载 iscsi（iscsi_target_mod 依赖 target_core_mod）。
// 返回第一条错误仅供日志参考——两个模块可能因依赖关系而部分"失败"。
func (m *Manager) loadModules(ctx context.Context) error {
	var firstErr error
	for _, mod := range []string{"target_core_mod", "iscsi_target_mod"} {
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

// ensureReady 确保 configfs 可用；不可用时尝试加载一次内核模块后重试，
// 仍不可用则返回 system.unavailable（component=lio）。
func (m *Manager) ensureReady(ctx context.Context) error {
	if m.configfsReady() {
		return nil
	}
	if err := m.loadModules(ctx); err != nil {
		m.logger.Warn("加载 LIO 内核模块失败", "error", err)
	}
	if m.configfsReady() {
		m.logger.Info("LIO configfs 已就绪")
		return nil
	}
	return apperr.New(apperr.CodeUnavailable, http.StatusInternalServerError).
		WithArg("component", "lio")
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
