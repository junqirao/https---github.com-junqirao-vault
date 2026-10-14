import { useCallback, useEffect, useSyncExternalStore } from 'react'
import { App } from 'antd'

import {
  agentApi,
  applyWebLayer,
  getAgentVersionInfo,
  isAgentUnavailableError,
  onAgentEndpointChanged,
  parseAgentEvent,
  subscribeAgentVersion,
  type AgentEventStream,
  type AgentVersionInfo
} from '../api/agentClient'
import type {
  AgentCacheStatus,
  AgentConfig,
  AgentEvent,
  AgentHealth,
  AgentMountMode,
  AgentMountState,
  AgentMountVolumeUsage,
  AgentServerState,
  AgentSessionState,
  AgentState,
  DownloadState,
  UploadState,
  WebUpdateState
} from '../api/agentTypes'
import { t } from '../i18n'

/** 挂载相关操作入参。 */
export interface AgentMountInput {
  allocation_id: string
  mount_mode?: AgentMountMode
  mount_path?: string
  /** 可选：随挂载状态落库用于展示与脚本变量注入（挂载参数本身由服务端下发）。 */
  repo_id?: string
  repo_name?: string
  /**
   * 归属服务端（多服务端下二选一必填）：省略时按"本机既有记录 → 主服务端"兜底。
   *
   * 分配 ID 只对"它所属的那台服务端"有意义，给错台会 404 agent.server_unknown。
   */
  server_key?: string
  server_url?: string
  server_name?: string
}

/** 卸载入参。没有 force：代理侧只剩一种卸载语义（一次尽力清理干净）。 */
export interface AgentUnmountInput {
  allocation_id: string
}

export interface UseAgentResult {
  /** 代理是否可用（未找到/未启动时为 false）。 */
  available: boolean
  state: AgentState | null
  health: AgentHealth | null
  config: AgentConfig | null
  /** 本地读缓存代理状态（来自 GET /agent/cache）；代理不可用时为 null。 */
  cache: AgentCacheStatus | null
  /**
   * 本机**挂载卷的真实用量**（来自 GET /agent/usage）；没有本机挂载/代理不可用时为空。
   *
   * 服务端的「已用」是已分配磁盘总量（按盘标称容量计费，等于库容量），
   * 卡片要显示"用户眼里的用了多少"只能靠这个本机实测值（见 pickMountUsage）。
   */
  usage: AgentMountVolumeUsage[]
  /**
   * 本机实测用量**正在取数**的键（分配 ID 与库 ID，判定见 repoUsageLoading）。
   *
   * 挂载刚成功时，代理那边卷往往还没就绪、量不出数字，而用户已经在等"用了多少"了。这个
   * 窗口里界面显示 loading 而不是 `-`（见 components/UsageBar 的 loading）。
   */
  usagePending: string[]
  /** 代理不可用时的错误；其余失败由调用方按需展示。 */
  error: unknown
  /** 母盘下载任务（来自 download 事件与 GET /agent/downloads）。 */
  downloads: DownloadState[]
  /** 本地目录上传任务（来自 upload 事件与 GET /agent/uploads）。 */
  uploads: UploadState[]
  /** 客户端资源热更状态（来自 web_update 事件与 GET /agent/update/web）。 */
  webUpdate: WebUpdateState | null
  mount: (input: AgentMountInput) => Promise<AgentMountState>
  unmount: (input: AgentUnmountInput) => Promise<void>
  remount: (input: { allocation_id: string }) => Promise<AgentMountState>
  /** 取消一个**进行中**的挂载（幂等；仅挂载中有效）。 */
  cancelMount: (input: { allocation_id: string }) => Promise<void>
  /**
   * 手动重连服务端（顶栏"未连接"旁的"重试"按钮）。
   *
   * 代理会清零自动重连的失败计数并立即重试一次：成功后自动重连循环重新获得
   * maxServerConnectAttempts 次机会（对应"手动重试会刷新计数"）。
   */
  reconnectServer: (serverKey?: string) => Promise<AgentServerState>
  refresh: () => Promise<void>
  /** 拉取一次本地读缓存代理状态（用量会随读写变化，由界面按需轮询）。 */
  refreshCache: () => Promise<void>
  /** 拉取一次本机挂载卷用量（随客户机读写变化，由页面按需轮询）。 */
  refreshUsage: () => Promise<void>
  /** 拉取一次下载任务列表（用于页面挂载时恢复进行中/历史状态）。 */
  refreshDownloads: () => Promise<void>
  /** 拉取一次上传任务列表（用于页面挂载时恢复进行中/历史状态）。 */
  refreshUploads: () => Promise<void>
  /** 触发客户端资源热更（202 立即返回；进度经 web_update 事件推送）。 */
  applyWebUpdate: () => Promise<WebUpdateState>
}

