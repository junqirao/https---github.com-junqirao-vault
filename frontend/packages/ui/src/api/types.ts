/**
 * 后端 DTO 类型定义。
 *
 * 严格对齐 internal/api/dto.go、internal/api/system.go、internal/app/*.go 的真实字段
 * （字段名以下划线风格的 JSON 命名为准）。**绝不包含 password_hash / chap_secret / owner_token**。
 */

/** 列表响应统一包裹：{ items: [...] }。 */
export interface ListResponse<T> {
  items: T[]
}

export type Role = 'super_admin' | 'user'
export type Permission = 'read' | 'mount' | 'manage'
export type RepoMode = 'shared' | 'exclusive'
// importing：母盘正在写入（数据已收完、建库任务在建盘），界面显示「写入中」，与 creating
// 一样不可挂载。注意它**不代表数据还在传**：这个库记录要到"数据都收完了、建库任务开跑"才存在。
// 还在传数据的那一帧是界面合成的占位库，标签取值另算（见 features/repo/uploadView.tsx 的
// repoStateValue）。
export type RepoState = 'creating' | 'importing' | 'active' | 'sealing' | 'deleting' | 'error'
export type ParentCondition = 'idle' | 'temp_shared' | 'maintenance'
export type DiskKind = 'parent' | 'diff' | 'standalone'
export type DiskState = 'creating' | 'ready' | 'published' | 'deleting' | 'error'
export type AllocationState = 'allocated' | 'mounting' | 'mounted' | 'releasing' | 'released'
export type LeaseState = 'active' | 'expired' | 'revoked' | 'released'
export type JobState = 'pending' | 'running' | 'succeeded' | 'failed' | 'cancelled'
export type AuditResult = 'ok' | 'denied' | 'error'
export type CertificateStatus = 'active' | 'revoked'

export interface UserDTO {
  id: string
  username: string
  role: Role
  enabled: boolean
  quota_bytes: number
  used_bytes: number
  remark: string
  created_at: number
  updated_at: number
}

export interface CertificateDTO {
  id: string
  user_id: string
  serial: string
  fingerprint: string
  spki_sha256: string
  status: CertificateStatus
  bound_ip: string
  bound_mac: string
  not_before: number
  not_after: number
  created_at: number
}

/** 一次性签发结果（POST /v1/users/{id}/certificates）。 */
export interface IssuedCertificateDTO {
  cert_pem: string
  /** 私钥仅本次返回，服务端不落库；禁止持久化。 */
  key_pem: string
  ca_pem: string
  serial: string
  fingerprint_sha256: string
  spki_sha256: string
  not_before: number
  not_after: number
}

/** 建库预创建的阶段：母盘 → 差异盘 → iSCSI 目标。 */
export type RepoPreparePhase = 'parent' | 'diffs' | 'targets'

/** 建库预创建进度快照（state=creating 期间存在）。 */
export interface RepoPrepareDTO {
  phase: RepoPreparePhase
  /** 当前阶段内已完成的数量。 */
  done: number
  /** 当前阶段需要完成的总数。 */
  total: number
  /** 失败时是**错误码**（如 disk.create_failed），非空表示该库已进入 error。 */
  error?: string
}

export interface RepoDTO {
  id: string
  name: string
  mode: RepoMode
  owner_id: string
  parent_disk_id?: string
  parent_version: number
  parent_condition?: ParentCondition
  /**
   * 共享数量：建库时预创建了几个差异盘（池位），也就是最多能分给几个用户。
   *
   * 与 `max_diff_disks` 永远同值（后者是改名前的字段，见服务端 repoDTO 的说明）。
   */
  share_count: number
  max_diff_disks: number
  quota_bytes: number
  /** 库容量：建库时设定的容量（那块盘的 VHDX 标称容量），服务端读时派生。 */
  capacity_bytes: number
  used_bytes: number
  state: RepoState
  group?: string
  /**
   * 说明备注（自由文本）。
   *
   * 主要用途是迁移：它会写进母盘旁边的元数据文件跟着盘走，换服务端后「导入磁盘」
   * 能一眼看出"这是财务的 Win10 母盘，别删"。
   */
  note?: string
  client_config?: Record<string, unknown>
  /** 是否为池化共享库：建库时已把差异盘池建好并发布，分配即用（挂载免等）。 */
  pool: boolean
  /** 建库进度：state=creating 时用于显示"正在派生差异盘 3/5"。 */
  prepare?: RepoPrepareDTO
  /**
   * **调用者自己**对这个库的有效权限（read | mount | manage）；老服务端不带这个字段。
   *
   * 判据是"我现在能对它做什么"：库的所有者与超管是 manage，成员按被授予的级别。
   * 界面据此决定这个库是"能管理"还是"只能看"——非库所有者点进详情页只应看到只读视图。
   */
  permission?: Permission
  created_at: number
  updated_at: number
}

