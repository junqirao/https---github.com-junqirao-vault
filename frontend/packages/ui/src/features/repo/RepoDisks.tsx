import { useEffect, useState } from 'react'
import { Button, Form, Input, Modal, Progress, Space, Tooltip, Typography } from 'antd'
import type { TableColumnsType } from 'antd'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { ConfirmDialog } from '../../components/ConfirmDialog'
import { DataTable } from '../../components/DataTable'
import { ErrorNotice } from '../../components/ErrorNotice'
import { SectionCard } from '../../components/SectionCard'
import { StatusTag } from '../../components/StatusTag'
import { agentApi } from '../../api/agentClient'
import type { DownloadState } from '../../api/agentTypes'
import { ApiError } from '../../api/errors'
import { useApi } from '../../api/provider'
import type { DiskDTO } from '../../api/types'
import { useAgent } from '../../hooks/useAgent'
import { useI18n } from '../../i18n'
import { formatBytes, formatTime } from '../../utils/format'
import { diskKindLabel } from '../../utils/labels'

export interface RepoDisksProps {
  repoId: string
}

/** 存储库下的磁盘列表（支持回收空间与异步删除）。 */
export function RepoDisks({ repoId }: RepoDisksProps): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const [error, setError] = useState<unknown>(null)
  const [pendingDelete, setPendingDelete] = useState<DiskDTO | null>(null)
  const agent = useAgent()
  const [copyDisk, setCopyDisk] = useState<DiskDTO | null>(null)
  const [copyForm] = Form.useForm<{ target_dir: string; file_name: string }>()
  const { config: agentConfig, downloads, refreshDownloads } = agent

  // 刷新页面后恢复进行中/历史下载；属背景恢复，失败不阻断页面（用户主动操作失败时会另行提示）。
  useEffect(() => {
    void refreshDownloads().catch(() => undefined)
  }, [refreshDownloads])

  const disksQuery = useQuery({
    queryKey: ['repo-disks', repoId],
    queryFn: () => api.listRepoDisks(repoId)
  })

  const invalidate = (): void => {
    void queryClient.invalidateQueries({ queryKey: ['repo-disks', repoId] })
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
          <Typography.Text type="secondary" ellipsis style={{ maxWidth: 320 }}>
            {value}
          </Typography.Text>
        </Tooltip>
      )
    },
    {
      title: t('common.size'),
      key: 'size',
      width: 180,
      render: (_value, disk) => `${formatBytes(disk.physical_bytes)} / ${formatBytes(disk.size_bytes)}`
    },
    {
      title: t('common.status'),
      dataIndex: 'state',
      key: 'state',
      width: 110,
      render: (value: string) => <StatusTag group="disk" value={value} />
    },
    {
      title: t('repo.mount.title'),
      dataIndex: 'mounted',
      key: 'mounted',
      width: 110,
      render: (value: boolean) => (value ? t('repo.mount.mounted') : t('repo.mount.notMounted'))
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
      width: 220,
      render: (_value, disk) => (
        <Space size={0}>
          {disk.kind === 'parent' ? (
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
  ]

  return (
    <SectionCard title={t('repo.detail.disks')}>
      {error ? <ErrorNotice error={error} /> : null}
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
        scroll={{ x: 900 }}
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
