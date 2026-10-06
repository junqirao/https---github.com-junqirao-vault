/**
 * 本地代理（Vault-Agent）客户端。
 *
 * 调用通道（按优先级）：
 *  - Electron：主进程拉起代理后，经 preload 的 `window.vault.agentRequest()` 转发请求、
 *    经 `agentEventsStart/onAgentEvent` 转发事件流。**令牌不出主进程**，也不依赖 CORS；
 *  - 浏览器 dev：回退直连 `VITE_AGENT_URL` / `VITE_AGENT_TOKEN`；
 *  - 两者都没有：视为"代理不可用"，所有调用抛出 `AgentUnavailableError`。
 */
import { ApiError, businessErrorFrom } from './errors'
import type {
  AgentConfig,
  AgentEvent,
  AgentHealth,
  AgentIdentity,
  AgentIdentityInstallInput,
  AgentIdentityLoginResponse,
  AgentHostState,
  AgentLog,
  AgentMountMode,
  AgentMountState,
  AgentRepoMountPref,
  AgentServerState,
  AgentSessionState,
  AgentState,
  DownloadState,
  LocalScanResult,
  ServerProbe,
  ServerSession,
  StartUploadInput,
  UpdateInfo,
  UploadState,
  WebUpdateState
} from './agentTypes'

/**
 * 代理状态（主进程下发；已剔除令牌）。
 *
 * `baseUrl` 仅供诊断展示，实际调用由主进程按自己的记录发起。
 */
export interface VaultAgentInfo {
  available: boolean
  baseUrl: string | null
  error?: string
  /** 代理自报版本（主进程查询 /agent/health 后回填）。 */
  version?: string | null
  /** 客户端期望的代理版本。 */
  expectedVersion?: string
  /** 代理版本与客户端期望是否不一致（陈旧代理）。 */
  versionMismatch?: boolean
}

/** 代理版本状态（来自主进程的 AgentInfo，供顶栏提示"代理版本不一致"）。 */
export interface AgentVersionInfo {
  agent: string | null
  expected: string
  mismatch: boolean
}

/** 一次代理请求（与 electron/agent.ts 的 AgentRequestOptions 对应）。 */
export interface AgentRequestOptions {
  method: string
  path: string
  body?: unknown
  timeoutMs?: number
}

/** 代理请求结果；`status === 0` 表示未到达代理，原因见 `error`。 */
export interface AgentRequestResult {
  status: number
  payload?: unknown
  error?: 'unavailable' | 'timeout' | 'network'
  message?: string
}

/** 代理事件流（SSE）消息。 */
export type AgentStreamMessage =
  | { kind: 'open' }
  | { kind: 'error' }
  | { kind: 'event'; type: string; data: string }

/** 系统目录选择结果（`canceled` 为用户取消）。 */
export interface DirectorySelection {
  canceled: boolean
  path?: string
}

/**
 * preload 暴露的 `window.vault`（Electron 下才存在）。
 *
 * 代理相关的三个能力标为可选：它们是"能力探测"入口——浏览器 dev（无 preload）与
 * 早期 preload 都没有它们，此时自动回退到 HTTP 直连分支。
 */
export interface VaultBridge {
  getConfig: () => Promise<unknown>
  setConfig: (value: unknown) => Promise<boolean>
  getAgent: () => Promise<VaultAgentInfo>
  onAgentChanged: (cb: (info: VaultAgentInfo) => void) => (() => void) | void
  agentRequest?: (options: AgentRequestOptions) => Promise<AgentRequestResult>
  agentEventsStart?: () => Promise<boolean>
  agentEventsStop?: () => Promise<boolean>
  onAgentEvent?: (cb: (message: AgentStreamMessage) => void) => (() => void) | void
  /** 把某个服务端的证书指纹登记为可信（TOFU）；见 trustServerCertificate。 */
  trustCertificate?: (serverUrl: string, sha256: string) => Promise<boolean>
  /** 打开系统目录选择框（Electron 下才存在）。 */
  selectDirectory?: () => Promise<DirectorySelection>
  openPath: (path: string) => Promise<void>
  showItemInFolder: (path: string) => Promise<void>
  quit: () => Promise<void>
  /** 立即重启客户端（Electron 下才存在；用于让热更后的渲染层资源生效）。 */
  relaunch?: () => Promise<void>
  /**
   * 让刚激活的热更渲染层立刻生效（Electron 下才存在）：主进程重新解析热更目录并加载，
   * **不需要重启客户端**。applied=false 表示当前已是最新层或没有可用热更层。
   */
  applyWebLayer?: () => Promise<{ applied: boolean; version?: string; reason?: string }>
}

