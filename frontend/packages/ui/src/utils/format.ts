import dayjs from 'dayjs'

const BYTE_UNITS = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'] as const

/** 以 1024 进制格式化字节数。 */
export function formatBytes(bytes: number | undefined | null): string {
  if (bytes === undefined || bytes === null || Number.isNaN(bytes)) return '-'
  if (bytes === 0) return '0 B'
  const negative = bytes < 0
  let value = Math.abs(bytes)
  let unitIndex = 0
  while (value >= 1024 && unitIndex < BYTE_UNITS.length - 1) {
    value /= 1024
    unitIndex += 1
  }
  const digits = value >= 100 || unitIndex === 0 ? 0 : value >= 10 ? 1 : 2
  const unit = BYTE_UNITS[unitIndex] ?? 'B'
  return `${negative ? '-' : ''}${value.toFixed(digits)} ${unit}`
}

/** 容量输入框统一使用的单位后缀（存量值仍是字节，只在表单里换算成 M 展示/录入）。 */
export const CAPACITY_UNIT = 'M'

/**
 * 母盘 VHDX 的最小容量：1 GB。
 *
 * 与服务端 sizing 规则一致（`CalculateVHDXSize`：容量留空或过小都取最小值，并向上取整到 1 GB）。
 * 用途：新建存储库时在客户端就能判断"库配额是否连母盘都装不下"——否则会等服务端返回
 * `repo.quota_exceeded`（"已用 0 B"，新建时很难懂，真实工单）。
 */
export const MIN_PARENT_BYTES = 1 << 30

/** 字节 → MB（1024 进制，保留 2 位小数；用于表单回填）。 */
export function bytesToMegabytes(bytes: number | undefined | null): number | undefined {
  if (bytes === undefined || bytes === null || Number.isNaN(bytes)) return undefined
  return Math.round((bytes / (1024 * 1024)) * 100) / 100
}

/** MB → 字节（四舍五入到整数；用于表单提交）。 */
export function megabytesToBytes(megabytes: number | undefined | null): number | undefined {
  if (megabytes === undefined || megabytes === null || Number.isNaN(megabytes)) return undefined
  return Math.round(megabytes * 1024 * 1024)
}

/**
 * antd Form.Item 的容量字段配套属性。
 *
 * 用法：`<Form.Item name="quota_bytes" getValueProps={capacityValueProps} normalize={capacityNormalize}>`
 * —— 表单里**存储的仍是字节**（接口无需改动），但界面按 M 展示与录入。
 */
export const capacityValueProps = (bytes?: number): { value?: number } => ({
  value: bytesToMegabytes(bytes)
})

export const capacityNormalize = (megabytes?: number): number | undefined => megabytesToBytes(megabytes)

/** 格式化毫秒时间戳；0/空值显示为 '-'。 */
export function formatTime(ms: number | undefined | null): string {
  if (!ms) return '-'
  return dayjs(ms).format('YYYY-MM-DD HH:mm:ss')
}

/** 计算距今剩余秒数（不小于 0）。 */
export function remainingSeconds(expiresAtMs: number): number {
  return Math.max(0, Math.ceil((expiresAtMs - Date.now()) / 1000))
}

/**
 * 脱敏展示敏感值（token / 密钥）。
 *
 * 只保留头尾少量字符，绝不完整展示。
 */
export function maskSecret(value: string | undefined | null): string {
  if (!value) return '-'
  if (value.length <= 8) return '••••'
  return `${value.slice(0, 4)}••••${value.slice(-4)}`
}

/** 取实例 ID 的短前缀，便于用户核对机器。 */
export function shortId(id: string | undefined | null, length = 8): string {
  if (!id) return '-'
  return id.slice(0, length)
}