/**
 * 模块级单例：整个应用只建立一条 SSE 连接、只持有一份代理状态。
 *
 * 多个组件调用 useAgent() 共享同一份快照，避免重复订阅与重复提示。
 */
let snapshot: {
  available: boolean
  state: AgentState | null
  health: AgentHealth | null
  config: AgentConfig | null
  cache: AgentCacheStatus | null
  usage: AgentMountVolumeUsage[]
  usagePending: string[]
  downloads: DownloadState[]
  uploads: UploadState[]
  webUpdate: WebUpdateState | null
  error: unknown
} = {
  available: false,
  state: null,
  health: null,
  config: null,
  cache: null,
  usage: [],
  usagePending: [],
  downloads: [],
  uploads: [],
  webUpdate: null,
  error: null
}

const snapshotListeners = new Set<() => void>()
// 事件通知出口。⚠️ 全局只应注册**一个**（见 useAgentEventNotifier）：
// 每个出口都会把一条事件弹成一条提示，注册 N 个就弹 N 条重复提示（真实反馈：
// "不管什么事件都会弹出来两个或者三个"——因为每个 useAgent 组件都注册了一个）。
const notifiers = new Set<(text: string) => void>()
/** 会话更新订阅者：宿主应用据此把新令牌同步到自己的会话存储与 API 客户端。 */
const sessionListeners = new Set<(session: AgentSessionState) => void>()

/**
 * 订阅"代理侧会话更新"（GET /agent/state 与 session 事件）。
 *
 * 用途：代理用客户端证书自动续期后会广播 session 事件，宿主应用据此把最新令牌
 * 同步到活动服务端会话（否则后续请求仍带旧令牌而 401）。
 */
export function subscribeAgentSession(listener: (session: AgentSessionState) => void): () => void {
  sessionListeners.add(listener)
  return () => {
    sessionListeners.delete(listener)
  }
}

function publishSession(session: AgentSessionState): void {
  for (const listener of sessionListeners) listener(session)
}

let source: AgentEventStream | null = null
let reconnectTimer: ReturnType<typeof setTimeout> | null = null
let retryDelay = 1000
let started = false

function setSnapshot(patch: Partial<typeof snapshot>): void {
  snapshot = { ...snapshot, ...patch }
  for (const listener of snapshotListeners) listener()
}

function subscribeSnapshot(listener: () => void): () => void {
  snapshotListeners.add(listener)
  return () => {
    snapshotListeners.delete(listener)
  }
}

function getSnapshot(): typeof snapshot {
  return snapshot
}

function notify(text: string): void {
  for (const notifier of notifiers) notifier(text)
}

