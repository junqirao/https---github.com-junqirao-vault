import { Tag } from 'antd'

import { hasKey, t } from '../i18n'
import { palette, radius } from '../tokens/palette'

type Tone = 'success' | 'warning' | 'error' | 'default' | 'processing'

/** 状态值 → 语义色调（克制配色，不使用动效）。 */
const TONE_BY_VALUE: Record<string, Tone> = {
  active: 'success',
  ready: 'success',
  published: 'success',
  succeeded: 'success',
  ok: 'success',
  mounted: 'success',
  // 记录说已挂载，但代理实测本机已无活动会话（盘实际不在）—— 不是错误，但必须显眼。
  disconnected: 'warning',
  compatible: 'success',
  idle: 'default',
  allocated: 'default',
  released: 'default',
  cancelled: 'default',
  pending: 'processing',
  running: 'processing',
  creating: 'processing',
  sealing: 'processing',
  mounting: 'processing',
  releasing: 'processing',
  derived: 'processing',
  deleting: 'warning',
  expired: 'warning',
  temp_shared: 'warning',
  maintenance: 'warning',
  revoked: 'error',
  error: 'error',
  failed: 'error',
  denied: 'error'
}

// 使用统一色板中的状态色（而非 antd 预设色名），避免渲染出 antd 默认的 #52C41A 绿。
const TONE_COLOR: Record<Tone, string | undefined> = {
  success: palette.success,
  warning: palette.warning,
  error: palette.danger,
  processing: palette.accent,
  default: undefined
}

export interface StatusTagProps {
  /** i18n 分组：repo / disk / lease / job / allocation / audit / parentCondition ... */
  group: string
  value: string | undefined | null
}

/** 状态标签：按 `state.<group>.<value>` 翻译，缺失时回退原始值。 */
export function StatusTag({ group, value }: StatusTagProps): JSX.Element {
  const style = { borderRadius: radius.sm, marginInlineEnd: 0 }
  if (!value) return <Tag bordered={false} style={style}>-</Tag>
  const key = `state.${group}.${value}`
  const text = hasKey(key) ? t(key) : value
  const tone = TONE_BY_VALUE[value] ?? 'default'
  return (
    <Tag bordered={false} color={TONE_COLOR[tone]} style={style}>
      {text}
    </Tag>
  )
}
