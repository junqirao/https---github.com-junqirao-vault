import { useMemo, useState } from 'react'
import { Alert, Button, Select, Space, Tooltip, Typography } from 'antd'
import type { TableColumnsType } from 'antd'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { ConfirmDialog } from '../../components/ConfirmDialog'
import { DataTable } from '../../components/DataTable'
import { ErrorNotice } from '../../components/ErrorNotice'
import { SectionCard } from '../../components/SectionCard'
import { StatusTag } from '../../components/StatusTag'
import { agentApi } from '../../api/agentClient'
import { useApi } from '../../api/provider'
import type { AllocationDTO } from '../../api/types'
import type { AgentMountMode, AgentMountState } from '../../api/agentTypes'
import { useAgent } from '../../hooks/useAgent'
import { useI18n } from '../../i18n'
import { spacing } from '../../tokens/palette'
import { formatTime } from '../../utils/format'
import { MountActionButtons, MountDialog, MountStateCell } from '../mount/MountControls'
import { MountSpecDebug } from '../mount/MountSpecDebug'

export interface RepoAllocationsProps {
  repoId: string
  isSuperAdmin: boolean
  currentUserId: string
  /** 客户端稳定标识（由宿主应用生成并持久化，不使用 MAC/IP）。 */
  clientId: string
}

/** 分配面板：分配用户、释放分配、经本地代理挂载/卸载，并保留挂载参数调试入口。 */
export function RepoAllocations({
  repoId,
  isSuperAdmin,
  currentUserId,
  clientId
}: RepoAllocationsProps): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const agent = useAgent()
  const queryClient = useQueryClient()
  const [error, setError] = useState<unknown>(null)
  const [selectedUser, setSelectedUser] = useState<string | undefined>(undefined)
  const [pendingRelease, setPendingRelease] = useState<AllocationDTO | null>(null)
  const [mountTarget, setMountTarget] = useState<AllocationDTO | null>(null)
  const [mountMode, setMountMode] = useState<AgentMountMode>('letter')
  const [mountPath, setMountPath] = useState('')
  const [busyId, setBusyId] = useState<string | null>(null)

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
    for (const user of usersQuery.data?.items ?? []) map.set(user.id, user.username)
    return map
  }, [usersQuery.data])

  const mountByAllocation = useMemo(() => {
    const map = new Map<string, AgentMountState>()
    for (const mount of agent.state?.mounts ?? []) map.set(mount.allocation_id, mount)
    return map
  }, [agent.state])

  const invalidate = (): void => {
    void queryClient.invalidateQueries({ queryKey: ['repo-allocations', repoId] })
    void queryClient.invalidateQueries({ queryKey: ['leases'] })
  }

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

  /** 代理操作统一包一层：记录错误与忙状态，避免重复代码。 */
  const runAgentAction = async (allocationId: string, action: () => Promise<unknown>): Promise<void> => {
    setError(null)
    setBusyId(allocationId)
    try {
      await action()
    } catch (err) {
      setError(err)
    } finally {
      setBusyId(null)
    }
  }

  const openMountDialog = (allocation: AllocationDTO): void => {
    setMountMode(agent.config?.default_mount_mode ?? 'letter')
    setMountPath(agent.config?.default_mount_dir ?? '')
    setError(null)
    setMountTarget(allocation)
  }

  const columns: TableColumnsType<AllocationDTO> = [
    {
      title: t('field.user'),
      dataIndex: 'user_id',
      key: 'user_id',
      render: (value: string) => userName.get(value) ?? value
    },
    {
      title: t('field.state'),
      dataIndex: 'state',
      key: 'state',
      width: 110,
      render: (value: string) => <StatusTag group="allocation" value={value} />
    },
    {
      title: t('common.status'),
      key: 'mount_state',
      width: 130,
      render: (_value, allocation) => <MountStateCell mount={mountByAllocation.get(allocation.id)} />
    },
    {
      title: t('field.mountPoint'),
      key: 'mount_path',
      render: (_value, allocation) => {
        const path = mountByAllocation.get(allocation.id)?.mount_path
        if (!path) return '-'
        return (
          <Tooltip title={path}>
            <Typography.Text ellipsis style={{ maxWidth: 220 }}>
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
      width: 170,
      render: (value: number) => formatTime(value)
    },
    {
      title: t('common.actions'),
      key: 'actions',
      width: 300,
      render: (_value, allocation) => (
        <Space size={0}>
          <MountActionButtons
            mount={mountByAllocation.get(allocation.id)}
            canMount={allocation.user_id === currentUserId}
            available={agent.available}
            busy={busyId === allocation.id}
            onMount={() => openMountDialog(allocation)}
            onUnmount={() => void runAgentAction(allocation.id, () => agent.unmount({ allocation_id: allocation.id }))}
            onRemount={() => void runAgentAction(allocation.id, () => agent.remount({ allocation_id: allocation.id }))}
            onOpenPath={(path) => void runAgentAction(allocation.id, () => agentApi.openPath(path))}
          />
          <Button type="link" size="small" danger onClick={() => setPendingRelease(allocation)}>
            {t('action.release')}
          </Button>
        </Space>
      )
    }
  ]

  return (
    <SectionCard title={t('repo.detail.allocations')}>
      {error ? <ErrorNotice error={error} /> : null}
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
      <DataTable<AllocationDTO>
        columns={columns}
        rows={allocationsQuery.data?.items ?? []}
        rowKey={(allocation) => allocation.id}
        loading={allocationsQuery.isLoading}
        empty={t('common.empty')}
        pagination={false}
      />

      <MountSpecDebug allocations={allocationsQuery.data?.items ?? []} userName={userName} clientId={clientId} />

      <MountDialog
        open={mountTarget !== null}
        mode={mountMode}
        path={mountPath}
        submitting={mountTarget !== null && busyId === mountTarget.id}
        onModeChange={setMountMode}
        onPathChange={setMountPath}
        onConfirm={() => {
          if (!mountTarget) return
          const target = mountTarget
          void runAgentAction(target.id, () =>
            agent.mount({
              allocation_id: target.id,
              mount_mode: mountMode,
              mount_path: mountMode === 'directory' ? mountPath.trim() : undefined
            })
          ).then(() => setMountTarget(null))
        }}
        onCancel={() => setMountTarget(null)}
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
    </SectionCard>
  )
}
