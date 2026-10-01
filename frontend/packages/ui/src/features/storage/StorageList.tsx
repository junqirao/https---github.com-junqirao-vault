import { useEffect, useMemo, useState } from 'react'
import { Alert, Button, Collapse, Form, Input, InputNumber, Modal, Select, Space, Switch, Tag, Tooltip, Typography } from 'antd'
import type { TableColumnsType } from 'antd'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { ConfirmDialog } from '../../components/ConfirmDialog'
import { DataTable } from '../../components/DataTable'
import { EmptyState } from '../../components/EmptyState'
import { ErrorNotice } from '../../components/ErrorNotice'
import { PageShell } from '../../components/PageShell'
import { useApi } from '../../api/provider'
import type { CreateStorageRequest, StorageDTO } from '../../api/types'
import { useI18n } from '../../i18n'
import { fontSize, spacing } from '../../tokens/palette'
import { formatBytes } from '../../utils/format'

const BYTES_PER_GB = 1024 ** 3

/** 创建模式（与后端 app.StorageMode* 一致）。 */
type StorageMode = 'thin' | 'register_dir' | 'register_lv'

interface StorageFormValues {
  name: string
  enabled: boolean
  mode: StorageMode
  /** 分配容量，仅 thin 使用；表单以 GB 为单位。 */
  sizeGB?: number
  fileSystem?: string
  mountPoint?: string
  /** 已有 LV 引用，仅 register_lv 使用。 */
  ref?: string
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

/** 存储管理（管理员）：列表 / 新建 / 编辑 / 启停 / 挂载 / 卸载 / 扩容 / 移除记录。 */
export function StorageList(): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const [form] = Form.useForm<StorageFormValues>()
  const [resizeForm] = Form.useForm<{ sizeGB: number }>()
  const [error, setError] = useState<unknown>(null)
  const [formOpen, setFormOpen] = useState(false)
  const [editing, setEditing] = useState<StorageDTO | null>(null)
  const [pendingDelete, setPendingDelete] = useState<StorageDTO | null>(null)
  const [pendingUnmount, setPendingUnmount] = useState<StorageDTO | null>(null)
  const [resizeTarget, setResizeTarget] = useState<StorageDTO | null>(null)

  // 能力探测：lvm=true 表示平台有"存储底层卷"（Linux），才展示 thin LV 相关的交互。
  const infoQuery = useQuery({ queryKey: ['system-info'], queryFn: () => api.systemInfo() })
  const supportsVolumes = Boolean(infoQuery.data?.capabilities.lvm)
  // 能力未知前不渲染与平台强相关的表单项，避免先闪出 Windows 的挂载点输入框再跳成「高级」。
  const platformKnown = infoQuery.isSuccess

  const storagesQuery = useQuery({ queryKey: ['storages', 'list'], queryFn: () => api.listStorages() })
  const storages = storagesQuery.data?.items ?? []

  const formMode = Form.useWatch('mode', form) ?? 'thin'
  // 池水位提示只在"新建 thin 卷"时需要；复用既有的 GET /v1/system/lvm（与系统设置页共享缓存）。
  // 必须带上 supportsVolumes：Windows 上表单里残留的 thin 默认值不该触发 LVM 提示。
  const creatingThin = supportsVolumes && formOpen && editing === null && formMode === 'thin'
  const lvmQuery = useQuery({ queryKey: ['lvm-status'], queryFn: () => api.lvmStatus(), enabled: supportsVolumes && creatingThin })

  const invalidate = (): void => {
    void queryClient.invalidateQueries({ queryKey: ['storages'] })
  }

  useEffect(() => {
    if (!formOpen) return
    setError(null)
    if (editing) {
      form.setFieldsValue({ name: editing.name, mountPoint: editing.path, enabled: editing.enabled })
    } else {
      form.resetFields()
      form.setFieldsValue({ enabled: true, mode: 'thin', fileSystem: 'ext4' })
    }
  }, [editing, form, formOpen])

