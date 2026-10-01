import type { ReactNode } from 'react'
import { Card } from 'antd'

import { spacing } from '../tokens/palette'

export interface SectionCardProps {
  title?: ReactNode
  extra?: ReactNode
  children?: ReactNode
  /** 去掉内边距（用于内嵌表格）。 */
  flush?: boolean
}

/** 分区卡片：统一标题、间距与边框。 */
export function SectionCard({ title, extra, children, flush }: SectionCardProps): JSX.Element {
  return (
    <Card
      title={title}
      extra={extra}
      styles={flush ? { body: { padding: 0 } } : undefined}
      style={{ marginBottom: spacing.md }}
    >
      {children}
    </Card>
  )
}
