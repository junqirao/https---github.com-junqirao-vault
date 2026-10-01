import { useEffect, useState } from 'react'
import { Alert, Button, Card, Checkbox, Form, Input, Space, Typography } from 'antd'

import { isApiError, translateError } from '../../api/errors'
import { useI18n } from '../../i18n'
import { spacing } from '../../tokens/palette'

export interface LoginFormValues {
  username: string
  password: string
  remember: boolean
}

export interface LoginFormProps {
  serverName?: string
  submitting?: boolean
  onBack?: () => void
  /** 非阻断提示（如自动登录未成功）；不参与错误处理。 */
  notice?: string
  /**
   * 客户端证书免密登录入口：传入即展示次要按钮（"用客户端证书登录"）。
   *
   * 只在调用方确认"本机证书已安装且绑定当前服务端"时传入，避免给出必然失败的按钮。
   * 存在这个入口的原因：自动尝试只跑一次（避免失败后反复打服务端），
   * 服务端重启/证书刚安装完等场景下用户需要能主动再试一次，而不必重开客户端。
   */
  certificateLogin?: { busy?: boolean; onClick: () => void }
  /** 抛出的错误由表单负责本地化展示。 */
  onSubmit: (values: LoginFormValues) => Promise<void>
}

interface ErrorText {
  key: string
  params: Record<string, unknown>
}

/** 登录错误码 → 文案 key 的定向映射（其余走通用映射）。 */
function loginErrorText(error: unknown): ErrorText | null {
  if (!isApiError(error)) return null
  if (error.kind === 'business' && error.code === 'auth.required') return { key: 'login.invalid', params: {} }
  if (error.kind === 'business' && error.code === 'auth.locked') {
    return { key: 'login.locked', params: { seconds: error.args.retry_after_seconds } }
  }
  if (error.kind === 'business' && error.code === 'auth.forbidden' && error.args.reason === 'super_admin_disabled') {
    return { key: 'login.superAdminDisabled', params: {} }
  }
  return null
}

/** 登录页：用户名 + 密码 + 记住我；按错误码给出定向提示。 */
export function LoginForm({
  submitting,
  onBack,
  notice,
  certificateLogin,
  onSubmit
}: LoginFormProps): JSX.Element {
  const { t } = useI18n()
  const [form] = Form.useForm<LoginFormValues>()
  const [errorText, setErrorText] = useState<ErrorText | null>(null)
  const [fallbackError, setFallbackError] = useState<unknown>(null)
  const [lockedSeconds, setLockedSeconds] = useState(0)

  useEffect(() => {
    if (lockedSeconds <= 0) return
    const timer = setTimeout(() => setLockedSeconds((value) => Math.max(0, value - 1)), 1000)
    return () => clearTimeout(timer)
  }, [lockedSeconds])

  const handleFinish = async (values: LoginFormValues): Promise<void> => {
    setErrorText(null)
    setFallbackError(null)
    try {
      await onSubmit(values)
    } catch (error) {
      const mapped = loginErrorText(error)
      if (mapped) {
        setErrorText(mapped)
        if (mapped.key === 'login.locked') {
          const seconds = Number(mapped.params.seconds)
          setLockedSeconds(Number.isFinite(seconds) && seconds > 0 ? Math.ceil(seconds) : 0)
        }
      } else {
        setFallbackError(error)
      }
    }
  }

  const locked = lockedSeconds > 0

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
      <Card style={{ width: 420, maxWidth: '100%' }}>
        <Typography.Title level={4} style={{ marginTop: 0 }}>
          {t('login.title')}
        </Typography.Title>

        {notice ? (
          <Alert type="info" showIcon style={{ marginBottom: spacing.md }} message={notice} />
        ) : null}
        {errorText ? (
          <Alert
            type="error"
            showIcon
            style={{ marginBottom: spacing.md }}
            message={t(errorText.key, errorText.params)}
            description={errorText.key === 'login.locked' ? t(errorText.key, { seconds: lockedSeconds }) : undefined}
          />
        ) : null}
        {fallbackError ? (
          <Alert type="error" showIcon style={{ marginBottom: spacing.md }} message={translateError(fallbackError)} />
        ) : null}

        <Form
          form={form}
          layout="vertical"
          initialValues={{ remember: true }}
          onFinish={(values) => void handleFinish(values)}
        >
          <Form.Item name="username" label={t('field.username')} rules={[{ required: true }]}>
            <Input autoComplete="username" spellCheck={false} />
          </Form.Item>
          <Form.Item name="password" label={t('field.password')} rules={[{ required: true }]}>
            <Input.Password autoComplete="current-password" />
          </Form.Item>
          <Form.Item name="remember" valuePropName="checked">
            <Checkbox>{t('login.remember')}</Checkbox>
          </Form.Item>
          <Space direction="vertical" style={{ width: '100%' }}>
            <Button type="primary" htmlType="submit" block loading={submitting} disabled={locked}>
              {submitting ? t('login.loggingIn') : t('login.submit')}
            </Button>
            {certificateLogin ? (
              <Button
                block
                loading={certificateLogin.busy}
                disabled={locked || submitting}
                onClick={certificateLogin.onClick}
              >
                {t('auth.certLogin.button')}
              </Button>
            ) : null}
            {onBack ? (
              <Button type="text" block onClick={onBack}>
                {t('boot.changeAddress')}
              </Button>
            ) : null}
          </Space>
        </Form>
      </Card>
    </div>
  )
}