/**
 * 事件流的最小接口。
 *
 * 与 EventSource 的常用子集保持一致，因此浏览器回退分支可以直接返回真正的 EventSource，
 * 而 IPC 分支返回一个在主进程转发消息之上包装出的同形对象。
 */
export interface AgentEventStream {
  onopen: ((event: Event) => void) | null
  onerror: ((event: Event) => void) | null
  addEventListener: (type: string, listener: (event: Event) => void) => void
  close: () => void
}

/** 代理不可用（未找到 / 未启动 / 未推送到渲染进程）。 */
export class AgentUnavailableError extends ApiError {
  constructor() {
    super({ kind: 'network', code: 'err.agent.unavailable' })
    this.name = 'AgentUnavailableError'
  }
}

export function isAgentUnavailableError(error: unknown): error is AgentUnavailableError {
  return error instanceof AgentUnavailableError
}

interface AgentEndpoint {
  baseUrl: string
  token: string
}

/** 代理调用通道：Electron 下走 IPC（令牌不出主进程），浏览器 dev 回退直连 HTTP。 */
type Transport = { kind: 'ipc' } | ({ kind: 'http' } & AgentEndpoint)

function hostBridge(): VaultBridge | undefined {
  if (typeof window === 'undefined') return undefined
  return (window as unknown as { vault?: VaultBridge }).vault
}

function trimBase(baseUrl: string): string {
  return baseUrl.replace(/\/+$/, '')
}

let transport: Transport | null = null
let pending: Promise<Transport | null> | null = null
let unavailableAt = 0
const endpointListeners = new Set<() => void>()

/** 代理版本状态：来自主进程 AgentInfo，供顶栏提示"代理版本不一致"。 */
let agentVersion: AgentVersionInfo = { agent: null, expected: '', mismatch: false }
const versionListeners = new Set<() => void>()

/** 读取当前代理版本状态（供 useSyncExternalStore 使用）。 */
export function getAgentVersionInfo(): AgentVersionInfo {
  return agentVersion
}

/** 订阅代理版本状态变化。 */
export function subscribeAgentVersion(listener: () => void): () => void {
  versionListeners.add(listener)
  return () => {
    versionListeners.delete(listener)
  }
}

function updateAgentVersion(info: VaultAgentInfo): void {
  const next: AgentVersionInfo = {
    agent: typeof info.version === 'string' ? info.version : null,
    expected: typeof info.expectedVersion === 'string' ? info.expectedVersion : '',
    mismatch: info.versionMismatch === true
  }
  if (
    next.agent === agentVersion.agent &&
    next.expected === agentVersion.expected &&
    next.mismatch === agentVersion.mismatch
  ) {
    return
  }
  agentVersion = next
  for (const listener of versionListeners) listener()
}

/** 订阅"代理通道变化"（代理重启后端口/令牌会变，可用性也会变化）。 */
export function onAgentEndpointChanged(listener: () => void): () => void {
  endpointListeners.add(listener)
  return () => {
    endpointListeners.delete(listener)
  }
}

/**
 * 应用主进程下发的代理状态。
 *
 * Electron 下只需要"代理是否可用"：地址与令牌都由主进程掌握，渲染进程不再需要它们。
 */
function applyAgentInfo(info: VaultAgentInfo): void {
  transport = info.available ? { kind: 'ipc' } : null
  unavailableAt = info.available ? 0 : Date.now()
  updateAgentVersion(info)
  for (const listener of endpointListeners) listener()
}

function envEndpoint(): AgentEndpoint | null {
  const env = import.meta.env
  const url = env?.VITE_AGENT_URL
  const token = env?.VITE_AGENT_TOKEN
  if (url && token) return { baseUrl: trimBase(url), token }
  return null
}

