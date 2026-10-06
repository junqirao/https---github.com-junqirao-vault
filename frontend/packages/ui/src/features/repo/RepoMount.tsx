import { useCallback, useMemo, useState } from 'react'
import { Button, Tooltip, Typography } from 'antd'
import { useQuery, useQueryClient } from '@tanstack/react-query'

import { agentApi } from '../../api/agentClient'
import { useApi } from '../../api/provider'
import type { AgentMountState } from '../../api/agentTypes'
import type { RepoDTO } from '../../api/types'
import { useAgent } from '../../hooks/useAgent'
import { useI18n } from '../../i18n'
import { fontSize, spacing } from '../../tokens/palette'
import { MountStateCell, mountSessionLive, openTargetOf } from '../mount/MountControls'

/** 存储库卡片上的挂载能力控制器。 */
export interface RepoMountController {
  /** 本库在本机的挂载状态（未挂载时为空）。 */
  mount: AgentMountState | undefined
  /** 挂载点：盘符模式为 "E:"，目录模式为绝对目录。 */
  mountPath: string
  /** 已挂载（挂载点可用、可打开）：要求记录是 mounted **且实测会话还在**。 */
  mounted: boolean
  /**
   * 实测的本机 iSCSI 会话状态：true/false；undefined = 尚未核对（按记录展示）。
   *
   * 界面判断"这块盘到底挂没挂上"一律看它，不看记录（见 mountSessionLive）。
   */
  sessionLive: boolean | undefined
  /** 记录还在，但代理实测本机已无活动会话（盘实际不在了）。 */
  sessionLost: boolean
  /**
   * 挂载**失败**（需要用户看一眼）：本次请求报错，或本机状态为 error/revoked，
   * 或记录说"已挂载"但实测会话已断。
   *
   * 与标签行里那个红色感叹号（MountStateCell 的 mountErrorLines）同一套口径：
   * 它返回 true，就一定有一段可读的报错能展示；库名前的红点与它说的也是同一件事。
   */
  failed: boolean
  /** 代理侧状态机进行中（挂载中 / 卸载中）。 */
  pending: boolean
  /** 本次操作请求进行中。 */
  busy: boolean
  /**
   * 可执行挂载（代理可用、库已就绪，且已有分配或本人有权自建分配）。
   *
   * 含"库已就绪"这一条：建库中（creating）的库还没有可挂的盘，服务端也会拒绝
   * （repo.creating），按钮提前置灰并说明原因。
   */
  canMount: boolean
  /**
   * 可执行卸载（代理可用，且已有分配或本人有权自建分配）。
   *
   * **与库状态无关**：库异常/删除中时，本机已经挂上的盘反而更需要能摘掉。
   */
  canUnmount: boolean
  /** 不可挂载的原因文案；可挂载时为空。 */
  disabledReason: string | undefined
  error: unknown
  mountRepo: () => Promise<void>
  unmountRepo: () => Promise<void>
  openMountDir: () => Promise<void>
}

/**
 * 存储库卡片的一键挂载：解析本人在该库的分配 → 挂载/卸载 → 打开本地挂载目录。
 *
 * 分配解析复用 `['repo-allocations', repoId]` 缓存（与存储库详情页同键，不重复请求）；
 * 库主尚无分配时按需自建（服务端对「同库同用户」幂等，已有未释放分配会直接复用）。
 */
