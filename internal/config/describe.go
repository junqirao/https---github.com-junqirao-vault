package config

import (
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// 管理页"全量配置表"的元数据。
//
// 目的（用户诉求）：让用户**知道有哪些可配置项、各自的含义**，并且能直接在页面上改；
// 页面上改不了的项要置灰并说明原因，而不是悄悄隐藏或让人瞎猜。
//
// 设计要点：
//   - Key 与 yaml 的缩进路径一致（如 storage.disks_dir），便于和配置文件对照；
//   - path 是 Config 结构上的字段路径，读写走反射，避免为每个键写一遍 get/set；
//   - Editable=false 的项由 EditableReason 说明"为什么不能在这里改"（界面置灰 + 悬浮提示）；
//   - NeedsRestart=true 表示改完要重启进程才真正生效（保存仍会落盘，供下次启动使用）。

// Kind 配置值的类型：决定管理页的控件形态与字符串解析方式。
type Kind string

const (
	// KindString 字符串。
	KindString Kind = "string"
	// KindInt 整数。
	KindInt Kind = "int"
	// KindFloat 浮点数（如 watermark_percent）。
	KindFloat Kind = "float"
	// KindBool 布尔（含 *bool，nil 表示"未设置 → 取默认"）。
	KindBool Kind = "bool"
	// KindDuration 时长字符串，如 "120s" / "12h"（空串 = 0 = 不限制）。
	KindDuration Kind = "duration"
	// KindList 字符串列表，界面上以逗号分隔录入。
	KindList Kind = "list"
	// KindSecret 敏感值：接口只回脱敏占位符，**绝不接受页面修改**。
	KindSecret Kind = "secret"
)

// maskedValue 是敏感值在接口上的占位符（主密钥 / 含口令的 DSN 绝不下发到浏览器）。
const maskedValue = "***"

// FieldDef 是全量配置表里的一行。
type FieldDef struct {
	// Key 点分路径（与 yaml 缩进路径一致）。
	Key string `json:"key"`
	// Section 所属配置段。
	Section string `json:"section"`
	// Kind 值类型。
	Kind Kind `json:"kind"`
	// Desc 说明（取自 config.example.yaml 的注释，界面直接展示）。
	Desc string `json:"desc"`
	// Editable 是否允许在管理页修改；false 时界面置灰并展示 EditableReason。
	Editable bool `json:"editable"`
	// EditableReason 不可编辑（或不宜在页面编辑）的原因。
	EditableReason string `json:"editable_reason,omitempty"`
	// NeedsRestart 修改后需重启进程才真正生效（保存仍会写回配置文件）。
	NeedsRestart bool `json:"needs_restart"`
	// Sensitive 值只以脱敏形式返回，且不允许页面修改。
	Sensitive bool `json:"sensitive"`
	// Default 默认值（界面用于对比"是否改过"）。
	Default any `json:"default"`

	// path 是 Config 上的字段路径（反射读写用），不对外暴露。
	path []string
}

// DescribeFields 返回**全量**配置项定义（顺序即展示顺序：按段分组）。
func DescribeFields() []FieldDef {
	return []FieldDef{
		// ---- server ----
		{
			Key: "server.instance_id", Section: "server", Kind: KindString, path: []string{"Server", "InstanceID"},
			Desc:           "服务端唯一标识。留空则首次启动生成并写回配置文件。必须跨重启保持稳定：客户端证书里绑定了该值。",
			Editable:       false,
			EditableReason: "换值会导致所有已登记客户端的证书失效，且由服务端首次启动自动生成",
			Default:        "",
		},
		{
			Key: "server.name", Section: "server", Kind: KindString, path: []string{"Server", "Name"},
			Desc:           "展示用别名（管理页与客户端多服务端列表使用）。留空时取主机名。",
			Editable:       false,
			EditableReason: "请在上方\"服务端名称\"处修改：运行期覆盖（存数据库）优先于配置文件，此处改了不会生效",
			Default:        "",
		},

		// ---- http ----
		{
			Key: "http.listen", Section: "http", Kind: KindString, path: []string{"HTTP", "Listen"},
			Desc:         "监听地址。仅本机访问用 127.0.0.1:8443；对外提供服务用 0.0.0.0:8443。",
			Editable:     true,
			NeedsRestart: true,
			Default:      "127.0.0.1:8443",
		},
		{
			Key: "http.tls.enabled", Section: "http", Kind: KindBool, path: []string{"HTTP", "TLS", "Enabled"},
			Desc:         "是否启用 HTTPS。生产环境务必保持 true：口令登录与 CHAP 密钥下发都依赖传输加密。",
			Editable:     true,
			NeedsRestart: true,
			Default:      false,
		},
		{
			Key: "http.tls.cert_file", Section: "http", Kind: KindString, path: []string{"HTTP", "TLS", "CertFile"},
			Desc:         "证书导出副本的位置（相对配置文件所在目录解析），留空则不导出；不影响 HTTPS 工作。",
			Editable:     true,
			NeedsRestart: true,
			Default:      "pki/server.crt",
		},
		{
			Key: "http.tls.key_file", Section: "http", Kind: KindString, path: []string{"HTTP", "TLS", "KeyFile"},
			Desc:         "私钥导出副本的位置（相对配置文件所在目录解析），留空则不导出。",
			Editable:     true,
			NeedsRestart: true,
			Default:      "pki/server.key",
		},

		// ---- database ----
		{
			Key: "database.driver", Section: "database", Kind: KindString, path: []string{"Database", "Driver"},
			Desc:         "数据库驱动：sqlite | mysql。",
			Editable:     true,
			NeedsRestart: true,
			Default:      "sqlite",
		},
		{
			Key: "database.sqlite.path", Section: "database", Kind: KindString, path: []string{"Database", "SQLite", "Path"},
			Desc:         "SQLite 数据库文件路径（必须位于本地磁盘，不允许 UNC/映射网络驱动器）。",
			Editable:     true,
			NeedsRestart: true,
			Default:      "data/vault.db",
		},
		{
			Key: "database.mysql.dsn", Section: "database", Kind: KindSecret, path: []string{"Database", "MySQL", "DSN"},
			Desc:           "MySQL 连接串（driver=mysql 时必填），形如 user:pass@tcp(127.0.0.1:3306)/vault?...",
			Editable:       false,
			EditableReason: "连接串含数据库口令：请直接编辑配置文件，不在页面上展示或修改",
			Sensitive:      true,
			Default:        "",
		},
		{
			Key: "database.mysql.max_open_conns", Section: "database", Kind: KindInt, path: []string{"Database", "MySQL", "MaxOpenConns"},
			Desc:         "MySQL 最大打开连接数。",
			Editable:     true,
			NeedsRestart: true,
			Default:      25,
		},
		{
			Key: "database.mysql.max_idle_conns", Section: "database", Kind: KindInt, path: []string{"Database", "MySQL", "MaxIdleConns"},
			Desc:         "MySQL 最大空闲连接数。",
			Editable:     true,
			NeedsRestart: true,
			Default:      5,
		},

		// ---- storage ----
		{
			Key: "storage.whitelist_root", Section: "storage", Kind: KindString, path: []string{"Storage", "WhitelistRoot"},
			Desc:           "「存储」种子（单根旧写法）。仅作为首次启动的种子写进 storages 表。",
			Editable:       false,
			EditableReason: "存储的唯一真源是数据库（storages 表）：请在管理端「存储」模块维护，这里改了不会生效",
			Default:        "",
		},
		{
			Key: "storage.whitelist_roots", Section: "storage", Kind: KindList, path: []string{"Storage", "WhitelistRoots"},
			Desc:           "「存储」种子（多根），非空时优先于 whitelist_root。同样只是首次启动的种子。",
			Editable:       false,
			EditableReason: "存储的唯一真源是数据库（storages 表）：请在管理端「存储」模块维护",
			Default:        "",
		},
		{
			Key: "storage.disks_dir", Section: "storage", Kind: KindString, path: []string{"Storage", "DisksDir"},
			Desc:     "磁盘目录（相对各个存储根的子目录）。",
			Editable: true,
			Default:  "disks",
		},
		{
			Key: "storage.staging_dir", Section: "storage", Kind: KindString, path: []string{"Storage", "StagingDir"},
			Desc:     "分块上传暂存目录（相对各个存储根）。必须与 disks_dir 同卷，否则无法原子改名。",
			Editable: true,
			Default:  "staging",
		},
		{
			Key: "storage.trash_dir", Section: "storage", Kind: KindString, path: []string{"Storage", "TrashDir"},
			Desc:     "软删除暂存目录（相对各个存储根）。",
			Editable: true,
			Default:  "meta/trash",
		},
		{
			Key: "storage.orphan_dir", Section: "storage", Kind: KindString, path: []string{"Storage", "OrphanDir"},
			Desc:     "孤儿文件暂存目录（相对各个存储根）。",
			Editable: true,
			Default:  "meta/orphan",
		},
		{
			Key: "storage.source_roots", Section: "storage", Kind: KindList, path: []string{"Storage", "SourceRoots"},
			Desc:     "允许被浏览（/v1/fs/*）与作为\"源目录\"建库的服务端本地目录白名单；留空则回退为当前生效的存储根。",
			Editable: true,
			Default:  "",
		},
		{
			Key: "storage.require_same_volume", Section: "storage", Kind: KindBool, path: []string{"Storage", "RequireSameVolume"},
			Desc:     "是否强制校验 disks_dir 与 staging_dir 同卷（默认 true，建议保持）。",
			Editable: true,
			Default:  true,
		},
		{
			Key: "storage.default_max_diff_disks", Section: "storage", Kind: KindInt, path: []string{"Storage", "DefaultMaxDiffDisks"},
			Desc:     "新建共享库时单个母盘允许的差异盘默认上限（0 或负数表示不限制）。",
			Editable: true,
			Default:  50,
		},
		{
			Key: "storage.size_granularity_mb", Section: "storage", Kind: KindInt, path: []string{"Storage", "SizeGranularityMB"},
			Desc:     "VHDX 容量向上取整粒度（MB）。按 MB 取整可避免空目录/小目录也占 1GB 配额。",
			Editable: true,
			Default:  1,
		},
		{
			Key: "storage.size_reserve_permille", Section: "storage", Kind: KindInt, path: []string{"Storage", "SizeReservePermille"},
			Desc:     "容量预留千分比（文件系统元数据 + 碎片）。100 = 10%。",
			Editable: true,
			Default:  100,
		},
		{
			Key: "storage.min_volume_free_permille", Section: "storage", Kind: KindInt, path: []string{"Storage", "MinVolumeFreePermille"},
			Desc:     "卷可用空间低于该千分比时禁止新建存储库/分配。150 = 15%。",
			Editable: true,
			Default:  150,
		},
		{
			Key: "storage.trash_retention_days", Section: "storage", Kind: KindInt, path: []string{"Storage", "TrashRetentionDays"},
			Desc:     "软删除保留天数（到期由后台任务清理）。",
			Editable: true,
			Default:  7,
		},
		{
			Key: "storage.size_granularity_gb", Section: "storage", Kind: KindInt, path: []string{"Storage", "SizeGranularityGB"},
			Desc:           "**已废弃**：原 GB 粒度，会让空目录也占 1GB 配额。保留仅为兼容旧配置文件，不参与任何计算。",
			Editable:       false,
			EditableReason: "已废弃字段（粒度改由 size_granularity_mb 决定）",
			Default:        0,
		},

		// ---- platform ----
		{
			Key: "platform.kind", Section: "platform", Kind: KindString, path: []string{"Platform", "Kind"},
			Desc:           "平台种类：auto（按运行 OS）| windows | linux。",
			Editable:       false,
			EditableReason: "由构建产物决定，本字段只作显式声明与一致性校验",
			Default:        "auto",
		},
		{
			Key: "platform.lvm.vg", Section: "platform", Kind: KindString, path: []string{"Platform", "LVM", "VG"},
			Desc:     "目标卷组名（仅 Linux）。不得含 '-'（device-mapper 会转义，导致磁盘引用不可逆）。",
			Editable: true,
			Default:  "vg0",
		},
		{
			Key: "platform.lvm.thin_pool", Section: "platform", Kind: KindString, path: []string{"Platform", "LVM", "ThinPool"},
			Desc:     "目标 thin pool 名（仅 Linux），限制同上。",
			Editable: true,
			Default:  "vault",
		},
		{
			Key: "platform.lvm.chunk_size", Section: "platform", Kind: KindString, path: []string{"Platform", "LVM", "ChunkSize"},
			Desc:     "thin pool chunk 大小（建池时使用；池已存在则忽略）。",
			Editable: true,
			Default:  "256K",
		},
		{
			Key: "platform.lvm.metadata_size", Section: "platform", Kind: KindString, path: []string{"Platform", "LVM", "MetadataSize"},
			Desc:     "thin pool 元数据大小（建池时使用）。",
			Editable: true,
			Default:  "4G",
		},
		{
			Key: "platform.lvm.autoextend_threshold", Section: "platform", Kind: KindInt, path: []string{"Platform", "LVM", "AutoextendThreshold"},
			Desc:     "thin pool 使用率告警阈值（%），仅用于校验与告警；真正的自动扩容由宿主机 lvm2-monitor 执行。",
			Editable: true,
			Default:  80,
		},
		{
			Key: "platform.lvm.autoextend_percent", Section: "platform", Kind: KindInt, path: []string{"Platform", "LVM", "AutoextendPercent"},
			Desc:     "自动扩容比例（%），同上仅用于告警。",
			Editable: true,
			Default:  20,
		},
		{
			Key: "platform.lvm.watermark_percent", Section: "platform", Kind: KindFloat, path: []string{"Platform", "LVM", "WatermarkPercent"},
			Desc:     "应用层空间闸门：thin pool 数据/元数据使用率达到该百分比即拒绝新建磁盘。",
			Editable: true,
			Default:  90,
		},
		{
			Key: "platform.iscsi.backend", Section: "platform", Kind: KindString, path: []string{"Platform", "Iscsi", "Backend"},
			Desc:           "iSCSI 后端实现名（Linux 目前只有 lio）。",
			Editable:       false,
			EditableReason: "后端由平台构建决定，不支持在线切换",
			Default:        "lio",
		},
		{
			Key: "platform.iscsi.configfs_root", Section: "platform", Kind: KindString, path: []string{"Platform", "Iscsi", "ConfigFSRoot"},
			Desc:           "LIO 在 configfs 中的挂载点（仅 Linux）。",
			Editable:       false,
			EditableReason: "平台内部路径，由系统挂载点决定",
			Default:        "/sys/kernel/config/target",
		},
		{
			Key: "platform.iscsi.portals", Section: "platform", Kind: KindList, path: []string{"Platform", "Iscsi", "Portals"},
			Desc:     "下发给客户端的 iSCSI 门户地址，**第一条生效**，格式 <host>[:port]，端口缺省 3260。多网卡服务器请填具体地址。",
			Editable: true,
			Default:  "0.0.0.0:3260",
		},
		{
			Key: "platform.iscsi.iqn_prefix", Section: "platform", Kind: KindString, path: []string{"Platform", "Iscsi", "IQNPrefix"},
			Desc:           "目标 IQN 前缀：完整 IQN = <前缀>:<目标名>。",
			Editable:       false,
			EditableReason: "改动会让已发布的目标 IQN 全部失配（需先卸载所有挂载）",
			Default:        "iqn.2026-01.com.vault",
		},

		// ---- client_compat ----
		{
			Key: "client_compat.enabled", Section: "client_compat", Kind: KindBool, path: []string{"ClientCompat", "Enabled"},
			Desc:     "是否启用客户端版本区间检查（关闭后仅强校验协议版本）。",
			Editable: true,
			Default:  true,
		},
		{
			Key: "client_compat.min", Section: "client_compat", Kind: KindString, path: []string{"ClientCompat", "Min"},
			Desc:     "最低支持的客户端版本（含）。缺失 → 按服务端版本推导；显式留空 → 不设下限。",
			Editable: true,
			Default:  "",
		},
		{
			Key: "client_compat.max", Section: "client_compat", Kind: KindString, path: []string{"ClientCompat", "Max"},
			Desc:     "最高支持的客户端版本（含，支持 1.9.x 通配）。缺失 → 推导；显式留空 → 不设上限。",
			Editable: true,
			Default:  "",
		},

		// ---- log ----
		{
			Key: "log.level", Section: "log", Kind: KindString, path: []string{"Log", "Level"},
			Desc:         "日志级别：debug | info | warn | error。",
			Editable:     true,
			NeedsRestart: true,
			Default:      "info",
		},
		{
			Key: "log.dir", Section: "log", Kind: KindString, path: []string{"Log", "Dir"},
			Desc:         "日志目录；留空则只输出到 stdout。",
			Editable:     true,
			NeedsRestart: true,
			Default:      "",
		},
		{
			Key: "log.keep_days", Section: "log", Kind: KindInt, path: []string{"Log", "KeepDays"},
			Desc:         "日志保留天数（<=0 表示不清理）。",
			Editable:     true,
			NeedsRestart: true,
			Default:      30,
		},
		{
			Key: "log.also_console", Section: "log", Kind: KindBool, path: []string{"Log", "AlsoConsole"},
			Desc:         "是否同时输出到控制台。",
			Editable:     true,
			NeedsRestart: true,
			Default:      false,
		},

		// ---- security ----
		{
			Key: "security.master_key", Section: "security", Kind: KindSecret, path: []string{"Security", "MasterKey"},
			Desc:           "数据保险箱根密钥（AES-256-GCM，用于加密 CHAP 密钥等敏感字段）。留空则首次启动生成并写回。",
			Editable:       false,
			EditableReason: "丢失该密钥将无法解密已落库的 CHAP 密钥：只能由服务端生成并保存在配置文件里",
			Sensitive:      true,
			Default:        "",
		},
		{
			Key: "security.bootstrap_enabled", Section: "security", Kind: KindBool, path: []string{"Security", "BootstrapEnabled"},
			Desc:     "是否允许通过初始化接口创建首个超级管理员（仅在系统尚未初始化时有意义）。",
			Editable: true,
			Default:  true,
		},
		{
			Key: "security.bootstrap_window", Section: "security", Kind: KindDuration, path: []string{"Security", "BootstrapWindow"},
			Desc:     "首次启动后允许初始化的时间窗口（如 30m）。留空或 0s 表示不限制。",
			Editable: true,
			Default:  "0s",
		},
		{
			Key: "security.super_admin_enabled", Section: "security", Kind: KindBool, path: []string{"Security", "SuperAdminEnabled"},
			Desc:     "内置超级管理员角色是否允许登录（关闭后该账号立即无法登录，账号数据保留）。",
			Editable: true,
			Default:  true,
		},
		{
			Key: "security.session_ttl", Section: "security", Kind: KindDuration, path: []string{"Security", "SessionTTL"},
			Desc:         "管理页会话有效期。",
			Editable:     true,
			NeedsRestart: true,
			Default:      "12h",
		},
		{
			Key: "security.login_max_failures", Section: "security", Kind: KindInt, path: []string{"Security", "LoginMaxFailures"},
			Desc:     "防爆破：窗口内失败次数达到阈值即锁定。",
			Editable: true,
			Default:  5,
		},
		{
			Key: "security.login_lock_window", Section: "security", Kind: KindDuration, path: []string{"Security", "LoginLockWindow"},
			Desc:     "防爆破：统计失败次数的窗口。",
			Editable: true,
			Default:  "15m",
		},
		{
			Key: "security.login_lock", Section: "security", Kind: KindDuration, path: []string{"Security", "LoginLock"},
			Desc:     "防爆破：触发阈值后的锁定时长。",
			Editable: true,
			Default:  "15m",
		},
		{
			Key: "security.lease_ttl", Section: "security", Kind: KindDuration, path: []string{"Security", "LeaseTTL"},
			Desc:     "客户端租约有效期（超时未心跳即判定离线）。",
			Editable: true,
			Default:  "120s",
		},
		{
			Key: "security.lease_heartbeat_interval", Section: "security", Kind: KindDuration, path: []string{"Security", "LeaseHeartbeatInterval"},
			Desc:     "下发给客户端的心跳间隔。",
			Editable: true,
			Default:  "30s",
		},
		{
			Key: "security.revoke_cooldown", Section: "security", Kind: KindDuration, path: []string{"Security", "RevokeCooldown"},
			Desc:     "踢下线后的重连抑制窗口。",
			Editable: true,
			Default:  "5m",
		},
		{
			Key: "security.enrollment_token_ttl", Section: "security", Kind: KindDuration, path: []string{"Security", "EnrollmentTokenTTL"},
			Desc:         "一次性注册令牌（enrollment token）有效期。",
			Editable:     true,
			NeedsRestart: true,
			Default:      "15m",
		},

		// ---- update ----
		{
			Key: "update.artifacts_dir", Section: "update", Kind: KindString, path: []string{"Update", "ArtifactsDir"},
			Desc:     "更新包（发布目录内容）存放目录；留空 = 不提供更新分发。相对路径按配置文件所在目录解析。",
			Editable: true,
			Default:  "",
		},
		{
			Key: "update.channel", Section: "update", Kind: KindString, path: []string{"Update", "Channel"},
			Desc:     "默认分发通道，必须与 manifest.json 里的 channel 一致。",
			Editable: true,
			Default:  "stable",
		},
		{
			Key: "update.min_client_version", Section: "update", Kind: KindString, path: []string{"Update", "MinClientVersion"},
			Desc:     "低于此版本的客户端将被强制升级（留空表示不强制）。",
			Editable: true,
			Default:  "",
		},
	}
}

// envVarOf 返回能覆盖该配置项的环境变量名（无则空），与 applyEnvOverrides 的映射一致。
func envVarOf(key string) string {
	switch key {
	case "server.name":
		return "VAULT_SERVER_NAME"
	case "http.listen":
		return "VAULT_HTTP_LISTEN"
	case "database.driver":
		return "VAULT_DB_DRIVER"
	case "database.sqlite.path":
		return "VAULT_DB_SQLITE_PATH"
	case "database.mysql.dsn":
		return "VAULT_DB_MYSQL_DSN"
	case "storage.whitelist_root":
		return "VAULT_STORAGE_ROOT"
	case "storage.whitelist_roots":
		return "VAULT_STORAGE_ROOTS"
	case "log.level":
		return "VAULT_LOG_LEVEL"
	case "log.dir":
		return "VAULT_LOG_DIR"
	case "security.master_key":
		return "VAULT_MASTER_KEY"
	default:
		return ""
	}
}

// Source 返回当前值的来源：env（环境变量覆盖）/ default（未改过）/ file（配置文件）。
//
// 为什么必须区分：被环境变量覆盖的项**改文件不会生效**。界面若不说清楚，运维会一直困惑
// "为什么保存了没变化" —— 这类项在页面上应提示去改环境变量。
func (d FieldDef) Source(c *Config) string {
	if name := envVarOf(d.Key); name != "" && strings.TrimSpace(os.Getenv(name)) != "" {
		return "env"
	}
	if d.String(c) == formatValue(d.Kind, d.Default) {
		return "default"
	}
	return "file"
}

// FindField 按键查找配置项定义。
func FindField(key string) (FieldDef, bool) {
	for _, def := range DescribeFields() {
		if def.Key == key {
			return def, true
		}
	}
	return FieldDef{}, false
}

// Get 读取该配置项在当前配置里的值（指针字段为 nil 时返回 nil）。
func (d FieldDef) Get(c *Config) any {
	if c == nil {
		return nil
	}
	v, ok := locate(c, d.path)
	if !ok {
		return nil
	}
	if v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return nil
		}
		return v.Elem().Interface()
	}
	return v.Interface()
}

