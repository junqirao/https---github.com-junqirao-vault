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
export type RepoState = 'creating' | 'active' | 'sealing' | 'deleting' | 'error'
export type ParentCondition = 'idle' | 'derived' | 'temp_shared' | 'maintenance'
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
  client_config?: Record<string, unknown>
  /** 是否为池化共享库：建库时已把差异盘池建好并发布，分配即用（挂载免等）。 */
  pool: boolean
  /** 建库进度：state=creating 时用于显示"正在派生差异盘 3/5"。 */
  prepare?: RepoPrepareDTO
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

/** GET /v1/system/lvm 的存储池与缓存现状。 */
export interface LvmPoolStatusDTO {
  kind: string
  vg: string
  thin_pool: string
  /** false 表示该 VG 或 thin pool 尚不存在，需要先初始化。 */
  exists: boolean
  size_bytes: number
  free_bytes: number
  /** thin pool 数据/元数据使用率（0–100）。 */
  data_percent: number
  metadata_percent: number
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

/** POST /v1/system/lvm/initialize 的请求体。 */
export interface InitializePoolRequest {
  vg?: string
  thin_pool?: string
  /** 组成 VG 的块设备全路径（VG 已存在时忽略）。 */
  hdd_devices?: string[]
  chunk_size?: string
  metadata_size?: string
  /** 用作 dm-cache 的块设备全路径（空表示不加缓存）。 */
  cache_devices?: string[]
  cache_chunk_size?: string
  cache_policy?: string
  /** writethrough（默认、安全）| writeback。 */
  cache_mode?: string
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

export interface EnrollmentTokenResponse {
  token: string
  expires_at: number
  server_instance_id: string
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
}

export interface ParentActionRequest {
  action: ParentAction
}

export type ParentAction =
  | 'derive'
  | 'temp_share'
  | 'unshare'
  | 'maintenance'
  | 'finish_maintenance'
  | 'cleanup_diffs'

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