/**
 * 存储底层卷类型：
 * - thin：系统用 thin pool 新建并格式化的卷（Linux，支持扩容）；
 * - lv：登记进来的已有 LV（只挂载、不格式化、删除时也不删卷）；
 * - dir：纯目录（Windows，或历史存储）。
 */
export type StorageKind = 'thin' | 'lv' | 'dir'

/** 存储：磁盘卷上的一个存储根目录记录（含所在卷的实时容量）。 */
export interface StorageDTO {
  id: string
  name: string
  /** 挂载点（Linux 下即底层卷的挂载目录）。 */
  path: string
  enabled: boolean
  created_at: number
  updated_at: number
  file_system: string
  free_bytes: number
  total_bytes: number
  volume_name: string
  disk_count: number
  used_bytes: number
  /** 底层卷类型；空串表示目录模式（Windows，或历史存储）。 */
  kind: StorageKind | ''
  /** 底层卷是否已挂载就绪；目录模式恒为 true。未挂载的存储不参与落盘。 */
  mounted: boolean
  /** 底层卷设备引用（如 /dev/mapper/vg-lv）；目录模式为空串。 */
  ref: string
  /** 底层卷所属的存储池（`<vg>/<thin_pool>`）；空串 = 服务端默认池 / 目录模式。 */
  pool_ref: string
  /** 分配容量（字节）；目录模式为 0。 */
  size_bytes: number
}

/** 创建存储的请求体（mode 留空时由服务端按平台取默认：Linux=thin，Windows=register_dir）。 */
export interface CreateStorageRequest {
  name: string
  /** 挂载点；目录模式（register_dir）必填，thin 可留空由服务端推导。 */
  mount_point?: string
  enabled?: boolean
  mode?: 'thin' | 'register_dir' | 'register_lv'
  /** 分配容量（字节）；mode=thin 时必填。 */
  size_bytes?: number
  /**
   * 存储池（`<vg>/<thin_pool>`，取自 `GET /v1/system/pools` 的 `items[].key`）。
   *
   * 一台机器可以有多个存储池；留空 = 服务端默认池。仅 mode=thin 使用。
   */
  pool_ref?: string
  /** 存储卷文件系统（ext4 默认 / xfs）。 */
  file_system?: string
  /** 已有 LV 引用；mode=register_lv 时必填。 */
  ref?: string
}

export interface MemberDTO {
  user_id: string
  perm: Permission
}

export interface DiskDTO {
  id: string
  repo_id: string
  kind: DiskKind
  vhdx_path: string
  parent_id?: string
  parent_version: number
  size_bytes: number
  physical_bytes: number
  vhd_type: string
  state: DiskState
  mounted: boolean
  created_at: number
  updated_at: number
}

export interface AllocationDTO {
  id: string
  repo_id: string
  disk_id: string
  user_id: string
  state: AllocationState
  created_at: number
  updated_at: number
}

export interface LeaseDTO {
  id: string
  allocation_id: string
  target_name: string
  user_id: string
  client_id: string
  mount_point: string
  state: LeaseState
  expires_at: number
  last_seen_at: number
  created_at: number
}

export interface JobDTO {
  id: string
  type: string
  ref_id: string
  state: JobState
  progress: number
  attempt: number
  failed: boolean
  created_at: number
  started_at: number
  finished_at: number
  /** 失败/重试时的错误摘要（稳定错误码与参数）；旧服务端不带该字段。 */
  last_error?: string
}

export interface AuditDTO {
  id: string
  user_id: string
  action: string
  resource: string
  detail: string
  ip: string
  result: AuditResult
  created_at: number
}

export interface ClientCompatInfo {
  enabled: boolean
  min?: string
  max?: string
}

