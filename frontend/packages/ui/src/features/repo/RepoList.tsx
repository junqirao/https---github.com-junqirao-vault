import { useCallback, useEffect, useMemo, useRef, useState, useSyncExternalStore } from 'react'
import type { ReactNode } from 'react'
import {
  Button,
  Card,
  Col,
  Input,
  Pagination,
  Progress,
  Row,
  Segmented,
  Skeleton,
  Space,
  Tooltip,
  Typography,
  message
} from 'antd'
import type { TableColumnsType } from 'antd'
import { AppstoreOutlined, ArrowLeftOutlined, BarsOutlined, FolderOutlined, SettingOutlined } from '@ant-design/icons'
import { useQuery, useQueryClient } from '@tanstack/react-query'

import { DataTable } from '../../components/DataTable'
import { EmptyState } from '../../components/EmptyState'
import { ErrorNotice } from '../../components/ErrorNotice'
import { PageShell } from '../../components/PageShell'
import { StatusTag } from '../../components/StatusTag'
import { usageColor } from '../../components/UsageBar'
import { useApi } from '../../api/provider'
import type { AgentMountState, UploadState } from '../../api/agentTypes'
import type { RepoDTO } from '../../api/types'
import { isActiveUpload, pickMountUsage, useAgent, useAgentUsagePolling } from '../../hooks/useAgent'
import { useAuth } from '../../hooks/useAuth'
import { useServerConfig } from '../../hooks/useServerConfig'
import { useI18n } from '../../i18n'
import { fontSize, palette, spacing } from '../../tokens/palette'
import { formatBytes, formatTime, repoUsage } from '../../utils/format'
import { repoModeLabel } from '../../utils/labels'
import { RepoCard, RepoRow } from './RepoCards'
import { GroupMountSettingsModal, RepoMountSettingsModal } from './RepoMountSettings'
import { isPlaceholderRepoId, uploadPlaceholderRepo } from './uploadView'

export interface RepoListProps {
  isSuperAdmin: boolean
  onCreate: () => void
  onOpen: (repoId: string) => void
  /**
   * 打开「上传任务」页：进行中的上传就以库的身份摆在列表里（见 RepoListEntry），
   * 点它进这个页面看进度 / 取消。
   *
   * 可选：管理端看的是服务端全量库，本机上传与它无关，不传即不出现上传中的条目。
   */
  onOpenUpload?: (uploadId: string) => void
  /**
   * 打开「导入母盘」页：把存储根里已存在的 VHDX 文件原地接管成存储库。
   *
   * 可选，且只在管理端出现：这是换服务端时的迁移入口（见 DiskImport），
   * 用户端没有存储根的概念，放它只会让人误以为能"导入别人的盘"。
   */
  onImport?: () => void
}

/**
 * 列表里的一行：一个存储库，外加它此刻的上传进度（正在上传时才有）。
 *
 * 上传中的库**不另起一块区域**，就和其他库并排躺在同一个分组里，用同一套卡片零件渲染 ——
 * 只有状态不同（真实反馈："就放在存储库那里展示，和其他存储库一样的样式，只是状态不同"）。
 */
interface RepoListEntry {
  repo: RepoDTO
  upload?: UploadState
}

/** 用户端展示形式：卡片网格（默认，一眼扫过多块盘）/ 整页宽度的列表（一库一行，便于横向比对）。 */
type RepoViewMode = 'card' | 'list'

/**
 * 每页条数按形态分别取值：卡片一行 3~4 张（每页 8 张 = 两行上下），列表一行一库（每页 10 行）。
 * 两者都压在一屏上下，分页才有意义 —— 一页要滚三屏的话，用户只会直接找搜索框。
 */
const PAGE_SIZE: Record<RepoViewMode, number> = { card: 8, list: 10 }

/**
 * 展示形式的本地偏好键。
 *
 * 与客户端配置（`vault.client.config`，走 Electron IPC 写 config.json）**分开**：展示形式是纯界面偏好，
 * 只跟"这台机器的这个界面"有关，不值得为它多铺一条跨进程写盘路径；localStorage 在浏览器 dev 与
 * Electron 渲染进程两边都在，刷新/重开都还在。
 */
const VIEW_PREF_KEY = 'vault.repo.view'

/**
 * 当前所在分组的会话记忆键。
 *
 * 用户从分组里点进存储库详情后，RepoList 会被卸载；按「返回」回来时组件重建，
 * 组件内的 activeGroup 已归零，于是回到外面的大列表而不是原来那个分组。
 * 用 sessionStorage 记住"这次会话最后进入的分组"：返回时能落回分组，关掉客户端后
 * 自然清空（下次进来仍从总览开始）。空串是一个合法分组（未分组），null = 总览。
 */
const GROUP_PREF_KEY = 'vault.repo.group'

function readStoredGroup(): string | null {
  try {
    const raw = window.sessionStorage.getItem(GROUP_PREF_KEY)
    return raw === null ? null : raw
  } catch {
    return null
  }
}

function storeGroup(group: string | null): void {
  try {
    if (group === null) window.sessionStorage.removeItem(GROUP_PREF_KEY)
    else window.sessionStorage.setItem(GROUP_PREF_KEY, group)
  } catch {
    // 会话记忆失败不影响使用：返回时落到总览而已。
  }
}

/** 读取本地记住的展示形式：读不到、值不合法、存储不可用，一律退回默认的卡片。 */
function readStoredView(): RepoViewMode {
  try {
    const raw = window.localStorage.getItem(VIEW_PREF_KEY)
    return raw === 'list' || raw === 'card' ? raw : 'card'
  } catch {
    return 'card'
  }
}