/** 拉取健康状态 / 全量状态 / 本地配置（代理不可用时收敛为 available:false）。 */
async function probe(): Promise<void> {
  try {
    const health = await agentApi.health()
    let state: AgentState | null = null
    let config: AgentConfig | null = null
    try {
      state = await agentApi.state()
    } catch {
      state = null
    }
    try {
      config = await agentApi.getConfig()
    } catch {
      config = null
    }
    // 客户端资源热更状态：页面刷新后据此恢复进度展示。
    let webUpdate: WebUpdateState | null = null
    try {
      webUpdate = (await agentApi.getWebUpdate()).web_update
    } catch {
      webUpdate = null
    }
    // 本地目录上传任务：页面刷新后据此恢复进度展示。
    let uploads: UploadState[] = []
    try {
      uploads = (await agentApi.getUploads()).items
    } catch {
      uploads = []
    }
    // 本地读缓存代理状态（用量与命中情况）：失败不影响代理可用性判断。
    let cache: AgentCacheStatus | null = null
    try {
      cache = await agentApi.getCache()
    } catch {
      cache = null
    }
    // 本机挂载卷的**真实**用量：失败按"没有实测值"处理（界面回退服务端数字）。
    let usage: AgentMountVolumeUsage[] = []
    try {
      usage = (await agentApi.getUsage()).items
    } catch {
      usage = []
    }
    setSnapshot({ available: true, health, state, config, cache, usage, webUpdate, uploads, error: null })
    // 代理侧已有会话（含自动续期后的最新令牌）：**逐台**同步给宿主应用 ——
    // 快照里的 session 只有主服务端那一份，多服务端下必须遍历 servers[] 才能都拿到。
    //
    // 嵌套会话不带自己的 server_key（父条目就是归属，见 docs/agent-api.md 的 AgentServerState），
    // 这里补上：宿主应用靠它把令牌写回**对应那一台**，缺了就会全部落到活动服务端名下。
    const sessions: AgentSessionState[] = []
    for (const item of state?.servers ?? []) {
      if (!item.session) continue
      const key = item.session.server_key ?? item.server_key
      sessions.push(key ? { ...item.session, server_key: key } : item.session)
    }
    if (sessions.length > 0) {
      for (const session of sessions) publishSession(session)
    } else if (state?.session) {
      // 旧版代理不带 servers[]：退回单服务端的兼容字段。
      publishSession(state.session)
    }
  } catch (error) {
    if (isAgentUnavailableError(error)) {
      // 代理不在运行，其内存态下载/上传任务与热更状态一并消失，清空避免展示陈旧进度。
      setSnapshot({
        available: false,
        health: null,
        state: null,
        config: null,
        cache: null,
        usage: [],
        // 量取用量的也是代理：它不在了就没人"正在取数"，转圈跟着停。
        usagePending: [],
        downloads: [],
        uploads: [],
        webUpdate: null,
        error
      })
    } else {
      // 代理在运行但本次调用失败（如未登录会话）：仍视为可用，保留错误供排障展示。
      setSnapshot({ available: true, error })
    }
  }
}

function upsertMount(mounts: AgentMountState[], next: AgentMountState): AgentMountState[] {
  const index = mounts.findIndex((item) => item.allocation_id === next.allocation_id)
  if (index < 0) return [...mounts, next]
  const copy = mounts.slice()
  copy[index] = next
  return copy
}

function upsertDownload(downloads: DownloadState[], next: DownloadState): DownloadState[] {
  const index = downloads.findIndex((item) => item.disk_id === next.disk_id)
  if (index < 0) return [next, ...downloads]
  const copy = downloads.slice()
  copy[index] = next
  return copy
}

/** 进行中的上传状态（与代理侧的 isActiveUploadState 对齐）。 */
const ACTIVE_UPLOAD_STATES: ReadonlyArray<UploadState['state']> = [
  'scanning',
  'creating',
  'uploading',
  'completing'
]

function isActiveUpload(upload: UploadState): boolean {
  return ACTIVE_UPLOAD_STATES.includes(upload.state)
}

/**
 * 合并/插入一条上传任务。
 *
 * 会话尚未创建时 upload_id 为空（见 internal/agent/upload.go 的初始 upload 事件与 202 响应）：
 * 此时按 local_dir 与进行中条目合并，避免留下"空 upload_id 影子条目"；
 * 收到真实 upload_id 且存在同 local_dir 的空 id 条目时，用它**替换**影子条目。
 */
function upsertUpload(uploads: UploadState[], next: UploadState): UploadState[] {
  if (next.upload_id === '') {
    const index = uploads.findIndex(
      (item) => item.upload_id === '' && item.local_dir === next.local_dir && isActiveUpload(item)
    )
    if (index < 0) return [next, ...uploads]
    const copy = uploads.slice()
    copy[index] = { ...copy[index], ...next }
    return copy
  }

  const index = uploads.findIndex((item) => item.upload_id === next.upload_id)
  const shadowIndex = uploads.findIndex(
    (item) => item.upload_id === '' && item.local_dir === next.local_dir
  )
  if (index >= 0) {
    const copy = uploads.slice()
    copy[index] = next
    if (shadowIndex >= 0) copy.splice(shadowIndex, 1)
    return copy
  }
  if (shadowIndex >= 0) {
    const copy = uploads.slice()
    copy[shadowIndex] = next
    return copy
  }
  return [next, ...uploads]
}

function mountName(allocationId: string): string {
  const found = snapshot.state?.mounts.find((item) => item.allocation_id === allocationId)
  return found?.repo_name || allocationId
}

/**
 * 主服务端的 server_key（缺失时回退到兼容字段，再回退到数组首项——代理保证主服务端在前）。
 *
 * 用于把不带 server_key 的事件（旧版代理）归到正确的那一台。
 */
