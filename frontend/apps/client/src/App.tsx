import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { App as AntApp, ConfigProvider } from 'antd'
import enUS from 'antd/locale/en_US'
import jaJP from 'antd/locale/ja_JP'
import koKR from 'antd/locale/ko_KR'
import zhCN from 'antd/locale/zh_CN'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { HashRouter } from 'react-router-dom'

import {
  ANTD_LOCALE_NAME,
  ApiProvider,
  AuthProvider,
  DEFAULT_SERVER_URL,
  ErrorBoundary,
  LoadingState,
  ServerConfigProvider,
  VaultApi,
  agentApi,
  createVaultTheme,
  identityMatchesServer,
  isAgentUnavailableError,
  isApiError,
  subscribeAgentSession,
  trustServerCertificate,
  type AgentIdentity,
  type AgentSessionState,
  type BootstrapResponse,
  type LoginResponse,
  type Role,
  type ServerEntry,
  type SessionUser
} from '@vault/ui'

import { PhaseView, type CertLoginFailure } from './flow/PhaseView'
import { useStartupFlow } from './flow/useStartupFlow'
import { selectActiveServer, useAppStore } from './store/appStore'

/** antd 语言包映射（key 为 @vault/ui 的 ANTD_LOCALE_NAME 取值）。 */
const ANTD_LOCALES: Record<string, typeof zhCN> = {
  zh_CN: zhCN,
  en_US: enUS,
  ja_JP: jaJP,
  ko_KR: koKR
}

const queryClient = new QueryClient({
  defaultOptions: {
    queries: { retry: 1, refetchOnWindowFocus: false, staleTime: 5000 }
  }
})

/**
 * 自动免密登录遇到"代理暂不可用"时的退避重试间隔（毫秒）。
 *
 * 代理是独立进程，界面常常比它先就绪；只试一次会出现"这次开机没能免密登录、
 * 下次就好了"的随机现象，用户只会觉得"又要输密码"。
 */
const CERT_LOGIN_RETRY_DELAYS_MS = [0, 1000, 3000]

/** 未勾选"记住我"时，令牌仅驻留内存，不落盘。 */
interface VolatileSession {
  key: string
  token: string
  expiresAt: number
  user: SessionUser
}

