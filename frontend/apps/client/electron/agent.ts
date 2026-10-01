import { execFile, spawn, type ChildProcess } from 'node:child_process'
import crypto from 'node:crypto'
import { promises as fs } from 'node:fs'
import http from 'node:http'
import path from 'node:path'
import { promisify } from 'node:util'
import { app } from 'electron'

/**
 * 本地代理（Vault-Agent.exe）生命周期管理。
 *
 * 职责：
 *  - 查找可执行文件（环境变量覆盖 → 主程序同目录 → resources/agent）；
 *  - 以一次性令牌拉起，解析 stdout 的 `AGENT_READY port=<n> token=<t>`；
 *  - 崩溃自动重启（每分钟最多 5 次），端口/令牌变化后通知渲染进程；
 *  - **启动前清理陈旧代理**（版本不一致 / 无法连通但进程存在），见 reapStaleAgent；
 *  - 找不到可执行文件时进入"降级模式"（available:false），不影响应用本身。
 *
 * 安全约束：令牌只经参数传入，不写日志；代理 stderr 直接丢弃。
 */

export interface AgentInfo {
  available: boolean
  baseUrl: string | null
  token: string | null
  error?: string
  /** 代理自报版本（GET /agent/health 的 agent_version）。 */
  version?: string | null
  /** 客户端期望的代理版本（app.getVersion()）。 */
  expectedVersion?: string
  /** 代理版本与客户端期望不一致（陈旧代理，界面据此提示）。 */
  versionMismatch?: boolean
}

/**
 * 渲染进程发起的代理请求。
 *
 * 渲染进程不再直连 127.0.0.1 的代理、也不持有令牌：它把「方法 + 路径 + 请求体」经 IPC
 * 交给主进程，由主进程附加 `X-Vault-Agent-Token` 后转发。这样既绕开了浏览器对跨源请求
 * 的 CORS 约束，也让令牌始终留在主进程内（见 request 的说明）。
 */
export interface AgentRequestOptions {
  method: string
  path: string
  body?: unknown
  timeoutMs?: number
}

/**
 * 代理请求结果。
 *
 * `status === 0` 表示请求根本没到达代理（代理不可用 / 网络错误 / 超时），此时 `error`
 * 给出细分原因；`status > 0` 时按普通 HTTP 语义处理（含 4xx/5xx，错误体在 `payload`）。
 */
export interface AgentRequestResult {
  status: number
  payload?: unknown
  error?: 'unavailable' | 'timeout' | 'network'
  message?: string
}

/** 代理事件流（SSE）推送到渲染进程的消息。 */
export type AgentStreamMessage =
  | { kind: 'open' }
  | { kind: 'error' }
  | { kind: 'event'; type: string; data: string }

const READY_TIMEOUT_MS = 15000
const MAX_RESTARTS_PER_WINDOW = 5
const RESTART_WINDOW_MS = 60000
const READY_PATTERN = /AGENT_READY\s+port=(\d+)\s+token=(\S+)/
/** 代理请求默认超时（与渲染进程侧的默认值保持一致）。 */
const DEFAULT_REQUEST_TIMEOUT_MS = 20000
/** 代理响应都是小 JSON；超过该上限视为异常，直接截断，避免内存被拖垮。 */
const MAX_RESPONSE_BYTES = 8 << 20
/** 代理可执行文件名；清理陈旧进程时**只**结束该文件名且位于安装目录内的进程。 */
const AGENT_EXE_NAME = 'Vault-Agent.exe'
/** 结束陈旧代理后等待其释放单实例锁的最长时间。 */
const STALE_KILL_WAIT_MS = 3000
/** 每次轮询"陈旧进程是否已退出"的间隔。 */
const STALE_KILL_POLL_MS = 150

const execFileAsync = promisify(execFile)

async function fileExists(file: string): Promise<boolean> {
  try {
    await fs.access(file)
    return true
  } catch {
    return false
  }
}

function messageOf(error: unknown): string {
  return error instanceof Error ? error.message : 'unknown_error'
}

function resourcesPath(): string {
  const value = (process as unknown as { resourcesPath?: string }).resourcesPath
  return typeof value === 'string' ? value : ''
}

