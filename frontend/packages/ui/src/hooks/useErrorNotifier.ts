import { useCallback } from 'react'
import { App } from 'antd'

import { translateError } from '../api/errors'

/** 统一的错误提示器：业务错误按 code 本地化，网络/超时使用本地文案。 */
export function useErrorNotifier(): (error: unknown) => void {
  const { message } = App.useApp()
  return useCallback(
    (error: unknown) => {
      message.error(translateError(error))
    },
    [message]
  )
}
