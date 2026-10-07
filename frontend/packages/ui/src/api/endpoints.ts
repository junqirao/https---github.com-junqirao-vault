import { ApiClient } from './client'
import type {
  AllocationDTO,
  AuditDTO,
  BlockDeviceDTO,
  BootstrapRequest,
  BootstrapResponse,
  BootstrapStatus,
  CertificateDTO,
  ClientCheckRequest,
  ClientCheckResponse,
  CreateRepoRequest,
  CreateRepoResponse,
  CreateStorageRequest,
  CreateUserRequest,
  CreateUserResponse,
  DeviceReleaseReportDTO,
  DiskDTO,
  EnrollmentTokenResponse,
  EstimatePoolSizeRequest,
  EstimatePoolSizeResponse,
  FsBrowseResult,
  FsRoot,
  FsStat,
  HealthStatus,
  HeartbeatResponse,
  InitializePoolRequest,
  IssuedCertificateDTO,
  JobDTO,
  JobState,
  LeaseDTO,
  ListResponse,
  LoginRequest,
  LoginResponse,
  LvmPoolStatusDTO,
  MemberDTO,
  PoolCatalogDTO,
  MountSpec,
  OrphanDeleteResultDTO,
  OrphanScanDTO,
  ParentAction,
  PoolDeleteRequest,
  PoolDeleteResult,
  RepoDTO,
  ConfigResponse,
  SettingsResponse,
  StorageDTO,
  SysDepsReport,
  SystemInfo,
  SystemLog,
  UpdateRepoRequest,
  UpdateUserRequest,
  UserDTO
} from './types'

/**
 * Vault REST 客户端。
 *
 * 路径与 internal/api/router.go 的真实路由一一对应；
 * 文档中未实现的路由（如 /v1/auth/mtls、/v1/repos/{id}/stats、/v1/uploads/*）刻意不封装。
 */
export class VaultApi extends ApiClient {
  // ---- 系统（匿名） ----

  systemInfo(): Promise<SystemInfo> {
    return this.request<SystemInfo>('GET', '/v1/system/info')
  }

  systemHealth(): Promise<HealthStatus> {
    return this.request<HealthStatus>('GET', '/v1/system/health')
  }

  /**
   * 系统依赖自检（需登录）：服务端启动时会自动修复能修的，修不了的在这里返回，
   * 由前端横幅逐条提示"缺了什么、怎么补"。
   */
  systemDeps(): Promise<SysDepsReport> {
    return this.request<SysDepsReport>('GET', '/v1/system/deps')
  }

  /**
   * 按需安装系统依赖（仅超级管理员）。
   *
   * 立刻返回 202 + 任务对象，装包在服务端后台跑（同步等待会顶到 HTTP 超时）：
   * 调用方拿到 job.id 后轮询 getJob() 直到终态，再刷新 systemDeps()。
   */
  installSystemDeps(key: string): Promise<JobDTO> {
    return this.request<JobDTO>('POST', '/v1/system/deps/install', { key })
  }

  clientCheck(body: ClientCheckRequest): Promise<ClientCheckResponse> {
    return this.request<ClientCheckResponse>('POST', '/v1/system/client-check', body)
  }

  bootstrapStatus(): Promise<BootstrapStatus> {
    return this.request<BootstrapStatus>('GET', '/v1/system/bootstrap')
  }

  bootstrap(body: BootstrapRequest): Promise<BootstrapResponse> {
    return this.request<BootstrapResponse>('POST', '/v1/system/bootstrap', body)
  }

  // ---- 认证 ----

  login(body: LoginRequest): Promise<LoginResponse> {
    return this.request<LoginResponse>('POST', '/v1/auth/login', body)
  }

  logout(): Promise<void> {
    return this.request<void>('POST', '/v1/auth/logout')
  }

  /**
   * 当前登录用户的完整信息（含配额与已用）。
   *
   * 配额是**用户级**的（多个库共享一份），登录响应的 user 只在登录那一刻准确，
   * 「我的存储库」要显示实时用量，所以单独走一个可重复拉取的接口。
   */
  me(): Promise<UserDTO> {
    return this.request<UserDTO>('GET', '/v1/auth/me')
  }

  // ---- 系统（管理员） ----

  settings(): Promise<SettingsResponse> {
    return this.request<SettingsResponse>('GET', '/v1/system/settings')
  }

  patchSettings(body: { server_name?: string; settings?: Record<string, string> }): Promise<SettingsResponse> {
    return this.request<SettingsResponse>('PATCH', '/v1/system/settings', body)
  }

