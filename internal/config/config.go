// Package config 负责服务端配置的加载、校验、默认推导与热重载。
//
// 设计要点（见 docs/implementation.md 3.4.2）：
//   - client_compat（客户端版本兼容区间）是**配置文件的一部分**，并支持整体关闭；
//   - 字段"缺失"→ 按服务端版本推导默认区间；字段"显式留空"→ 该端不限制；
//     **显式写了非法值 → 启动失败（fail-fast）**，避免"全部客户端被判不兼容"这种更难排查的故障；
//   - api_version 是协议常量，不在此配置，也不可关闭。
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"vault/internal/semver"
	"vault/internal/version"
)

// Config 是服务端全量配置。
type Config struct {
	Server       ServerConfig       `yaml:"server"`
	HTTP         HTTPConfig         `yaml:"http"`
	Database     DatabaseConfig     `yaml:"database"`
	Storage      StorageConfig      `yaml:"storage"`
	ClientCompat ClientCompatConfig `yaml:"client_compat"`
	Log          LogConfig          `yaml:"log"`
	Security     SecurityConfig     `yaml:"security"`
	Update       UpdateConfig       `yaml:"update"`
	Platform     PlatformConfig     `yaml:"platform"`
}

// ServerConfig 服务端自身标识。instance_id 用于多服务端场景下的身份识别。
type ServerConfig struct {
	// InstanceID 服务端唯一标识，首次启动自动生成并持久化。
	InstanceID string `yaml:"instance_id"`
	// Name 展示用别名，管理页可改。
	Name string `yaml:"name"`
}

// HTTPConfig HTTP 服务配置。
type HTTPConfig struct {
	Listen string    `yaml:"listen"`
	TLS    TLSConfig `yaml:"tls"`
}

// TLSConfig TLS 配置。启用时使用服务端自建 CA 签发的证书。
type TLSConfig struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// DatabaseConfig 双驱动数据库配置。
type DatabaseConfig struct {
	// Driver 取值：sqlite | mysql。
	Driver string       `yaml:"driver"`
	SQLite SQLiteConfig `yaml:"sqlite"`
	MySQL  MySQLConfig  `yaml:"mysql"`
}

// SQLiteConfig SQLite 配置。DB 文件必须位于本地磁盘。
type SQLiteConfig struct {
	Path string `yaml:"path"`
}

// MySQLConfig MySQL 配置。
type MySQLConfig struct {
	DSN          string `yaml:"dsn"`
	MaxOpenConns int    `yaml:"max_open_conns"`
	MaxIdleConns int    `yaml:"max_idle_conns"`
}

// StorageConfig 存储布局与限制。
type StorageConfig struct {
	// WhitelistRoot 存储（Storage）根目录（旧字段，单根）。
	//
	// ⚠️ 仅作为「存储」的**首次初始化种子**：服务端首次启动（storages 表为空）时会把它们
	// 写入数据库；之后请在管理端「存储」模块维护，本配置**不再实时生效**。
	//
	// 与 WhitelistRoots 的关系：WhitelistRoots 非空时优先使用它；
	// 为空时回退到本字段（老配置继续可用）。二者最终都会被规范化。
	WhitelistRoot string `yaml:"whitelist_root"`
	// WhitelistRoots 存储（Storage）根目录列表（多根）。非空时优先于 WhitelistRoot。
	//
	// ⚠️ 同 WhitelistRoot：仅作首次初始化种子；数据库（storages 表）才是唯一真源。
	//
	// 多个根可以位于同一块盘/同一个卷：统计磁盘可用空间时会**按卷去重**，
	// 互为父子（如 D:\Vault 与 D:\Vault\extra）的根只按上游所在卷统计一次。
	WhitelistRoots []string `yaml:"whitelist_roots"`
	// DisksDir / StagingDir 相对**各个**存储根的子目录；
	// 二者必须同卷，否则上传完成后无法原子改名（暂存目录会建在选定的目标根下）。
	DisksDir   string `yaml:"disks_dir"`
	StagingDir string `yaml:"staging_dir"`
	TrashDir   string `yaml:"trash_dir"`
	// OrphanDir 孤儿文件暂存目录（相对各个根，即 <root>/meta/orphan）。
	OrphanDir string `yaml:"orphan_dir"`
	// SourceRoots 允许被浏览（/v1/fs/*）与作为"源目录"（仓库 Create 的 source_dir）的服务端
	// 本地目录白名单。
	//
	// 语义：非空时**只允许**这些目录（及其子目录）；留空则回退为当前生效的存储根
	// （storages 表中启用的存储路径；DB 无记录时回退 whitelist_roots 种子根），
	// 从而与历史默认行为保持兼容（原先任意绝对目录都可用 → 现在至少所有存储根可用）。
	//
	// 相对路径按**配置文件所在目录**解析（与 update.artifacts_dir / pki 同一套语义）。
	SourceRoots []string `yaml:"source_roots"`
	// RequireSameVolume 强制校验 DisksDir 与 StagingDir 同卷，默认 true。
	RequireSameVolume *bool `yaml:"require_same_volume"`
	// DefaultMaxDiffDisks 新建共享库时单个母盘允许的差异盘默认上限。
	DefaultMaxDiffDisks int `yaml:"default_max_diff_disks"`
	// SizeGranularityMB 创建 VHDX 时容量向上取整的粒度（**MB**）。默认 1（1MB）。
	//
	// 为什么是 MB 而不是 GB：见 domain.DefaultSizing 的说明 —— GB 粒度会让空目录/小目录
	// 也推导出 1GB 的标称容量，而分配时按母盘标称容量预留用量，等于"没用就占 1G"。
	SizeGranularityMB int64 `yaml:"size_granularity_mb"`
	// SizeGranularityGB **已废弃**（原 GB 粒度，会让空目录也占 1GB 配额）。
	//
	// 保留字段仅为兼容旧配置文件（否则解析会失败），**不再参与任何计算**；
	// 粒度一律由 SizeGranularityMB 决定。
	SizeGranularityGB int64 `yaml:"size_granularity_gb"`
	// SizeReservePermille 容量预留千分比（文件系统元数据 + 碎片）。
	SizeReservePermille int64 `yaml:"size_reserve_permille"`
	// MinVolumeFreePermille 卷可用空间低于该千分比时禁止新建/分配。
	MinVolumeFreePermille int64 `yaml:"min_volume_free_permille"`
	// TrashRetentionDays 软删除保留天数。
	TrashRetentionDays int `yaml:"trash_retention_days"`
}

