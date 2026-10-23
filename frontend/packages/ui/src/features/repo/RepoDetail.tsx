import { useEffect, useRef, useState } from 'react'
import { Alert, Button, Descriptions, Form, Input, InputNumber, Modal, Progress, Select, Space, Tabs, message } from 'antd'
import { ArrowLeftOutlined } from '@ant-design/icons'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { ConfirmDialog } from '../../components/ConfirmDialog'
import { ErrorNotice } from '../../components/ErrorNotice'
import { LoadingState } from '../../components/LoadingState'
import { PageShell } from '../../components/PageShell'
import { SectionCard } from '../../components/SectionCard'
import { StatusTag } from '../../components/StatusTag'
import { UsageBar, UsageValueText } from '../../components/UsageBar'
import { useApi } from '../../api/provider'
import type { JobDTO, ParentAction, RepoDTO, UpdateRepoRequest } from '../../api/types'
import { pickMountUsage, repoUsageLoading, useAgent, useAgentUsagePolling } from '../../hooks/useAgent'
import { isJobTerminal, useJobProgress } from '../../hooks/useJobProgress'
import { useI18n } from '../../i18n'

import { spacing } from '../../tokens/palette'
import { formatBytes, formatTime, repoUsage } from '../../utils/format'
import { parentConditionLabel, repoModeLabel } from '../../utils/labels'
import { RepoDisks } from './RepoDisks'
import { RepoMembers } from './RepoMembers'
import { RepoParentFiles } from './RepoParentFiles'
import { RepoVerifyDiff } from './RepoVerifyDiff'
import { RepoMountActions, useRepoMount } from './RepoMount'
import { RepoGroupSelect } from './RepoGroupSelect'
import { RepoMountSettingsPanel } from './RepoMountSettings'

export interface RepoDetailProps {
  repoId: string
  isSuperAdmin: boolean
  currentUserId: string
  /** 当前用户的显示名：非管理员无法拉取用户列表，用于自己的分配列回退显示。 */
  currentUserName: string
  onBack: () => void
}

interface EditValues {
  name: string
  group?: string
  quota_bytes?: number
  max_diff_disks?: number
  /** 说明备注：随母盘元数据一起迁移，换机器后「导入磁盘」能认出这是谁家的盘。 */
  note?: string
}

