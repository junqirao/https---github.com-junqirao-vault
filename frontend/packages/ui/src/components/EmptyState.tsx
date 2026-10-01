import type { ReactNode } from 'react'
import { Button, Empty, Typography } from 'antd'

import { spacing, fontSize } from '../tokens/palette'

export interface EmptyStateProps {
  title: ReactNode
  description?: ReactNode
  action?: {
    label: ReactNode
    onClick: () => void
  }
}

/** 空状态：统一"暂无数据 + 可选主操作"的呈现。 */
export function EmptyState({ title, description, action }: EmptyStateProps): JSX.Element {
  return (
    <div style={{ padding: `${spacing.xl}px ${spacing.md}px`, maxWidth: 480, margin: '0 auto' }}>
      <Empty
        image={Empty.PRESENTED_IMAGE_SIMPLE}
        description={
          <div>
            <Typography.Text style={{ fontSize: fontSize.lg }}>{title}</Typography.Text>
            {description ? (
              <div>
                <Typography.Text type="secondary">{description}</Typography.Text>
              </div>
            ) : null}
          </div>
        }
      >
        {action ? <Button type="primary" onClick={action.onClick}>{action.label}</Button> : null}
      </Empty>
    </div>
  )
}