/** 记住展示形式：localStorage 可能不可用（隐私模式、配额写满），写不进去就当没记住，绝不影响界面。 */
function storeView(view: RepoViewMode): void {
  try {
    window.localStorage.setItem(VIEW_PREF_KEY, view)
  } catch {
    // 忽略：偏好没记住不是错误，不值得打扰用户。
  }
}

/**
 * 「中间态」：库会**自己往下走**、不需要人管的状态（建库中 / 写母盘中 / 加封中 / 删除中）。
 *
 * error 刻意不算：它不会自己好，等人动手修的状态盯着轮询只是白耗请求。
 */
const PENDING_REPO_STATES: ReadonlyArray<RepoDTO['state']> = [
  'creating',
  'importing',
  'sealing',
  'deleting'
]

/**
 * 列表里还有中间态时的自动刷新间隔。
 *
 * 5s 是"标签该消失时基本立刻就消失"与"别把列表当心跳打"之间的取中：这段窗口本身是分钟级的
 * （写母盘 30G 几十分钟起步），刷新只是去接一个迟早会发生的状态变化。
 */
const PENDING_REFRESH_MS = 5000

/**
 * 用户端分组区块标题：标题 + 数量在左，`extra`（形态切换）贴右。
 *
 * 切换控件放在内容区第一个分组的标题行右侧，而不是页面头部工具栏：它只作用于这一块列表，
 * 与搜索/刷新/新建（页面级操作）不是一类东西；标题为空时（拿不到当前用户的分组兜底）只渲染右侧控件。
 */
function RepoGroupHeader({
  title,
  count,
  extra
}: {
  title: string
  count: number
  extra?: ReactNode
}): JSX.Element {
  return (
    <div
      style={{
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'space-between',
        gap: spacing.sm,
        marginBottom: spacing.sm
      }}
    >
      {title ? (
        <Space align="baseline" size={spacing.xs}>
          <Typography.Text strong>{title}</Typography.Text>
          <Typography.Text type="secondary" style={{ fontSize: fontSize.sm }}>
            {count}
          </Typography.Text>
        </Space>
      ) : (
        <span />
      )}
      {extra}
    </div>
  )
}

/**
 * 配额总览：把**用户级配额**摊在列表最上面。
 *
 * 为什么需要单独一块：配额是用户级的一份（名下所有库共用），而卡片上的进度条分母是
 * 各库自己的容量 —— 用户此前只能逐个库估算，"我总共用了多少、还剩多少"没有任何地方
 * 能回答（/v1/users/{id} 只对超管开放）。这里是「我的存储库」页面上唯一的总账。
 *
 * 取不到时整块不渲染：老服务端没有 /v1/auth/me，或请求失败时，页面照常用，
 * 不能因为一块统计把整个列表带崩。
 */
function QuotaOverview({ repos }: { repos: RepoDTO[] }): JSX.Element | null {
  const api = useApi()
  const { t } = useI18n()
  const meQuery = useQuery({ queryKey: ['me'], queryFn: () => api.me() })
  const user = meQuery.data
  if (!user) return null

  // quota_bytes = 0 表示不限（只受存储池容量约束），此时没有分母，不画进度条。
  const unlimited = user.quota_bytes <= 0
  const percent = unlimited ? 0 : Math.min(100, Math.round((user.used_bytes / user.quota_bytes) * 100))
  const creating = repos.filter((repo) => repo.state === 'creating').length

  return (
    <Card styles={{ body: { padding: spacing.md } }}>
      <div style={{ display: 'flex', alignItems: 'baseline', gap: spacing.xs, flexWrap: 'wrap' }}>
        <Typography.Text strong>{t('repo.quota.title')}</Typography.Text>
        <Typography.Text style={{ marginLeft: 'auto', fontSize: fontSize.sm }}>
          {`${t('repo.quota.used')} ${formatBytes(user.used_bytes)} / ${t('repo.quota.total')} ${
            unlimited ? t('common.unlimited') : formatBytes(user.quota_bytes)
          }`}
        </Typography.Text>
        {/* 不限配额时没有分母，百分比无意义（进度条也不画）。 */}
        {unlimited ? null : (
          <Typography.Text strong style={{ fontSize: fontSize.sm }}>
            {`${percent}%`}
          </Typography.Text>
        )}
      </div>
      {unlimited ? null : (
        <Progress
          percent={percent}
          showInfo={false}
          size="small"
          // 与库卡片同一条分档规则（见 components/UsageBar 的 usageColor）：配额也是"占比"，
          // 同一页上两处条子一套颜色，免得一条橙一条蓝看着像两种口径。
          strokeColor={usageColor(percent)}
          trailColor={palette.neutralFill}
          style={{ marginTop: spacing.xxs, marginBottom: 0 }}
        />
      )}
      <Space size={spacing.md} wrap style={{ marginTop: spacing.sm }}>
        {unlimited ? null : (
          <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
            {t('repo.quota.remaining')} {formatBytes(Math.max(0, user.quota_bytes - user.used_bytes))}
          </Typography.Text>
        )}
        <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
          {t('repo.quota.repos')} {repos.length}
        </Typography.Text>
        {creating > 0 ? (
          <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
            {t('repo.quota.creating')} {creating}
          </Typography.Text>
        ) : null}
      </Space>
    </Card>
  )
}