  /** 全量配置表（读）：所有配置项 + 说明 + 是否可编辑。
   * （不叫 config()：基类的 config() 是 API 客户端自身的配置。） */
  serverConfig(): Promise<ConfigResponse> {
    return this.request<ConfigResponse>('GET', '/v1/system/config')
  }

  /** 在线改配置（写回 config.yaml）：只接受 editable 的键。 */
  patchConfig(body: { settings: Record<string, string> }): Promise<ConfigResponse> {
    return this.request<ConfigResponse>('PATCH', '/v1/system/config', body)
  }

  auditLogs(params?: { user_id?: string; action?: string; limit?: number; offset?: number }): Promise<ListResponse<AuditDTO>> {
    return this.request<ListResponse<AuditDTO>>('GET', '/v1/system/audit', undefined, params)
  }

  /**
   * 服务端日志（管理端「服务日志」页，仅超级管理员）。
   *
   * 日志按天切分：`day` 指定日期（省略即当天），`tailBytes` 限制读取的文件末尾字节数
   * （省略由服务端取默认 256KiB）。注意服务端的查询参数名是 `tail`。
   */
  systemLogs(params?: { day?: string; tailBytes?: number }): Promise<SystemLog> {
    return this.request<SystemLog>('GET', '/v1/system/logs', undefined, {
      day: params?.day,
      tail: params?.tailBytes
    })
  }

  /** 现扫一遍孤儿磁盘文件（未登记却真实存在的 .vhdx；仅超级管理员）。 */
  scanOrphans(): Promise<OrphanScanDTO> {
    return this.request<OrphanScanDTO>('GET', '/v1/system/orphans')
  }

  /**
   * 删除一个孤儿磁盘文件（仅超级管理员；不可逆，服务端会在删除前重新校验它仍未被登记）。
   */
  deleteOrphan(path: string): Promise<OrphanDeleteResultDTO> {
    return this.request<OrphanDeleteResultDTO>('POST', '/v1/system/orphans/delete', { path })
  }

  // ---- 平台存储池（LVM thin pool / dm-cache；仅超级管理员，Windows 返回 501） ----

  lvmStatus(): Promise<LvmPoolStatusDTO> {
    return this.request<LvmPoolStatusDTO>('GET', '/v1/system/lvm')
  }

  /** 列出**全部**存储池与卷组（多存储池：创建存储时"选择或新建"用）。 */
  listPools(): Promise<PoolCatalogDTO> {
    return this.request<PoolCatalogDTO>('GET', '/v1/system/pools')
  }

  listBlockDevices(): Promise<ListResponse<BlockDeviceDTO>> {
    return this.request<ListResponse<BlockDeviceDTO>>('GET', '/v1/system/block-devices')
  }

  /**
   * 释放一块设备（卸载 → 关 swap → 抹签名 → 移出卷组 → 停 md/dm → 重扫）。
   *
   * 破坏性操作：调用前必须由用户显式确认。返回逐步结果，已完成的步骤不会回滚。
   */
  releaseBlockDevice(path: string): Promise<DeviceReleaseReportDTO> {
    return this.request<DeviceReleaseReportDTO>('POST', '/v1/system/block-devices/release', { path })
  }

  /** 新建一个存储池（幂等：同名池已存在时跳过）。 */
  createPool(body: InitializePoolRequest): Promise<LvmPoolStatusDTO> {
    return this.request<LvmPoolStatusDTO>('POST', '/v1/system/pools', body)
  }

  /**
   * 删除一个存储池（可选连带删除卷组并释放磁盘）。
   *
   * 破坏性操作：调用前必须由用户显式确认。返回逐步结果，已完成的步骤不会回滚。
   * `thin_pool` 传空串表示"只删这个空卷组"（此时 `remove_volume_group` 必须为 true）。
   */
  deletePool(body: PoolDeleteRequest): Promise<PoolDeleteResult> {
    return this.request<PoolDeleteResult>('POST', '/v1/system/pools/delete', body)
  }

  /**
   * 估算"用这些设备新建卷组后能建多大的 thin pool"。
   *
   * 只用于"新建卷组"这条路径：卷组还不存在，目录里没有 pool_max_bytes 可用，
   * 而容量口径必须与建池时的校验一致（否则自动填入的值一提交就超限）。
   */
  estimatePoolSize(body: EstimatePoolSizeRequest): Promise<EstimatePoolSizeResponse> {
    return this.request<EstimatePoolSizeResponse>('POST', '/v1/system/pools/estimate', body)
  }

  initializePool(body: InitializePoolRequest): Promise<LvmPoolStatusDTO> {
    return this.request<LvmPoolStatusDTO>('POST', '/v1/system/lvm/initialize', body)
  }

