import type { ThemeConfig } from 'antd'

import { fontFamily, fontSize, palette, radius, spacing } from './palette'

/**
 * 由设计 token 生成 antd 主题。
 *
 * 刻意关闭动效（motion: false）与透明效果，符合"简洁大气、无动效"的取向。
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
      }
    }
  }
}