// EffectiveRoots 返回规范化后的存储（Storage）种子根列表。
//
// ⚠️ 仅作为「存储」实体的**首次初始化种子**与数据库不可用时的兜底；运行时放置以
// 数据库的 storages 表为准（见 app.Deps.storageSelection）。
//
// 语义：WhitelistRoots 非空时优先使用它；否则回退到 WhitelistRoot（老配置继续可用）。
// 规范化 = 去空白 + filepath.Clean（去尾部斜杠、统一分隔符）+ 大小写不敏感去重
// （Windows 路径大小写不敏感，`D:\Vault` 与 `d:\vault` 视为同一个根）。
func (s StorageConfig) EffectiveRoots() []string {
	src := s.WhitelistRoots
	if len(src) == 0 && strings.TrimSpace(s.WhitelistRoot) != "" {
		src = []string{s.WhitelistRoot}
	}
	out := make([]string, 0, len(src))
	seen := make(map[string]bool, len(src))
	for _, r := range src {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		r = filepath.Clean(r)
		key := strings.ToLower(r)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	return out
}

// PlatformConfig 配置"虚拟磁盘 / 存储池 / iSCSI"三块平台能力的后端选择与参数。
//
// 平台后端由**构建产物**决定（Windows 包只含 winbackend，Linux 包只含 linuxbackend），
// 因此 kind 只作显式声明与一致性校验：声明为 windows 却跑在 Linux 上会在启动时报错，
// 而不是静默降级成错误的磁盘布局。
//
// ⚠️ 缓存设备（SSD）**不在此配置**：它在"建池"时由管理端选择器选定，
// 之后由 LVM 自己持久持有（dm-cache 元数据在 <pool>_cmeta），本系统只读不存。
type PlatformConfig struct {
	// Kind 平台种类：auto（按运行 OS）| windows | linux。默认 auto。
	Kind string `yaml:"kind"`
	// LVM 存储池参数（仅 Linux 有意义）。
	LVM LVMPoolConfig `yaml:"lvm"`
	// Iscsi iSCSI 目标参数。
	Iscsi IscsiPlatformConfig `yaml:"iscsi"`

	// AutoRepair 启动时自动修复"可安全自动化"的系统依赖缺项：
	// 挂载 configfs、加载/重载 LIO 内核模块、写 /etc/modules-load.d/vault-lio.conf。
	// nil 视为启用；显式设为 false 时启动只探测并告警，不改动宿主机
	// （适合由 Ansible/镜像构建等外部工具接管依赖的环境）。
	AutoRepair *bool `yaml:"auto_repair"`
	// AutoInstall 允许在检测到命令行工具（LVM2 / mkfs 等）缺失时自动安装对应软件包
	// （apt-get / dnf / yum / zypper / apk / pacman）。nil 视为启用。
	//
	// 离线环境、或不允许服务端自行装包的生产环境请显式设为 false——
	// 此时缺失项仍会在启动日志与 `Vault-Server doctor` 里给出**人工安装命令**。
	AutoInstall *bool `yaml:"auto_install"`
}

// AutoRepairEnabled 返回是否允许启动期自动修复系统依赖（未配置视为启用）。
func (p PlatformConfig) AutoRepairEnabled() bool { return p.AutoRepair == nil || *p.AutoRepair }

// AutoInstallEnabled 返回是否允许自动安装缺失的依赖包（未配置视为启用）。
func (p PlatformConfig) AutoInstallEnabled() bool { return p.AutoInstall == nil || *p.AutoInstall }

// LVMPoolConfig 是 Linux 侧 LVM thin pool 的目标位置与建池参数。
//
// VG / ThinPool 只用于"瞄准哪个池"：池不存在时管理端会引导执行一次初始化建池。
type LVMPoolConfig struct {
	// VG 目标卷组名。不得含 '-'（device-mapper 会转义，导致磁盘引用不可逆解析）。
	VG string `yaml:"vg"`
	// ThinPool 目标 thin pool 名。限制同上。
	ThinPool string `yaml:"thin_pool"`
	// ChunkSize thin pool 的 chunk 大小（如 256K）。
	ChunkSize string `yaml:"chunk_size"`
	// MetadataSize thin pool 元数据大小（如 256M / 4G）；**留空表示按池容量自适应**
	// （见 linuxlvm.poolMetaSizeFor：约 1/500，夹在 64 MiB ~ 4 GiB）。
	MetadataSize string `yaml:"metadata_size"`
	// AutoextendThreshold / AutoextendPercent 仅用于**校验与告警**：
	// 真正的自动扩容由宿主机的 lvm.conf + lvm2-monitor 执行，本服务不修改宿主机配置。
	AutoextendThreshold int `yaml:"autoextend_threshold"`
	AutoextendPercent   int `yaml:"autoextend_percent"`
	// WatermarkPercent 应用层空间闸门：thin pool 数据/元数据使用率达到即拒绝新建磁盘。
	WatermarkPercent float64 `yaml:"watermark_percent"`
}

// IscsiPlatformConfig 是 iSCSI 目标服务的参数。
type IscsiPlatformConfig struct {
	// Backend 后端实现名。Linux 目前只有 lio。
	Backend string `yaml:"backend"`
	// ConfigFSRoot LIO 在 configfs 中的挂载点（仅 Linux）。
	ConfigFSRoot string `yaml:"configfs_root"`
	// Portals 下发给客户端的 iSCSI 门户地址列表（如 ["192.168.1.10:3260"]）。
	//
	// **第一条生效**：填具体地址即固定客户端连接的门户；填通配地址（如 0.0.0.0:3260）
	// 表示地址自动推导（http.listen 主机 → 本机首个非回环 IP → 主机名），端口仍取此值。
	// 见 internal/app/lease.go 的 portalFromConfig。
	Portals []string `yaml:"portals"`
	// IQNPrefix 目标 IQN 前缀：完整 IQN = <prefix>:<target_name>。
	IQNPrefix string `yaml:"iqn_prefix"`
}

// ClientCompatConfig 客户端版本兼容区间配置。
type ClientCompatConfig struct {
	// Enabled 总开关。nil 视为 true。
	Enabled *bool `yaml:"enabled"`
	// Min 最低支持的客户端版本（含）。nil=缺失→推导；""=显式留空→不设下限。
	Min *string `yaml:"min"`
	// Max 最高支持的客户端版本（含，支持 "1.9.x" 通配）。nil=缺失→推导；""=显式留空→不设上限。
	Max *string `yaml:"max"`
}

// LogConfig 日志配置。
type LogConfig struct {
	Level       string `yaml:"level"`
	Dir         string `yaml:"dir"`
	KeepDays    int    `yaml:"keep_days"`
	AlsoConsole bool   `yaml:"also_console"`
}

// SecurityConfig 安全相关。
type SecurityConfig struct {
	// MasterKey 用于加密 CHAP 密钥等敏感字段（AES-GCM）。留空则首次启动生成并写回配置文件。
	MasterKey string `yaml:"master_key"`

	// BootstrapEnabled 是否允许通过初始化接口创建首个超级管理员。
	//
	// 仅在系统**尚未初始化**（无超级管理员且无数据）时才有意义——初始化是一次性的。
	// nil 视为启用。设为 false 可彻底关闭 Web/客户端初始化入口，改用 CLI 引导。
	BootstrapEnabled *bool `yaml:"bootstrap_enabled"`

	// SuperAdminEnabled 内置超级管理员账号是否可用。nil 视为启用。
	//
	// 账号本身存放在数据库中（由初始化流程创建），本开关只控制其"能否登录"：
	// 关闭后该账号立即失效，用于收紧权限；也可作为锁定恢复的应急开关。
	SuperAdminEnabled *bool `yaml:"super_admin_enabled"`

	// BootstrapWindow 首次启动后允许初始化的时间窗口（自首次启动起算）。
	//
	// 为 0 表示不限制。设置窗口可降低"部署后长时间未初始化、被局域网内他人抢注"的风险。
	BootstrapWindow Duration `yaml:"bootstrap_window"`

	// SessionTTL 管理页会话有效期。
	SessionTTL Duration `yaml:"session_ttl"`
	// LoginMaxFailures / LoginLockDuration 防爆破。
	LoginMaxFailures int      `yaml:"login_max_failures"`
	LoginLockWindow  Duration `yaml:"login_lock_window"`
	LoginLock        Duration `yaml:"login_lock"`
	// LeaseTTL 客户端租约有效期。
	LeaseTTL Duration `yaml:"lease_ttl"`
	// LeaseHeartbeatInterval 客户端心跳间隔（下发给客户端）。
	LeaseHeartbeatInterval Duration `yaml:"lease_heartbeat_interval"`
	// RevokeCooldown 踢下线后的重连抑制窗口。
	RevokeCooldown Duration `yaml:"revoke_cooldown"`
	// EnrollmentTokenTTL 一次性注册令牌有效期。
	EnrollmentTokenTTL Duration `yaml:"enrollment_token_ttl"`
}

// UpdateConfig 客户端更新分发（服务端仅作 mirror，不参与签名）。
type UpdateConfig struct {
	// ArtifactsDir 更新包存放目录。
	ArtifactsDir string `yaml:"artifacts_dir"`
	// Channel 默认分发通道。
	Channel string `yaml:"channel"`
	// MinClientVersion 低于此版本的客户端将被强制升级（留空表示不强制）。
	MinClientVersion string `yaml:"min_client_version"`
}

// Duration 是支持 YAML 字符串（如 "120s"）的时长类型。
type Duration time.Duration

// UnmarshalYAML 实现 yaml.Unmarshaler。
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	s = strings.TrimSpace(s)
	if s == "" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("config: 非法时长 %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalYAML 实现 yaml.Marshaler。
func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

// Std 返回标准库时长。
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Loaded 是加载并校验后的配置快照。
type Loaded struct {
	// Raw 原始配置。
	Raw Config
	// Compat 已解析并推导完成的客户端兼容区间。
	Compat ResolvedCompat
	// Path 配置文件路径（用于展示与写回）。
	Path string
}

// ResolvedCompat 是解析后的客户端兼容区间。
type ResolvedCompat struct {
	// Enabled 为 false 时，客户端版本区间判定被整体跳过。
	Enabled bool
	// Min 为 nil 表示不设下限。
	Min *semver.Version
	// Max 为 nil 表示不设上限。
	Max *semver.Version
	// WidenedUpperBound 为 true 表示：默认推导出的区间自身不自洽，
	// 因此把上界放宽为"不限制"（详见 resolveCompat 的说明）。
	// 调用方应在启动日志中给出 WARN。
	WidenedUpperBound bool
}

// Load 从 path 读取并校验配置。文件不存在时返回错误。
func Load(path string) (*Loaded, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: 读取配置失败: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("config: 解析 YAML 失败: %w", err)
	}

	applyEnvOverrides(&c)
	c.applyDefaults()

	compat, err := c.resolveCompat()
	if err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, err
	}

	return &Loaded{Raw: c, Compat: compat, Path: path}, nil
}