function primaryServerKey(state: AgentState | null): string | undefined {
  if (!state) return undefined
  return state.primary_key ?? state.server?.server_key ?? state.servers?.[0]?.server_key
}

/** 解析一条事件归属的服务端键；事件不带键时归给主服务端（旧版代理语义）。 */
function resolveEventServerKey(state: AgentState | null, eventKey?: string): string | undefined {
  if (eventKey) return eventKey
  return primaryServerKey(state)
}

/**
 * 这条归属键是否就是"主服务端那一台"（兼容字段 `server` 只反映它）。
 *
 * 键不可解析时返回 true：那就是旧版代理 / 单服务端的情形，兼容字段是界面的唯一来源，
 * 不更新它顶栏就会冻在初始值上。
 */
function isPrimaryServerKey(state: AgentState | null, key: string | undefined): boolean {
  if (key === undefined) return true
  return key === primaryServerKey(state)
}

/**
 * 把一条 server 事件**只**应用到它那一台（旧版代理不带 server_key 时归给主服务端）。
 *
 * 为什么必须逐台：两台服务端各发各的 server 事件，一律写 compat `server` 会让它们互相覆盖
 * —— 界面于是在两台之间来回闪。
 */
function applyServerEvent(current: AgentState, event: Extract<AgentEvent, { type: 'server' }>): AgentState {
  const key = resolveEventServerKey(current, event.server_key)
  const previous = (current.servers ?? []).find((item) => item.server_key === key) ?? current.server
  const next: AgentServerState = {
    ...previous,
    connected: event.connected,
    // 旧版代理不带这两个字段：缺省时保留原值，别把"连接中"抹成未知状态。
    phase: event.phase ?? previous.phase,
    fail_count: event.fail_count ?? previous.fail_count,
    last_error: event.last_error
  }
  const servers = (current.servers ?? []).map((item) => (item.server_key === key ? next : item))
  // 兼容字段 server 一律指主服务端：只有这条事件属于主服务端时才更新它。
  const isPrimary = isPrimaryServerKey(current, key)
  const server = isPrimary
    ? {
        ...current.server,
        connected: next.connected,
        phase: next.phase,
        fail_count: next.fail_count,
        last_error: next.last_error
      }
    : current.server
  return { ...current, servers, server }
}

