/**
 * 表单校验规则。
 *
 * **与服务端 internal/app/bootstrap.go 的 validateUsername / validatePassword 保持一致**，
 * 前端预校验只为即时反馈，服务端仍是最终权威。
 */

/** 服务端地址默认值（服务端默认启用 TLS，见 http.tls.enabled）。 */
export const DEFAULT_SERVER_URL = 'https://127.0.0.1:8443'

/** 本端协议版本常量（与服务端 version.APIVersion 对应）。 */
export const CLIENT_API_VERSION = 1

export const USERNAME_MIN = 3
export const USERNAME_MAX = 64
export const PASSWORD_MIN = 8
export const PASSWORD_MAX = 128

const USERNAME_PATTERN = /^[A-Za-z0-9_.-]+$/

/** 用户名：3~64 字符，仅允许字母/数字/下划线/连字符/点号。 */
export function isValidUsername(value: string): boolean {
  const length = [...value].length
  if (length < USERNAME_MIN || length > USERNAME_MAX) return false
  return USERNAME_PATTERN.test(value)
}

/** 密码：8~128 字符且必须同时包含字母与数字。 */
export function isValidPassword(value: string): boolean {
  const length = [...value].length
  if (length < PASSWORD_MIN || length > PASSWORD_MAX) return false
  return hasLetterAndDigit(value)
}

export function hasLetterAndDigit(value: string): boolean {
  return /[A-Za-z]/.test(value) && /[0-9]/.test(value)
}

/**
 * 服务端地址归一化。
 *
 * 无协议时补 **https://**（服务端默认启用 TLS，补 http 会连到 TLS 端口并收到 HTTP 400）；
 * 去掉末尾多余的斜杠；保留端口与路径。仅接受 http/https。
 */
export function normalizeBaseUrl(value: string): string {
  const trimmed = value.trim()
  if (!trimmed) return ''
  const withScheme = /^[a-zA-Z][a-zA-Z0-9+.-]*:\/\//.test(trimmed) ? trimmed : `https://${trimmed}`
  return withScheme.replace(/\/+$/, '')
}

export function isValidHttpUrl(value: string): boolean {
  const normalized = normalizeBaseUrl(value)
  if (!normalized) return false
  try {
    const url = new URL(normalized)
    return url.protocol === 'http:' || url.protocol === 'https:'
  } catch {
    return false
  }
}