// 模板 config.example.yaml 是**两个平台共用一份**的，其中存储种子的默认值只能写成一个
// 平台的形态；而校验要求"每个根都必须是**本平台**的绝对路径"（见 validate），于是另一个
// 平台拿到模板原样生成就会立刻启动失败：
//
//	加载配置失败: config: storage.whitelist_roots 的每个根都必须是绝对路径: "D:\\VaultData"
//
// （Windows 的 D:\VaultData 在 Linux 上 filepath.IsAbs=false；/var/lib/vault 在 Windows
// 上同理。）因此"从模板生成配置文件"这一步要按当前平台改写这几处字面量。
const (
	// winStorageRoot 是模板里 Windows 形态的存储种子默认根（给人看的写法）。
	winStorageRoot = `D:\VaultData`
	// winStorageRootYAMLLiteral 是它在模板里的**字面量**写法：位于 YAML 双引号内，
	// 反斜杠被转义成两个，所以文件里实际就是 D:\\VaultData 这 12 个字符。
	winStorageRootYAMLLiteral = `D:\\VaultData`
	// linuxStorageRoot 是 Linux 上的等价默认根：取存储挂载点的父目录
	// （「存储」的默认挂载点是 /var/lib/vault/storages/<名称>-<短 ID>，见 docs/design.md）。
	// ⚠️ Linux 上该根**不参与存储种子**（否则会种出一个绕过 thin pool 的目录存储），
	// 只作为 GET /v1/fs/* 与建库 source_dir 的默认浏览白名单；存储请在管理端创建。
	linuxStorageRoot = "/var/lib/vault"
)

