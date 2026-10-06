import type { ReactNode } from 'react'
import { Card } from 'antd'

import { spacing } from '../tokens/palette'

export interface SectionCardProps {
  title?: ReactNode
  extra?: ReactNode
  children?: ReactNode
  /** 去掉内边距（用于内嵌表格）。 */
  flush?: boolean
  /**
   * 首次取数中：由 antd Card 自己渲染骨架屏（标题 + 内容占位）。
   *
   * 该传的时候一定要传：卡片里的内容多半是 `data?.x ?? '-'` 或"空态"，不传 loading 的话，
   * 用户第一眼看到的是"全部为空/暂无数据"，数据到了再突然填满 —— 像是"这块就是空的"。
   */
  loading?: boolean
}

/** 分区卡片：统一标题、间距与边框。 */
export function SectionCard({ title, extra, children, flush, loading }: SectionCardProps): JSX.Element {
  return (
    <Card
      title={title}
      extra={extra}
      loading={loading}
      styles={flush ? { body: { padding: 0 } } : undefined}
      style={{ marginBottom: spacing.md }}
    >
      {children}
    </Card>
  )
}