/** GET /v1/system/info 的 capabilities.volumes 元素：同一卷上的多个根合并为一条。 */
export interface VolumeCapabilityDTO {
  name: string
  file_system: string
  free_bytes: number
  total_bytes: number
  roots: string[]
}

export interface Capabilities {
  /** 当前平台后端种类：windows | linux（空串表示未装配磁盘后端）。 */
  platform_kind: string
  win_target: boolean
  /** 是否支持真实枚举 iSCSI 会话（Windows 不支持，Linux(LIO) 支持）。 */
  iscsi_sessions: boolean
  /** 是否具备 LVM 存储池管理能力（Windows 恒为 false）。 */
  lvm: boolean
  /** 目标 thin pool 上是否已挂载 dm-cache（lvm 为 false 时恒为 false）。 */
  lvm_cache: boolean
  hyperv: boolean
  /** 第一个存储根目录（向后兼容保留）。 */
  storage_root: string
  /** 全部存储根目录（按配置顺序）。 */
  storage_roots: string[]
  file_system: string
  /** 全部根所在卷按卷去重后的可用空间合计。 */
  volume_free_bytes: number
  /** 全部根所在卷按卷去重后的总空间合计。 */
  volume_total_bytes: number
  /** 按卷去重后的各卷空间明细。 */
  volumes: VolumeCapabilityDTO[]
  database_driver: string
}

/** GET /v1/system/block-devices 的 items 元素：一块可被选作 LVM 物理卷/缓存盘的块设备。 */
export interface BlockDeviceDTO {
  /** 内核短名，如 sdb。 */
  name: string
  /** 设备全路径，如 /dev/sdb。 */
  path: string
  size_bytes: number
  model: string
  /** true=机械盘(HDD)，false=固态(SSD/NVMe)。 */
  rotational: boolean
  /** 承载根文件系统/引导/swap —— 禁止选作缓存盘。 */
  system: boolean
  has_fs: boolean
  has_pv: boolean
  vg_name?: string
  partitions: number
  /** 不可选用时的原因（可直接展示）；可选用时为空。 */
  reason?: string
}

/** 释放设备过程中的一步结果。 */
export interface DeviceReleaseStepDTO {
  /** 步骤标识（umount / swapoff / wipefs / pvremove / mdadm / dmsetup / partprobe / udevadm）。 */
  step: string
  /** 该步作用的设备或挂载点。 */
  target?: string
  /** 执行成功。 */
  ok: boolean
  /** 无需执行（未挂载、不是 swap、没有对应签名、工具缺失等）。 */
  skipped: boolean
  /** 失败原因或补充说明（原始命令输出，可能为空）。 */
  detail?: string
}

/** POST /v1/system/block-devices/release 的响应：一次"释放设备"的逐步结果。 */
export interface DeviceReleaseReportDTO {
  path: string
  steps: DeviceReleaseStepDTO[]
  /** 释放后该设备是否已可被选作卷组设备。 */
  released: boolean
  /** 仍不可用的原因（released 为 true 时为空）。 */
  reason?: string
}

/** GET /v1/system/lvm 的存储池与缓存现状。 */
export interface LvmPoolStatusDTO {
  kind: string
  vg: string
  thin_pool: string
  /** 池的稳定标识（`<vg>/<thin_pool>`）：创建存储时作为 `pool_ref` 传回。 */
  key: string
  /** 是否为服务端配置的默认池（列表置顶 / 预选用）。 */
  default: boolean
  /** false 表示该 VG 或 thin pool 尚不存在，需要先初始化。 */
  exists: boolean
  size_bytes: number
  free_bytes: number
  /** thin pool 数据/元数据使用率（0–100）。 */
  data_percent: number
  metadata_percent: number
  /**
   * 该池上新建存储能设置的最大逻辑容量（字节；0 表示未知）。
   *
   * 即池自身的数据容量，**不是**上面的 `size_bytes`（那是卷组容量，可能比池大得多）：
   * 池是存储的容器，拿卷组容量当上限会把池撑爆。用作"容量"输入框的预填值与 max。
   */
  max_storage_volume_bytes: number
  cache_attached: boolean
  cache_mode?: string
  cache_chunk_size?: string
  cache_policy?: string
  cache_dirty_blocks: number
  cache_read_hits: number
  cache_read_misses: number
  cache_write_hits: number
  cache_write_misses: number
  cache_devices?: string[]
  /** 空表示正常；否则为 Fail / needs_check 等。 */
  cache_health?: string
}

