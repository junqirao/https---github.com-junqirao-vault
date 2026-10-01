/**
 * 本地代理（Vault-Agent）DTO 类型。
 *
 * 严格对齐 docs/agent-api.md（v1 契约），字段名沿用下划线风格。
 * **绝不包含** lease 的 CHAP 密钥与会话令牌。
 */
import type { Role } from './types'

export type AgentMountMode = 'letter' | 'directory'
export type AgentMountStatus = 'mounting' | 'mounted' | 'unmounting' | 'error' | 'revoked'

/**
 * 挂载过程中的细粒度阶段（MountState.phase），仅在 state='mounting' 时存在。
 *
 * 线上契约（与 internal/app、internal/agent/state.go 一一对应）：
 *   - 服务端侧：allocating / preparing_disk / configuring_target
 *     （服务端在自己的同步请求内发事件，代理经 SSE 转发，见 internal/app/events.go）
 *   - 本机侧：requesting / connecting / online / mount_point / post_script
 *
 * 为什么要它：服务端侧的挂载是同步重活（准备磁盘 + PowerShell 下发 iSCSI 目标，
 * 实测数十秒），只给一个"挂载中"用户无从知道卡在哪一步。
 */
export type AgentMountPhase =
  | 'requesting'
  | 'allocating'
  | 'preparing_disk'
  | 'configuring_target'
  | 'connecting'
  | 'online'
  | 'mount_point'
  | 'post_script'

/** GET /agent/state 与事件流中的 MountState。 */
export interface AgentMountState {
  repo_id: string
  repo_name: string
  allocation_id: string
  lease_id: string
  target_iqn: string
  portal: string
  mount_mode: AgentMountMode
  /** letter 模式为 "E:"；directory 模式为绝对目录。 */
  mount_path: string
  disk_number?: number
  state: AgentMountStatus
  /** 挂载中的细粒度阶段（见 AgentMountPhase）；非 mounting 时缺省。 */
  phase?: AgentMountPhase
  mounted_at?: number
  last_heartbeat_at?: number
  last_error?: string
  /**
   * 最近一次失败的**原始报错文本**（如 PowerShell 脚本自报的 .NET 异常）。
   *
   * 与 last_error 的区别：last_error 是稳定码（如 `connect:platform.ps_failed`），
   * 这个字段才是"为什么"——按项目约定原始报错只进日志，这里是为本机诊断开的**刻意例外**，
   * 只出现在挂载状态里（诊断面板/挂载状态悬浮可读、可复制），不进任何 API 错误响应。
   */
  last_error_detail?: string
}

export type DownloadStatus = 'running' | 'done' | 'failed' | 'canceled'

/**
 * 单个母盘下载任务的状态（POST /agent/disks/download、GET /agent/downloads 与 download 事件）。
 *
 * `target_path` 为落地后的完整文件路径；`error` 为稳定错误码（仅 failed 时非空）。
 */
export interface DownloadState {
  disk_id: string
  file_name: string
  target_path: string
  state: DownloadStatus
  received_bytes: number
  total_bytes: number
  error?: string
  started_at: number
  finished_at?: number
}

export interface AgentServerState {
  url: string
  instance_id?: string
  name?: string
  connected: boolean
  last_error?: string
}

export interface AgentUserState {
  id: string
  username: string
}

export interface AgentUpdateState {
  channel: string
  current_version: string
  available_version?: string
  downloading: boolean
  /** 自更新产物下载进度（页面刷新后据此恢复展示）。 */
  received_bytes: number
  total_bytes: number
  /** 最近一次"无可用更新"的稳定原因码（代理侧，如 no_manifest / not_newer）。 */
  reason?: string
  /** 更新源（服务端）返回的具体原因（如 manifest_missing / artifacts_dir_not_configured）。 */
  server_reason?: string
}

/**
 * 当前服务端会话（GET /agent/state 的 session 字段与 session 事件）。
 *
 * 这是本机 127.0.0.1 的本地接口（与 /agent/identity、/agent/state 同信任级别），
 * 因此会返回 token：渲染进程据此在代理自动续期后更新自己持有的令牌。
 */
export interface AgentSessionState {
  server_url: string
  token: string
  expires_at: number
  user_id: string
  username: string
}

