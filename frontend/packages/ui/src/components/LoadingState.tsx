import { Flex, Spin, Typography } from 'antd'

import { fontSize, spacing } from '../tokens/palette'

export interface LoadingStateProps {
  text?: string
  /** 占满视口的居中加载（用于启动检查等整屏场景）。 */
  fullscreen?: boolean
}

export function LoadingState({ text, fullscreen }: LoadingStateProps): JSX.Element {
  const content = (
    <Flex vertical align="center" justify="center" gap={spacing.sm}>
      <Spin />
      {text ? <Typography.Text type="secondary" style={{ fontSize: fontSize.sm }}>{text}</Typography.Text> : null}
    </Flex>
  )
  if (!fullscreen) return <div style={{ padding: spacing.xl }}>{content}</div>
  return (
    <Flex align="center" justify="center" style={{ minHeight: '100vh', padding: spacing.lg }}>
      {content}
    </Flex>
  )
}
