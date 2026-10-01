//go:build windows

package winvhd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Microsoft/go-winio/vhd"

	"vault/internal/apperr"
	"vault/internal/platform/winps"
)

const (
	// minSizeBytes VirtDisk API 对 MaximumSize 的硬下限（3MB）。
	minSizeBytes uint64 = 3 * 1024 * 1024
	// blockSizeInBytes VHDX 默认块大小 2MB。
	blockSizeInBytes uint32 = 2 * 1024 * 1024
	// blockSizeInMB 差异盘创建时使用的块大小（MB）。
	blockSizeInMB uint32 = 2
	// headFingerprintBytes 指纹计算读取的头部字节数（1MiB）。
	headFingerprintBytes int64 = 1024 * 1024
	// psTimeout 维护类 PowerShell 脚本的超时（compact/reset 可能较慢）。
	psTimeout = 30 * time.Minute
)

// Options 是 Manager 的构造参数。
type Options struct {
	// Logger 日志器，可为 nil。
	Logger *slog.Logger
	// HyperVModuleAvailable 目标环境是否具备 Hyper-V 模块（Set-VHD / Optimize-VHD）。
	// 它决定 ResetDiskIdentifier 的实现路径：不可用时直接返回
	// platform.reset_disk_id_unavailable，由上层决定降级策略。
	HyperVModuleAvailable bool
}

// Manager 管理本机的 VHDX 文件。
type Manager struct {
	logger          *slog.Logger
	hyperVAvailable bool
	ps              *winps.Runner

	mu   sync.Mutex
	open map[string]syscall.Handle // 已由本进程挂载的 VHDX（规范化路径 -> 句柄）
}

// HyperVAvailable 报告构造时探测到的 Hyper-V 模块可用性。
//
// 仅供能力展示使用（app 层通过 platform.BackendCapabilities 读取）；
// 不影响 ResetDiskIdentifier 的调用路径——那条路径由本包内部按同一标志自行判定。
func (m *Manager) HyperVAvailable() bool { return m.hyperVAvailable }

// NewManager 构造 Manager。
func NewManager(opt Options) *Manager {
	logger := opt.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		logger:          logger,
		hyperVAvailable: opt.HyperVModuleAvailable,
		ps:              winps.NewRunner(winps.Options{Logger: logger, Timeout: psTimeout}),
		open:            make(map[string]syscall.Handle),
	}
}

// CreateDynamic 创建一个动态扩展（稀疏）VHDX。
func (m *Manager) CreateDynamic(ctx context.Context, path string, sizeBytes uint64) error {
	return m.create(ctx, path, sizeBytes, vhd.CreateVirtualDiskFlagNone)
}

// CreateFixed 创建一个固定大小（预分配全部物理空间）的 VHDX。
func (m *Manager) CreateFixed(ctx context.Context, path string, sizeBytes uint64) error {
	return m.create(ctx, path, sizeBytes, vhd.CreateVirtualDiskFlagFullPhysicalAllocation)
}

// CreateDifferencing 创建差异盘（childPath 引用 parentPath，容量继承父盘）。
//
// 注意：父盘不能被移动/改名，且创建差异盘后父盘应视为只读。
func (m *Manager) CreateDifferencing(ctx context.Context, childPath, parentPath string) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if childPath == "" || parentPath == "" {
		return apperr.InvalidParam("path")
	}
	if err := os.MkdirAll(filepath.Dir(childPath), 0o755); err != nil {
		return mapVHDError("mkdir", err)
	}
	if err := m.runVHD(ctx, func() error {
		return vhd.CreateDiffVhd(childPath, parentPath, blockSizeInMB)
	}); err != nil {
		return err
	}
	m.logger.Info("已创建差异盘", "child", childPath, "parent", parentPath)
	return nil
}

func (m *Manager) create(ctx context.Context, path string, sizeBytes uint64, flag vhd.CreateVirtualDiskFlag) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if path == "" {
		return apperr.InvalidParam("path")
	}
	maximumSize, err := normalizeMaxSize(sizeBytes)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return mapVHDError("mkdir", err)
	}
	params := &vhd.CreateVirtualDiskParameters{
		Version: 2,
		Version2: vhd.CreateVersion2{
			MaximumSize:      maximumSize,
			BlockSizeInBytes: blockSizeInBytes,
		},
	}
	// 注意：使用 CREATE_VIRTUAL_DISK_VERSION_2 时 access mask 必须为 NONE，否则返回 ERROR_INVALID_PARAMETER(87)。
	if err := m.runVHD(ctx, func() error {
		handle, err := vhd.CreateVirtualDisk(path, vhd.VirtualDiskAccessNone, flag, params)
		if err != nil {
			return err
		}
		return syscall.CloseHandle(handle)
	}); err != nil {
		return err
	}
	m.logger.Info("已创建 VHDX", "path", path, "size_bytes", maximumSize, "fixed", flag == vhd.CreateVirtualDiskFlagFullPhysicalAllocation)
	return nil
}