export function useRepoMount(repo: RepoDTO, currentUserId: string): RepoMountController {
  const api = useApi()
  const agent = useAgent()
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>(null)

  // 代理不可用时既无法挂载也拿不到本地挂载状态，直接跳过请求。
  const allocationsQuery = useQuery({
    queryKey: ['repo-allocations', repo.id],
    queryFn: () => api.listAllocations(repo.id),
    enabled: agent.available
  })

  const allocationId = useMemo(() => {
    const items = allocationsQuery.data?.items ?? []
    const mine = items.find((item) => item.user_id === currentUserId && item.state !== 'released')
    return mine?.id
  }, [allocationsQuery.data, currentUserId])

  const mount = useMemo(() => {
    const mounts = agent.state?.mounts ?? []
    if (allocationId) {
      const hit = mounts.find((item) => item.allocation_id === allocationId)
      if (hit) return hit
    }
    // 兜底：挂载请求也带 repo_id，可据此关联到本库（例如分配列表尚未刷新时）。
    return mounts.find((item) => item.repo_id !== '' && item.repo_id === repo.id)
  }, [agent.state, allocationId, repo.id])

  const isOwner = repo.owner_id === currentUserId
  // 实测会话状态：界面判断"挂没挂上"以此为准（会话被断后记录仍是 mounted，会撒谎）。
  const sessionLive = mountSessionLive(mount)
  const sessionLost = mount !== undefined && sessionLive === false
  // 断线时不再算"已挂载"：挂载点已随会话消失，不能显示、也不能打开（真实诉求：
  // "挂载和卸载的按钮应以实际为准，看对应的 iSCSI 连接是否在活动中的"）。
  const mounted = mount?.state === 'mounted' && !sessionLost
  const pending = mount?.state === 'mounting' || mount?.state === 'unmounting'
  // 失败口径与标签行的红色感叹号一致（见 MountStateCell → mountErrorLines）：本次请求报错，
  // 或本机状态 error/revoked；断线（sessionLost）也算 —— 盘实际不在，标签行已经写着「已断开」，
  // 此时还标一颗"从没挂过"的灰点，两个标记会互相矛盾。
  const failed = Boolean(error) || mount?.state === 'error' || mount?.state === 'revoked' || sessionLost
  // 库必须 active 才能挂载：建库中（creating）时差异盘还没派生出来，此时"挂载"点了也只会
  // 换来一句对不上的报错（真实反馈："逻辑有误啊，creating 中的存储库不允许挂载啊"）。
  // 服务端同样拦住（RequestMount → repo.creating），这里只是提前把按钮置灰并说清原因。
  const repoReady = repo.state === 'active'
  const canUnmount = agent.available && (allocationId !== undefined || isOwner)
  const canMount = canUnmount && repoReady

  const disabledReason = !agent.available
    ? t('agent.hostMissing')
    : !repoReady
      ? t('repo.card.notReady', { state: t(`state.repo.${repo.state}`) })
      : canMount
        ? undefined
        : t('repo.card.notAllocated')

  const invalidate = useCallback(async (): Promise<void> => {
    await queryClient.invalidateQueries({ queryKey: ['repo-allocations', repo.id] })
    await queryClient.invalidateQueries({ queryKey: ['leases'] })
  }, [queryClient, repo.id])

  const mountRepo = useCallback(async (): Promise<void> => {
    setError(null)
    setBusy(true)
    try {
      let target = allocationId
      if (!target) {
        // 库主首次挂载：先按库的默认差异盘参数建立分配，再交给代理挂载。
        const created = await api.allocate(repo.id, currentUserId)
        target = created.id
      }
      await agent.mount({ allocation_id: target, repo_id: repo.id, repo_name: repo.name })
      await invalidate()
    } catch (err) {
      setError(err)
    } finally {
      setBusy(false)
    }
  }, [agent, allocationId, api, currentUserId, invalidate, repo.id, repo.name])

  const unmountRepo = useCallback(async (): Promise<void> => {
    if (!mount) return
    setError(null)
    setBusy(true)
    try {
      await agent.unmount({ allocation_id: mount.allocation_id })
      await invalidate()
    } catch (err) {
      setError(err)
    } finally {
      setBusy(false)
    }
  }, [agent, invalidate, mount])

  const openMountDir = useCallback(async (): Promise<void> => {
    // 会话已断时挂载点不存在，打开只会报"路径不存在"（mounted 已含实测会话判定）。
    if (!mount || !mounted) return
    setError(null)
    try {
      await agentApi.openPath(openTargetOf(mount))
    } catch (err) {
      setError(err)
    }
  }, [mount])

  return {
    mount,
    mountPath: mount?.mount_path ?? '',
    mounted,
    sessionLive,
    sessionLost,
    failed,
    pending,
    busy,
    canMount,
    canUnmount,
    disabledReason,
    error,
    mountRepo,
    unmountRepo,
    openMountDir
  }
}

export interface RepoMountActionsProps {
  controller: RepoMountController
}