async function resolveTransport(force = false): Promise<Transport | null> {
  if (!force && transport) return transport
  if (!force && pending) return pending
  // 代理不可用时短暂缓存，避免高频重试。
  if (!force && unavailableAt && Date.now() - unavailableAt < 3000) return null

  pending = (async (): Promise<Transport | null> => {
    const host = hostBridge()
    const getAgent = host?.getAgent
    // Electron：由主进程转发，渲染进程既不持有令牌也不需要 CORS。
    if (getAgent && host?.agentRequest) {
      try {
        applyAgentInfo(await getAgent())
      } catch {
        transport = null
        unavailableAt = Date.now()
      }
      return transport
    }
    // 浏览器 dev：直连（地址与令牌来自 Vite 环境变量）。
    const env = envEndpoint()
    transport = env ? { kind: 'http', ...env } : null
    if (!transport) unavailableAt = Date.now()
    return transport
  })()

  try {
    return await pending
  } finally {
    pending = null
  }
}

async function requireTransport(): Promise<Transport> {
  const resolved = await resolveTransport()
  if (!resolved) throw new AgentUnavailableError()
  return resolved
}

async function readBody(response: Response): Promise<unknown> {
  const text = await response.text()
  if (!text) return undefined
  try {
    return JSON.parse(text) as unknown
  } catch {
    return undefined
  }
}

interface RequestOptions {
  method: string
  path: string
  body?: unknown
  timeoutMs?: number
}

async function request<T>(options: RequestOptions): Promise<T> {
  const target = await requireTransport()
  if (target.kind === 'ipc') return ipcRequest<T>(options)
  return httpRequest<T>(target, options)
}

/** IPC 通道：交给主进程代发（令牌不出主进程，也不经过浏览器同源策略）。 */
async function ipcRequest<T>(options: RequestOptions): Promise<T> {
  const call = hostBridge()?.agentRequest
  if (!call) throw new AgentUnavailableError()

  let result: AgentRequestResult
  try {
    result = await call({
      method: options.method,
      path: options.path,
      body: options.body,
      timeoutMs: options.timeoutMs
    })
  } catch (error) {
    // IPC 本身失败（主进程异常/窗口销毁）：等价于网络不可达。
    throw new ApiError({
      kind: 'network',
      code: 'network.error',
      message: error instanceof Error ? error.message : undefined
    })
  }

  if (result.status === 0) {
    if (result.error === 'unavailable') throw new AgentUnavailableError()
    if (result.error === 'timeout') throw new ApiError({ kind: 'timeout', code: 'network.timeout' })
    throw new ApiError({ kind: 'network', code: 'network.error', message: result.message })
  }
  if (result.status < 200 || result.status >= 300) {
    throw businessErrorFrom(result.status, result.payload)
  }
  return result.payload as T
}

/** HTTP 通道：仅浏览器 dev 回退使用（地址与令牌来自 Vite 环境变量）。 */
async function httpRequest<T>(target: AgentEndpoint, options: RequestOptions): Promise<T> {
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), options.timeoutMs ?? 20000)
  const headers: Record<string, string> = {
    Accept: 'application/json',
    'X-Vault-Agent-Token': target.token
  }
  if (options.body !== undefined) headers['Content-Type'] = 'application/json'

  let response: Response
  try {
    response = await fetch(`${target.baseUrl}${options.path}`, {
      method: options.method,
      headers,
      signal: controller.signal,
      body: options.body === undefined ? undefined : JSON.stringify(options.body)
    })
  } catch (error) {
    if (error instanceof DOMException && error.name === 'AbortError') {
      throw new ApiError({ kind: 'timeout', code: 'network.timeout' })
    }
    throw new ApiError({
      kind: 'network',
      code: 'network.error',
      message: error instanceof Error ? error.message : undefined
    })
  } finally {
    clearTimeout(timer)
  }

  const payload = await readBody(response)
  if (!response.ok) throw businessErrorFrom(response.status, payload)
  return payload as T
}

/**
 * 挂载/卸载涉及 iSCSI 会话与磁盘上线，超时放宽到 5 分钟。
 *
 * 预算：服务端同步发布 iSCSI 目标最多 2 分钟（代理侧 mountRequestTimeout），
 * 之后还有门户登记、会话建立（最多 60s）、磁盘上线与后置脚本。
 * 代理会通过 mount 事件持续上报阶段，界面不是"卡住不动"。
 */
const MOUNT_TIMEOUT_MS = 300000