// String 返回该配置项的可展示值（敏感项返回脱敏占位符）。
func (d FieldDef) String(c *Config) string {
	if d.Sensitive {
		v := d.Get(c)
		if v == nil {
			return ""
		}
		if s, ok := v.(string); ok && strings.TrimSpace(s) == "" {
			return ""
		}
		return maskedValue
	}
	return formatValue(d.Kind, d.Get(c))
}

// DefaultString 返回默认值的可展示形式（界面用于对比"是否改过"）。
func (d FieldDef) DefaultString() string {
	return formatValue(d.Kind, d.Default)
}

// Set 把字符串写入该配置项（按 Kind 解析；敏感项一律拒绝）。
func (d FieldDef) Set(c *Config, raw string) error {
	if d.Sensitive {
		return fmt.Errorf("配置项 %s 为敏感项，不允许在线修改", d.Key)
	}
	if c == nil {
		return fmt.Errorf("配置为空")
	}
	v, ok := locate(c, d.path)
	if !ok {
		return fmt.Errorf("配置项 %s 未绑定结构字段", d.Key)
	}
	raw = strings.TrimSpace(raw)
	switch d.Kind {
	case KindString:
		return setString(v, raw)
	case KindInt:
		if raw == "" {
			return setInt(v, 0)
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return fmt.Errorf("配置项 %s 需要整数，实际 %q", d.Key, raw)
		}
		return setInt(v, n)
	case KindFloat:
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("配置项 %s 需要小数，实际 %q", d.Key, raw)
		}
		if v.Kind() != reflect.Float64 && v.Kind() != reflect.Float32 {
			return fmt.Errorf("配置项 %s 的类型不是浮点", d.Key)
		}
		v.SetFloat(f)
		return nil
	case KindBool:
		b, err := parseBool(raw)
		if err != nil {
			return fmt.Errorf("配置项 %s 需要布尔值（true/false），实际 %q", d.Key, raw)
		}
		return setBool(v, b)
	case KindDuration:
		if raw == "" {
			return setInt(v, 0)
		}
		dur, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("配置项 %s 需要时长（如 120s/12h），实际 %q", d.Key, raw)
		}
		return setInt(v, int64(dur))
	case KindList:
		if v.Kind() != reflect.Slice {
			return fmt.Errorf("配置项 %s 的类型不是列表", d.Key)
		}
		v.Set(reflect.ValueOf(splitList(raw)))
		return nil
	default:
		return fmt.Errorf("配置项 %s 的类型 %s 不支持修改", d.Key, d.Kind)
	}
}

