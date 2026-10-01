import { ApiError, businessErrorFrom } from './errors'

/** 客户端运行时配置（服务端地址 + 会话令牌）。 */
export interface ApiClientConfig {
  baseUrl: string
  token?: string
}

export interface ApiClientOptions {
  /** 每次请求时读取最新配置（地址/令牌可随时切换）。 */
  getConfig: () => ApiClientConfig
  /** 收到 401 时回调（用于自动跳登录页）。 */
  onUnauthorized?: () => void
  /**
   * 收到 401 时尝试续期会话；返回 true 表示已拿到新令牌，调用方会**重放一次**原请求。
   *
   * 实现方（宿主应用）应保证单飞：并发 401 只触发一次续期（例如共享同一个 in-flight Promise），
   * 续期失败返回 false，由 onUnauthorized 负责清理会话并回到登录阶段。
   */
  onRenewSession?: () => Promise<boolean>
  /** 请求超时（毫秒）。 */
  timeoutMs?: number
  fetchImpl?: typeof fetch
}

export type QueryValue = string | number | boolean | undefined | null

/** 拼接查询串，忽略空值。 */
export function buildQuery(params?: Record<string, QueryValue>): string {
  if (!params) return ''
  const search = new URLSearchParams()
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined || value === null || value === '') continue
    search.set(key, String(value))
  }
  const qs = search.toString()
  return qs ? `?${qs}` : ''
}

/**
 * REST 客户端基类。
 *
 * 统一处理 4 类情况：
 *  1. 成功 → 返回解析后的数据；
 *  2. 业务错误 → 抛出携带 code/args 的 ApiError（由 UI 本地化）；
 *  3. 网络错误/超时 → 抛出 network/timeout 类型的 ApiError；
 *  4. 401 未认证 → 触发 onUnauthorized 回调后抛出。
 */
export class ApiClient {
  protected readonly options: ApiClientOptions

  constructor(options: ApiClientOptions) {
    this.options = options
  }

  /** 当前配置（供 SSE 地址拼接等使用）。 */
  config(): ApiClientConfig {
    return this.options.getConfig()
  }

  /** 构造已带令牌的 SSE 地址（EventSource 无法自定义请求头，令牌走查询参数）。 */
  sseUrl(path: string): string {
    const { baseUrl, token } = this.config()
    const query = buildQuery({ token })
    return `${trimBase(baseUrl)}${path}${query}`
  }

  /**
   * 发起请求。
   *
   * `allowRenew` 控制"401 → 续期一次 → 重放一次"：重放时传 false，保证**同一请求最多重放一次**
   * （避免续期成功后仍 401 时无限循环）。续期本身的并发去重由 onRenewSession 的实现方负责（单飞）。
   */
  protected async request<T>(
    method: string,
    path: string,
    body?: unknown,
    query?: Record<string, QueryValue>,
    allowRenew = true
  ): Promise<T> {
    const { baseUrl, token } = this.config()
    const url = `${trimBase(baseUrl)}${path}${buildQuery(query)}`
    const controller = new AbortController()
    const timeout = this.options.timeoutMs ?? 20000
    const timer = setTimeout(() => controller.abort(), timeout)

    const headers: Record<string, string> = { Accept: 'application/json' }
    if (token) headers.Authorization = `Bearer ${token}`
    if (body !== undefined) headers['Content-Type'] = 'application/json'

    const doFetch = this.options.fetchImpl ?? fetch
    let response: Response
    try {
      response = await doFetch(url, {
        method,
        headers,
        signal: controller.signal,
        body: body === undefined ? undefined : JSON.stringify(body)
      })
    } catch (error) {
      if (isAbortError(error)) {
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

    if (response.status === 204) return undefined as T

    const payload = await readBody(response)

    if (response.status === 401) {
      // 仅对"已带令牌的请求"尝试续期（登录等匿名接口的 401 不代表会话过期，不该触发续期）。
      // 续期成功则用新令牌重放一次（allowRenew=false，保证同一请求最多重放一次）。
      if (allowRenew && token && this.options.onRenewSession) {
        const renewed = await this.options.onRenewSession()
        if (renewed) return this.request<T>(method, path, body, query, false)
      }
      this.options.onUnauthorized?.()
      throw businessErrorFrom(response.status, payload)
    }
    if (!response.ok) {
      // 服务端默认启用 TLS：用明文 http 访问 TLS 端口会得到 HTTP 400
      // （"Client sent an HTTP request to an HTTPS server"）。
      // 这里给出明确的可操作指引（改用 https），而不是退化成通用的 HTTP 错误。
      if (response.status === 400 && isPlainHttp(baseUrl)) {
        throw new ApiError({ kind: 'business', code: 'agent.tls_required', status: response.status })
      }
      throw businessErrorFrom(response.status, payload)
    }
    if (payload === undefined || payload === null) return undefined as T
    return payload as T
  }
}

function trimBase(baseUrl: string): string {
  return baseUrl.replace(/\/+$/, '')
}

/** 是否为明文 http 地址（服务端默认启用 TLS，明文访问会得到 HTTP 400）。 */
function isPlainHttp(baseUrl: string): boolean {
  return /^http:\/\//i.test(baseUrl.trim())
}

function isAbortError(error: unknown): boolean {
  return error instanceof DOMException && error.name === 'AbortError'
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
