import { hasKey, t } from '../i18n'
import { formatBytes } from '../utils/format'

/** 错误分类，对应"所有请求必须处理的 4 类情况"。 */
export type ApiErrorKind = 'business' | 'unauthorized' | 'network' | 'timeout' | 'http'

/** 统一的 API 错误。业务错误的 code/args 直接来自服务端错误响应体。 */
export class ApiError extends Error {
  readonly kind: ApiErrorKind
  readonly code: string
  readonly args: Record<string, unknown>
  readonly status: number

  constructor(params: {
    kind: ApiErrorKind
    code: string
    args?: Record<string, unknown>
    status?: number
    message?: string
  }) {
    super(params.message ?? params.code)
    this.name = 'ApiError'
    this.kind = params.kind
    this.code = params.code
    this.args = params.args ?? {}
    this.status = params.status ?? 0
  }

  /** 是否为可本地化的业务错误（服务端返回了稳定错误码）。 */
  get isBusiness(): boolean {
    return this.kind === 'business'
  }
}

export function isApiError(value: unknown): value is ApiError {
  return value instanceof ApiError
}

/** 从服务端错误响应体解析出业务错误。 */
export function businessErrorFrom(status: number, body: unknown): ApiError {
  const detail = extractErrorDetail(body)
  if (detail) {
    return new ApiError({
      kind: 'business',
      code: detail.code,
      args: detail.args,
      status
    })
  }
  return new ApiError({ kind: 'http', code: 'http.error', status })
}

interface ErrorDetail {
  code: string
  args: Record<string, unknown>
}

function extractErrorDetail(body: unknown): ErrorDetail | null {
  if (typeof body !== 'object' || body === null) return null
  const errorField = (body as { error?: unknown }).error
  if (typeof errorField !== 'object' || errorField === null) return null
  const code = (errorField as { code?: unknown }).code
  if (typeof code !== 'string' || code === '') return null
  const rawArgs = (errorField as { args?: unknown }).args
  const args = typeof rawArgs === 'object' && rawArgs !== null ? (rawArgs as Record<string, unknown>) : {}
  return { code, args }
}

interface ErrorMessage {
  key: string
  params: Record<string, unknown>
}

function numericArgs(args: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = { ...args }
  if (typeof args.quota_bytes === 'number') out.quota = formatBytes(args.quota_bytes)
  if (typeof args.used_bytes === 'number') out.used = formatBytes(args.used_bytes)
  if (typeof args.need_bytes === 'number') out.need = formatBytes(args.need_bytes)
  return out
}

/** 把（code, args）映射为 i18n key 与插值参数。 */
export function errorMessageOf(code: string, args: Record<string, unknown> = {}): ErrorMessage {
  if (code === 'system.invalid_param') {
    const field = typeof args.field === 'string' ? args.field : ''
    if (field === 'username') {
      if (typeof args.reason === 'string') return { key: 'err.param.username.illegal_character', params: args }
      if (typeof args.min_length === 'number' || typeof args.max_length === 'number') {
        return { key: 'err.param.username.length', params: args }
      }
    }
    if (field === 'password') {
      if (args.reason === 'need_letter_and_digit') {
        return { key: 'err.param.password.need_letter_and_digit', params: args }
      }
      if (typeof args.min_length === 'number') return { key: 'err.param.password.too_short', params: args }
      if (typeof args.max_length === 'number') return { key: 'err.param.password.too_long', params: args }
    }
    return { key: 'err.system.invalid_param', params: args }
  }

  if (code === 'auth.forbidden' && typeof args.reason === 'string') {
    const reasonKey = `err.reason.${args.reason}`
    if (hasKey(reasonKey)) return { key: reasonKey, params: args }
  }

  return { key: `err.${code}`, params: numericArgs(args) }
}

/**
 * 本地化错误文案。
 *
 * - 业务错误：按 code 映射到 `err.<code>`（含特殊码的细化映射）；
 * - 网络/超时：使用本地文案；
 * - 其他：通用文案 + 原始 code（不直接暴露 code 作为主文案）。
 */
export function translateError(error: unknown): string {
  if (!isApiError(error)) return t('err.unknown', { code: 'unknown' })

  if (error.kind === 'network' || error.kind === 'timeout') {
    return t(error.code)
  }
  if (error.kind === 'business') {
    const { key, params } = errorMessageOf(error.code, error.args)
    if (hasKey(key)) return t(key, params)
    return t('err.unknown', { code: error.code })
  }
  if (error.kind === 'unauthorized') {
    return t('err.auth.required')
  }
  return t('err.http.error', { status: error.status })
}

/** 供 UI 附带展示的原始 code（业务错误才有意义）。 */
export function errorCodeOf(error: unknown): string | undefined {
  return isApiError(error) && error.kind === 'business' ? error.code : undefined
}

/**
 * 挂载失败的**阶段提示** key（无对应阶段时返回 undefined）。
 *
 * 为什么需要：代理返回的 `agent.mount_failed` 只带 `args.stage`（原始报错按项目约定
 * 只进日志），界面只显示"挂载失败（阶段：connect）"时用户完全无从下手。而 connect /
 * portal 阶段绝大多数就是"本机 iSCSI 发起程序服务（MSiSCSI）没运行"或"门户地址不可达"
 * —— 把这条可执行建议直接贴到界面上（真实工单）。
 */
export function errorHintKeyOf(error: unknown): string | undefined {
  if (!isApiError(error) || error.kind !== 'business' || error.code !== 'agent.mount_failed') {
    return undefined
  }
  const stage = typeof error.args.stage === 'string' ? error.args.stage : ''
  switch (stage) {
    case 'initiator_service':
    case 'connect':
    case 'wait_connected':
      return 'err.agent.mount_failed.hint.connect'
    case 'portal':
      return 'err.agent.mount_failed.hint.portal'
    case 'find_disk':
      return 'err.agent.mount_failed.hint.disk'
    default:
      return undefined
  }
}

/**
 * 挂载诊断信息的展示顺序与 i18n 键（代理只在这些字段存在时才带上）。
 *
 * detail 是原始报错（如 Connect-IscsiTarget 的 .NET 异常）：失败记录会被代理抹掉，
 * 因此它随错误响应一并带回，作为界面上唯一能看到的真因（真实反馈："根本没法定位错误"）。
 */
const MOUNT_ERROR_INFO_KEYS = ['portal', 'target_iqn', 'auth_mode', 'tcp', 'detail'] as const

/**
 * 从挂载失败错误里取出**连接信息**（代理对 connect / wait_connected 阶段失败会带上）。
 *
 * 为什么需要：过去只有"挂载失败（阶段：connect）"这一句话 —— 门户地址是什么、端口通不通、
 * 目标名对不对，用户一项都拿不到，等于没法定位（真实反馈："根本没法定位错误"）。
 * `tcp` 是代理对门户做的 TCP 可达性探测结论（reachable / timeout / refused / unreachable），
 * 它能一刀切开"网络/防火墙不可达"与"TCP 通但登录/认证失败"两类完全不同的原因。
 */
export function mountErrorInfoOf(error: unknown): Array<{ key: string; value: string }> {
  if (!isApiError(error) || error.kind !== 'business') return []
  const out: Array<{ key: string; value: string }> = []
  for (const key of MOUNT_ERROR_INFO_KEYS) {
    const value = error.args[key]
    if (typeof value === 'string' && value.trim() !== '') out.push({ key, value: value.trim() })
  }
  return out
}