function applyEvent(event: AgentEvent): void {
  const current = snapshot.state
  switch (event.type) {
    case 'mount': {
      // 挂载过程中 `mount` 事件会**多次**推送（阶段推进：requesting → allocating →
      // preparing_disk → configuring_target → connecting → … → mounted），
      // 因此只在"状态真正变为 mounted"时提示一次。
      //
      // 否则一次挂载会刷出一串"已挂载"提示 —— 而它可能最终根本没成功
      // （真实反馈："客户端挂载时会出现一大批已挂载的提示，但并未挂载成功"）。
      //
      // 同理，`unmounting` 收到 mounted 是**卸载失败后的状态恢复**（磁盘其实还挂着，只是
      // 移除挂载点这一步失败，见 docs/implementation.md 5.5），不是"刚挂上"，也不能弹提示 ——
      // 真实反馈："点了卸载，然后提示挂载成功？"
      //
      // 再同理，**会话实测**也会推 mount 事件（会话断开/恢复时刷新 session_active，state 仍是
      // mounted，见 internal/agent/session_probe.go）。这类事件与"刚挂上"无关，所以要求
      // `previous` 必须存在：连"之前是不是 mounted"都不知道（尚未拿到这条记录）就没有
      // 任何理由弹"已挂载"。真正的挂载流程一定先经过 mounting 状态，不会因此漏提示。
      const previous = current?.mounts.find((item) => item.allocation_id === event.mount.allocation_id)
      if (current) setSnapshot({ state: { ...current, mounts: upsertMount(current.mounts, event.mount) } })
      if (
        previous !== undefined &&
        event.mount.state === 'mounted' &&
        previous.state !== 'mounted' &&
        previous.state !== 'unmounting'
      ) {
        notify(t('agent.event.mounted', { name: event.mount.repo_name || event.mount.allocation_id }))
      }
      return
    }
    case 'unmount': {
      // 只有"原先确实挂着东西"才提示已卸载：挂载失败后的清理也会发 unmount（代理会抹掉
      // 失败记录，见 mountEngine.mount），此时提示"已卸载"会让人以为刚才挂载成功过。
      const previous = current?.mounts.find((item) => item.allocation_id === event.allocation_id)
      const name = mountName(event.allocation_id)
      if (current) {
        setSnapshot({
          state: { ...current, mounts: current.mounts.filter((item) => item.allocation_id !== event.allocation_id) }
        })
      }
      if (previous?.state === 'mounted' || previous?.state === 'unmounting') {
        notify(t('agent.event.unmounted', { name }))
      }
      return
    }
    case 'revoked': {
      const name = mountName(event.allocation_id)
      if (current) {
        setSnapshot({
          state: { ...current, mounts: current.mounts.filter((item) => item.allocation_id !== event.allocation_id) }
        })
      }
      notify(
        event.reason
          ? t('agent.event.revoked', { name, reason: event.reason })
          : t('agent.event.revokedNoReason', { name })
      )
      return
    }
    case 'server': {
      const key = resolveEventServerKey(current, event.server_key)
      // 连上不弹提示：多服务端会同时连上，每台都弹就成了一串通知，顶栏状态本身就是提示。
      // "断开"仍是用户事件，保留提示；phase 变化（connecting → disconnected）不算翻转，
      // 否则自动重连也会被刷成一串通知。
      const wasConnected =
        ((current?.servers ?? []).find((item) => item.server_key === key) ?? current?.server)?.connected ?? false
      if (current) setSnapshot({ state: applyServerEvent(current, event) })
      if (wasConnected && !event.connected) {
        notify(t('agent.event.serverDisconnected'))
      }
      return
    }
    case 'session': {
      // 代理侧自动续期成功：按 server_key 更新**对应那一台**的会话并通知宿主应用切换令牌。
      if (current) {
        const key = resolveEventServerKey(current, event.session.server_key)
        const isPrimary = isPrimaryServerKey(current, key)
        const servers = (current.servers ?? []).map((item) =>
          item.server_key === key ? { ...item, session: event.session } : item
        )
        setSnapshot({
          state: { ...current, servers, session: isPrimary ? event.session : current.session }
        })
      }
      publishSession(event.session)
      return
    }
    case 'update': {
      if (current) {
        setSnapshot({
          state: {
            ...current,
            update: {
              ...current.update,
              available_version: event.available_version,
              downloading: event.downloading,
              received_bytes: event.received_bytes,
              total_bytes: event.total_bytes
            }
          }
        })
      }
      return
    }
    case 'web_update': {
      setSnapshot({ webUpdate: event.web_update })
      // 静默热更：代理刚激活新的渲染层资源 → 立刻切换，无需用户手动点"立即重启"。
      // 主进程按"解析出的入口文件是否等于已加载文件"去重（同文件返回 applied=false），
      // 因此这里可以放心重复调用，不会造成重载循环。
      if (event.web_update.state === 'activated') {
        void applyWebLayer().then((result) => {
          if (result.applied) {
            console.info(`[vault] 已自动应用热更渲染层：${result.version ?? ''}`)
          }
        })
      }
      return
    }
    case 'download': {
      setSnapshot({ downloads: upsertDownload(snapshot.downloads, event.download) })
      return
    }
    case 'upload': {
      setSnapshot({ uploads: upsertUpload(snapshot.uploads, event.upload) })
      return
    }
    case 'host': {
      // 本机就绪状态（iSCSI 发起端）：代理启动探测完成或状态变化时推送。
      // 界面据此挂/撤横幅，**无需重启客户端**（例如管理员刚执行了 Start-Service MSiSCSI）。
      if (current) setSnapshot({ state: { ...current, host: event.host } })
      return
    }
    case 'heartbeat':
      return
  }
}

function handleEvent(type: string, event: Event): void {
  const data = (event as MessageEvent<string>).data
  const parsed = parseAgentEvent(type, data)
  if (parsed) applyEvent(parsed)
}

const EVENT_TYPES = [
  'mount',
  'unmount',
  'revoked',
  'server',
  'host',
  'session',
  'update',
  'web_update',
  'download',
  'upload',
  'heartbeat'
] as const

function closeSource(): void {
  if (source) {
    source.close()
    source = null
  }
  if (reconnectTimer) {
    clearTimeout(reconnectTimer)
    reconnectTimer = null
  }
}

/** 指数退避重连：代理不可用时也不刷屏，仅静默重试。 */
function scheduleReconnect(): void {
  if (reconnectTimer) return
  const delay = retryDelay
  retryDelay = Math.min(retryDelay * 2, 30000)
  reconnectTimer = setTimeout(() => {
    reconnectTimer = null
    void (async () => {
      await probe()
      connect()
    })()
  }, delay)
}