/** 卡片 / 列表形态切换：只挂在分组标题行右侧（见 RepoGroupHeader 的 extra）。 */
function RepoViewSwitch({
  value,
  onChange
}: {
  value: RepoViewMode
  onChange: (next: RepoViewMode) => void
}): JSX.Element {
  const { t } = useI18n()
  return (
    <Segmented
      value={value}
      onChange={(next) => onChange(next as RepoViewMode)}
      options={[
        {
          value: 'card',
          label: (
            <Space size={spacing.xs}>
              <AppstoreOutlined />
              {t('repo.view.card')}
            </Space>
          )
        },
        {
          value: 'list',
          label: (
            <Space size={spacing.xs}>
              <BarsOutlined />
              {t('repo.view.list')}
            </Space>
          )
        }
      ]}
    />
  )
}

interface RepoSectionProps {
  title: string
  items: RepoListEntry[]
  viewMode: RepoViewMode
  currentUserId: string
  /** 标题行右侧的附加内容（形态切换）；只由列表页交给第一个分组。 */
  headerExtra?: ReactNode
  onOpen: (repoId: string) => void
  onConfigure: (repo: RepoDTO) => void
  onOpenUpload?: (uploadId: string) => void
}

/**
 * 一个分组区块：标题 + 当前页 + 分页。
 *
 * 分页**每个分组各自一份**：两个分组的含义不同（我拥有的 / 我参与的），
 * 合成一个跨组的分页器时，"第 2 页"读到什么全凭两组各有几个库，很难理解。
 */
function RepoSection({
  title,
  items,
  viewMode,
  currentUserId,
  headerExtra,
  onOpen,
  onConfigure,
  onOpenUpload
}: RepoSectionProps): JSX.Element {
  const [page, setPage] = useState(1)
  const pageSize = PAGE_SIZE[viewMode]
  // 切换形态会换掉每页条数（卡片 8 / 列表 10），当前页可能已经越界；
  // 回到第 1 页最不容易迷失（越界本身另有兜底，见下面的 current）。
  useEffect(() => setPage(1), [viewMode])

  const pageCount = Math.max(1, Math.ceil(items.length / pageSize))
  const current = Math.min(page, pageCount)
  const visible = items.slice((current - 1) * pageSize, current * pageSize)

  return (
    <div>
      {title || headerExtra ? <RepoGroupHeader title={title} count={items.length} extra={headerExtra} /> : null}
      {viewMode === 'card' ? (
        <Row gutter={[spacing.md, spacing.md]}>
          {visible.map(({ repo, upload }) => (
            <Col key={repo.id} xs={24} sm={12} xl={8} xxl={6}>
              <RepoCard
                repo={repo}
                upload={upload}
                onOpenUpload={onOpenUpload}
                currentUserId={currentUserId}
                onOpen={onOpen}
                onConfigure={onConfigure}
              />
            </Col>
          ))}
        </Row>
      ) : (
        <Space direction="vertical" size={spacing.sm} style={{ width: '100%' }}>
          {visible.map(({ repo, upload }) => (
            <RepoRow
              key={repo.id}
              repo={repo}
              upload={upload}
              onOpenUpload={onOpenUpload}
              currentUserId={currentUserId}
              onOpen={onOpen}
              onConfigure={onConfigure}
            />
          ))}
        </Space>
      )}
      {items.length > pageSize ? (
        <div style={{ display: 'flex', justifyContent: 'center', marginTop: spacing.md }}>
          <Pagination
            size="small"
            current={current}
            pageSize={pageSize}
            total={items.length}
            showSizeChanger={false}
            onChange={setPage}
          />
        </div>
      ) : null}
    </div>
  )
}

/** 加载骨架：按当前形态出骨架（卡片网格 / 列表行），避免加载完"跳一下"。 */
function RepoListSkeleton({ viewMode }: { viewMode: RepoViewMode }): JSX.Element {
  if (viewMode === 'card') {
    return (
      <Row gutter={[spacing.md, spacing.md]}>
        {Array.from({ length: 8 }).map((_, index) => (
          <Col key={index} xs={24} sm={12} xl={8} xxl={6}>
            <Card style={{ height: '100%' }} styles={{ body: { padding: spacing.md } }}>
              <Skeleton active paragraph={{ rows: 3 }} />
            </Card>
          </Col>
        ))}
      </Row>
    )
  }
  return (
    <Space direction="vertical" size={spacing.sm} style={{ width: '100%' }}>
      {Array.from({ length: 6 }).map((_, index) => (
        <Card key={index} styles={{ body: { padding: `${spacing.sm}px ${spacing.md}px` } }}>
          <Skeleton active paragraph={{ rows: 1 }} />
        </Card>
      ))}
    </Space>
  )
}

/** 把库按 meta.group 归类；空分组单独归为一组（未分组）。 */
function groupKeyOf(repo: RepoDTO): string {
  return (repo.group ?? '').trim()
}

interface RepoGroupBucket {
  key: string
  title: string
  items: RepoListEntry[]
}

/** 按分组聚合条目，供"总览"页渲染分组卡片；未分组排在最后。 */
function buildGroups(entries: RepoListEntry[], ungroupedLabel: string): RepoGroupBucket[] {
  const map = new Map<string, RepoListEntry[]>()
  for (const entry of entries) {
    const key = groupKeyOf(entry.repo)
    const list = map.get(key)
    if (list) list.push(entry)
    else map.set(key, [entry])
  }
  const buckets = Array.from(map.entries()).map(([key, items]) => ({
    key,
    // 分组名**没有**变化：key 就是服务端 meta.group 原值（空串 = 未分组）。
    title: key || ungroupedLabel,
    items
  }))
  buckets.sort((a, b) => {
    if (a.key === '') return 1
    if (b.key === '') return -1
    return a.key.localeCompare(b.key)
  })
  return buckets
}

