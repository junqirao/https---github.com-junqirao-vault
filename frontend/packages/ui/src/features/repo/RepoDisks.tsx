import { useEffect, useMemo, useState } from 'react'
import { Alert, Button, Form, Input, Modal, Progress, Select, Space, Tag, Tooltip, Typography } from 'antd'
import type { TableColumnsType } from 'antd'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { ConfirmDialog } from '../../components/ConfirmDialog'
import { DataTable } from '../../components/DataTable'
import { ErrorNotice } from '../../components/ErrorNotice'
import { SectionCard } from '../../components/SectionCard'
import { StatusTag } from '../../components/StatusTag'
import { agentApi } from '../../api/agentClient'
import type { AgentMountMode, AgentMountState, DownloadState } from '../../api/agentTypes'
import { ApiError } from '../../api/errors'
import { useApi } from '../../api/provider'
import type { AllocationDTO, AllocationState, DiskDTO } from '../../api/types'
import { useAgent } from '../../hooks/useAgent'
import { useServerConfig } from '../../hooks/useServerConfig'
import { useI18n } from '../../i18n'
import { palette, spacing } from '../../tokens/palette'
import { formatBytes, formatTime } from '../../utils/format'
import { diskKindLabel } from '../../utils/labels'
import { MountActionButtons, MountDialog, mountSessionLive } from '../mount/MountControls'

export interface RepoDisksProps {
  repoId: string
  isSuperAdmin: boolean
  currentUserId: string
  /** 当前用户的显示名：非管理员无法拉取用户列表，用于自己的分配列回退显示。 */
  currentUserName: string
}

/**
 * 分配状态 → 挂载语义（未挂载 / 挂载中 / 已挂载 / 释放中）。
 *
 * 磁盘的 `mounted` 字段是**服务端本机挂载**标记，客户端把自己的盘挂到本机后不会回写它，
 * 因此永远显示"未挂载"。真正能反映"谁挂载了"的是分配状态（代理挂载成功后会回写为 mounted）。
 */
const MOUNT_STATE_META: Record<AllocationState, { labelKey: string; color?: string }> = {
  allocated: { labelKey: 'repo.mount.notMounted' },
  mounting: { labelKey: 'state.mount.mounting', color: palette.accent },
  mounted: { labelKey: 'repo.mount.mounted', color: palette.success },
  releasing: { labelKey: 'state.allocation.releasing', color: palette.warning },
  released: { labelKey: 'state.allocation.released' }
}

/** 磁盘的挂载状态：由该盘上的分配状态聚合得出（一条挂上即视为已挂载）。 */
function diskMountState(allocs: AllocationDTO[]): AllocationState | null {
  if (allocs.length === 0) return null
  if (allocs.some((alloc) => alloc.state === 'mounted')) return 'mounted'
  if (allocs.some((alloc) => alloc.state === 'mounting')) return 'mounting'
  if (allocs.some((alloc) => alloc.state === 'releasing')) return 'releasing'
  return 'allocated'
}