/** 客户端期望的代理版本：与客户端同版本发布，因此直接取应用版本号。 */
function expectedAgentVersion(): string {
  try {
    return app.getVersion()
  } catch {
    return ''
  }
}

/** 运行一段 PowerShell 并返回 stdout（隐藏窗口，避免黑框）。 */
export async function runPowerShell(script: string): Promise<string> {
  const { stdout } = await execFileAsync(
    'powershell.exe',
    ['-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-Command', script],
    { windowsHide: true, timeout: 5000, maxBuffer: 1 << 20 }
  )
  return stdout
}

/** 一个正在运行的同名进程。 */
export interface NamedProcess {
  pid: number
  exePath: string
}

/** 判断 child 是否位于 roots 任一目录（含其子目录）之下（Windows 大小写不敏感）。 */
function isUnderRoot(child: string, roots: string[]): boolean {
  const target = path.resolve(child).toLowerCase()
  return roots.some((root) => {
    const base = path.resolve(root).toLowerCase()
    return target === base || target.startsWith(base + path.sep)
  })
}

/**
 * 枚举本机所有指定镜像名的进程（PID + 可执行文件路径）。
 *
 * 使用 PowerShell + Get-CimInstance：只有它能在同一次调用里同时拿到 PID 与进程镜像路径，
 * 从而支持"只结束位于指定目录内"的安全过滤，而不是按进程名一刀切（taskkill /IM）。
 */
export async function listProcesses(name: string): Promise<NamedProcess[]> {
  const script =
    `Get-CimInstance Win32_Process -Filter "Name='${name}'" | ` +
    `Select-Object ProcessId,ExecutablePath | ConvertTo-Json -Compress`
  const stdout = (await runPowerShell(script)).trim()
  if (!stdout) return []
  let parsed: unknown
  try {
    parsed = JSON.parse(stdout)
  } catch {
    return []
  }
  const rows = Array.isArray(parsed) ? parsed : [parsed]
  const out: NamedProcess[] = []
  for (const row of rows) {
    if (typeof row !== 'object' || row === null) continue
    const value = row as { ProcessId?: unknown; ExecutablePath?: unknown }
    const pid = Number(value.ProcessId)
    const exePath = typeof value.ExecutablePath === 'string' ? value.ExecutablePath : ''
    if (Number.isFinite(pid) && pid > 0) out.push({ pid, exePath })
  }
  return out
}

/** GET 代理 health（带超时），返回 agent_version 与版本是否不一致。 */
function fetchHealth(
  baseUrl: string,
  token: string,
  expectedVersion: string
): Promise<{ version: string | null; mismatch: boolean }> {
  return new Promise((resolve, reject) => {
    const query = expectedVersion ? `?expected_version=${encodeURIComponent(expectedVersion)}` : ''
    const request = http.get(
      `${baseUrl}/agent/health${query}`,
      { headers: { 'X-Vault-Agent-Token': token } },
      (response) => {
        if (response.statusCode !== 200) {
          response.resume()
          reject(new Error(`health ${response.statusCode}`))
          return
        }
        let body = ''
        response.setEncoding('utf8')
        response.on('data', (chunk: string) => {
          body += chunk
        })
        response.on('end', () => {
          try {
            const parsed = JSON.parse(body) as { agent_version?: unknown; version_mismatch?: unknown }
            resolve({
              version: typeof parsed.agent_version === 'string' ? parsed.agent_version : null,
              mismatch: parsed.version_mismatch === true
            })
          } catch (error) {
            reject(error instanceof Error ? error : new Error('bad health json'))
          }
        })
      }
    )
    request.on('error', reject)
    request.setTimeout(3000, () => {
      request.destroy(new Error('health timeout'))
    })
  })
}

export class AgentManager {
  private child: ChildProcess | null = null
  private info: AgentInfo = { available: false, baseUrl: null, token: null, error: 'not_started' }
  private readonly listeners = new Set<(info: AgentInfo) => void>()
  private restartTimes: number[] = []
  private stopping = false
  private readyTimer: ReturnType<typeof setTimeout> | null = null
  /**
   * 额外的"可清理目录"。
   *
   * 默认只清理客户端自身安装目录下的陈旧代理；当新版客户端接管了另一个安装目录下的旧
   * 客户端时（见 main.ts 的 takeoverOlderInstance），旧目录会加入这里，使旧实例遗留的
   * Vault-Agent.exe 也能被回收——否则它仍占着 Global\VaultAgent 锁，新代理起不来。
   */
  private readonly extraRoots = new Set<string>()

