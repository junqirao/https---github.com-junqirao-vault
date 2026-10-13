import { useEffect, useMemo, useRef, useState } from 'react'
import { Modal, Typography } from 'antd'
import dayjs from 'dayjs'
import * as echarts from 'echarts/core'
import { LineChart } from 'echarts/charts'
import { GridComponent, TooltipComponent } from 'echarts/components'
import { CanvasRenderer } from 'echarts/renderers'
import type { EChartsOption } from 'echarts'

import { agentApi } from '../../api/agentClient'
import type { AgentCacheSample } from '../../api/agentTypes'
import { useI18n } from '../../i18n'
import { fontSize, palette, spacing } from '../../tokens/palette'
import { formatBytes } from '../../utils/format'

// 只注册用到的图表/组件，走 echarts 的按需引入（体积远小于整包）。
echarts.use([LineChart, GridComponent, TooltipComponent, CanvasRenderer])

/**
 * 「缓存指标曲线」弹窗：近 5 分钟（每 5 秒一点）的命中率 / IOPS / 缓存占用曲线。
 *
 * 数据来自 GET /agent/cache/history（代理侧只记累计计数，速率在这里按相邻点差分）。
 * 绘图用 ECharts（按真实反馈："建议你用 echarts 去做"）：坐标轴、tooltip、平滑缩放
 * 都由它接管，比之前手写的 SVG 折线可靠得多。
 */

/** 一条折线：name 用于 tooltip，data 与采样点对齐，null 表示该时刻无数据（断线）。 */
interface Series {
  name: string
  color: string
  data: (number | null)[]
}

/** ECharts 的薄封装：负责 init/resize/dispose，option 变化时整份替换。 */
function EChart({ option, height }: { option: EChartsOption; height: number }): JSX.Element {
  const hostRef = useRef<HTMLDivElement | null>(null)
  const chartRef = useRef<ReturnType<typeof echarts.init> | null>(null)

  useEffect(() => {
    if (!hostRef.current) return
    const chart = echarts.init(hostRef.current)
    chartRef.current = chart
    const onResize = (): void => chart.resize()
    window.addEventListener('resize', onResize)
    return () => {
      window.removeEventListener('resize', onResize)
      chart.dispose()
      chartRef.current = null
    }
  }, [])

  useEffect(() => {
    // notMerge=true：系列数量会变（例如 L2 未启用时少一条），必须整份替换而不是合并。
    chartRef.current?.setOption(option, true)
  }, [option])

  return <div ref={hostRef} style={{ width: '100%', height }} />
}

/**
 * 组装一张折线图的 option。
 *
 * percent 为真时 y 轴按 0..1 百分比显示；`unit` 是"这张图的数带单位"的格式化函数
 * （目前只有字节类的缓存占用图给）：**tooltip 与坐标轴都要走它**，否则图上只有标题栏
 * 那一处有人话单位，鼠标一碰就是 "8388608.0" 这样一坨裸字节数（真实反馈："展示单位…
 * 没法看"）—— 坐标轴同样会退化成裸数字，还得靠人自己去数几位数。
 *
 * 不带单位的图（IOPS 这类计数）不传 unit：坐标轴保持原始刻度数字 —— 拿 IOPS 的
 * "不足 100 补一位小数"去渲染刻度会得到 "0.0 / 500 / 1000" 这种不一致的刻度。
 */
function lineOption(
  times: string[],
  series: Series[],
  opts: { percent?: boolean; unit?: (v: number) => string } = {}
): EChartsOption {
  const valueText = opts.percent ? (v: number): string => `${(v * 100).toFixed(0)}%` : opts.unit
  const axisText = opts.percent ? (v: number): string => `${Math.round(v * 100)}%` : opts.unit
  return {
    animation: false,
    // 左侧留 56：带单位的刻度标签（"1.20 GB"）比纯数字宽，48 会把首位挤掉。
    grid: { left: 56, right: 12, top: 12, bottom: 24 },
    tooltip: {
      trigger: 'axis',
      valueFormatter: (v) =>
        typeof v === 'number' ? (valueText ?? ((x: number) => x.toFixed(1)))(v) : '—'
    },
    xAxis: {
      type: 'category',
      data: times,
      boundaryGap: false,
      axisTick: { show: false },
      axisLine: { lineStyle: { color: palette.neutralFill } },
      axisLabel: { fontSize: 10, color: palette.textTertiary, hideOverlap: true }
    },
    yAxis: {
      type: 'value',
      min: 0,
      max: opts.percent ? 1 : undefined,
      splitLine: { lineStyle: { color: palette.neutralFill } },
      axisLabel: {
        fontSize: 10,
        color: palette.textTertiary,
        formatter: axisText ?? '{value}'
      }
    },
    series: series.map((s) => ({
      name: s.name,
      type: 'line',
      data: s.data,
      showSymbol: false,
      connectNulls: false,
      lineStyle: { width: 1.6, color: s.color },
      itemStyle: { color: s.color }
    }))
  }
}