function connect(): void {
  closeSource()
  let next: AgentEventStream
  try {
    next = agentApi.subscribe()
  } catch {
    scheduleReconnect()
    return
  }
  source = next
  next.onopen = () => {
    retryDelay = 1000
  }
  next.onerror = () => {
    if (source === next) {
      next.close()
      source = null
    }
    scheduleReconnect()
  }
  for (const type of EVENT_TYPES) {
    next.addEventListener(type, (event) => handleEvent(type, event))
  }
}

function start(): void {
  if (started) return
  started = true
  // 代理重启后端口/令牌会变：重新探测并立即重建事件流。
  onAgentEndpointChanged(() => {
    retryDelay = 1000
    void (async () => {
      await probe()
      connect()
    })()
  })
  void (async () => {
    await probe()
    connect()
  })()
}

/**
 * 注册代理事件的**全局通知出口**（整个应用只调用一次，放应用外壳里）。
 *
 * 为什么不放进 useAgent()：通知是全局唯一的，而 useAgent 会被多个组件调用——
 * 每个组件注册一个出口，同一条事件就会弹出 N 条重复提示（真实反馈）。
 */
export function useAgentEventNotifier(): void {
  const { message } = App.useApp()

  useEffect(() => {
    const notifier = (text: string): void => {
      message.info(text)
    }
    notifiers.add(notifier)
    return () => {
      notifiers.delete(notifier)
    }
  }, [message])
}

/**
 * 从本机用量快照里挑出某个库的**实测**用量（客户端存储库卡片/详情用）。
 *
 * 快照里只会有**本机正挂着**的库，所以"挑到了"就等于"这块盘在本机挂着"；挑不到
 * （未挂载 / 代理不可用 / 取数失败）返回 undefined，此时客户端界面不报用量（写 `-`）、
 * 只报上限，进度条置灰（见 RepoCards 的 RepoUsage），只有管理端仍按服务端口径显示。
 */
export function pickMountUsage(
  items: AgentMountVolumeUsage[] | undefined,
  repoId?: string,
  allocationId?: string
): AgentMountVolumeUsage | undefined {
  // 取数失败的那一条（error 非空、total_bytes=0）不算实测值：它给不出"用了多少"。
  const usable = (item: AgentMountVolumeUsage): boolean => item.total_bytes > 0 && !item.error
  const hit = allocationId ? items?.find((item) => item.allocation_id === allocationId && usable(item)) : undefined
  if (hit) return hit
  if (!repoId) return undefined
  return items?.find((item) => item.repo_id === repoId && usable(item))
}

/**
 * 「这块库的实测用量正在取数」：刚挂上，代理那边还没量出来 —— 界面此刻显示 loading，
 * 而不是先摆一个 `-`（`-` 说的是"不知道"，这一刻其实是"正在读"，见 awaitMountUsage）。
 *
 * 已经拿到实测值就一律为 false：真数字优先，转圈绝不盖住它。
 */
export function repoUsageLoading(
  pending: string[] | undefined,
  repoId: string | undefined,
  measured: boolean
): boolean {
  if (measured || !repoId || !pending || pending.length === 0) return false
  return pending.includes(repoId)
}

/** 拉一次实测用量并落快照（单次失败保留上一份：等下一次轮询再试）。 */
async function refreshUsageSnapshot(): Promise<void> {
  try {
    const result = await agentApi.getUsage()
    setSnapshot({ usage: result.items })
  } catch {
    // 单次取数失败保留上一次的实测值（卸载后代理快照里就没有那一条了）。
  }
}

/** 追用量的小会儿：每 1s 问一次，最多 20s（挂载完成后的那十几秒够用了）。 */
const USAGE_CATCHUP_INTERVAL_MS = 1000
const USAGE_CATCHUP_TIMEOUT_MS = 20000

/** 标记 / 取消"正在追问用量"的键（分配 ID + 库 ID，界面按库 ID 查）。 */
function setUsagePending(keys: string[], pending: boolean): void {
  const current = snapshot.usagePending
  setSnapshot({
    usagePending: pending
      ? [...new Set([...current, ...keys])]
      : current.filter((item) => !keys.includes(item))
  })
}

/**
 * 在跑的"追问用量"任务：分配 ID → 任务。
 *
 * 卸载 / 取消挂载要能把对应那个停掉：否则盘已经摘了，卡片还在为一个永远不会到来的数字
 * 转圈，得干等到超时。
 */
