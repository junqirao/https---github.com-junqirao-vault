import { useEffect, useMemo, useState } from 'react'
import { Alert, Button, Table, Tooltip, Typography } from 'antd'
import type { TableColumnsType } from 'antd'
import { useQuery } from '@tanstack/react-query'

import { ConfirmDialog } from '../../components/ConfirmDialog'
import { ErrorNotice } from '../../components/ErrorNotice'
import { PageShell } from '../../components/PageShell'
import { SectionCard } from '../../components/SectionCard'
import { agentApi } from '../../api/agentClient'
import { useApi } from '../../api/provider'
import type { AgentMountState } from '../../api/agentTypes'
import { useAgent } from '../../hooks/useAgent'
import { useI18n } from '../../i18n'
import { spacing } from '../../tokens/palette'
import { openTargetOf, MountActionButtons, MountStateCell } from './MountControls'

/** 心跳间隔未知，估算租约剩余时使用该兜底 TTL（秒）。 */
const FALLBACK_TTL_SECONDS = 60

function estimateRemaining(mount: AgentMountState, now: number): number | null {
  const base = mount.last_heartbeat_at ?? mount.mounted_at
  if (!base) return null
  return Math.max(0, Math.ceil((base + FALLBACK_TTL_SECONDS * 1000 - now) / 1000))
}