// DefaultStorageRoot 返回该平台下模板默认的存储种子根。
//
// 供启动提示与下面的模板改写共用同一份取值，避免文案与生成结果两处各写一份而漂移。
func DefaultStorageRoot(goos string) string {
	if goos == "windows" {
		return winStorageRoot
	}
	return linuxStorageRoot
}

// platformizeExample 按 goos 改写模板中"另一个平台专用"的存储种子默认值。
//
// 只做**字面量替换、不做 YAML 往返**：生成出来的 config.yaml 必须保留模板的全部注释
// （运维就是照着注释改配置的，见 persist.go 里 Save 会丢注释的说明）。
// 注释里的单反斜杠写法（`# 建议独立数据盘，例如 D:\VaultData`）也一并改写，
// 否则 Linux 生成的配置里会留着"建议用 D:\VaultData"这种自相矛盾的说明。
func platformizeExample(goos string, data []byte) []byte {
	if goos == "windows" {
		return data // 模板本身就是 Windows 形态，无需改写
	}
	out := bytes.ReplaceAll(data, []byte(winStorageRootYAMLLiteral), []byte(linuxStorageRoot))
	return bytes.ReplaceAll(out, []byte(winStorageRoot), []byte(linuxStorageRoot))
}

// EnsureFromExample 在目标配置文件**不存在**时，从同目录的模板文件生成一份。
//
// 模板路径由目标路径推导：把 <base><ext> 映射为 <base>.example<ext>，
// 例如 config.yaml → config.example.yaml。
//
// 这样做的两个目的：
//   - 部署包只需携带 config.example.yaml，避免升级解压时覆盖运维已修改的 config.yaml；
//   - 首次启动即自动得到一份可用的配置，开箱可跑（首次启动会再补写实例 ID 与主密钥）。
//
// 生成时会把模板里"另一个平台专用"的默认值改写成当前平台可用的值（见 platformizeExample），
// 保证生成的 config.yaml **一定能通过本平台的校验**、开箱即可启动。
//
// 返回值 created 表示本次是否真的生成了新文件；目标已存在时返回 false, nil（绝不覆盖）。
// 若目标不存在且模板也不存在，返回带明确指引的错误。
func EnsureFromExample(path string) (created bool, err error) {
	if strings.TrimSpace(path) == "" {
		return false, fmt.Errorf("config: 配置文件路径为空")
	}

	if _, statErr := os.Stat(path); statErr == nil {
		return false, nil // 已存在：绝不覆盖
	} else if !os.IsNotExist(statErr) {
		return false, fmt.Errorf("config: 检查配置文件失败: %w", statErr)
	}

	template := examplePathFor(path)
	data, readErr := os.ReadFile(template)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return false, fmt.Errorf(
				"config: 未找到配置文件 %s，也未找到模板 %s；"+
					"请把配置模板复制为 %s 后再启动，或用 -config 指定已有的配置文件",
				path, template, path)
		}
		return false, fmt.Errorf("config: 读取配置模板失败: %w", readErr)
	}

	// 模板是两个平台共用的一份：生成前按当前平台改写其中的平台专属默认值，
	// 否则 Linux 上会生成一份含 Windows 绝对路径的配置，紧接着就被 validate 拒绝。
	data = platformizeExample(runtime.GOOS, data)

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			return false, fmt.Errorf("config: 创建配置目录失败: %w", mkErr)
		}
	}
	// 0600：配置中会写入 security.master_key（用于加密 CHAP 密钥），需限制可读范围。
	if writeErr := os.WriteFile(path, data, 0o600); writeErr != nil {
		return false, fmt.Errorf("config: 生成配置文件失败: %w", writeErr)
	}
	return true, nil
}