const usageCatchups = new Map<string, { keys: string[]; cancelled: boolean }>()

/** 停掉某次追问（卸载 / 取消挂载）：界面立刻回到"不知道"（`-`）。 */
function stopUsageCatchup(allocationId: string): void {
  const task = usageCatchups.get(allocationId)
  if (!task) return
  task.cancelled = true
  usageCatchups.delete(allocationId)
  setUsagePending(task.keys, false)
}

/**
 * 挂载成功后追问这块卷的实测用量，直到量得出来（或超时）。
 *
 * 为什么要追（真实反馈："挂载磁盘后要马上更新容量信息"）：
 *   - 代理侧虽然会因挂载集合变化立刻重测（见 internal/agent/volusage.go 的 mountsKey），
 *     但卷刚就绪的那一两秒 Get-Volume 往往还量不到容量（那一条带着 error 回来、不缓存，
 *     下一拍才量得到）；
 *   - 而页面轮询是 10s 一跳：不追的话，用户点完挂载要盯着"用量怎么还没出来"十几秒。
 * 追问期间界面显示 loading（判定见 repoUsageLoading）：说的是"正在读"，而不是"读不到"。
 *
 * 超时就放手：剩下的交给页面那 10s 轮询，不再一直占着代理的 PowerShell。
 */
async function awaitMountUsage(allocationId: string): Promise<void> {
  // 库 ID 一起记：界面是按库 ID 查的（分配 ID 只有挂载/分配那几个组件手里才有）。
  const repoId = snapshot.state?.mounts.find((item) => item.allocation_id === allocationId)?.repo_id
  const keys = [allocationId, repoId].filter((item): item is string => typeof item === 'string' && item !== '')

  const task = { keys, cancelled: false }
  usageCatchups.set(allocationId, task)
  setUsagePending(keys, true)
  try {
    const deadline = Date.now() + USAGE_CATCHUP_TIMEOUT_MS
    while (!task.cancelled) {
      // 挂载请求返回前的那次 probe 可能已经把用量带回来了：先看再等，能省掉一次追问。
      if (pickMountUsage(snapshot.usage, repoId, allocationId) !== undefined) return
      if (Date.now() >= deadline) return
      await new Promise<void>((resolve) => {
        window.setTimeout(resolve, USAGE_CATCHUP_INTERVAL_MS)
      })
      if (task.cancelled) return
      await refreshUsageSnapshot()
    }
  } finally {
    // 只有自己还是当前任务时才收尾：重挂会为同一个分配另起一个任务（键一模一样），
    // 旧任务在这里把 pending 一清，新任务刚点亮的转圈就跟着灭了。
    // 被 stopUsageCatchup 停掉的那种（它已经删了表项、清了 pending）在这里什么都不用做。
    if (usageCatchups.get(allocationId) === task) {
      usageCatchups.delete(allocationId)
      setUsagePending(keys, false)
    }
  }
}

/**
 * 页面级轮询"本机挂载卷用量"（离开页面即停）。
 *
 * 与缓存状态同样的做法（见 RepoList 的说明）：轮询器放在页面这一层，放卡片里会变成
 * 每张卡一个。代理侧另有 15s 的取数缓存（见 internal/agent/volusage.go），所以按 10s
 * 问并不会每 10s 真去起一个 PowerShell。
 */
export function useAgentUsagePolling(enabled: boolean): void {
  const { available, refreshUsage } = useAgent()
  useEffect(() => {
    if (!available || !enabled) return
    void refreshUsage()
    const timer = window.setInterval(() => void refreshUsage(), 10000)
    return () => window.clearInterval(timer)
  }, [available, enabled, refreshUsage])
}

