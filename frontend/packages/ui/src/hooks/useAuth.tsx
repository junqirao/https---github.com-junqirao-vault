import { createContext, useContext, type ReactNode } from 'react'

import type { Role } from '../api/types'

export interface SessionUser {
  id: string
  username: string
  role: Role
}

export interface AuthContextValue {
  user: SessionUser | null
  isAuthenticated: boolean
  isSuperAdmin: boolean
  logout: () => void
}

const AuthContext = createContext<AuthContextValue | null>(null)

export function AuthProvider({ value, children }: { value: AuthContextValue; children: ReactNode }): JSX.Element {
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}

/** 当前会话（用户与权限）。 */
export function useAuth(): AuthContextValue {
  const value = useContext(AuthContext)
  if (!value) throw new Error('useAuth() 必须在 <AuthProvider> 内使用')
  return value
}

/** 权限判定：仅超级管理员可见管理模块。 */
export function usePermission(): { isSuperAdmin: boolean } {
  const { isSuperAdmin } = useAuth()
  return { isSuperAdmin }
}
