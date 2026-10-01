import { useState } from 'react'
import { Alert, Button, Space, Tag, Tooltip, Typography } from 'antd'
import type { TableColumnsType } from 'antd'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { ConfirmDialog } from '../../components/ConfirmDialog'
import { DataTable } from '../../components/DataTable'
import { EmptyState } from '../../components/EmptyState'
import { ErrorNotice } from '../../components/ErrorNotice'
import { PageShell } from '../../components/PageShell'
import { useApi } from '../../api/provider'
import type { OrphanFileDTO, OrphanKind } from '../../api/types'
import { useI18n } from '../../i18n'
import { spacing } from '../../tokens/palette'
import { formatBytes, formatTime } from '../../utils/format'

/** 孤儿清单的缓存键；删除成功后失效它即触发重新扫描。 */
const ORPHAN_QUERY_KEY = ['system', 'orphans']

/** 分类配色：差异盘/母盘可能承载用户数据，用暖色提醒；其余保持中性。 */
const KIND_COLOR: Record<OrphanKind, string> = {
  diff: 'orange',
  parent: 'blue',
  other: 'default'
}

/** 分类的 i18n key（与后端 app.OrphanKind* 一致）。 */
function kindKey(kind: OrphanKind): string {
  if (kind === 'diff') return 'orphan.kind.diff'
  if (kind === 'parent') return 'orphan.kind.parent'
  return 'orphan.kind.other'
}

/**
 * 孤儿磁盘（仅超级管理员）：现扫存储根下"未被数据库登记却真实存在的 .vhdx"，逐个手动删除。
 *
 * 为什么要有这一页：对账（reconcile）发现这类文件时只报告（危险操作默认 AutoFixed=false），
 * 表现就是一屏 WARN 日志 —— 用户看到却无处处理。这里把同一份扫描结果做成可操作清单，
 * 把不可逆的删除留在管理员的判断之下（差异盘/母盘可能承载用户数据）。
 */