/** 卡片头部的挂载/卸载按钮：与「打开」图标并排，随标题行垂直居中。 */
export function RepoMountActions({ controller }: RepoMountActionsProps): JSX.Element {
  const { t } = useI18n()
  const { mount, sessionLost, pending, busy, canMount, canUnmount, disabledReason } = controller
  // 挂载/卸载只受"代理可用 + 有分配"约束，与库状态无关：库异常/建库中时仍要能摘掉本机的盘。
  // "操作进行中"（pending）由下面的 working 统一接管，这里不再重复判断。
  const mountDisabled = !canMount
  const unmountDisabled = !canUnmount
  // 记录在、但实测已无活动会话（会话被断/服务重启）：盘实际不在了，"卸载"是错的入口 ——
  // 用户点它只是把记录收掉，盘还是挂不上。此时给"挂载"：代理会先清理残留再真重连
  // （服务端/代理自己闭环），用户不必先卸载再挂载。
  const showMount = mount === undefined || sessionLost
  // 操作进行中：本次请求在跑，或代理状态机已经是 mounting/unmounting。
  const working = busy || pending

  return (
    // 卡片整卡可点进详情，按钮区的事件不得冒泡触发跳转。
    <span style={{ display: 'inline-flex', flexShrink: 0 }} onClick={(event) => event.stopPropagation()}>
      {working ? (
        // 进行中只留一个"置灰 + 转圈"的按钮，**不写文字**。
        //
        // 为什么不能沿用文字：挂载请求一发出，代理状态机立刻变成 mounting，showMount 翻假，
        // 按钮会换成"卸载"（而且被 pending 一起置灰）—— 用户刚点完挂载就看见"卸载"，
        // 只能理解为"点错了"或"系统反过来要求我卸载"。留空就不会说错。
        // minWidth 与带文字的按钮对齐，避免操作前后布局跳动。
        <Button
          size="small"
          loading
          disabled
          aria-label={t('common.loading')}
          style={{ minWidth: 64 }}
        />
      ) : showMount ? (
        <Tooltip title={disabledReason}>
          <span style={{ display: 'inline-block' }}>
            <Button
              size="small"
              type="primary"
              disabled={mountDisabled}
              onClick={() => void controller.mountRepo()}
            >
              {t('action.mount')}
            </Button>
          </span>
        </Tooltip>
      ) : (
        <Button size="small" disabled={unmountDisabled} onClick={() => void controller.unmountRepo()}>
          {t('action.unmount')}
        </Button>
      )}
    </span>
  )
}

/**
 * 卡片标签行里的挂载状态标签（无记录时显示"未挂载"）。
 *
 * 与存储库状态标签**并排**：用户一眼就能读到"库正常 / 本机未挂载"这一组状态，
 * 不必滚到卡片底部才知道本机挂没挂上。失败原因紧随其后（红色感叹号，hover 查看并可复制）。
 */
export function RepoMountStateTag({ controller }: RepoMountActionsProps): JSX.Element {
  return (
    // 整卡可点进详情，标签自身的事件不得冒泡触发跳转。
    <span
      style={{ display: 'inline-flex', alignItems: 'center', gap: spacing.xxs }}
      onClick={(event) => event.stopPropagation()}
    >
      {/* 错误只用一个红色感叹号展示：本次请求错误与本机残留错误由 MountStateCell 合并成
          多行悬浮详情（用户反馈：两个标记很乱）。 */}
      <MountStateCell mount={controller.mount} requestError={controller.error} />
    </span>
  )
}

/** 卡片底部的挂载信息：只保留挂载点（失败原因见状态标签旁的感叹号图标）。 */
export function RepoMountStatus({ controller }: RepoMountActionsProps): JSX.Element | null {
  const { mountPath, mounted } = controller
  if (!mounted || !mountPath) return null

  return (
    <div style={{ minWidth: 0 }} onClick={(event) => event.stopPropagation()}>
      <Tooltip title={mountPath}>
        <Typography.Text type="secondary" ellipsis style={{ fontSize: fontSize.xs, maxWidth: 180 }}>
          {mountPath}
        </Typography.Text>
      </Tooltip>
    </div>
  )
}