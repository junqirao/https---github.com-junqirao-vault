import { useMemo, useState } from 'react'
import { Alert, Button, Descriptions, Form, Input, Modal, Space, Tag, Typography } from 'antd'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { DataTable } from '../../components/DataTable'
import { ErrorNotice } from '../../components/ErrorNotice'
import { PageShell } from '../../components/PageShell'
import { SectionCard } from '../../components/SectionCard'
import { StatusTag } from '../../components/StatusTag'
import { useApi } from '../../api/provider'
import type { HealthStatus, StorageDTO } from '../../api/types'
import { useI18n } from '../../i18n'
import { fontSize, spacing } from '../../tokens/palette'
import { formatBytes } from '../../utils/format'
import { LvmPoolCard } from './LvmPoolCard'
import { ServerConfigCard } from './ServerConfigCard'

export interface SystemSettingsProps {
  isSuperAdmin: boolean
}

/** 系统设置：版本契约、能力探测与健康检查（只读展示，真源在服务端 config.yaml）。 */
export function SystemSettings({ isSuperAdmin }: SystemSettingsProps): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const [error, setError] = useState<unknown>(null)
  const [renameOpen, setRenameOpen] = useState(false)
  const [renameForm] = Form.useForm<{ server_name: string }>()

  const infoQuery = useQuery({ queryKey: ['system-info'], queryFn: () => api.systemInfo() })
  const settingsQuery = useQuery({ queryKey: ['system-settings'], queryFn: () => api.settings(), enabled: isSuperAdmin })
  const healthQuery = useQuery({ queryKey: ['system-health'], queryFn: () => api.systemHealth() })
  const storagesQuery = useQuery({ queryKey: ['storages', 'list'], queryFn: () => api.listStorages() })

  const storages = useMemo<StorageDTO[]>(() => storagesQuery.data?.items ?? [], [storagesQuery.data])

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

  const renameMutation = useMutation({
    mutationFn: (serverName: string) => api.patchSettings({ server_name: serverName }),
    onSuccess: () => {
      setRenameOpen(false)
      void queryClient.invalidateQueries({ queryKey: ['system-settings'] })
      void queryClient.invalidateQueries({ queryKey: ['system-info'] })
    },
    onError: (err) => setError(err)
  })

  const info = infoQuery.data
  const compat = info?.client_compat
  const health = healthQuery.data

  return (
    <PageShell title={t('page.system.title')}>
      {/* 版本契约与能力探测仍是只读；服务端配置可在下方"服务端配置"里在线修改（不可改的项已置灰）。 */}
      <Alert type="info" showIcon message={t('system.configHint')} style={{ marginBottom: 16 }} />
      {error ? <ErrorNotice error={error} /> : null}
      {infoQuery.error ? <ErrorNotice error={infoQuery.error} /> : null}

      <SectionCard
        title={t('field.version')}
        loading={infoQuery.isLoading || settingsQuery.isLoading}
        extra={
          isSuperAdmin ? (
            <Button
              size="small"
              onClick={() => {
                renameForm.setFieldsValue({ server_name: settingsQuery.data?.server.name ?? info?.server_name ?? '' })
                setRenameOpen(true)
              }}
            >
              {t('common.edit')}
            </Button>
          ) : null
        }
      >
        <Descriptions size="small" column={2} bordered>
          <Descriptions.Item label={t('system.instanceId')}>{info?.server_instance_id ?? '-'}</Descriptions.Item>
          <Descriptions.Item label={t('system.serverName')}>{settingsQuery.data?.server.name ?? info?.server_name ?? '-'}</Descriptions.Item>
          <Descriptions.Item label={t('system.apiVersion')}>{info?.api_version ?? '-'}</Descriptions.Item>
          <Descriptions.Item label={t('system.serverVersion')}>{info?.server_version ?? '-'}</Descriptions.Item>
        </Descriptions>
      </SectionCard>

      <SectionCard title={t('system.clientCompat')} loading={infoQuery.isLoading}>
        {compat && !compat.enabled ? (
          <Alert type="warning" showIcon message={t('system.clientCompat.disabled')} description={t('system.clientCompat.disabledWarn')} />
        ) : null}
        <Descriptions size="small" column={2} bordered style={{ marginTop: compat && !compat.enabled ? 16 : 0 }}>
          <Descriptions.Item label={t('system.clientCompat.min')}>{compat?.min || t('system.clientCompat.unbounded')}</Descriptions.Item>
          <Descriptions.Item label={t('system.clientCompat.max')}>{compat?.max || t('system.clientCompat.unbounded')}</Descriptions.Item>
        </Descriptions>
      </SectionCard>

      <SectionCard title={t('system.features')} loading={infoQuery.isLoading}>
        <Space wrap>
          {(info?.features ?? []).length === 0 ? (
            <Typography.Text type="secondary">{t('common.empty')}</Typography.Text>
          ) : (
            (info?.features ?? []).map((feature) => <Tag key={feature}>{feature}</Tag>)
          )}
        </Space>
      </SectionCard>

      <SectionCard title={t('system.capabilities')} loading={infoQuery.isLoading}>
        <Descriptions size="small" column={2} bordered>
          <Descriptions.Item label={t('system.cap.platform')}>{info?.capabilities.platform_kind || '-'}</Descriptions.Item>
          <Descriptions.Item label={t('system.cap.lvm')}>
            {info?.capabilities.lvm ? t('system.cap.available') : t('system.cap.unavailable')}
          </Descriptions.Item>
          <Descriptions.Item label={t('system.cap.winTarget')}>
            {info?.capabilities.win_target ? t('system.cap.available') : t('system.cap.unavailable')}
          </Descriptions.Item>
          <Descriptions.Item label={t('system.cap.hyperv')}>
            {info?.capabilities.hyperv ? t('system.cap.available') : t('system.cap.unavailable')}
          </Descriptions.Item>
          <Descriptions.Item label={t('system.cap.fileSystem')}>{info?.capabilities.file_system || '-'}</Descriptions.Item>
          <Descriptions.Item label={t('system.cap.databaseDriver')}>{info?.capabilities.database_driver || '-'}</Descriptions.Item>
        </Descriptions>
      </SectionCard>

      {isSuperAdmin ? <ServerConfigCard isSuperAdmin={isSuperAdmin} /> : null}

      <LvmPoolCard enabled={Boolean(info?.capabilities.lvm)} isSuperAdmin={isSuperAdmin} />

      <SectionCard title={t('storage.title')} loading={storagesQuery.isLoading}>
        {storagesQuery.error ? <ErrorNotice error={storagesQuery.error} /> : null}
        {storages.length === 0 ? (
          <Typography.Text type="secondary">{t('storage.empty')}</Typography.Text>
        ) : (
          <>
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
            {storages.map((storage) => (
              <div key={storage.id} style={{ marginBottom: spacing.xs }}>
                <Typography.Text strong>{storage.name}</Typography.Text>
                <Typography.Text type="secondary" code style={{ marginLeft: spacing.sm }}>
                  {storage.path}
                </Typography.Text>
                <Typography.Text style={{ marginLeft: spacing.sm }}>
                  {`${formatBytes(storage.free_bytes)} / ${formatBytes(storage.total_bytes)}`}
                </Typography.Text>
              </div>
            ))}
          </>
        )}
      </SectionCard>

      <SectionCard title={t('system.health')}>
        {healthQuery.error ? <ErrorNotice error={healthQuery.error} /> : null}
        <HealthTable health={health} loading={healthQuery.isLoading} />
      </SectionCard>

      <Modal
        open={renameOpen}
        title={t('system.rename.title')}
        width={480}
        centered
        okText={t('common.save')}
        cancelText={t('common.cancel')}
        confirmLoading={renameMutation.isPending}
        onCancel={() => setRenameOpen(false)}
        onOk={() => {
          void renameForm.validateFields().then((values) => renameMutation.mutate(values.server_name))
        }}
      >
        <Form form={renameForm} layout="vertical">
          <Form.Item name="server_name" label={t('field.serverNameOverride')} rules={[{ required: true }]}>
            <Input maxLength={64} />
          </Form.Item>
        </Form>
      </Modal>
    </PageShell>
  )
}