/**
 * 一个卷组（存储池的容量来源）。
 *
 * `GET /v1/system/pools` 返回它，用于"在已有卷组里新建池"或"新建卷组"。
 */
export interface VolumeGroupDTO {
  name: string
  /** 卷组总容量 / 剩余空间（字节）。 */
  size_bytes: number
  free_bytes: number
  /** 物理卷数量。 */
  pv_count: number
  /** 该卷组里已有的 thin pool 名（通常 0 或 1 个）。 */
  thin_pools?: string[]
  /**
   * 在该卷组上**新建** thin pool 时数据容量的上限（字节；<=0 表示不可新建）。
   *
   * 由后端按"剩余空间 − 元数据预留"算好，直接用作容量输入框的 max：
   * 池的元数据要另占卷组空间，"填满剩余空间"必被 LVM 拒绝，口径只能有一处权威计算。
   */
  pool_max_bytes: number
}

/** `GET /v1/system/pools` 的响应：多存储池目录。 */
export interface PoolCatalogDTO {
  /** 现有存储池（每个卷组的 thin pool 一项），默认池在最前。 */
  items: LvmPoolStatusDTO[]
  /** 现有卷组（新建池时可选已有卷组，或新建卷组）。 */
  volume_groups: VolumeGroupDTO[]
  /** 服务端配置的默认池（可能尚未创建）。 */
  default_vg: string
  default_pool: string
}

/** POST /v1/system/pools/estimate 的请求体：新建卷组前估算可建池容量。 */
export interface EstimatePoolSizeRequest {
  /** 打算组成新卷组的块设备全路径（至少一块）。 */
  hdd_devices: string[]
  /** 池元数据大小（如 "4G"）；留空取服务端配置值。 */
  metadata_size?: string
}

/** POST /v1/system/pools/estimate 的响应。 */
export interface EstimatePoolSizeResponse {
  /**
   * 这些设备能建出的 thin pool 数据容量上限（字节；<=0 表示建不出来）。
   *
   * 由后端按与真正建池时**同一套**口径算（已扣掉池元数据 tmeta + pmspare 与卷组开销），
   * 直接用作容量输入框的自动填入值与 max：卷组还不存在时没有 pool_max_bytes，
   * 前端自己乘系数必然与后端校验对不上。
   */
  pool_max_bytes: number
}

/** POST /v1/system/pools（与 /v1/system/lvm/initialize 等价）的请求体。 */
export interface InitializePoolRequest {
  vg?: string
  thin_pool?: string
  /** 组成 VG 的块设备全路径（VG 已存在时忽略）。 */
  hdd_devices?: string[]
  /**
   * thin pool 的**数据容量**（字节）。
   *
   * thin pool 必须给容量，否则 lvcreate 只会报 "No command with matching syntax
   * recognised"。省略（0）表示"占满 VG 当前剩余空间"。
   */
  size_bytes?: number
  chunk_size?: string
  metadata_size?: string
  /** 用作 dm-cache 的块设备全路径（空表示不加缓存）。 */
  cache_devices?: string[]
  cache_chunk_size?: string
  cache_policy?: string
  /** writethrough（默认、安全）| writeback。 */
  cache_mode?: string
}

/** POST /v1/system/pools/delete 的请求体：删除一个存储池（可选连带删除卷组与释放磁盘）。 */
export interface PoolDeleteRequest {
  vg: string
  /** 传空串表示"只删这个空卷组"（此时 remove_volume_group 必须为 true）。 */
  thin_pool: string
  /** 是否一并删除卷组（该卷组上只剩这一个池时才有意义，由服务端校验）。 */
  remove_volume_group: boolean
  /**
   * 是否释放组成卷组的磁盘（执行 pvremove 清掉 PV 标签，让盘重新可选作卷组设备）。
   *
   * 注意**不抹文件系统签名**：盘上旧数据仍在，更深的清理是「释放设备」入口的职责。
   * 且仅在 `remove_volume_group=true` 时生效——卷组没删就没有 PV 可清，服务端会忽略它。
   */
  release_devices: boolean
}