/** 本地代理状态与挂载操作（单例订阅 + SSE 实时更新）。 */
export function useAgent(): UseAgentResult {
  const current = useSyncExternalStore(subscribeSnapshot, getSnapshot, getSnapshot)

  useEffect(() => {
    start()
  }, [])

  const refresh = useCallback(async (): Promise<void> => {
    await probe()
  }, [])

  const refreshCache = useCallback(async (): Promise<void> => {
    try {
      setSnapshot({ cache: await agentApi.getCache() })
    } catch {
      // 单次取数失败不改判定：保留上一次的用量快照，等下次轮询再试。
    }
  }, [])

  const refreshUsage = useCallback(async (): Promise<void> => {
    await refreshUsageSnapshot()
  }, [])

  const refreshDownloads = useCallback(async (): Promise<void> => {
    const result = await agentApi.getDownloads()
    setSnapshot({ downloads: result.items })
  }, [])

  const refreshUploads = useCallback(async (): Promise<void> => {
    const result = await agentApi.getUploads()
    setSnapshot({ uploads: result.items })
  }, [])

  const applyWebUpdate = useCallback(async (): Promise<WebUpdateState> => {
    // 202 返回的初始状态（downloading）先落快照，随后由 web_update 事件持续更新。
    const result = await agentApi.applyWebUpdate()
    setSnapshot({ webUpdate: result.web_update })
    return result.web_update
  }, [])

  const mount = useCallback(
    async (input: AgentMountInput): Promise<AgentMountState> => {
      const result = await agentApi.mount(input)
      await refresh()
      // 挂上了就得马上看到用量：追问这块卷的实测值（界面这段时间显示 loading，
      // 见 awaitMountUsage）。不等它 —— 挂载按钮该立刻回到"卸载"。
      void awaitMountUsage(input.allocation_id)
      return result.mount
    },
    [refresh]
  )

  const unmount = useCallback(
    async (input: AgentUnmountInput): Promise<void> => {
      await agentApi.unmount(input)
      // 摘掉了就没有"用量正在路上"这回事了：刚挂上那次追问立刻收工，
      // 别让卡片为一个永远不会到来的数字转圈（见 stopUsageCatchup）。
      stopUsageCatchup(input.allocation_id)
      await refresh()
    },
    [refresh]
  )

  const remount = useCallback(
    async (input: { allocation_id: string }): Promise<AgentMountState> => {
      const result = await agentApi.remount(input)
      await refresh()
      // 重挂同样是一块"刚就绪"的卷：用量也马上跟上（见 awaitMountUsage）。
      void awaitMountUsage(input.allocation_id)
      return result.mount
    },
    [refresh]
  )

  const cancelMount = useCallback(
    async (input: { allocation_id: string }): Promise<void> => {
      await agentApi.cancelMount(input)
      // 取消掉的挂载不会再有用量：把那一次追问停掉（它只可能等来超时）。
      stopUsageCatchup(input.allocation_id)
      // 取消是异步收场的：代理的挂载流程随后按失败清理并删除记录，这里先刷一次让状态尽快
      // 反映；最终状态由代理推送的 unmount 事件兜底。
      await refresh()
    },
    [refresh]
  )

  const reconnectServer = useCallback(async (serverKey?: string): Promise<AgentServerState> => {
    try {
      // 代理已把失败计数清零并把阶段置回 connecting，这里落一次快照让标签立刻变回"连接中"
      // （不等下一次 GET /agent/state 轮询）。多服务端下只更新**该台**那一份。
      const server = await agentApi.reconnectServer(serverKey)
      const latest = snapshot.state
      if (latest) {
        const key = server.server_key ?? serverKey ?? primaryServerKey(latest)
        const servers = (latest.servers ?? []).map((item) => (item.server_key === key ? { ...item, ...server } : item))
        const isPrimary = isPrimaryServerKey(latest, key)
        setSnapshot({
          state: { ...latest, servers, server: isPrimary ? { ...latest.server, ...server } : latest.server },
          error: null
        })
      }
      return server
    } catch (error) {
      // 重试本身失败（证书过期、服务端不可达、代理正在退出…）：原因落到 agent.error，
      // 排障面板里能直接读到，界面只需要把按钮恢复成可点。
      setSnapshot({ error })
      throw error
    }
  }, [])

  return {
    ...current,
    mount,
    unmount,
    remount,
    cancelMount,
    reconnectServer,
    refresh,
    refreshCache,
    refreshUsage,
    refreshDownloads,
    refreshUploads,
    applyWebUpdate
  }
}

/**
 * 代理版本状态（供顶栏提示"代理版本与客户端不一致"）。
 *
 * 数据来自 Electron 主进程下发的 AgentInfo：主进程在代理就绪后会带客户端期望版本
 * 查询 /agent/health，据此判定代理是否为陈旧版本；与 useAgent 的代理 HTTP 状态相互独立。
 */
export function useAgentVersion(): AgentVersionInfo {
  return useSyncExternalStore(subscribeAgentVersion, getAgentVersionInfo, getAgentVersionInfo)
}
