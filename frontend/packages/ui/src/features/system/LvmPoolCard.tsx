import { useState } from 'react'
import { Alert, Button, Descriptions, Modal, Select, Space, Tag, Typography } from 'antd'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { ErrorNotice } from '../../components/ErrorNotice'
import { SectionCard } from '../../components/SectionCard'
import { useApi } from '../../api/provider'
import type { InitializePoolRequest } from '../../api/types'
import { useI18n } from '../../i18n'
import { spacing } from '../../tokens/palette'
import { formatBytes } from '../../utils/format'
import { BlockDevicePicker } from './BlockDevicePicker'

export interface LvmPoolCardProps {
  /** 是否具备 LVM 存储池管理能力（来自 system-info 的能力探测）。 */
  enabled: boolean
  isSuperAdmin: boolean
  /**
   * 能力探测（system-info）还在路上：先按"可能存在"渲染骨架。
   *
   * 不这么做的话，`enabled` 在探测回来前一直是 false，整张卡片会先缺席、探测完再"冒"出来
   * —— 首屏看起来就是页面抖了一下。
   */
  loading?: boolean
}

/**
 * 存储池卡片：展示 LVM thin pool 与 dm-cache 的现状，并提供**一次性**的池初始化。
 *
 * 只读部分（容量/使用率/缓存统计）的真源在服务端 config.yaml 与 LVM 自身；
 * 本卡片不做任何在线修改，唯一的写操作是"初始化存储池"——它幂等：
 * 已存在的卷组 / thin pool / 缓存一律跳过，绝不覆盖既有数据。
 */