  // ---- 用户（管理员） ----

  listUsers(params?: { keyword?: string; limit?: number; offset?: number }): Promise<ListResponse<UserDTO>> {
    return this.request<ListResponse<UserDTO>>('GET', '/v1/users', undefined, params)
  }

  createUser(body: CreateUserRequest): Promise<CreateUserResponse> {
    return this.request<CreateUserResponse>('POST', '/v1/users', body)
  }

  getUser(id: string): Promise<UserDTO> {
    return this.request<UserDTO>('GET', `/v1/users/${encodeURIComponent(id)}`)
  }

  updateUser(id: string, body: UpdateUserRequest): Promise<UserDTO> {
    return this.request<UserDTO>('PATCH', `/v1/users/${encodeURIComponent(id)}`, body)
  }

  deleteUser(id: string): Promise<void> {
    return this.request<void>('DELETE', `/v1/users/${encodeURIComponent(id)}`)
  }

  listCertificates(id: string): Promise<ListResponse<CertificateDTO>> {
    return this.request<ListResponse<CertificateDTO>>('GET', `/v1/users/${encodeURIComponent(id)}/certificates`)
  }

  createCertificate(
    id: string,
    input?: { bound_ip?: string; bound_mac?: string }
  ): Promise<IssuedCertificateDTO> {
    return this.request<IssuedCertificateDTO>(
      'POST',
      `/v1/users/${encodeURIComponent(id)}/certificates`,
      input
    )
  }

  revokeCertificate(userId: string, certificateId: string): Promise<void> {
    return this.request<void>(
      'DELETE',
      `/v1/users/${encodeURIComponent(userId)}/certificates/${encodeURIComponent(certificateId)}`
    )
  }

  issueEnrollmentToken(id: string): Promise<EnrollmentTokenResponse> {
    return this.request<EnrollmentTokenResponse>(
      'POST',
      `/v1/users/${encodeURIComponent(id)}/enrollment-token`
    )
  }

  // ---- 存储库 ----

  listRepos(params?: { owner_id?: string; limit?: number; offset?: number }): Promise<ListResponse<RepoDTO>> {
    return this.request<ListResponse<RepoDTO>>('GET', '/v1/repos', undefined, params)
  }

  createRepo(body: CreateRepoRequest): Promise<CreateRepoResponse> {
    return this.request<CreateRepoResponse>('POST', '/v1/repos', body)
  }

  getRepo(id: string): Promise<RepoDTO> {
    return this.request<RepoDTO>('GET', `/v1/repos/${encodeURIComponent(id)}`)
  }

  updateRepo(id: string, body: UpdateRepoRequest): Promise<RepoDTO> {
    return this.request<RepoDTO>('PATCH', `/v1/repos/${encodeURIComponent(id)}`, body)
  }

  deleteRepo(id: string): Promise<void> {
    return this.request<void>('DELETE', `/v1/repos/${encodeURIComponent(id)}`)
  }

  listMembers(id: string): Promise<ListResponse<MemberDTO>> {
    return this.request<ListResponse<MemberDTO>>('GET', `/v1/repos/${encodeURIComponent(id)}/members`)
  }

  setMembers(id: string, members: MemberDTO[]): Promise<ListResponse<MemberDTO>> {
    return this.request<ListResponse<MemberDTO>>('PUT', `/v1/repos/${encodeURIComponent(id)}/members`, { members })
  }

  listAllocations(id: string): Promise<ListResponse<AllocationDTO>> {
    return this.request<ListResponse<AllocationDTO>>('GET', `/v1/repos/${encodeURIComponent(id)}/allocations`)
  }

  allocate(id: string, userId: string): Promise<AllocationDTO> {
    return this.request<AllocationDTO>('POST', `/v1/repos/${encodeURIComponent(id)}/allocations`, { user_id: userId })
  }

  parentAction(id: string, action: ParentAction): Promise<RepoDTO> {
    return this.request<RepoDTO>('POST', `/v1/repos/${encodeURIComponent(id)}/parent/actions`, { action })
  }

  copyRepo(id: string, newRepoName: string): Promise<JobDTO> {
    return this.request<JobDTO>('POST', `/v1/repos/${encodeURIComponent(id)}/copy`, { new_repo_name: newRepoName })
  }

  listRepoDisks(id: string): Promise<ListResponse<DiskDTO>> {
    return this.request<ListResponse<DiskDTO>>('GET', `/v1/repos/${encodeURIComponent(id)}/disks`)
  }

  // ---- 存储 ----