/** POST /v1/system/pools/delete 的响应：一次"删除存储池"的结果与逐步明细。 */
export interface PoolDeleteResult {
  vg: string
  /** 池名；只删卷组时后端 omitempty，该字段不会出现。 */
  thin_pool?: string
  /** 卷组是否被一并删除。 */
  removed_volume_group: boolean
  /** 被释放（已执行 pvremove）的设备全路径；无释放时后端 omitempty。 */
  released_devices?: string[]
  /** 释放设备过程中的逐步结果（与 block-devices/release 同结构）。 */
  steps: DeviceReleaseStepDTO[]
}

export interface SystemInfo {
  server_instance_id: string
  server_name: string
  api_version: number
  server_version: string
  client_compat: ClientCompatInfo
  features: string[]
  capabilities: Capabilities
}

export interface HealthStatus {
  ok: boolean
  checks: Record<string, string>
  failures?: string[]
}

/**
 * GET /v1/system/deps：系统依赖自检（服务端启动时自动修复，修不了的用横幅告知）。
 *
 * 仅 Linux 会返回 supported=true：探测 configfs 挂载、LIO 内核模块、LVM/mkfs 等命令行工具。
 * 接口是只读的——修复只在启动期与 `vault-server doctor` 里做；这里每次重新探测，
 * 因此运维在服务器上补齐缺项后，刷新页面横幅即消失。
 */
export interface SysDepsItem {
  /** 稳定标识，如 configfs_mount / kernel_modules / lvm_tools。 */
  key: string
  title: string
  /** 核心功能必需（缺失即服务端部分功能不可用）。 */
  required: boolean
  /** ok | fixed | missing */
  status: 'ok' | 'fixed' | 'missing'
  detail?: string
  fixed?: string
  /** 人工处置建议（含可直接执行的命令）。 */
  hint?: string
  /**
   * 该项此刻能否由服务端"一键安装"（POST /v1/system/deps/install）。
   *
   * 服务端已把"是否 root、platform.auto_install 开关、该项是否属于可装包的工具类"
   * 一并判定完，前端只看这一个布尔——两边各判一次迟早会出现"按钮可点、一点就失败"。
   * 注意它**不含权限信息**：调用该接口还需超级管理员，界面另需按当前用户角色 gate。
   */
  installable: boolean
}

export interface SysDepsReport {
  /** 当前平台是否支持系统依赖自检（仅 Linux 为 true）。 */
  supported: boolean
  /** 必需项是否全部就绪。 */
  ok: boolean
  checked_at: string
  elapsed_ms: number
  /** 服务端是否以 root 运行（非 root 无法自动修复）。 */
  root: boolean
  repair_enabled: boolean
  install_enabled: boolean
  configfs_root?: string
  items: SysDepsItem[]
  /** 仍缺失的必需项 key（横幅的核心内容）。 */
  missing: string[]
  /** 最近一次自动修复解决的项 key（含启动期）。 */
  fixed: string[]
}

/**
 * GET /v1/system/logs：服务端日志（管理端「服务日志」页，仅超级管理员）。
 *
 * 与本地代理的 GET /agent/log 同形（按天切分、一次回一天的文件尾部），差别在于这里的
 * 日志来自**服务端进程自己**的日志文件。
 */
export interface SystemLog {
  /** 本次返回的日期（YYYY-MM-DD）；请求未指定 day 时即当天。 */
  day: string
  /** 存在日志文件的日期（YYYY-MM-DD，升序），供界面做按时间切分的选择。 */
  days: string[]
  /** 该日期日志文件路径（可能为空）。 */
  path: string
  /** 日志文件末尾的原始文本（JSON Lines，slog 输出）。 */
  text: string
  /** 读取失败时的稳定错误码。 */
  error?: string
}

export type ClientCheckState =
  | 'compatible'
  | 'needs_client_upgrade'
  | 'needs_server_upgrade'
  | 'incompatible_protocol'

export interface ClientCheckResponse {
  state: ClientCheckState
  compatible: boolean
  api_version: number
  server_version: string
  client_compat: ClientCompatInfo
  reason?: string
}

export type BlockedReason =
  | 'already_initialized'
  | 'bootstrap_disabled'
  | 'bootstrap_window_expired'
  | 'bootstrap_blocked'

