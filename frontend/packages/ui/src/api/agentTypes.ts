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
  /**
   * 这条挂载归属**哪台**服务端（与 ServerEntry.key 同算法：实例 ID 优先，其次规范化地址）。
   *
   * 多服务端下心跳、挂载点回写、租约释放都必须发给"当初受理这次挂载的那台"。
   * 老状态文件里没有该字段的记录，代理加载时归给"当时唯一/主服务端"（因此可能缺省）。
   */
  server_key?: string
  server_url?: string
  server_name?: string
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
  /**
   * **实测**的本机 iSCSI 会话状态：本机发起端当前是否还有到 target_iqn 的活动连接。
   *
   * state 只是代理的**记录/意图**，这块盘是否真的挂着由会话决定：会话被系统或用户断开
   * （MSiSCSI 服务重启、长时间断网、手动在发起程序里断开）之后记录仍是 mounted，
   * 界面于是显示"已挂载 + 卸载"，而盘根本不在 —— 用户既用不了盘、点卸载还会失败。
   * 代理每 ≈20s 实测一次（Get-IscsiSession），界面的挂载/卸载按钮以它为准。
   *
   * ⚠️ 只有实测为真时才是 true；探测失败不猜（保持原值）。
   */
  session_active?: boolean
  /**
   * 最近一次**得出会话结论**的时刻（毫秒时间戳）；结论不变时不刷新。
   *
   * 缺省/0 表示**尚未核对**（代理刚启动）：此时**不能**把 session_active 的缺省值
   * 当成"断线"，要按 state 展示（判定请统一走 mountSessionLive）。
   */
  session_checked_at?: number
  /**
   * 该库是否启用了本地读缓存（来自本机每库配置 repo_mounts[id].cache_enabled）。
   *
   * 这是"用户的意图"，不代表链路真的走了代理 —— 是否生效看 cache_active。
   */
  cache_enabled?: boolean
  /**
   * 缓存是否**真的生效**：true 表示这次挂载走的是 本机 → 本地缓存代理 → 服务端目标。
   *
   * false 且 cache_enabled=true 说明代理不可用（端口占用/后端不可达/CHAP/配额不足），
   * 挂载已**自动回退直连**服务端目标 —— 挂载照样成功，只是没有缓存加速。
   */
  cache_active?: boolean
  /** 缓存未生效（回退直连）的原因；仅 cache_enabled=true 且 cache_active=false 时非空。 */
  cache_error?: string
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

/** 服务端连接阶段（`AgentServerState.phase`）。 */
export type AgentServerPhase = 'connecting' | 'connected' | 'disconnected'

