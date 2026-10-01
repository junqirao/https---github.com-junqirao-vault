import { Alert, Space, Typography } from 'antd'

import { errorCodeOf, translateError } from '../api/errors'

export interface ErrorNoticeProps {
  error: unknown
  /** 业务错误时附带展示原始 code，便于排障（不替代本地化文案）。 */
  showCode?: boolean
}

/** 错误提示：业务错误按 code 本地化，网络/超时使用本地文案。 */
export function ErrorNotice({ error, showCode }: ErrorNoticeProps): JSX.Element | null {
  if (!error) return null
  const code = showCode === false ? undefined : errorCodeOf(error)
  return (
    <Alert
      type="error"
      showIcon
      style={{ marginBottom: 16 }}
      message={
        <Space direction="vertical" size={0}>
          <Typography.Text>{translateError(error)}</Typography.Text>
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
