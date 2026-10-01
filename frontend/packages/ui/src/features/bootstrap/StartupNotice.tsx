import type { ReactNode } from 'react'
import { Alert, Button, Card, Space, Typography } from 'antd'

import type { ClientCheckResponse } from '../../api/types'
import { hasKey, useI18n } from '../../i18n'
import { spacing } from '../../tokens/palette'
import { CLIENT_API_VERSION } from '../../utils/validation'

function CenteredCard({ children }: { children: ReactNode }): JSX.Element {
  return (
    <div
      style={{
        height: '100%',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        padding: spacing.lg
      }}
    >
      <Card style={{ width: 560, maxWidth: '100%' }}>{children}</Card>
    </div>
  )
}

export interface OfflineNoticeProps {
  detail?: string
  loading?: boolean
  onRetry: () => void
  onChangeAddress: () => void
}

/** 网络失败：提供重试与"修改地址"。 */
export function OfflineNotice({ detail, loading, onRetry, onChangeAddress }: OfflineNoticeProps): JSX.Element {
  const { t } = useI18n()
  return (
    <CenteredCard>
      <Alert
        type="error"
        showIcon
        message={t('boot.offline.title')}
        description={
          <Space direction="vertical" size={2}>
            <Typography.Text type="secondary">{t('boot.offline.desc')}</Typography.Text>
            {detail ? (
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                {detail}
              </Typography.Text>
            ) : null}
          </Space>
        }
      />
      <Space style={{ marginTop: spacing.lg }}>
        <Button type="primary" loading={loading} onClick={onRetry}>
          {t('common.retry')}
        </Button>
        <Button onClick={onChangeAddress}>{t('boot.changeAddress')}</Button>
      </Space>
    </CenteredCard>
  )
}

export interface BlockedNoticeProps {
  /** 服务端返回的 blocked_reason。 */
  reason?: string
  onGoLogin: () => void
  onChangeAddress: () => void
}

/** 无法初始化（关闭自助初始化 / 窗口过期 / 已初始化）。 */
export function BlockedNotice({ reason, onGoLogin, onChangeAddress }: BlockedNoticeProps): JSX.Element {
  const { t } = useI18n()
  const reasonKey = reason ? `boot.blocked.${reason}` : ''
  const message = reasonKey && hasKey(reasonKey) ? t(reasonKey) : t('boot.blocked.title')
  return (
    <CenteredCard>
      <Typography.Title level={4} style={{ marginTop: 0 }}>
        {t('boot.blocked.title')}
      </Typography.Title>
      <Alert type="warning" showIcon message={message} />
      <Space style={{ marginTop: spacing.lg }}>
        <Button type="primary" onClick={onGoLogin}>
          {t('login.title')}
        </Button>
        <Button onClick={onChangeAddress}>{t('boot.changeAddress')}</Button>
      </Space>
    </CenteredCard>
  )
}

export interface CompatNoticeProps {
  check: ClientCheckResponse
  onContinue: () => void
  onChangeAddress: () => void
}

/** 版本不兼容提示页；协议不匹配时不允许继续，区间不匹配允许"只读查看"。 */
export function CompatNotice({ check, onContinue, onChangeAddress }: CompatNoticeProps): JSX.Element {
  const { t } = useI18n()
  const stateKey = check.state === 'compatible' ? 'incompatible_protocol' : check.state
  const title = t(`boot.compat.${stateKey}.title`)
  const params: Record<string, unknown> = {
    serverApiVersion: check.api_version,
    clientMaxApiVersion: CLIENT_API_VERSION,
    min: check.client_compat.min ?? '',
    max: check.client_compat.max ?? ''
  }
  const description = t(`boot.compat.${stateKey}.desc`, params)
  const readOnlyAllowed = check.state !== 'incompatible_protocol'
  return (
    <CenteredCard>
      <Typography.Title level={4} style={{ marginTop: 0 }}>
        {t('boot.compat.title')}
      </Typography.Title>
      <Alert type="warning" showIcon message={title} description={description} />
      <Space style={{ marginTop: spacing.lg }}>
        {readOnlyAllowed ? <Button type="primary" onClick={onContinue}>{t('boot.compat.continueReadonly')}</Button> : null}
        <Button onClick={onChangeAddress}>{t('boot.changeAddress')}</Button>
      </Space>
    </CenteredCard>
  )
}
