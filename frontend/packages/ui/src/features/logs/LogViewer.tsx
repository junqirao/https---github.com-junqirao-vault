import { useCallback, useMemo, useState } from 'react'
import { Button, Checkbox, DatePicker, Input, Select, Space, Tag, Typography } from 'antd'
import type { TableColumnsType } from 'antd'
import { ReloadOutlined } from '@ant-design/icons'
import { useQuery } from '@tanstack/react-query'
import dayjs, { type Dayjs } from 'dayjs'

import { agentApi } from '../../api/agentClient'
import { ApiError } from '../../api/errors'
import { useApi } from '../../api/provider'
import { DataTable } from '../../components/DataTable'
import { EmptyState } from '../../components/EmptyState'
import { ErrorNotice } from '../../components/ErrorNotice'
import { PageShell } from '../../components/PageShell'
import { SectionCard } from '../../components/SectionCard'
import { useI18n } from '../../i18n'
import { fontSize, palette, radius, spacing } from '../../tokens/palette'

/**
 * 一次读取的日志字节数。
 *
 * 日志按天切分后单份文件仍可能很大，所以只取尾部：太小看不到上下文，太大拖慢解析与渲染。
 * 1MiB 大约对应 4~6 千条记录，配合分页足够排查，再多就该缩时间范围了。
 */
const TAIL_BYTES = 1024 * 1024

/** 日志级别（slog JSON 输出为大写 LEVEL，比较时统一小写）。 */
const LEVELS = ['debug', 'info', 'warn', 'error'] as const

/** 结构化字段的等宽字体（与日志原文一致，便于按列对齐阅读）。 */
const MONO_FONT = 'Consolas, "Courier New", monospace'

/** 一条解析后的日志。*/
interface LogEntry {
  /** 行号：同一份日志内稳定唯一，供表格 rowKey 与展开行使用。 */
  id: string
  /** 是否为合法 JSON 行。尾部可能从半行开始，解析失败的要能原样展示。 */
  parsed: boolean
  ts: number | null
  level: string
  msg: string
  /** 除 ts_ms/level/msg 之外的结构化字段（保持原始顺序）。 */
  fields: [string, unknown][]
  /** 原始对象，供展开行展示完整 JSON。 */
  json: Record<string, unknown> | null
  raw: string
  /** 预置的小写原文，供关键字过滤直接复用。 */
  search: string
}

/** 解析一行 slog JSON；不是合法对象（含尾部截断的半行）时退化为纯文本行。 */
function parseEntry(line: string, index: number): LogEntry {
  const base: LogEntry = {
    id: String(index),
    parsed: false,
    ts: null,
    level: '',
    msg: line,
    fields: [],
    json: null,
    raw: line,
    search: line.toLowerCase()
  }
  let value: unknown
  try {
    value = JSON.parse(line) as unknown
  } catch {
    return base
  }
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return base

  const record = { ...(value as Record<string, unknown>) }
  const ts = typeof record.ts_ms === 'number' ? record.ts_ms : null
  const level = typeof record.level === 'string' ? record.level : ''
  const msg = typeof record.msg === 'string' ? record.msg : ''
  delete record.ts_ms
  delete record.level
  delete record.msg
  return { ...base, parsed: true, ts, level, msg, fields: Object.entries(record), json: value as Record<string, unknown> }
}

/** 把原始日志文本（JSON Lines）解析成结构化记录。 */
function parseEntries(text: string): LogEntry[] {
  const lines = text.split('\n')
  const out: LogEntry[] = []
  for (let index = 0; index < lines.length; index += 1) {
    const line = lines[index] ?? ''
    if (line.trim() === '') continue
    out.push(parseEntry(line, index))
  }
  return out
}

/** 字段值 → 单行摘要（字符串原样，对象/数组用紧凑 JSON）。 */
function summarize(value: unknown): string {
  if (typeof value === 'string') return value
  if (value === null || value === undefined) return String(value)
  if (typeof value === 'number' || typeof value === 'boolean') return String(value)
  try {
    return JSON.stringify(value) ?? String(value)
  } catch {
    return String(value)
  }
}

/** 级别标签：error / warn 上色，info / debug 保持中性，避免满屏高饱和色块。 */
function LevelTag({ level }: { level: string }): JSX.Element {
  const value = level.toLowerCase()
  const color =
    value === 'error' ? palette.danger : value === 'warn' || value === 'warning' ? palette.warning : undefined
  return (
    <Tag bordered={false} color={color} style={{ borderRadius: radius.sm, marginInlineEnd: 0 }}>
      {level ? level.toUpperCase() : '-'}
    </Tag>
  )
}

/** 日志来源：本机代理（客户端）日志，或服务端日志。 */
export type LogSourceKind = 'agent' | 'server'