/** 我的挂载：来自本地代理的实时挂载状态，支持批量卸载。 */
export function MountList(): JSX.Element {
  const { t } = useI18n()
  const api = useApi()
  const agent = useAgent()
  const [selected, setSelected] = useState<string[]>([])
  const [error, setError] = useState<unknown>(null)
  const [busyId, setBusyId] = useState<string | null>(null)
  const [pendingUnmount, setPendingUnmount] = useState<AgentMountState[] | null>(null)
  const [pendingForce, setPendingForce] = useState<AgentMountState[] | null>(null)
  const [now, setNow] = useState(() => Date.now())

  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 30000)
    return () => clearInterval(timer)
  }, [])

  const leasesQuery = useQuery({
    queryKey: ['leases', 'mine'],
    queryFn: () => api.listLeases(),
    enabled: agent.available
  })

  const leaseExpiry = useMemo(() => {
    const map = new Map<string, number>()
    for (const lease of leasesQuery.data?.items ?? []) map.set(lease.id, lease.expires_at)
    return map
  }, [leasesQuery.data])

  const mounts = agent.state?.mounts ?? []

  const remainingOf = (mount: AgentMountState): string => {
    const expiresAt = leaseExpiry.get(mount.lease_id)
    if (expiresAt) return t('common.seconds', { seconds: String(Math.max(0, Math.ceil((expiresAt - now) / 1000))) })
    const estimate = estimateRemaining(mount, now)
    if (estimate === null) return '-'
    return t('agent.mount.leaseEstimate', { seconds: String(estimate) })
  }

  const runUnmount = async (targets: AgentMountState[], force = false): Promise<void> => {
    setError(null)
    try {
      for (const mount of targets) {
        setBusyId(mount.allocation_id)
        await agent.unmount({ allocation_id: mount.allocation_id, force })
      }
      setSelected([])
      setPendingForce(null)
    } catch (err) {
      setError(err)
      // 普通卸载失败（磁盘/会话/挂载点在本机早已不存在等）时给出**强制卸载**出口：
      // 代理会跳过失败的清理步骤、尽力拆除残留并删除记录。没有这个出口，脏记录
      // （如"卡在卸载中"）永远无法从界面删除（真实反馈："一直卡在卸载中，也无法删除"）。
      if (!force) setPendingForce(targets)
    } finally {
      setBusyId(null)
      setPendingUnmount(null)
    }
  }

  const openPath = (path: string): void => {
    setError(null)
    void agentApi.openPath(path).catch((err: unknown) => setError(err))
  }

  const columns: TableColumnsType<AgentMountState> = [
    { title: t('agent.mount.repo'), dataIndex: 'repo_name', key: 'repo_name' },
    {
      title: t('agent.mount.mode'),
      dataIndex: 'mount_mode',
      key: 'mount_mode',
      width: 120,
      render: (value: string) =>
        t(value === 'directory' ? 'settings.mountMode.directory' : 'settings.mountMode.letter')
    },
    {
      title: t('agent.mount.path'),
      dataIndex: 'mount_path',
      key: 'mount_path',
      render: (value: string, mount) => (
        <Tooltip title={value}>
          <Typography.Link
            disabled={!agent.available}
            ellipsis
            style={{ maxWidth: 260 }}
            onClick={() => openPath(openTargetOf(mount))}
          >
            {value}
          </Typography.Link>
        </Tooltip>
      )
    },
    {
      title: t('common.status'),
      dataIndex: 'state',
      key: 'state',
      width: 130,
      render: (_value: string, mount) => <MountStateCell mount={mount} />
    },
    {
      title: t('agent.mount.lease'),
      key: 'lease',
      width: 140,
      render: (_value, mount) => remainingOf(mount)
    },
    // 不再单独开"最后错误"列：错误以红色感叹号挂在状态列上（hover 展开详情），
    // 长期把错误码摆在表格里既占地方、也让人以为故障一直没处理（真实反馈）。
    {
      title: t('common.actions'),
      key: 'actions',
      width: 260,
      render: (_value, mount) => (
        <MountActionButtons
          mount={mount}
          canMount
          available={agent.available}
          busy={busyId === mount.allocation_id}
          onMount={() => undefined}
          onUnmount={() => setPendingUnmount([mount])}
          onRemount={() => {
            setError(null)
            setBusyId(mount.allocation_id)
            void agent.remount({ allocation_id: mount.allocation_id }).catch((err: unknown) => setError(err)).finally(() => setBusyId(null))
          }}
          onOpenPath={openPath}
        />
      )
    }
  ]

  return (
    <PageShell
      title={t('page.mount.title')}
      extra={
        <>
          <Typography.Text type="secondary">{t('agent.action.selected', { count: selected.length })}</Typography.Text>
          <Button onClick={() => void agent.refresh()}>{t('common.refresh')}</Button>
          <Button
            danger
            disabled={!agent.available || selected.length === 0}
            onClick={() => setPendingUnmount(mounts.filter((mount) => selected.includes(mount.allocation_id)))}
          >
            {t('agent.action.unmountSelected')}
          </Button>
        </>
      }
    >
      {!agent.available ? (
        <Alert type="warning" showIcon message={t('agent.degraded.title')} description={t('agent.degraded.desc')} style={{ marginBottom: spacing.md }} />
      ) : null}
      {error ? <ErrorNotice error={error} /> : null}

      <SectionCard title={t('page.mount.title')}>
        <Table<AgentMountState>
          rowKey={(mount) => mount.allocation_id}
          columns={columns}
          dataSource={mounts}
          loading={agent.available && agent.state === null}
          pagination={{ pageSize: 20, hideOnSinglePage: true, showSizeChanger: false }}
          locale={{ emptyText: t('agent.mounts.empty') }}
          rowSelection={{
            selectedRowKeys: selected,
            onChange: (keys) => setSelected(keys.map((key) => String(key))),
            getCheckboxProps: (mount) => ({ disabled: mount.state === 'mounting' || mount.state === 'unmounting' })
          }}
        />
      </SectionCard>

      <ConfirmDialog
        open={pendingUnmount !== null}
        danger
        title={t('action.unmount')}
        content={
          (pendingUnmount?.length ?? 0) > 1
            ? t('agent.unmount.batchConfirm', { count: String(pendingUnmount?.length ?? 0) })
            : t('agent.unmount.confirm')
        }
        loading={busyId !== null}
        onConfirm={() => {
          if (pendingUnmount) void runUnmount(pendingUnmount)
        }}
        onCancel={() => setPendingUnmount(null)}
      />

      {/* 强制卸载：普通卸载失败后的兜底出口（代理跳过失败的清理步骤并删除记录）。 */}
      <ConfirmDialog
        open={pendingForce !== null}
        danger
        title={t('agent.unmount.forceTitle')}
        content={t('agent.unmount.forceConfirm')}
        loading={busyId !== null}
        onConfirm={() => {
          if (pendingForce) void runUnmount(pendingForce, true)
        }}
        onCancel={() => setPendingForce(null)}
      />
    </PageShell>
  )
}