export function OrphanDisks(): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const [error, setError] = useState<unknown>(null)
  const [pendingDelete, setPendingDelete] = useState<OrphanFileDTO | null>(null)

  // 进页面即现扫一遍：用户点开要看的是"此刻磁盘上真实存在什么"，而不是对账报告里的旧快照。
  const scanQuery = useQuery({
    queryKey: ORPHAN_QUERY_KEY,
    queryFn: () => api.scanOrphans(),
    refetchOnMount: 'always'
  })
  const scan = scanQuery.data
  const files = scan?.files ?? []
  const skipped = scan?.skipped ?? []

  const deleteMutation = useMutation({
    mutationFn: (path: string) => api.deleteOrphan(path),
    onSuccess: () => {
      setError(null)
      setPendingDelete(null)
      void queryClient.invalidateQueries({ queryKey: ORPHAN_QUERY_KEY })
    },
    // 失败留在确认框里展示原因：清单是扫描那一刻的快照，文件可能已被重新用上
    // （system.orphan_registered）或已被占用（system.orphan_delete_failed）。
    onError: (err) => setError(err)
  })

  const columns: TableColumnsType<OrphanFileDTO> = [
    {
      title: t('orphan.col.path'),
      dataIndex: 'path',
      key: 'path',
      render: (value: string) => (
        <Tooltip title={value}>
          <Typography.Text code copyable={{ text: value, tooltips: [t('common.copy'), t('common.copied')] }} style={{ maxWidth: 520 }} ellipsis>
            {value}
          </Typography.Text>
        </Tooltip>
      )
    },
    {
      title: t('orphan.col.root'),
      dataIndex: 'root',
      key: 'root',
      width: 200,
      render: (value: string) => (
        <Tooltip title={value}>
          <Typography.Text type="secondary" ellipsis style={{ maxWidth: 180 }}>
            {value}
          </Typography.Text>
        </Tooltip>
      )
    },
    {
      title: t('orphan.col.kind'),
      dataIndex: 'kind',
      key: 'kind',
      width: 130,
      render: (kind: OrphanKind) =>
        kind === 'diff' ? (
          <Tooltip title={t('orphan.kindHint.diff')}>
            <Tag color={KIND_COLOR[kind]}>{t(kindKey(kind))}</Tag>
          </Tooltip>
        ) : (
          <Tag color={KIND_COLOR[kind]}>{t(kindKey(kind))}</Tag>
        )
    },
    {
      title: t('orphan.col.size'),
      dataIndex: 'size_bytes',
      key: 'size_bytes',
      width: 120,
      render: (value: number) => formatBytes(value)
    },
    {
      title: t('orphan.col.modifiedAt'),
      dataIndex: 'modified_at',
      key: 'modified_at',
      width: 180,
      render: (value: number) => formatTime(value)
    },
    {
      title: t('common.actions'),
      key: 'actions',
      width: 100,
      render: (_value, file) => (
        <Button
          type="link"
          size="small"
          danger
          loading={deleteMutation.isPending && deleteMutation.variables === file.path}
          onClick={() => {
            setError(null)
            setPendingDelete(file)
          }}
        >
          {t('orphan.delete')}
        </Button>
      )
    }
  ]

  return (
    <PageShell
      title={t('orphan.title')}
      subtitle={t('orphan.subtitle')}
      extra={
        <Button
          type="primary"
          loading={scanQuery.isFetching}
          onClick={() => {
            setError(null)
            void scanQuery.refetch()
          }}
        >
          {t('orphan.rescan')}
        </Button>
      }
    >
      {error ? <ErrorNotice error={error} /> : null}
      {scanQuery.error ? <ErrorNotice error={scanQuery.error} /> : null}

      <Alert type="info" showIcon message={t('orphan.reconciledHint')} style={{ marginBottom: spacing.md }} />
      <Alert type="warning" showIcon message={t('orphan.dangerHint')} style={{ marginBottom: spacing.md }} />

      {skipped.length > 0 ? (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: spacing.md }}
          message={t('orphan.skipped')}
          description={
            <Space direction="vertical" size={0}>
              {skipped.map((reason) => (
                <Typography.Text key={reason} style={{ fontSize: 12 }}>
                  {reason}
                </Typography.Text>
              ))}
            </Space>
          }
        />
      ) : null}

      {scan ? (
        <Space direction="vertical" size={0} style={{ marginBottom: spacing.sm }}>
          <Typography.Text type="secondary">
            {files.length > 0
              ? t('orphan.summary', {
                  checked: scan.checked,
                  count: files.length,
                  size: formatBytes(scan.total_bytes)
                })
              : t('orphan.checkedOnly', { checked: scan.checked })}
          </Typography.Text>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            {t('orphan.sizeNote')}
          </Typography.Text>
        </Space>
      ) : null}

      <DataTable<OrphanFileDTO>
        columns={columns}
        rows={files}
        rowKey={(file) => file.path}
        loading={scanQuery.isLoading}
        empty={<EmptyState title={t('orphan.empty')} description={t('orphan.emptyDesc')} />}
        scroll={{ x: 1200 }}
      />

      <ConfirmDialog
        open={pendingDelete !== null}
        danger
        title={t('orphan.deleteTitle')}
        content={
          <>
            <div>{t('orphan.deleteConfirm')}</div>
            {pendingDelete ? (
              <Typography.Text code style={{ display: 'block', marginTop: spacing.sm }}>
                {pendingDelete.path}
              </Typography.Text>
            ) : null}
            {error ? (
              <div style={{ marginTop: spacing.sm }}>
                <ErrorNotice error={error} />
              </div>
            ) : null}
          </>
        }
        loading={deleteMutation.isPending}
        onConfirm={() => {
          if (!pendingDelete) return
          setError(null)
          deleteMutation.mutate(pendingDelete.path)
        }}
        onCancel={() => setPendingDelete(null)}
      />
    </PageShell>
  )
}
