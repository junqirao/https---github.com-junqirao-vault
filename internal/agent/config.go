package agent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"vault/internal/apperr"
)

// DataDir 返回代理本地数据目录（默认 %ProgramData%\Vault）。
func DataDir() string {
	if pd := strings.TrimSpace(os.Getenv("ProgramData")); pd != "" {
		return filepath.Join(pd, "Vault")
	}
	return filepath.Join(os.TempDir(), "Vault")
}

// DefaultConfigPath 返回默认的本地配置文件路径。
func DefaultConfigPath() string { return filepath.Join(DataDir(), "agent-config.json") }

// DefaultStatePath 返回默认的本地状态文件路径。
func DefaultStatePath() string { return filepath.Join(DataDir(), "agent-state.json") }

// RepoMountPref 是**单个存储库**在本机的挂载偏好（见 docs/agent-api.md「本地配置」）。
//
// 为什么是客户端本地配置而不是服务端库属性：挂载形态与挂载目录都是"这台机器"的事
// （盘符、D:\vault\xxx 之类的本地路径对别的机器没有意义），自动挂载也由本机代理执行。
type RepoMountPref struct {
	// MountMode letter | directory；空串表示跟随 DefaultMountMode。
	MountMode string `json:"mount_mode,omitempty"`
	// MountDir 目录模式的目标目录；空串表示跟随 DefaultMountDir（<根>\<服务端别名>\<库名>）。
	MountDir string `json:"mount_dir,omitempty"`
	// AutoMount 是否在该库所在客户端启动（拿到会话）后自动挂载它。
	//
	// 语义与全局 AutoMount 的区别：全局开关只管"恢复本机上次留下的挂载记录"，
	// 而这里是**每个库独立**的表态 —— 为 true 时即使本机没有记录（甚至还没有分配）
	// 也要挂上；为 false 时连记录都不恢复（用户明确关掉了这个库的自动挂载）。
	AutoMount bool `json:"auto_mount"`
}

// Config 是代理的本地配置（见 docs/agent-api.md「本地配置」）。
type Config struct {
	// AutoMount 是否在拿到服务端会话后自动恢复本地记录的挂载。
	AutoMount bool `json:"auto_mount"`
	// AutoLogin 是否在启动/断线后用本地客户端证书（identity.json）免密登录。
	//
	// 默认**开启**：配置文件里没有 auto_login 这一项（全新安装，或旧版本写下的配置）
	// 一律按开启处理，只有显式写成 false 才关闭（见 normalizeConfig 与 UnmarshalJSON）。
	//
	// 为什么默认开启：服务端会话只存在内存里，服务端一重启全部令牌失效；
	// 若默认关闭，用户每次重启服务端都要重新输一遍密码——这正是要消除的现象。
	AutoLogin bool `json:"auto_login"`
	// DefaultMountMode letter | directory。
	DefaultMountMode string `json:"default_mount_mode"`
	// DefaultMountDir 目录模式的挂载根目录。
	DefaultMountDir string `json:"default_mount_dir"`
	// DefaultDownloadDir 母盘内容下载的默认目标目录。
	DefaultDownloadDir string `json:"default_download_dir"`
	// DownloadConnections 母盘内容下载的分段并发数（1..8，默认 4）。
	DownloadConnections int `json:"download_connections"`
	// Language 界面语言。
	Language string `json:"language"`
	// StartAtLogin 是否开机启动。
	StartAtLogin bool `json:"start_at_login"`
	// UpdateChannel 更新通道。
	UpdateChannel string `json:"update_channel"`
	// ServerAlias 服务端别名（目录模式命名空间，见 3.4.5）。
	ServerAlias string `json:"server_alias"`
	// RepoMounts 按存储库 ID 保存的挂载偏好；没有条目的库跟随上面的全局默认值。
	RepoMounts map[string]RepoMountPref `json:"repo_mounts,omitempty"`

	// autoLoginSet 记录 auto_login 是否被**显式**配置过。
	//
	// 不落盘、不出现在 JSON 里：只为区分"用户明确关掉了免密登录"与"配置里没这项"。
	// 后者要按默认开启处理（见 AutoLogin 注释）。
	autoLoginSet bool
}

// UnmarshalJSON 解析本地配置，并记录 auto_login 是否出现过。
//
// 实现方式：先用 map 探一次键是否存在，再按普通结构体解析 —— 这样以后新增配置项
// 不需要同步维护第二份字段清单（写过一份重复清单的代码最容易在加字段时漏改）。
func (c *Config) UnmarshalJSON(data []byte) error {
	// plain 去掉本类型的方法集，避免递归调用本方法。
	type plain Config
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	var parsed plain
	if err := json.Unmarshal(data, &parsed); err != nil {
		return err
	}
	*c = Config(parsed)
	_, c.autoLoginSet = probe["auto_login"]
	return nil
}