/** 存储库详情：概览 + 成员 + 磁盘（含分配与挂载），以及编辑/删除/母盘操作/复制。 */
export function RepoDetail({ repoId, isSuperAdmin, currentUserId, currentUserName, onBack }: RepoDetailProps): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const agent = useAgent()
  const queryClient = useQueryClient()
  const [error, setError] = useState<unknown>(null)
  const [editOpen, setEditOpen] = useState(false)
  const [copyOpen, setCopyOpen] = useState(false)
  const [deleteOpen, setDeleteOpen] = useState(false)
  // 维护态「清理差异盘」的异步任务：提交后跟踪它的进度，终态再刷新页面数据。
  const [cleanupJob, setCleanupJob] = useState<JobDTO | null>(null)
  // 「退出维护 / 撤销临时共享」的异步任务：拆母盘 iSCSI 目标要走多次 PowerShell（几十秒起），
  // 服务端改成任务返回，界面跟踪到终态再刷新（见 RepoService.runParentTeardown）。
  const [parentJob, setParentJob] = useState<JobDTO | null>(null)
  // 维护态「扩容母盘」弹窗。
  const [resizeOpen, setResizeOpen] = useState(false)
  // 上传核对弹窗。
  const [verifyOpen, setVerifyOpen] = useState(false)
  // 存储库迁移：目标存储选择弹窗 + 本次发起的迁移任务（用于跟踪进度与终态刷新）。
  const [moveOpen, setMoveOpen] = useState(false)
  const [moveStorageID, setMoveStorageID] = useState('')
  const [moveJob, setMoveJob] = useState<JobDTO | null>(null)
  const [resizeForm] = Form.useForm<{ size_gb: number }>()
  const [editForm] = Form.useForm<EditValues>()
  const [copyForm] = Form.useForm<{ new_repo_name: string }>()

  // 防误触：从列表点进详情的那一下如果紧跟第二次点击（双击/手速快），可能正好落在
  // 刚出现的「返回」上，表现为"点进去就跳回上一页"。挂载后极短时间内的返回一律忽略。
  const mountedAt = useRef(Date.now())
  const goBack = (): void => {
    if (Date.now() - mountedAt.current < 500) return
    onBack()
  }

  const repoQuery = useQuery({
    queryKey: ['repo', repoId],
    queryFn: () => api.getRepo(repoId)
  })

  // 迁移弹窗的目标存储列表：只在打开弹窗时拉，避免每次进详情都多一个请求。
  const storagesQuery = useQuery({
    queryKey: ['storages'],
    queryFn: () => api.listStorages(),
    enabled: moveOpen
  })

  // 「母盘文件」页签的前置条件：维护态 + **已无差异盘**。
  // 有差异盘时改母盘会让它们基于旧基线，因此页签与接口都必须拒绝（见 ParentMount）。
  // 与「磁盘」tab 共用同一 query key，清理完成后自动刷新。
  const disksQuery = useQuery({
    queryKey: ['repo-disks', repoId],
    queryFn: () => api.listRepoDisks(repoId),
    enabled: repoQuery.data?.parent_condition === 'maintenance'
  })

  // 概览里的「已用」也要是本机实测值（见下面 repoUsage 的说明）：页面在时按 10s 轮询。
  useAgentUsagePolling(true)

  const refresh = (): void => {
    void queryClient.invalidateQueries({ queryKey: ['repo', repoId] })
    void queryClient.invalidateQueries({ queryKey: ['repos'] })
    // 容量变更（扩容/维护/母盘更新）会改用户级用量，列表页顶部的总账也要跟着变。
    void queryClient.invalidateQueries({ queryKey: ['me'] })
    // 维护相关操作会改磁盘记录（扩容改容量、清理删差异盘、退出维护拆目标），
    // 「磁盘」tab 必须跟着刷新，否则看到的还是操作前的数据。
    void queryClient.invalidateQueries({ queryKey: ['repo-disks', repoId] })
    // 母盘临时挂载的 @temp 占位分配也会随维护进入/退出出现/消失。
    void queryClient.invalidateQueries({ queryKey: ['repo-allocations', repoId] })
  }

  const updateMutation = useMutation({
    mutationFn: (values: UpdateRepoRequest) => api.updateRepo(repoId, values),
    onSuccess: () => {
      setEditOpen(false)
      refresh()
    },
    onError: (err) => setError(err)
  })

  const deleteMutation = useMutation({
    mutationFn: () => api.deleteRepo(repoId),
    onSuccess: () => {
      setDeleteOpen(false)
      // 删除是"先删记录、后台回收资源"：记录此刻已经不存在，只刷新列表。
      // 这里**不能**再 refresh() 详情 —— 会立刻 refetch 一个已删除的库并闪出 404 错误。
      queryClient.removeQueries({ queryKey: ['repo', repoId] })
      void queryClient.invalidateQueries({ queryKey: ['repos'] })
      // 删除会释放这块库占用的配额：列表页的顶部总账必须重新拉。
      void queryClient.invalidateQueries({ queryKey: ['me'] })
      onBack()
    },
    onError: (err) => setError(err)
  })

  const parentMutation = useMutation({
    mutationFn: (action: ParentAction) => api.parentAction(repoId, action),
    onSuccess: (result) => {
      // 退出维护 / 撤销共享：服务端返回 202 任务（拆目标很慢，不能同步等）。
      if ('type' in result) {
        setParentJob(result)
        // 任务列表页也要能看到这条任务。
        void queryClient.invalidateQueries({ queryKey: ['jobs'] })
        return
      }
      refresh()
    },
    onError: (err) => setError(err)
  })

  const cleanupMutation = useMutation({
    mutationFn: () => api.cleanupDiffs(repoId),
    onSuccess: (job) => {
      setCleanupJob(job)
      // 任务列表页也要能看到这条清理任务。
      void queryClient.invalidateQueries({ queryKey: ['jobs'] })
    },
    onError: (err) => setError(err)
  })

  // 清理任务进度：走服务端事件推送（见 useJobProgress），到终态再刷新库数据。
  const cleanupProgress = useJobProgress(cleanupJob?.id, cleanupJob)
  const cleanupRunning = cleanupJob !== null && !isJobTerminal(cleanupProgress?.state)
  const cleanupState = cleanupProgress?.state

  useEffect(() => {
    if (!cleanupJob || !isJobTerminal(cleanupState)) return
    if (cleanupState === 'succeeded') {
      message.success(t('repo.maintenance.cleanupDone'))
    } else {
      message.error(t('repo.maintenance.cleanupFailed'))
    }
    setCleanupJob(null)
    refresh()
    // refresh 是本组件内的稳定动作（只读 queryClient），不需要进依赖。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [cleanupJob, cleanupState, t])

  // 退出维护 / 撤销共享任务进度：与清理差异盘同一套（事件推送 + 终态刷新）。
  const parentProgress = useJobProgress(parentJob?.id, parentJob)
  const parentRunning = parentJob !== null && !isJobTerminal(parentProgress?.state)

  // 取消任务（卡住时的唯一出口）：parent_teardown / cleanup_diffs 都能从详情页直接取消。
  const cancelJobMutation = useMutation({
    mutationFn: (id: string) => api.cancelJob(id),
    onSuccess: (_job, id) => {
      message.success(t('job.cancelDone'))
      if (parentJob?.id === id) setParentJob(null)
      if (cleanupJob?.id === id) setCleanupJob(null)
      refresh()
    },
    onError: (err) => setError(err)
  })

  useEffect(() => {
    if (!parentJob || !isJobTerminal(parentProgress?.state)) return
    if (parentProgress?.state === 'succeeded') {
      message.success(t('repo.parent.teardownDone'))
    } else {
      message.error(t('repo.parent.teardownFailed'))
    }
    setParentJob(null)
    refresh()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [parentJob, parentProgress?.state, t])

  // 维护态的母盘文件管理改由服务端本地挂载 + 文件 API 完成（见 RepoParentFiles），
  // 不再把母盘通过 iSCSI 挂到客户端，因此这里没有 @temp 分配/挂载逻辑。

  const resizeMutation = useMutation({
    mutationFn: (sizeBytes: number) => api.resizeParentDisk(repoId, sizeBytes),
    onSuccess: () => {
      setResizeOpen(false)
      refresh()
    },
    onError: (err) => setError(err)
  })

  const copyMutation = useMutation({
    mutationFn: (newName: string) => api.copyRepo(repoId, newName),
    onSuccess: () => {
      setCopyOpen(false)
      void queryClient.invalidateQueries({ queryKey: ['jobs'] })
    },
    onError: (err) => setError(err)
  })

  const moveMutation = useMutation({
    mutationFn: (storageID: string) => api.moveRepo(repoId, storageID),
    onSuccess: (job) => {
      setMoveOpen(false)
      setMoveStorageID('')
      setMoveJob(job)
      void queryClient.invalidateQueries({ queryKey: ['jobs'] })
      refresh()
    },
    onError: (err) => setError(err)
  })

  const cancelMoveMutation = useMutation({
    mutationFn: () => api.cancelRepoMove(repoId),
    onSuccess: () => {
      message.success(t('repo.move.canceled'))
      refresh()
    },
    onError: (err) => setError(err)
  })

  // 迁移进度：进入详情时如果已经在迁移，靠 meta.move.job_id 接着跟；本次发起的任务用返回的 JobDTO。
  const moveProgress = useJobProgress(repoQuery.data?.move?.job_id ?? moveJob?.id, moveJob)
  useEffect(() => {
    if (!moveJob || !isJobTerminal(moveProgress?.state)) return
    if (moveProgress?.state === 'succeeded') message.success(t('repo.move.done'))
    else if (moveProgress?.state === 'cancelled') message.info(t('repo.move.canceled'))
    else message.error(t('repo.move.failed'))
    setMoveJob(null)
    refresh()
    // refresh 只读 queryClient，不需要进依赖。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [moveJob, moveProgress?.state, t])

  // 迁移期间 meta.move 的字节进度由任务持续写库：这里按固定节拍重取详情，
  // 让进度条和阶段文案不需要等任务终态才更新。离开页面即停。
  const repoMoving = repoQuery.data?.state === 'moving'
  const refetchRepo = repoQuery.refetch
  useEffect(() => {
    if (!repoMoving) return
    const timer = window.setInterval(() => void refetchRepo(), 3000)
    return () => window.clearInterval(timer)
  }, [repoMoving, refetchRepo])

  if (repoQuery.isLoading) return <LoadingState text={t('common.loading')} />

  const repo = repoQuery.data
  // 这个库我现在能不能管理：真源是服务端下发的权限等级（manage = 超管 / 库所有者 / 被授予
  // manage 的成员，见 docs/implementation.md 9.3）。老服务端不带这个字段时回退到"超管或库所有者"
  // —— 宁可少展示管理入口，也不能把它摆给只能看的成员：非库所有者点进详情页只能看，
  // 按钮却全都亮着，点下去只会撞 403。
  const canManage = repo
    ? repo.permission
      ? repo.permission === 'manage'
      : isSuperAdmin || repo.owner_id === currentUserId
    : isSuperAdmin
  // 用量口径与卡片一致（分母 = 库容量，见 repoUsage）。本机挂着这个库时用本机**实测**的
  // 卷用量：服务端的 used_bytes 是已分配磁盘总量（按盘标称容量计费，等于库容量）。
  const mountUsage = pickMountUsage(agent.usage, repo?.id)
  const {
    percent: usagePercent,
    usedText: usageUsedText,
    totalText: usageTotalText,
    measured: usageMeasured
  } = repoUsage(repo, mountUsage, !isSuperAdmin)
  // 未挂载时客户端不报用量、条置灰（见 RepoCards 的 RepoUsage）：那一刻服务端回退值是
  // 等于库容量的"已分配容量"，用量写 `-`；上限照报，仍是"已用 / 上限"那一行文案。
  // 管理端是例外：运维看的正是"这个库分配出去多大"，那个绝对数在这里是有效信息（与表格同口径）。
  const usageMuted = !isSuperAdmin && !usageMeasured
  // 刚挂上、实测用量还在取数：把 `-` 与条都换成转圈（见 useAgent 的 repoUsageLoading）。
  // 只在客户端那条路上成立 —— 管理端显示的是服务端口径，本机实测值到位后只是换个口径，
  // 不该拿转圈盖住一个已经有效的数字。
  const usageLoading = !isSuperAdmin && repoUsageLoading(agent.usagePending, repo?.id, usageMeasured)

  const openEdit = (target: RepoDTO): void => {
    editForm.setFieldsValue({
      name: target.name,
      group: target.group,
      max_diff_disks: target.max_diff_disks,
      note: target.note
    })
    setError(null)
    setEditOpen(true)
  }

  const isMaintenance = repo?.parent_condition === 'maintenance'
  // 刚发起的那一瞬间 repo 快照可能还是 active：以手头的任务状态补上，界面不要有一帧"看起来没反应"。
  const isMoving =
    repo?.state === 'moving' || (moveJob !== null && !isJobTerminal(moveProgress?.state))
  const movePercent =
    repo?.move && repo.move.total_bytes > 0
      ? Math.min(100, Math.round((repo.move.bytes / repo.move.total_bytes) * 100))
      : repo?.move && repo.move.files_total > 0
        ? Math.min(100, Math.round((repo.move.files_done / repo.move.files_total) * 100))
        : (moveProgress?.progress ?? 0)
  const targetStorages = (storagesQuery.data?.items ?? []).filter(
    (storage) => storage.enabled && storage.mounted && storage.id !== repo?.storage_id
  )
  const diffCount = (disksQuery.data?.items ?? []).filter((disk) => disk.kind === 'diff').length
  const parentFilesReady = isMaintenance && canManage && diffCount === 0
  const toggleMaintenance = (): void => {
    setError(null)
    parentMutation.mutate(isMaintenance ? 'finish_maintenance' : 'maintenance')
  }

  return (
    <PageShell
      title={repo?.name ?? t('repo.detail.title')}
      leading={<Button icon={<ArrowLeftOutlined />} onClick={goBack}>{t('repo.detail.back')}</Button>}
      extra={
        <>
          {/* 与列表卡片同一套"挂载/卸载"联动按钮：进了详情也不必退回列表才能挂载。
              库记录可能还没查回来（加载中/出错），此时不渲染。 */}
          {/* read 成员只能看：挂载/卸载后端都不放行，按钮就不摆出来。 */}
          {repo && repo.permission !== 'read' && !isMoving ? (
            <RepoDetailMountActions repo={repo} currentUserId={currentUserId} />
          ) : null}
          {/* 管理类操作只对能管理这个库的人展示（非库所有者点详情只能看）。 */}
          {canManage ? (
            <>
              <Button onClick={() => repo && openEdit(repo)} disabled={!repo || isMoving}>
                {t('common.edit')}
              </Button>
              {/* 进入 / 退出维护态合成一个按钮：文案与动作按当前母盘条件切换。 */}
              {repo?.mode === 'shared' ? (
                <Button
                  loading={parentMutation.isPending || parentRunning}
                  disabled={repo.parent_condition === 'temp_shared' || parentRunning || isMoving}
                  title={repo.parent_condition === 'temp_shared' ? t('repo.maintenance.unshareFirst') : undefined}
                  onClick={toggleMaintenance}
                >
                  {t(isMaintenance ? 'repo.parent.action.finish_maintenance' : 'repo.parent.action.maintenance')}
                </Button>
              ) : null}
              {/* 退出维护/撤销共享是异步任务：进度 + 取消按钮就摆在这里，卡住时不用去别处找。
                  取消仅超管可用（服务端同样只放行超管）。 */}
              {parentRunning ? (
                <Space>
                  <Progress percent={parentProgress?.progress ?? 0} size="small" style={{ width: 160 }} />
                  {isSuperAdmin ? (
                    <Button
                      size="small"
                      danger
                      loading={cancelJobMutation.isPending}
                      onClick={() => cancelJobMutation.mutate(parentJob!.id)}
                    >
                      {t('job.cancel')}
                    </Button>
                  ) : null}
                </Space>
              ) : null}
              {repo?.mode === 'shared' && repo?.parent_disk_id ? (
                <Button onClick={() => setVerifyOpen(true)} disabled={isMoving}>
                  {t('repo.verifyDiff.button')}
                </Button>
              ) : null}
              <Button
                disabled={isMoving}
                onClick={() => {
                  copyForm.setFieldsValue({ new_repo_name: `${repo?.name ?? 'repo'}-copy` })
                  setCopyOpen(true)
                }}
              >
                {t('action.copy')}
              </Button>
              {/* 迁移放在「复制」之后：两者都是整库级动作，但迁移会移动原库本身。 */}
              <Button
                disabled={
                  !repo ||
                  repo.state !== 'active' ||
                  (repo.mode === 'shared' && (repo.parent_condition ?? 'idle') !== 'idle')
                }
                onClick={() => {
                  setMoveStorageID('')
                  setError(null)
                  setMoveOpen(true)
                }}
              >
                {t('repo.move.button')}
              </Button>
              <Button danger disabled={isMoving} onClick={() => setDeleteOpen(true)}>
                {t('common.delete')}
              </Button>
            </>
          ) : null}
        </>
      }
    >
      {error ? <ErrorNotice error={error} /> : null}
      {repoQuery.error ? <ErrorNotice error={repoQuery.error} /> : null}
      {/* 只能看的库（非所有者 / read、mount 成员）说清楚为什么没有管理入口。 */}
      {repo && !canManage ? (
        <Alert type="info" showIcon message={t('repo.detail.readOnly')} style={{ marginBottom: spacing.md }} />
      ) : null}
      {/* 迁移横幅：复制期间库不可用，进度与中止入口都放在这里。 */}
      {isMoving ? (
        <Alert
          type="info"
          showIcon
          style={{ marginBottom: spacing.md }}
          message={t('repo.move.banner')}
          description={
            <Space direction="vertical" size={spacing.sm} style={{ width: '100%' }}>
              <span>
                {t('repo.move.bannerHint', {
                  from: repo?.move?.from_storage_name || '-',
                  to: repo?.move?.to_storage_name || '-'
                })}
              </span>
              <Progress percent={movePercent} size="small" status="active" />
              <Space wrap size={spacing.md}>
                <span>
                  {t('repo.move.bytes', {
                    done: formatBytes(repo?.move?.bytes ?? 0),
                    total: formatBytes(repo?.move?.total_bytes ?? 0)
                  })}
                </span>
                <span>
                  {t('repo.move.files', {
                    done: String(repo?.move?.files_done ?? 0),
                    total: String(repo?.move?.files_total ?? 0)
                  })}
                </span>
                {canManage ? (
                  <Button
                    size="small"
                    danger
                    loading={cancelMoveMutation.isPending}
                    onClick={() => cancelMoveMutation.mutate()}
                  >
                    {t('repo.move.cancel')}
                  </Button>
                ) : null}
              </Space>
            </Space>
          }
        />
      ) : null}
      {/* 维护态横幅：所有能看到这个库的人都该知道它当前不可用；清理入口只给能管理的人。 */}
      {isMaintenance ? (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: spacing.md }}
          message={t('repo.maintenance.banner')}
          description={
            <Space direction="vertical" size={spacing.sm} style={{ width: '100%' }}>
              <span>{t('repo.maintenance.bannerHint')}</span>
              {isMaintenance && canManage && diffCount > 0 ? (
                <span>{t('repo.parentFiles.needCleanup', { count: diffCount })}</span>
              ) : null}
              {canManage ? (
                <Space wrap>
                  <Button
                    size="small"
                    onClick={() => {
                      const currentGb = Math.ceil((repo?.capacity_bytes ?? 0) / (1024 * 1024 * 1024))
                      resizeForm.setFieldsValue({ size_gb: currentGb + 1 })
                      setResizeOpen(true)
                    }}
                  >
                    {t('repo.maintenance.resize')}
                  </Button>
                  {cleanupRunning ? (
                    <Space>
                      <Progress percent={cleanupProgress?.progress ?? 0} size="small" style={{ width: 220 }} />
                      <span>{t('repo.maintenance.cleanupRunning')}</span>
                      {isSuperAdmin ? (
                        <Button
                          size="small"
                          danger
                          loading={cancelJobMutation.isPending}
                          onClick={() => cancelJobMutation.mutate(cleanupJob!.id)}
                        >
                          {t('job.cancel')}
                        </Button>
                      ) : null}
                    </Space>
                  ) : (
                    <Button danger size="small" loading={cleanupMutation.isPending} onClick={() => cleanupMutation.mutate()}>
                      {t('repo.maintenance.cleanup')}
                    </Button>
                  )}
                </Space>
              ) : null}
            </Space>
          }
        />
      ) : null}

      <Tabs
        // 用户端默认落在「设置」：卡片「详情」进来看的就是"这个库在本机怎么挂"。
        // 管理端没有挂载配置（配置存在本机），保持原来的「概览」默认。
        defaultActiveKey={isSuperAdmin ? 'overview' : 'settings'}
        items={[
          ...(isSuperAdmin
            ? []
            : [
                {
                  key: 'settings',
                  label: t('repo.detail.settings'),
                  // 库记录还没到（加载中/出错）时先占位：不能因为详情没回来就少一个默认 tab。
                  children: repo ? <RepoMountSettingsPanel repo={repo} /> : <LoadingState />
                }
              ]),
          {
            key: 'overview',
            label: t('repo.detail.overview'),
            children: (
              <SectionCard title={t('repo.detail.overview')}>
                <Descriptions size="small" column={2} bordered>
                  <Descriptions.Item label={t('common.id')}>{repo?.id ?? '-'}</Descriptions.Item>
                  <Descriptions.Item label={t('field.mode')}>{repo ? repoModeLabel(repo.mode) : '-'}</Descriptions.Item>
                  <Descriptions.Item label={t('field.group')}>{repo?.group || '-'}</Descriptions.Item>
                  <Descriptions.Item label={t('field.note')} span={2}>
                    {repo?.note || '-'}
                  </Descriptions.Item>
                  <Descriptions.Item label={t('common.status')}>
                    {repo ? <StatusTag group="repo" value={repo.state} /> : '-'}
                  </Descriptions.Item>
                  <Descriptions.Item label={t('repo.parentCondition')}>
                    {repo ? parentConditionLabel(repo.parent_condition) : '-'}
                  </Descriptions.Item>
                  <Descriptions.Item label={t('repo.parentVersion')}>{repo?.parent_version ?? '-'}</Descriptions.Item>
                  {/* 库**没有配额**：库只有"容量"（建库时设定），这个容量就是它占用的配额
                      （计入用户已用配额）。所以这里只展示占用了多少，不再提供配额输入。
                      口径与卡片、管理端表格一致（分母 = 库容量，见 repoUsage）。 */}
                  <Descriptions.Item label={t('repo.used')}>
                    <div style={{ minWidth: 160 }}>
                      <div>
                        <UsageValueText usedText={usageUsedText} totalText={usageTotalText} loading={usageLoading} />
                      </div>
                      <UsageBar percent={usagePercent} muted={usageMuted} loading={usageLoading} />
                    </div>
                  </Descriptions.Item>
                  <Descriptions.Item label={t('field.maxDiffDisks')}>{repo?.max_diff_disks ?? '-'}</Descriptions.Item>
                  <Descriptions.Item label={t('common.createdAt')}>{formatTime(repo?.created_at)}</Descriptions.Item>
                </Descriptions>
              </SectionCard>
            )
          },
          // 可管理成员是管理信息：只有能管这个库的人才看得到这一页。
          ...(canManage
            ? [
                {
                  key: 'members',
                  label: t('repo.detail.members'),
                  children: <RepoMembers repoId={repoId} isSuperAdmin={isSuperAdmin} />
                }
              ]
            : []),
          // 维护态下直接管理母盘文件：服务端本地挂载 + 文件 API（不再挂到客户端）。
          // 只有**已无差异盘**时才开放：有差异盘时改母盘会让它们基于旧基线。
          ...(parentFilesReady
            ? [
                {
                  key: 'parentFiles',
                  label: t('repo.parentFiles.tab'),
                  children: <RepoParentFiles repoId={repoId} />
                }
              ]
            : []),
          {
            key: 'disks',
            label: t('repo.detail.disks'),
            children: (
              <RepoDisks
                repoId={repoId}
                isSuperAdmin={isSuperAdmin}
                canManage={canManage}
                currentUserId={currentUserId}
                currentUserName={currentUserName}
              />
            )
          }
        ]}
      />

      <Modal
        open={editOpen}
        title={t('repo.edit.title')}
        width={520}
        centered
        okText={t('common.save')}
        cancelText={t('common.cancel')}
        confirmLoading={updateMutation.isPending}
        onCancel={() => setEditOpen(false)}
        onOk={() => {
          void editForm.validateFields().then((values) => {
            updateMutation.mutate(values)
          })
        }}
      >
        <Form form={editForm} layout="vertical">
          <Form.Item name="name" label={t('field.name')} rules={[{ required: true }]}>
            <Input maxLength={64} />
          </Form.Item>
          <Form.Item name="group" label={t('field.group')}>
            <RepoGroupSelect />
          </Form.Item>
          {/* 备注写在母盘旁边的元数据文件里跟着盘走：换服务端把 disks/ 拷过去后，
              导入页面就能直接告诉管理员"这是财务的 Win10 母盘，别删"。 */}
          <Form.Item name="note" label={t('field.note')}>
            <Input.TextArea maxLength={512} rows={2} showCount />
          </Form.Item>
          <Form.Item name="max_diff_disks" label={t('field.maxDiffDisks')}>
            <InputNumber min={0} style={{ width: '100%' }} />
          </Form.Item>
        </Form>
      </Modal>

      <Modal
        open={copyOpen}
        title={t('repo.copy.title')}
        width={520}
        centered
        okText={t('common.confirm')}
        cancelText={t('common.cancel')}
        confirmLoading={copyMutation.isPending}
        onCancel={() => setCopyOpen(false)}
        onOk={() => {
          void copyForm.validateFields().then((values) => copyMutation.mutate(values.new_repo_name))
        }}
      >
        <Space direction="vertical" style={{ width: '100%' }}>
          <Alert type="info" showIcon message={t('repo.copy.desc')} />
          <Form form={copyForm} layout="vertical">
            <Form.Item name="new_repo_name" label={t('field.newName')} rules={[{ required: true }]}>
              <Input maxLength={64} />
            </Form.Item>
          </Form>
        </Space>
      </Modal>

      <Modal
        open={moveOpen}
        title={t('repo.move.title')}
        width={520}
        centered
        okText={t('common.confirm')}
        cancelText={t('common.cancel')}
        confirmLoading={moveMutation.isPending}
        okButtonProps={{ disabled: !moveStorageID || targetStorages.length === 0 }}
        onCancel={() => setMoveOpen(false)}
        onOk={() => {
          if (!moveStorageID) {
            message.warning(t('repo.move.needTarget'))
            return
          }
          moveMutation.mutate(moveStorageID)
        }}
      >
        <Space direction="vertical" size={spacing.md} style={{ width: '100%' }}>
          <Alert type="warning" showIcon message={t('repo.move.desc')} />
          {repo?.storage_name ? (
            <Space>
              <span>{t('repo.move.current')}</span>
              <strong>{repo.storage_name}</strong>
            </Space>
          ) : null}
          {targetStorages.length === 0 ? (
            <Alert type="info" showIcon message={t('repo.move.noTargets')} />
          ) : (
            <Select
              style={{ width: '100%' }}
              placeholder={t('repo.move.targetPlaceholder')}
              value={moveStorageID || undefined}
              onChange={setMoveStorageID}
              loading={storagesQuery.isLoading}
              options={targetStorages.map((storage) => ({
                value: storage.id,
                label: `${storage.name} · ${storage.path}`
              }))}
            />
          )}
        </Space>
      </Modal>

      <ConfirmDialog
        open={deleteOpen}
        danger
        title={t('common.delete')}
        content={t('repo.delete.confirm', { name: repo?.name ?? '' })}
        loading={deleteMutation.isPending}
        onConfirm={() => deleteMutation.mutate()}
        onCancel={() => setDeleteOpen(false)}
      />

      <Modal
        open={resizeOpen}
        title={t('repo.maintenance.resizeTitle')}
        width={480}
        centered
        okText={t('common.confirm')}
        cancelText={t('common.cancel')}
        confirmLoading={resizeMutation.isPending}
        onCancel={() => setResizeOpen(false)}
        onOk={() => {
          void resizeForm.validateFields().then((values) => {
            resizeMutation.mutate(Math.round(values.size_gb * 1024 * 1024 * 1024))
          })
        }}
      >
        <Space direction="vertical" style={{ width: '100%' }}>
          <Alert type="info" showIcon message={t('repo.maintenance.resizeHint')} />
          <Form form={resizeForm} layout="vertical">
            <Form.Item name="size_gb" label={t('repo.maintenance.resizeSize')} rules={[{ required: true }]}>
              <InputNumber
                min={Math.ceil((repo?.capacity_bytes ?? 0) / (1024 * 1024 * 1024)) + 1}
                step={1}
                style={{ width: '100%' }}
                addonAfter="GB"
              />
            </Form.Item>
          </Form>
        </Space>
      </Modal>

      <RepoVerifyDiff repoId={repoId} open={verifyOpen} onClose={() => setVerifyOpen(false)} />
    </PageShell>
  )
}

/**
 * 详情页标题栏的挂载/卸载按钮：直接复用列表卡片的控制器与组件，行为完全一致
 * （无分配时由库主按需自建分配，再交给本机代理挂载）。
 *
 * 单独抽成一个组件，是因为 `useRepoMount` 需要一份完整的库记录，而详情页的库是异步查出来的
 * ——在父组件里调用会踩到 "hooks 必须在提前 return 之前" 的限制。
 *
 * 尺寸取中号：这一排（返回/编辑/母盘操作/复制/删除）都是 antd 默认尺寸，挂载按钮必须同款
 * （真实反馈："存储库详情里的挂载按钮要和其他按钮一样大"），卡片里的小号尺寸是紧凑排布才用的。
 *
 * 样式取描边：这一排邻居清一色是描边按钮，挂载按钮若还是卡片的实心主色，尺寸虽同、视觉重量
 * 却压过整排（真实反馈："看起来比别的按钮大了，我要统一"）。实心留给卡片——那里它是唯一的主操作。
 */
function RepoDetailMountActions({ repo, currentUserId }: { repo: RepoDTO; currentUserId: string }): JSX.Element {
  const controller = useRepoMount(repo, currentUserId)
  return <RepoMountActions controller={controller} size="middle" variant="outlined" />
}