/**
 * 代理所在主机的就绪状态（GET /agent/state 的 host 字段与 host 事件）。
 *
 * 目前只覆盖 iSCSI 发起端（MSiSCSI）：它没运行时挂载必定失败在 connect 阶段，而错误里
 * 没有可执行信息。界面据此在**启动时**就挂横幅提示，而不是等用户点了挂载才知道。
 */
export interface AgentHostState {
  /** 是否找得到 iSCSI 发起端（IscsiInitiator 模块 + cmdlet）。 */
  iscsi_available: boolean
  /** 归一化服务状态：running / stopped / unknown。 */
  iscsi_service: 'running' | 'stopped' | 'unknown'
  /** 现在能否发起点 iSCSI 连接（模块可用且服务在运行）。 */
  iscsi_ready: boolean
  /** 不就绪的稳定原因码：module_missing / service_stopped / probe_failed。 */
  reason?: 'module_missing' | 'service_stopped' | 'probe_failed'
  /** 探测失败时的稳定错误码。 */
  error?: string
  /** 最近一次探测时间（毫秒时间戳）。 */
  checked_at?: number
}

/** GET /agent/log：代理日志尾部（日志模块）。 */
export interface AgentLog {
  /** 当天日志文件路径（可能为空）。 */
  path: string
  /** 日志文件末尾的原始文本（JSON Lines，按 level/msg 等字段组织的 slog 输出）。 */
  text: string
  /** 读取失败时的稳定错误码。 */
  error?: string
}

/** GET /agent/state。 */
export interface AgentState {
  server: AgentServerState
  user: AgentUserState
  mounts: AgentMountState[]
  auto_mount: boolean
  /** 本机就绪状态（iSCSI 发起端）；代理尚未探测完时字段可能缺省。 */
  host?: AgentHostState
  update: AgentUpdateState
  /** 当前会话；未登录（未推送会话）时缺省。 */
  session?: AgentSessionState
}

/** GET /agent/health。 */
export interface AgentHealth {
  ok: boolean
  agent_version: string
  platform: string
  admin: boolean
  /** 本机就绪状态（同 AgentState.host）。 */
  host?: AgentHostState
  /** 仅在调用方传入 expected_version 查询参数时返回。 */
  expected_version?: string
  /** 代理版本与调用方期望是否不一致（仅在传入 expected_version 时返回）。 */
  version_mismatch?: boolean
  server: {
    connected: boolean
    url: string
    name: string
    last_error?: string
  }
}

/** GET /agent/config 与 PATCH /agent/config。 */
export interface AgentConfig {
  auto_mount: boolean
  default_mount_mode: AgentMountMode
  default_mount_dir: string
  /** 母盘下载到本地时的默认目标目录（绝对路径）。 */
  default_download_dir: string
  language: string
  start_at_login: boolean
  update_channel: string
  server_alias: string
  /** 是否用本地客户端证书免密登录（identity.json 存在时生效）。 */
  auto_login: boolean
}

/** GET /agent/identity 与 POST /agent/identity/install 的响应（永不含私钥与证书原文）。 */
export interface AgentIdentity {
  installed: boolean
  server_url?: string
  server_instance_id?: string
  server_cert_sha256?: string
  user_id?: string
  username?: string
  serial?: string
  fingerprint_sha256?: string
  spki_sha256?: string
  not_before?: number
  not_after?: number
  installed_at?: number
}

/** POST /agent/identity/install 的请求体。 */
export interface AgentIdentityInstallInput {
  server_url: string
  token: string
  user_id: string
  username: string
  cert_sha256: string
}

/** POST /agent/identity/login 的响应（与口令登录一致，由代理原样透出）。 */
export interface AgentIdentityLoginResponse {
  token: string
  expires_at: number
  user: { id: string; username: string; role: Role }
}

/** POST /agent/session 的请求体（会话令牌只在传输中使用，不进入日志）。 */
export interface ServerSession {
  server_url: string
  server_instance_id: string
  server_name: string
  token: string
  user_id: string
  username: string
  expires_at: number
  /**
   * 服务端证书 DER 的 SHA-256（小写十六进制）。
   *
   * 由渲染进程在连通性探测成功后固化并随会话推送，代理据此固定指纹校验服务端证书
   * （见 internal/agent/server.go 的 newServerClient）。缺省时退回默认链路校验。
   */
  cert_sha256?: string
}