// Attach 在服务端本地挂载 VHDX，返回可用于卸载的句柄。
//
// 调用方必须保留返回的句柄直至挂载结束（挂载为"非永久生命周期"，句柄关闭即自动分离），
// 并在结束时调用 Detach / DetachHandle。
func (m *Manager) Attach(ctx context.Context, path string, readOnly bool) (uintptr, error) {
	if err := ctxErr(ctx); err != nil {
		return 0, err
	}
	key := pathKey(path)
	m.mu.Lock()
	if _, ok := m.open[key]; ok {
		m.mu.Unlock()
		return 0, apperr.DiskBusy()
	}
	m.mu.Unlock()

	// 只读语义由 attach 的标志位承担，**不体现在 OpenVirtualDisk 的 access mask 上**
	// （那个 mask 只能是 NONE，见 openForAttach 的说明）。
	flags := vhd.AttachVirtualDiskFlagNone
	if readOnly {
		flags |= vhd.AttachVirtualDiskFlagReadOnly
	}

	var handle syscall.Handle
	err := m.runVHD(ctx, func() error {
		h, err := openForAttach(path)
		if err != nil {
			return err
		}
		// Version 2 是 RS5 之后的结构，go-winio 兼容 version 1/2。
		params := &vhd.AttachVirtualDiskParameters{Version: 2}
		if err := vhd.AttachVirtualDisk(h, flags, params); err != nil {
			_ = syscall.CloseHandle(h)
			return err
		}
		handle = h
		return nil
	})
	if err != nil {
		return 0, err
	}

	m.mu.Lock()
	m.open[key] = handle
	m.mu.Unlock()
	m.logger.Info("已挂载 VHDX", "path", path, "read_only", readOnly, "handle", uintptr(handle))
	return uintptr(handle), nil
}

// Detach 卸载 VHDX。若该 VHDX 由本进程 Attach 过，会先关闭对应句柄再尝试句柄之外的分离。
func (m *Manager) Detach(ctx context.Context, path string) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	key := pathKey(path)
	m.mu.Lock()
	handle, tracked := m.open[key]
	delete(m.open, key)
	m.mu.Unlock()

	if tracked {
		if err := syscall.CloseHandle(handle); err != nil {
			m.logger.Warn("关闭 VHDX 句柄失败", "path", path, "err", err.Error())
		}
		m.logger.Info("已卸载 VHDX", "path", path)
		return nil
	}

	if err := m.runVHD(ctx, func() error { return vhd.DetachVhd(path) }); err != nil {
		return err
	}
	m.logger.Info("已分离 VHDX", "path", path)
	return nil
}

// DetachHandle 关闭 Attach 返回的句柄（等价于卸载，供持有句柄的调用方使用）。
func (m *Manager) DetachHandle(handle uintptr) error {
	h := syscall.Handle(handle)
	m.mu.Lock()
	for key, tracked := range m.open {
		if tracked == h {
			delete(m.open, key)
			break
		}
	}
	m.mu.Unlock()
	if err := syscall.CloseHandle(h); err != nil {
		return mapVHDError("detach_handle", err)
	}
	return nil
}

// openForAttach 以 attach 场景下**唯一合法**的组合打开 VHDX：access mask 必须为 NONE。
//
// ⚠️ 不能用 VIRTUAL_DISK_ACCESS_ALL / ATTACH_RW / ATTACH_RO / GET_INFO：
// go-winio 固定以 version 2 的 OPEN_VIRTUAL_DISK_PARAMETERS 调用 OpenVirtualDisk，
// 而该版本要求 access mask 为 NONE，其它取值一律返回 ERROR_INVALID_PARAMETER(87)
// （"The parameter is incorrect."）——与 CreateVirtualDisk 的 version 2 规则同源。
//
// 实测（go-winio v0.6.2）：
//
//	mask=None                               → OK
//	mask=All / AttachRW / AttachRO / GetInfo → ERROR_INVALID_PARAMETER
//
// 这里连标志位一起对齐 go-winio 自带的 AttachVhd（CachedIO + IgnoreRelativeParentLocator），
// 后者是被广泛使用的"挂载 VHD"实现；IgnoreRelativeParentLocator 对差异盘的相对父路径也更宽容。
//
// 只读不在这里表达：它由 AttachVirtualDisk 的 READ_ONLY 标志承担（见 Attach）。
func openForAttach(path string) (syscall.Handle, error) {
	return vhd.OpenVirtualDisk(
		path,
		vhd.VirtualDiskAccessNone,
		vhd.OpenVirtualDiskFlagCachedIO|vhd.OpenVirtualDiskFlagIgnoreRelativeParentLocator,
	)
}