export interface BootstrapStatus {
  initialized: boolean
  needs_bootstrap: boolean
  has_super_admin: boolean
  has_data: boolean
  bootstrap_enabled: boolean
  super_admin_enabled: boolean
  blocked_reason?: BlockedReason | string
  server_name: string
  server_instance_id: string
  api_version: number
}

export interface LoginResponse {
  token: string
  expires_at: number
  user: UserDTO
}

export interface BootstrapResponse {
  token: string
  expires_at: number
  user: Pick<UserDTO, 'id' | 'username' | 'role'>
}

/** POST /v1/users 的响应：用户字段 + 自动签发的一次性证书（签发失败时缺省）。 */
export interface CreateUserResponse extends UserDTO {
  certificate?: IssuedCertificateDTO
}

export interface CreateRepoResponse {
  repo: RepoDTO
  disk: DiskDTO
}

/** POST /v1/allocations/{id}/mount 的响应（含一次性 CHAP 密钥，禁止落盘）。 */
export interface MountSpec {
  server_instance_id: string
  server_name: string
  target_iqn: string
  portal_address: string
  portal_port: number
  auth_mode: string
  chap_user?: string
  chap_secret?: string
  disk_size_bytes: number
  lease_id: string
  lease_ttl_seconds: number
  heartbeat_seconds: number
  mount_mode: string
  mount_path: string
  post_script?: string
}

export interface HeartbeatResponse {
  lease_ttl_seconds: number
  expires_at: number
}

export interface SettingsResponse {
  server: {
    instance_id: string
    name: string
  }
  client_compat: ClientCompatInfo
  storage: {
    whitelist_root: string
    whitelist_roots: string[]
  }
  overrides?: Record<string, string>
}

/**
 * 全量配置表的一行（GET /v1/system/config）。
 *
 * 目的：让用户看到有哪些可配置项与各自含义；`editable=false` 的项界面置灰并展示
 * `editable_reason`，`sensitive=true` 的项值已脱敏且不允许修改。
 */
export interface ConfigField {
  /** 点分路径，与配置文件缩进路径一致（如 storage.disks_dir）。 */
  key: string
  section: string
  kind: 'string' | 'int' | 'float' | 'bool' | 'duration' | 'list' | 'secret'
  /** 当前生效值（敏感项为 ***）。 */
  value: string
  default: string
  desc: string
  editable: boolean
  editable_reason?: string
  /** 改完需重启进程才生效（保存仍会落盘）。 */
  needs_restart: boolean
  sensitive: boolean
  /** 值来源：env 表示被环境变量覆盖（改文件不生效）。 */
  source: 'env' | 'default' | 'file'
}

export interface ConfigResponse {
  /** 配置文件路径（界面展示"改的是哪个文件"）。 */
  path: string
  /** 保存的副作用提示（写回会丢失注释）。 */
  warning?: string
  /** 本次改动是否包含"需重启才生效"的项。 */
  restart_required: boolean
  fields: ConfigField[]
}

/** 请求体类型。 */
export interface LoginRequest {
  username: string
  password: string
}

export interface BootstrapRequest {
  username: string
  password: string
  server_name?: string
}

export interface ClientCheckRequest {
  api_version: number
  client_version: string
}

export interface CreateUserRequest {
  username: string
  password: string
  role?: Role
  quota_bytes?: number
  remark?: string
}

export interface UpdateUserRequest {
  role?: Role
  quota_bytes?: number
  remark?: string
  enabled?: boolean
  password?: string
}

export interface CreateRepoRequest {
  name: string
  mode: RepoMode
  owner_id?: string
  /** 指定存储；缺省时服务端按所在卷可用空间自动选择。 */
  storage_id?: string
  source_dir?: string
  size_bytes?: number
  max_diff_disks?: number
  quota_bytes?: number
  group?: string
}

export interface UpdateRepoRequest {
  name?: string
  quota_bytes?: number
  max_diff_disks?: number
  group?: string
  /** 说明备注；传空串表示清空。它会随母盘元数据一起迁移。 */
  note?: string
}

export interface ParentActionRequest {
  action: ParentAction
}

export type ParentAction = 'temp_share' | 'unshare' | 'maintenance' | 'finish_maintenance'

