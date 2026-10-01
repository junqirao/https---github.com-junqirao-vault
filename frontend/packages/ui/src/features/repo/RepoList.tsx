import { useMemo, useState } from 'react'
import { Button, Card, Col, Input, Progress, Row, Skeleton, Space, Tag, Tooltip, Typography } from 'antd'
import type { TableColumnsType } from 'antd'
import { FolderOpenOutlined } from '@ant-design/icons'
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
import { formatBytes, formatTime } from '../../utils/format'
import { repoModeLabel } from '../../utils/labels'
import { RepoMountActions, RepoMountStateTag, RepoMountStatus, useRepoMount } from './RepoMount'

export interface RepoListProps {
  isSuperAdmin: boolean
  onCreate: () => void
  onOpen: (repoId: string) => void
}

interface RepoCardProps {
  repo: RepoDTO
  currentUserId: string
  onOpen: (repoId: string) => void
}

/**
 * 用户端卡片：整卡可点进入详情，容量进度条使用统一色板 token。
 *
 * 卡片上直接提供挂载能力：状态 + 一键挂载/卸载；右上角「打开」在已挂载时
 * 打开本地挂载目录（未挂载不可点），避免为了一次挂载先进详情页。
 */
function RepoCard({ repo, currentUserId, onOpen }: RepoCardProps): JSX.Element {
  const { t } = useI18n()
  const mountController = useRepoMount(repo, currentUserId)
  // 库**没有配额**：库只有"容量"（建库时设定），这个容量就是它占用的配额。
  // 因此卡片上只展示"这个库占用了多少配额"；只有存量数据里残留了库级配额（>0）时才带分母。
  const percent = repo.quota_bytes > 0 ? Math.min(100, Math.round((repo.used_bytes / repo.quota_bytes) * 100)) : 0
  const capacity =
    repo.quota_bytes > 0
      ? `${formatBytes(repo.used_bytes)} / ${formatBytes(repo.quota_bytes)}`
      : formatBytes(repo.used_bytes)

  return (
    <Card
      hoverable
      onClick={() => onOpen(repo.id)}
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
        <Tooltip title={t('agent.action.openDirectory')}>
          <Button
            type="text"
            size="small"
            icon={<FolderOpenOutlined />}
            aria-label={t('agent.action.openDirectory')}
            disabled={!mountController.mounted}
            onClick={(event) => {
              event.stopPropagation()
              void mountController.openMountDir()
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
          <Typography.Text style={{ fontSize: fontSize.sm }}>{capacity}</Typography.Text>
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

      <Space size={spacing.md} wrap style={{ marginTop: spacing.sm }}>
        <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
          {t('field.maxDiffDisks')} {repo.max_diff_disks}
        </Typography.Text>
        <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
          {t('repo.parentVersion')} {repo.parent_version}
        </Typography.Text>
      </Space>

      {/* 底部一行：左侧挂载点/失败原因，右侧挂载按钮（按钮一律靠右，不被内容挤到左边） */}
      <div style={{ display: 'flex', alignItems: 'flex-start', gap: spacing.sm, marginTop: spacing.sm }}>
        <RepoMountStatus controller={mountController} />
        <div style={{ marginLeft: 'auto', flexShrink: 0 }}>
          <RepoMountActions controller={mountController} />
        </div>
      </div>
    </Card>
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

/** 存储库列表：管理端表格；用户端响应式卡片（按归属分组）。 */
export function RepoList({ isSuperAdmin, onCreate, onOpen }: RepoListProps): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const { user } = useAuth()
  const [keyword, setKeyword] = useState('')

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
        // 与卡片一致：库只有容量、没有配额，这里展示"该库占用了多少配额"。
        title: t('repo.used'),
        key: 'size',
        render: (_value, repo) =>
          repo.quota_bytes > 0
            ? `${formatBytes(repo.used_bytes)} / ${formatBytes(repo.quota_bytes)}`
            : formatBytes(repo.used_bytes)
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
        {sections
          .filter((section) => section.items.length > 0)
          .map((section) => (
            <div key={section.key}>
              {section.title ? <RepoGroupHeader title={section.title} count={section.items.length} /> : null}
              <Row gutter={spacing.md}>
                {section.items.map((repo) => (
                  <Col key={repo.id} xs={24} sm={12} xl={8} xxl={6}>
                    <RepoCard repo={repo} currentUserId={userId ?? ''} onOpen={onOpen} />
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
