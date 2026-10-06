import { useEffect, useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import { Button, Card, Col, Input, Pagination, Progress, Row, Segmented, Skeleton, Space, Typography } from 'antd'
import type { TableColumnsType } from 'antd'
import { AppstoreOutlined, BarsOutlined } from '@ant-design/icons'
import { useQuery } from '@tanstack/react-query'

import { DataTable } from '../../components/DataTable'
import { EmptyState } from '../../components/EmptyState'
import { ErrorNotice } from '../../components/ErrorNotice'
import { PageShell } from '../../components/PageShell'
import { StatusTag } from '../../components/StatusTag'
import { useApi } from '../../api/provider'
import type { RepoDTO } from '../../api/types'
import { useAuth } from '../../hooks/useAuth'
import { useI18n } from '../../i18n'
import { fontSize, palette, spacing } from '../../tokens/palette'
import { formatBytes, formatTime, repoUsage } from '../../utils/format'
import { repoModeLabel } from '../../utils/labels'
import { RepoCard, RepoRow } from './RepoCards'
import { RepoMountSettingsModal } from './RepoMountSettings'

export interface RepoListProps {
  isSuperAdmin: boolean
  onCreate: () => void
  onOpen: (repoId: string) => void
}

/** 用户端展示形式：卡片网格（默认，一眼扫过多块盘）/ 整页宽度的列表（一库一行，便于横向比对）。 */
type RepoViewMode = 'card' | 'list'

/**
 * 每页条数按形态分别取值：卡片一行 3~4 张（每页 8 张 = 两行上下），列表一行一库（每页 10 行）。
 * 两者都压在一屏上下，分页才有意义 —— 一页要滚三屏的话，用户只会直接找搜索框。
 */
const PAGE_SIZE: Record<RepoViewMode, number> = { card: 8, list: 10 }

/**
 * 展示形式的本地偏好键。
 *
 * 与客户端配置（`vault.client.config`，走 Electron IPC 写 config.json）**分开**：展示形式是纯界面偏好，
 * 只跟"这台机器的这个界面"有关，不值得为它多铺一条跨进程写盘路径；localStorage 在浏览器 dev 与
 * Electron 渲染进程两边都在，刷新/重开都还在。
 */
const VIEW_PREF_KEY = 'vault.repo.view'

/** 读取本地记住的展示形式：读不到、值不合法、存储不可用，一律退回默认的卡片。 */
function readStoredView(): RepoViewMode {
  try {
    const raw = window.localStorage.getItem(VIEW_PREF_KEY)
    return raw === 'list' || raw === 'card' ? raw : 'card'
  } catch {
    return 'card'
  }
}

/** 记住展示形式：localStorage 可能不可用（隐私模式、配额写满），写不进去就当没记住，绝不影响界面。 */
function storeView(view: RepoViewMode): void {
  try {
    window.localStorage.setItem(VIEW_PREF_KEY, view)
  } catch {
    // 忽略：偏好没记住不是错误，不值得打扰用户。
  }
}

/**
 * 用户端分组区块标题：标题 + 数量在左，`extra`（形态切换）贴右。
 *
 * 切换控件放在内容区第一个分组的标题行右侧，而不是页面头部工具栏：它只作用于这一块列表，
 * 与搜索/刷新/新建（页面级操作）不是一类东西；标题为空时（拿不到当前用户的分组兜底）只渲染右侧控件。
 */
function RepoGroupHeader({
  title,
  count,
  extra
}: {
  title: string
  count: number
  extra?: ReactNode
}): JSX.Element {
  return (
    <div
      style={{
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'space-between',
        gap: spacing.sm,
        marginBottom: spacing.sm
      }}
    >
      {title ? (
        <Space align="baseline" size={spacing.xs}>
          <Typography.Text strong>{title}</Typography.Text>
          <Typography.Text type="secondary" style={{ fontSize: fontSize.sm }}>
            {count}
          </Typography.Text>
        </Space>
      ) : (
        <span />
      )}
      {extra}
    </div>
  )
}

/**
 * 配额总览：把**用户级配额**摊在列表最上面。
 *
 * 为什么需要单独一块：配额是用户级的一份（名下所有库共用），而卡片上的进度条分母是
 * 各库自己的容量 —— 用户此前只能逐个库估算，"我总共用了多少、还剩多少"没有任何地方
 * 能回答（/v1/users/{id} 只对超管开放）。这里是「我的存储库」页面上唯一的总账。
 *
 * 取不到时整块不渲染：老服务端没有 /v1/auth/me，或请求失败时，页面照常用，
 * 不能因为一块统计把整个列表带崩。
 */
function QuotaOverview({ repos }: { repos: RepoDTO[] }): JSX.Element | null {
  const api = useApi()
  const { t } = useI18n()
  const meQuery = useQuery({ queryKey: ['me'], queryFn: () => api.me() })
  const user = meQuery.data
  if (!user) return null

  // quota_bytes = 0 表示不限（只受存储池容量约束），此时没有分母，不画进度条。
  const unlimited = user.quota_bytes <= 0
  const percent = unlimited ? 0 : Math.min(100, Math.round((user.used_bytes / user.quota_bytes) * 100))
  const creating = repos.filter((repo) => repo.state === 'creating').length

  return (
    <Card styles={{ body: { padding: spacing.md } }}>
      <div style={{ display: 'flex', alignItems: 'baseline', gap: spacing.xs, flexWrap: 'wrap' }}>
        <Typography.Text strong>{t('repo.quota.title')}</Typography.Text>
        <Typography.Text style={{ marginLeft: 'auto', fontSize: fontSize.sm }}>
          {`${t('repo.quota.used')} ${formatBytes(user.used_bytes)} / ${t('repo.quota.total')} ${
            unlimited ? t('common.unlimited') : formatBytes(user.quota_bytes)
          }`}
        </Typography.Text>
        {/* 不限配额时没有分母，百分比无意义（进度条也不画）。 */}
        {unlimited ? null : (
          <Typography.Text strong style={{ fontSize: fontSize.sm }}>
            {`${percent}%`}
          </Typography.Text>
        )}
      </div>
      {unlimited ? null : (
        <Progress
          percent={percent}
          showInfo={false}
          size="small"
          strokeColor={palette.accent}
          trailColor={palette.neutralFill}
          style={{ marginTop: spacing.xxs, marginBottom: 0 }}
        />
      )}
      <Space size={spacing.md} wrap style={{ marginTop: spacing.sm }}>
        {unlimited ? null : (
          <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
            {t('repo.quota.remaining')} {formatBytes(Math.max(0, user.quota_bytes - user.used_bytes))}
          </Typography.Text>
        )}
        <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
          {t('repo.quota.repos')} {repos.length}
        </Typography.Text>
        {creating > 0 ? (
          <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
            {t('repo.quota.creating')} {creating}
          </Typography.Text>
        ) : null}
      </Space>
    </Card>
  )
}

/** 卡片 / 列表形态切换：只挂在分组标题行右侧（见 RepoGroupHeader 的 extra）。 */
function RepoViewSwitch({
  value,
  onChange
}: {
  value: RepoViewMode
  onChange: (next: RepoViewMode) => void
}): JSX.Element {
  const { t } = useI18n()
  return (
    <Segmented
      value={value}
      onChange={(next) => onChange(next as RepoViewMode)}
      options={[
        {
          value: 'card',
          label: (
            <Space size={spacing.xs}>
              <AppstoreOutlined />
              {t('repo.view.card')}
            </Space>
          )
        },
        {
          value: 'list',
          label: (
            <Space size={spacing.xs}>
              <BarsOutlined />
              {t('repo.view.list')}
            </Space>
          )
        }
      ]}
    />
  )
}

interface RepoSectionProps {
  title: string
  items: RepoDTO[]
  viewMode: RepoViewMode
  currentUserId: string
  /** 标题行右侧的附加内容（形态切换）；只由列表页交给第一个分组。 */
  headerExtra?: ReactNode
  onOpen: (repoId: string) => void
  onConfigure: (repo: RepoDTO) => void
}

/**
 * 一个分组区块：标题 + 当前页 + 分页。
 *
 * 分页**每个分组各自一份**：两个分组的含义不同（我拥有的 / 我参与的），
 * 合成一个跨组的分页器时，"第 2 页"读到什么全凭两组各有几个库，很难理解。
 */
function RepoSection({
  title,
  items,
  viewMode,
  currentUserId,
  headerExtra,
  onOpen,
  onConfigure
}: RepoSectionProps): JSX.Element {
  const [page, setPage] = useState(1)
  const pageSize = PAGE_SIZE[viewMode]
  // 切换形态会换掉每页条数（卡片 8 / 列表 10），当前页可能已经越界；
  // 回到第 1 页最不容易迷失（越界本身另有兜底，见下面的 current）。
  useEffect(() => setPage(1), [viewMode])

  const pageCount = Math.max(1, Math.ceil(items.length / pageSize))
  const current = Math.min(page, pageCount)
  const visible = items.slice((current - 1) * pageSize, current * pageSize)

  return (
    <div>
      {title || headerExtra ? <RepoGroupHeader title={title} count={items.length} extra={headerExtra} /> : null}
      {viewMode === 'card' ? (
        <Row gutter={[spacing.md, spacing.md]}>
          {visible.map((repo) => (
            <Col key={repo.id} xs={24} sm={12} xl={8} xxl={6}>
              <RepoCard repo={repo} currentUserId={currentUserId} onOpen={onOpen} onConfigure={onConfigure} />
            </Col>
          ))}
        </Row>
      ) : (
        <Space direction="vertical" size={spacing.sm} style={{ width: '100%' }}>
          {visible.map((repo) => (
            <RepoRow key={repo.id} repo={repo} currentUserId={currentUserId} onOpen={onOpen} onConfigure={onConfigure} />
          ))}
        </Space>
      )}
      {items.length > pageSize ? (
        <div style={{ display: 'flex', justifyContent: 'center', marginTop: spacing.md }}>
          <Pagination
            size="small"
            current={current}
            pageSize={pageSize}
            total={items.length}
            showSizeChanger={false}
            onChange={setPage}
          />
        </div>
      ) : null}
    </div>
  )
}

/** 加载骨架：按当前形态出骨架（卡片网格 / 列表行），避免加载完"跳一下"。 */
function RepoListSkeleton({ viewMode }: { viewMode: RepoViewMode }): JSX.Element {
  if (viewMode === 'card') {
    return (
      <Row gutter={[spacing.md, spacing.md]}>
        {Array.from({ length: 8 }).map((_, index) => (
          <Col key={index} xs={24} sm={12} xl={8} xxl={6}>
            <Card style={{ height: '100%' }} styles={{ body: { padding: spacing.md } }}>
              <Skeleton active paragraph={{ rows: 3 }} />
            </Card>
          </Col>
        ))}
      </Row>
    )
  }
  return (
    <Space direction="vertical" size={spacing.sm} style={{ width: '100%' }}>
      {Array.from({ length: 6 }).map((_, index) => (
        <Card key={index} styles={{ body: { padding: `${spacing.sm}px ${spacing.md}px` } }}>
          <Skeleton active paragraph={{ rows: 1 }} />
        </Card>
      ))}
    </Space>
  )
}

/** 存储库列表：管理端表格；用户端卡片/列表两种形态（可切换 + 分组分页）。 */
export function RepoList({ isSuperAdmin, onCreate, onOpen }: RepoListProps): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const { user } = useAuth()
  const [keyword, setKeyword] = useState('')
  // 展示形式只影响用户端（管理端是一张运维表，形态切换对它没有意义）。
  // 初值取本地记住的那一份：用户上次选了列表，刷新/重开客户端仍是列表。
  const [viewMode, setViewMode] = useState<RepoViewMode>(readStoredView)
  /** 切换形态：即时生效并记住（下次进来直接是这一种）。 */
  const changeView = (next: RepoViewMode): void => {
    setViewMode(next)
    storeView(next)
  }
  // 挂载配置弹窗只服务当前点开的这一个库（弹窗在列表这一层只保留一份，避免每张卡都挂一个）。
  const [settingsRepo, setSettingsRepo] = useState<RepoDTO | null>(null)

  const reposQuery = useQuery({
    queryKey: ['repos', isSuperAdmin ? 'all' : 'mine'],
    queryFn: () => api.listRepos({ limit: 200 })
  })

  const usersQuery = useQuery({
    queryKey: ['users', 'options'],
    queryFn: () => api.listUsers({ limit: 200 }),
    enabled: isSuperAdmin
  })

  const ownerName = useMemo(() => {
    const map = new Map<string, string>()
    for (const item of usersQuery.data?.items ?? []) map.set(item.id, item.username)
    return map
  }, [usersQuery.data])

  const rows = useMemo(() => {
    const items = reposQuery.data?.items ?? []
    const lowered = keyword.trim().toLowerCase()
    if (!lowered) return items
    return items.filter((repo) => repo.name.toLowerCase().includes(lowered))
  }, [reposQuery.data, keyword])

  const columns = useMemo<TableColumnsType<RepoDTO>>(() => {
    const base: TableColumnsType<RepoDTO> = [
      { title: t('field.name'), dataIndex: 'name', key: 'name' },
      { title: t('field.mode'), dataIndex: 'mode', key: 'mode', width: 200, render: (value: string) => repoModeLabel(value) },
      { title: t('field.group'), dataIndex: 'group', key: 'group', render: (value?: string) => value || '-' },
      {
        // 与卡片同一口径（分母 = 库容量，见 repoUsage）；表格里只出文字，不出进度条。
        title: t('repo.used'),
        key: 'size',
        render: (_value, repo) => repoUsage(repo).text
      },
      {
        title: t('common.status'),
        dataIndex: 'state',
        key: 'state',
        width: 110,
        render: (value: string) => <StatusTag group="repo" value={value} />
      },
      {
        title: t('common.createdAt'),
        dataIndex: 'created_at',
        key: 'created_at',
        width: 170,
        render: (value: number) => formatTime(value)
      },
      {
        title: t('common.actions'),
        key: 'actions',
        width: 100,
        render: (_value, repo) => (
          <Button type="link" size="small" onClick={() => onOpen(repo.id)}>
            {t('repo.detail.title')}
          </Button>
        )
      }
    ]
    if (isSuperAdmin) {
      base.splice(2, 0, {
        title: t('field.owner'),
        dataIndex: 'owner_id',
        key: 'owner_id',
        render: (value: string) => ownerName.get(value) ?? value
      })
    }
    return base
  }, [isSuperAdmin, onOpen, ownerName, t])

  const renderUserRepos = (): JSX.Element => {
    if (reposQuery.isLoading) return <RepoListSkeleton viewMode={viewMode} />

    if (rows.length === 0) {
      return (
        <EmptyState
          title={t('repo.empty')}
          description={t('repo.empty.desc')}
          action={{ label: t('common.create'), onClick: onCreate }}
        />
      )
    }

    const userId = user?.id
    const sections = userId
      ? [
          { key: 'owned', title: t('repo.group.owned'), items: rows.filter((repo) => repo.owner_id === userId) },
          { key: 'joined', title: t('repo.group.joined'), items: rows.filter((repo) => repo.owner_id !== userId) }
        ]
      : [{ key: 'all', title: '', items: rows }]

    return (
      <Space direction="vertical" size={spacing.lg} style={{ width: '100%' }}>
        <QuotaOverview repos={rows} />
        {sections
          .filter((section) => section.items.length > 0)
          .map((section, index) => (
            // key 里带上关键字：搜索词一变就重挂载，页码随之回到第 1 页
            // （否则在第 3 页搜索很容易搜出"空结果"，其实结果在第 1 页）。
            <RepoSection
              key={`${section.key}-${keyword}`}
              title={section.title}
              items={section.items}
              viewMode={viewMode}
              currentUserId={userId ?? ''}
              // 切换控件只挂第一个分组（正常就是「我拥有的」）的标题行右侧：
              // 它管的是下面这一整片列表，挂一次就够；没有自己拥有的库时自然落到「我参与的」标题行。
              headerExtra={index === 0 ? <RepoViewSwitch value={viewMode} onChange={changeView} /> : undefined}
              onOpen={onOpen}
              onConfigure={setSettingsRepo}
            />
          ))}
      </Space>
    )
  }

  return (
    <PageShell
      title={t(isSuperAdmin ? 'page.repos.title' : 'page.myRepos.title')}
      extra={
        <>
          {/* 展示形式切换已移入内容区第一个分组标题行右侧（只服务用户端，见 RepoViewSwitch）。 */}
          <Input.Search
            allowClear
            placeholder={t('common.search')}
            style={{ width: 220 }}
            onChange={(event) => setKeyword(event.target.value)}
          />
          <Button onClick={() => void reposQuery.refetch()}>{t('common.refresh')}</Button>
          <Button type="primary" onClick={onCreate}>
            {t('common.create')}
          </Button>
        </>
      }
    >
      {reposQuery.error ? <ErrorNotice error={reposQuery.error} /> : null}
      {/* 每库独立的挂载配置：卡片「配置」图标打开，只改被点的这一个库 */}
      {settingsRepo ? (
        <RepoMountSettingsModal repo={settingsRepo} open onClose={() => setSettingsRepo(null)} />
      ) : null}
      {isSuperAdmin ? (
        <DataTable<RepoDTO>
          columns={columns}
          rows={rows}
          rowKey={(repo) => repo.id}
          loading={reposQuery.isLoading}
          empty={
            <EmptyState
              title={t('repo.empty')}
              description={t('repo.empty.desc')}
              action={{ label: t('common.create'), onClick: onCreate }}
            />
          }
        />
      ) : (
        renderUserRepos()
      )}
    </PageShell>
  )
}