  /** 加入一个允许清理陈旧代理的目录（用于接管其它安装目录时回收其代理）。 */
  allowStaleRoot(dir: string): void {
    if (dir) this.extraRoots.add(dir)
  }

  onChanged(listener: (info: AgentInfo) => void): () => void {
    this.listeners.add(listener)
    return () => {
      this.listeners.delete(listener)
    }
  }

  getInfo(): AgentInfo {
    return this.info
  }

  start(): void {
    this.stopping = false
    void this.spawnOnce()
  }

  /** 请求代理自行卸载全部挂载后退出（用于托盘"退出"，超时即放弃）。 */
  async shutdown(timeoutMs = 3000): Promise<void> {
    const { available, baseUrl, token } = this.info
    if (!available || !baseUrl || !token) return
    await new Promise<void>((resolve) => {
      const request = http.request(
        `${baseUrl}/agent/shutdown`,
        { method: 'POST', headers: { 'X-Vault-Agent-Token': token } },
        (response) => {
          response.resume()
          response.on('end', () => resolve())
        }
      )
      request.on('error', () => resolve())
      request.setTimeout(timeoutMs, () => {
        request.destroy()
        resolve()
      })
      request.end()
    })
  }

  /** 活跃挂载数量（代理不可用时返回 0）。 */
  async activeMountCount(): Promise<number> {
    const { available, baseUrl, token } = this.info
    if (!available || !baseUrl || !token) return 0
    return new Promise<number>((resolve) => {
      const request = http.get(
        `${baseUrl}/agent/state`,
        { headers: { 'X-Vault-Agent-Token': token } },
        (response) => {
          if (response.statusCode !== 200) {
            response.resume()
            resolve(0)
            return
          }
          let body = ''
          response.setEncoding('utf8')
          response.on('data', (chunk: string) => {
            body += chunk
          })
          response.on('end', () => {
            try {
              const parsed = JSON.parse(body) as { mounts?: unknown }
              resolve(Array.isArray(parsed.mounts) ? parsed.mounts.length : 0)
            } catch {
              resolve(0)
            }
          })
        }
      )
      request.on('error', () => resolve(0))
      request.setTimeout(3000, () => {
        request.destroy()
        resolve(0)
      })
    })
  }

  /**
   * 以主进程身份请求本地代理。
   *
   * 为什么放在主进程：渲染进程原本用 fetch 直连 127.0.0.1 的代理，这属于跨源请求，
   * 需要代理放开 CORS，且必须把一次性令牌交给渲染进程。改由主进程代发后：
   *   1. 渲染进程只传「方法 + 路径 + 请求体」，令牌不再离开主进程；
   *   2. 不再依赖 CORS（请求由 Node 发出，不受浏览器同源策略约束）。
   *
   * 返回值不做错误抛出：网络/超时以 `status: 0` + `error` 表达，4xx/5xx 原样回传，
   * 由渲染进程统一映射为业务错误。
   */
  async request(options: AgentRequestOptions): Promise<AgentRequestResult> {
    const { available, baseUrl, token } = this.info
    if (!available || !baseUrl || !token) return { status: 0, error: 'unavailable' }

    const body = options.body === undefined ? null : Buffer.from(JSON.stringify(options.body), 'utf8')
    return new Promise<AgentRequestResult>((resolve) => {
      let settled = false
      const done = (result: AgentRequestResult): void => {
        if (settled) return
        settled = true
        resolve(result)
      }

      const request = http.request(
        `${baseUrl}${options.path}`,
        {
          method: options.method,
          headers: {
            Accept: 'application/json',
            'X-Vault-Agent-Token': token,
            ...(body ? { 'Content-Type': 'application/json', 'Content-Length': body.length } : {})
          }
        },
        (response) => {
          const chunks: Buffer[] = []
          let size = 0
          response.on('data', (chunk: Buffer) => {
            size += chunk.length
            if (size <= MAX_RESPONSE_BYTES) chunks.push(chunk)
          })
          response.on('end', () => {
            const text = Buffer.concat(chunks).toString('utf8')
            let payload: unknown
            if (text) {
              try {
                payload = JSON.parse(text) as unknown
              } catch {
                payload = undefined
              }
            }
            done({ status: response.statusCode ?? 0, payload })
          })
          response.on('error', (error) => done({ status: 0, error: 'network', message: messageOf(error) }))
        }
      )

      request.setTimeout(options.timeoutMs ?? DEFAULT_REQUEST_TIMEOUT_MS, () => {
        request.destroy()
        done({ status: 0, error: 'timeout' })
      })
      request.on('error', (error) => done({ status: 0, error: 'network', message: messageOf(error) }))
      if (body) request.write(body)
      request.end()
    })
  }

