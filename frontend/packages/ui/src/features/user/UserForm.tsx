import { useEffect, useState } from 'react'
import { Form, Input, InputNumber, Modal, Select, Switch } from 'antd'
import { useMutation } from '@tanstack/react-query'

import { ErrorNotice } from '../../components/ErrorNotice'
import { useApi } from '../../api/provider'
import type { CreateUserResponse, Role, UserDTO } from '../../api/types'
import { useI18n } from '../../i18n'
import { CAPACITY_UNIT, capacityNormalize, capacityValueProps } from '../../utils/format'
import { isValidPassword, isValidUsername, PASSWORD_MAX, PASSWORD_MIN, USERNAME_MAX, USERNAME_MIN } from '../../utils/validation'

export interface UserFormProps {
  open: boolean
  mode: 'create' | 'edit'
  user?: UserDTO | null
  onCancel: () => void
  onSubmitted: (result: CreateUserResponse) => void
}

interface FormValues {
  username: string
  password?: string
  role: Role
  quota_bytes?: number
  remark?: string
  enabled?: boolean
}

/** 用户表单（新建 / 编辑）。 */
export function UserForm({ open, mode, user, onCancel, onSubmitted }: UserFormProps): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const [form] = Form.useForm<FormValues>()
  const [error, setError] = useState<unknown>(null)

  useEffect(() => {
    if (!open) return
    setError(null)
    if (mode === 'edit' && user) {
      form.setFieldsValue({
        username: user.username,
        role: user.role,
        quota_bytes: user.quota_bytes,
        remark: user.remark,
        enabled: user.enabled
      })
    } else {
      form.resetFields()
      form.setFieldsValue({ role: 'user' })
    }
  }, [form, mode, open, user])

  const mutation = useMutation({
    mutationFn: async (values: FormValues) => {
      if (mode === 'create') {
        return api.createUser({
          username: values.username.trim(),
          password: values.password ?? '',
          role: values.role,
          quota_bytes: values.quota_bytes,
          remark: values.remark
        })
      }
      if (!user) throw new Error('missing user')
      return api.updateUser(user.id, {
        role: values.role,
        quota_bytes: values.quota_bytes,
        remark: values.remark,
        enabled: values.enabled,
        password: values.password ? values.password : undefined
      })
    },
    onSuccess: (result) => onSubmitted(result),
    onError: (err) => setError(err)
  })

  const submit = (): void => {
    void form.validateFields().then((values) => mutation.mutate(values))
  }

  return (
    <Modal
      open={open}
      title={t(mode === 'create' ? 'user.create.title' : 'user.edit.title')}
      width={640}
      centered
      okText={t('common.save')}
      cancelText={t('common.cancel')}
      confirmLoading={mutation.isPending}
      onOk={submit}
      onCancel={onCancel}
      destroyOnClose
      maskClosable={false}
    >
      {error ? <ErrorNotice error={error} /> : null}
      <Form form={form} layout="vertical">
        {mode === 'create' ? (
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
                    : Promise.reject(
                        new Error(t('err.param.username.length', { min_length: USERNAME_MIN, max_length: USERNAME_MAX }))
                      )
              }
            ]}
          >
            <Input autoComplete="off" spellCheck={false} />
          </Form.Item>
        ) : null}
        <Form.Item
          name="password"
          label={t(mode === 'create' ? 'field.password' : 'user.passwordReset')}
          extra={mode === 'create' ? t('bootstrap.password.hint') : undefined}
          rules={
            mode === 'create'
              ? [
                  { required: true },
                  {
                    validator: (_rule, value: string) =>
                      isValidPassword(value ?? '')
                        ? Promise.resolve()
                        : Promise.reject(new Error(t('err.param.password.need_letter_and_digit')))
                  }
                ]
              : [
                  {
                    validator: (_rule, value: string) =>
                      !value || (value.length >= PASSWORD_MIN && value.length <= PASSWORD_MAX && isValidPassword(value))
                        ? Promise.resolve()
                        : Promise.reject(new Error(t('err.param.password.need_letter_and_digit')))
                  }
                ]
          }
        >
          <Input.Password autoComplete="new-password" />
        </Form.Item>
        <Form.Item name="role" label={t('field.role')} rules={[{ required: true }]}>
          <Select<Role>
            options={[
              { value: 'user', label: t('role.user') },
              { value: 'super_admin', label: t('role.super_admin') }
            ]}
          />
        </Form.Item>
        <Form.Item
          name="quota_bytes"
          label={t('field.quota')}
          extra={t('user.quota.hint')}
          getValueProps={capacityValueProps}
          normalize={capacityNormalize}
        >
          <InputNumber min={0} addonAfter={CAPACITY_UNIT} style={{ width: '100%' }} />
        </Form.Item>
        <Form.Item name="remark" label={t('field.remark')}>
          <Input.TextArea rows={2} maxLength={200} />
        </Form.Item>
        {mode === 'edit' ? (
          <Form.Item name="enabled" label={t('common.status')} valuePropName="checked">
            <Switch checkedChildren={t('common.enabled')} unCheckedChildren={t('common.disabled')} />
          </Form.Item>
        ) : null}
      </Form>
    </Modal>
  )
}
