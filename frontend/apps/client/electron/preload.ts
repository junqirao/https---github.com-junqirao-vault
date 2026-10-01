import { contextBridge, ipcRenderer, type IpcRendererEvent } from 'electron'

import type { AgentRequestOptions, AgentRequestResult, AgentStreamMessage } from './agent'

/**
 * preload：只暴露最小 API。
 *
 * 渲染进程只能读写本地配置、查询本地代理状态、经主进程转发调用本地代理、
 * 打开路径与请求退出；无法访问 Node/Electron 其他能力
 * （nodeIntegration 关闭、contextIsolation 开启）。
 */

/**
 * 下发给渲染进程的代理状态。
 *
 * 刻意**不含令牌**：所有对代理的调用都由主进程代发（见 agentRequest），
 * 令牌只存在于主进程内存中。
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

const api = {
  getConfig: (): Promise<unknown> => ipcRenderer.invoke('vault:get-config'),
  setConfig: (value: unknown): Promise<boolean> => ipcRenderer.invoke('vault:set-config', value),
  getConfigPath: (): Promise<string> => ipcRenderer.invoke('vault:config-path'),
  getAgent: (): Promise<VaultAgentInfo> => ipcRenderer.invoke('vault:agent-info'),
  /** 代理重启（端口/令牌变化）时回调；返回取消订阅函数。 */
  onAgentChanged: (callback: (info: VaultAgentInfo) => void): (() => void) => {
    const listener = (_event: IpcRendererEvent, info: VaultAgentInfo): void => callback(info)
    ipcRenderer.on('vault:agent-changed', listener)
    return () => {
      ipcRenderer.removeListener('vault:agent-changed', listener)
    }
  },
  /** 经主进程转发一次代理请求（令牌不进入渲染进程）。 */
  agentRequest: (options: AgentRequestOptions): Promise<AgentRequestResult> =>
    ipcRenderer.invoke('vault:agent-request', options),
  /** 让主进程订阅代理事件流，并把事件推给本渲染进程。 */
  agentEventsStart: (): Promise<boolean> => ipcRenderer.invoke('vault:agent-events-start'),
  /** 关闭主进程为本渲染进程建立的代理事件流。 */
  agentEventsStop: (): Promise<boolean> => ipcRenderer.invoke('vault:agent-events-stop'),
  /** 订阅代理事件流消息（open/error/event）；返回取消订阅函数。 */
  onAgentEvent: (callback: (message: AgentStreamMessage) => void): (() => void) => {
    const listener = (_event: IpcRendererEvent, message: AgentStreamMessage): void => callback(message)
    ipcRenderer.on('vault:agent-event', listener)
    return () => {
      ipcRenderer.removeListener('vault:agent-event', listener)
    }
  },
  /** 把服务端证书指纹登记为可信（TOFU 首次信任）；见 electron/main.ts 的说明。 */
  trustCertificate: (serverUrl: string, sha256: string): Promise<boolean> =>
    ipcRenderer.invoke('vault:trust-certificate', { serverUrl, sha256 }),
  /** 打开系统目录选择框（选择源目录）；用户取消时 canceled=true。 */
  selectDirectory: (): Promise<{ canceled: boolean; path?: string }> =>
    ipcRenderer.invoke('vault:select-directory'),
  openPath: (target: string): Promise<void> => ipcRenderer.invoke('vault:open-path', target),
  showItemInFolder: (target: string): Promise<void> => ipcRenderer.invoke('vault:show-item', target),
  quit: (): Promise<void> => ipcRenderer.invoke('vault:quit'),
  /** 立即重启客户端（用于让已激活的渲染层热更资源生效）。 */
  relaunch: (): Promise<void> => ipcRenderer.invoke('vault:relaunch'),
  /**
   * 让"刚激活的热更渲染层"立刻生效：主进程重新解析热更目录并加载（无需重启客户端）。
   *
   * applied=false 表示当前已是最新层（或没有可用热更层），调用方无需处理。
   */
  applyWebLayer: (): Promise<{ applied: boolean; version?: string; reason?: string }> =>
    ipcRenderer.invoke('vault:apply-web-layer')
} as const

contextBridge.exposeInMainWorld('vault', api)