  /**
   * 订阅代理事件流（SSE），把事件逐条推给调用方（主进程再转给渲染进程）。
   *
   * 同样是为了不让令牌出主进程：浏览器的 EventSource 无法自定义请求头，只能把令牌写进
   * 查询串；改由主进程订阅后，渲染进程只收到事件本身。
   *
   * ⚠️ 代理的 tokenMiddleware **只从 `X-Vault-Agent-Token` 请求头取令牌**
   * （见 internal/agent/httpapi.go），查询串令牌一律 401，因此这里必须用请求头，
   * 与同文件其它调用（request / fetchHealth / activeMountCount）保持一致。
   *
   * 返回取消函数。连接中断只上报一次 `error`，重连由渲染进程侧的退避逻辑发起。
   */
  openEventStream(onMessage: (message: AgentStreamMessage) => void): () => void {
    const { available, baseUrl, token } = this.info
    if (!available || !baseUrl || !token) {
      onMessage({ kind: 'error' })
      return () => undefined
    }

    let closed = false
    let request: http.ClientRequest | null = null
    const close = (): void => {
      closed = true
      request?.destroy()
      request = null
    }

    request = http.get(
      `${baseUrl}/agent/events`,
      { headers: { Accept: 'text/event-stream', 'X-Vault-Agent-Token': token } },
      (response) => {
        if (closed) {
          response.resume()
          return
        }
        if (response.statusCode !== 200) {
          response.resume()
          onMessage({ kind: 'error' })
          close()
          return
        }
        onMessage({ kind: 'open' })

        response.setEncoding('utf8')
        let buffer = ''
        let eventType = 'message'
        let dataLines: string[] = []
        const flush = (): void => {
          if (dataLines.length === 0) return
          onMessage({ kind: 'event', type: eventType, data: dataLines.join('\n') })
          eventType = 'message'
          dataLines = []
        }

        response.on('data', (chunk: string) => {
          buffer += chunk
          const lines = buffer.split('\n')
          buffer = lines.pop() ?? ''
          for (const raw of lines) {
            const line = raw.endsWith('\r') ? raw.slice(0, -1) : raw
            if (line === '') {
              flush()
              continue
            }
            if (line.startsWith(':')) continue // 注释/保活
            if (line.startsWith('event:')) eventType = line.slice(6).trim()
            else if (line.startsWith('data:')) dataLines.push(line.slice(5).trim())
          }
        })
        const abort = (): void => {
          if (!closed) onMessage({ kind: 'error' })
          close()
        }
        response.on('end', abort)
        response.on('error', abort)
      }
    )

    // SSE 是长连接：不设总超时，靠代理侧的 heartbeat 事件维持，断了由渲染进程重连。
    request.on('error', () => {
      if (!closed) onMessage({ kind: 'error' })
      close()
    })
    return close
  }

  /** 停止守护（应用退出时调用，不再重启）。 */
  stop(): void {
    this.stopping = true
    this.clearReadyTimer()
    this.child?.kill()
    this.child = null
    this.publish({ available: false, baseUrl: null, token: null, error: 'stopped' })
  }

  private publish(info: AgentInfo): void {
    this.info = info
    for (const listener of this.listeners) listener(info)
  }

  private clearReadyTimer(): void {
    if (this.readyTimer) {
      clearTimeout(this.readyTimer)
      this.readyTimer = null
    }
  }