// locate 按字段路径定位到可写的反射值。
func locate(c *Config, path []string) (reflect.Value, bool) {
	v := reflect.ValueOf(c).Elem()
	for _, name := range path {
		if !v.IsValid() {
			return reflect.Value{}, false
		}
		if v.Kind() == reflect.Ptr {
			if v.IsNil() {
				return reflect.Value{}, false
			}
			v = v.Elem()
		}
		if v.Kind() != reflect.Struct {
			return reflect.Value{}, false
		}
		v = v.FieldByName(name)
	}
	return v, v.IsValid() && v.CanSet()
}

func setString(v reflect.Value, raw string) error {
	if v.Kind() == reflect.Ptr {
		if v.Type().Elem().Kind() != reflect.String {
			return fmt.Errorf("字段不是字符串指针")
		}
		p := reflect.New(v.Type().Elem())
		p.Elem().SetString(raw)
		v.Set(p)
		return nil
	}
	if v.Kind() != reflect.String {
		return fmt.Errorf("字段不是字符串")
	}
	v.SetString(raw)
	return nil
}

func setBool(v reflect.Value, b bool) error {
	if v.Kind() == reflect.Ptr {
		if v.Type().Elem().Kind() != reflect.Bool {
			return fmt.Errorf("字段不是布尔指针")
		}
		p := reflect.New(v.Type().Elem())
		p.Elem().SetBool(b)
		v.Set(p)
		return nil
	}
	if v.Kind() != reflect.Bool {
		return fmt.Errorf("字段不是布尔")
	}
	v.SetBool(b)
	return nil
}

func setInt(v reflect.Value, n int64) error {
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(n)
		return nil
	default:
		return fmt.Errorf("字段不是整型")
	}
}

// parseBool 兼容常见写法（true/false/1/0/yes/no/on/off，大小写不敏感）。
func parseBool(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1", "yes", "on":
		return true, nil
	case "false", "0", "no", "off", "":
		return false, nil
	default:
		return false, fmt.Errorf("无法解析为布尔值")
	}
}

// splitList 把逗号分隔的字符串切成列表（空串 → 空列表）。
//
// 同时兼容中文逗号：运维从文档里复制时很容易带上。
func splitList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []string{}
	}
	raw = strings.ReplaceAll(raw, "，", ",")
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if item := strings.TrimSpace(part); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// formatValue 把值渲染成可展示（且可回写）的字符串。
func formatValue(kind Kind, value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	case Duration:
		return v.Std().String()
	case []string:
		return strings.Join(v, ", ")
	default:
		return fmt.Sprintf("%v", v)
	}
}