/** 服务端可浏览的根目录（GET /v1/fs/roots）。 */
export interface FsRoot {
  name: string
  path: string
  /** 该根当前是否真实存在（首次建盘前尚未创建为 false）。 */
  exists: boolean
}

/** GET /v1/fs/browse 的条目（只含子目录）。 */
export interface FsBrowseItem {
  name: string
  path: string
  has_children: boolean
}

/** GET /v1/fs/browse 的响应（path 不存在时 exists 为 false、items 为空）。 */
export interface FsBrowseResult {
  path: string
  parent?: string
  root: string
  exists: boolean
  items: FsBrowseItem[]
}

/** GET /v1/fs/stat 的响应（目录文件数与总字节数；path 不存在时 exists 为 false 且计数为 0）。 */
export interface FsStat {
  path: string
  exists: boolean
  file_count: number
  total_bytes: number
  truncated?: boolean
}

/** 孤儿文件分类（与后端 app.OrphanKind* 一致）。 */
export type OrphanKind = 'diff' | 'parent' | 'other'

/**
 * 一个未被数据库登记的磁盘文件（GET /v1/system/orphans 的一项）。
 *
 * 这类文件只由对账（reconcile）发现，而它"仅报告"的表现就是一屏 WARN 日志 —— 用户看到
 * 却无处处理，因此才有管理端「孤儿磁盘」页面：扫出来、由人确认后逐个删。
 */
export interface OrphanFileDTO {
  /** 绝对路径。 */
  path: string
  /** 该文件所属的存储根（界面用来分组与展示）。 */
  root: string
  kind: OrphanKind
  /** 文件长度（逻辑大小；VHDX 多为稀疏文件，物理占用可能更小）。 */
  size_bytes: number
  /** 最后修改时间（毫秒）。 */
  modified_at: number
}

/** GET /v1/system/orphans 的响应：一次现扫的结果。 */
export interface OrphanScanDTO {
  /** 扫描时间（毫秒）。 */
  at: number
  roots: string[]
  /** 扫过的 .vhdx 文件数（含已登记的）。 */
  checked: number
  files: OrphanFileDTO[]
  total_bytes: number
  /** 未能扫描的位置说明（平台不支持、根不可访问等），原样展示。 */
  skipped?: string[]
}

/** POST /v1/system/orphans/delete 的响应。 */
export interface OrphanDeleteResultDTO {
  path: string
  /** 释放的字节数。 */
  freed_bytes: number
}

// ---- 导入已有母盘（管理端；仅超级管理员） ----

/** 导入候选的分类（与后端 app.DiskImportKind* 一致）。 */
export type DiskImportKind = 'parent' | 'standalone' | 'diff' | 'other'

/** 不可导入/失败的原因码（与后端 app.DiskImportSkip 与 DiskImportFail 两组常量一致）。 */
export type DiskImportSkipReason =
  | 'already_registered'
  | 'diff_disk'
  | 'path_invalid'
  | 'not_found'
  | 'meta_unreadable'
  | 'capacity_invalid'
  | 'create_failed'

/** 建议容量的来源（与后端 app.DiskImportCapacity* 一致），决定界面对这个数字的信赖程度。 */
export type DiskImportCapacitySource = 'meta' | 'manual' | 'file'

/** sidecar 里记录的"这块盘是怎么来的"。 */
export interface DiskMetaSourceDTO {
  /** empty | dir | import。 */
  kind?: string
  /** 建库时使用的服务端源目录（本机路径，迁移后可能已不存在）。 */
  dir?: string
  /** 建库时设置的库分组标签。 */
  group?: string
  /** 原服务端上的存储库 ID（迁移后新库会得到新 ID，这里保留原值供对照）。 */
  repo_id?: string
  /** 生成它的机器名。 */
  host?: string
  /** 建库时间（毫秒）。 */
  at?: number
}

/**
 * 母盘旁的同名元数据文件（`base.vhdx.meta.json`）的内容。
 *
 * 它是"出厂标签"：真正的源仍是服务端数据库，但盘被拷到另一台机器上时就只剩它了。
 * 因此这里每个字段都是人/别的机器能直接读懂的裸值。
 */