/**
 * 把主进程转发来的事件流消息包装成 EventSource 形状的对象。
 *
 * 之所以在渲染进程侧再包一层：调用方（useAgent）按 EventSource 的用法书写，
 * 换成 IPC 后无需改动它们的用法。
 */
function ipcEventStream(
  start: () => Promise<boolean>,
  stop: () => Promise<boolean>,
  onEvent: (cb: (message: AgentStreamMessage) => void) => (() => void) | void
): AgentEventStream {
  const listeners = new Map<string, Set<(event: Event) => void>>()
  let detach: () => void = () => undefined

  const emit = (type: string, data: string): void => {
    const set = listeners.get(type)
    if (!set || set.size === 0) return
    const event = { type, data } as unknown as Event
    for (const listener of [...set]) listener(event)
  }

  const stream: AgentEventStream = {
    onopen: null,
    onerror: null,
    addEventListener: (type, listener) => {
      let set = listeners.get(type)
      if (!set) {
        set = new Set()
        listeners.set(type, set)
      }
      set.add(listener)
    },
    close: () => {
      detach()
      void stop()
    }
  }

  const off = onEvent((message) => {
    if (message.kind === 'open') stream.onopen?.(new Event('open'))
    else if (message.kind === 'error') stream.onerror?.(new Event('error'))
    else emit(message.type, message.data)
  })
  if (typeof off === 'function') detach = off

  void start()
  return stream
}

/**
 * 把服务端证书指纹登记为可信（TOFU 首次信任）。
 *
 * 为什么必须有这一步：服务端使用自建 CA，Chromium 会以"证书链不可信"拒绝它的所有请求
 * （等价于 curl 的 SEC_E_UNTRUSTED_ROOT），表现为"测试连接成功，但保存后什么都做不了"。
 * 指纹来自本地代理的连通性探测（该探测有意跳过链校验，只回传指纹供人工核对），
 * 用户在配置页确认服务端名称/指纹后保存，即完成首次信任。
 *
 * 主进程只放行"实际证书指纹 == 已登记指纹"的自签证书，不是无差别忽略证书错误。
 * 浏览器 dev 环境（无 preload）下不做任何事，返回 false。
 */
export async function trustServerCertificate(serverUrl: string, sha256?: string): Promise<boolean> {
  const trust = hostBridge()?.trustCertificate
  const fingerprint = (sha256 ?? '').trim().toLowerCase()
  if (!trust || !/^[0-9a-f]{64}$/.test(fingerprint)) return false
  try {
    return await trust(serverUrl, fingerprint)
  } catch {
    return false
  }
}

/**
 * 立即重启客户端，让已激活的渲染层热更资源生效。
 *
 * 只有 Electron（preload）提供该能力；浏览器 dev 下静默忽略（无法重启进程）。
 */
export async function relaunchClient(): Promise<void> {
  const relaunch = hostBridge()?.relaunch
  if (!relaunch) return
  await relaunch()
}

/**
 * 让刚激活的热更渲染层立刻生效（静默热更的最后一环，无需重启客户端）。
 *
 * 只在 Electron（preload 提供该能力）下有效；浏览器 dev 静默返回 applied=false。
 * 主进程按"入口文件是否已加载"去重：同文件返回 applied=false，因此可放心重复调用。
 */
export async function applyWebLayer(): Promise<{ applied: boolean; version?: string }> {
  const apply = hostBridge()?.applyWebLayer
  if (!apply) return { applied: false }
  try {
    const result = await apply()
    return { applied: result.applied, version: result.version }
  } catch {
    // 切换失败不影响使用：当前界面继续可用，下次启动会走正常加载路径。
    return { applied: false }
  }
}

/** 当前环境是否提供系统目录选择（Electron preload 才提供）。 */
export function supportsDirectoryPicker(): boolean {
  return typeof hostBridge()?.selectDirectory === 'function'
}

/**
 * 打开系统目录选择框。
 *
 * 非 Electron 环境（浏览器 dev / 测试）返回 null，调用方据此降级为手动输入路径。
 */
export async function selectDirectory(): Promise<DirectorySelection | null> {
  const select = hostBridge()?.selectDirectory
  if (!select) return null
  return select()
}

/** 规范化服务端地址（去空白、去尾部斜杠、主机名大小写不敏感），用于身份归属比较。 */
function normalizeServerUrl(url: string | undefined): string {
  return (url ?? '').trim().replace(/\/+$/, '').toLowerCase()
}

