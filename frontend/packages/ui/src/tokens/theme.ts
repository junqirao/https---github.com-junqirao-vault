import type { ThemeConfig } from 'antd'

import { fontFamily, fontSize, palette, radius, spacing } from './palette'

/**
 * 由设计 token 生成 antd 主题。
 *
 * 刻意关闭 antd 全局动效（motion: false）与透明效果，符合"简洁大气、无装饰动效"的取向。
 * 例外只有一处、由业务样式精确指定（见 apps/client/index.html 与 tokens/palette.ts 顶部说明）：
 * 卡片悬停阴影的 0.2s 浮起过渡。
 */
export function createVaultTheme(): ThemeConfig {
  return {
    token: {
      colorPrimary: palette.accent,
      colorPrimaryHover: palette.accentHover,
      colorPrimaryActive: palette.accentActive,
      colorInfo: palette.info,
      colorSuccess: palette.success,
      colorWarning: palette.warning,
      colorError: palette.danger,

      colorText: palette.textPrimary,
      colorTextSecondary: palette.textSecondary,
      colorTextTertiary: palette.textTertiary,

      colorBgLayout: palette.bgPage,
      colorBgContainer: palette.bgContainer,
      colorBgElevated: palette.bgElevated,

      colorBorder: palette.border,
      colorBorderSecondary: palette.border,

      borderRadius: radius.md,
      borderRadiusLG: radius.lg,
      borderRadiusSM: radius.sm,

      fontFamily,
      fontSize: fontSize.md,
      fontSizeLG: fontSize.lg,
      fontSizeSM: fontSize.sm,
      fontSizeHeading1: fontSize.xxl,
      fontSizeHeading2: fontSize.xl,
      fontSizeHeading3: fontSize.lg,

      controlHeight: 36,
      lineWidth: 1,
      wireframe: false,
      motion: false,
      padding: spacing.md,
      margin: spacing.md
    },
    components: {
      Layout: {
        headerBg: palette.bgContainer,
        headerHeight: 56,
        headerPadding: `0 ${spacing.lg}px`,
        bodyBg: palette.bgPage,
        siderBg: palette.bgContainer,
        triggerBg: palette.bgContainer
      },
      Menu: {
        itemBg: 'transparent',
        itemSelectedBg: palette.neutralFill,
        itemSelectedColor: palette.accent,
        itemHeight: 38,
        itemMarginInline: spacing.xs,
        activeBarWidth: 0
      },
      Card: {
        headerBg: palette.bgContainer,
        paddingLG: spacing.lg
      },
      Table: {
        headerBg: palette.bgPage,
        rowHoverBg: palette.bgHover
      },
      Steps: {
        iconSize: 28
      },
      Descriptions: {
        labelBg: palette.bgPage
      },
      /**
       * 状态点尺寸：默认 `fontSizeSM / 2`（本项目为 6px），这里对齐设计里的 8px。
       *
       * 只有一处用到：库名前的挂载状态圆点（features/repo/RepoCards.tsx 的 RepoMountDot）。
       * 该圆点现在是 antd `Badge` 的色点，尺寸只能从组件 token 给 —— 与 16px 的库名同处一行时
       * 8px 不抢戏但一眼可辨（真实反馈："如果没有挂载则展示灰色，挂载失败展示红色"）。
       */
      Badge: {
        statusSize: 8
      }
    }
  }
}