export interface LogViewerProps {
  /**
   * 日志来源：
   *  - 'agent'（默认）：本机代理（客户端）日志，读 GET /agent/log；
   *  - 'server'：服务端日志，读 GET /v1/system/logs（管理端「服务日志」页，仅超级管理员）。
   *
   * 两者不能混用：代理日志只有本机能读，管理员在别的机器上打开管理控制台时，
   * 代理日志只会显示自己这台客户端的记录，与服务端发生了什么毫无关系。
   */
  source?: LogSourceKind
}

/**
 * 日志查看器（客户端「日志」页 / 管理端「服务日志」页共用）。
 *
 * 为什么要有它：挂载失败时原始报错（如 Connect-IscsiTarget 的 .NET 异常）只进日志文件，
 * 界面只能看到稳定码（真实反馈："在客户端上加一个日志模块……不然什么都看不到"）。
 * 管理端同理：服务端内部报错只进服务端日志，管理员必须能就地看到。
 *
 * 日志本身是结构化 JSON，直接铺成控制台既难读也翻不动，因此这里：
 * - 按天切分（日志文件本来就一天一份），先选日期再看；
 * - 解析成表格（时间 / 级别 / 消息 / 字段），字段多时可以展开看完整 JSON；
 * - 支持时间范围、级别、关键字过滤与分页，避免日志一多就没法看。
 */
