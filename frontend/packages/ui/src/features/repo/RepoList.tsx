import { useMemo, useState } from 'react'
import { Button, Card, Col, Input, Progress, Row, Skeleton, Space, Tag, Tooltip, Typography, message } from 'antd'
import type { TableColumnsType } from 'antd'
import { InfoCircleOutlined, SettingOutlined } from '@ant-design/icons'
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
import { RepoMountActions, RepoMountStateTag, RepoMountStatus, useRepoMount } from './RepoMount'
import { RepoMountSettingsModal } from './RepoMountSettings'

export interface RepoListProps {
  isSuperAdmin: boolean
  onCreate: () => void
  onOpen: (repoId: string) => void
}

interface RepoCardProps {
  repo: RepoDTO
  currentUserId: string
  onOpen: (repoId: string) => void
  onConfigure: (repo: RepoDTO) => void
}

/**
 * 用户端卡片：整卡可点打开本机挂载目录，容量进度条使用统一色板 token。
 *
 * 卡片上的三处动作分工（都只在这一张卡上完成，不必先进详情页）：
 *   - 整卡点击 = 打开文件（未挂载时如实提示）；
 *   - 「详情」图标 = 进详情页（默认停在「设置」tab）；
 *   - 「配置」图标 + 挂载按钮 = 这个库在本机怎么挂 / 挂上或卸下。
 */
function RepoCard({ repo, currentUserId, onOpen, onConfigure }: RepoCardProps): JSX.Element {
  const { t } = useI18n()
  const mountController = useRepoMount(repo, currentUserId)
  // 用量口径统一在 repoUsage 里（分母 = 库容量），卡片只负责画。
  const { percent, text: usage } = repoUsage(repo)

  /**
   * 整卡点击 = 打开本机挂载目录。
   *
   * 原来的「打开目录」小按钮撤掉了（按用户要求把"点击卡片进详情"换成"打开文件"）；
   * 未挂载时打开只会得到系统级"路径不存在"，这里直接如实提示未挂载。
   */
  const openMountDir = (): void => {
    if (!mountController.mounted) {
      void message.warning(t('repo.mount.notMounted'))
      return
    }
    void mountController.openMountDir()
  }

  return (
    <Card
      hoverable
      onClick={openMountDir}
      style={{ height: '100%' }}
      styles={{ body: { padding: spacing.md } }}
    >
      <div style={{ display: 'flex', alignItems: 'center', gap: spacing.xs }}>
        <Typography.Text
          strong
          ellipsis={{ tooltip: repo.name }}
          style={{ flex: 1, minWidth: 0, fontSize: fontSize.lg }}
        >
          {repo.name}
        </Typography.Text>
        <Tooltip title={t('repo.card.detail')}>
          <Button
            type="text"
            size="small"
            icon={<InfoCircleOutlined />}
            aria-label={t('repo.card.detail')}
            onClick={(event) => {
              event.stopPropagation()
              onOpen(repo.id)
            }}
          />
        </Tooltip>
      </div>

      <Space size={spacing.xs} wrap style={{ marginTop: spacing.xs }}>
        <Tag>{repoModeLabel(repo.mode)}</Tag>
        <StatusTag group="repo" value={repo.state} />
        {/* 挂载状态紧跟存储库状态：两个状态并排读，避免"看着正常却挂载失败"的困惑 */}
        <RepoMountStateTag controller={mountController} />
      </Space>

      <div style={{ marginTop: spacing.md }}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', gap: spacing.xs }}>
          <Typography.Text type="secondary" style={{ fontSize: fontSize.sm }}>
            {t('repo.used')}
          </Typography.Text>
          <Typography.Text style={{ fontSize: fontSize.sm }}>{usage}</Typography.Text>
        </div>
        <Progress
          percent={percent}
          showInfo={false}
          size="small"
          strokeColor={palette.accent}
          trailColor={palette.neutralFill}
          style={{ marginTop: spacing.xxs, marginBottom: 0 }}
        />
      </div>

      {/* 建库进度：只在这一刻有信息量（服务端正在预创建差异盘/目标），建完换回常驻信息行 */}
      <RepoPrepareProgress repo={repo} />

      <Space size={spacing.md} wrap style={{ marginTop: spacing.sm }}>
        <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
          {t('repo.card.shareCount')} {repo.share_count ?? repo.max_diff_disks}
        </Typography.Text>
        {/* 「已预创建」= 差异盘与 iSCSI 目标在建库时就备好：分配到手的盘挂载即用，不必现场下发 */}
        {repo.pool ? (
          <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
            {t('repo.card.pool')}
          </Typography.Text>
        ) : null}
        <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
          {t('repo.parentVersion')} {repo.parent_version}
        </Typography.Text>
      </Space>

      {/* 底部一行：左侧挂载点/失败原因，右侧「配置」+挂载按钮（按钮一律靠右，不被内容挤到左边） */}
      <div style={{ display: 'flex', alignItems: 'flex-start', gap: spacing.sm, marginTop: spacing.sm }}>
        <RepoMountStatus controller={mountController} />
        <div
          style={{
            marginLeft: 'auto',
            flexShrink: 0,
            display: 'flex',
            alignItems: 'center',
            gap: spacing.xs
          }}
        >
          {/* 配置图标紧挨挂载按钮左侧：改的是"这一个库在本机怎么挂"，与挂载是同一处动作 */}
          <Tooltip title={t('repo.settings.title')}>
            <Button
              type="text"
              size="small"
              icon={<SettingOutlined />}
              aria-label={t('repo.settings.title')}
              onClick={(event) => {
                event.stopPropagation()
                onConfigure(repo)
              }}
            />
          </Tooltip>
          <RepoMountActions controller={mountController} />
        </div>
      </div>
    </Card>
  )
}

