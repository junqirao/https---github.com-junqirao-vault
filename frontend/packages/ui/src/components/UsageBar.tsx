import type { CSSProperties } from 'react'
import { Progress } from 'antd'
import { LoadingOutlined } from '@ant-design/icons'

import { fontSize, palette, spacing } from '../tokens/palette'

/** 叠在条正中的那一层（百分数或转圈）：靠 inset: 0 与条严丝合缝，且不吃鼠标事件。 */
const overlayStyle: CSSProperties = {
  position: 'absolute',
  inset: 0,
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
  lineHeight: 1,
  pointerEvents: 'none'
}

/** 用量分档阈值（百分数）：≥70% 起警示，≥90% 起告急。 */
const USAGE_WARNING_PERCENT = 70
const USAGE_DANGER_PERCENT = 90

/**
 * 用量配色：条的颜色随占比分档（真实反馈："进度条的颜色要随存储用量的占比而变化"）。
 *
 * 分档只在"快满了"时换色，所以阈值取 70/90 两档而不是平均切三段：用量一半、六成的库满地
 * 都是，那些条理应长得一样、安安静静；真正需要眼睛停一下的是"这盘要满了"那几块。
 * 70% 起变成橙色 = "该留意了"，90% 起变红 = "该清数据 / 加盘了"（那时再写一点就可能满，
 * 而库满了的表现是客户机里直接写失败）。
 *
 * 充裕档用**主色蓝**而不是绿：绿色在全站是"成功"（见 tokens/palette 的 success），
 * 而"用了 30%"不是一件被评价为好或坏的事，涂成绿色会被读成系统对它的状态判定；
 * 蓝色还与全站主色一致 —— 正常状态下这条进度条是不抢眼的，褪到背景里正合适。
 *
 * 管理端存储池表格的"数据占用"条也用这个（同一件事在哪儿看都是同一套颜色）。
 */
export function usageColor(percent: number): string {
  if (percent >= USAGE_DANGER_PERCENT) return palette.danger
  if (percent >= USAGE_WARNING_PERCENT) return palette.warning
  return palette.accent
}

export interface UsageBarProps {
  /** 0–100（调用方负责钳位与取整，口径见 utils/format 的 repoUsage）。 */
  percent: number
  /**
   * 置灰：没有可用的用量时只画一条**空轨道**（不画填充、也不画百分数）。
   *
   * 未挂载本机的库给不出"用户用了多少"—— 服务端的 used_bytes 是**已分配磁盘总量**
   * （按盘标称容量计费，等于库容量），拿它当"已用"就是在报一个会被误读的数。此时把条留成灰占位：
   * 位置在、形状在，挂上之后数字自然出现（与「监控」入口未挂载时置灰同一个做法）。
   */
  muted?: boolean
  /**
   * 正在取数（刚挂上这块库，代理那边还没量出来，见 useAgent 的 repoUsageLoading）：
   * 条里不填、不写百分数，只在正中叠一个转圈。
   *
   * 与置灰的区别只在**说了什么**：置灰说"不知道"（`-`），转圈说"正在读" —— 用户刚点完挂载，
   * 看到一条灰到底的空条只会以为没挂上，看到一个转圈才知道再等一下就有数。
   */
  loading?: boolean
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
 * 所以：条仍用组件（拿它的 aria 语义），颜色自己按分档给（见 usageColor），
 * 百分数自己绝对居中叠一层。
 *
 * 两处"看起来多余"的写法都是在压 antd 的 inline-block 行盒：
 *   - 外层 lineHeight: 0、Progress 的 fontSize/lineHeight: 0 —— 行盒的基线间隙会让外层比
 *     条本身高出一截，居中的字就会整体偏低；压掉之后外层高度 = 条的高度（16px）。
 *   - 于是叠层的 inset: 0 与条**严丝合缝**，字落在条的正中心。
 *
 * 字用「按进度分色」的渐变裁切：压在实心段上的部分取白字、压在浅灰底纹上的部分取深灰字
 * —— 任何单一颜色总有一头糊在底色里（5% 时白字看不见，95% 时深字看不见）。实心段换档
 * （蓝/橙/红，见 usageColor）不影响这套裁切：三档底色都压得住白字。
 *
 * 卡片、列表行与详情页共用这一个，保证同一条数据在哪儿看都长得一样。
 *
 * 没有数字可报时有**两种**玩法，都在条正中表达，不再写百分数（写了就是"0%"这个假数）：
 *   - 置灰（muted，未挂载本机）：只剩这条灰轨道本身，把位置占住；
 *   - 取数中（loading，刚挂上、还在量）：灰轨道 + 一个转圈。
 */
export function UsageBar({ percent, muted = false, loading = false, style }: UsageBarProps): JSX.Element {
  // 取数中与置灰一样：不填、不写数 —— 这一刻没有数字可填，填 0 或写 "0%" 都会被读成
  // "一个字节没用"（而 loading 只在"本来要写 `-`"时才会为真，见 props 的说明）。
  const blank = muted || loading
  const clamped = blank ? 0 : Math.min(100, Math.max(0, Math.round(percent)))
  // 空条的颜色给轨道色：不留"该是什么档"的悬念，也不必让分档函数去操心一个没画出来的颜色。
  const strokeColor = blank ? palette.neutralFill : usageColor(clamped)
  return (
    <div style={{ position: 'relative', lineHeight: 0, marginTop: spacing.xxs, ...style }}>
      {/* size 只给 height：line 形态下传单个数字是"宽高都设成它"（条会变成 16px 长的方块），
          传对象才是"只改粗细"，正好 = 条的厚度。 */}
      <Progress
        percent={clamped}
        showInfo={false}
        size={{ height: 16 }}
        strokeColor={strokeColor}
        trailColor={palette.neutralFill}
        style={{ marginBottom: 0, fontSize: 0, lineHeight: 0 }}
      />
      {loading ? (
        // 转圈用深灰：条的底纹是浅灰，白色或彩色的转圈都会糊在底上。
        <span style={overlayStyle}>
          <LoadingOutlined spin style={{ fontSize: fontSize.xs, color: palette.textTertiary }} />
        </span>
      ) : muted ? null : (
        <span
          style={{
            ...overlayStyle,
            fontSize: fontSize.xs,
            fontWeight: 600,
            backgroundImage: `linear-gradient(90deg, ${palette.textInverse} 0 ${clamped}%, ${palette.textSecondary} ${clamped}% 100%)`,
            WebkitBackgroundClip: 'text',
            backgroundClip: 'text',
            color: 'transparent'
          }}
        >
          {`${clamped}%`}
        </span>
      )}
    </div>
  )
}

/**
 * 「已用 / 上限」的数值行：与用量条成对出现的那句话。
 *
 * 用量未知写 `-`（口径见 utils/format 的 repoUsage）；`loading` 时把那半边换成转圈 ——
 * 与条上的转圈说的是同一件事（"正在读"），一个在数上、一个在条上。整句没法局部替换，
 * 所以这里收的是两半文字（见 repoUsage 的返回值）。
 */
export function UsageValueText({
  usedText,
  totalText,
  loading = false
}: {
  /** 「已用」那半边：未知时是 `-`。 */
  usedText: string
  /** 「上限」那半边；上限未知时为空串。 */
  totalText: string
  loading?: boolean
}): JSX.Element {
  return (
    <>
      {loading ? (
        <LoadingOutlined spin style={{ fontSize: fontSize.xs, color: palette.textTertiary }} />
      ) : (
        usedText
      )}
      {totalText ? ` / ${totalText}` : ''}
    </>
  )
}