// PhysicalPath 返回 VHDX 挂载后对应的物理磁盘路径（形如 \\.\PhysicalDrive2）。
//
// 注意：本方法要求该 VHDX 已在本机挂载，否则拿不到物理路径。
// 打开方式同样走 openForAttach（GET_INFO 掩码在 version 2 参数下不可用）。
func (m *Manager) PhysicalPath(ctx context.Context, path string) (string, error) {
	if err := ctxErr(ctx); err != nil {
		return "", err
	}
	var physicalPath string
	err := m.runVHD(ctx, func() error {
		handle, err := openForAttach(path)
		if err != nil {
			return err
		}
		defer syscall.CloseHandle(handle) //nolint:errcheck
		p, err := vhd.GetVirtualDiskPhysicalPath(handle)
		if err != nil {
			return err
		}
		physicalPath = p
		return nil
	})
	if err != nil {
		return "", err
	}
	return physicalPath, nil
}

// Exists 判断 VHDX 文件是否存在。
func (m *Manager) Exists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// FileSize 返回 VHDX 文件大小（字节，即 VHDX 在宿主卷上的实际占用）。
func (m *Manager) FileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, mapVHDError("stat", err)
	}
	return info.Size(), nil
}

// Fingerprint 计算母盘内容指纹，形如 "size=123;mtime=456;head=<sha256>"。
//
// 用于检测"绕过 Vault 直接挂载母盘写入"的情况（见 docs/implementation.md 5.4）：
// 尺寸、最后写入时间、头部 1MiB 的 SHA-256 三者任一变化即视为母盘被修改。
func (m *Manager) Fingerprint(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", mapVHDError("fingerprint", err)
	}
	defer file.Close() //nolint:errcheck

	info, err := file.Stat()
	if err != nil {
		return "", mapVHDError("fingerprint", err)
	}

	hasher := sha256.New()
	if _, err := io.CopyN(hasher, file, headFingerprintBytes); err != nil && err != io.EOF {
		return "", mapVHDError("fingerprint", err)
	}

	return fmt.Sprintf("size=%d;mtime=%d;head=%s",
		info.Size(), info.ModTime().Unix(), hex.EncodeToString(hasher.Sum(nil))), nil
}

// Delete 删除 VHDX 文件。文件被挂载或被 iSCSI 使用时返回 platform.sharing_violation。
func (m *Manager) Delete(ctx context.Context, path string) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return mapVHDError("delete", err)
	}
	m.logger.Info("已删除 VHDX 文件", "path", path)
	return nil
}

// runVHD 执行阻塞的 VirtDisk 调用，并在 ctx 取消时立即返回。
//
// 注意：VirtDisk 的系统调用不可中断，ctx 取消后底层调用仍会在后台跑完（可能留下未回收的句柄），
// 因此调用方应使用足够宽松的超时，不要依赖取消来立即终止长任务。
func (m *Manager) runVHD(ctx context.Context, fn func() error) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		// ⚠️ 必须显式判空后再返回：mapVHDError 的返回类型是 *apperr.Error，
		// 直接 `return mapVHDError("vhd", nil)` 会得到一个"接口非 nil、内部指针为 nil"
		// 的**假错误**（typed nil）。后果：
		//   1) 上层 `if err != nil` 把成功当失败（本机所有 VHDX 操作都会"失败"）；
		//   2) 该错误经任务队列传到 worker 后，errors.Is/As 会对 nil 接收者解引用，
		//      直接 panic 打挂整个服务端进程。
		if err == nil {
			return nil
		}
		return mapVHDError("vhd", err)
	case <-ctx.Done():
		m.logger.Warn("VHDX 操作被取消（底层调用仍在后台执行）", "err", ctx.Err().Error())
		return apperr.New(CodeVHDFailed, http.StatusInternalServerError).WithCause(ctx.Err())
	}
}

// ctxErr 把 context 错误映射为平台错误。
func ctxErr(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return apperr.New(CodeVHDFailed, http.StatusInternalServerError).WithCause(err)
	}
	return nil
}

// normalizeMaxSize 校验并规整 VHDX 的 MaximumSize（必须是 512 的倍数且不小于 3MB）。
func normalizeMaxSize(sizeBytes uint64) (uint64, error) {
	if sizeBytes < minSizeBytes {
		return 0, apperr.InvalidParam("size_bytes")
	}
	return (sizeBytes + 511) / 512 * 512, nil
}

// pathKey 生成用于句柄登记的路径键（大小写不敏感）。
func pathKey(path string) string {
	return strings.ToLower(filepath.Clean(path))
}
