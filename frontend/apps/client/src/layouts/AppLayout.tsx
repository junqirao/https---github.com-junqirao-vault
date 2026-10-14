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
  FileSearchOutlined,
  FileTextOutlined,
  HddOutlined,
  InfoCircleOutlined,
  ReloadOutlined,
  SettingOutlined,
  TeamOutlined,
  UserOutlined
} from '@ant-design/icons'
import { Alert, Avatar, Button, Dropdown, Layout, Menu, Popover, Space, Tag, Tooltip, Typography } from 'antd'
import type { MenuProps } from 'antd'
import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { Outlet, useLocation, useNavigate } from 'react-router-dom'

// 侧边栏品牌处展示的版本号取自 package.json（构建期注入），与发布包版本同源。
import { CLIENT_VERSION } from '../config'
// 与本地条目比对服务端地址用同一口径：代理回传的 url 与本地 baseUrl 可能差一个尾斜杠。
import { normalizeUrl } from '../store/appStore'

import {
  LanguageSwitcher,
  LayoutModeProvider,
  LoadingState,
  SystemDepsBanner,
  layout,
  palette,
  roleLabel,
  spacing,
  translateError,
  useAgent,
  useAgentEventNotifier,
  useAgentVersion,
  useApi,
  useAuth,
  useI18n,
  useServerConfig,
  useServerLatency,
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

/**
 * 切换服务端后、整页重载前的等待（毫秒）。
 *
 * activeKey 的持久化走的是异步 IPC（Electron 下写 %APPDATA%/Vault/config.json），
 * 立刻 `location.reload()` 会读回**旧**的服务端，表现为"切了等于没切"。
 * 300ms 足够一次本地写盘落地，用户也感知不到延迟。
 */
const SERVER_SWITCH_RELOAD_DELAY_MS = 300

/** 主界面布局：按 mode 渲染应用端或管理端两套菜单，顶栏元素两模式一致。 */
export function AppLayout({ mode, language, onLanguageChange, onManageServers, autoMount }: AppLayoutProps): JSX.Element {
  const { t } = useI18n()
  const navigate = useNavigate()
  // 代理事件的全局通知出口：整个应用只注册一次（放外壳里）。
  // 之前每个 useAgent 组件都注册一个，同一条事件会弹出 2-3 条重复提示。
  useAgentEventNotifier()
  const location = useLocation()
  const { user, isSuperAdmin, logout } = useAuth()
  const { servers, activeKey, active, setActive } = useServerConfig()
  const agent = useAgent()
  const agentVersion = useAgentVersion()
  // 服务端往返延迟（顶栏指标，每 10 秒自测一次）：见下面 latencyText / latencyColor。
  const latency = useServerLatency(active?.baseUrl)
  // 手动重连进行中（按钮转圈）：只影响按钮，不参与任何状态判定。
  const [serverRetrying, setServerRetrying] = useState(false)

  /**
   * 平台能力（匿名接口，管理端才查）：目前只用它判断「孤儿磁盘」这项菜单该不该出现。
   *
   * 孤儿磁盘 = 存储根 disks/ 下"未登记却真实存在"的 .vhdx，这个布局只对 Windows 成立；
   * Linux 上磁盘是 LVM thin LV（不在存储根目录里），扫描永远是空的。未取到平台信息时
   * 按"不支持"处理——宁可在 Windows 上晚一拍出现，也不要在 Linux 上先闪出再消失。
   */
  const api = useApi()
  const infoQuery = useQuery({
    queryKey: ['system-info'],
    queryFn: () => api.systemInfo(),
    enabled: mode === 'admin'
  })
  const orphansSupported = infoQuery.data?.capabilities.platform_kind === 'windows'

  const appMenuItems: MenuProps['items'] = [
    { key: '/my-repos', icon: <CloudOutlined />, label: t('nav.myRepos') },
    { key: '/logs', icon: <FileTextOutlined />, label: t('nav.logs') },
    { key: '/settings', icon: <SettingOutlined />, label: t('nav.settings') },
    // 「关于」紧贴「配置」之下：两者都是"这台客户端自己"的事（配置改行为，关于看版本与许可）。
    // 被问"你那边是什么版本"时用户第一眼会往侧边栏找，排在配置下面最顺手。
    { key: '/about', icon: <InfoCircleOutlined />, label: t('nav.about') }
  ]

  // 紧挨「存储」：孤儿磁盘就是存储根里"未登记却真实存在"的 VHDX，属于存储侧清理入口；
  // 非 Windows 服务端不显示（见 orphansSupported 的说明）。
  const orphansMenuItem: MenuProps['items'] = orphansSupported
    ? [{ key: '/admin/orphans', icon: <FileSearchOutlined />, label: t('nav.orphans') }]
    : []

  const adminMenuItems: MenuProps['items'] = [
    { key: '/admin/users', icon: <TeamOutlined />, label: t('nav.users') },
    { key: '/admin/repos', icon: <AppstoreOutlined />, label: t('nav.repos') },
    { key: '/admin/storages', icon: <DatabaseOutlined />, label: t('nav.storages') },
    ...orphansMenuItem,
    { key: '/admin/leases', icon: <HddOutlined />, label: t('nav.leases') },
    { key: '/admin/jobs', icon: <DashboardOutlined />, label: t('nav.jobs') },
    { key: '/admin/audit', icon: <AuditOutlined />, label: t('nav.audit') },
    { key: '/admin/logs', icon: <FileTextOutlined />, label: t('nav.serverLogs') },
    { key: '/admin/system', icon: <ClusterOutlined />, label: t('nav.system') },
    // 管理端也留一个：关于里的版本/许可是登录角色无关的，排障时不该因为"当前在管理端"就找不到。
    // 路径带 /admin 前缀：管理外壳的路由是挂在 /admin 下的，写 '/about' 会跳到应用端那棵树上
    // （菜单高亮与外壳模式都会跟着不对）。
    { key: '/admin/about', icon: <InfoCircleOutlined />, label: t('nav.about') }
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
      if (key === '__manage__') {
        onManageServers()
        return
      }
      // 点当前这台不用刷新（否则等于无故重载一次）。
      if (key === activeKey) return
      setActive(key)
      // 换服务端后整页重载：hash 路由保留（仍停在当前页面），但 React Query 缓存与页面内部
      // 状态全部重建。只改 activeKey 是不够的——查询缓存键里没有服务端标识，
      // 列表会短暂显示上一台服务端的数据，且已打开的表单还握着旧对象的 ID。
      window.setTimeout(() => window.location.reload(), SERVER_SWITCH_RELOAD_DELAY_MS)
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

  // 服务端连接阶段：phase 是代理侧的权威取值（connecting / connected / disconnected），
  // 但**不能让 phase 覆盖掉 connected** —— 旧版代理不带 phase，而心跳恢复连接时会先
  // 把 connected 置真；这里以 connected 为准做一次归一。
  //
  // 取的是**当前活动服务端**那一台：多服务端下每台各发各的 server 事件，compat `server`
  // 只指主服务端，直接用会出现"切到 B 了，顶栏还在看 A 连没连上"。按活动键取不到时
  // 退回 compat（旧版代理 / 客户端刚启动还没拿到 servers[]）。
  //
  // 键对不上时按地址兜底：代理会把"地址键"合并成"实例 ID 键"（反之亦然），本地条目可能还停在
  // 另一种写法上（老配置迁移过来的条目尤其如此），只按 key 找会退回主服务端那一份。
  const server =
    (activeKey
      ? (agent.state?.servers?.find((item) => item.server_key === activeKey) ??
        (active?.baseUrl
          ? agent.state?.servers?.find((item) => normalizeUrl(item.url) === normalizeUrl(active.baseUrl))
          : undefined))
      : undefined) ?? agent.state?.server
  // 排障面板逐台列出：新代理给 servers[]，旧代理退回兼容的 health.server（只有一台）。
  const serverList = agent.state?.servers?.length ? agent.state.servers : agent.health?.servers ?? []
  const serverPhase = server?.connected ? 'connected' : server?.phase
  // "连接中"：代理正在连/自动重连。它不再出现在顶栏（顶栏"通没通"由延迟指标说明），
  // 只留在两处判定里：管理端不在此时遮罩（瞬时抖动不该盖死内容区），以及下面的"未连接"。
  const serverConnecting = agent.available && serverPhase === 'connecting'
  // "未连接"只在代理**明确**说"自动重连已放弃"（phase=disconnected）时才算数：这样旧版代理
  // （不带 phase）不会被误判成失败，也就不会挂出一个注定 404 的"重试"按钮。
  const serverExhausted = agent.available && serverPhase === 'disconnected'

  /**
   * 管理端在**确认连不上**服务端时遮罩管理页内容区（顶栏与侧边栏不遮，见下面 render 里遮罩的位置）。
   *
   * 理由：管理端的每个动作（列表、删除、建池…）都要打服务端，断开时点了必然失败；
   * 让用户对着一个注定报错的界面点，只会换回一串网络错误。
   *
   * 为什么**不把"连接中"算进来**：连接中大多是瞬时的（事件流重连、心跳抖动的退避重试），而管理端
   * 的请求是渲染进程**直连 baseUrl** 的，跟代理这条链路的瞬时抖动没关系。为一次瞬时抖动把内容区
   * 盖死，用户读到的是"点一下删除就卡死了"（真实反馈："点删除库会卡在连接中"）。
   *
   * 为什么带 agent.available 前置条件：代理没起来时 server 状态是"未知"（快照为空）而不是
   * "未连接"，而服务端 API 是渲染进程直连 baseUrl 的、可能完全正常——不加这个前提，
   * 代理一异常就会把界面凭空锁死。
   */
  const adminBlocked = mode === 'admin' && agent.available && !server?.connected && !serverConnecting

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
  /**
   * 顶栏延迟指标：数值本身就是状态。
   *
   * 顶栏不再挂"已连接/未连接"标签 —— 那句话既占位置，又说不清"到底有多快"；一个带颜色的
   * 毫秒数把"通没通"和"有多快"合成一件事：测到数字就是通了（绿/黄/红按快慢），
   * 测不到（超时/网络错/非 2xx）就是没通，显示红色的 `- ms`。
   */
  const latencyText = t('agent.latency.value', {
    value: latency.down ? '-' : latency.latencyMs === undefined ? '…' : latency.latencyMs
  })
  // 阈值取"人眼看不出 vs 明显发钝"的分界：100ms 以内算流畅，300ms 以上算迟钝。
  const latencyColor = latency.down
    ? palette.danger
    : latency.latencyMs === undefined
      ? palette.textTertiary
      : latency.latencyMs < 100
        ? palette.success
        : latency.latencyMs < 300
          ? palette.warning
          : palette.danger

  /**
   * 服务器选择栏的悬停提示：实例 ID + 服务地址 + 指标口径。
   *
   * 前两者都是"核对这台是哪台"的稳定标识，但都长（ID 是 srv-<uuid>）：常驻在顶栏上只占地方，
   * 收进悬停提示里，要核对时一悬停就能看到完整值（顶栏上只留能一眼认出的名字）。
   */
  const serverTip = (
    <span style={{ display: 'block' }}>
      <span style={{ fontFamily: 'ui-monospace, SFMono-Regular, Consolas, monospace' }}>
        {active?.instanceId || '-'}
      </span>
      <br />
      <span>{active?.baseUrl || '-'}</span>
      <br />
      <span style={{ opacity: 0.75 }}>{t('agent.latency.hint')}</span>
    </span>
  )

  // 手动重连：代理侧已不再自动重试（连续失败达上限），只能由用户点这里再试一次；
  // 重试会清零失败计数，代理随后重新获得自动重连机会。
  const handleServerRetry = (): void => {
    setServerRetrying(true)
    void (async () => {
      try {
        // 只重连活动那一台：代理可能同时连着好几台，替别的台重试没有意义。
        await agent.reconnectServer(activeKey ?? undefined)
      } catch {
        // 失败原因已由 useAgent 落进 agent.error（排障面板可见），这里只负责恢复按钮。
      } finally {
        setServerRetrying(false)
      }
    })()
  }

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
          {serverList.length > 0
            ? serverList.map((item) => (
                <Space direction="vertical" size={0} key={item.server_key ?? item.url}>
                  <Typography.Text>{`${t('agent.server.url')}: ${item.url || '-'}`}</Typography.Text>
                  <Typography.Text>{`${t('agent.server.name')}: ${item.name || '-'}`}</Typography.Text>
                  <Typography.Text>
                    {`${t('agent.server.connected')}: ${item.connected ? t('common.yes') : t('common.no')}`}
                  </Typography.Text>
                  {item.last_error ? (
                    <Typography.Text type="warning">
                      {`${t('agent.server.lastError')}: ${item.last_error}`}
                    </Typography.Text>
                  ) : null}
                </Space>
              ))
            : (
                <>
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
              )}
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
                // 每一项都是路由（包括「关于」）：不再有"点了开弹窗、不留高亮"的特例。
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
                  {/* 悬停提示的锚点必须是 DOM 节点（Tooltip 拿它做悬停判定与定位），
                      所以里面用 span 而不是 Space；按钮本身的点击（开服务端下拉）不受影响。 */}
                  <Tooltip title={serverTip} placement="bottomLeft" mouseEnterDelay={0.3}>
                    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
                      <span>{active?.serverName || active?.baseUrl || t('settings.server.noName')}</span>
                      {/* 延迟指标：数字即状态，同时兼作排障面板的入口 —— 顶栏不再为它单独占一个图标位。
                          点它要拦掉冒泡，否则服务端下拉会跟着一起弹出来。
                          刷新按钮跟着面板标题走：面板内容会被"最近一次挂载失败"原文撑长，
                          按钮放底部既难找、又会被内容推走；无障碍语义由 aria-label 兜住。
                          等宽数字：免得每 10 秒刷新时整条顶栏左右微跳。 */}
                      <Popover
                        content={agentDetail}
                        trigger="click"
                        placement="bottomLeft"
                        title={
                          <Space size={spacing.xs}>
                            <span>{t('agent.troubleshoot.title')}</span>
                            <Button
                              type="text"
                              size="small"
                              icon={<ReloadOutlined />}
                              title={t('common.refresh')}
                              aria-label={t('common.refresh')}
                              onClick={() => void agent.refresh()}
                            />
                          </Space>
                        }
                      >
                        <span
                          role="button"
                          tabIndex={0}
                          aria-label={t('agent.troubleshoot.title')}
                          onClick={(event) => event.stopPropagation()}
                          onKeyDown={(event) => {
                            if (event.key !== 'Enter' && event.key !== ' ') return
                            // 空格会滚页面、回车会"点"外层的按钮：都拦掉，只手动补一次 click。
                            event.preventDefault()
                            event.stopPropagation()
                            event.currentTarget.click()
                          }}
                          style={{
                            color: latencyColor,
                            cursor: 'pointer',
                            fontVariantNumeric: 'tabular-nums'
                          }}
                        >
                          {latencyText}
                        </span>
                      </Popover>
                      <DownOutlined style={{ fontSize: 10 }} />
                    </span>
                  </Tooltip>
                </Button>
              </Dropdown>
              {serverExhausted ? (
                <Button size="small" type="link" loading={serverRetrying} onClick={handleServerRetry}>
                  {t('agent.server.retry')}
                </Button>
              ) : null}
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

          {/* 内容区容器：连接遮罩只盖这一块（见 adminBlocked），顶栏与侧边栏不盖。
              理由：顶栏上有"服务端切换 / 延迟指标 / 重试 / 退出登录"，全屏遮住之后一旦卡在
              "连接中"，用户连退出登录都点不到，只能去杀进程。 */}
          <div style={{ position: 'relative', display: 'flex', flex: 1, minHeight: 0 }}>
            <Layout.Content style={{ flex: 1, minWidth: 0, background: palette.bgPage, overflow: 'auto' }}>
              {/* 服务端系统依赖缺失（缺 configfs 挂载 / LIO 内核模块 / targetcli 等）：
                  启动时能自动修的已修，修不了的在这里告诉使用者"缺了什么、怎么补"。
                  能装包的项再给个"安装"按钮——但那个接口只有超级管理员能调，
                  非管理员渲染出来只会点了拿 403，所以按角色 gate。 */}
              <SystemDepsBanner canInstall={isSuperAdmin} />
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
            {adminBlocked ? (
              <div
                style={{
                  position: 'absolute',
                  inset: 0,
                  // 必须高过 antd 的浮层（Modal/message/notification 都在 1000~1050）：
                  // 断开时若正好开着某个弹窗，它的 portal 挂在 body 末尾、会浮在低 z-index 的遮罩之上，
                  // 用户就能继续点一个后端已经不可达的表单。
                  zIndex: 1200,
                  display: 'flex',
                  alignItems: 'center',
                  justifyContent: 'center',
                  background: palette.bgPage
                }}
              >
                <LoadingState text={t('agent.gate.hint')} />
              </div>
            ) : null}
          </div>
        </Layout>
      </Layout>
    </LayoutModeProvider>
  )
}
