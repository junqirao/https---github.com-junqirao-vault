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
 * 存储库用量：进度条百分比 + 「已用 / 上限」文案。
 *
 * 卡片、详情页概览共用一个口径（同一台机器上这两处必须显示同一个数）；管理端表格走服务端
 * 口径 —— 运维看的是宿主占用，与客户端"这块库我用了多少"本来就不是一回事。
 *
 * 分母取**库容量**（建库时设定的容量 = 那块盘的 VHDX 标称容量，服务端读时派生）。
 * 不能拿 `quota_bytes` 当分母：库没有"配额"概念，新建库根本不写这个字段（恒为 0），
 * 于是百分比恒为 0、进度条永远空着 —— 界面上就是"看不出用了多少"。
 * 存量老库里若还残留库级配额（>0），回退用它当分母，免得老库退化成没有分母。
 *
 * 分子取本机**实测**的卷用量（客户端挂载着这个库时才有，见 AgentMountVolumeUsage）：
 * 服务端的 used_bytes 是宿主侧物理占用 —— 客户机里删掉数据它也不回落，卡片上的「已用」
 * 于是永远停在写过的最高水位，不是用户眼里的用量。有实测值时以它为准（分母同样用卷容量：
 * 那就是这块盘的标称容量）。
 *
 * 第三个参数 `localOnly` 是客户端的规则（真实反馈："未挂载的情况下不展示已用存储的量，
 * 那个进度条置灰就行了"、"已用那里只展示一个上限就可以了"）：没有实测值时**不拿服务端数字
 * 兜底**，用量写 `-`（不知道），上限照报 —— 文案就是 `- / 1000 MB`。`-` 占住"已用"那个
 * 位置：用户既不会把宿主最高水位读成自己的用量，也不会把两个数看反。管理端不传（false），
 * 它要看的正是宿主占用那个绝对数。
 *
 * 别拿 `formatBytes(0)` 当"未知"：那是"确实一个字节没用"，两件事。
 *
 * 返回值给的是**零件**加整句：`text`（"已用 / 上限"）给管理端表格这类只要一句话的地方，
 * `usedText` / `totalText` 两半给客户端 —— 刚挂上、用量还在取数时，「已用」那半边要换成
 * 转圈（见 components/UsageBar 的 UsageValueText），整句没法局部替换。
 *
 * `measured` = 用量是不是本机实测值：置灰进度条、给「已用」标签挂悬浮说明都用它
 * （见 RepoCards 的 RepoUsage），免得每个调用方各自判一遍。
 */
export function repoUsage(
  repo?: {
    used_bytes?: number
    capacity_bytes?: number
    quota_bytes?: number
  },
  local?: { used_bytes?: number; total_bytes?: number },
  localOnly = false
): { percent: number; usedText: string; totalText: string; text: string; measured: boolean } {
  const localTotal = local?.total_bytes && local.total_bytes > 0 ? local.total_bytes : 0
  const measured = localTotal > 0
  // 用量未知用 undefined 表示（客户端没实测到），与"用了 0 字节"区分开。
  const used = measured ? (local?.used_bytes ?? 0) : localOnly ? undefined : (repo?.used_bytes ?? 0)
  const capacity = repo?.capacity_bytes && repo.capacity_bytes > 0 ? repo.capacity_bytes : 0
  const total = measured ? localTotal : capacity > 0 ? capacity : (repo?.quota_bytes ?? 0)
  const usedText = used === undefined ? '-' : formatBytes(used)
  // 分母为 0 = 上限根本不知道（不是"上限 0"）：只报已知的那半边，不写"上限 0"。
  if (total <= 0) return { percent: 0, usedText, totalText: '', text: usedText, measured }
  // 超 100% 是真的会发生的：用量按"每个差异盘按容量预留"计，共享库多位用户同时在建盘时
  // 会短暂超过单份容量。钳到 100 只影响条形长度，文字仍显示真实数值。
  const totalText = formatBytes(total)
  return {
    percent: used === undefined ? 0 : Math.min(100, Math.round((used / total) * 100)),
    usedText,
    totalText,
    text: `${usedText} / ${totalText}`,
    measured
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
