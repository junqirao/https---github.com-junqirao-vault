import { useEffect, useState } from 'react'
import { Alert, Button, Descriptions, Modal, Space, Typography } from 'antd'

import { CopyableText } from '../../components/CopyableText'
import { ErrorNotice } from '../../components/ErrorNotice'
import { LoadingState } from '../../components/LoadingState'
import { useApi } from '../../api/provider'
import type { EnrollmentTokenResponse } from '../../api/types'
import { useI18n } from '../../i18n'
import { formatTime, shortId } from '../../utils/format'

export interface UserEnrollmentModalProps {
  open: boolean
  userId?: string
  onClose: () => void
}

/** 注册令牌弹窗：打开即签发一次性令牌，默认脱敏展示。 */
export function UserEnrollmentModal({ open, userId, onClose }: UserEnrollmentModalProps): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const [token, setToken] = useState<EnrollmentTokenResponse | null>(null)
  const [error, setError] = useState<unknown>(null)
  const [loading, setLoading] = useState(false)
  const [revealed, setRevealed] = useState(false)

  useEffect(() => {
    if (!open || !userId) return
    let cancelled = false
    setToken(null)
    setError(null)
    setRevealed(false)
    setLoading(true)
    api
      .issueEnrollmentToken(userId)
      .then((result) => {
        if (!cancelled) setToken(result)
      })
      .catch((err: unknown) => {
        if (!cancelled) setError(err)
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [api, open, userId])

  return (
    <Modal
      open={open}
      title={t('user.enrollment.title')}
      width={560}
      onCancel={onClose}
      footer={<Button onClick={onClose}>{t('common.close')}</Button>}
      destroyOnClose
      maskClosable={false}
    >
      {error ? <ErrorNotice error={error} /> : null}
      {loading ? <LoadingState text={t('common.loading')} /> : null}
      {token ? (
        <Space direction="vertical" size={8} style={{ width: '100%' }}>
          <Alert type="warning" showIcon message={t('user.enrollment.desc')} description={t('user.enrollment.warning')} />
          <Descriptions size="small" column={1} bordered>
            <Descriptions.Item label={t('user.enrollment.serverId')}>{shortId(token.server_instance_id, 8)}</Descriptions.Item>
            <Descriptions.Item label={t('user.enrollment.expiresAt')}>{formatTime(token.expires_at)}</Descriptions.Item>
          </Descriptions>
          <Space direction="vertical" size={4} style={{ width: '100%' }}>
            <CopyableText value={token.token} masked={!revealed} monospace />
            <Space>
              <Button size="small" onClick={() => setRevealed((value) => !value)}>
                {revealed ? t('common.hide') : t('common.reveal')}
              </Button>
            </Space>
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              {t('user.enrollment.maskedHint')}
            </Typography.Text>
          </Space>
        </Space>
      ) : null}
    </Modal>
  )
}