/** 探测到的服务端证书摘要（仅连通性探测返回，供与服务器 pki/server.crt 比对）。 */
export interface ServerProbeCert {
  /** 证书 DER 的 SHA-256 指纹（小写十六进制）。 */
  sha256: string
  subject?: string
  /** 过期时间（Unix 秒）。 */
  not_after?: number
}

/** POST /agent/server/test 的响应。 */
export interface ServerProbe {
  ok: boolean
  /** 实际生效的地址（可能已从 http 自动升级为 https）。 */
  server_url: string
  /** 用户原始输入。 */
  normalized_from: string
  /** 是否发生了 http → https 的自动升级。 */
  upgraded_to_tls: boolean
  server_name: string
  api_version: number
  server_version: string
  server_instance_id: string
  client_compat: {
    enabled: boolean
    min?: string
    max?: string
  }
  /** 服务端证书摘要；纯 http 服务端缺省。 */
  cert?: ServerProbeCert
}

/** POST /agent/update/check 的响应。 */
export interface UpdateInfo {
  available: boolean
  version?: string
  notes?: string
  size_bytes?: number
  source?: string
  /** 不可用时的稳定原因码（代理侧：server_unreachable / not_newer / no_artifact / no_manifest ...）。 */
  reason?: string
  /** 更新源（服务端）的结构化错误：错误码（如 update.no_manifest）。 */
  server_code?: string
  /** 服务端错误的具体原因（如 manifest_missing / artifacts_dir_not_configured）。 */
  server_reason?: string
  /** 服务端错误的插值参数。 */
  server_args?: Record<string, unknown>
}

/** 渲染层资源热更状态（POST /agent/update/web/apply、GET /agent/update/web、web_update 事件）。 */
export type WebUpdateStatus = 'idle' | 'downloading' | 'verifying' | 'extracting' | 'activated' | 'failed'

/**
 * WebUpdateState：客户端资源热更状态。
 *
 * `activated` 表示资源已就位到本地磁盘（<webapp>/<version> + current.json 已切换），**需重启客户端生效**；
 * `failed` 时 `error` 为稳定错误码（如 agent.web_update_failed）。
 */
export interface WebUpdateState {
  state: WebUpdateStatus
  available_version?: string
  active_version?: string
  received_bytes: number
  total_bytes: number
  error?: string
  updated_at?: number
}

/** 上传任务状态（POST /agent/uploads/start、GET /agent/uploads、upload 事件）。 */
export type UploadStatus =
  | 'scanning'
  | 'creating'
  | 'uploading'
  | 'completing'
  | 'done'
  | 'failed'
  | 'canceled'

/** 单个本地目录上传任务的状态。 */
export interface UploadState {
  upload_id: string
  repo_name: string
  local_dir: string
  state: UploadStatus
  total_files: number
  total_bytes: number
  uploaded_bytes: number
  chunk_total: number
  chunk_done: number
  job_id?: string
  repo_id?: string
  error?: string
  started_at: number
  finished_at?: number
}

/** POST /agent/fs/scan 的响应。 */
export interface LocalScanResult {
  path: string
  file_count: number
  total_bytes: number
  warnings: string[]
}

/** POST /agent/uploads/start 的请求体。 */
export interface StartUploadInput {
  local_dir: string
  repo_name: string
  repo_mode: 'shared' | 'exclusive'
  storage_id?: string
  quota_bytes?: number
  source_mode: 'copy' | 'move'
}

/** 事件流（GET /agent/events）解析结果。 */
export type AgentEvent =
  | { type: 'mount'; mount: AgentMountState }
  | { type: 'unmount'; allocation_id: string }
  | { type: 'revoked'; allocation_id: string; reason?: string }
  | { type: 'server'; connected: boolean; last_error?: string }
  | { type: 'host'; host: AgentHostState }
  | { type: 'session'; session: AgentSessionState }
  | { type: 'update'; available_version?: string; downloading: boolean; received_bytes: number; total_bytes: number }
  | { type: 'web_update'; web_update: WebUpdateState }
  | { type: 'download'; download: DownloadState }
  | { type: 'upload'; upload: UploadState }
  | { type: 'heartbeat'; at: number }
