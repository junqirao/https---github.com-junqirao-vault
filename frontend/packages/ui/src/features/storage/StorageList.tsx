import { useEffect, useMemo, useState } from 'react'
import { Alert, Button, Form, Input, InputNumber, Modal, Space, Switch, Tag, Tooltip, Typography } from 'antd'
import type { TableColumnsType } from 'antd'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { ConfirmDialog } from '../../components/ConfirmDialog'
import { DataTable } from '../../components/DataTable'
import { EmptyState } from '../../components/EmptyState'
import { ErrorNotice } from '../../components/ErrorNotice'
import { PageShell } from '../../components/PageShell'
import { useApi } from '../../api/provider'
import type { StorageDTO } from '../../api/types'
import { useI18n } from '../../i18n'
import { fontSize, spacing } from '../../tokens/palette'
import { formatBytes } from '../../utils/format'

const BYTES_PER_GB = 1024 ** 3

/** 编辑表单值：只有这三个字段可改（挂载点对卷承载的存储是锁定项）。 */
interface StorageEditValues {
  name: string
  enabled: boolean
  mountPoint?: string
}

/** 按卷去重后的容量汇总：同一卷上的多条存储只计一条容量。 */
interface VolumeSummary {
  volumeName: string
  freeBytes: number
  totalBytes: number
  count: number
}

/** 是否由底层卷承载（thin / lv）；目录模式（Windows、或历史存储）为 false。 */
function isVolumeBacked(storage: StorageDTO): boolean {
  return storage.kind === 'thin' || storage.kind === 'lv'
}

/**
 * 存储管理（管理员）：列表 / 编辑 / 启停 / 挂载 / 卸载 / 扩容 / 移除记录。
 *
 * 创建不在本页：逻辑链是「选/建存储池 → 设定存储 → 存储库」，内联建池还要带块设备表格，
 * 弹窗装不下也看不清，所以独立成创建页（StorageCreateForm），保存后跳回本页。
 */