/**
 * 本地身份是否绑定到给定服务端。
 *
 * instance_id 双方都有时以它为准（服务端换地址也算同一台）；否则回退规范化地址比较。
 */
export function identityMatchesServer(
  identity: Pick<AgentIdentity, 'server_url' | 'server_instance_id'>,
  server: { baseUrl: string; instanceId?: string }
): boolean {
  if (identity.server_instance_id && server.instanceId) {
    return identity.server_instance_id === server.instanceId
  }
  const left = normalizeServerUrl(identity.server_url)
  return left !== '' && left === normalizeServerUrl(server.baseUrl)
}

export const agentApi = {
  /** 探测代理是否可用（不会抛 AgentUnavailableError）。 */
  async probe(): Promise<boolean> {
    return (await resolveTransport()) !== null
  },

  health(): Promise<AgentHealth> {
    return request<AgentHealth>({ method: 'GET', path: '/agent/health' })
  },

  state(): Promise<AgentState> {
    return request<AgentState>({ method: 'GET', path: '/agent/state' })
  },

  /**
   * 读取代理日志（日志模块）。日志按天切分：`day` 指定日期（省略即当天），
   * `tailBytes` 限制读取的文件末尾字节数（省略由代理取默认 256KiB）。
   */
  log(options?: { day?: string; tailBytes?: number }): Promise<AgentLog> {
    const query = new URLSearchParams()
    if (options?.day) query.set('day', options.day)
    if (options?.tailBytes && options.tailBytes > 0) query.set('tail', String(options.tailBytes))
    const suffix = query.toString()
    return request<AgentLog>({ method: 'GET', path: `/agent/log${suffix ? `?${suffix}` : ''}` })
  },

  mount(input: {
    allocation_id: string
    mount_mode?: AgentMountMode
    mount_path?: string
    repo_id?: string
    repo_name?: string
  }): Promise<{ mount: AgentMountState }> {
    return request({ method: 'POST', path: '/agent/mount', body: input, timeoutMs: MOUNT_TIMEOUT_MS })
  },

  unmount(input: { allocation_id: string }): Promise<{ ok: true }> {
    return request({ method: 'POST', path: '/agent/unmount', body: input, timeoutMs: MOUNT_TIMEOUT_MS })
  },

  remount(input: { allocation_id: string }): Promise<{ mount: AgentMountState }> {
    return request({ method: 'POST', path: '/agent/remount', body: input, timeoutMs: MOUNT_TIMEOUT_MS })
  },

  /** 把母盘拷贝到本地：202 立即返回；同一磁盘已在下载时返回既有任务。 */
  startDiskDownload(input: {
    disk_id: string
    target_dir?: string
    file_name?: string
    server_url?: string
  }): Promise<{ download: DownloadState }> {
    return request({ method: 'POST', path: '/agent/disks/download', body: input })
  },

  /** 列出下载任务（按开始时间倒序）。 */
  getDownloads(): Promise<{ items: DownloadState[] }> {
    return request({ method: 'GET', path: '/agent/downloads' })
  },

  /** 取消下载（幂等；取消进行中的任务）。 */
  cancelDownload(diskId: string): Promise<{ ok: true }> {
    return request({ method: 'DELETE', path: `/agent/downloads/${encodeURIComponent(diskId)}` })
  },

  /** 扫描本地目录（文件数 / 大小 / 跳过项）；目录过大或为空时返回明确错误。 */
  scanLocalDir(path: string): Promise<LocalScanResult> {
    return request<LocalScanResult>({ method: 'POST', path: '/agent/fs/scan', body: { path }, timeoutMs: 120000 })
  },

  /** 启动本地目录上传：202 立即返回初始状态；进度经 upload 事件推送。 */
  startUpload(input: StartUploadInput): Promise<{ upload: UploadState }> {
    return request<{ upload: UploadState }>({ method: 'POST', path: '/agent/uploads/start', body: input })
  },

  /** 列出上传任务（页面刷新后据此恢复进度展示）。 */
  getUploads(): Promise<{ items: UploadState[] }> {
    return request<{ items: UploadState[] }>({ method: 'GET', path: '/agent/uploads' })
  },

  /** 取消上传（保留已传分块，可续传）。 */
  cancelUpload(uploadId: string): Promise<{ ok: true }> {
    return request({ method: 'DELETE', path: `/agent/uploads/${encodeURIComponent(uploadId)}` })
  },

  pushSession(input: ServerSession): Promise<{ ok: true }> {
    return request({ method: 'POST', path: '/agent/session', body: input })
  },

  clearSession(): Promise<{ ok: true }> {
    return request({ method: 'DELETE', path: '/agent/session' })
  },

  testServer(url: string): Promise<ServerProbe> {
    return request({ method: 'POST', path: '/agent/server/test', body: { url } })
  },

  /**
   * 手动重连服务端（界面上的"重试"按钮）。
   *
   * 代理会清零自动重连的失败计数并立即重试一次 —— 即"手动重试会刷新计数"：
   * 之后自动重连循环重新获得 maxServerConnectAttempts 次机会。
   */
  reconnectServer(): Promise<AgentServerState> {
    return request({ method: 'POST', path: '/agent/server/reconnect' })
  },

  getConfig(): Promise<AgentConfig> {
    return request<AgentConfig>({ method: 'GET', path: '/agent/config' })
  },

  patchConfig(patch: Partial<AgentConfig>): Promise<{ config: AgentConfig }> {
    return request({ method: 'PATCH', path: '/agent/config', body: patch })
  },

  /**
   * 写入**单个存储库**的挂载偏好（形态 / 目录 / 启动后自动挂载）。
   *
   * 单独一个接口而不是走 PATCH /agent/config 的整表替换：每个库的配置互相独立，
   * 整表替换会让"两个窗口各改一个库"变成后写覆盖前写。
   */
  setRepoMountPref(repoId: string, pref: AgentRepoMountPref): Promise<{ config: AgentConfig }> {
    return request({
      // 必须是 POST 而不是 PUT：Electron 主进程会按方法放行（只接受 GET/POST/PATCH/DELETE，
      // 见 apps/client/electron/main.ts 的 parseAgentRequest），PUT 会被它挡在代理之外，
      // 界面上只会看到 network.error（"无法连接服务端"），与实际原因完全不符。
      method: 'POST',
      path: `/agent/repo-mounts/${encodeURIComponent(repoId)}`,
      body: pref
    })
  },

  /** 读取本地客户端证书身份（未安装时仅 installed:false；绝不含私钥/证书原文）。 */
  getIdentity(): Promise<AgentIdentity> {
    return request<AgentIdentity>({ method: 'GET', path: '/agent/identity' })
  },

  /** 安装本地客户端身份：代理本地生成密钥对，用当前会话向服务端换取证书并落盘。 */
  installIdentity(input: AgentIdentityInstallInput): Promise<AgentIdentity> {
    return request<AgentIdentity>({ method: 'POST', path: '/agent/identity/install', body: input })
  },

  /** 移除本地身份（幂等）。 */
  removeIdentity(): Promise<{ ok: true }> {
    return request({ method: 'DELETE', path: '/agent/identity' })
  },

  /** 用本地证书免密登录；代理已把会话写回本地状态，此处只透出服务端响应。 */
  identityLogin(input?: { server_url?: string }): Promise<AgentIdentityLoginResponse> {
    return request({ method: 'POST', path: '/agent/identity/login', body: input ?? {} })
  },

  checkUpdate(): Promise<UpdateInfo> {
    return request<UpdateInfo>({ method: 'POST', path: '/agent/update/check', timeoutMs: 60000 })
  },

  applyUpdate(): Promise<{ started: true }> {
    return request({ method: 'POST', path: '/agent/update/apply', timeoutMs: 60000 })
  },

  /** 客户端资源热更：202 立即返回初始状态；后续进度经 web_update 事件推送。 */
  applyWebUpdate(): Promise<{ web_update: WebUpdateState }> {
    return request({ method: 'POST', path: '/agent/update/web/apply', timeoutMs: 60000 })
  },

  /** 读取客户端资源热更状态（页面刷新后据此恢复展示）。 */
  getWebUpdate(): Promise<{ web_update: WebUpdateState }> {
    return request({ method: 'GET', path: '/agent/update/web' })
  },

  shutdown(): Promise<{ ok: true }> {
    return request({ method: 'POST', path: '/agent/shutdown' })
  },

  /**
   * 订阅事件流。
   *
   * Electron：由主进程建立 SSE 长连接并把事件推给渲染进程（令牌不出主进程，
   * 主进程经 `X-Vault-Agent-Token` 请求头携带令牌）；
   * 浏览器 dev：直连 EventSource，令牌走查询参数（EventSource 无法自定义请求头）。
   * 需先完成一次探针调用（resolveTransport），否则同步抛出 AgentUnavailableError。
   */
  subscribe(): AgentEventStream {
    const host = hostBridge()
    const start = host?.agentEventsStart
    const stop = host?.agentEventsStop
    const onEvent = host?.onAgentEvent
    if (start && stop && onEvent) return ipcEventStream(start, stop, onEvent)
    if (!transport || transport.kind !== 'http') throw new AgentUnavailableError()
    // ⚠️ 浏览器回退分支不支持 SSE 鉴权：代理的 tokenMiddleware 只认 `X-Vault-Agent-Token`
    // 请求头，而 EventSource 无法设置请求头，因此本分支恒返回 401、事件通道不可用。
    // 仅 Electron 主进程通道（ipcEventStream）才能真正建立事件流，此处保留只为开发环境不崩溃。
    const query = new URLSearchParams({ token: transport.token })
    return new EventSource(`${transport.baseUrl}/agent/events?${query.toString()}`)
  },

  /** 打开挂载目录（Electron 下走 shell.openPath）。 */
  async openPath(path: string): Promise<void> {
    const host = hostBridge()
    if (!host?.openPath) throw new AgentUnavailableError()
    await host.openPath(path)
  },

  /** 在文件管理器中定位（Electron 下走 shell.showItemInFolder）。 */
  async showItemInFolder(path: string): Promise<void> {
    const host = hostBridge()
    if (!host?.showItemInFolder) throw new AgentUnavailableError()
    await host.showItemInFolder(path)
  }
}

