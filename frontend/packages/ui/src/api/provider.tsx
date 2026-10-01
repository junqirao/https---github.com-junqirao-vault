import { createContext, useContext, type ReactNode } from 'react'

import type { VaultApi } from './endpoints'

const ApiContext = createContext<VaultApi | null>(null)

/** 注入 REST 客户端（由宿主应用构造，UI 包不关心地址与令牌来源）。 */
export function ApiProvider({ api, children }: { api: VaultApi; children: ReactNode }): JSX.Element {
  return <ApiContext.Provider value={api}>{children}</ApiContext.Provider>
}

/** 读取当前 REST 客户端。 */
export function useApi(): VaultApi {
  const api = useContext(ApiContext)
  if (!api) throw new Error('useApi() 必须在 <ApiProvider> 内使用')
  return api
}
