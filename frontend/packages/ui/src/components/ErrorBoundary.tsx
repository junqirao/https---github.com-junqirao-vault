import { Component, type ErrorInfo, type ReactNode } from 'react'
import { Button, Result } from 'antd'

import { useI18n } from '../i18n'
import { spacing } from '../tokens/palette'

/**
 * 渲染期错误边界。
 *
 * 为什么必须有：只要某个页面在渲染期抛异常（例如接口返回的字段为 null，而组件直接读了
 * `.length`），React 会卸载整棵子树，用户看到的就是**白屏**——既没有提示也无法自救。
 * 这里把异常收敛成一张可读的提示卡 + 一个"重试"（重新加载）按钮，保证任何单点故障
 * 都不会以白屏的形式暴露给用户。
 */
export class ErrorBoundary extends Component<{ children: ReactNode }, { hasError: boolean }> {
  override state: { hasError: boolean } = { hasError: false }

  static getDerivedStateFromError(): { hasError: boolean } {
    return { hasError: true }
  }

  override componentDidCatch(error: Error, info: ErrorInfo): void {
    // 只记录异常本身与组件栈，不打日志到远端；控制台留痕便于排障。
    console.error('[vault] 界面渲染异常：', error.message, info.componentStack)
  }

  override render(): ReactNode {
    if (!this.state.hasError) return this.props.children
    return <ErrorFallback />
  }
}

function ErrorFallback(): JSX.Element {
  const { t } = useI18n()
  return (
    <div style={{ padding: spacing.lg }}>
      <Result
        status="error"
        title={t('err.unknown')}
        extra={
          <Button type="primary" onClick={() => window.location.reload()}>
            {t('common.retry')}
          </Button>
        }
      />
    </div>
  )
}