// ConfigPatch 是本地配置的部分更新（字段为 nil 表示不改动）。
type ConfigPatch struct {
	AutoMount        *bool   `json:"auto_mount"`
	AutoLogin        *bool   `json:"auto_login"`
	DefaultMountMode *string `json:"default_mount_mode"`
	DefaultMountDir  *string `json:"default_mount_dir"`
	// DefaultDownloadDir 母盘内容下载的默认目标目录。
	DefaultDownloadDir *string `json:"default_download_dir"`
	// DownloadConnections 母盘内容下载的分段并发数（1..8）。
	DownloadConnections *int    `json:"download_connections"`
	Language            *string `json:"language"`
	StartAtLogin        *bool   `json:"start_at_login"`
	UpdateChannel       *string `json:"update_channel"`
	ServerAlias         *string `json:"server_alias"`
}

// defaultConfig 返回默认配置。
func defaultConfig() Config {
	return Config{
		AutoMount: false,
		// 默认开启证书免密登录：服务端重启会清空会话，用户不该为此重新输密码。
		// 这里不置 autoLoginSet，因此 normalizeConfig 也会把"未表态"归一为开启。
		AutoLogin:        true,
		DefaultMountMode: mountModeLetter,
		DefaultMountDir:  `C:\Vault`,
		// DefaultDownloadDir 与 DefaultMountDir 同风格：默认落在 C:\Vault 下。
		DefaultDownloadDir: `C:\Vault\Downloads`,
		// DownloadConnections 默认 4 段并发；高速链路上最稳，也不会撑爆服务端。
		DownloadConnections: downloadDefaultConnections,
		Language:            "zh-CN",
		StartAtLogin:        false,
		UpdateChannel:       "stable",
	}
}

// ConfigStore 负责本地配置的读写与持久化（%ProgramData%\Vault\agent-config.json）。
type ConfigStore struct {
	path   string
	logger *slog.Logger

	mu  sync.RWMutex
	cfg Config
}

// NewConfigStore 加载本地配置；文件不存在时创建默认配置。
func NewConfigStore(path string, logger *slog.Logger) (*ConfigStore, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if strings.TrimSpace(path) == "" {
		path = DefaultConfigPath()
	}
	s := &ConfigStore{path: path, logger: logger, cfg: defaultConfig()}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		var loaded Config
		if err := json.Unmarshal(data, &loaded); err != nil {
			return nil, fmt.Errorf("agent: 解析本地配置失败: %w", err)
		}
		s.cfg = normalizeConfig(loaded)
	case os.IsNotExist(err):
		if err := s.persistLocked(); err != nil {
			logger.Warn("创建本地配置文件失败（将仅使用内存配置）", "path", path, "error", err)
		}
	default:
		return nil, fmt.Errorf("agent: 读取本地配置失败: %w", err)
	}
	return s, nil
}

// normalizeConfig 补全空值，避免旧版本配置缺字段导致行为异常。
func normalizeConfig(c Config) Config {
	def := defaultConfig()
	// auto_login 缺省即开启：旧配置里没有这一项时按开启处理（见 Config.AutoLogin 注释）。
	// 已显式配置过（autoLoginSet）则完全尊重用户选择。
	if !c.autoLoginSet {
		c.AutoLogin = true
	}
	if strings.TrimSpace(c.DefaultMountMode) == "" {
		c.DefaultMountMode = def.DefaultMountMode
	}
	if strings.TrimSpace(c.DefaultMountDir) == "" {
		c.DefaultMountDir = def.DefaultMountDir
	}
	if strings.TrimSpace(c.DefaultDownloadDir) == "" {
		c.DefaultDownloadDir = def.DefaultDownloadDir
	}
	c.DownloadConnections = normalizeDownloadConnections(c.DownloadConnections)
	if strings.TrimSpace(c.Language) == "" {
		c.Language = def.Language
	}
	if strings.TrimSpace(c.UpdateChannel) == "" {
		c.UpdateChannel = def.UpdateChannel
	}
	// 每库挂载偏好：非法形态归一为"跟随默认"（不丢用户的自动挂载表态），目录去空白。
	// 这里就地改 map：它只在 load / Patch 的持有锁路径上，不会被外部共享。
	for repoID, pref := range c.RepoMounts {
		if mode := normalizeMountMode(pref.MountMode); mode != "" {
			pref.MountMode = mode
		} else {
			pref.MountMode = ""
		}
		pref.MountDir = strings.TrimSpace(pref.MountDir)
		c.RepoMounts[repoID] = pref
	}
	return c
}