export function LogViewer({ source = 'agent' }: LogViewerProps): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const fromServer = source === 'server'
  /** 选中的日期；空串表示"当天"（由代理回填 data.day）。 */
  const [day, setDay] = useState('')
  const [level, setLevel] = useState<string>('all')
  const [keyword, setKeyword] = useState('')
  const [range, setRange] = useState<[Dayjs, Dayjs] | null>(null)
  /** 最新的排在最前（日志文件按时间正序写入，默认倒序看）。 */
  const [newestFirst, setNewestFirst] = useState(true)

  const logQuery = useQuery({
    // 两种来源的缓存互不覆盖（同一台机器上两个页面可能同时打开）。
    queryKey: ['logs', source, day],
    queryFn: () =>
      fromServer
        ? api.systemLogs({ day: day || undefined, tailBytes: TAIL_BYTES })
        : agentApi.log({ day: day || undefined, tailBytes: TAIL_BYTES })
  })
  const data = logQuery.data

  const entries = useMemo(() => parseEntries(data?.text ?? ''), [data?.text])

  const filtered = useMemo(() => {
    const kw = keyword.trim().toLowerCase()
    const from = range ? range[0].valueOf() : null
    const to = range ? range[1].valueOf() : null
    return entries.filter((entry) => {
      if (level !== 'all' && entry.level.toLowerCase() !== level) return false
      // 时间范围只对能解析出时间的行生效：无时间戳的行不该被"筛掉了却看不出来"。
      if (from !== null && (entry.ts === null || entry.ts < from)) return false
      if (to !== null && (entry.ts === null || entry.ts > to)) return false
      if (kw !== '' && !entry.search.includes(kw)) return false
      return true
    })
  }, [entries, keyword, level, range])

  const rows = useMemo(() => (newestFirst ? [...filtered].reverse() : filtered), [filtered, newestFirst])

  const dayOptions = useMemo(() => {
    const days = data?.days ?? []
    // 当天文件可能尚未落盘（Days 里没有它），但它是本次实际读取的日期，必须可选中。
    const list = data?.day && !days.includes(data.day) ? [...days, data.day] : days
    return [...list].sort().reverse().map((value) => ({ value, label: value }))
  }, [data])

  /**
   * 读不动日志文件时回的稳定错误码（代理与被查询的服务端返回同形结构）。
   * 包成业务错误才能走到统一的按码本地化提示（错误码本身也照样贴出来，方便排障）。
   */
  const logError = useMemo(
    () => (data?.error ? new ApiError({ kind: 'business', code: data.error }) : null),
    [data?.error]
  )

  const columns = useMemo<TableColumnsType<LogEntry>>(
    () => [
      {
        title: t('log.col.time'),
        dataIndex: 'ts',
        key: 'ts',
        width: 190,
        render: (_: unknown, entry: LogEntry) =>
          entry.ts === null ? '-' : dayjs(entry.ts).format('YYYY-MM-DD HH:mm:ss.SSS')
      },
      {
        title: t('log.col.level'),
        dataIndex: 'level',
        key: 'level',
        width: 96,
        render: (_: unknown, entry: LogEntry) => <LevelTag level={entry.level} />
      },
      {
        title: t('log.col.message'),
        dataIndex: 'msg',
        key: 'msg',
        ellipsis: true,
        // 日志正文是"需要被复制"的内容（贴到工单/群里排查），显式放开选中。
        render: (value: string) => <span className="selectable">{value}</span>
      },
      {
        title: t('log.col.fields'),
        key: 'fields',
        width: 420,
        ellipsis: true,
        render: (_: unknown, entry: LogEntry) =>
          entry.fields.length === 0 ? (
            '-'
          ) : (
            <span className="selectable" style={{ fontFamily: MONO_FONT, fontSize: fontSize.xs }}>
              {entry.fields.map(([key, value]) => `${key}=${summarize(value)}`).join('  ')}
            </span>
          )
      }
    ],
    [t]
  )

  const resetFilters = useCallback(() => {
    setLevel('all')
    setKeyword('')
    setRange(null)
  }, [])

  return (
    <PageShell
      title={fromServer ? t('page.serverLogs.title') : t('page.logs.title')}
      extra={
        <Button icon={<ReloadOutlined />} loading={logQuery.isFetching} onClick={() => void logQuery.refetch()}>
          {t('common.refresh')}
        </Button>
      }
    >
      <SectionCard title={t('log.filter.title')}>
        <Space direction="vertical" size={spacing.sm} style={{ width: '100%' }}>
          <Space wrap size={spacing.sm}>
            <Select
              style={{ width: 170 }}
              value={day || data?.day}
              loading={logQuery.isLoading}
              onChange={setDay}
              options={dayOptions}
              placeholder={t('log.filter.day')}
            />
            <DatePicker.RangePicker
              showTime
              style={{ width: 380 }}
              value={range}
              format="YYYY-MM-DD HH:mm:ss"
              placeholder={[t('log.filter.from'), t('log.filter.to')]}
              onChange={(dates) => {
                const start = dates?.[0]
                const end = dates?.[1]
                setRange(start && end ? [start, end] : null)
              }}
            />
            <Select
              style={{ width: 140 }}
              value={level}
              onChange={setLevel}
              options={[
                { value: 'all', label: t('log.level.all') },
                ...LEVELS.map((item) => ({ value: item, label: item.toUpperCase() }))
              ]}
            />
            <Input
              allowClear
              style={{ width: 240 }}
              placeholder={t('log.keyword')}
              value={keyword}
              onChange={(event) => setKeyword(event.target.value)}
            />
            <Checkbox checked={newestFirst} onChange={(event) => setNewestFirst(event.target.checked)}>
              {t('log.newestFirst')}
            </Checkbox>
            <Button onClick={resetFilters}>{t('log.filter.reset')}</Button>
          </Space>
          {/* 日志文件路径是排查时要贴给别人的内容，给一个复制按钮。 */}
          <Typography.Text
            type="secondary"
            className="selectable"
            style={{ fontSize: fontSize.xs }}
            copyable={data?.path ? { text: data.path } : false}
          >
            {t('log.path')}: {data?.path || '-'}
          </Typography.Text>
        </Space>
      </SectionCard>

      <SectionCard
        title={t('log.entries.title')}
        extra={
          <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
            {t('log.entries.count', { shown: filtered.length, total: entries.length })}
          </Typography.Text>
        }
        flush
      >
        {logQuery.error ? (
          <div style={{ padding: spacing.md }}>
            <ErrorNotice error={logQuery.error} />
          </div>
        ) : logError ? (
          <div style={{ padding: spacing.md }}>
            <ErrorNotice error={logError} />
          </div>
        ) : null}
        {/* 分页直接用表格自带的 Pagination（antd 框架组件），不另做一套；
            条数总量已经由卡片标题右侧给出，分页里不再重复一遍。 */}
        <DataTable<LogEntry>
          columns={columns}
          rows={rows}
          rowKey={(entry) => entry.id}
          loading={logQuery.isLoading}
          empty={<EmptyState title={t('log.empty')} description={t('log.emptyHint')} />}
          scroll={{ x: 1080 }}
          pagination={{ pageSize: 50, showSizeChanger: true }}
          expandable={{
            expandedRowRender: (entry) => {
              const detail = entry.parsed ? JSON.stringify(entry.json, null, 2) : entry.raw
              return (
                <>
                  {/* 展开的完整 JSON 是典型的"要贴出去"的内容：给一个显式复制入口，
                      另外整块文本也放开选中（见 index.html 的全局禁选）。 */}
                  <Typography.Text
                    type="secondary"
                    style={{ display: 'block', marginBottom: spacing.xxs, fontSize: fontSize.xs }}
                    copyable={{ text: detail, tooltips: [t('common.copy'), t('common.copied')] }}
                  >
                    {t('log.detail.copy')}
                  </Typography.Text>
                  <pre
                    className="selectable"
                    style={{
                      margin: 0,
                      fontFamily: MONO_FONT,
                      fontSize: fontSize.xs,
                      lineHeight: 1.6,
                      whiteSpace: 'pre-wrap',
                      wordBreak: 'break-all'
                    }}
                  >
                    {detail}
                  </pre>
                </>
              )
            }
          }}
        />
      </SectionCard>
    </PageShell>
  )
}