/** 按卷去重后的容量汇总：同一卷上的多条存储只计一条容量。 */
interface VolumeSummary {
  volumeName: string
  freeBytes: number
  totalBytes: number
  count: number
}

interface HealthRow {
  name: string
  result: string
}

/**
 * 健康检查结果表。
 *
 * `loading` 传进来时必须用上：`checks` 是异步来的，不传的话首屏先渲染一次"暂无数据"，
 * 数据到了再变成几行 —— 用户最先看到的是一句错话。
 */
function HealthTable({ health, loading }: { health?: HealthStatus; loading?: boolean }): JSX.Element {
  const { t } = useI18n()
  const rows: HealthRow[] = Object.entries(health?.checks ?? {}).map(([name, result]) => ({ name, result }))
  return (
    <>
      {health && !health.ok ? (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 16 }}
          message={t('common.failed')}
          description={t('system.health.failures', { names: (health.failures ?? []).join(', ') })}
        />
      ) : null}
      <DataTable<HealthRow>
        columns={[
          { title: t('common.name'), dataIndex: 'name', key: 'name' },
          {
            title: t('field.result'),
            dataIndex: 'result',
            key: 'result',
            render: (value: string) => <StatusTag group="audit" value={value === 'ok' ? 'ok' : 'error'} />
          },
          { title: t('field.detail'), dataIndex: 'result', key: 'detail' }
        ]}
        rows={rows}
        rowKey={(row) => row.name}
        loading={loading}
        empty={t('common.empty')}
        pagination={false}
      />
    </>
  )
}
