import { createContext, useContext, type ReactNode } from 'react'
import { Space, Typography } from 'antd'

import { fontSize, palette, spacing } from '../tokens/palette'

/** 外壳模式：应用端（居中收窄）或管理端（铺满可用宽度）。 */
export type LayoutMode = 'app' | 'admin'

const LayoutModeContext = createContext<LayoutMode>('app')

/** 布局模式上下文：由外层布局注入，页面外壳据此选择内容宽度策略。 */
export function LayoutModeProvider({ mode, children }: { mode: LayoutMode; children: ReactNode }): JSX.Element {
  return <LayoutModeContext.Provider value={mode}>{children}</LayoutModeContext.Provider>
}

/** 读取当前外壳模式（默认应用端）。 */
export function useLayoutMode(): LayoutMode {
  return useContext(LayoutModeContext)
}

export interface PageShellProps {
  title: ReactNode
  subtitle?: ReactNode
  extra?: ReactNode
  children?: ReactNode
}

/** 页面外壳：统一标题/副标题/操作区与留白。应用端居中且限宽，管理端铺满宽度并带标题栏分隔。 */
export function PageShell({ title, subtitle, extra, children }: PageShellProps): JSX.Element {
  const admin = useLayoutMode() === 'admin'
  return (
    <div
      style={{
        padding: spacing.lg,
        maxWidth: admin ? undefined : 1440,
        margin: admin ? 0 : '0 auto'
      }}
    >
      <div
        style={{
          display: 'flex',
          alignItems: 'flex-start',
          justifyContent: 'space-between',
          gap: spacing.md,
          marginBottom: spacing.lg,
          ...(admin ? { paddingBottom: spacing.sm, borderBottom: `1px solid ${palette.border}` } : {})
        }}
      >
        <div style={{ minWidth: 0 }}>
          <Typography.Title level={4} style={{ margin: 0 }}>
            {title}
          </Typography.Title>
          {subtitle ? (
            <Typography.Text type="secondary" style={{ fontSize: fontSize.sm }}>
              {subtitle}
            </Typography.Text>
          ) : null}
        </div>
        {extra ? <Space wrap>{extra}</Space> : null}
      </div>
      {children}
    </div>
  )
}