/**
 * 分组批量操作的状态放在**模块级**而不是组件内：批量挂载/卸载可能跑几十秒，
 * 用户切到别的页面再回来时组件已经卸载重建，组件内的 busy/progress 会归零 ——
 * 看起来像"操作消失了"，其实后台还在跑。这里按分组 key 保存进度，任何页面重新挂载
 * 都能接着显示，也不会因为重新挂载而重复触发同一批操作。
 */
interface GroupOpState {
  busy: boolean
  progress: { done: number; total: number }
}

const GROUP_OP_IDLE: GroupOpState = { busy: false, progress: { done: 0, total: 0 } }
const groupOps = new Map<string, GroupOpState>()
const groupOpListeners = new Set<() => void>()

function setGroupOp(key: string, state: GroupOpState): void {
  groupOps.set(key, state)
  for (const listener of groupOpListeners) listener()
}

function subscribeGroupOps(listener: () => void): () => void {
  groupOpListeners.add(listener)
  return () => {
    groupOpListeners.delete(listener)
  }
}

function useGroupOpState(key: string): GroupOpState {
  return useSyncExternalStore(
    subscribeGroupOps,
    () => groupOps.get(key) ?? GROUP_OP_IDLE,
    () => GROUP_OP_IDLE
  )
}

/**
 * 分组整体挂载 / 卸载：按组内每个库的分配逐个调本机代理。
 *
 * 为什么在客户端做而不是服务端：挂载是"这台机器"的行为（盘符、本地会话、缓存代理都在
 * 本机），服务端只下发目标；分组只是一个标签。因此这里按组遍历，逐个复用单库挂载那一套
 * 语义（没有分配先建、再交给代理）。
 */
function useGroupMount(groupKey: string, repos: RepoDTO[], currentUserId: string) {
  const api = useApi()
  const agent = useAgent()
  const { active } = useServerConfig()
  const queryClient = useQueryClient()
  const { t } = useI18n()
  const op = useGroupOpState(groupKey)

  const mountAll = useCallback(async (): Promise<void> => {
    if (groupOps.get(groupKey)?.busy) return
    const targets = repos.filter((repo) => repo.state === 'active' && !isPlaceholderRepoId(repo.id))
    if (targets.length === 0) return
    setGroupOp(groupKey, { busy: true, progress: { done: 0, total: targets.length } })
    let ok = 0
    let fail = 0
    for (const repo of targets) {
      try {
        const allocations = await api.listAllocations(repo.id)
        let allocation = allocations.items.find(
          (item) => item.user_id === currentUserId && item.state !== 'released'
        )
        if (!allocation && repo.owner_id === currentUserId) {
          allocation = await api.allocate(repo.id, currentUserId)
        }
        if (!allocation) throw new Error(t('repo.card.notAllocated'))
        await agent.mount({
          allocation_id: allocation.id,
          repo_id: repo.id,
          repo_name: repo.name,
          repo_group: repo.group,
          // 只有「挂载分组」这一入口才允许分组配置覆盖组内库自己的配置。
          use_group_config: true,
          server_key: active?.key,
          server_url: active?.baseUrl
        })
        ok += 1
      } catch {
        fail += 1
      }
      setGroupOp(groupKey, {
        busy: true,
        progress: { done: (groupOps.get(groupKey)?.progress.done ?? 0) + 1, total: targets.length }
      })
    }
    await queryClient.invalidateQueries({ queryKey: ['repo-allocations'] })
    await queryClient.invalidateQueries({ queryKey: ['leases'] })
    setGroupOp(groupKey, { busy: false, progress: { done: 0, total: 0 } })
    if (fail > 0) message.warning(t('repo.group.mountPartial', { ok, fail }))
    else message.success(t('repo.group.mountDone', { count: ok }))
  }, [active?.baseUrl, active?.key, agent, api, currentUserId, groupKey, queryClient, repos, t])

  const unmountAll = useCallback(async (): Promise<void> => {
    if (groupOps.get(groupKey)?.busy) return
    const mounted = agent.state?.mounts ?? []
    const targets = repos
      .map((repo) => mountOfRepo(mounted, repo))
      .filter((item): item is NonNullable<typeof item> => Boolean(item))
    if (targets.length === 0) return
    setGroupOp(groupKey, { busy: true, progress: { done: 0, total: targets.length } })
    let ok = 0
    let fail = 0
    for (const item of targets) {
      try {
        await agent.unmount({ allocation_id: item.allocation_id })
        ok += 1
      } catch {
        fail += 1
      }
      setGroupOp(groupKey, {
        busy: true,
        progress: { done: (groupOps.get(groupKey)?.progress.done ?? 0) + 1, total: targets.length }
      })
    }
    await queryClient.invalidateQueries({ queryKey: ['repo-allocations'] })
    await queryClient.invalidateQueries({ queryKey: ['leases'] })
    setGroupOp(groupKey, { busy: false, progress: { done: 0, total: 0 } })
    if (fail > 0) message.warning(t('repo.group.unmountPartial', { ok, fail }))
    else message.success(t('repo.group.unmountDone', { count: ok }))
  }, [agent, groupKey, queryClient, repos, t])

  return { busy: op.busy, progress: op.progress, mountAll, unmountAll }
}

/** 某条挂载记录是否属于这个库（repo_id 优先，旧记录缺 repo_id 时退回库名匹配）。 */
function mountOfRepo(mounts: AgentMountState[], repo: RepoDTO): AgentMountState | undefined {
  return mounts.find(
    (mount) => mount.repo_id === repo.id || (mount.repo_id === '' && mount.repo_name === repo.name)
  )
}

