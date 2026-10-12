import type { CSSProperties } from 'react'
import { Progress } from 'antd'

import { fontSize, palette, spacing } from '../tokens/palette'

export interface UsageBarProps {
  /** 0–100（调用方负责钳位与取整，口径见 utils/format 的 repoUsage）。 */
  percent: number
  style?: CSSProperties
}

/**
 * 用量进度条：条**加厚到刚好容下百分数**，百分数画在条的**正中间**。
 *
 * 为什么不用 antd 自带的 showInfo：
 *   - line 形态的 info 落在条的**右侧**（不在条里），不是"中间"；
 *   - small 尺寸的条只有 6px 高，字根本塞不进去；
 *   - 新增的 percentPosition={{ type: 'inner' }} 是把字塞进**实心段**里，且实心段会被
 *     撑到"至少一个字宽"（minWidth: max-content）—— 用量 1% 的库会画出一截看着像 15% 的
 *     蓝条，对"用量"这种"条长 = 比例"的图来说是在说谎。
 * 所以：条仍用组件（拿它的 aria 语义与配色），百分数自己绝对居中叠一层。
 *
 * 两处"看起来多余"的写法都是在压 antd 的 inline-block 行盒：
 *   - 外层 lineHeight: 0、Progress 的 fontSize/lineHeight: 0 —— 行盒的基线间隙会让外层比
 *     条本身高出一截，居中的字就会整体偏低；压掉之后外层高度 = 条的高度（16px）。
 *   - 于是叠层的 inset: 0 与条**严丝合缝**，字落在条的正中心。
 *
 * 字用「按进度分色」的渐变裁切：压在蓝色实心段上的部分取白字、压在浅灰底纹上的部分取
 * 深灰字 —— 任何单一颜色总有一头糊在底色里（5% 时白字看不见，95% 时深字看不见）。
 *
 * 卡片、列表行与详情页共用这一个，保证同一条数据在哪儿看都长得一样。
 */
export function UsageBar({ percent, style }: UsageBarProps): JSX.Element {
  const clamped = Math.min(100, Math.max(0, Math.round(percent)))
  return (
    <div style={{ position: 'relative', lineHeight: 0, marginTop: spacing.xxs, ...style }}>
      {/* size 只给 height：line 形态下传单个数字是"宽高都设成它"（条会变成 16px 长的方块），
          传对象才是"只改粗细"，正好 = 条的厚度。 */}
      <Progress
        percent={clamped}
        showInfo={false}
        size={{ height: 16 }}
        strokeColor={palette.accent}
        trailColor={palette.neutralFill}
        style={{ marginBottom: 0, fontSize: 0, lineHeight: 0 }}
      />
      <span
        style={{
          position: 'absolute',
          inset: 0,
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
          fontSize: fontSize.xs,
          fontWeight: 600,
          lineHeight: 1,
          pointerEvents: 'none',
          backgroundImage: `linear-gradient(90deg, ${palette.textInverse} 0 ${clamped}%, ${palette.textSecondary} ${clamped}% 100%)`,
          WebkitBackgroundClip: 'text',
          backgroundClip: 'text',
          color: 'transparent'
        }}
      >
        {`${clamped}%`}
      </span>
    </div>
  )
}
