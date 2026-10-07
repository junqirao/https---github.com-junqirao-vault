import type { CSSProperties } from 'react'
import { Tooltip } from 'antd'

import { fontSize, palette, radius, spacing } from '../tokens/palette'
import { formatBytes } from '../utils/format'

/**
 * 容量进度条的告警分档：75% 起转琥珀（该考虑清理了）、90% 起转红（马上要满）。
 *
 * 90% 这条线刻意与后端建卷闸门 defaultWatermarkPercent 对齐 —— 到这里为止，
 * 界面上的红条和"再建卷会被拒"是同一个事实，不会出现"看着还绿、其实已经建不进去"。
 */
const WARN_PERCENT = 75
const DANGER_PERCENT = 90

/** 进度条填充色：常态用主色，接近上限转琥珀，逼近上限转红。 */
function barColor(percent: number): string {
  if (percent >= DANGER_PERCENT) return palette.danger
  if (percent >= WARN_PERCENT) return palette.warning
  return palette.accent
}

export interface StorageOptionProps {
  /** 第一排：存储名称（深灰黑粗体）。 */
  name: string
  /** 第二排：挂载路径（灰色小字；过长省略，悬停看全路径）。 */
  path: string
  /** 已用字节。 */
  usedBytes: number
  /** 容量字节；<=0 表示容量未知（如所在卷查不到容量），此时只报已用量。 */
  totalBytes: number
}

/**
 * 存储下拉里的富选项：三排 = 名称 / 路径 / 容量进度条。
 *
 * 进度条：已用与容量写在右侧，百分比**居中压在条上**。
 * 文字要同时压在两段底色上（左半是彩色填充、右半是浅色轨道），因此同一个百分比
 * 画两份、各按填充边界裁一半：深色那份只露在轨道上，白色那份只露在填充上 ——
 * 无论进度多少，数字都清晰可读（这是不用 antd Progress 内嵌文案的原因：
 * 它的文字会随填充变宽而被挤到左边，百分比就不在"条中间"了）。
 */
export function StorageOption({ name, path, usedBytes, totalBytes }: StorageOptionProps): JSX.Element {
  const known = totalBytes > 0 && Number.isFinite(totalBytes)
  const percent = known ? Math.min(100, Math.max(0, Math.round((usedBytes / totalBytes) * 100))) : 0
  const color = barColor(percent)
  const label = known ? `${percent}%` : '-'

  const centeredLabel: CSSProperties = {
    position: 'absolute',
    inset: 0,
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    fontSize: fontSize.xs,
    lineHeight: 1,
    fontVariantNumeric: 'tabular-nums'
  }

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 2, padding: `${spacing.xxs}px 0` }}>
      <span style={{ color: palette.textPrimary, fontWeight: 600 }}>{name}</span>
      <Tooltip title={path || '-'} mouseEnterDelay={0.4}>
        <span
          style={{
            display: 'block',
            color: palette.textTertiary,
            fontSize: fontSize.xs,
            whiteSpace: 'nowrap',
            overflow: 'hidden',
            textOverflow: 'ellipsis'
          }}
        >
          {path || '-'}
        </span>
      </Tooltip>
      <div style={{ display: 'flex', alignItems: 'center', gap: spacing.xs }}>
        <div
          style={{
            position: 'relative',
            flex: 1,
            minWidth: 80,
            height: 16,
            borderRadius: radius.pill,
            backgroundColor: palette.neutralFill,
            overflow: 'hidden'
          }}
        >
          {percent > 0 ? (
            <div
              style={{
                position: 'absolute',
                insetBlock: 0,
                insetInlineStart: 0,
                width: `${percent}%`,
                borderRadius: radius.pill,
                backgroundColor: color
              }}
            />
          ) : null}
          <span style={{ ...centeredLabel, color: palette.textSecondary, clipPath: `inset(0 0 0 ${percent}%)` }}>
            {label}
          </span>
          <span
            style={{
              ...centeredLabel,
              color: palette.textInverse,
              clipPath: `inset(0 ${100 - percent}% 0 0)`
            }}
          >
            {label}
          </span>
        </div>
        <span
          style={{
            color: palette.textSecondary,
            fontSize: fontSize.xs,
            whiteSpace: 'nowrap',
            fontVariantNumeric: 'tabular-nums'
          }}
        >
          {known ? `${formatBytes(usedBytes)} / ${formatBytes(totalBytes)}` : formatBytes(usedBytes)}
        </span>
      </div>
    </div>
  )
}