export function LvmPoolCard({ enabled, isSuperAdmin, loading }: LvmPoolCardProps): JSX.Element | null {
  const api = useApi()
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const [error, setError] = useState<unknown>(null)
  const [open, setOpen] = useState(false)
  const [hdd, setHdd] = useState<string[]>([])
  const [cache, setCache] = useState<string[]>([])
  const [cacheMode, setCacheMode] = useState<'writethrough' | 'writeback'>('writethrough')
  const [cachePolicy, setCachePolicy] = useState<string>('')

  const statusQuery = useQuery({ queryKey: ['lvm-status'], queryFn: () => api.lvmStatus(), enabled })
  // 设备列表只在打开弹窗时拉取：lsblk 是子进程调用，没必要随页面刷。
  const devicesQuery = useQuery({ queryKey: ['block-devices'], queryFn: () => api.listBlockDevices(), enabled: enabled && open })

  const initMutation = useMutation({
    mutationFn: () => {
      const body: InitializePoolRequest = {
        hdd_devices: hdd,
        cache_devices: cache
      }
      if (cache.length > 0) {
        body.cache_mode = cacheMode
        if (cachePolicy) {
          body.cache_policy = cachePolicy
        }
      }
      return api.initializePool(body)
    },
    onSuccess: () => {
      setOpen(false)
      setHdd([])
      setCache([])
      setError(null)
      void queryClient.invalidateQueries({ queryKey: ['lvm-status'] })
      void queryClient.invalidateQueries({ queryKey: ['system-info'] })
    },
    onError: (err) => setError(err)
  })

  // 能力确定不存在才整块不显示；探测中（loading）先按"可能有"渲染骨架，避免卡片闪现。
  if (!enabled && !loading) {
    return null
  }

  const status = statusQuery.data
  const devices = devicesQuery.data?.items ?? []
  const cacheDevices: string[] = status?.cache_devices ?? []
  const healthBad = Boolean(status?.cache_health)

  return (
    <SectionCard
      title={t('system.lvm.title')}
      loading={loading || statusQuery.isLoading}
      extra={
        isSuperAdmin ? (
          // 状态没到就点"初始化"，弹窗里的既有池/设备判断都是空的 —— 先禁掉。
          <Button size="small" disabled={loading || statusQuery.isLoading} onClick={() => setOpen(true)}>
            {status?.exists ? t('system.lvm.init.attachCache') : t('system.lvm.init.action')}
          </Button>
        ) : null
      }
    >
      <Alert type="info" showIcon message={t('system.lvm.hint')} style={{ marginBottom: spacing.md }} />
      {error ? <ErrorNotice error={error} /> : null}
      {statusQuery.error ? <ErrorNotice error={statusQuery.error} /> : null}
      {healthBad ? (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: spacing.md }}
          message={t('system.lvm.cache.healthBad', { status: status?.cache_health ?? '' })}
        />
      ) : null}

      <Descriptions size="small" column={2} bordered>
        <Descriptions.Item label={t('system.lvm.vg')}>{status?.vg || '-'}</Descriptions.Item>
        <Descriptions.Item label={t('system.lvm.thinPool')}>{status?.thin_pool || '-'}</Descriptions.Item>
        <Descriptions.Item label={t('common.status')}>
          {status?.exists ? (
            <Tag color="green">{t('system.lvm.exists.true')}</Tag>
          ) : (
            <Tag color="orange">{t('system.lvm.exists.false')}</Tag>
          )}
        </Descriptions.Item>
        <Descriptions.Item label={t('system.lvm.size')}>
          {status ? `${formatBytes(status.free_bytes)} / ${formatBytes(status.size_bytes)}` : '-'}
        </Descriptions.Item>
        <Descriptions.Item label={t('system.lvm.dataPercent')}>
          {status ? `${status.data_percent.toFixed(1)}%` : '-'}
        </Descriptions.Item>
        <Descriptions.Item label={t('system.lvm.metadataPercent')}>
          {status ? `${status.metadata_percent.toFixed(1)}%` : '-'}
        </Descriptions.Item>
        <Descriptions.Item label={t('system.lvm.cache')}>
          {status?.cache_attached ? (
            <Tag color="green">{t('system.lvm.cache.attached')}</Tag>
          ) : (
            <Tag>{t('system.lvm.cache.detached')}</Tag>
          )}
        </Descriptions.Item>
        <Descriptions.Item label={t('system.lvm.cache.mode')}>{status?.cache_mode || '-'}</Descriptions.Item>
        <Descriptions.Item label={t('system.lvm.cache.policy')}>{status?.cache_policy || '-'}</Descriptions.Item>
        <Descriptions.Item label={t('system.lvm.cache.chunkSize')}>{status?.cache_chunk_size || '-'}</Descriptions.Item>
        <Descriptions.Item label={t('system.lvm.cache.devices')}>
          {cacheDevices.length === 0 ? '-' : cacheDevices.join(', ')}
        </Descriptions.Item>
        <Descriptions.Item label={t('system.lvm.cache.dirty')}>{status?.cache_dirty_blocks ?? 0}</Descriptions.Item>
      </Descriptions>

      <Modal
        open={open}
        title={t('system.lvm.init.title')}
        width={760}
        centered
        okText={t('system.lvm.init.action')}
        cancelText={t('common.cancel')}
        confirmLoading={initMutation.isPending}
        onCancel={() => setOpen(false)}
        onOk={() => initMutation.mutate()}
      >
        <Alert type="warning" showIcon message={t('system.lvm.init.hint')} style={{ marginBottom: spacing.md }} />
        {devicesQuery.error ? <ErrorNotice error={devicesQuery.error} /> : null}
        {error ? <ErrorNotice error={error} /> : null}

        <Space direction="vertical" size={spacing.md} style={{ width: '100%' }}>
          <div>
            <Typography.Text strong>{t('system.lvm.init.hdd')}</Typography.Text>
            <BlockDevicePicker
              purpose="vg"
              devices={devices}
              loading={devicesQuery.isLoading}
              value={hdd}
              onChange={setHdd}
            />
          </div>
          <div>
            <Typography.Text strong>{t('system.lvm.init.cache')}</Typography.Text>
            <BlockDevicePicker
              purpose="cache"
              devices={devices}
              loading={devicesQuery.isLoading}
              value={cache}
              onChange={setCache}
            />
            <Space size={spacing.sm} style={{ marginTop: spacing.sm }} wrap>
              <Select<'writethrough' | 'writeback'>
                size="small"
                style={{ width: 280 }}
                value={cacheMode}
                disabled={cache.length === 0}
                onChange={setCacheMode}
                options={[
                  { value: 'writethrough', label: t('system.lvm.init.cacheMode.writeThrough') },
                  { value: 'writeback', label: t('system.lvm.init.cacheMode.writeBack') }
                ]}
              />
              <Select<string>
                size="small"
                style={{ width: 200 }}
                value={cachePolicy}
                disabled={cache.length === 0}
                onChange={setCachePolicy}
                options={[
                  { value: '', label: t('system.lvm.init.policyDefault') },
                  { value: 'smq', label: 'smq' },
                  { value: 'mq', label: 'mq' }
                ]}
              />
            </Space>
          </div>
        </Space>
      </Modal>
    </SectionCard>
  )
}