/**
 * 建库进度：把服务端"预创建"的三个阶段摊给用户看（建母盘 → 派生差异盘 → 建 iSCSI 目标）。
 *
 * 只在 state=creating 且拿到进度时有意义：建库是个几十秒到几分钟的异步过程，
 * 只显示"建库中"三个字，用户无法区分"在建"和"卡住了"。
 */
function RepoPrepareProgress({ repo }: { repo: RepoDTO }): JSX.Element | null {
  const { t } = useI18n()
  const prepare = repo.prepare
  if (repo.state !== 'creating' || !prepare) return null
  const percent = prepare.total > 0 ? Math.min(100, Math.round((prepare.done / prepare.total) * 100)) : 0

  return (
    <div style={{ marginTop: spacing.sm }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', gap: spacing.xs }}>
        <Typography.Text type="secondary" style={{ fontSize: fontSize.sm }}>
          {t(`repo.prepare.${prepare.phase}`)}
        </Typography.Text>
        <Typography.Text type="secondary" style={{ fontSize: fontSize.sm }}>
          {`${prepare.done}/${prepare.total}`}
        </Typography.Text>
      </div>
      <Progress
        percent={percent}
        showInfo={false}
        size="small"
        strokeColor={palette.accent}
        trailColor={palette.neutralFill}
        style={{ marginTop: spacing.xxs, marginBottom: 0 }}
      />
    </div>
  )
}

/** 用户端分组区块标题：标题 + 数量，不加解释性文案。 */
function RepoGroupHeader({ title, count }: { title: string; count: number }): JSX.Element {
  return (
    <Space align="baseline" size={spacing.xs} style={{ marginBottom: spacing.sm }}>
      <Typography.Text strong>{title}</Typography.Text>
      <Typography.Text type="secondary" style={{ fontSize: fontSize.sm }}>
        {count}
      </Typography.Text>
    </Space>
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

/** 存储库列表：管理端表格；用户端响应式卡片（按归属分组）。 */
export function RepoList({ isSuperAdmin, onCreate, onOpen }: RepoListProps): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const { user } = useAuth()
  const [keyword, setKeyword] = useState('')
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
    if (reposQuery.isLoading) {
      return (
        <Row gutter={spacing.md}>
          {Array.from({ length: 6 }).map((_, index) => (
            <Col key={index} xs={24} sm={12} xl={8} xxl={6}>
              <Card style={{ height: '100%' }} styles={{ body: { padding: spacing.md } }}>
                <Skeleton active paragraph={{ rows: 3 }} />
              </Card>
            </Col>
          ))}
        </Row>
      )
    }

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
          .map((section) => (
            <div key={section.key}>
              {section.title ? <RepoGroupHeader title={section.title} count={section.items.length} /> : null}
              <Row gutter={spacing.md}>
                {section.items.map((repo) => (
                  <Col key={repo.id} xs={24} sm={12} xl={8} xxl={6}>
                    <RepoCard
                      repo={repo}
                      currentUserId={userId ?? ''}
                      onOpen={onOpen}
                      onConfigure={setSettingsRepo}
                    />
                  </Col>
                ))}
              </Row>
            </div>
          ))}
      </Space>
    )
  }

  return (
    <PageShell
      title={t(isSuperAdmin ? 'page.repos.title' : 'page.myRepos.title')}
      extra={
        <>
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