export function StorageList({ onCreate }: { onCreate: () => void }): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const [form] = Form.useForm<StorageEditValues>()
  const [resizeForm] = Form.useForm<{ sizeGB: number }>()
  const [error, setError] = useState<unknown>(null)
  const [formOpen, setFormOpen] = useState(false)
  const [editing, setEditing] = useState<StorageDTO | null>(null)
  const [pendingDelete, setPendingDelete] = useState<StorageDTO | null>(null)
  const [pendingUnmount, setPendingUnmount] = useState<StorageDTO | null>(null)
  const [resizeTarget, setResizeTarget] = useState<StorageDTO | null>(null)

  const storagesQuery = useQuery({ queryKey: ['storages', 'list'], queryFn: () => api.listStorages() })
  const storages = storagesQuery.data?.items ?? []

  const invalidate = (): void => {
    void queryClient.invalidateQueries({ queryKey: ['storages'] })
  }

  useEffect(() => {
    if (!formOpen || !editing) return
    setError(null)
    form.setFieldsValue({ name: editing.name, mountPoint: editing.path, enabled: editing.enabled })
  }, [editing, form, formOpen])

  const saveMutation = useMutation({
    mutationFn: async (values: StorageEditValues) => {
      // 弹窗只在编辑态打开，editing 不该为空；用窄化而不是断言，避免类型逃逸。
      if (!editing) throw new Error('storage not selected')
      const patch: { name: string; enabled: boolean; path?: string } = {
        name: values.name,
        enabled: values.enabled
      }
      // 绑定底层卷的存储其"挂载点即路径"，改路径会让卷与目录脱钩，后端会拒绝，这里直接不发。
      const mountPoint = (values.mountPoint ?? '').trim()
      if (mountPoint && mountPoint !== editing.path && !isVolumeBacked(editing)) {
        patch.path = mountPoint
      }
      return api.updateStorage(editing.id, patch)
    },
    onSuccess: () => {
      setError(null)
      setFormOpen(false)
      invalidate()
    },
    onError: (err) => setError(err)
  })

  const toggleMutation = useMutation({
    mutationFn: ({ id, enabled }: { id: string; enabled: boolean }) => api.updateStorage(id, { enabled }),
    onSuccess: () => {
      setError(null)
      invalidate()
    },
    onError: (err) => setError(err)
  })

  const deleteMutation = useMutation({
    mutationFn: (id: string) => api.deleteStorage(id),
    onSuccess: () => {
      setError(null)
      setPendingDelete(null)
      invalidate()
    },
    onError: (err) => setError(err)
  })

  const mountMutation = useMutation({
    mutationFn: (id: string) => api.mountStorage(id),
    onSuccess: () => {
      setError(null)
      invalidate()
    },
    onError: (err) => setError(err)
  })

  // 卸载保留确认框（失败时留在框内展示原因，如"有进行中的上传"）。
  const unmountMutation = useMutation({
    mutationFn: (id: string) => api.unmountStorage(id),
    onSuccess: () => {
      setError(null)
      setPendingUnmount(null)
      invalidate()
    },
    onError: (err) => setError(err)
  })

  const resizeMutation = useMutation({
    mutationFn: ({ id, sizeBytes }: { id: string; sizeBytes: number }) => api.resizeStorage(id, sizeBytes),
    onSuccess: () => {
      setError(null)
      setResizeTarget(null)
      invalidate()
    },
    onError: (err) => setError(err)
  })

  const volumeSummaries = useMemo<VolumeSummary[]>(() => {
    const map = new Map<string, VolumeSummary>()
    for (const storage of storages) {
      const key = storage.volume_name || storage.path
      const existing = map.get(key)
      if (existing) {
        existing.count += 1
      } else {
        map.set(key, {
          volumeName: storage.volume_name || '-',
          freeBytes: storage.free_bytes,
          totalBytes: storage.total_bytes,
          count: 1
        })
      }
    }
    return [...map.values()]
  }, [storages])

  const kindLabel = (kind: StorageDTO['kind']): string => {
    if (kind === 'thin') return t('storage.kind.thin')
    if (kind === 'lv') return t('storage.kind.lv')
    return t('storage.kind.dir')
  }

  const openResize = (storage: StorageDTO): void => {
    setError(null)
    setResizeTarget(storage)
    resizeForm.setFieldsValue({ sizeGB: Math.round((storage.size_bytes / BYTES_PER_GB) * 100) / 100 })
  }

  const columns: TableColumnsType<StorageDTO> = [
    { title: t('field.name'), dataIndex: 'name', key: 'name', width: 160 },
    {
      title: t('field.type'),
      key: 'kind',
      width: 100,
      render: (_value, storage) => <Tag>{kindLabel(storage.kind)}</Tag>
    },
    {
      title: t('storage.capacity'),
      key: 'capacity',
      width: 190,
      render: (_value, storage) =>
        // 有底层卷时展示"已用 ÷ 分配"（配额口径）；纯目录模式退化为所在卷可用空间。
        storage.size_bytes > 0
          ? `${formatBytes(storage.used_bytes)} / ${formatBytes(storage.size_bytes)}`
          : `${formatBytes(storage.free_bytes)} / ${formatBytes(storage.total_bytes)}`
    },
    {
      title: t('field.mountPoint'),
      dataIndex: 'path',
      key: 'path',
      render: (value: string) => (
        <Tooltip title={value}>
          <Typography.Text type="secondary" ellipsis style={{ maxWidth: 320 }}>
            {value}
          </Typography.Text>
        </Tooltip>
      )
    },
    {
      title: t('system.cap.fileSystem'),
      dataIndex: 'file_system',
      key: 'file_system',
      width: 110,
      render: (value: string) => value || '-'
    },
    { title: t('storage.diskCount'), dataIndex: 'disk_count', key: 'disk_count', width: 90 },
    {
      title: t('common.status'),
      key: 'enabled',
      width: 170,
      render: (_value, storage) => (
        <Space size={spacing.xxs} wrap>
          <Switch
            size="small"
            checked={storage.enabled}
            loading={toggleMutation.isPending && toggleMutation.variables?.id === storage.id}
            checkedChildren={t('common.enabled')}
            unCheckedChildren={t('common.disabled')}
            onChange={(checked) => {
              setError(null)
              toggleMutation.mutate({ id: storage.id, enabled: checked })
            }}
          />
          {isVolumeBacked(storage) ? (
            storage.mounted ? (
              <Tag color="green">{t('storage.mounted')}</Tag>
            ) : (
              <Tag color="red">{t('storage.notMounted')}</Tag>
            )
          ) : null}
        </Space>
      )
    },
    {
      title: t('common.actions'),
      key: 'actions',
      width: 250,
      render: (_value, storage) => (
        <Space size={0} wrap>
          <Button
            type="link"
            size="small"
            onClick={() => {
              setEditing(storage)
              setFormOpen(true)
            }}
          >
            {t('common.edit')}
          </Button>
          {isVolumeBacked(storage) ? (
            storage.mounted ? (
              <Button
                type="link"
                size="small"
                onClick={() => {
                  setError(null)
                  setPendingUnmount(storage)
                }}
              >
                {t('storage.unmount')}
              </Button>
            ) : (
              <Button
                type="link"
                size="small"
                loading={mountMutation.isPending && mountMutation.variables === storage.id}
                onClick={() => {
                  setError(null)
                  mountMutation.mutate(storage.id)
                }}
              >
                {t('storage.mount')}
              </Button>
            )
          ) : null}
          {storage.kind === 'thin' ? (
            <Button type="link" size="small" onClick={() => openResize(storage)}>
              {t('storage.resize')}
            </Button>
          ) : null}
          <Button
            type="link"
            size="small"
            danger
            onClick={() => {
              setError(null)
              setPendingDelete(storage)
            }}
          >
            {t('storage.remove')}
          </Button>
        </Space>
      )
    }
  ]

  return (
    <PageShell
      title={t('storage.title')}
      extra={
        <>
          <Button onClick={() => void storagesQuery.refetch()}>{t('common.refresh')}</Button>
          <Button type="primary" onClick={onCreate}>
            {t('storage.create')}
          </Button>
        </>
      }
    >
      {error ? <ErrorNotice error={error} /> : null}
      {storagesQuery.error ? <ErrorNotice error={storagesQuery.error} /> : null}

      {volumeSummaries.length > 0 ? (
        <Space size={spacing.md} wrap style={{ marginBottom: spacing.sm }}>
          <Typography.Text type="secondary" style={{ fontSize: fontSize.sm }}>
            {t('storage.volumeSummary')}
          </Typography.Text>
          {volumeSummaries.map((volume) => (
            <Typography.Text key={volume.volumeName} style={{ fontSize: fontSize.sm }}>
              {`${volume.volumeName} · ${formatBytes(volume.freeBytes)} / ${formatBytes(volume.totalBytes)} · ${volume.count}`}
            </Typography.Text>
          ))}
        </Space>
      ) : null}

      <DataTable<StorageDTO>
        columns={columns}
        rows={storages}
        rowKey={(storage) => storage.id}
        loading={storagesQuery.isLoading}
        empty={<EmptyState title={t('storage.empty')} />}
        scroll={{ x: 1280 }}
      />

      <Modal
        open={formOpen}
        title={t('storage.edit')}
        width={560}
        centered
        okText={t('common.save')}
        cancelText={t('common.cancel')}
        confirmLoading={saveMutation.isPending}
        onOk={() => {
          void form.validateFields().then((values) => {
            setError(null)
            saveMutation.mutate(values)
          })
        }}
        onCancel={() => setFormOpen(false)}
        destroyOnClose
        maskClosable={false}
      >
        {error ? <ErrorNotice error={error} /> : null}
        <Form form={form} layout="vertical">
          <Form.Item name="name" label={t('field.name')} rules={[{ required: true }]}>
            <Input maxLength={64} />
          </Form.Item>
          <Form.Item name="enabled" label={t('common.status')} valuePropName="checked">
            <Switch checkedChildren={t('common.enabled')} unCheckedChildren={t('common.disabled')} />
          </Form.Item>
          {editing ? (
            <Form.Item
              name="mountPoint"
              label={t('field.mountPoint')}
              extra={isVolumeBacked(editing) ? t('storage.pathLocked') : undefined}
            >
              <Input spellCheck={false} disabled={isVolumeBacked(editing)} />
            </Form.Item>
          ) : null}
        </Form>
      </Modal>

      <Modal
        open={resizeTarget !== null}
        title={t('storage.resize')}
        width={480}
        centered
        okText={t('common.save')}
        cancelText={t('common.cancel')}
        confirmLoading={resizeMutation.isPending}
        onOk={() => {
          void resizeForm.validateFields().then((values) => {
            if (!resizeTarget) return
            setError(null)
            resizeMutation.mutate({ id: resizeTarget.id, sizeBytes: Math.round(values.sizeGB * BYTES_PER_GB) })
          })
        }}
        onCancel={() => setResizeTarget(null)}
        destroyOnClose
        maskClosable={false}
      >
        {error ? <ErrorNotice error={error} /> : null}
        <Alert type="info" showIcon message={t('storage.resizeHint')} style={{ marginBottom: spacing.md }} />
        <Form form={resizeForm} layout="vertical">
          <Form.Item name="sizeGB" label={t('storage.sizeGb')} rules={[{ required: true }]}>
            <InputNumber min={0.1} precision={2} style={{ width: '100%' }} addonAfter="GB" />
          </Form.Item>
        </Form>
      </Modal>

      <ConfirmDialog
        open={pendingUnmount !== null}
        title={t('storage.unmount')}
        content={
          <>
            <div>{t('storage.unmountConfirm')}</div>
            {error ? (
              <div style={{ marginTop: spacing.sm }}>
                <ErrorNotice error={error} />
              </div>
            ) : null}
          </>
        }
        loading={unmountMutation.isPending}
        onConfirm={() => {
          if (!pendingUnmount) return
          setError(null)
          unmountMutation.mutate(pendingUnmount.id)
        }}
        onCancel={() => setPendingUnmount(null)}
      />

      <ConfirmDialog
        open={pendingDelete !== null}
        danger
        title={t('storage.remove')}
        content={
          <>
            <div>{t('storage.removeConfirm')}</div>
            {error ? (
              <div style={{ marginTop: spacing.sm }}>
                <ErrorNotice error={error} />
              </div>
            ) : null}
          </>
        }
        loading={deleteMutation.isPending}
        onConfirm={() => {
          if (!pendingDelete) return
          setError(null)
          deleteMutation.mutate(pendingDelete.id)
        }}
        onCancel={() => setPendingDelete(null)}
      />
    </PageShell>
  )
}
