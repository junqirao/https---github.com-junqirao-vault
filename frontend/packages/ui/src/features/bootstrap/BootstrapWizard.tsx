import { useMemo, useState } from 'react'
import { Alert, Button, Checkbox, Descriptions, Form, Input, Space, Steps, Typography } from 'antd'

import { ErrorNotice } from '../../components/ErrorNotice'
import { PasswordStrength } from '../../components/PasswordStrength'
import { isApiError } from '../../api/errors'
import type { BootstrapResponse } from '../../api/types'
import { useI18n } from '../../i18n'
import { spacing } from '../../tokens/palette'
import { shortId } from '../../utils/format'
import { isValidPassword, isValidUsername, USERNAME_MAX, USERNAME_MIN } from '../../utils/validation'

export interface BootstrapInputValues {
  username: string
  password: string
  serverName?: string
}

export interface BootstrapWizardProps {
  serverName: string
  serverInstanceId: string
  apiVersion: number
  submitting?: boolean
  onSubmit: (input: BootstrapInputValues) => Promise<BootstrapResponse>
  onEnterConsole: () => void
  onGoLogin: () => void
}

interface FormValues {
  username: string
  password: string
  confirmPassword: string
  serverName?: string
}

const STEP_KEYS = ['step1', 'step2', 'step3', 'step4', 'step5'] as const

/**
 * 初始化向导（像路由器初始化，5 步）。
 *
 * 步骤：欢迎 → 账号 → 服务端名称 → 确认 → 完成。
 * 表单校验与服务端 bootstrap 规则一致；提交失败按错误码分支处理。
 */
