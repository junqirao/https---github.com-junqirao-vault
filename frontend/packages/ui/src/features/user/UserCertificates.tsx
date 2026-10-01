import { useState } from 'react'
import { Button, Drawer, Space } from 'antd'
import type { TableColumnsType } from 'antd'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { ConfirmDialog } from '../../components/ConfirmDialog'
import { DataTable } from '../../components/DataTable'
import { EmptyState } from '../../components/EmptyState'
import { ErrorNotice } from '../../components/ErrorNotice'
import { StatusTag } from '../../components/StatusTag'
import { useApi } from '../../api/provider'
import type { CertificateDTO, IssuedCertificateDTO } from '../../api/types'
import { useI18n } from '../../i18n'
import { spacing } from '../../tokens/palette'
import { formatTime } from '../../utils/format'
import { IssuedCertificateModal } from './IssuedCertificateModal'

export interface UserCertificatesProps {
  open: boolean
  userId?: string
  onClose: () => void
}

/** 用户证书抽屉：查看证书、签发新证书并支持吊销。 */
export function UserCertificates({ open, userId, onClose }: UserCertificatesProps): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const [error, setError] = useState<unknown>(null)
  const [pendingRevoke, setPendingRevoke] = useState<CertificateDTO | null>(null)
  const [issuedCert, setIssuedCert] = useState<IssuedCertificateDTO | null>(null)

  const certificatesQuery = useQuery({
    queryKey: ['user-certificates', userId],
    queryFn: () => api.listCertificates(userId ?? ''),
    enabled: open && Boolean(userId)
  })

  const revokeMutation = useMutation({
    mutationFn: (certificate: CertificateDTO) => api.revokeCertificate(certificate.user_id, certificate.id),
    onSuccess: () => {
      setPendingRevoke(null)
      void queryClient.invalidateQueries({ queryKey: ['user-certificates', userId] })
    },
    onError: (err) => setError(err)
  })

  const generateMutation = useMutation({
    mutationFn: () => api.createCertificate(userId ?? ''),
    onSuccess: (result) => {
      setIssuedCert(result)
      void queryClient.invalidateQueries({ queryKey: ['user-certificates', userId] })
    },
    onError: (err) => setError(err)
  })

  const columns: TableColumnsType<CertificateDTO> = [
    { title: t('user.cert.serial'), dataIndex: 'serial', key: 'serial', width: 160 },
    {
      title: t('user.cert.fingerprint'),
      dataIndex: 'fingerprint',
      key: 'fingerprint',
      render: (value: string) => <span style={{ fontSize: 12 }}>{value}</span>
    },
    {
      title: t('common.status'),
      dataIndex: 'status',
      key: 'status',
      width: 110,
      render: (value: string) => <StatusTag group="certificate" value={value} />
    },
    { title: t('common.expiresAt'), dataIndex: 'not_after', key: 'not_after', width: 170, render: (value: number) => formatTime(value) },
    {
      title: t('common.actions'),
      key: 'actions',
      width: 100,
      render: (_value, certificate) => (
        <Button
          type="link"
          size="small"
          danger
          disabled={certificate.status === 'revoked'}
          onClick={() => setPendingRevoke(certificate)}
        >
          {t('action.revoke')}
        </Button>
      )
    }
  ]

  return (
    <Drawer open={open} title={t('user.certificates.title')} width={720} onClose={onClose} destroyOnClose>
      {error ? <ErrorNotice error={error} /> : null}
      {certificatesQuery.error ? <ErrorNotice error={certificatesQuery.error} /> : null}
      <Space style={{ marginBottom: spacing.md }}>
        <Button type="primary" loading={generateMutation.isPending} onClick={() => generateMutation.mutate()}>
          {t('user.cert.generate')}
        </Button>
      </Space>
      <DataTable<CertificateDTO>
        columns={columns}
        rows={certificatesQuery.data?.items ?? []}
        rowKey={(certificate) => certificate.id}
        loading={certificatesQuery.isLoading}
        empty={<EmptyState title={t('user.cert.empty')} />}
        pagination={false}
        scroll={{ x: 700 }}
      />
      <ConfirmDialog
        open={pendingRevoke !== null}
        danger
        title={t('action.revoke')}
        content={t('user.cert.revoke.confirm')}
        loading={revokeMutation.isPending}
        onConfirm={() => pendingRevoke && revokeMutation.mutate(pendingRevoke)}
        onCancel={() => setPendingRevoke(null)}
      />
      <IssuedCertificateModal
        open={issuedCert !== null}
        certificate={issuedCert}
        onClose={() => setIssuedCert(null)}
      />
    </Drawer>
  )
}