  const saveMutation = useMutation({
    mutationFn: (values: StorageFormValues) => {
      const mountPoint = (values.mountPoint ?? '').trim()
      if (editing) {
        const patch: { name: string; enabled: boolean; path?: string } = {
          name: values.name,
          enabled: values.enabled
        }
        // 绑定底层卷的存储其"挂载点即路径"，改路径会让卷与目录脱钩，后端会拒绝，这里直接不发。
        if (mountPoint && mountPoint !== editing.path && !isVolumeBacked(editing)) {
          patch.path = mountPoint
        }
        return api.updateStorage(editing.id, patch)
      }
      // Windows 没有底层卷：模式恒为登记目录（显式写死，避免把表单里的 thin 默认值发给后端拿 501）。
      const mode: StorageMode = supportsVolumes ? values.mode : 'register_dir'
      const body: CreateStorageRequest = {
        name: values.name,
        enabled: values.enabled,
        mode
      }
      if (mountPoint) body.mount_point = mountPoint
      if (mode === 'thin') {
        body.size_bytes = Math.round((values.sizeGB ?? 0) * BYTES_PER_GB)
        if (values.fileSystem) body.file_system = values.fileSystem
      }
      if (mode === 'register_lv') body.ref = (values.ref ?? '').trim()
      return api.createStorage(body)
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
          <Button
            type="primary"
            onClick={() => {
              setEditing(null)
              setFormOpen(true)
            }}
          >
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
        title={t(editing ? 'storage.edit' : 'storage.create')}
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
        {creatingThin ? (
          <Alert
            type={lvmQuery.data && !lvmQuery.data.exists ? 'warning' : 'info'}
            showIcon
            style={{ marginBottom: spacing.md }}
            message={
              lvmQuery.data && lvmQuery.data.exists
                ? t('storage.poolHint', {
                    free: formatBytes(lvmQuery.data.free_bytes),
                    total: formatBytes(lvmQuery.data.size_bytes),
                    percent: lvmQuery.data.data_percent.toFixed(1)
                  })
                : t('storage.poolMissing')
            }
          />
        ) : null}
        <Form form={form} layout="vertical" initialValues={{ enabled: true, mode: 'thin', fileSystem: 'ext4' }}>
          <Form.Item name="name" label={t('field.name')} rules={[{ required: true }]}>
            <Input maxLength={64} />
          </Form.Item>

          {!editing && platformKnown && !supportsVolumes ? (
            // Windows：没有底层卷，存储就是本地目录，必须显式给出。
            <Form.Item
              name="mountPoint"
              label={t('field.mountPoint')}
              rules={[{ required: true, message: t('storage.mountPointRequired') }]}
            >
              <Input spellCheck={false} />
            </Form.Item>
          ) : null}

          {!editing && supportsVolumes && formMode === 'thin' ? (
            <>
              <Form.Item
                name="sizeGB"
                label={t('storage.sizeGb')}
                rules={[{ required: true, message: t('storage.sizeGbRequired') }]}
              >
                <InputNumber min={0.1} precision={2} style={{ width: '100%' }} addonAfter="GB" />
              </Form.Item>
              <Form.Item name="fileSystem" label={t('system.cap.fileSystem')}>
                <Select
                  options={[
                    { value: 'ext4', label: 'ext4' },
                    { value: 'xfs', label: 'xfs' }
                  ]}
                />
              </Form.Item>
            </>
          ) : null}

          {!editing && supportsVolumes && formMode === 'register_lv' ? (
            <Form.Item
              name="ref"
              label={t('storage.ref')}
              rules={[{ required: true, message: t('storage.refRequired') }]}
              extra={t('storage.refHint')}
            >
              <Input spellCheck={false} placeholder="/dev/mapper/vg-lv" />
            </Form.Item>
          ) : null}

          <Form.Item name="enabled" label={t('common.status')} valuePropName="checked">
            <Switch checkedChildren={t('common.enabled')} unCheckedChildren={t('common.disabled')} />
          </Form.Item>

          {!editing && supportsVolumes ? (
            <Collapse
              ghost
              items={[
                {
                  key: 'advanced',
                  label: t('storage.advanced'),
                  children: (
                    <>
                      <Form.Item name="mode" label={t('field.mode')}>
                        <Select<StorageMode>
                          options={[
                            { value: 'thin', label: t('storage.mode.thin') },
                            { value: 'register_dir', label: t('storage.mode.registerDir') },
                            { value: 'register_lv', label: t('storage.mode.registerLv') }
                          ]}
                        />
                      </Form.Item>
                      <Form.Item
                        name="mountPoint"
                        label={t('field.mountPoint')}
                        extra={formMode === 'register_dir' ? t('storage.mountPointRequired') : t('storage.mountPointAuto')}
                        rules={[{ required: formMode === 'register_dir', message: t('storage.mountPointRequired') }]}
                      >
                        <Input spellCheck={false} placeholder={t('storage.mountPointPlaceholder')} />
                      </Form.Item>
                    </>
                  )
                }
              ]}
            />
          ) : null}

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