export function BootstrapWizard(props: BootstrapWizardProps): JSX.Element {
  const { t } = useI18n()
  const [form] = Form.useForm<FormValues>()
  const [step, setStep] = useState(0)
  const [ack, setAck] = useState(false)
  const [localError, setLocalError] = useState<string | null>(null)
  const [apiError, setApiError] = useState<unknown>(null)
  const [alreadyInitialized, setAlreadyInitialized] = useState(false)
  const [result, setResult] = useState<BootstrapResponse | null>(null)

  const serverInstanceShort = useMemo(() => shortId(props.serverInstanceId, 8), [props.serverInstanceId])

  const gotoNext = async (): Promise<void> => {
    setLocalError(null)
    setApiError(null)
    if (step === 1) {
      try {
        await form.validateFields(['username', 'password', 'confirmPassword'])
      } catch {
        return
      }
    }
    setStep((value) => Math.min(4, value + 1))
  }

  const gotoPrevious = (): void => setStep((value) => Math.max(0, value - 1))

  const submit = async (): Promise<void> => {
    setLocalError(null)
    setApiError(null)
    if (!ack) {
      setLocalError(t('bootstrap.ack.required'))
      return
    }
    const values = form.getFieldsValue()
    try {
      const response = await props.onSubmit({
        username: values.username.trim(),
        password: values.password,
        serverName: values.serverName?.trim() || undefined
      })
      setResult(response)
      setStep(4)
    } catch (err) {
      if (isApiError(err) && err.kind === 'business') {
        if (err.code === 'system.already_initialized') {
          setAlreadyInitialized(true)
          return
        }
        if (err.code === 'system.bootstrap_disabled' || err.code === 'system.bootstrap_blocked') {
          setLocalError(t('bootstrap.error.disabled'))
          return
        }
      }
      setApiError(err)
    }
  }

  const renderStepBody = (): JSX.Element => {
    if (step === 0) {
      return (
        <Space direction="vertical" size={spacing.sm} style={{ width: '100%' }}>
          <Typography.Paragraph style={{ marginBottom: 0 }}>{t('bootstrap.step1.body')}</Typography.Paragraph>
          <Typography.Text type="secondary">{t('bootstrap.step1.check')}</Typography.Text>
          <Descriptions size="small" column={1} bordered>
            <Descriptions.Item label={t('system.serverName')}>{props.serverName || '-'}</Descriptions.Item>
            <Descriptions.Item label={t('system.instanceId')}>{serverInstanceShort}</Descriptions.Item>
            <Descriptions.Item label={t('system.apiVersion')}>{props.apiVersion}</Descriptions.Item>
          </Descriptions>
        </Space>
      )
    }
    if (step === 3) {
      const values = form.getFieldsValue()
      return (
        <Space direction="vertical" size={spacing.sm} style={{ width: '100%' }}>
          <Typography.Paragraph style={{ marginBottom: 0 }}>{t('bootstrap.step4.body')}</Typography.Paragraph>
          <Descriptions size="small" column={1} bordered>
            <Descriptions.Item label={t('field.username')}>{values.username || '-'}</Descriptions.Item>
            <Descriptions.Item label={t('field.password')}>
              {'•'.repeat(Math.min(12, values.password?.length ?? 0)) || '-'}
            </Descriptions.Item>
            <Descriptions.Item label={t('field.serverName')}>{values.serverName || '-'}</Descriptions.Item>
          </Descriptions>
          <Checkbox checked={ack} onChange={(event) => setAck(event.target.checked)}>
            {t('bootstrap.step4.ack')}
          </Checkbox>
        </Space>
      )
    }
    if (step === 4) {
      return (
        <Space direction="vertical" size={spacing.sm} style={{ width: '100%' }}>
          <Alert type="success" showIcon message={t('bootstrap.step5.success')} />
          <Descriptions size="small" column={1} bordered>
            <Descriptions.Item label={t('bootstrap.result.username')}>{result?.user.username || '-'}</Descriptions.Item>
            <Descriptions.Item label={t('bootstrap.result.instanceId')}>{serverInstanceShort}</Descriptions.Item>
          </Descriptions>
        </Space>
      )
    }
    return <div />
  }

  const isFormStep = step === 1 || step === 2

  return (
    <div style={{ height: '100%', padding: spacing.xl, background: 'var(--vault-bg-page, #F5F5F7)' }}>
      <div style={{ maxWidth: 760, margin: '0 auto' }}>
        <Typography.Title level={4} style={{ marginTop: 0 }}>
          {t('bootstrap.title')}
        </Typography.Title>

        <Steps
          current={step}
          size="small"
          style={{ margin: `${spacing.lg}px 0` }}
          items={STEP_KEYS.map((key) => ({ title: t(`bootstrap.${key}.title`) }))}
        />

        {alreadyInitialized ? (
          <Alert
            type="info"
            showIcon
            message={t('bootstrap.error.alreadyInitialized')}
            action={<Button size="small" onClick={props.onGoLogin}>{t('bootstrap.goLogin')}</Button>}
          />
        ) : (
          <>
            {localError ? (
              <Alert type="error" showIcon message={localError} style={{ marginBottom: spacing.md }} />
            ) : null}
            {apiError ? <ErrorNotice error={apiError} /> : null}

            {renderStepBody()}

            <Form form={form} layout="vertical" initialValues={{ username: 'admin' }} style={{ marginTop: spacing.md, display: isFormStep ? 'block' : 'none' }}>
              <div style={{ display: step === 1 ? 'block' : 'none' }}>
                <Form.Item
                  name="username"
                  label={t('field.username')}
                  extra={t('bootstrap.username.hint')}
                  rules={[
                    { required: true },
                    {
                      validator: (_rule, value: string) =>
                        isValidUsername(value ?? '')
                          ? Promise.resolve()
                          : Promise.reject(new Error(t('err.param.username.length', { min_length: USERNAME_MIN, max_length: USERNAME_MAX })))
                    }
                  ]}
                >
                  <Input autoComplete="off" spellCheck={false} />
                </Form.Item>
                <Form.Item
                  name="password"
                  label={t('field.password')}
                  extra={t('bootstrap.password.hint')}
                  rules={[
                    { required: true },
                    {
                      validator: (_rule, value: string) =>
                        isValidPassword(value ?? '')
                          ? Promise.resolve()
                          : Promise.reject(new Error(t('err.param.password.need_letter_and_digit')))
                    }
                  ]}
                >
                  <Input.Password autoComplete="new-password" />
                </Form.Item>
                <Form.Item noStyle shouldUpdate={(prev, next) => prev.password !== next.password}>
                  {() => <PasswordStrength value={form.getFieldValue('password') ?? ''} />}
                </Form.Item>
                <Form.Item
                  name="confirmPassword"
                  label={t('field.confirmPassword')}
                  dependencies={['password']}
                  rules={[
                    { required: true },
                    ({ getFieldValue }) => ({
                      validator: (_rule, value: string) =>
                        value === getFieldValue('password')
                          ? Promise.resolve()
                          : Promise.reject(new Error(t('bootstrap.confirmPassword.mismatch')))
                    })
                  ]}
                >
                  <Input.Password autoComplete="new-password" />
                </Form.Item>
              </div>

              <div style={{ display: step === 2 ? 'block' : 'none' }}>
                <Form.Item name="serverName" label={t('field.serverName')} extra={t('bootstrap.step3.hint')}>
                  <Input placeholder={props.serverName} maxLength={64} />
                </Form.Item>
                <Typography.Text type="secondary">{t('bootstrap.step3.body')}</Typography.Text>
              </div>
            </Form>
          </>
        )}

        <Space style={{ marginTop: spacing.lg }}>
          {step > 0 && step < 4 && !alreadyInitialized ? (
            <Button onClick={gotoPrevious}>{t('common.previous')}</Button>
          ) : null}
          {step < 3 && !alreadyInitialized ? <Button type="primary" onClick={() => void gotoNext()}>{t('common.next')}</Button> : null}
          {step === 3 && !alreadyInitialized ? (
            <Button type="primary" loading={props.submitting} onClick={() => void submit()}>
              {props.submitting ? t('bootstrap.submitting') : t('bootstrap.submit')}
            </Button>
          ) : null}
          {step === 4 ? (
            <Button type="primary" onClick={props.onEnterConsole}>
              {t('bootstrap.enterConsole')}
            </Button>
          ) : null}
        </Space>
      </div>
    </div>
  )
}
