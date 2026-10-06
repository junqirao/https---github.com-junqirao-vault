import { useState } from 'react'

import {
  BlockedNotice,
  BootstrapWizard,
  CompatNotice,
  DEFAULT_SERVER_URL,
  LanguageSwitcher,
  LoadingState,
  LoginForm,
  OfflineNotice,
  ServerEndpointForm,
  agentApi,
  useI18n,
  useServerConfig,
  type BootstrapResponse,
  type Language,
  type ServerChoice
} from '@vault/ui'

import type { StartupFlow } from './useStartupFlow'
import { AppRoutes } from '../routes'

export interface PhaseViewProps {
  flow: StartupFlow
  language: Language
  onLanguageChange: (language: Language) => void
  onConfigure: (baseUrl: string) => Promise<void>
  onLogin: (values: { username: string; password: string; remember: boolean }) => Promise<void>
  onBootstrap: (input: { username: string; password: string; serverName?: string }) => Promise<BootstrapResponse>
  serverName?: string
  /** 正在用客户端证书静默登录（登录阶段展示加载态）。 */
  autoLoginPending?: boolean
  /** 自动登录未成功（登录阶段展示一句短提示后回退到口令表单）。 */
  autoLoginFailed?: boolean
  /** 会话已过期且未能自动续期（登录阶段提示"请重新登录"）。 */
  sessionExpired?: boolean
  /** 证书免密登录失败的原因（null 表示未失败）。 */
  certificateLoginFailure?: CertLoginFailure | null
  /** 本机证书可用（已安装且绑定当前服务端）→ 登录页展示"用客户端证书登录"按钮。 */
  certificateLoginAvailable?: boolean
  /** 正在用证书登录（按钮 loading）。 */
  certificateLoginBusy?: boolean
  /** 点击"用客户端证书登录"：手动重试一次（不受"自动只试一次"的限制）。 */
  onCertificateLogin?: () => void
}

/**
 * 客户端证书免密登录的失败原因。
 *
 * 之所以细分到原因：只提示一句"自动登录未成功，请使用密码登录"，用户无法判断
 * 是代理没起来、本机没装证书，还是证书不属于当前服务端，也就无从下手处理。
 */
export type CertLoginFailure =
  | 'agent_unavailable'
  | 'not_installed'
  | 'server_mismatch'
  | 'rejected'
  | 'no_server'

/** 失败原因 → 提示文案 key（均在 i18n 的 auth.certLogin.* 下）。 */
const CERT_LOGIN_NOTICE: Record<CertLoginFailure, string> = {
  agent_unavailable: 'auth.certLogin.agentUnavailable',
  not_installed: 'auth.certLogin.notInstalled',
  server_mismatch: 'auth.certLogin.mismatch',
  rejected: 'auth.certLogin.rejected',
  no_server: 'auth.autoLogin.failed'
}