  listStorages(): Promise<ListResponse<StorageDTO>> {
    return this.request<ListResponse<StorageDTO>>('GET', '/v1/storages')
  }

  createStorage(input: CreateStorageRequest): Promise<StorageDTO> {
    return this.request<StorageDTO>('POST', '/v1/storages', input)
  }

  updateStorage(id: string, patch: { name?: string; path?: string; enabled?: boolean }): Promise<StorageDTO> {
    return this.request<StorageDTO>('PATCH', `/v1/storages/${encodeURIComponent(id)}`, patch)
  }

  deleteStorage(id: string): Promise<void> {
    return this.request<void>('DELETE', `/v1/storages/${encodeURIComponent(id)}`)
  }

  /** 挂载存储的底层卷（仅超管；Windows / 目录模式返回 501）。 */
  mountStorage(id: string): Promise<StorageDTO> {
    return this.request<StorageDTO>('POST', `/v1/storages/${encodeURIComponent(id)}/mount`)
  }

  /** 卸载存储的底层卷（仅超管；有进行中的上传时返回 409 storage.volume_busy）。 */
  unmountStorage(id: string): Promise<StorageDTO> {
    return this.request<StorageDTO>('POST', `/v1/storages/${encodeURIComponent(id)}/unmount`)
  }

  /** 扩容存储的底层卷（仅超管；仅系统创建的 thin 卷）。 */
  resizeStorage(id: string, sizeBytes: number): Promise<StorageDTO> {
    return this.request<StorageDTO>('POST', `/v1/storages/${encodeURIComponent(id)}/resize`, {
      size_bytes: sizeBytes
    })
  }

  // ---- 服务端目录浏览（已登录用户） ----

  listFsRoots(): Promise<ListResponse<FsRoot>> {
    return this.request<ListResponse<FsRoot>>('GET', '/v1/fs/roots')
  }

  browseFs(path: string): Promise<FsBrowseResult> {
    return this.request<FsBrowseResult>('GET', '/v1/fs/browse', undefined, { path })
  }

  statFs(path: string): Promise<FsStat> {
    return this.request<FsStat>('GET', '/v1/fs/stat', undefined, { path })
  }

  // ---- 磁盘 / 分配 ----

  getDisk(id: string): Promise<DiskDTO> {
    return this.request<DiskDTO>('GET', `/v1/disks/${encodeURIComponent(id)}`)
  }

  deleteDisk(id: string): Promise<JobDTO> {
    return this.request<JobDTO>('DELETE', `/v1/disks/${encodeURIComponent(id)}`)
  }

  compactDisk(id: string): Promise<JobDTO> {
    return this.request<JobDTO>('POST', `/v1/disks/${encodeURIComponent(id)}/compact`)
  }

  releaseAllocation(id: string): Promise<void> {
    return this.request<void>('DELETE', `/v1/allocations/${encodeURIComponent(id)}`)
  }

  mountAllocation(id: string, clientId: string): Promise<MountSpec> {
    return this.request<MountSpec>('POST', `/v1/allocations/${encodeURIComponent(id)}/mount`, { client_id: clientId })
  }

  // ---- 租约 ----

  listLeases(params?: { repo_id?: string }): Promise<ListResponse<LeaseDTO>> {
    return this.request<ListResponse<LeaseDTO>>('GET', '/v1/leases', undefined, params)
  }

  heartbeat(leaseId: string, clientId: string): Promise<HeartbeatResponse> {
    return this.request<HeartbeatResponse>('POST', `/v1/leases/${encodeURIComponent(leaseId)}/heartbeat`, {
      client_id: clientId
    })
  }

  reportMounted(leaseId: string, clientId: string, mountPoint: string): Promise<void> {
    return this.request<void>('POST', `/v1/leases/${encodeURIComponent(leaseId)}/mounted`, {
      client_id: clientId,
      mount_point: mountPoint
    })
  }

  releaseLease(leaseId: string, clientId: string): Promise<void> {
    return this.request<void>('POST', `/v1/leases/${encodeURIComponent(leaseId)}/release`, {
      client_id: clientId
    })
  }

  revokeLease(leaseId: string): Promise<void> {
    return this.request<void>('DELETE', `/v1/leases/${encodeURIComponent(leaseId)}`)
  }

  // ---- 任务 ----

  listJobs(params?: { state?: JobState; limit?: number; offset?: number }): Promise<ListResponse<JobDTO>> {
    return this.request<ListResponse<JobDTO>>('GET', '/v1/jobs', undefined, params)
  }

  getJob(id: string): Promise<JobDTO> {
    return this.request<JobDTO>('GET', `/v1/jobs/${encodeURIComponent(id)}`)
  }
}
