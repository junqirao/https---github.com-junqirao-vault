/// <reference types="vite/client" />

/** 构建期由 vite 注入的应用版本（真源为 package.json 的 version，见 vite.config.mts）。 */
declare const __APP_VERSION__: string

/**
 * 本地代理状态（由主进程提供，见 electron/agent.ts）。
 *
 * 刻意**不含令牌**：对代理的调用都由主进程代发（见 VaultBridge.agentRequest）。
 */
interface VaultAgentInfo {
  available: boolean
  baseUrl: string | null
  error?: string
  /** 代理自报版本。 */
  version?: string | null
  /** 客户端期望的代理版本。 */
  expectedVersion?: string
  /** 代理版本与客户端期望是否不一致（陈旧代理）。 */
  versionMismatch?: boolean
}

/** 一次代理请求（与 packages/ui/src/api/agentClient.ts 的同名类型对应）。 */
interface AgentRequestOptions {
  method: string
  path: string
  body?: unknown
  timeoutMs?: number
}

/** 代理请求结果；`status === 0` 表示未到达代理（原因见 error）。 */
interface AgentRequestResult {
  status: number
  payload?: unknown
  error?: 'unavailable' | 'timeout' | 'network'
  message?: string
}

/** 代理事件流（SSE）消息。 */
type AgentStreamMessage =
  | { kind: 'open' }
  | { kind: 'error' }
  | { kind: 'event'; type: string; data: string }

/** preload 暴露的最小 API（见 electron/preload.ts）。 */
interface VaultBridge {
  getConfig: () => Promise<unknown>
  setConfig: (value: unknown) => Promise<boolean>
  getConfigPath: () => Promise<string>
  getAgent: () => Promise<VaultAgentInfo>
  onAgentChanged: (callback: (info: VaultAgentInfo) => void) => (() => void) | void
  agentRequest: (options: AgentRequestOptions) => Promise<AgentRequestResult>
  agentEventsStart: () => Promise<boolean>
  agentEventsStop: () => Promise<boolean>
  onAgentEvent: (callback: (message: AgentStreamMessage) => void) => (() => void) | void
  /** 把服务端证书指纹登记为可信（TOFU 首次信任）。 */
  trustCertificate: (serverUrl: string, sha256: string) => Promise<boolean>
  /** 打开系统目录选择框（选择源目录）；用户取消时 canceled=true。 */
  selectDirectory: () => Promise<{ canceled: boolean; path?: string }>
  openPath: (path: string) => Promise<void>
  showItemInFolder: (path: string) => Promise<void>
  quit: () => Promise<void>
}

interface Window {
  vault?: VaultBridge
}
