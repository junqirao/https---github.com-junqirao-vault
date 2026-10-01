import type { ReactNode } from 'react'
import { useState } from 'react'
import { CheckCircleFilled, CloseCircleFilled, ExclamationCircleFilled } from '@ant-design/icons'
import { Button, Card, Descriptions, Form, Input, Space, Typography } from 'antd'

import type { ServerProbe } from '../../api/agentTypes'
import { translateError } from '../../api/errors'
import { CopyableText } from '../../components/CopyableText'
import { useI18n } from '../../i18n'
import { palette, radius, spacing } from '../../tokens/palette'
import { DEFAULT_SERVER_URL, isValidHttpUrl, normalizeBaseUrl } from '../../utils/validation'

export interface ServerEndpointFormProps {
  initialBaseUrl?: string
  /**
   * 用给定地址执行一次连通性探测（经本地代理，自动处理 http/https 与自签证书）。
   * 返回值中的 `server_url` 是**实际生效**的地址，可能已从 http 自动升级为 https。
   */
  onTest: (baseUrl: string) => Promise<ServerProbe>
  onSubmit: (baseUrl: string) => void | Promise<void>
  submitting?: boolean
  /** 已有本地配置时允许取消（回到主界面/登录）。 */
  onCancel?: () => void
}

interface FormValues {
  baseUrl: string
}

type PanelTone = 'success' | 'warning' | 'error'

/** 状态色只取自统一色板（见 tokens/palette.ts），不在此另写十六进制。 */
const PANEL_COLOR: Record<PanelTone, string> = {
  success: palette.success,
  warning: palette.warning,
  error: palette.danger
}

/**
 * 结果面板：白底卡片 + 左侧 3px 细色带承载语义，颜色只用于图标与标题，不做整块铺色。
 * 延续"简洁大气、不做动效与透明效果"的风格。
 */
function ResultPanel({
  tone,
  icon,
  title,
  children
}: {
  tone: PanelTone
  icon: ReactNode
  title: string
  children?: ReactNode
}): JSX.Element {
  const accent = PANEL_COLOR[tone]
  return (
    <div
      style={{
        background: palette.bgContainer,
        border: `1px solid ${palette.border}`,
        borderLeft: `3px solid ${accent}`,
        borderRadius: radius.sm,
        padding: `${spacing.sm}px ${spacing.md}px`,
        marginBottom: spacing.sm
      }}
    >
      <Space align="center" size={spacing.xs} style={{ marginBottom: children ? spacing.xs : 0 }}>
        {icon}
        <Typography.Text strong style={{ color: accent }}>
          {title}
        </Typography.Text>
      </Space>
      {children}
    </div>
  )
}

/** 服务端地址配置页：填写地址 → 测试连接（展示服务端信息）→ 保存。 */
export function ServerEndpointForm({
  initialBaseUrl,
  onTest,
  onSubmit,
  submitting,
  onCancel
}: ServerEndpointFormProps): JSX.Element {
  const { t } = useI18n()
  const [form] = Form.useForm<FormValues>()
  const [testing, setTesting] = useState(false)
  const [tested, setTested] = useState<ServerProbe | null>(null)
  const [error, setError] = useState<unknown>(null)

  const handleTest = async (): Promise<void> => {
    const values = await form.validateFields()
    setError(null)
    setTested(null)
    setTesting(true)
    try {
      const info = await onTest(normalizeBaseUrl(values.baseUrl))
      setTested(info)
      // 探测返回的才是实际生效地址（可能已自动升级为 https），回填输入框。
      if (info.server_url && info.server_url !== values.baseUrl) {
        form.setFieldValue('baseUrl', info.server_url)
      }
    } catch (err) {
      setError(err)
    } finally {
      setTesting(false)
    }
  }

  const handleSubmit = async (): Promise<void> => {
    const values = await form.validateFields()
    const input = normalizeBaseUrl(values.baseUrl)
    // 提交必须使用探测返回的实际生效地址，否则可能把会 400 的 http 地址存下来。
    const effective = tested && tested.server_url === input ? tested.server_url : input
    setError(null)
    try {
      await onSubmit(effective)
    } catch (err) {
      // 保存失败必须给出可见反馈：否则表现为"点了保存没反应"，无从排查。
      setError(err)
    }
  }

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
      <Card style={{ width: 560, maxWidth: '100%' }}>
        <Typography.Title level={4} style={{ marginTop: 0 }}>
          {t('server.title')}
        </Typography.Title>

        <Form form={form} layout="vertical" initialValues={{ baseUrl: initialBaseUrl || DEFAULT_SERVER_URL }}>
          <Form.Item
            name="baseUrl"
            label={t('field.baseUrl')}
            extra={t('server.tlsHint')}
            rules={[
              { required: true, message: t('server.required') },
              {
                validator: (_rule, value: string) =>
                  !value || isValidHttpUrl(value)
                    ? Promise.resolve()
                    : Promise.reject(new Error(t('server.invalidUrl')))
              }
            ]}
          >
            <Input placeholder={t('server.baseUrlPlaceholder')} autoComplete="off" spellCheck={false} />
          </Form.Item>

          <Space style={{ marginBottom: spacing.md }}>
            <Button onClick={() => void handleTest()} loading={testing}>
              {testing ? t('server.testing') : t('server.test')}
            </Button>
            <Button type="primary" onClick={() => void handleSubmit()} loading={submitting}>
              {t('server.saveAndContinue')}
            </Button>
            {onCancel ? <Button type="text" onClick={onCancel}>{t('common.cancel')}</Button> : null}
          </Space>
        </Form>

        {tested ? (
          <>
            {tested.upgraded_to_tls ? (
              <ResultPanel
                tone="warning"
                icon={<ExclamationCircleFilled style={{ color: palette.warning }} />}
                title={t('server.tlsUpgraded')}
              />
            ) : null}
            <ResultPanel
              tone="success"
              icon={<CheckCircleFilled style={{ color: palette.success }} />}
              title={t('server.testSuccess')}
            >
              <Descriptions size="small" column={1} style={{ marginTop: spacing.xxs }}>
                <Descriptions.Item label={t('system.serverName')}>{tested.server_name || '-'}</Descriptions.Item>
                <Descriptions.Item label={t('server.apiVersion')}>{tested.api_version}</Descriptions.Item>
                <Descriptions.Item label={t('server.serverVersion')}>{tested.server_version}</Descriptions.Item>
                <Descriptions.Item label={t('field.baseUrl')}>{tested.server_url}</Descriptions.Item>
                <Descriptions.Item label={t('server.certFingerprint')}>
                  <CopyableText value={tested.cert?.sha256} monospace />
                </Descriptions.Item>
              </Descriptions>
            </ResultPanel>
          </>
        ) : null}

        {error ? (
          <ResultPanel
            tone="error"
            icon={<CloseCircleFilled style={{ color: palette.danger }} />}
            title={t('server.testFailed')}
          >
            <Typography.Text type="secondary">{translateError(error)}</Typography.Text>
          </ResultPanel>
        ) : null}
      </Card>
    </div>
  )
}
