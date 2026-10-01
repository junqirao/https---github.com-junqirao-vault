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

/**
 * 存储库用量：进度条百分比 + 「已用 / 总量」文案。
 *
 * 卡片、详情页概览、管理端表格共用同一个口径，三处必须显示同一个数。
 *
 * 分母取**库容量**（建库时设定的容量 = 那块盘的 VHDX 标称容量，服务端读时派生）。
 * 不能拿 `quota_bytes` 当分母：库没有"配额"概念，新建库根本不写这个字段（恒为 0），
 * 于是百分比恒为 0、进度条永远空着 —— 界面上就是"看不出用了多少"。
 * 存量老库里若还残留库级配额（>0），回退用它当分母，免得老库退化成没有分母。
 *
 * 容量也没派生出来时（0，例如盘记录已回收）分母为 0：此时只报已用量，
 * 绝不除零，也不假装进度条是 0%（那会被读成"没占用"）。
 */
export function repoUsage(repo?: {
  used_bytes?: number
  capacity_bytes?: number
  quota_bytes?: number
}): { percent: number; text: string } {
  const used = repo?.used_bytes ?? 0
  const capacity = repo?.capacity_bytes && repo.capacity_bytes > 0 ? repo.capacity_bytes : 0
  const total = capacity > 0 ? capacity : (repo?.quota_bytes ?? 0)
  if (total <= 0) return { percent: 0, text: formatBytes(used) }
  // 超 100% 是真的会发生的：用量按"每个差异盘按容量预留"计，共享库多位用户同时在建盘时
  // 会短暂超过单份容量。钳到 100 只影响条形长度，文字仍显示真实数值。
  return {
    percent: Math.min(100, Math.round((used / total) * 100)),
    text: `${formatBytes(used)} / ${formatBytes(total)}`
  }
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