/** 分组总览卡片：组名 + 库数 + 挂载/卸载 + 分组挂载设置。 */
function RepoGroupCard({
  bucket,
  currentUserId,
  onOpen,
  onSettings
}: {
  bucket: RepoGroupBucket
  currentUserId: string
  onOpen: () => void
  onSettings: () => void
}): JSX.Element {
  const { t } = useI18n()
  const agent = useAgent()
  const repos = bucket.items.map((entry) => entry.repo)
  const { busy, progress, mountAll, unmountAll } = useGroupMount(bucket.key, repos, currentUserId)
  const mounts = agent.state?.mounts ?? []
  const mountedRepos = repos.filter((repo) => mountOfRepo(mounts, repo) !== undefined)
  const mountedCount = mountedRepos.length
  const activeCount = repos.filter((repo) => repo.state === 'active').length
  const totalBytes = repos.reduce((sum, repo) => sum + (repo.capacity_bytes || 0), 0)
  const mountedBytes = mountedRepos.reduce(
    (sum, repo) => sum + (pickMountUsage(agent.usage, repo.id)?.total_bytes || repo.capacity_bytes || 0),
    0
  )
  // 一个按钮承载挂载 / 卸载：只要组内有库挂载着，就提供"卸载"；否则是"挂载"。
  const hasMounted = mountedCount > 0
  const percent = totalBytes > 0 ? Math.min(100, Math.round((mountedBytes / totalBytes) * 100)) : 0

  return (
    <Card
      hoverable
      onClick={onOpen}
      style={{ height: '100%', minHeight: 184 }}
      styles={{ body: { padding: spacing.md, display: 'flex', flexDirection: 'column', height: '100%' } }}
    >
      {/* 标题行：图标与组名保持常规字号，只有卡片本身更大；数量贴右。 */}
      <div style={{ display: 'flex', alignItems: 'center', gap: spacing.xs }}>
        <FolderOutlined style={{ color: palette.accent }} />
        <Typography.Text strong ellipsis style={{ minWidth: 0, fontSize: fontSize.lg }}>
          {bucket.title}
        </Typography.Text>
        <Typography.Text type="secondary" style={{ marginLeft: 'auto', fontSize: fontSize.sm }}>
          {t('repo.group.repoCount', { count: repos.length })}
        </Typography.Text>
      </div>

      <div style={{ marginTop: spacing.md, marginBottom: spacing.sm, flex: 1 }}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline' }}>
          <Typography.Text type="secondary" style={{ fontSize: fontSize.sm }}>
            {t('repo.group.mountedStorage')}
          </Typography.Text>
          <Typography.Text style={{ fontSize: fontSize.sm }}>
            {formatBytes(mountedBytes)} / {formatBytes(totalBytes)}
          </Typography.Text>
        </div>
        <Progress
          percent={percent}
          showInfo={false}
          size="small"
          strokeColor={usageColor(percent)}
          trailColor={palette.neutralFill}
          style={{ marginTop: spacing.xxs, marginBottom: 0 }}
        />
      </div>

      {/* 动作放右下：挂载/卸载一个按钮，设置只留图标、不要边框。 */}
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'flex-end', gap: spacing.xs }}>
        <Button
          size="small"
          type={hasMounted ? 'default' : 'primary'}
          loading={busy}
          disabled={repos.length === 0 || (!hasMounted && activeCount === 0)}
          onClick={(event) => {
            event.stopPropagation()
            void (hasMounted ? unmountAll() : mountAll())
          }}
        >
          {busy && progress.total > 0
            ? `${t(hasMounted ? 'repo.group.unmounting' : 'repo.group.mounting')} ${progress.done}/${progress.total}`
            : hasMounted
              ? t('repo.group.unmountAll')
              : t('repo.group.mountAll')}
        </Button>
        {/* 未分组没有分组名，无法承载分组配置；不给设置入口。 */}
        {bucket.key ? (
          <Tooltip title={t('repo.group.settings')}>
            <Button
              type="text"
              size="small"
              icon={<SettingOutlined />}
              aria-label={t('repo.group.settings')}
              onClick={(event) => {
                event.stopPropagation()
                onSettings()
              }}
            />
          </Tooltip>
        ) : null}
      </div>
    </Card>
  )
}