  /** 代理可执行文件的候选路径（按优先级）。 */
  private candidates(): string[] {
    return [
      path.join(path.dirname(app.getPath('exe')), AGENT_EXE_NAME),
      resourcesPath() ? path.join(resourcesPath(), 'agent', AGENT_EXE_NAME) : '',
      path.join(app.getAppPath(), 'resources', 'agent', AGENT_EXE_NAME)
    ].filter((candidate) => candidate !== '')
  }

  private async locate(): Promise<string | null> {
    const override = process.env.VAULT_AGENT_PATH
    if (override && (await fileExists(override))) return override
    for (const candidate of this.candidates()) {
      if (await fileExists(candidate)) return candidate
    }
    return null
  }

  /**
   * 允许结束的目录白名单：客户端安装目录、resources 目录、以及实际定位到的代理所在目录。
   *
   * 安全边界：只结束**路径位于这些目录（含子目录）之下**的 Vault-Agent.exe，
   * 绝不误杀用户放在其它路径下的同名进程。
   */
  private allowedRoots(locatedExe: string | null): string[] {
    const roots = new Set<string>()
    roots.add(path.dirname(app.getPath('exe')))
    const res = resourcesPath()
    if (res) roots.add(res)
    try {
      roots.add(app.getAppPath())
    } catch {
      // getAppPath 在极早期不可用时可忽略。
    }
    if (locatedExe) roots.add(path.dirname(locatedExe))
    for (const extra of this.extraRoots) roots.add(extra)
    return [...roots]
  }

  /**
   * 结束"陈旧代理"进程。
   *
   * 为什么必须清理陈旧代理：
   * Vault-Agent 用命名互斥体 `Global\VaultAgent` 保证全机单实例。若旧版本客户端
   * 遗留了一个仍在运行的 Vault-Agent.exe（进程还在、锁还在），当前客户端派生的新代理
   * 会因拿不到互斥体而立即退出，客户端既连不上旧代理（旧代理的端口/令牌不在本客户端手中），
   * 也无法启动新代理——表现为界面一直沿用旧行为（看起来"客户端老是访问旧内容"）。
   * 因此在拉起当前版本代理之前，必须先结束这些陈旧进程。
   *
   * 判定为"陈旧"的条件（二者之一即满足）：
   *   1. 存在可连通的代理（已知 baseUrl/token），但其版本与客户端期望版本不一致；
   *   2. 存在同名进程但无法连通（拿不到端口/令牌），无法确认其版本 ⇒ 一律视为陈旧。
   *
   * 返回 null 表示"可继续启动"；返回错误字符串表示清理失败，调用方应走降级路径（不阻塞界面）。
   */
  private async reapStaleAgent(locatedExe: string | null): Promise<string | null> {
    const roots = this.allowedRoots(locatedExe)
    const expected = expectedAgentVersion()

    // 条件 1：若本地已有一个可连通的代理，先读它的版本。
    if (this.info.available && this.info.baseUrl && this.info.token) {
      try {
        const { version, mismatch } = await fetchHealth(this.info.baseUrl, this.info.token, expected)
        const stale = mismatch || (version !== null && expected !== '' && version !== expected)
        if (!stale) {
          // 版本一致的现役代理：无需清理（正常启动流程下不应出现，保险起见保留）。
          return null
        }
      } catch {
        // 连不上：继续按"进程存在即陈旧"处理。
      }
    }

    let processes: NamedProcess[]
    try {
      processes = await listProcesses(AGENT_EXE_NAME)
    } catch (error) {
      // 无法枚举：不阻塞启动（可能只是权限/命令不可用），交给后续正常流程。
      return `stale_list_failed:${messageOf(error)}`
    }

    const stale = processes.filter((p) => p.exePath !== '' && isUnderRoot(p.exePath, roots))
    if (stale.length === 0) return null

    let failed = false
    for (const proc of stale) {
      try {
        // 只结束同名且位于客户端安装目录内的进程（已由 isUnderRoot 过滤）。
        process.kill(proc.pid, 'SIGKILL')
      } catch {
        failed = true
      }
    }
    if (failed) return 'stale_kill_failed'

    // 等待互斥体释放：被杀进程需要一点时间真正退出并释放 Global\VaultAgent。
    const deadline = Date.now() + STALE_KILL_WAIT_MS
    for (;;) {
      let remaining: NamedProcess[]
      try {
        remaining = (await listProcesses(AGENT_EXE_NAME)).filter(
          (p) => p.exePath !== '' && isUnderRoot(p.exePath, roots)
        )
      } catch {
        break
      }
      if (remaining.length === 0 || Date.now() >= deadline) break
      await new Promise((resolve) => setTimeout(resolve, STALE_KILL_POLL_MS))
    }
    return null
  }

