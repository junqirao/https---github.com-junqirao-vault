import { useMemo, useState } from 'react'
import { Button, Input } from 'antd'
import type { TableColumnsType } from 'antd'
import { useQuery } from '@tanstack/react-query'

import { DataTable } from '../../components/DataTable'
import { EmptyState } from '../../components/EmptyState'
import { ErrorNotice } from '../../components/ErrorNotice'
import { PageShell } from '../../components/PageShell'
import { StatusTag } from '../../components/StatusTag'
import { useApi } from '../../api/provider'
import type { AuditDTO } from '../../api/types'
import { useI18n } from '../../i18n'
import { formatTime } from '../../utils/format'

/** 审计日志（仅超级管理员）。 */
export function AuditList(): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const [action, setAction] = useState('')
  const [userId, setUserId] = useState('')

  const auditQuery = useQuery({
    queryKey: ['audit', action, userId],
    queryFn: () => api.auditLogs({ action: action || undefined, user_id: userId || undefined, limit: 200 })
  })

  const columns = useMemo<TableColumnsType<AuditDTO>>(
    () => [
      { title: t('common.createdAt'), dataIndex: 'created_at', key: 'created_at', width: 170, render: (value: number) => formatTime(value) },
      { title: t('field.user'), dataIndex: 'user_id', key: 'user_id', width: 220, render: (value: string) => <span style={{ fontSize: 12 }}>{value || '-'}</span> },
      { title: t('field.action'), dataIndex: 'action', key: 'action', width: 200 },
      { title: t('field.resource'), dataIndex: 'resource', key: 'resource', render: (value: string) => <span style={{ fontSize: 12 }}>{value || '-'}</span> },
      { title: t('field.detail'), dataIndex: 'detail', key: 'detail', render: (value: string) => <span style={{ fontSize: 12 }}>{value || '-'}</span> },
      { title: t('field.ip'), dataIndex: 'ip', key: 'ip', width: 140 },
      {
        title: t('field.result'),
        dataIndex: 'result',
        key: 'result',
        width: 110,
        render: (value: string) => <StatusTag group="audit" value={value} />
      }
    ],
    [t]
  )

  return (
    <PageShell
      title={t('page.audit.title')}
      extra={
        <>
          <Input
            allowClear
            placeholder={t('audit.filter.action')}
            style={{ width: 200 }}
            onChange={(event) => setAction(event.target.value)}
          />
          <Input
            allowClear
            placeholder={t('audit.filter.userId')}
            style={{ width: 220 }}
            onChange={(event) => setUserId(event.target.value)}
          />
          <Button onClick={() => void auditQuery.refetch()}>{t('common.refresh')}</Button>
        </>
      }
    >
      {auditQuery.error ? <ErrorNotice error={auditQuery.error} /> : null}
      <DataTable<AuditDTO>
        columns={columns}
        rows={auditQuery.data?.items ?? []}
        rowKey={(log) => log.id}
        loading={auditQuery.isLoading}
        empty={<EmptyState title={t('audit.empty')} />}
        scroll={{ x: 1200 }}
      />
    </PageShell>
  )
}
