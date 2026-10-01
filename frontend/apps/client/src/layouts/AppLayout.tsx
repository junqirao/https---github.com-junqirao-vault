import {
  AppstoreOutlined,
  AuditOutlined,
  CloudOutlined,
  ClusterOutlined,
  ControlOutlined,
  DashboardOutlined,
  DatabaseOutlined,
  DesktopOutlined,
  DownOutlined,
  FileTextOutlined,
  FolderOutlined,
  HddOutlined,
  SettingOutlined,
  TeamOutlined,
  UserOutlined
} from '@ant-design/icons'
import { Alert, Avatar, Button, Dropdown, Layout, Menu, Popover, Space, Tag, Typography } from 'antd'
import type { MenuProps } from 'antd'
import { Outlet, useLocation, useNavigate } from 'react-router-dom'

// 侧边栏品牌处展示的版本号取自 package.json（构建期注入），与发布包版本同源。
import { CLIENT_VERSION } from '../config'

import {
  LanguageSwitcher,
  LayoutModeProvider,
  layout,
  palette,
  roleLabel,
  spacing,
  translateError,
  useAgent,
  useAgentVersion,
  useAuth,
  useI18n,
  useServerConfig,
  type Language
} from '@vault/ui'

/** 外壳模式：应用端（面向普通用户）或管理端（仅超级管理员可达）。 */
export type AppShellMode = 'app' | 'admin'

export interface AppLayoutProps {
  mode: AppShellMode
  language: Language
  onLanguageChange: (language: Language) => void
  onManageServers: () => void
  /** 自动挂载进度（由本地代理执行，前端只展示）。 */
  autoMount?: { total: number; pending: number } | null
}