// examplePathFor 由配置路径推导模板路径：<base><ext> → <base>.example<ext>。
func examplePathFor(path string) string {
	ext := filepath.Ext(path)
	return strings.TrimSuffix(path, ext) + ".example" + ext
}

// applyDefaults 填充未配置项的默认值。
func (c *Config) applyDefaults() {
	if c.Server.Name == "" {
		host, _ := os.Hostname()
		c.Server.Name = host
	}
	if c.HTTP.Listen == "" {
		c.HTTP.Listen = "127.0.0.1:8443"
	}
	if c.Database.Driver == "" {
		c.Database.Driver = "sqlite"
	}
	if c.Database.SQLite.Path == "" {
		c.Database.SQLite.Path = filepath.Join("data", "vault.db")
	}
	if c.Database.MySQL.MaxOpenConns <= 0 {
		c.Database.MySQL.MaxOpenConns = 25
	}
	if c.Database.MySQL.MaxIdleConns <= 0 {
		c.Database.MySQL.MaxIdleConns = 5
	}
	// 规范化存储（Storage）种子根：把 EffectiveRoots 的结果回写，使 WhitelistRoot 始终等于第一个根
	// （对外 storage_root 契约），WhitelistRoots 为全部去重后的根。
	// 注意：这些根仅用于首次初始化「存储」实体与 DB 不可用时的兜底，运行时以数据库为准。
	roots := c.Storage.EffectiveRoots()
	c.Storage.WhitelistRoots = roots
	c.Storage.WhitelistRoot = ""
	if len(roots) > 0 {
		c.Storage.WhitelistRoot = roots[0]
	}
	if c.Storage.DisksDir == "" {
		c.Storage.DisksDir = "disks"
	}
	if c.Storage.StagingDir == "" {
		c.Storage.StagingDir = "staging"
	}
	if c.Storage.TrashDir == "" {
		c.Storage.TrashDir = filepath.Join("meta", "trash")
	}
	if c.Storage.OrphanDir == "" {
		c.Storage.OrphanDir = filepath.Join("meta", "orphan")
	}
	if c.Storage.DefaultMaxDiffDisks <= 0 {
		c.Storage.DefaultMaxDiffDisks = 50
	}
	if c.Storage.SizeGranularityMB <= 0 {
		c.Storage.SizeGranularityMB = 1 // 1MB 粒度
	}
	if c.Storage.SizeReservePermille <= 0 {
		c.Storage.SizeReservePermille = 100 // 10%
	}
	if c.Storage.MinVolumeFreePermille <= 0 {
		c.Storage.MinVolumeFreePermille = 150 // 15%
	}
	if c.Storage.TrashRetentionDays <= 0 {
		c.Storage.TrashRetentionDays = 7
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Log.KeepDays <= 0 {
		c.Log.KeepDays = 30
	}
	if c.Security.SessionTTL == 0 {
		c.Security.SessionTTL = Duration(12 * time.Hour)
	}
	if c.Security.LoginMaxFailures <= 0 {
		c.Security.LoginMaxFailures = 5
	}
	if c.Security.LoginLockWindow == 0 {
		c.Security.LoginLockWindow = Duration(15 * time.Minute)
	}
	if c.Security.LoginLock == 0 {
		c.Security.LoginLock = Duration(15 * time.Minute)
	}
	if c.Security.LeaseTTL == 0 {
		c.Security.LeaseTTL = Duration(120 * time.Second)
	}
	if c.Security.LeaseHeartbeatInterval == 0 {
		c.Security.LeaseHeartbeatInterval = Duration(30 * time.Second)
	}
	if c.Security.RevokeCooldown == 0 {
		c.Security.RevokeCooldown = Duration(5 * time.Minute)
	}
	if c.Security.EnrollmentTokenTTL == 0 {
		c.Security.EnrollmentTokenTTL = Duration(15 * time.Minute)
	}
	if c.Update.Channel == "" {
		c.Update.Channel = "stable"
	}
	if c.Platform.Kind == "" {
		c.Platform.Kind = "auto"
	}
	if c.Platform.LVM.VG == "" {
		c.Platform.LVM.VG = "vg0"
	}
	if c.Platform.LVM.ThinPool == "" {
		c.Platform.LVM.ThinPool = "vault"
	}
	if c.Platform.LVM.ChunkSize == "" {
		c.Platform.LVM.ChunkSize = "256K"
	}
	// MetadataSize 刻意**不设默认值**：留空表示"按池容量自适应"
	// （见 linuxlvm.poolMetaSizeFor：约 1/500，夹在 64 MiB ~ 4 GiB）。
	// 曾经默认 4G，结果在两块 8G 盘（16G 卷组）上要吃掉 4G tmeta + 4G pmspare，
	// 用户想建的池直接建不出来（真机反馈）。
	if c.Platform.LVM.AutoextendThreshold <= 0 {
		c.Platform.LVM.AutoextendThreshold = 80
	}
	if c.Platform.LVM.AutoextendPercent <= 0 {
		c.Platform.LVM.AutoextendPercent = 20
	}
	if c.Platform.LVM.WatermarkPercent <= 0 {
		c.Platform.LVM.WatermarkPercent = 90
	}
	if c.Platform.Iscsi.Backend == "" {
		c.Platform.Iscsi.Backend = "lio"
	}
	if c.Platform.Iscsi.ConfigFSRoot == "" {
		c.Platform.Iscsi.ConfigFSRoot = "/sys/kernel/config/target"
	}
	if len(c.Platform.Iscsi.Portals) == 0 {
		c.Platform.Iscsi.Portals = []string{"0.0.0.0:3260"}
	}
	if c.Platform.Iscsi.IQNPrefix == "" {
		c.Platform.Iscsi.IQNPrefix = "iqn.2026-01.com.vault"
	}
}

// resolveCompat 解析并推导客户端兼容区间。
//
// 规则（见 docs/implementation.md 3.4.2）：
//
//	enabled=false            → 跳过区间判定，Enabled=false
//	enabled=true + 字段缺失   → 按服务端版本推导
//	enabled=true + 字段为空串 → 该端不限制
//	enabled=true + 两端均显式配置且不自洽 → 返回错误（fail-fast，配置错误应尽早暴露）
//
// 关键约束：**默认推导绝不能导致服务端启动失败**。
// 推导出的区间可能不自洽——典型场景是未注入版本号时 `version.Version` 为 `0.0.0-dev`，
// 推导上界为 `0.0.x`，而内置下限更高（例如 `0.9.0`）。
// 此时按"放宽上界为不限制"处理并标记 WidenedUpperBound，由调用方打 WARN，
// 避免"因为一个默认值把服务端锁死"这种更难排查的故障。
func (c *Config) resolveCompat() (ResolvedCompat, error) {
	enabled := c.ClientCompat.Enabled == nil || *c.ClientCompat.Enabled
	if !enabled {
		return ResolvedCompat{Enabled: false}, nil
	}

	parse := func(field string, p *string) (*semver.Version, bool, error) {
		if p == nil {
			return nil, false, nil // 缺失
		}
		s := strings.TrimSpace(*p)
		if s == "" {
			return nil, true, nil // 显式留空 → 不限制
		}
		v, err := semver.Parse(s)
		if err != nil {
			return nil, true, fmt.Errorf("config: client_compat.%s 非法: %w", field, err)
		}
		return &v, true, nil
	}

	minV, minSet, err := parse("min", c.ClientCompat.Min)
	if err != nil {
		return ResolvedCompat{}, err
	}
	maxV, maxSet, err := parse("max", c.ClientCompat.Max)
	if err != nil {
		return ResolvedCompat{}, err
	}

	// 两端都由用户显式给出时，区间必须自洽 —— 这是配置错误，fail-fast。
	if minSet && maxSet && minV != nil && maxV != nil && !minV.LTE(*maxV) {
		return ResolvedCompat{}, fmt.Errorf(
			"config: client_compat 区间非法: min=%s 大于 max=%s（请修正配置文件后重启）",
			minV.String(), maxV.String())
	}

	// 缺失（非显式留空）→ 推导默认值。
	if !minSet {
		floor := semver.MustParse(version.MinSupportedClientVersion)
		minV = &floor
	}
	if !maxSet {
		sv, parseErr := semver.Parse(version.Version)
		if parseErr != nil {
			sv = semver.Version{}
		}
		if sv.Major == 0 {
			// pre-1.0 阶段不承诺跨版本兼容语义：默认**不设上界**。
			// 否则在未注入版本号（如 `0.0.0-dev`）时推导出的 `0.0.x`
			// 会把所有客户端都判定为"服务端过旧"，开发期互相拒绝。
			maxV = nil
		} else {
			upper := semver.Version{Major: sv.Major, Minor: sv.Minor, Wildcard: semver.PatchWildcard}
			maxV = &upper
		}
	}

	res := ResolvedCompat{Enabled: true, Min: minV, Max: maxV}

	// 推导出的区间可能不自洽：放宽上界而不是拒绝启动。
	if minV != nil && maxV != nil && !minV.LTE(*maxV) {
		res.Max = nil
		res.WidenedUpperBound = true
	}

	return res, nil
}

// validate 校验必填项与目录约束。
func (c *Config) validate() error {
	if c.Server.InstanceID == "" {
		c.Server.InstanceID = "srv-" + uuid.NewString()
	}
	switch c.Database.Driver {
	case "sqlite", "mysql":
	default:
		return fmt.Errorf("config: database.driver 非法: %q（可选 sqlite|mysql）", c.Database.Driver)
	}
	if c.Database.Driver == "mysql" && strings.TrimSpace(c.Database.MySQL.DSN) == "" {
		return fmt.Errorf("config: database.driver=mysql 时必须配置 database.mysql.dsn")
	}
	roots := c.Storage.EffectiveRoots()
	if len(roots) == 0 {
		return fmt.Errorf("config: 必须配置 storage.whitelist_roots（或 storage.whitelist_root）：VHDX 只允许在这些目录下创建")
	}
	for _, root := range roots {
		if !filepath.IsAbs(root) {
			return fmt.Errorf("config: storage.whitelist_roots 的每个根都必须是绝对路径: %q", root)
		}
		if isNetworkPath(root) {
			return fmt.Errorf("config: storage.whitelist_roots 不允许位于网络路径: %q", root)
		}
	}
	if c.HTTP.TLS.Enabled {
		// 证书由服务端自建 CA 自动签发并维护在 <配置文件目录>/pki 下，
		// 因此 cert_file/key_file 允许留空（留空 = 仅由服务端内部管理，不额外导出副本）。
		if c.HTTP.TLS.CertFile == "" {
			c.HTTP.TLS.CertFile = filepath.Join("pki", "server.crt")
		}
		if c.HTTP.TLS.KeyFile == "" {
			c.HTTP.TLS.KeyFile = filepath.Join("pki", "server.key")
		}
	}
	return nil
}

// isNetworkPath 判断是否为 UNC 或映射网络驱动器。SQLite/白名单目录都不允许放网络路径。
func isNetworkPath(p string) bool {
	return strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//")
}

// splitRoots 按分号或逗号拆分环境变量里的多根列表（两者混用也可）。
func splitRoots(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool { return r == ';' || r == ',' })
}