// normalizeDownloadConnections 归一化下载分段并发数：未配置（<=0）取默认值，
// 超过上限取上限，1..8 原样保留（见 docs/agent-api.md「本地配置」）。
func normalizeDownloadConnections(n int) int {
	switch {
	case n <= 0:
		return downloadDefaultConnections
	case n > downloadMaxConnections:
		return downloadMaxConnections
	default:
		return n
	}
}

// Path 返回配置文件路径。
func (s *ConfigStore) Path() string { return s.path }

// Get 返回配置副本。
func (s *ConfigStore) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneConfig(s.cfg)
}

// cloneConfig 深拷贝配置里的 map，避免调用方（含只读的 GET /agent/config）改到 store 内部状态。
func cloneConfig(c Config) Config {
	if len(c.RepoMounts) == 0 {
		return c
	}
	out := make(map[string]RepoMountPref, len(c.RepoMounts))
	for repoID, pref := range c.RepoMounts {
		out[repoID] = pref
	}
	c.RepoMounts = out
	return c
}

// SetRepoMountPref 写入单个存储库的挂载偏好并持久化。
//
// 单独开一个入口（而不是复用 Patch 的整表替换）：每个库的配置互相独立，
// 整表替换会让"两个窗口各改一个库"变成最后一次写覆盖掉前一次。
func (s *ConfigStore) SetRepoMountPref(repoID string, pref RepoMountPref) (Config, error) {
	repoID = strings.TrimSpace(repoID)
	if repoID == "" {
		return s.Get(), apperr.InvalidParam("repo_id")
	}
	if raw := strings.TrimSpace(pref.MountMode); raw != "" {
		mode := normalizeMountMode(raw)
		if mode == "" {
			return s.Get(), apperr.InvalidParam("mount_mode")
		}
		pref.MountMode = mode
	} else {
		pref.MountMode = ""
	}
	pref.MountDir = strings.TrimSpace(pref.MountDir)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.RepoMounts == nil {
		s.cfg.RepoMounts = make(map[string]RepoMountPref)
	}
	s.cfg.RepoMounts[repoID] = pref
	if err := s.persistLocked(); err != nil {
		s.logger.Warn("写入本地配置失败（仅内存生效）", "path", s.path, "error", err)
		return cloneConfig(s.cfg), err
	}
	return cloneConfig(s.cfg), nil
}

// Patch 部分更新配置并持久化。
//
// 写盘失败不阻断调用（返回新配置与错误），避免因磁盘权限问题让界面配置无法生效。
func (s *ConfigStore) Patch(p ConfigPatch) (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if p.AutoMount != nil {
		s.cfg.AutoMount = *p.AutoMount
	}
	if p.AutoLogin != nil {
		s.cfg.AutoLogin = *p.AutoLogin
		// 用户显式表过态了：此后不再套用"缺省即开启"。
		s.cfg.autoLoginSet = true
	}
	if p.DefaultMountMode != nil {
		mode := normalizeMountMode(*p.DefaultMountMode)
		if mode == "" {
			return s.cfg, apperr.InvalidParam("default_mount_mode")
		}
		s.cfg.DefaultMountMode = mode
	}
	if p.DefaultMountDir != nil {
		s.cfg.DefaultMountDir = strings.TrimSpace(*p.DefaultMountDir)
	}
	if p.DefaultDownloadDir != nil {
		dir, err := validateAbsDir(*p.DefaultDownloadDir, "default_download_dir")
		if err != nil {
			return s.cfg, err
		}
		s.cfg.DefaultDownloadDir = dir
	}
	if p.DownloadConnections != nil {
		if v := *p.DownloadConnections; v < 1 || v > downloadMaxConnections {
			return s.cfg, apperr.InvalidParam("download_connections")
		}
		s.cfg.DownloadConnections = *p.DownloadConnections
	}
	if p.Language != nil {
		s.cfg.Language = strings.TrimSpace(*p.Language)
	}
	if p.StartAtLogin != nil {
		s.cfg.StartAtLogin = *p.StartAtLogin
	}
	if p.UpdateChannel != nil {
		s.cfg.UpdateChannel = strings.TrimSpace(*p.UpdateChannel)
	}
	if p.ServerAlias != nil {
		s.cfg.ServerAlias = strings.TrimSpace(*p.ServerAlias)
	}

	s.cfg = normalizeConfig(s.cfg)
	if err := s.persistLocked(); err != nil {
		s.logger.Warn("写入本地配置失败（仅内存生效）", "path", s.path, "error", err)
		return s.cfg, err
	}
	return s.cfg, nil
}

// persistLocked 写盘。调用方需持有锁。
func (s *ConfigStore) persistLocked() error {
	data, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0o600)
}
