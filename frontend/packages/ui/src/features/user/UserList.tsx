import { useMemo, useState } from 'react'
import { App, Button, Input, Space } from 'antd'
import type { TableColumnsType } from 'antd'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { ConfirmDialog } from '../../components/ConfirmDialog'
import { DataTable } from '../../components/DataTable'
import { EmptyState } from '../../components/EmptyState'
import { ErrorNotice } from '../../components/ErrorNotice'
import { PageShell } from '../../components/PageShell'
import { useApi } from '../../api/provider'
import type { IssuedCertificateDTO, UserDTO } from '../../api/types'
import { useI18n } from '../../i18n'
import { formatBytes, formatTime } from '../../utils/format'
import { roleLabel } from '../../utils/labels'
import { IssuedCertificateModal } from './IssuedCertificateModal'
import { UserEnrollmentModal } from './UserEnrollmentModal'
import { UserCertificates } from './UserCertificates'
import { UserForm } from './UserForm'

/** 用户管理（管理员）：列表 / 新建 / 编辑 / 删除 / 签发注册令牌 / 证书。 */
export function UserList(): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const { message } = App.useApp()
  const queryClient = useQueryClient()
  const [keyword, setKeyword] = useState('')
  const [error, setError] = useState<unknown>(null)
  const [formOpen, setFormOpen] = useState(false)
  const [editing, setEditing] = useState<UserDTO | null>(null)
  const [pendingDelete, setPendingDelete] = useState<UserDTO | null>(null)
  const [enrollUser, setEnrollUser] = useState<UserDTO | null>(null)
  const [certUser, setCertUser] = useState<UserDTO | null>(null)
  const [issuedCert, setIssuedCert] = useState<IssuedCertificateDTO | null>(null)

  const usersQuery = useQuery({
    queryKey: ['users', 'list', keyword],
    queryFn: () => api.listUsers({ keyword: keyword || undefined, limit: 200 })
  })

  const invalidate = (): void => {
    void queryClient.invalidateQueries({ queryKey: ['users'] })
  }

  const deleteMutation = useMutation({
    mutationFn: (id: string) => api.deleteUser(id),
    onSuccess: () => {
      setPendingDelete(null)
      invalidate()
    },
    onError: (err) => setError(err)
  })

  // 证书的签发入口只在"用户证书"抽屉里（避免同一功能在列表与抽屉重复出现）。
  const columns = useMemo<TableColumnsType<UserDTO>>(
    () => [
      { title: t('field.username'), dataIndex: 'username', key: 'username' },
      { title: t('field.role'), dataIndex: 'role', key: 'role', width: 160, render: (value: string) => roleLabel(value) },
      {
        title: t('common.status'),
        dataIndex: 'enabled',
        key: 'enabled',
        width: 110,
        render: (value: boolean) => (value ? t('common.enabled') : t('common.disabled'))
      },
      {
        title: t('user.quota.used'),
        key: 'quota',
        width: 200,
        // quota_bytes = 0 表示"不限"（只受存储池容量约束），不能显示成 "0 B" —— 会让人
        // 以为该用户一点配额都没有（真实反馈）。
        render: (_value, user) =>
          user.quota_bytes > 0
            ? `${formatBytes(user.used_bytes)} / ${formatBytes(user.quota_bytes)}`
            : `${formatBytes(user.used_bytes)} / ${t('common.unlimited')}`
      },
      { title: t('field.remark'), dataIndex: 'remark', key: 'remark', render: (value?: string) => value || '-' },
      { title: t('common.createdAt'), dataIndex: 'created_at', key: 'created_at', width: 170, render: (value: number) => formatTime(value) },
      {
        title: t('common.actions'),
        key: 'actions',
        width: 400,
        render: (_value, user) => (
          <Space size={0} wrap>
            <Button
              type="link"
              size="small"
              onClick={() => {
                setEditing(user)
                setFormOpen(true)
              }}
            >
              {t('common.edit')}
            </Button>
            <Button type="link" size="small" onClick={() => setEnrollUser(user)}>
              {t('action.enrollment')}
            </Button>
            <Button type="link" size="small" onClick={() => setCertUser(user)}>
              {t('user.certificates.title')}
            </Button>
            <Button type="link" size="small" danger onClick={() => setPendingDelete(user)}>
              {t('common.delete')}
            </Button>
          </Space>
        )
      }
    ],
    [t]
  )

  return (
    <PageShell
      title={t('page.users.title')}
      extra={
        <>
          <Input.Search
            allowClear
            placeholder={t('user.filter.keyword')}
            style={{ width: 220 }}
            onChange={(event) => setKeyword(event.target.value)}
          />
          <Button onClick={() => void usersQuery.refetch()}>{t('common.refresh')}</Button>
          <Button
            type="primary"
            onClick={() => {
              setEditing(null)
              setFormOpen(true)
            }}
          >
            {t('common.create')}
          </Button>
        </>
      }
    >
      {error ? <ErrorNotice error={error} /> : null}
      {usersQuery.error ? <ErrorNotice error={usersQuery.error} /> : null}
      <DataTable<UserDTO>
        columns={columns}
        rows={usersQuery.data?.items ?? []}
        rowKey={(user) => user.id}
        loading={usersQuery.isLoading}
        empty={<EmptyState title={t('user.empty')} />}
        scroll={{ x: 1100 }}
      />

      <UserForm
        open={formOpen}
        mode={editing ? 'edit' : 'create'}
        user={editing}
        onCancel={() => setFormOpen(false)}
        onSubmitted={(result) => {
          setFormOpen(false)
          invalidate()
          if (result.certificate) {
            setIssuedCert(result.certificate)
          } else {
            message.warning(t('user.cert.issueFailed'))
          }
        }}
      />

      <UserEnrollmentModal open={enrollUser !== null} userId={enrollUser?.id} onClose={() => setEnrollUser(null)} />
      <UserCertificates open={certUser !== null} userId={certUser?.id} onClose={() => setCertUser(null)} />
      <IssuedCertificateModal
        open={issuedCert !== null}
        certificate={issuedCert}
        onClose={() => setIssuedCert(null)}
      />

      <ConfirmDialog
        open={pendingDelete !== null}
        danger
        title={t('common.delete')}
        content={t('user.delete.confirm', { username: pendingDelete?.username ?? '' })}
        loading={deleteMutation.isPending}
        onConfirm={() => pendingDelete && deleteMutation.mutate(pendingDelete.id)}
        onCancel={() => setPendingDelete(null)}
      />
    </PageShell>
  )
}