/** 解析一条 SSE 事件（无效数据返回 null，不抛错）。 */
export function parseAgentEvent(type: string, data: string): AgentEvent | null {
  let raw: unknown
  try {
    raw = JSON.parse(data) as unknown
  } catch {
    return null
  }
  if (typeof raw !== 'object' || raw === null) return null
  const value = raw as Record<string, unknown>
  switch (type) {
    case 'mount':
      return { type: 'mount', mount: raw as AgentMountState }
    case 'unmount':
      return { type: 'unmount', allocation_id: String(value.allocation_id ?? '') }
    case 'revoked':
      return {
        type: 'revoked',
        allocation_id: String(value.allocation_id ?? ''),
        reason: typeof value.reason === 'string' ? value.reason : undefined
      }
    case 'server':
      return {
        type: 'server',
        connected: value.connected === true,
        // phase / fail_count 决定顶栏标签显示"连接中"还是"未连接 + 重试"（见 agentTypes.ts）。
        phase:
          value.phase === 'connecting' || value.phase === 'connected' || value.phase === 'disconnected'
            ? value.phase
            : undefined,
        fail_count: typeof value.fail_count === 'number' ? value.fail_count : undefined,
        last_error: typeof value.last_error === 'string' ? value.last_error : undefined
      }
    case 'host':
      // 本机就绪状态（iSCSI 发起端）：启动探测完成或状态变化时推送。
      return { type: 'host', host: raw as AgentHostState }
    case 'session':
      return { type: 'session', session: raw as AgentSessionState }
    case 'update':
      return {
        type: 'update',
        available_version: typeof value.available_version === 'string' ? value.available_version : undefined,
        downloading: value.downloading === true,
        received_bytes: typeof value.received_bytes === 'number' ? value.received_bytes : 0,
        total_bytes: typeof value.total_bytes === 'number' ? value.total_bytes : 0
      }
    case 'web_update':
      return { type: 'web_update', web_update: raw as WebUpdateState }
    case 'download':
      return { type: 'download', download: raw as DownloadState }
    case 'upload':
      return { type: 'upload', upload: raw as UploadState }
    case 'heartbeat':
      return { type: 'heartbeat', at: typeof value.at === 'number' ? value.at : Date.now() }
    default:
      return null
  }
}

// 代理重启后端口/令牌会变：主进程通知后刷新端点并广播给订阅者。
if (typeof window !== 'undefined') {
  hostBridge()?.onAgentChanged?.((info) => applyAgentInfo(info))
}