export function App(): JSX.Element {
  const hydrated = useAppStore((state) => state.hydrated)
  const hydrate = useAppStore((state) => state.hydrate)
  const servers = useAppStore((state) => state.servers)
  const activeKey = useAppStore((state) => state.activeKey)
  const active = useAppStore(selectActiveServer)
  const language = useAppStore((state) => state.language)
  const setLanguage = useAppStore((state) => state.setLanguage)
  const setActiveServer = useAppStore((state) => state.setActive)
  const upsertServer = useAppStore((state) => state.upsertServer)
  const removeServer = useAppStore((state) => state.removeServer)
  const updateActiveServer = useAppStore((state) => state.updateActiveServer)
  const updateActiveToken = useAppStore((state) => state.updateActiveToken)

  const volatileSessionRef = useRef<VolatileSession | null>(null)
  const unauthorizedRef = useRef<() => void>(() => undefined)
  /** 401 续期的单飞 Promise：并发 401 只触发一次续期。 */
  const renewInFlightRef = useRef<Promise<boolean> | null>(null)
  /** 续期入口经 ref 暴露给 ApiClient（避免 useMemo 依赖变化）。 */
  const renewRef = useRef<() => Promise<boolean>>(async () => false)
  /** 进入登录阶段后是否已尝试过免密登录（只试一次，避免失败后反复重试）。 */
  const autoLoginTriedRef = useRef(false)
  const [autoLoginState, setAutoLoginState] = useState<'idle' | 'pending' | 'failed'>('idle')
  /** 会话过期（401 且续期失败）后进入登录阶段时的提示。 */
  const [sessionExpired, setSessionExpired] = useState(false)
  /** 证书免密登录失败的原因（登录页据此给出可操作提示）。 */
  const [certLoginFailure, setCertLoginFailure] = useState<CertLoginFailure | null>(null)
  /** 本机证书已安装且绑定当前服务端 → 登录页展示"用客户端证书登录"按钮。 */
  const [certLoginAvailable, setCertLoginAvailable] = useState(false)
  const [certLoginBusy, setCertLoginBusy] = useState(false)

  const api = useMemo(
    () =>
      new VaultApi({
        getConfig: () => {
          const current = selectActiveServer(useAppStore.getState())
          const volatile = volatileSessionRef.current
          const token =
            current?.token ?? (volatile && current && volatile.key === current.key ? volatile.token : undefined)
          return { baseUrl: current?.baseUrl ?? DEFAULT_SERVER_URL, token }
        },
        onUnauthorized: () => unauthorizedRef.current(),
        // 401 兜底：先用本地证书续期一次（单飞），失败则回到登录阶段。
        onRenewSession: () => renewRef.current()
      }),
    []
  )

  const flow = useStartupFlow(api)
  const { setPhase } = flow

  useEffect(() => {
    void hydrate()
  }, [hydrate])

  const clearSession = useCallback((): void => {
    volatileSessionRef.current = null
    updateActiveServer({
      token: undefined,
      tokenExpiresAt: undefined,
      userId: undefined,
      username: undefined,
      role: undefined
    })
  }, [updateActiveServer])

  useEffect(() => {
    unauthorizedRef.current = () => {
      // 先判断此前是否真的持有会话：仅在"曾经登录过"时提示"登录已过期"。
      const hadSession = Boolean(selectActiveServer(useAppStore.getState())?.token ?? volatileSessionRef.current)
      clearSession()
      if (hadSession) setSessionExpired(true)
      setPhase({ kind: 'login' })
    }
  }, [clearSession, setPhase])

  const sessionUser: SessionUser | null = useMemo(() => {
    const volatile = volatileSessionRef.current
    if (volatile && active && volatile.key === active.key) return volatile.user
    if (active?.userId && active.username && active.role) {
      return { id: active.userId, username: active.username, role: active.role }
    }
    return null
  }, [active])

  /**
   * 把当前会话推送给本地代理（代理据此访问服务端）。
   *
   * 失败不得阻塞登录流程：仅记录错误码（绝不记录令牌），顶栏依据代理状态提示。
   */
  const pushAgentSession = useCallback(async (): Promise<void> => {
    const current = selectActiveServer(useAppStore.getState())
    if (!current) return
    const volatile = volatileSessionRef.current
    const sameKey = volatile && volatile.key === current.key
    const token = current.token ?? (sameKey ? volatile.token : undefined)
    const user: SessionUser | null = sameKey
      ? volatile.user
      : current.userId && current.username && current.role
        ? { id: current.userId, username: current.username, role: current.role }
        : null
    if (!token || !user) return
    const expiresAt = current.tokenExpiresAt ?? (sameKey ? volatile.expiresAt : undefined)
    try {
      await agentApi.pushSession({
        server_url: current.baseUrl,
        server_instance_id: current.instanceId ?? current.key,
        server_name: current.serverName || current.baseUrl,
        token,
        user_id: user.id,
        username: user.username,
        expires_at: expiresAt ?? Date.now() + 12 * 60 * 60 * 1000,
        // 代理据此固定服务端证书指纹（自签 CA 场景）；未探测过则为空，退回默认链路校验。
        cert_sha256: current.certSha256
      })
    } catch (error) {
      const code = isApiError(error) ? error.code : error instanceof Error ? error.name : 'unknown'
      console.warn(`[agent] session push failed: ${code}`)
    }
  }, [])

  /**
   * 应用代理侧自动续期得到的新会话（session 事件 / GET /agent/state）。
   *
   * 把新令牌同步到活动服务端会话：API 客户端（getConfig）读取同一来源，因此后续请求即用新令牌。
   * "记住我"的会话写回 zustand store；仅本机的易失会话（未记住）只更新内存 ref，避免令牌落盘。
   */
  const applyRenewedSession = useCallback(
    (session: AgentSessionState): void => {
      const current = selectActiveServer(useAppStore.getState())
      if (!current) return
      const volatile = volatileSessionRef.current
      const user: SessionUser = {
        id: session.user_id || current.userId || '',
        username: session.username || current.username || '',
        role: current.role ?? 'user'
      }
      if (volatile && volatile.key === current.key) {
        if (volatile.token === session.token && volatile.expiresAt === session.expires_at) return
        volatileSessionRef.current = {
          key: current.key,
          token: session.token,
          expiresAt: session.expires_at,
          user
        }
        return
      }
      if (current.token === session.token && current.tokenExpiresAt === session.expires_at) return
      updateActiveToken(session.token, session.expires_at)
    },
    [updateActiveToken]
  )

  /**
   * 401 兜底：用本地客户端证书续期会话一次（单飞）。
   *
   * ⚠️ 这里**不要求** agent 的 auto_login 开关：auto_login 管的是"启动/登录页是否自动免密登录"，
   * 而本函数是"会话失效后的自愈"。服务端重启会清空内存会话表，证书登录的会话必须能自动恢复，
   * 否则用户只是重启了一下服务端就被踢回登录页重新输密码。
   */
  const renewSession = useCallback((): Promise<boolean> => {
    const existing = renewInFlightRef.current
    if (existing) return existing
    const task = (async (): Promise<boolean> => {
      try {
        const current = selectActiveServer(useAppStore.getState())
        if (!current) return false
        const identity = await agentApi.getIdentity()
        if (!identity.installed || !identityMatchesServer(identity, current)) return false
        const response = await agentApi.identityLogin({ server_url: current.baseUrl })
        applyRenewedSession({
          server_url: current.baseUrl,
          token: response.token,
          expires_at: response.expires_at,
          user_id: response.user.id,
          username: response.user.username
        })
        // 同步给本地代理：代理侧同样可能因服务端重启而持有失效令牌。
        void pushAgentSession()
        return true
      } catch (error) {
        const code = isApiError(error) ? error.code : error instanceof Error ? error.name : 'unknown'
        console.warn(`[agent] session renew failed: ${code}`)
        return false
      }
    })()
    renewInFlightRef.current = task
    void task.finally(() => {
      renewInFlightRef.current = null
    })
    return task
  }, [applyRenewedSession, pushAgentSession])

  useEffect(() => {
    renewRef.current = renewSession
  }, [renewSession])

  // 代理自动续期后经 session 事件同步最新令牌到活动服务端会话。
  useEffect(() => subscribeAgentSession(applyRenewedSession), [applyRenewedSession])

  // 登录态或服务端变化后同步会话到本地代理（代理不可用时静默失败）。
  useEffect(() => {
    if (!hydrated) return
    void pushAgentSession()
  }, [hydrated, activeKey, sessionUser, pushAgentSession])

  const handleConfigure = useCallback(
    async (baseUrl: string): Promise<void> => {
      const before = useAppStore.getState().activeKey
      // 经本地代理探测：代理能安全处理自签证书，并回传服务端名称、实例 ID 与证书指纹。
      // 探测失败必须向上抛（由配置页展示原因），否则就成了"点了没反应"。
      const probe = await agentApi.testServer(baseUrl)
      const effective = probe.server_url || baseUrl
      const certSha256 = probe.cert?.sha256
      // 固化证书指纹（TOFU 首次信任）：渲染进程与本地代理此后都按该指纹校验服务端证书。
      await trustServerCertificate(effective, certSha256)
      const entry: ServerEntry = {
        key: probe.server_instance_id || effective,
        baseUrl: effective,
        serverName: probe.server_name || '',
        instanceId: probe.server_instance_id || undefined,
        certSha256
      }
      upsertServer(entry)
      if (useAppStore.getState().activeKey === before) flow.recheck()
    },
    [flow, upsertServer]
  )

  /**
   * 写入会话（口令登录与证书免密登录共用的成功路径）。
   *
   * 代理透出的证书登录响应可能只含 id/username，缺 role 时回退到该服务端已知角色，
   * 否则会话会被判为未认证。
   */
  const applyLoginSession = useCallback(
    (
      response: { token: string; expires_at: number; user: { id: string; username: string; role: Role } },
      remember: boolean
    ): void => {
      const current = selectActiveServer(useAppStore.getState())
      const user: SessionUser = {
        id: response.user.id,
        username: response.user.username,
        role: response.user.role ?? current?.role ?? 'user'
      }
      // 重新登录成功：清除"登录已过期"提示。
      setSessionExpired(false)
      if (remember) {
        volatileSessionRef.current = null
        updateActiveServer({
          token: response.token,
          tokenExpiresAt: response.expires_at,
          userId: user.id,
          username: user.username,
          role: user.role
        })
      } else {
        volatileSessionRef.current = current
          ? { key: current.key, token: response.token, expiresAt: response.expires_at, user }
          : null
      }
    },
    [updateActiveServer]
  )

  const handleLogin = useCallback(
    async (values: { username: string; password: string; remember: boolean }): Promise<void> => {
      const response: LoginResponse = await api.login({ username: values.username, password: values.password })
      applyLoginSession(response, values.remember)
      setPhase({ kind: 'ready' })
      void pushAgentSession()
    },
    [api, applyLoginSession, pushAgentSession, setPhase]
  )

  const handleBootstrap = useCallback(
    async (input: { username: string; password: string; serverName?: string }): Promise<BootstrapResponse> => {
      const response = await api.bootstrap({
        username: input.username,
        password: input.password,
        server_name: input.serverName
      })
      volatileSessionRef.current = null
      setSessionExpired(false)
      updateActiveServer({
        token: response.token,
        tokenExpiresAt: response.expires_at,
        userId: response.user.id,
        username: response.user.username,
        role: response.user.role,
        serverName: input.serverName || selectActiveServer(useAppStore.getState())?.serverName
      })
      void pushAgentSession()
      return response
    },
    [api, pushAgentSession, updateActiveServer]
  )

  const handleLogout = useCallback((): void => {
    // 主动登出后不再**自动**用证书登录，避免"点了退出又被登回来"。
    // （登录页仍保留"用客户端证书登录"按钮：那是用户的显式动作。）
    autoLoginTriedRef.current = true
    setSessionExpired(false)
    setCertLoginFailure(null)
    void api.logout().catch(() => undefined)
    void agentApi.clearSession().catch(() => undefined)
    clearSession()
    setPhase({ kind: 'login' })
  }, [api, clearSession, setPhase])

  // 进入登录阶段即清除自动登录提示态，避免登出后残留加载/失败提示。
  useEffect(() => {
    if (flow.phase.kind !== 'login') setAutoLoginState('idle')
  }, [flow.phase.kind])

  /**
   * 用本地客户端证书换取会话（免密登录）。
   *
   * 返回 null 表示成功，否则返回失败原因码（由调用方决定怎么提示）。
   * 刻意不抛异常：自动尝试与登录页的"用客户端证书登录"按钮共用同一实现，两者都只要结果。
   *
   * 与 renewSession（上面的 401 自愈）是**有意分开**的两条路径，不要合并：
   *   - 本函数在登录页使用，成功后写入完整会话（与口令登录同一条成功路径）；
   *   - renewSession 在"已在应用内"时使用，只替换令牌，避免把未勾选"记住我"的
   *     易失会话意外落盘。
   */
  const certLogin = useCallback(async (): Promise<CertLoginFailure | null> => {
    const current = selectActiveServer(useAppStore.getState())
    if (!current) return 'no_server'

    let identity: AgentIdentity
    try {
      identity = await agentApi.getIdentity()
    } catch (error) {
      // 代理不可用（未启动/未推送到渲染进程）：初始化阶段常见，调用方会退避重试。
      const code = isAgentUnavailableError(error) ? 'agent_unavailable' : 'unknown'
      console.warn(`[agent] identity unavailable: ${code}`)
      return 'agent_unavailable'
    }
    if (!identity.installed) {
      setCertLoginAvailable(false)
      return 'not_installed'
    }
    if (!identityMatchesServer(identity, current)) {
      setCertLoginAvailable(false)
      return 'server_mismatch'
    }
    setCertLoginAvailable(true)

    try {
      const response = await agentApi.identityLogin({ server_url: current.baseUrl })
      applyLoginSession(response, true)
      setSessionExpired(false)
      setCertLoginFailure(null)
      setPhase({ kind: 'ready' })
      // 同步给本地代理：代理侧同样可能因服务端重启而持有失效令牌。
      void pushAgentSession()
      return null
    } catch (error) {
      const code = isApiError(error) ? error.code : error instanceof Error ? error.name : 'unknown'
      console.warn(`[agent] cert login failed: ${code}`)
      return 'rejected'
    }
  }, [applyLoginSession, pushAgentSession, setPhase])

  /**
   * 登录页上的"用客户端证书登录"：手动重试一次。
   *
   * 自动尝试只跑一轮（避免失败后反复打服务端），这个入口让用户不必重开客户端
   * （服务端刚重启完、证书刚安装完等场景）。
   */
  const handleCertificateLogin = useCallback((): void => {
    setCertLoginBusy(true)
    setAutoLoginState('pending')
    void (async (): Promise<void> => {
      const failure = await certLogin()
      setCertLoginFailure(failure)
      setAutoLoginState(failure ? 'failed' : 'idle')
      setCertLoginBusy(false)
    })()
  }, [certLogin])

  /**
   * 进入登录阶段后自动用客户端证书免密登录（自动只尝试一次，失败后可由用户手动重试）。
   *
   * ⚠️ 与旧实现的两点差异：
   *  1) **不再要求**代理的 auto_login 开关为 true。该开关现在默认开启，
   *     只有运维显式关掉（auto_login === false）才跳过自动免密登录；
   *     旧实现把"配置里没有这一项"的零值 false 当成"用户不想免密登录"，
   *     于是服务端每次重启都要求重新输密码。服务端会话只存在内存里，重启即全部失效。
   *  2) 代理/身份可能比界面启动得慢，因此对"代理暂不可用"做几次退避重试（见
   *     CERT_LOGIN_RETRY_DELAYS_MS）；旧实现只试一次，失败就永久放弃，
   *     表现为"有时能免密、有时不能"。
   */
  useEffect(() => {
    if (!hydrated || flow.phase.kind !== 'login' || autoLoginTriedRef.current) return
    autoLoginTriedRef.current = true

    void (async (): Promise<void> => {
      try {
        const config = await agentApi.getConfig()
        if (config.auto_login === false) return
      } catch {
        // 代理不可用：不在此处返回，交给下面的重试与失败提示，避免"什么都不说"。
      }

      setAutoLoginState('pending')
      let failure: CertLoginFailure | null = 'agent_unavailable'
      for (const delay of CERT_LOGIN_RETRY_DELAYS_MS) {
        if (delay > 0) await new Promise((resolve) => setTimeout(resolve, delay))
        failure = await certLogin()
        if (failure !== 'agent_unavailable') break
      }
      setCertLoginFailure(failure)
      setAutoLoginState(failure ? 'failed' : 'idle')
    })()
  }, [hydrated, flow.phase.kind, certLogin])

  const antdLocale = ANTD_LOCALES[ANTD_LOCALE_NAME[language]] ?? zhCN

  return (
    <ConfigProvider theme={createVaultTheme()} locale={antdLocale}>
      <AntApp>
        <QueryClientProvider client={queryClient}>
          <ApiProvider api={api}>
            <ServerConfigProvider
              value={{
                servers,
                activeKey,
                active,
                setActive: setActiveServer,
                upsert: upsertServer,
                remove: removeServer,
                updateActive: updateActiveServer
              }}
            >
              <AuthProvider
                value={{
                  user: sessionUser,
                  isAuthenticated: Boolean(sessionUser),
                  isSuperAdmin: sessionUser?.role === 'super_admin',
                  logout: handleLogout
                }}
              >
                <HashRouter>
                  {!hydrated ? (
                    <LoadingState fullscreen />
                  ) : (
                    <ErrorBoundary>
                      <PhaseView
                        flow={flow}
                        language={language}
                        onLanguageChange={setLanguage}
                        onConfigure={handleConfigure}
                        onLogin={handleLogin}
                        onBootstrap={handleBootstrap}
                        serverName={active?.serverName || active?.baseUrl}
                        autoLoginPending={autoLoginState === 'pending'}
                        autoLoginFailed={autoLoginState === 'failed'}
                        sessionExpired={sessionExpired}
                        certificateLoginFailure={certLoginFailure}
                        certificateLoginAvailable={certLoginAvailable}
                        certificateLoginBusy={certLoginBusy}
                        onCertificateLogin={handleCertificateLogin}
                      />
                    </ErrorBoundary>
                  )}
                </HashRouter>
              </AuthProvider>
            </ServerConfigProvider>
          </ApiProvider>
        </QueryClientProvider>
      </AntApp>
    </ConfigProvider>
  )
}
