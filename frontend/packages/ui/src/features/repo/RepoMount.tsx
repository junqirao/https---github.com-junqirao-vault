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
import { MountStateCell, openTargetOf } from '../mount/MountControls'

/** 存储库卡片上的挂载能力控制器。 */
export interface RepoMountController {
  /** 本库在本机的挂载状态（未挂载时为空）。 */
  mount: AgentMountState | undefined
  /** 挂载点：盘符模式为 "E:"，目录模式为绝对目录。 */
  mountPath: string
  /** 已挂载（挂载点可用、可打开）。 */
  mounted: boolean
  /** 代理侧状态机进行中（挂载中 / 卸载中）。 */
  pending: boolean
  /** 本次操作请求进行中。 */
  busy: boolean
  /** 可执行挂载（代理可用，且已有分配或本人有权自建分配）。 */
  canMount: boolean
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
  const mounted = mount?.state === 'mounted'
  const pending = mount?.state === 'mounting' || mount?.state === 'unmounting'
  const canMount = agent.available && (allocationId !== undefined || isOwner)

  const disabledReason = !agent.available
    ? t('agent.hostMissing')
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
    if (!mount || mount.state !== 'mounted') return
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
    pending,
    busy,
    canMount,
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
  const { mount, pending, busy, canMount, disabledReason } = controller
  const disabled = !canMount || pending

  return (
    // 卡片整卡可点进详情，按钮区的事件不得冒泡触发跳转。
    <span style={{ display: 'inline-flex', flexShrink: 0 }} onClick={(event) => event.stopPropagation()}>
      {mount ? (
        <Button size="small" disabled={disabled} loading={busy} onClick={() => void controller.unmountRepo()}>
          {t('action.unmount')}
        </Button>
      ) : (
        <Tooltip title={disabledReason}>
          <span style={{ display: 'inline-block' }}>
            <Button
              size="small"
              type="primary"
              disabled={disabled}
              loading={busy}
              onClick={() => void controller.mountRepo()}
            >
              {t('action.mount')}
            </Button>
          </span>
        </Tooltip>
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