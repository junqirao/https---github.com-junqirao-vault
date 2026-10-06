import { Alert, Space, Typography } from 'antd'

import { errorCodeOf, errorHintKeyOf, translateError } from '../api/errors'
import { useI18n } from '../i18n'

export interface ErrorNoticeProps {
  error: unknown
  /** 业务错误时附带展示原始 code，便于排障（不替代本地化文案）。 */
  showCode?: boolean
}

/**
 * 错误提示：业务错误按 code 本地化，网络/超时使用本地文案。
 *
 * 业务错误若有对应的**可执行建议**（errorHintKeyOf，如"卷被占用请关掉占用程序"），
 * 一并贴出来 —— 只有一句"卸载失败（阶段：mount_point）"用户根本不知道该做什么。
 */
export function ErrorNotice({ error, showCode }: ErrorNoticeProps): JSX.Element | null {
  const { t } = useI18n()
  if (!error) return null
  const code = showCode === false ? undefined : errorCodeOf(error)
  const hintKey = errorHintKeyOf(error)
  return (
    <Alert
      type="error"
      showIcon
      style={{ marginBottom: 16 }}
      message={
        // 错误详情是用户要复制给管理员的原文：单独放开文本选择（全局默认禁选）。
        <Space className="selectable" direction="vertical" size={0}>
          <Typography.Text>{translateError(error)}</Typography.Text>
          {hintKey ? (
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              {t(hintKey)}
            </Typography.Text>
          ) : null}
          {code ? (
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              {code}
            </Typography.Text>
          ) : null}
        </Space>
      }
    />
  )
}