// Compat 返回值语义便捷方法：判断某客户端版本是否满足当前配置的区间。
// 注意：调用方需先校验协议版本（api_version），该检查不可关闭。
func (r ResolvedCompat) Allows(clientVersion string) (bool, error) {
	if !r.Enabled {
		return true, nil
	}
	v, err := semver.Parse(clientVersion)
	if err != nil {
		return false, fmt.Errorf("config: 客户端版本非法 %q: %w", clientVersion, err)
	}
	return semver.InRange(v, r.Min, r.Max), nil
}

// Describe 返回人类可读的区间描述，用于日志与 system/info。
func (r ResolvedCompat) Describe() string {
	if !r.Enabled {
		return "已关闭版本检查"
	}
	minStr, maxStr := "不限", "不限"
	if r.Min != nil {
		minStr = r.Min.String()
	}
	if r.Max != nil {
		maxStr = r.Max.String()
	}
	return fmt.Sprintf("[%s, %s]", minStr, maxStr)
}

// applyEnvOverrides 应用少量关键环境变量覆盖，便于容器/脚本部署。
// 只覆盖"运维常改"的项，避免复杂的反射映射带来不可预期的行为。
func applyEnvOverrides(c *Config) {
	if v := os.Getenv("VAULT_SERVER_NAME"); v != "" {
		c.Server.Name = v
	}
	if v := os.Getenv("VAULT_SERVER_INSTANCE_ID"); v != "" {
		c.Server.InstanceID = v
	}
	if v := os.Getenv("VAULT_HTTP_LISTEN"); v != "" {
		c.HTTP.Listen = v
	}
	if v := os.Getenv("VAULT_DB_DRIVER"); v != "" {
		c.Database.Driver = v
	}
	if v := os.Getenv("VAULT_DB_SQLITE_PATH"); v != "" {
		c.Database.SQLite.Path = v
	}
	if v := os.Getenv("VAULT_DB_MYSQL_DSN"); v != "" {
		c.Database.MySQL.DSN = v
	}
	if v := os.Getenv("VAULT_STORAGE_ROOT"); v != "" {
		// 该变量语义是"把白名单覆盖为单个根"，因此同时清空多根列表。
		c.Storage.WhitelistRoot = v
		c.Storage.WhitelistRoots = nil
	}
	if v := os.Getenv("VAULT_STORAGE_ROOTS"); v != "" {
		// 分隔符：分号或逗号均可。
		c.Storage.WhitelistRoots = splitRoots(v)
		c.Storage.WhitelistRoot = ""
	}
	if v := os.Getenv("VAULT_LOG_LEVEL"); v != "" {
		c.Log.Level = v
	}
	if v := os.Getenv("VAULT_LOG_DIR"); v != "" {
		c.Log.Dir = v
	}
	if v := os.Getenv("VAULT_MASTER_KEY"); v != "" {
		c.Security.MasterKey = v
	}
}