  private canRestart(): boolean {
    const now = Date.now()
    this.restartTimes = this.restartTimes.filter((at) => now - at < RESTART_WINDOW_MS)
    return this.restartTimes.length < MAX_RESTARTS_PER_WINDOW
  }

  private async spawnOnce(): Promise<void> {
    const exe = await this.locate()
    if (!exe) {
      // 降级模式：不崩溃，仅标记不可用。
      this.publish({ available: false, baseUrl: null, token: null, error: 'agent_not_found' })
      return
    }

    // 启动前清理陈旧代理（见 reapStaleAgent 的说明）。
    const cleanupError = await this.reapStaleAgent(exe)
    if (cleanupError) {
      // 清理失败：不阻塞界面，走"代理不可用"的降级路径并在状态里说明原因。
      this.publish({
        available: false,
        baseUrl: null,
        token: null,
        error: cleanupError,
        expectedVersion: expectedAgentVersion()
      })
      return
    }

    const token = crypto.randomBytes(32).toString('base64url')
    let child: ChildProcess
    try {
      child = spawn(exe, ['-token', token], { windowsHide: true, stdio: ['ignore', 'pipe', 'pipe'] })
    } catch (error) {
      this.publish({ available: false, baseUrl: null, token: null, error: messageOf(error) })
      return
    }

    this.child = child
    child.stdout?.setEncoding('utf8')
    let buffer = ''
    const handleLine = (line: string): void => {
      const match = READY_PATTERN.exec(line)
      if (!match) return
      this.clearReadyTimer()
      const baseUrl = `http://127.0.0.1:${Number(match[1])}`
      const agentToken = match[2] ?? token
      this.publish({ available: true, baseUrl, token: agentToken })
      // 就绪后异步补全版本信息（不阻塞启动）。
      void this.refreshVersion(baseUrl, agentToken)
    }

    child.stdout?.on('data', (chunk: string) => {
      buffer += chunk
      const lines = buffer.split(/\r?\n/)
      buffer = lines.pop() ?? ''
      for (const line of lines) handleLine(line)
    })
    // 代理 stderr 可能含敏感上下文，直接丢弃，不写入日志。
    child.stderr?.on('data', () => undefined)
    child.on('exit', () => this.handleExit())

    this.readyTimer = setTimeout(() => {
      this.readyTimer = null
      if (this.info.available) return
      this.publish({ available: false, baseUrl: null, token: null, error: 'agent_timeout' })
      this.child?.kill()
    }, READY_TIMEOUT_MS)
  }

  /** 查询代理 health，把版本与"版本是否不一致"补充进状态（供顶栏提示）。 */
  private async refreshVersion(baseUrl: string, token: string): Promise<void> {
    const expected = expectedAgentVersion()
    try {
      const { version, mismatch } = await fetchHealth(baseUrl, token, expected)
      if (!this.info.available || this.info.baseUrl !== baseUrl) return
      const inferred = expected !== '' && version !== null && version !== expected
      this.publish({
        ...this.info,
        version,
        expectedVersion: expected,
        versionMismatch: mismatch || inferred
      })
    } catch {
      // 拿不到版本不影响可用性，保持现状。
    }
  }

  private handleExit(): void {
    this.child = null
    this.clearReadyTimer()
    if (this.stopping) return
    if (!this.canRestart()) {
      this.publish({ available: false, baseUrl: null, token: null, error: 'agent_crashed' })
      return
    }
    this.restartTimes.push(Date.now())
    this.publish({ available: false, baseUrl: null, token: null, error: 'agent_restarting' })
    void this.spawnOnce()
  }
}