/** 单张指标图：HTML 标题（含最新值）+ ECharts 绘图区。 */
function MetricChart({
  times,
  title,
  series,
  format,
  percent,
  unit
}: {
  times: string[]
  title: string
  series: Series[]
  /** 标题右侧"最新值"的渲染。 */
  format: (v: number) => string
  percent?: boolean
  /** 数值**带单位**（字节类）：坐标轴与 tooltip 也用 format 渲染，不给则坐标轴是裸数字。 */
  unit?: boolean
}): JSX.Element {
  const option = useMemo(
    () => lineOption(times, series, { percent, unit: unit ? format : undefined }),
    [times, series, percent, unit, format]
  )

  return (
    <div>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', gap: spacing.sm }}>
        <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
          {title}
        </Typography.Text>
        <span style={{ display: 'inline-flex', gap: spacing.sm }}>
          {series.map((s) => {
            const last = [...s.data].reverse().find((v) => v !== null && Number.isFinite(v))
            return (
              <Typography.Text key={s.name} type="secondary" style={{ fontSize: fontSize.xs }}>
                <span
                  aria-hidden
                  style={{
                    display: 'inline-block',
                    width: 8,
                    height: 8,
                    borderRadius: 4,
                    background: s.color,
                    marginRight: 4
                  }}
                />
                {s.name} {last !== undefined && last !== null ? format(last) : '—'}
              </Typography.Text>
            )
          })}
        </span>
      </div>
      <EChart option={option} height={132} />
    </div>
  )
}

/** 相邻采样点差分成速率（每秒）；目标重建导致计数归零时该区间为 null（断线而不是画假值）。 */
function rateOf(samples: AgentCacheSample[], pick: (s: AgentCacheSample) => number): (number | null)[] {
  return samples.map((s, i) => {
    if (i === 0) return null
    const prev = samples[i - 1]
    const dt = s.t - prev.t
    const dv = pick(s) - pick(prev)
    if (dt <= 0 || dv < 0) return null
    return dv / dt
  })
}

export interface CacheMetricsDialogProps {
  open: boolean
  /** 库的分配 ID（GET /agent/cache/history 的 targets 键）。 */
  allocationId: string
  repoName: string
  /** 该库是否开启了缓存：开缓存展示命中率/占用等全套曲线，未开只展示读写 IOPS。 */
  cacheEnabled: boolean
  onClose: () => void
}

export function CacheMetricsDialog({
  open,
  allocationId,
  repoName,
  cacheEnabled,
  onClose
}: CacheMetricsDialogProps): JSX.Element {
  const { t } = useI18n()
  const [samples, setSamples] = useState<AgentCacheSample[]>([])

  // 弹窗打开期间每 5 秒（与代理采样间隔一致）拉一次历史；失败保留上一份数据，不打扰用户。
  useEffect(() => {
    if (!open) return
    let stopped = false
    const load = async (): Promise<void> => {
      try {
        const history = await agentApi.getCacheHistory()
        if (!stopped) setSamples(history.targets[allocationId] ?? [])
      } catch {
        /* 保留上一份数据 */
      }
    }
    void load()
    const timer = setInterval(() => void load(), 5000)
    return () => {
      stopped = true
      clearInterval(timer)
    }
  }, [open, allocationId])

  const fmtIops = (v: number): string => (v >= 100 ? v.toFixed(0) : v.toFixed(1))
  const fmtPct = (v: number): string => `${(v * 100).toFixed(0)}%`

  // x 轴共用同一串时刻：各图按采样点对齐，逐点传下去（不再用模块级可变状态）。
  const times = useMemo(() => samples.map((s) => dayjs(s.t * 1000).format('HH:mm:ss')), [samples])

  const hitRate: (number | null)[] = samples.map((s, i) => {
    if (i === 0) return null
    const prev = samples[i - 1]
    const dr = s.reads - prev.reads
    const dh = s.hits - prev.hits
    if (dr <= 0 || dh < 0) return null
    return dh / dr
  })

  return (
    <Modal
      open={open}
      title={`${repoName} · ${t('repo.monitor.title')}`}
      footer={null}
      width={760}
      onCancel={onClose}
      destroyOnHidden
    >
      <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
        {t('repo.cache.chart.window')}
      </Typography.Text>
      {samples.length < 2 ? (
        <div style={{ padding: `${spacing.xl}px 0`, textAlign: 'center' }}>
          <Typography.Text type="secondary">{t('repo.cache.chart.empty')}</Typography.Text>
        </div>
      ) : (
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: spacing.md, marginTop: spacing.sm }}>
          {/* 未开缓存的库只有读写 IOPS：它经本机代理直通，没有命中/占用可看 */}
          {cacheEnabled ? (
            <MetricChart
              times={times}
              title={t('repo.cache.chart.hitRate')}
              series={[{ name: '', color: palette.success, data: hitRate }]}
              format={fmtPct}
              percent
            />
          ) : null}
          <MetricChart
            times={times}
            title={t('repo.cache.chart.read')}
            series={
              cacheEnabled
                ? [
                    { name: t('repo.cache.chart.read'), color: palette.accent, data: rateOf(samples, (s) => s.reads) },
                    { name: t('repo.cache.chart.readHit'), color: palette.success, data: rateOf(samples, (s) => s.hits) }
                  ]
                : [
                    { name: t('repo.cache.chart.read'), color: palette.accent, data: rateOf(samples, (s) => s.reads) }
                  ]
            }
            format={fmtIops}
          />
          <MetricChart
            times={times}
            title={t('repo.cache.chart.write')}
            series={[{ name: t('repo.cache.chart.write'), color: palette.warning, data: rateOf(samples, (s) => s.writes) }]}
            format={fmtIops}
          />
          {cacheEnabled ? (
            <MetricChart
              times={times}
              title={t('repo.cache.chart.usage')}
              series={[
                { name: 'L1', color: palette.accent, data: samples.map((s) => s.l1_used_bytes) },
                { name: 'L2', color: palette.warning, data: samples.map((s) => s.l2_used_bytes) }
              ]}
              format={formatBytes}
              unit
            />
          ) : null}
        </div>
      )}
    </Modal>
  )
}
