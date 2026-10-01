import { CheckCircleFilled, CloseCircleFilled } from '@ant-design/icons'
import { Space, Tag, Typography } from 'antd'

import { useI18n } from '../i18n'
import { palette, spacing } from '../tokens/palette'
import { PASSWORD_MAX, PASSWORD_MIN, hasLetterAndDigit } from '../utils/validation'

export interface PasswordStrengthProps {
  value: string
}

/** 密码强度提示：与服务端规则一致（长度 8~128 且同时含字母与数字）。 */
export function PasswordStrength({ value }: PasswordStrengthProps): JSX.Element {
  const { t } = useI18n()
  const lengthOk = value.length >= PASSWORD_MIN && value.length <= PASSWORD_MAX
  const letterDigitOk = hasLetterAndDigit(value)
  const satisfied = (lengthOk ? 1 : 0) + (letterDigitOk ? 1 : 0)

  const strengthKey = satisfied === 2 ? 'password.strength.strong' : satisfied === 1 ? 'password.strength.medium' : 'password.strength.weak'
  const color = satisfied === 2 ? palette.success : satisfied === 1 ? palette.warning : palette.danger

  const rules: Array<{ label: string; ok: boolean }> = [
    { label: t('password.rule.length'), ok: lengthOk },
    { label: t('password.rule.letter'), ok: /[A-Za-z]/.test(value) },
    { label: t('password.rule.digit'), ok: /[0-9]/.test(value) }
  ]

  return (
    <div style={{ marginBottom: spacing.sm }}>
      <Space direction="vertical" size={2}>
        {rules.map((rule) => (
          <Space key={rule.label} size={6}>
            {rule.ok ? (
              <CheckCircleFilled style={{ color: palette.success, fontSize: 12 }} />
            ) : (
              <CloseCircleFilled style={{ color: 'rgba(0,0,0,0.25)', fontSize: 12 }} />
            )}
            <Typography.Text type={rule.ok ? undefined : 'secondary'} style={{ fontSize: 12 }}>
              {rule.label}
            </Typography.Text>
          </Space>
        ))}
        <Space size={6}>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            {t('password.strength.label')}
          </Typography.Text>
          <Tag color={color} style={{ marginInlineEnd: 0 }}>
            {t(strengthKey)}
          </Tag>
        </Space>
      </Space>
    </div>
  )
}