/** 按启动阶段渲染对应界面（服务端配置 / 初始化向导 / 登录 / 主界面 / 各类提示页）。 */
export function PhaseView({
  flow,
  language,
  onLanguageChange,
  onConfigure,
  onLogin,
  onBootstrap,
  serverName,
  autoLoginPending,
  autoLoginFailed,
  sessionExpired,
  certificateLoginFailure,
  certificateLoginAvailable,
  certificateLoginBusy,
  onCertificateLogin
}: PhaseViewProps): JSX.Element {
  const { t } = useI18n()
  const { phase, checking, setPhase, recheck, continueAfterCompat } = flow
  const [configuring, setConfiguring] = useState(false)
  // 服务端注册表来自 <ServerConfigProvider>（与顶栏切换器同一份数据）。
  const { servers, activeKey, active, setActive } = useServerConfig()
  const activeBaseUrl = active?.baseUrl

  /**
   * "无法连接服务端"页上的备选服务端：**除当前这台以外**的已保存服务端。
   *
   * 只做展示层筛选（key 即服务端实例标识，同一台服务端在 store 里只有一条，见 appStore 的去重），
   * 选中后交给 setActive —— activeKey 一变，useStartupFlow 的 effect 就会对新服务端重跑整套检查。
   */
  const serverChoices: ServerChoice[] = servers
    .filter((server) => server.key !== activeKey)
    .map((server) => ({ key: server.key, label: server.serverName || server.baseUrl, baseUrl: server.baseUrl }))

  if (configuring || phase.kind === 'config') {
    return (
      <ServerEndpointForm
        initialBaseUrl={activeBaseUrl || DEFAULT_SERVER_URL}
        onTest={(baseUrl) => agentApi.testServer(baseUrl)}
        onSubmit={async (baseUrl) => {
          await onConfigure(baseUrl)
          setConfiguring(false)
        }}
        submitting={checking}
        onCancel={activeBaseUrl ? () => setConfiguring(false) : undefined}
      />
    )
  }

  if (phase.kind === 'loading') {
    return (
      <>
        <LanguageCorner language={language} onLanguageChange={onLanguageChange} />
        <LoadingState fullscreen text={t('boot.checking')} />
      </>
    )
  }

  if (phase.kind === 'offline') {
    return (
      <OfflineNotice
        detail={phase.detail}
        loading={checking}
        onRetry={recheck}
        onChangeAddress={() => setConfiguring(true)}
        // 有其他服务端时多一个"选择服务器"入口：连不上当前这台时，切到另一台已配好的
        // 服务端比重新填地址快得多（详见 OfflineNotice 的说明）。
        alternatives={serverChoices}
        onSelectServer={setActive}
      />
    )
  }

  if (phase.kind === 'blocked') {
    return (
      <BlockedNotice
        reason={phase.status.blocked_reason}
        onGoLogin={() => setPhase({ kind: 'login' })}
        onChangeAddress={() => setConfiguring(true)}
      />
    )
  }

  if (phase.kind === 'compat') {
    return (
      <CompatNotice
        check={phase.check}
        onContinue={() => continueAfterCompat(phase.check)}
        onChangeAddress={() => setConfiguring(true)}
      />
    )
  }

  if (phase.kind === 'bootstrap') {
    return (
      <BootstrapWizard
        serverName={phase.status.server_name}
        serverInstanceId={phase.status.server_instance_id}
        apiVersion={phase.status.api_version}
        submitting={checking}
        onSubmit={onBootstrap}
        onEnterConsole={() => setPhase({ kind: 'ready' })}
        onGoLogin={() => setPhase({ kind: 'login' })}
      />
    )
  }

  if (phase.kind === 'login') {
    return (
      <>
        <LanguageCorner language={language} onLanguageChange={onLanguageChange} />
        {autoLoginPending ? (
          <LoadingState fullscreen text={t('login.loggingIn')} />
        ) : (
          <LoginForm
            serverName={serverName}
            onBack={() => setConfiguring(true)}
            onSubmit={onLogin}
            notice={
              autoLoginFailed && certificateLoginFailure
                ? t(CERT_LOGIN_NOTICE[certificateLoginFailure])
                : autoLoginFailed
                  ? t('auth.autoLogin.failed')
                  : sessionExpired
                    ? t('err.auth.session_expired')
                    : undefined
            }
            certificateLogin={
              onCertificateLogin && certificateLoginAvailable
                ? { busy: certificateLoginBusy, onClick: onCertificateLogin }
                : undefined
            }
          />
        )}
      </>
    )
  }

  return (
    <AppRoutes
      language={language}
      onLanguageChange={onLanguageChange}
      onManageServers={() => setConfiguring(true)}
      autoMount={flow.autoMount}
    />
  )
}

/**
 * 语言切换：放在窗口**左下角**，不占顶部视觉焦点（启动/登录/初始化阶段还没有侧边栏，
 * 因此这里给一个左下角入口；进入主界面后的入口在管理控制台侧边栏左下角）。
 */
function LanguageCorner({
  language,
  onLanguageChange
}: {
  language: Language
  onLanguageChange: (language: Language) => void
}): JSX.Element {
  return (
    <div style={{ position: 'fixed', left: 16, bottom: 16 }}>
      <LanguageSwitcher compact value={language} onChange={onLanguageChange} />
    </div>
  )
}