/** 分组下钻后的内容：返回 + 组名 + 批量挂载/卸载 + 分组设置 + 库列表。 */
function RepoGroupDetail({
  bucket,
  currentUserId,
  viewMode,
  onViewModeChange,
  onBack,
  onOpen,
  onConfigure,
  onOpenUpload,
  onSettings
}: {
  bucket: RepoGroupBucket
  currentUserId: string
  viewMode: RepoViewMode
  onViewModeChange: (next: RepoViewMode) => void
  onBack: () => void
  onOpen: (repoId: string) => void
  onConfigure: (repo: RepoDTO) => void
  onOpenUpload?: (uploadId: string) => void
  onSettings: () => void
}): JSX.Element {
  const { t } = useI18n()
  const agent = useAgent()
  // 防误触：进入分组的那一次点击如果是双击的第二下（或手速很快），会正好落在
  // 刚出现的「返回」按钮上，表现为"一点进去就弹回总览"。这里忽略挂载后极短时间内的返回。
  const mountedAt = useRef(Date.now())
  const repos = bucket.items.map((entry) => entry.repo)
  const { busy, progress, mountAll, unmountAll } = useGroupMount(bucket.key, repos, currentUserId)
  const mounts = agent.state?.mounts ?? []
  const mountedCount = repos.filter((repo) => mountOfRepo(mounts, repo) !== undefined).length
  const activeCount = repos.filter((repo) => repo.state === 'active').length
  const hasMounted = mountedCount > 0

  return (
    <Space direction="vertical" size={spacing.lg} style={{ width: '100%' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: spacing.sm, flexWrap: 'wrap' }}>
        <Button
          icon={<ArrowLeftOutlined />}
          onClick={() => {
            if (Date.now() - mountedAt.current < 500) return
            onBack()
          }}
        >
          {t('repo.group.back')}
        </Button>
        <FolderOutlined style={{ color: palette.accent }} />
        <Typography.Text strong>{bucket.title}</Typography.Text>
        <Typography.Text type="secondary" style={{ fontSize: fontSize.sm }}>
          {bucket.items.length}
        </Typography.Text>
        <span style={{ marginLeft: 'auto' }}>
          <Space wrap>
            <Button
              type={hasMounted ? 'default' : 'primary'}
              size="small"
              loading={busy}
              disabled={repos.length === 0 || (!hasMounted && activeCount === 0)}
              onClick={() => void (hasMounted ? unmountAll() : mountAll())}
            >
              {busy && progress.total > 0
                ? `${t(hasMounted ? 'repo.group.unmounting' : 'repo.group.mounting')} ${progress.done}/${progress.total}`
                : hasMounted
                  ? t('repo.group.unmountAll')
                  : t('repo.group.mountAll')}
            </Button>
            {bucket.key ? (
              <Tooltip title={t('repo.group.settings')}>
                <Button
                  type="text"
                  size="small"
                  icon={<SettingOutlined />}
                  aria-label={t('repo.group.settings')}
                  onClick={onSettings}
                />
              </Tooltip>
            ) : null}
            <RepoViewSwitch value={viewMode} onChange={onViewModeChange} />
          </Space>
        </span>
      </div>
      <RepoSection
        title=""
        items={bucket.items}
        viewMode={viewMode}
        currentUserId={currentUserId}
        onOpen={onOpen}
        onConfigure={onConfigure}
        onOpenUpload={onOpenUpload}
      />
    </Space>
  )
}