// Watcher 监听配置文件变化并回调新的配置快照。
//
// 热重载语义（见 docs/implementation.md 3.4.2 约束 8）：
// 生效于新的 system/info 响应与后续连接；**不强制断开已建立的连接**。
// 若新配置校验失败，保留旧配置并记录错误，避免把服务改成不可用。
type Watcher struct {
	path     string
	onChange func(*Loaded)

	mu      sync.RWMutex
	current *Loaded

	stopOnce sync.Once
	stopCh   chan struct{}
	watcher  *fsnotify.Watcher
}

// NewWatcher 创建配置监听器，并立即加载一次。
func NewWatcher(path string, onChange func(*Loaded)) (*Watcher, error) {
	loaded, err := Load(path)
	if err != nil {
		return nil, err
	}
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("config: 创建文件监听失败: %w", err)
	}
	// 监听目录而非文件，避免编辑器"替换文件"式保存导致监听失效。
	dir := filepath.Dir(path)
	if err := fw.Add(dir); err != nil {
		_ = fw.Close()
		return nil, fmt.Errorf("config: 监听目录失败: %w", err)
	}

	w := &Watcher{
		path:     path,
		onChange: onChange,
		current:  loaded,
		stopCh:   make(chan struct{}),
		watcher:  fw,
	}
	go w.loop()
	return w, nil
}