/** 存储库下的磁盘列表：磁盘 + 分配（谁在用/挂载）+ 挂载与卸载 + 回收空间与异步删除。 */
export function RepoDisks({ repoId, isSuperAdmin, currentUserId, currentUserName }: RepoDisksProps): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const [error, setError] = useState<unknown>(null)
  const [pendingDelete, setPendingDelete] = useState<DiskDTO | null>(null)
  const agent = useAgent()
  const { active } = useServerConfig()
  const [copyDisk, setCopyDisk] = useState<DiskDTO | null>(null)
  const [copyForm] = Form.useForm<{ target_dir: string; file_name: string }>()
  const { config: agentConfig, downloads, refreshDownloads } = agent

  // 分配（用户 + 差异盘）与挂载操作（分配/释放/挂载/卸载）合并在此面板。
  const [selectedUser, setSelectedUser] = useState<string | undefined>(undefined)
  const [pendingRelease, setPendingRelease] = useState<AllocationDTO | null>(null)
  const [mountTarget, setMountTarget] = useState<AllocationDTO | null>(null)
  const [mountMode, setMountMode] = useState<AgentMountMode>('letter')
  const [mountPath, setMountPath] = useState('')
  const [busyId, setBusyId] = useState<string | null>(null)
  const [pendingUnmount, setPendingUnmount] = useState<AgentMountState | null>(null)

  // 刷新页面后恢复进行中/历史下载；属背景恢复，失败不阻断页面（用户主动操作失败时会另行提示）。
  useEffect(() => {
    void refreshDownloads().catch(() => undefined)
  }, [refreshDownloads])

  const disksQuery = useQuery({
    queryKey: ['repo-disks', repoId],
    queryFn: () => api.listRepoDisks(repoId)
  })

  const allocationsQuery = useQuery({
    queryKey: ['repo-allocations', repoId],
    queryFn: () => api.listAllocations(repoId)
  })

  const usersQuery = useQuery({
    queryKey: ['users', 'options'],
    queryFn: () => api.listUsers({ limit: 200 }),
    enabled: isSuperAdmin
  })

  const userName = useMemo(() => {
    const map = new Map<string, string>()
    // 非管理员拿不到用户列表，至少让自己那条显示成用户名而不是 UUID。
    if (currentUserName) map.set(currentUserId, currentUserName)
    for (const user of usersQuery.data?.items ?? []) map.set(user.id, user.username)
    return map
  }, [usersQuery.data, currentUserId, currentUserName])

  /** 磁盘 → 该盘上的活跃分配（差异盘一对一；独享库多用户共用同一盘）。 */
  const allocationsByDisk = useMemo(() => {
    const map = new Map<string, AllocationDTO[]>()
    for (const alloc of allocationsQuery.data?.items ?? []) {
      if (alloc.state === 'released') continue
      const list = map.get(alloc.disk_id) ?? []
      list.push(alloc)
      map.set(alloc.disk_id, list)
    }
    return map
  }, [allocationsQuery.data])

  /** 分配 → 本机代理的挂载状态（只包含本客户端自己的挂载）。 */
  const mountByAllocation = useMemo(() => {
    const map = new Map<string, AgentMountState>()
    for (const mount of agent.state?.mounts ?? []) map.set(mount.allocation_id, mount)
    return map
  }, [agent.state])

  const invalidate = (): void => {
    void queryClient.invalidateQueries({ queryKey: ['repo-disks', repoId] })
    void queryClient.invalidateQueries({ queryKey: ['repo-allocations', repoId] })
    void queryClient.invalidateQueries({ queryKey: ['leases'] })
    void queryClient.invalidateQueries({ queryKey: ['jobs'] })
  }

  const compactMutation = useMutation({
    mutationFn: (diskId: string) => api.compactDisk(diskId),
    onSuccess: invalidate,
    onError: (err) => setError(err)
  })

  const deleteMutation = useMutation({
    mutationFn: (diskId: string) => api.deleteDisk(diskId),
    onSuccess: () => {
      setPendingDelete(null)
      invalidate()
    },
    onError: (err) => setError(err)
  })

  const downloadMutation = useMutation({
    mutationFn: (input: { disk_id: string; target_dir: string; file_name: string }) =>
      agentApi.startDiskDownload(input),
    onSuccess: () => {
      setCopyDisk(null)
      void refreshDownloads().catch(() => undefined)
    },
    onError: (err) => setError(err)
  })

  const cancelMutation = useMutation({
    mutationFn: (diskId: string) => agentApi.cancelDownload(diskId),
    onSuccess: () => {
      void refreshDownloads().catch(() => undefined)
    },
    onError: (err) => setError(err)
  })

  const allocateMutation = useMutation({
    mutationFn: (userId: string) => api.allocate(repoId, userId),
    onSuccess: () => {
      setSelectedUser(undefined)
      invalidate()
    },
    onError: (err) => setError(err)
  })

  const releaseMutation = useMutation({
    mutationFn: (allocationId: string) => api.releaseAllocation(allocationId),
    onSuccess: () => {
      setPendingRelease(null)
      invalidate()
    },
    onError: (err) => setError(err)
  })

  /** 代理操作统一包一层：记录错误与忙状态，成功后刷新分配（分配状态随挂载变化）避免重复代码。 */
  const runAgentAction = async (allocationId: string, action: () => Promise<unknown>): Promise<void> => {
    setError(null)
    setBusyId(allocationId)
    try {
      await action()
      invalidate()
    } catch (err) {
      setError(err)
    } finally {
      setBusyId(null)
    }
  }

  // 目录名里的服务端名称，与代理侧同一口径：本地别名优先，其次服务端名称，最后 vault。
  const mountServerAlias = (agentConfig?.server_alias ?? '').trim() || (active?.serverName ?? '').trim() || 'vault'

  const openMountDialog = (allocation: AllocationDTO): void => {
    // 先预填**这个库自己的**挂载设置（存储库 → 挂载设置），没配过才落回代理的兜底默认值。
    // 挂载形态与目录已经只在库那边配置，从卡片点"挂载"时必须按它来 —— 否则用户刚在某库
    // 配好 D:\vault，点挂载却被这里的默认值顶成 C:\Vault（请求里的 mount_path 优先级更高）。
    const pref = agentConfig?.repo_mounts?.[repoId]
    setMountMode(pref?.mount_mode || agentConfig?.default_mount_mode || 'letter')
    setMountPath(pref?.mount_dir || agentConfig?.default_mount_dir || '')
    setError(null)
    setMountTarget(allocation)
  }

  /**
   * 卸载：一次调用即尽力清理干净（取消挂载点、下线、断开会话、取消持久化、回写释放），
   * 不再区分"普通卸载 / 强制卸载"（代理侧只有一个 unmount，见 mountEngine.unmount）。
   *
   * 只有一种失败：卷正被占用（挂载点还在），此时如实报错并保留记录 ——
   * 用户关掉占用它的程序再点一次即可，界面不再弹"要不要强制卸载"。
   */
  const runUnmount = async (mount: AgentMountState): Promise<void> => {
    setError(null)
    setBusyId(mount.allocation_id)
    try {
      await agent.unmount({ allocation_id: mount.allocation_id })
      invalidate()
    } catch (err) {
      setError(err)
    } finally {
      setBusyId(null)
      setPendingUnmount(null)
    }
  }

  const openCopy = (disk: DiskDTO): void => {
    setCopyDisk(disk)
    copyForm.setFieldsValue({
      // 默认取本地代理配置的 default_download_dir（useAgent 经 agentApi.getConfig 读取）。
      target_dir: agentConfig?.default_download_dir ?? '',
      file_name: baseName(disk.vhdx_path)
    })
  }

  const openFolder = (targetPath: string): void => {
    void agentApi.openPath(dirName(targetPath)).catch((err: unknown) => setError(err))
  }

  const columns: TableColumnsType<DiskDTO> = [
    { title: t('field.type'), dataIndex: 'kind', key: 'kind', width: 110, render: (value: string) => diskKindLabel(value) },
    {
      title: t('field.resource'),
      dataIndex: 'vhdx_path',
      key: 'vhdx_path',
      render: (value: string) => (
        <Tooltip title={value}>
          <Typography.Text type="secondary" ellipsis style={{ maxWidth: 300 }}>
            {value}
          </Typography.Text>
        </Tooltip>
      )
    },
    {
      title: t('common.size'),
      key: 'size',
      width: 170,
      render: (_value, disk) => `${formatBytes(disk.physical_bytes)} / ${formatBytes(disk.size_bytes)}`
    },
    {
      title: t('common.status'),
      dataIndex: 'state',
      key: 'state',
      width: 100,
      render: (value: string) => <StatusTag group="disk" value={value} />
    },
    {
      // 谁在用这块盘（差异盘即"谁挂载了"）：列出该盘上的分配用户。
      title: t('field.user'),
      key: 'user',
      width: 180,
      render: (_value, disk) => {
        const allocs = allocationsByDisk.get(disk.id) ?? []
        if (allocs.length === 0) return '-'
        return (
          <Space direction="vertical" size={0}>
            {allocs.map((alloc) => (
              <Typography.Text key={alloc.id} ellipsis style={{ maxWidth: 160, display: 'block' }}>
                {userName.get(alloc.user_id) ?? alloc.user_id}
              </Typography.Text>
            ))}
          </Space>
        )
      }
    },
    {
      title: t('repo.mount.state'),
      key: 'mount_state',
      width: 110,
      render: (_value, disk) => {
        const allocs = allocationsByDisk.get(disk.id) ?? []
        const state = diskMountState(allocs)
        if (!state) return '-'
        // 自己的记录若被代理**实测**已无活动会话：服务端分配状态仍是"已挂载"，但这块盘
        // 在本机实际已不在了，如实显示"已断开"（真实诉求：标签与按钮都以实际会话为准）。
        // 别人的分配无从得知对方的会话，只按服务端状态展示。
        const mine = allocs.find((alloc) => alloc.user_id === currentUserId)
        if (state === 'mounted' && mountSessionLive(mine ? mountByAllocation.get(mine.id) : undefined) === false) {
          return (
            <Tooltip title={t('agent.mount.sessionLost')}>
              <StatusTag group="mount" value="disconnected" />
            </Tooltip>
          )
        }
        const meta = MOUNT_STATE_META[state]
        return (
          <Tag bordered={false} color={meta.color}>
            {t(meta.labelKey)}
          </Tag>
        )
      }
    },
    {
      title: t('field.mountPoint'),
      key: 'mount_path',
      width: 180,
      render: (_value, disk) => {
        const mine = (allocationsByDisk.get(disk.id) ?? []).find((alloc) => alloc.user_id === currentUserId)
        const mineMount = mine ? mountByAllocation.get(mine.id) : undefined
        // 实测已无活动会话时挂载点早已随会话消失：再显示旧路径就是撒谎（点开只会报路径不存在）。
        const path = mountSessionLive(mineMount) === false ? undefined : mineMount?.mount_path
        if (!path) return '-'
        return (
          <Tooltip title={path}>
            <Typography.Text ellipsis style={{ maxWidth: 160 }}>
              {path}
            </Typography.Text>
          </Tooltip>
        )
      }
    },
    {
      title: t('common.createdAt'),
      dataIndex: 'created_at',
      key: 'created_at',
      width: 160,
      render: (value: number) => formatTime(value)
    },
    {
      title: t('common.actions'),
      key: 'actions',
      width: 320,
      render: (_value, disk) => {
        const allocs = allocationsByDisk.get(disk.id) ?? []
        const mine = allocs.find((alloc) => alloc.user_id === currentUserId)
        const mount = mine ? mountByAllocation.get(mine.id) : undefined
        return (
          <Space size={0} wrap>
            <MountActionButtons
              mount={mount}
              canMount={mine !== undefined}
              available={agent.available}
              busy={mine !== undefined && busyId === mine.id}
              onMount={() => mine && openMountDialog(mine)}
              onUnmount={() => mount && setPendingUnmount(mount)}
              onRemount={() => mine && void runAgentAction(mine.id, () => agent.remount({ allocation_id: mine.id }))}
              onOpenPath={(path) => void agentApi.openPath(path).catch((err: unknown) => setError(err))}
            />
            {allocs
              .filter((alloc) => isSuperAdmin || alloc.user_id === currentUserId)
              .map((alloc) => (
                <Button key={alloc.id} type="link" size="small" danger onClick={() => setPendingRelease(alloc)}>
                  {t('action.release')}
                </Button>
              ))}
            {disk.kind === 'parent' && mine === undefined ? (
              <Button type="link" size="small" onClick={() => openCopy(disk)}>
                {t('disk.copyToLocal')}
              </Button>
            ) : null}
            <Button
              type="link"
              size="small"
              onClick={() => compactMutation.mutate(disk.id)}
              loading={compactMutation.isPending && compactMutation.variables === disk.id}
            >
              {t('action.compact')}
            </Button>
            <Button type="link" size="small" danger onClick={() => setPendingDelete(disk)}>
              {t('common.delete')}
            </Button>
          </Space>
        )
      }
    }
  ]

  return (
    <SectionCard title={t('repo.detail.disks')}>
      {error || allocationsQuery.error ? <ErrorNotice error={error ?? allocationsQuery.error} /> : null}
      {!agent.available ? (
        <Alert type="warning" showIcon message={t('agent.hostMissing')} style={{ marginBottom: spacing.md }} />
      ) : null}
      {isSuperAdmin ? (
        <Space style={{ marginBottom: spacing.md }} wrap>
          <Select
            showSearch
            value={selectedUser}
            placeholder={t('repo.assign.user')}
            style={{ width: 260 }}
            loading={usersQuery.isLoading}
            onChange={setSelectedUser}
            optionFilterProp="label"
            options={(usersQuery.data?.items ?? []).map((user) => ({ value: user.id, label: user.username }))}
          />
          <Button
            type="primary"
            disabled={!selectedUser}
            loading={allocateMutation.isPending}
            onClick={() => selectedUser && allocateMutation.mutate(selectedUser)}
          >
            {t('action.assign')}
          </Button>
        </Space>
      ) : null}
      {downloads.length > 0 ? (
        <DownloadList
          downloads={downloads}
          cancellingId={cancelMutation.isPending ? cancelMutation.variables : undefined}
          onCancel={(diskId) => cancelMutation.mutate(diskId)}
          onOpenFolder={openFolder}
        />
      ) : null}
      <DataTable<DiskDTO>
        columns={columns}
        rows={disksQuery.data?.items ?? []}
        rowKey={(disk) => disk.id}
        loading={disksQuery.isLoading}
        empty={t('common.empty')}
        pagination={false}
        scroll={{ x: 1500 }}
      />
      <ConfirmDialog
        open={pendingDelete !== null}
        danger
        title={t('common.delete')}
        content={pendingDelete?.vhdx_path}
        loading={deleteMutation.isPending}
        onConfirm={() => pendingDelete && deleteMutation.mutate(pendingDelete.id)}
        onCancel={() => setPendingDelete(null)}
      />
      <ConfirmDialog
        open={pendingRelease !== null}
        danger
        title={t('action.release')}
        content={t('repo.release.confirm')}
        loading={releaseMutation.isPending}
        onConfirm={() => pendingRelease && releaseMutation.mutate(pendingRelease.id)}
        onCancel={() => setPendingRelease(null)}
      />
      <ConfirmDialog
        open={pendingUnmount !== null}
        danger
        title={t('action.unmount')}
        content={t('agent.unmount.confirm')}
        loading={busyId !== null}
        onConfirm={() => {
          if (pendingUnmount) void runUnmount(pendingUnmount)
        }}
        onCancel={() => setPendingUnmount(null)}
      />
      <Modal
        open={copyDisk !== null}
        title={t('disk.copyToLocal')}
        width={480}
        centered
        okText={t('disk.copy.start')}
        cancelText={t('common.cancel')}
        confirmLoading={downloadMutation.isPending}
        onCancel={() => setCopyDisk(null)}
        onOk={() => {
          if (!copyDisk) return
          void copyForm.validateFields().then((values) =>
            downloadMutation.mutate({
              disk_id: copyDisk.id,
              target_dir: values.target_dir,
              file_name: values.file_name
            })
          )
        }}
      >
        <Form form={copyForm} layout="vertical">
          <Form.Item
            name="target_dir"
            label={t('disk.copy.targetDir')}
            extra={t('disk.copy.dirHint')}
            rules={[{ required: true }]}
          >
            <Input />
          </Form.Item>
          <Form.Item name="file_name" label={t('disk.copy.fileName')} rules={[{ required: true }]}>
            <Input />
          </Form.Item>
        </Form>
      </Modal>

      <MountDialog
        open={mountTarget !== null}
        mode={mountMode}
        path={mountPath}
        serverAlias={mountServerAlias}
        submitting={mountTarget !== null && busyId === mountTarget.id}
        onModeChange={setMountMode}
        onPathChange={setMountPath}
        onConfirm={() => {
          if (!mountTarget) return
          const target = mountTarget
          void runAgentAction(target.id, () =>
            agent.mount({
              allocation_id: target.id,
              repo_id: repoId,
              mount_mode: mountMode,
              mount_path: mountMode === 'directory' ? mountPath.trim() : undefined
            })
          ).then(() => setMountTarget(null))
        }}
        onCancel={() => setMountTarget(null)}
      />
    </SectionCard>
  )
}

