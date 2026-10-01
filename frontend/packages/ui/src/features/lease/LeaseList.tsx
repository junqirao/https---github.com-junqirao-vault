import { useMemo, useState } from 'react'
import { Button } from 'antd'
import type { TableColumnsType } from 'antd'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { ConfirmDialog } from '../../components/ConfirmDialog'
import { DataTable } from '../../components/DataTable'
import { EmptyState } from '../../components/EmptyState'
import { ErrorNotice } from '../../components/ErrorNotice'
import { PageShell } from '../../components/PageShell'
import { StatusTag } from '../../components/StatusTag'
import { useApi } from '../../api/provider'
import type { LeaseDTO } from '../../api/types'
import { useI18n } from '../../i18n'
import { formatTime } from '../../utils/format'

export interface LeaseListProps {
  /** 仅超级管理员可强制下线。 */
  canRevoke: boolean
}

/** 在线租约列表（权威来源为服务端 leases 表）。 */
export function LeaseList({ canRevoke }: LeaseListProps): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const [error, setError] = useState<unknown>(null)
  const [pendingRevoke, setPendingRevoke] = useState<LeaseDTO | null>(null)

  const leasesQuery = useQuery({
    queryKey: ['leases'],
    queryFn: () => api.listLeases(),
    refetchInterval: 15000
  })

  const usersQuery = useQuery({
    queryKey: ['users', 'options'],
    queryFn: () => api.listUsers({ limit: 200 }),
    enabled: canRevoke
  })

  const userName = useMemo(() => {
    const map = new Map<string, string>()
    for (const user of usersQuery.data?.items ?? []) map.set(user.id, user.username)
    return map
  }, [usersQuery.data])

  const revokeMutation = useMutation({
    mutationFn: (leaseId: string) => api.revokeLease(leaseId),
    onSuccess: () => {
      setPendingRevoke(null)
      void queryClient.invalidateQueries({ queryKey: ['leases'] })
    },
    onError: (err) => setError(err)
  })

  const columns: TableColumnsType<LeaseDTO> = [
    { title: t('lease.field.target'), dataIndex: 'target_name', key: 'target_name' },
    {
      title: t('field.user'),
      dataIndex: 'user_id',
      key: 'user_id',
      render: (value: string) => userName.get(value) ?? value
    },
    { title: t('field.clientId'), dataIndex: 'client_id', key: 'client_id', render: (value: string) => <span style={{ fontSize: 12 }}>{value}</span> },
    { title: t('field.mountPoint'), dataIndex: 'mount_point', key: 'mount_point', render: (value?: string) => value || '-' },
    {
      title: t('common.status'),
      dataIndex: 'state',
      key: 'state',
      width: 130,
      render: (value: string) => <StatusTag group="lease" value={value} />
    },
    { title: t('lease.field.lastSeen'), dataIndex: 'last_seen_at', key: 'last_seen_at', width: 170, render: (value: number) => formatTime(value) },
    { title: t('common.expiresAt'), dataIndex: 'expires_at', key: 'expires_at', width: 170, render: (value: number) => formatTime(value) },
    ...(canRevoke
      ? [
          {
            title: t('common.actions'),
            key: 'actions',
            width: 120,
            render: (_value: unknown, lease: LeaseDTO) => (
              <Button
                type="link"
                size="small"
                danger
                disabled={lease.state !== 'active'}
                onClick={() => setPendingRevoke(lease)}
              >
                {t('action.revoke')}
              </Button>
            )
          } as TableColumnsType<LeaseDTO>[number]
        ]
      : [])
  ]

  return (
    <PageShell
      title={t('page.leases.title')}
      extra={<Button onClick={() => void leasesQuery.refetch()}>{t('common.refresh')}</Button>}
    >
      {error ? <ErrorNotice error={error} /> : null}
      {leasesQuery.error ? <ErrorNotice error={leasesQuery.error} /> : null}
      <DataTable<LeaseDTO>
        columns={columns}
        rows={leasesQuery.data?.items ?? []}
        rowKey={(lease) => lease.id}
        loading={leasesQuery.isLoading}
        empty={<EmptyState title={t('lease.empty')} />}
        scroll={{ x: 1000 }}
      />
      <ConfirmDialog
        open={pendingRevoke !== null}
        danger
        title={t('action.revoke')}
        content={t('lease.revoke.confirm')}
        loading={revokeMutation.isPending}
        onConfirm={() => pendingRevoke && revokeMutation.mutate(pendingRevoke.id)}
        onCancel={() => setPendingRevoke(null)}
      />
    </PageShell>
  )
}
