import { Typography } from 'antd'

import { maskSecret } from '../utils/format'

export interface CopyableTextProps {
  value: string | undefined | null
  /** 展示时脱敏（token / 密钥等敏感值必须开启）。 */
  masked?: boolean
  monospace?: boolean
}

/**
 * 可复制文本。
 *
 * 安全约束：`masked` 只影响**展示**，复制内容仍为真实值，
 * 但界面与日志中绝不出现完整敏感值。
 */
export function CopyableText({ value, masked, monospace }: CopyableTextProps): JSX.Element {
  const raw = value ?? ''
  if (!raw) return <Typography.Text type="secondary">-</Typography.Text>
  const display = masked ? maskSecret(raw) : raw
  return (
    <Typography.Text
      code={monospace}
      style={{ fontFamily: monospace ? 'ui-monospace, SFMono-Regular, Menlo, monospace' : undefined }}
      copyable={{ text: raw, tooltips: [undefined, undefined] }}
    >
      {display}
    </Typography.Text>
  )
}