/** 存储库列表：管理端表格；用户端卡片/列表两种形态（可切换 + 分组分页）。 */
export function RepoList({ isSuperAdmin, onCreate, onOpen, onOpenUpload, onImport }: RepoListProps): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const { user } = useAuth()
  const agent = useAgent()
  const [keyword, setKeyword] = useState('')
  // 展示形式只影响用户端（管理端是一张运维表，形态切换对它没有意义）。
  // 初值取本地记住的那一份：用户上次选了列表，刷新/重开客户端仍是列表。
  const [viewMode, setViewMode] = useState<RepoViewMode>(readStoredView)
  /** 切换形态：即时生效并记住（下次进来直接是这一种）。 */
  const changeView = (next: RepoViewMode): void => {
    setViewMode(next)
    storeView(next)
  }
  // 挂载配置弹窗只服务当前点开的这一个库（弹窗在列表这一层只保留一份，避免每张卡都挂一个）。
  const [settingsRepo, setSettingsRepo] = useState<RepoDTO | null>(null)
  // 分组挂载设置弹窗（同一次只开一个分组）。
  const [settingsGroup, setSettingsGroup] = useState<string | null>(null)
  // 用户端的分组下钻：null = 分组总览；否则是当前进入的分组 key（空串 = 未分组）。
  // 初值取会话记忆：从详情页返回时能落回原来的分组（见 GROUP_PREF_KEY）。
  const [activeGroup, setActiveGroupState] = useState<string | null>(readStoredGroup)
  const setActiveGroup = useCallback((next: string | null): void => {
    setActiveGroupState(next)
    storeGroup(next)
  }, [])

  // 缓存用量与命中情况会随读写持续变化：页面在时按 10s 轮询一次（离开页面即停）。
  // 管理端是运维表、不展示缓存，跳过轮询。单个轮询器放在这一层：放到卡片里会变成每张卡一个。
  const refreshCache = agent.refreshCache
  useEffect(() => {
    if (!agent.available || isSuperAdmin) return
    void refreshCache()
    const timer = window.setInterval(() => void refreshCache(), 10000)
    return () => window.clearInterval(timer)
  }, [agent.available, isSuperAdmin, refreshCache])

  // 卡片上的「已用」取本机挂载卷的实测值（见 repoUsage）：同样按 10s 轮询，同一个轮询器
  // 也只放这一层。管理端是运维表（看的是服务端口径），不轮询。
  useAgentUsagePolling(!isSuperAdmin)

  const reposQuery = useQuery({
    queryKey: ['repos', isSuperAdmin ? 'all' : 'mine'],
    queryFn: () => api.listRepos({ limit: 200 })
  })

  const usersQuery = useQuery({
    queryKey: ['users', 'options'],
    queryFn: () => api.listUsers({ limit: 200 }),
    enabled: isSuperAdmin
  })

  const ownerName = useMemo(() => {
    const map = new Map<string, string>()
    for (const item of usersQuery.data?.items ?? []) map.set(item.id, item.username)
    return map
  }, [usersQuery.data])

  // 进行中的上传（本机代理的实时快照，含速率与 ETA）：它们就以库的身份混在列表里（见 entries）。
  // 管理端不参与：上传是本机代理的行为，而它看的是服务端全量库。
  const activeUploads = useMemo(
    () => (isSuperAdmin || !onOpenUpload ? [] : agent.uploads.filter(isActiveUpload)),
    [agent.uploads, isSuperAdmin, onOpenUpload]
  )

  /**
   * 列表里还有"没走完"的库吗 —— 有就自己盯着刷新，直到一个都不剩。
   *
   * 为什么必须自动刷（真实反馈："上传完成后要去掉上传中这个标签"）：那张「上传中」卡是
   * **客户端合成**的占位库（服务端此刻还没有这条记录，见 uploadPlaceholderRepo），它变成真库、
   * 以及真库从「写入中」变成正常，全都只发生在服务端，而且都是后台异步发生的（建库任务是队列里
   * 的另一次请求、写母盘几十分钟）—— 没有任何事件会推到这个页面上。于是不手点刷新的人会一直
   * 看着一枚早就该消失的标签（在新建页发起上传、随后切到列表页时尤其明显：那里的收尾刷新随
   * 表单卸载一起没了）。
   *
   * 两头一起看：
   *   - 服务端侧：列表里有 creating / importing / sealing / deleting 的库（见 PENDING_REPO_STATES），
   *     它们自己会往下走；
   *   - 本机侧：还有进行中的上传 —— 包括**重传覆盖**（那时库仍是 active，只看状态会漏），以及
   *     刚够到「建库中」那一帧（服务端的库记录还没出现，全靠占位卡顶着）。
   */
  const hasPendingRepos = useMemo(
    () =>
      activeUploads.length > 0 ||
      (reposQuery.data?.items ?? []).some((repo) => PENDING_REPO_STATES.includes(repo.state)),
    [activeUploads, reposQuery.data]
  )

  // 中间态存在期间按固定节拍重取。
  //
  // 用 effect + setInterval 而不是 useQuery 的 refetchInterval：这条判据要读的正是**这次查询的
  // 结果**（列表里有没有中间态的库），写成 refetchInterval 就得绕回上一轮渲染的值；这样子写也
  // 与本页复用缓存那个轮询器（见上面的 refreshCache）一个模样。卸载即停，不留后台心跳。
  const refetchRepos = reposQuery.refetch
  useEffect(() => {
    if (!hasPendingRepos) return
    const timer = window.setInterval(() => void refetchRepos(), PENDING_REFRESH_MS)
    return () => window.clearInterval(timer)
  }, [hasPendingRepos, refetchRepos])

  // 上传刚收工的那一拍**立刻**补一次：真库记录正是这一刻才在服务端出现的，而列表手里还是旧快照。
  // 不补的话，占位卡先消失、真库要等下一拍轮询才回来 —— 中间那几秒列表里空着一块，看着像库被
  // 弄丢了。判据用"活跃集合里少了人"：只在上传真正结束时触发一次，不跟着进度事件走（进度事件
  // 每秒都有，跟着它走就等于每秒打一次列表接口）。
  const activeUploadIds = useMemo(
    () => activeUploads.map((upload) => upload.upload_id || upload.local_dir),
    [activeUploads]
  )
  const lastActiveIds = useRef(activeUploadIds)
  useEffect(() => {
    const finished = lastActiveIds.current.some((id) => !activeUploadIds.includes(id))
    lastActiveIds.current = activeUploadIds
    if (finished) void refetchRepos()
  }, [activeUploadIds, refetchRepos])

  const entries = useMemo<RepoListEntry[]>(() => {
    const items = reposQuery.data?.items ?? []
    const byName = new Map(activeUploads.map((upload) => [upload.repo_name, upload]))

    // 服务端已经有这个库（上传到了建库阶段）：用**服务端记录**承载上传进度，
    // 这样点进去有真 id、状态也以服务端为准 —— 不另造一张同名卡片。
    const merged: RepoListEntry[] = items.map((repo) => {
      const upload = byName.get(repo.name)
      // 仍然是 active 的库不算"上传中"：它此刻能正常挂载使用（这是重传覆盖还没轮到重建
      // 母盘的场景），给它贴上「上传中」只会让人以为库不能用了 —— 这次覆盖上传的进度去
      // 「上传任务」页看（新建页发起后就在那儿）。
      return { repo, upload: upload && repo.state !== 'active' ? upload : undefined }
    })

    // 服务端还没有的（扫描 / 建会话 / 传数据三个阶段）：合成一条，让它和其他库一样出现在
    // 列表里 —— 发起上传的人必须马上在列表里看到"这块库正在生成"。
    const known = new Set(items.map((repo) => repo.name))
    for (const upload of activeUploads) {
      if (known.has(upload.repo_name)) continue
      merged.push({ repo: uploadPlaceholderRepo(upload, user?.id ?? ''), upload })
    }

    const lowered = keyword.trim().toLowerCase()
    if (!lowered) return merged
    return merged.filter((entry) => entry.repo.name.toLowerCase().includes(lowered))
  }, [reposQuery.data, keyword, activeUploads, user?.id])

  // 记忆的分组可能已经被改名/删空：这时落回总览，别停在一个不存在的分组里。
  //
  // 用 entries（含**上传中的占位库**）判断，而不是只看服务端已存在的记录：
  // 未分组的上传占位库同样属于「未分组」这个分组，只看服务端列表会把它误判成
  // "分组不存在"，于是点进未分组又被立刻弹回总览。
  useEffect(() => {
    if (isSuperAdmin || activeGroup === null || reposQuery.isLoading) return
    const exists = entries.some((entry) => groupKeyOf(entry.repo) === activeGroup)
    if (!exists) setActiveGroup(null)
  }, [activeGroup, isSuperAdmin, reposQuery.isLoading, entries, setActiveGroup])

  const columns = useMemo<TableColumnsType<RepoDTO>>(() => {
    const base: TableColumnsType<RepoDTO> = [
      { title: t('field.name'), dataIndex: 'name', key: 'name' },
      { title: t('field.mode'), dataIndex: 'mode', key: 'mode', width: 200, render: (value: string) => repoModeLabel(value) },
      { title: t('field.group'), dataIndex: 'group', key: 'group', render: (value?: string) => value || '-' },
      {
        // 管理端表格看的是**服务端口径**（已分配磁盘总量，分母 = 库容量，见 repoUsage）：
        // 运维要的是"这块库分配出去多大"，与客户端卡片的本机实测值是两回事。
        title: t('repo.used'),
        key: 'size',
        render: (_value, repo) => repoUsage(repo).text
      },
      {
        title: t('common.status'),
        dataIndex: 'state',
        key: 'state',
        width: 110,
        render: (value: string) => <StatusTag group="repo" value={value} />
      },
      {
        title: t('common.createdAt'),
        dataIndex: 'created_at',
        key: 'created_at',
        width: 170,
        render: (value: number) => formatTime(value)
      },
      {
        title: t('common.actions'),
        key: 'actions',
        width: 100,
        render: (_value, repo) => (
          <Button type="link" size="small" onClick={() => onOpen(repo.id)}>
            {t('repo.detail.title')}
          </Button>
        )
      }
    ]
    if (isSuperAdmin) {
      base.splice(2, 0, {
        title: t('field.owner'),
        dataIndex: 'owner_id',
        key: 'owner_id',
        render: (value: string) => ownerName.get(value) ?? value
      })
    }
    return base
  }, [isSuperAdmin, onOpen, ownerName, t])

  const renderUserRepos = (): JSX.Element => {
    if (reposQuery.isLoading) return <RepoListSkeleton viewMode={viewMode} />

    // 上传中的占位库已经算在 entries 里，所以这里不必再单独判一次"有没有在上传"。
    if (entries.length === 0) {
      return (
        <EmptyState
          title={t('repo.empty')}
          description={t('repo.empty.desc')}
          action={{ label: t('common.create'), onClick: onCreate }}
        />
      )
    }

    const userId = user?.id ?? ''
    const groups = buildGroups(entries, t('repo.group.ungrouped'))
    const current = activeGroup === null ? undefined : groups.find((bucket) => bucket.key === activeGroup)

    // 下钻视图：只列当前分组的库。分组被删除/改名（服务端数据变了）时回退到总览。
    if (current) {
      return (
        <RepoGroupDetail
          key={`${current.key}-${keyword}`}
          bucket={current}
          currentUserId={userId}
          viewMode={viewMode}
          onViewModeChange={changeView}
          onBack={() => setActiveGroup(null)}
          onOpen={onOpen}
          onConfigure={setSettingsRepo}
          onOpenUpload={onOpenUpload}
          onSettings={() => setSettingsGroup(current.key)}
        />
      )
    }

    return (
      <Space direction="vertical" size={spacing.lg} style={{ width: '100%' }}>
        {/* 配额总览只认服务端的库：还没建成的上传中库不算"一个存储库"，也不占容量。 */}
        <QuotaOverview repos={entries.filter((entry) => !isPlaceholderRepoId(entry.repo.id)).map((entry) => entry.repo)} />
        <Row gutter={[spacing.md, spacing.md]}>
          {groups.map((bucket) => (
            <Col key={bucket.key || '__ungrouped__'} xs={24} sm={12} xl={12} xxl={8}>
              <RepoGroupCard
                bucket={bucket}
                currentUserId={userId}
                onOpen={() => setActiveGroup(bucket.key)}
                onSettings={() => setSettingsGroup(bucket.key)}
              />
            </Col>
          ))}
        </Row>
      </Space>
    )
  }

  return (
    <PageShell
      title={t(isSuperAdmin ? 'page.repos.title' : 'page.myRepos.title')}
      extra={
        <>
          {/* 展示形式切换已移入内容区第一个分组标题行右侧（只服务用户端，见 RepoViewSwitch）。 */}
          <Input.Search
            allowClear
            placeholder={t('common.search')}
            style={{ width: 220 }}
            onChange={(event) => setKeyword(event.target.value)}
          />
          <Button onClick={() => void reposQuery.refetch()}>{t('common.refresh')}</Button>
          {/* 导入放在「新建」左边：它是低频的迁移动作，位置上与主操作区分开，
              免得日常建库时手滑点进来。 */}
          {isSuperAdmin && onImport ? <Button onClick={onImport}>{t('repo.import')}</Button> : null}
          <Button type="primary" onClick={onCreate}>
            {t('common.create')}
          </Button>
        </>
      }
    >
      {reposQuery.error ? <ErrorNotice error={reposQuery.error} /> : null}
      {/* 每库独立的挂载配置：卡片「配置」图标打开，只改被点的这一个库 */}
      {settingsRepo ? (
        <RepoMountSettingsModal repo={settingsRepo} open onClose={() => setSettingsRepo(null)} />
      ) : null}
      {/* 分组挂载设置：一次只开一个分组（含"使用分组配置"开关）。 */}
      {settingsGroup !== null ? (
        <GroupMountSettingsModal group={settingsGroup} open onClose={() => setSettingsGroup(null)} />
      ) : null}
      {isSuperAdmin ? (
        <DataTable<RepoDTO>
          columns={columns}
          rows={entries.map((entry) => entry.repo)}
          rowKey={(repo) => repo.id}
          loading={reposQuery.isLoading}
          empty={
            <EmptyState
              title={t('repo.empty')}
              description={t('repo.empty.desc')}
              action={{ label: t('common.create'), onClick: onCreate }}
            />
          }
        />
      ) : (
        renderUserRepos()
      )}
    </PageShell>
  )
}