export interface AgentServerState {
  /**
   * 该台的本地键（实例 ID 优先，其次规范化地址）：与前端 ServerEntry.key 同算法。
   *
   * 状态、挂载、事件三者的归属键 —— 一条 `server` 事件只更新它那一台。
   * 旧版代理不带该字段。
   */
  server_key?: string
  /** 是否为主服务端（自更新 / 渲染层热更的更新源；无服务端上下文的操作默认用它）。 */
  primary?: boolean
  url: string
  instance_id?: string
  name?: string
  /** 本机给该台起的别名（目录模式的挂载点命名 `<别名>_<库名>` 优先用它），可空。 */
  alias?: string
  connected: boolean
  /**
   * 连接阶段：`connecting` 表示正在连接/重连（界面显示转圈的"连接中"，而不是"未连接"）；
   * `disconnected` 表示已断开且自动重连次数已用尽，等界面手动重试。
   *
   * 旧版代理不带该字段，界面按 `connected` 兜底即可。
   */
  phase?: AgentServerPhase
  /** 连续连接失败次数；达到上限后代理停止自动重连（手动重试会清零）。 */
  fail_count?: number
  last_error?: string
  /** 仅 /agent/state 的 servers[] 逐台附带：该台的登录用户。 */
  user?: AgentUserState
  /** 仅 /agent/state 的 servers[] 逐台附带：该台的会话（未推送会话时缺省）。 */
  session?: AgentSessionState
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
  /**
   * 该会话归属的服务端键（与 ServerEntry.key 同算法）。
   *
   * 宿主应用据此把续期后的新令牌写回**对应那一台**的服务端条目，而不是活动服务端。
   * 旧版代理不带该字段（等价于"唯一的那台"）。
   */
  server_key?: string
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

/** GET /agent/log：代理日志（日志模块）。日志按天切分，一次回一天的文件尾部。 */
export interface AgentLog {
  /** 本次返回的日期（YYYY-MM-DD）；请求未指定 day 时即当天。 */
  day: string
  /** 存在日志文件的日期（YYYY-MM-DD，升序），供界面做按时间切分的选择。 */
  days: string[]
  /** 该日期日志文件路径（可能为空）。 */
  path: string
  /** 日志文件末尾的原始文本（JSON Lines，按 level/msg 等字段组织的 slog 输出）。 */
  text: string
  /** 读取失败时的稳定错误码。 */
  error?: string
}

/** GET /agent/state。 */
export interface AgentState {
  /**
   * 全部已登记服务端（多服务端：逐台完整状态，含该台的登录用户与会话）。
   *
   * 客户端启动时为每一项各推一次会话即完成"全部自动登录"（见 ServerSession.server_key）。
   */
  servers: AgentServerState[]
  /** 主服务端的 server_key（更新源等无服务端上下文操作的目标）。 */
  primary_key?: string
  /**
   * 主服务端那一台（单服务端时代的**兼容字段**；多服务端下界面应改用 servers[] 逐台渲染）。
   */
  server: AgentServerState
  user: AgentUserState
  mounts: AgentMountState[]
  auto_mount: boolean
  /** 本机就绪状态（iSCSI 发起端）；代理尚未探测完时字段可能缺省。 */
  host?: AgentHostState
  update: AgentUpdateState
  /** 主服务端会话；未登录（未推送会话）时缺省。 */
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
  /** 全部已登记服务端（逐台一份，主服务端在前）；旧版代理不带该字段。 */
  servers?: AgentServerState[]
  /** 主服务端的 server_key；旧版代理不带该字段。 */
  primary_key?: string
  /** 仅在调用方传入 expected_version 查询参数时返回。 */
  expected_version?: string
  /** 代理版本与调用方期望是否不一致（仅在传入 expected_version 时返回）。 */
  version_mismatch?: boolean
  /** 主服务端那一台（兼容字段）。 */
  server: {
    connected: boolean
    url: string
    name: string
    last_error?: string
  }
}

/**
 * 单个存储库自己的挂载偏好（GET /agent/config 的 repo_mounts，写用 POST /agent/repo-mounts/{id}）。
 *
 * 为什么不放服务端：挂载形态与挂载目录是"这台机器"的事（盘符、D:\vault\xxx 这类本地路径
 * 对别的机器没有意义），自动挂载也由本机代理执行。没有条目的库跟随上面的全局默认值。
 */
export interface AgentRepoMountPref {
  /** letter | directory；空串表示跟随 default_mount_mode。 */
  mount_mode?: AgentMountMode | ''
  /**
   * 目录模式的**父目录**；空串表示直接用 default_mount_dir。
   *
   * 实际挂载点是它下面一层 `<服务端名称>_<存储库名称>` 子目录（代理侧规则，
   * 与界面提示同一口径：前端预览见 utils/mountPath.ts）。
   */
  mount_dir?: string
  /**
   * 是否在该库所在客户端启动后自动挂载它（每个库独立）。
   *
   * 与全局 auto_mount 的区别：全局开关只管"恢复本机上次留下的挂载记录"，
   * 这里是每库显式表态 —— 为 true 时本机没有记录也会挂上；为 false 时连记录都不恢复。
   */
  auto_mount: boolean
  /**
   * 是否为该库启用**本地 iSCSI 读缓存代理**（每个库独立，**默认关闭**）。
   *
   * 开启后本机到服务端的链路变成 本机发起端 → 本地缓存代理（127.0.0.1:3261）→ 服务端目标：
   * 多占用一份本机内存（L1）与磁盘（L2，总量在「客户端设置」里配，按已启用的库均分）。
   * 代理不可用时会**自动回退直连**服务端目标（挂载照常成功，卡片提示「缓存未生效」）。
   * 改动需要**重新挂载**这个库才生效。
   */
  cache_enabled?: boolean
}

/** GET /agent/config 与 PATCH /agent/config。 */
export interface AgentConfig {
  auto_mount: boolean
  /**
   * 挂载形态/目录的兜底默认值，**只读**：界面上没有这两项，PATCH 也不接受
   * （挂载形态与目录一律按库配置，见 AgentRepoMountPref）。它们仍然返回，
   * 用于给没配过的库**预填**并作为挂载根的兜底。
   */
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
  /**
   * 本地读缓存代理的 **L1（内存）总预算**（字节）：客户端总量，在已启用缓存的库之间均分。
   *
   * 用户只在这一层设置（一个客户端一个代理）；某个库分到多少由代理按"已启用缓存的库数"算。
   */
  cache_l1_bytes: number
  /**
   * 本地读缓存代理的 **L2（本地磁盘文件）总预算**（字节）：同样是客户端总量、按库均分。
   * **0 表示关闭 L2**（只做内存缓存），是合法取值。
   */
  cache_l2_bytes: number
  /**
   * L2 缓存文件的存放目录（客户端设置里可改）。
   *
   * 空串表示默认位置（程序数据目录下的 `iscsi-cache`）。非空必须是**绝对路径**，
   * 否则 PATCH 返回 system.invalid_param（args.field=cache_l2_dir）。
   * 改动在**下一次挂载**时生效（L2 文件路径在创建缓存目标时定下）。
   */
  cache_l2_dir: string
  /** 按存储库 ID 保存的挂载偏好；没有条目的库跟随上面的全局默认值（PATCH 不接受该字段）。 */
  repo_mounts?: Record<string, AgentRepoMountPref>
}

/**
 * 单个库（分配）的缓存用量与命中情况（GET /agent/cache 的 targets[]）。
 *
 * 只有**当前在本地门户注册了目标**的库才会出现在这里 —— 也就是"已启用缓存且本次挂载
 * 真的走了代理"的库；未启用或已回退直连的库不会出现。
 */
export interface AgentCacheTargetStatus {
  /** 与挂载记录、存储库卡片一一对应。 */
  allocation_id: string
  /** 本地门户为该库公示的 IQN（挂载链路上实际连接的那个）。 */
  target_iqn: string
  l1_used_bytes: number
  l1_limit_bytes: number
  /** L2 未启用时为 0。 */
  l2_used_bytes: number
  l2_limit_bytes: number
  /** 读命令数。 */
  reads: number
  /** **整条命令**完全由缓存满足的次数（命中率的分母/分子口径见 hit_rate）。 */
  request_hits: number
  /** 分段细粒度计数。 */
  l1_hits: number
  partial_hits: number
  l2_hits: number
  backend_reads: number
  writes: number
  /** request_hits / reads（0..1）；reads 为 0 时为 0。 */
  hit_rate: number
}

/**
 * 本地 iSCSI 读缓存代理的对外状态（GET /agent/cache）。
 *
 * 门户是**懒启动**的：没有任何库启用缓存时不监听任何端口（running=false，targets 为空）。
 */
export interface AgentCacheStatus {
  running: boolean
  /** 门户监听地址（未运行时为空）。 */
  addr?: string
  /** 客户端总量预算。 */
  l1_limit_bytes: number
  l2_limit_bytes: number
  /** 当前均分给每个启用缓存的库的配额。 */
  l1_quota_bytes: number
  l2_quota_bytes: number
  /** 已启用缓存的库数（配额分母）。 */
  enabled_repos: number
  targets: AgentCacheTargetStatus[]
  /** 最近一次打开门户失败的原因（如端口被占用）；正常时为空。 */
  error?: string
}

/** GET /agent/identity 与 POST /agent/identity/install 的响应（永不含私钥与证书原文）。 */
export interface AgentIdentity {
  installed: boolean
  /**
   * 身份归属的服务端标识：实例 ID 优先，其次规范化地址（与 ServerEntry.key 同源）。
   *
   * 一台机器可同时保存多个服务端的证书，靠它区分"这份证书是谁的"。
   */
  server_key?: string
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

/**
 * GET /agent/identities 的响应：本机保存的**全部**服务端身份。
 *
 * 多服务端下一个服务端一份证书，用于在设置里列出"哪些服务端装了证书"并单独撤销某一个。
 */
export interface AgentIdentityList {
  /** "未指定服务端"时使用的身份键；无身份时缺省。 */
  active?: string
  identities: AgentIdentity[]
}

/** POST /agent/identity/login 的响应（与口令登录一致，由代理原样透出）。 */
export interface AgentIdentityLoginResponse {
  token: string
  expires_at: number
  user: { id: string; username: string; role: Role }
}

/** POST /agent/session 的请求体（会话令牌只在传输中使用，不进入日志）。 */
export interface ServerSession {
  /**
   * 指明这段会话属于**哪一台**服务端（与 ServerEntry.key 同算法：实例 ID 优先，其次规范化地址）。
   *
   * 省略时由代理按 server_instance_id / server_url 推导；响应回传实际归属键。
   */
  server_key?: string
  /** 把该台设为主服务端（更新源等无服务端上下文操作的目标）；首台注册时自动成为主服务端。 */
  primary?: boolean
  /** 客户端为**本机**给该台起的别名；目录模式挂载点命名 `<别名>_<库名>` 优先用它。 */
  alias?: string
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
  /**
   * 把库建到**哪台**服务端（二选一即可）。
   *
   * 存储 ID 只对"它所属的那台服务端"有意义：给了标识却匹配不上任何已登记服务端会得到
   * `agent.server_unknown`；都省略时按主服务端兜底（老客户端）。
   */
  server_key?: string
  server_url?: string
}

/** 事件流（GET /agent/events）解析结果。 */
export type AgentEvent =
  | { type: 'mount'; mount: AgentMountState }
  | { type: 'unmount'; allocation_id: string }
  /** server_key 标识这条挂载归属哪台（旧版代理不带）。 */
  | { type: 'revoked'; allocation_id: string; reason?: string; server_key?: string }
  /**
   * 一条 server 事件只更新**它那一台**（server_key 为归属键，旧版代理不带）。
   *
   * 不带 server_key 时按"主服务端那一台"处理，兼容单服务端语义。
   */
  | {
      type: 'server'
      server_key?: string
      url?: string
      name?: string
      connected: boolean
      phase?: AgentServerPhase
      fail_count?: number
      last_error?: string
    }
  | { type: 'host'; host: AgentHostState }
  | { type: 'session'; session: AgentSessionState }
  | { type: 'update'; available_version?: string; downloading: boolean; received_bytes: number; total_bytes: number }
  | { type: 'web_update'; web_update: WebUpdateState }
  | { type: 'download'; download: DownloadState }
  | { type: 'upload'; upload: UploadState }
  | { type: 'heartbeat'; at: number; allocation_id?: string; server_key?: string }
