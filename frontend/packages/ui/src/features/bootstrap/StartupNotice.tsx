import type { ReactNode } from 'react'
import { DownOutlined } from '@ant-design/icons'
import { Alert, Button, Card, Dropdown, Space, Typography } from 'antd'

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

/** 「选择服务器」下拉里的一个备选服务端（只带展示需要的字段）。 */
export interface ServerChoice {
  key: string
  /** 展示名：服务端名称，缺省时用地址。 */
  label: string
  baseUrl: string
}

export interface OfflineNoticeProps {
  detail?: string
  loading?: boolean
  onRetry: () => void
  onChangeAddress: () => void
  /**
   * **其他**已保存的服务端（不含当前这个，也不含与当前指向同一台服务端的条目）。
   *
   * 为空则不出现"选择服务器"按钮：只有一个服务端时能做的只有重试或改地址，
   * 摆一个必然没内容的下拉只会让人多点一次。
   */
  alternatives?: ServerChoice[]
  /** 选中某个备选服务端（切换活动服务端；启动流程会自动对新服务端重新检查）。 */
  onSelectServer?: (key: string) => void
}

/**
 * 网络失败：提供重试、"修改地址"与"选择服务器"。
 *
 * 为什么这个页面必须能换服务端（真实诉求："在选择其他服务器时，如果无法连接，
 * 在无法连接服务端报错页面时如果有其他服务端备选项的话，在修改地址按钮后面加一个
 * 选择服务器按钮，可以选别的服务器"）：
 * 客户端支持保存多台服务端，而"连不上"十有八九是**当前这台**的问题（换过地址/机器关机/
 * 不在同一网段）。此时最省事的出路是切到另一台**已经配好、证书指纹与会话都在**的服务端，
 * 而不是重新敲一遍地址（重敲会走到 /agent/test-server，连不上时还得先排查网络）。
 * 下拉里**不放当前服务端**：它刚刚失败，想再试一次点"重试"就够了。
 */
export function OfflineNotice({
  detail,
  loading,
  onRetry,
  onChangeAddress,
  alternatives = [],
  onSelectServer
}: OfflineNoticeProps): JSX.Element {
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
        {alternatives.length > 0 && onSelectServer ? (
          <Dropdown
            trigger={['click']}
            placement="topLeft"
            menu={{
              items: alternatives.map((server) => ({
                key: server.key,
                // 名称与地址分两行：多服务端下只给地址不好认（地址一样只差端口），
                // 只给名称又不好确认是哪台机器。名称与地址相同时只留一行。
                label: (
                  <Space direction="vertical" size={0}>
                    <Typography.Text>{server.label}</Typography.Text>
                    {server.label === server.baseUrl ? null : (
                      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                        {server.baseUrl}
                      </Typography.Text>
                    )}
                  </Space>
                )
              })),
              onClick: ({ key }) => onSelectServer(key)
            }}
          >
            <Button>
              <Space size={6}>
                {t('boot.chooseServer')}
                <DownOutlined style={{ fontSize: 10 }} />
              </Space>
            </Button>
          </Dropdown>
        ) : null}
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