export interface DiskMetaFileDTO {
  schema: number
  app?: string
  /** 存储库名称（迁移后要恢复的首要信息）。 */
  name?: string
  /** 说明备注。 */
  note?: string
  repo_id?: string
  mode?: string
  kind?: string
  /** 该盘面向的客户端操作系统（windows / linux）。 */
  client_os?: string
  /** 盘内分区上的文件系统（ntfs / ext4）。 */
  file_system?: string
  /**
   * 盘的**标称容量**（VHDX VirtualSize）。
   *
   * ⚠️ 导入时最关键的字段：客户端按容量精确匹配本地 iSCSI 盘，差一个字节就找不到盘。
   */
  capacity_bytes?: number
  vhd_type?: string
  parent_version?: number
  max_diff_disks?: number
  source?: DiskMetaSourceDTO
  written_at?: number
  updated_at?: number
}

/** GET /v1/system/disks/import/scan 的一项候选。 */
export interface DiskImportCandidateDTO {
  /** 磁盘文件绝对路径（导入时原样回传）。 */
  path: string
  /** 所属存储根。 */
  root: string
  kind: DiskImportKind
  /** **文件大小**（不是盘的标称容量；VHDX 稀疏，通常小于容量）。 */
  size_bytes: number
  modified_at: number
  /** 元数据文件路径（无论存不存在都给，便于提示"去哪里补元数据"）。 */
  meta_path?: string
  /** 读到的元数据（没有 sidecar 时为 null）。 */
  meta?: DiskMetaFileDTO | null
  /** sidecar 存在但读不出来时的原因。 */
  meta_error?: string
  /** 数据库里已有这条路径（导入时会被跳过）。 */
  registered: boolean
  /** 可导入（未登记 + 不是差异盘 + 元数据可读 + 有容量）。 */
  importable: boolean
  /** 不可导入时的原因码。 */
  skip_reason?: DiskImportSkipReason
  /** 建议的存储库名称（界面预填、可改）。 */
  suggested_name: string
  /** 建议的标称容量。 */
  suggested_capacity_bytes: number
  capacity_source: DiskImportCapacitySource
}

/** GET /v1/system/disks/import/scan 的响应：一次现扫的结果。 */
export interface DiskImportScanDTO {
  at: number
  /** 本次覆盖的存储根。 */
  roots: string[]
  /** 扫过的 .vhdx 文件数（含已登记与不可导入的）。 */
  checked: number
  /** 其中可导入的数量。 */
  importable_count: number
  /** 全部候选（含已登记与差异盘：界面要解释"它为什么被跳过"）。 */
  candidates: DiskImportCandidateDTO[]
  /** 未能扫描的位置说明，原样展示。 */
  skipped?: string[]
}

/** 一条导入请求。 */
export interface DiskImportItemDTO {
  path: string
  /** 存储库名称；留空则用元数据里的名称，再退到父目录名。 */
  name?: string
  /** 说明备注；留空则用元数据里的备注。 */
  note?: string
  /** 标称容量；留空（0）则用元数据里的容量，再退到文件大小。 */
  capacity_bytes?: number
}

/** POST /v1/system/disks/import 的请求体。 */
export interface DiskImportRequestDTO {
  /** 归属用户（迁移后原主人在新机器上不一定存在，由管理员显式指定）。 */
  owner_id: string
  items: DiskImportItemDTO[]
}

/** 一条导入成功的结果。 */
export interface ImportedDiskDTO {
  path: string
  repo_id: string
  name: string
  mode: string
  kind: string
  capacity_bytes: number
  /** 容量是**按文件大小估的**（可能偏小 → 客户端可能匹配不到盘）。 */
  capacity_estimated: boolean
}

/** 一条被跳过的结果。 */
export interface DiskImportSkipDTO {
  path: string
  code: DiskImportSkipReason
}

/** 一条失败的结果。 */
export interface DiskImportFailureDTO {
  path: string
  code: DiskImportSkipReason
  detail?: string
}

/**
 * POST /v1/system/disks/import 的响应。
 *
 * 逐条返回而不是"全成或全败"：迁移时几十块盘里有一块读不出来，不该让其余全部陪葬。
 */
export interface DiskImportResultDTO {
  imported: ImportedDiskDTO[]
  skipped: DiskImportSkipDTO[]
  failed: DiskImportFailureDTO[]
}