/** 取路径的文件名（兼容 Windows 反斜杠与 POSIX 斜杠）。 */
function baseName(path: string): string {
  const index = Math.max(path.lastIndexOf('\\'), path.lastIndexOf('/'))
  return index >= 0 ? path.slice(index + 1) : path
}

/** 取路径所在目录（兼容 Windows 反斜杠与 POSIX 斜杠）。 */
function dirName(path: string): string {
  const index = Math.max(path.lastIndexOf('\\'), path.lastIndexOf('/'))
  return index > 0 ? path.slice(0, index) : path
}

/** 下载任务紧凑展示：进行中显示进度与取消，完成后提供打开所在文件夹。 */
function DownloadList({
  downloads,
  cancellingId,
  onCancel,
  onOpenFolder
}: {
  downloads: DownloadState[]
  cancellingId?: string
  onCancel: (diskId: string) => void
  onOpenFolder: (targetPath: string) => void
}): JSX.Element {
  const { t } = useI18n()
  return (
    <Space direction="vertical" size={8} style={{ width: '100%', marginBottom: 16 }}>
      {downloads.map((download) => {
        const percent =
          download.total_bytes > 0
            ? Math.min(100, Math.round((download.received_bytes / download.total_bytes) * 100))
            : 0
        return (
          <div key={download.disk_id} style={{ width: '100%' }}>
            <Space size={8} wrap>
              <Typography.Text strong>{download.file_name || download.disk_id}</Typography.Text>
              {download.state === 'running' ? (
                <Progress percent={percent} size="small" status="active" style={{ width: 220 }} />
              ) : null}
              {download.state === 'running' ? (
                <Typography.Text type="secondary">
                  {`${formatBytes(download.received_bytes)} / ${formatBytes(download.total_bytes)}`}
                </Typography.Text>
              ) : null}
              {download.state === 'running' ? (
                <Button
                  type="link"
                  size="small"
                  danger
                  loading={cancellingId === download.disk_id}
                  onClick={() => onCancel(download.disk_id)}
                >
                  {t('disk.copy.cancel')}
                </Button>
              ) : null}
              {download.state === 'done' ? (
                <Typography.Text type="success">{t('disk.copy.done')}</Typography.Text>
              ) : null}
              {download.state === 'done' ? (
                <Button type="link" size="small" onClick={() => onOpenFolder(download.target_path)}>
                  {t('disk.copy.openFolder')}
                </Button>
              ) : null}
              {download.state === 'canceled' ? (
                <Typography.Text type="secondary">{t('disk.copy.canceled')}</Typography.Text>
              ) : null}
            </Space>
            <Typography.Text type="secondary" style={{ display: 'block', wordBreak: 'break-all' }}>
              {download.target_path}
            </Typography.Text>
            {download.state === 'failed' ? (
              <ErrorNotice error={new ApiError({ kind: 'business', code: download.error || 'system.internal' })} />
            ) : null}
          </div>
        )
      })}
    </Space>
  )
}
