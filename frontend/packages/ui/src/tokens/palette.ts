/**
 * 设计变量（唯一真源）。
 *
 * 风格取向：简洁大气、偏苹果风；**不做动效与透明效果**。
 * 所有间距/圆角/字号在此集中定义，再由 createVaultTheme() 映射为 antd theme token。
 */

/**
 * 色板（克制、低饱和）。**唯一真源**：任何地方都不要另写十六进制状态色。
 *
 * 状态色的取值刻意避开 antd 默认的 `#52C41A` 及其派生的发灰浅色 success 背景，
 * 改用更现代、更克制的 Tailwind 色阶；它们同时映射为 antd 主题 token
 * （见 theme.ts 的 colorSuccess / colorWarning / colorError），
 * 因此 Alert / Button / 图标 / 状态标签会统一使用同一套状态色。
 */
export const palette = {
  /** 主色：系统蓝。 */
  accent: '#0A6CFF',
  accentHover: '#3B87FF',
  accentActive: '#0053CC',

  textPrimary: 'rgba(0, 0, 0, 0.88)',
  textSecondary: 'rgba(0, 0, 0, 0.62)',
  textTertiary: 'rgba(0, 0, 0, 0.45)',
  textInverse: '#FFFFFF',

  bgPage: '#F5F5F7',
  bgContainer: '#FFFFFF',
  bgElevated: '#FFFFFF',
  bgHover: 'rgba(0, 0, 0, 0.03)',

  border: 'rgba(0, 0, 0, 0.10)',
  borderStrong: 'rgba(0, 0, 0, 0.18)',

  /** 成功：#16A34A（Tailwind green-600）。 */
  success: '#16A34A',
  /** 警告：#D97706（Tailwind amber-600）。 */
  warning: '#D97706',
  /** 危险：#DC2626（Tailwind red-600）。 */
  danger: '#DC2626',
  info: '#0A6CFF',

  neutralFill: 'rgba(0, 0, 0, 0.04)'
} as const

/** 间距刻度（4 的倍数）。 */
export const spacing = {
  xxs: 4,
  xs: 8,
  sm: 12,
  md: 16,
  lg: 24,
  xl: 32,
  xxl: 48
} as const

/** 圆角刻度。 */
export const radius = {
  sm: 6,
  md: 10,
  lg: 14,
  pill: 999
} as const

/** 字号刻度。 */
export const fontSize = {
  xs: 12,
  sm: 13,
  md: 14,
  lg: 16,
  xl: 20,
  xxl: 28
} as const

/** 布局尺寸。 */
export const layout = {
  sidebarWidth: 220,
  headerHeight: 56,
  contentMaxWidth: 1200
} as const

/** 无衬线字体栈（优先系统字体，贴近 macOS 观感）。 */
export const fontFamily =
  '-apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", "Hiragino Sans GB", "Microsoft YaHei", Helvetica, Arial, sans-serif'