// Current 返回当前生效的配置快照。
// Path 返回被监听的配置文件路径（管理页展示"配置来自哪个文件"，也是在线修改的落盘目标）。
func (w *Watcher) Path() string {
	return w.path
}

func (w *Watcher) Current() *Loaded {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.current
}

// Close 停止监听。
func (w *Watcher) Close() error {
	w.stopOnce.Do(func() { close(w.stopCh) })
	return w.watcher.Close()
}

func (w *Watcher) loop() {
	base := filepath.Base(w.path)
	var debounce *time.Timer

	for {
		select {
		case <-w.stopCh:
			return
		case event, ok := <-w.watcher.Events:
			if !ok {
				return
			}
			if filepath.Base(event.Name) != base {
				continue
			}
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}
			// 去抖：编辑器保存常触发多次事件。
			if debounce != nil {
				debounce.Stop()
			}
			debounce = time.AfterFunc(500*time.Millisecond, w.reload)
		case <-w.watcher.Errors:
			// 监听错误不致命，等待下一次事件即可。
		}
	}
}

func (w *Watcher) reload() {
	loaded, err := Load(w.path)
	if err != nil {
		// 保留旧配置：配置写坏不应把运行中的服务打挂。
		if w.onChange != nil {
			w.onChange(nil)
		}
		return
	}
	w.mu.Lock()
	w.current = loaded
	w.mu.Unlock()
	if w.onChange != nil {
		w.onChange(loaded)
	}
}