/** 主界面布局：按 mode 渲染应用端或管理端两套菜单，顶栏元素两模式一致。 */
export function AppLayout({ mode, language, onLanguageChange, onManageServers, autoMount }: AppLayoutProps): JSX.Element {
  const { t } = useI18n()
  const navigate = useNavigate()
  const location = useLocation()
  const { user, isSuperAdmin, logout } = useAuth()
  const { servers, activeKey, active, setActive } = useServerConfig()
  const agent = useAgent()
  const agentVersion = useAgentVersion()

  const appMenuItems: MenuProps['items'] = [
    { key: '/my-repos', icon: <CloudOutlined />, label: t('nav.myRepos') },
    { key: '/mounts', icon: <FolderOutlined />, label: t('nav.mounts') },
    { key: '/logs', icon: <FileTextOutlined />, label: t('nav.logs') },
    { key: '/settings', icon: <SettingOutlined />, label: t('nav.settings') }
  ]

  const adminMenuItems: MenuProps['items'] = [
    { key: '/admin/users', icon: <TeamOutlined />, label: t('nav.users') },
    { key: '/admin/repos', icon: <AppstoreOutlined />, label: t('nav.repos') },
    { key: '/admin/storages', icon: <DatabaseOutlined />, label: t('nav.storages') },
    { key: '/admin/leases', icon: <HddOutlined />, label: t('nav.leases') },
    { key: '/admin/jobs', icon: <DashboardOutlined />, label: t('nav.jobs') },
    { key: '/admin/audit', icon: <AuditOutlined />, label: t('nav.audit') },
    { key: '/admin/logs', icon: <FileTextOutlined />, label: t('nav.logs') },
    { key: '/admin/system', icon: <ClusterOutlined />, label: t('nav.system') }
  ]

  const menuItems = mode === 'admin' ? adminMenuItems : appMenuItems

  const serverMenu: MenuProps = {
    items: [
      ...servers.map((server) => ({
        key: server.key,
        label: server.serverName || server.baseUrl
      })),
      { type: 'divider' as const },
      { key: '__manage__', label: t('boot.changeAddress') }
    ],
    selectable: true,
    selectedKeys: activeKey ? [activeKey] : [],
    onClick: ({ key }) => {
      if (key === '__manage__') onManageServers()
      else setActive(key)
    }
  }

  const userMenu: MenuProps = {
    items: [
      { key: 'identity', label: `${user?.username ?? '-'} · ${roleLabel(user?.role ?? 'user')}`, disabled: true },
      { type: 'divider' as const },
      { key: 'logout', label: t('nav.logout') }
    ],
    onClick: ({ key }) => {
      if (key === 'logout') logout()
    }
  }

  const selectedKey =
    mode === 'admin'
      ? location.pathname
      : location.pathname.startsWith('/repos') || location.pathname.startsWith('/my-repos')
        ? '/my-repos'
        : location.pathname

  const agentTone = !agent.available
    ? 'default'
    : agent.state?.server.connected
      ? 'green'
      : 'orange'

  // 本机 iSCSI 发起程序未就绪：挂载必定失败在 connect 阶段，而错误里没有可执行信息。
  // 启动时就挂横幅提示（代理探测到状态变化会推 host 事件，横幅随之出现/消失，无需重启）。
  //
  // checked_at === 0 表示**代理还没探测完**：此时 reason 为空，若直接按"未就绪"提示，
  // 会先闪一条原因不明的警告再变成真实原因（探测要跑一次 PowerShell，约 1s）。
  const hostState = agent.state?.host
  const hostWarning = agent.available && hostState && hostState.checked_at !== 0 && !hostState.iscsi_ready ? hostState : undefined
  const hostDescKey =
    hostWarning?.reason === 'module_missing'
      ? 'agent.host.module_missing'
      : hostWarning?.reason === 'probe_failed'
        ? 'agent.host.probe_failed'
        : 'agent.host.service_stopped'
  const agentText = !agent.available
    ? t('agent.state.notFound')
    : agent.state?.server.connected
      ? t('agent.state.connected')
      : t('agent.state.disconnected')

  // 最近一次挂载失败的**原始报错**（代理侧 last_error_detail）。
  //
  // 为什么在这里露出来：按项目约定，界面上平时只能看到稳定错误码（如 agent.mount_failed），
  // 用户完全无从知道"为什么"（真实工单："客户端报错至少要知道为什么"）。它只存在挂载状态里，
  // 不进任何 API 错误响应，这里如实展示并支持一键复制（便于贴给运维/开发）。
  const lastMountFailure = (() => {
    const failed = (agent.state?.mounts ?? []).filter(
      (item) => item.state === 'error' && (item.last_error_detail || item.last_error)
    )
    return failed.length > 0 ? failed[failed.length - 1] : undefined
  })()

  const agentDetail = (
    <Space direction="vertical" size={4} style={{ maxWidth: 360 }}>
      {!agent.available ? <Typography.Text type="warning">{t('agent.hostMissing')}</Typography.Text> : null}
      {agent.health ? (
        <>
          <Typography.Text>{`${t('agent.version')}: ${agent.health.agent_version}`}</Typography.Text>
          <Typography.Text>{`${t('agent.platform')}: ${agent.health.platform}`}</Typography.Text>
          <Typography.Text>
            {`${t('agent.admin')}: ${agent.health.admin ? t('agent.admin.yes') : t('agent.admin.no')}`}
          </Typography.Text>
          <Typography.Text>{`${t('agent.server.url')}: ${agent.health.server.url || '-'}`}</Typography.Text>
          <Typography.Text>{`${t('agent.server.name')}: ${agent.health.server.name || '-'}`}</Typography.Text>
          <Typography.Text>
            {`${t('agent.server.connected')}: ${agent.health.server.connected ? t('common.yes') : t('common.no')}`}
          </Typography.Text>
          {agent.health.server.last_error ? (
            <Typography.Text type="warning">
              {`${t('agent.server.lastError')}: ${agent.health.server.last_error}`}
            </Typography.Text>
          ) : null}
        </>
      ) : null}
      {lastMountFailure ? (
        <Typography.Text
          type="warning"
          copyable={{
            text: [
              lastMountFailure.repo_name || lastMountFailure.repo_id,
              lastMountFailure.last_error || '',
              lastMountFailure.last_error_detail || ''
            ]
              .filter((part) => part !== '')
              .join('\n'),
            tooltips: [t('common.copy'), t('common.copied')]
          }}
        >
          {`${t('agent.lastMountError')}（${lastMountFailure.repo_name || lastMountFailure.repo_id}）：${
            lastMountFailure.last_error_detail || lastMountFailure.last_error || ''
          }`}
        </Typography.Text>
      ) : null}
      {agent.error ? <Typography.Text type="danger">{translateError(agent.error)}</Typography.Text> : null}
      {agentVersion.mismatch ? (
        <Typography.Text type="warning">
          {t('agent.versionMismatch.detail', {
            agent: agentVersion.agent ?? '-',
            client: agentVersion.expected || '-'
          })}
        </Typography.Text>
      ) : null}
      <Button size="small" onClick={() => void agent.refresh()}>
        {t('common.refresh')}
      </Button>
    </Space>
  )

  return (
    <LayoutModeProvider mode={mode}>
      <Layout style={{ height: '100vh' }}>
        <Layout.Sider width={layout.sidebarWidth} theme="light" style={{ borderRight: `1px solid ${palette.border}` }}>
          <div style={{ display: 'flex', flexDirection: 'column', height: '100%' }}>
            <div style={{ padding: `${spacing.md}px ${spacing.lg}px ${spacing.sm}px` }}>
              <Space direction="vertical" size={0}>
                <Space align="baseline" size={6}>
                  <Typography.Text strong style={{ fontSize: 16 }}>
                    {t('app.name')}
                  </Typography.Text>
                  <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                    v{CLIENT_VERSION}
                  </Typography.Text>
                </Space>
                {mode === 'admin' ? (
                  <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                    {t('nav.admin')}
                  </Typography.Text>
                ) : null}
              </Space>
            </div>
            <div style={{ flex: 1, minHeight: 0, overflow: 'auto' }}>
              <Menu
                mode="inline"
                selectedKeys={[selectedKey]}
                items={menuItems}
                style={{ borderInlineEnd: 'none' }}
                onClick={({ key }) => navigate(key)}
              />
            </div>
            {mode === 'admin' ? (
              <div style={{ padding: spacing.xs, borderTop: `1px solid ${palette.border}` }}>
                <LanguageSwitcher compact value={language} onChange={onLanguageChange} />
              </div>
            ) : null}
          </div>
        </Layout.Sider>

        <Layout>
          <Layout.Header
            style={{
              display: 'flex',
              alignItems: 'center',
              justifyContent: 'space-between',
              borderBottom: `1px solid ${palette.border}`,
              background: palette.bgContainer
            }}
          >
            <Space size={spacing.xs}>
              <Dropdown menu={serverMenu} trigger={['click']}>
                <Button type="text">
                  <Space size={6}>
                    <ClusterOutlined />
                    <span>{active?.serverName || active?.baseUrl || t('settings.server.noName')}</span>
                    <DownOutlined style={{ fontSize: 10 }} />
                  </Space>
                </Button>
              </Dropdown>
              {active?.instanceId ? <Tag>{active.instanceId.slice(0, 8)}</Tag> : null}
              <Popover content={agentDetail} title={t('agent.troubleshoot.title')} trigger="click" placement="bottomLeft">
                <Tag color={agentTone} style={{ cursor: 'pointer' }}>
                  {agentText}
                </Tag>
              </Popover>
              {agentVersion.mismatch ? (
                <Tag color={palette.warning} style={{ cursor: 'pointer' }}>
                  {t('agent.versionMismatch')}
                </Tag>
              ) : null}
            </Space>

            <Space size={spacing.xs}>
              {isSuperAdmin ? (
                <Button
                  size="small"
                  type="text"
                  icon={mode === 'admin' ? <DesktopOutlined /> : <ControlOutlined />}
                  onClick={() => navigate(mode === 'admin' ? '/my-repos' : '/admin/users')}
                >
                  {mode === 'admin' ? t('nav.switchToApp') : t('nav.switchToAdmin')}
                </Button>
              ) : null}
              <Dropdown menu={userMenu} trigger={['click']}>
                <Button type="text">
                  <Space size={6}>
                    <Avatar size="small" icon={<UserOutlined />} />
                    <span>{user?.username ?? '-'}</span>
                  </Space>
                </Button>
              </Dropdown>
            </Space>
          </Layout.Header>

          <Layout.Content style={{ background: palette.bgPage, overflow: 'auto' }}>
            {hostWarning ? (
              <Alert
                type="warning"
                showIcon
                style={{ margin: `${spacing.md}px ${spacing.md}px 0` }}
                message={t('agent.host.title')}
                description={t(hostDescKey, { error: hostWarning.error ?? '' })}
              />
            ) : null}
            {autoMount ? (
              <Alert
                type="info"
                showIcon
                style={{ margin: `${spacing.md}px ${spacing.md}px 0` }}
                message={t('agent.autoMount.progress', { pending: autoMount.pending, total: autoMount.total })}
              />
            ) : null}
            <Outlet />
          </Layout.Content>
        </Layout>
      </Layout>
    </LayoutModeProvider>
  )
}
