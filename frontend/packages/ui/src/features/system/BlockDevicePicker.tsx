import { useState } from 'react'
import { Alert, Button, Modal, Table, Tag, Tooltip, Typography } from 'antd'
import { ReloadOutlined } from '@ant-design/icons'
import { useMutation } from '@tanstack/react-query'

import type { BlockDeviceDTO, DeviceReleaseReportDTO } from '../../api/types'
import { useApi } from '../../api/provider'
import { ConfirmDialog } from '../../components/ConfirmDialog'
import { ErrorNotice } from '../../components/ErrorNotice'
import { useI18n } from '../../i18n'
import { formatBytes } from '../../utils/format'

export interface BlockDevicePickerProps {
  devices: BlockDeviceDTO[]
  loading?: boolean
  /**
   * 允许对不可用设备执行"释放"（卸载/抹签名/移出卷组）。
   *
   * 默认关闭：这是破坏性动作，只在"新建存储"这种明确要挑盘入卷组的场景开放。
   */
  allowRelease?: boolean
  /** 释放成功后回调：父组件据此刷新设备列表与存储池目录。 */
  onReleased?: () => void
  /**
   * 已被别处占用的设备全路径（如"已经选作容量盘"）。
   *
   * 这些盘**禁用勾选**并在状态列直接给出原因：同一块盘不能既承载 thin pool
   * 又承载 cache pool，让人在缓存选择器里一眼看清"哪块已经被占"，
   * 比等到提交时被后端拒绝要好得多。
   */
  usedPaths?: string[]
  /** 展示给用户的原因文案（配合 usedPaths 使用）。 */
  usedReason?: string
  /** 已选中的设备全路径。 */
  value?: string[]
  /** value/onChange 可缺省：本组件可直接放进 Form.Item 由表单注入。 */
  onChange?: (paths: string[]) => void
}

/**
 * 块设备选择器：列出服务端可见的整盘设备，供"组成卷组 / 选作 dm-cache 缓存盘"。
 *
 * 不可选设备（系统盘、已挂载/有文件系统、已属其它卷组、有子分区）由服务端在
 * `reason` 字段给出原因并**禁用勾选**——前端不重复实现判定逻辑，避免两边规则漂移。
 *
 * 开启 allowRelease 后，不可用设备可一键"释放"：服务端逐步执行并把每一步结果回传，
 * 这里原样展示（成功/跳过/失败），卡在哪一步一目了然。
 */
export function BlockDevicePicker({
  devices,
  loading,
  allowRelease = false,
  onReleased,
  usedPaths = [],
  usedReason,
  value = [],
  onChange
}: BlockDevicePickerProps): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const [pending, setPending] = useState<BlockDeviceDTO | null>(null)
  const [report, setReport] = useState<DeviceReleaseReportDTO | null>(null)
  const used = new Set(usedPaths)

  const releaseMutation = useMutation({
    mutationFn: (path: string) => api.releaseBlockDevice(path),
    onSuccess: (result) => {
      setPending(null)
      setReport(result)
      onReleased?.()
    }
  })

  const closeRelease = (): void => {
    releaseMutation.reset()
    setPending(null)
  }

  return (
    <>
      <Table<BlockDeviceDTO>
        size="small"
        rowKey={(d) => d.path}
        loading={loading}
        dataSource={devices}
        pagination={false}
        scroll={{ y: 260, x: 960 }}
        locale={{ emptyText: t('common.empty') }}
        rowSelection={{
          selectedRowKeys: value,
          onChange: (keys) => onChange?.(keys.map(String)),
          getCheckboxProps: (d) => ({ disabled: Boolean(d.reason) || used.has(d.path) })
        }}
        columns={[
          {
            title: t('system.lvm.device.name'),
            key: 'name',
            render: (_: unknown, d) => <Typography.Text code>{d.path}</Typography.Text>
          },
          {
            title: t('common.size'),
            dataIndex: 'size_bytes',
            key: 'size',
            render: (v: number) => formatBytes(v)
          },
          {
            title: t('system.lvm.device.model'),
            dataIndex: 'model',
            key: 'model',
            render: (v: string) => v || '-'
          },
          {
            title: t('system.lvm.device.kind'),
            dataIndex: 'rotational',
            key: 'kind',
            render: (rotational: boolean) => (
              <Tag color={rotational ? 'default' : 'green'}>
                {rotational ? t('system.lvm.device.hdd') : t('system.lvm.device.ssd')}
              </Tag>
            )
          },
          {
            // 只讲"能不能选"：原因本身是数据，动作另起一列，两者不挤在一格里。
            title: t('common.status'),
            key: 'status',
            render: (_: unknown, d) =>
              used.has(d.path) ? (
                <Typography.Text type="secondary">
                  {usedReason ?? t('system.lvm.device.inUse')}
                </Typography.Text>
              ) : d.reason ? (
                <Typography.Text type="secondary">{d.reason}</Typography.Text>
              ) : (
                <Tag color="blue">{t('system.lvm.device.selectable')}</Tag>
              )
          },
          // 动作列只在开放"释放"时出现：不给只读的选盘场景留一列空白。
          ...(allowRelease
            ? [
                {
                  title: t('common.actions'),
                  key: 'actions',
                  width: 96,
                  render: (_: unknown, d: BlockDeviceDTO) =>
                    // 已被本次创建占用的盘（如已选作容量盘）不给"释放"：
                    // 那会把这台机器上刚挑好的盘从选择里抹掉。
                    d.reason && !used.has(d.path) ? (
                      <Button
                        type="link"
                        size="small"
                        danger
                        onClick={() => {
                          releaseMutation.reset()
                          setPending(d)
                        }}
                      >
                        {t('system.lvm.device.release')}
                      </Button>
                    ) : (
                      <Typography.Text type="secondary">-</Typography.Text>
                    )
                }
              ]
            : [])
        ]}
      />

      <ConfirmDialog
        open={pending !== null}
        danger
        title={t('system.lvm.device.release')}
        content={
          <>
            <div>{t('system.lvm.device.releaseConfirm', { path: pending?.path ?? '' })}</div>
            <div style={{ marginTop: 8 }}>
              <Typography.Text type="danger">{t('system.lvm.device.releaseWarn')}</Typography.Text>
            </div>
            {releaseMutation.error ? (
              <div style={{ marginTop: 12 }}>
                <ErrorNotice error={releaseMutation.error} />
              </div>
            ) : null}
          </>
        }
        loading={releaseMutation.isPending}
        onConfirm={() => {
          if (!pending) return
          releaseMutation.mutate(pending.path)
        }}
        onCancel={closeRelease}
      />

      <Modal
        open={report !== null}
        title={t('system.lvm.device.releaseResult')}
        width={640}
        centered
        footer={
          <Button type="primary" onClick={() => setReport(null)}>
            {t('common.close')}
          </Button>
        }
        onCancel={() => setReport(null)}
      >
        {report ? (
          <>
            <Alert
              type={report.released ? 'success' : 'warning'}
              showIcon
              style={{ marginBottom: 12 }}
              message={report.path}
              description={report.released ? t('system.lvm.device.releaseOk') : report.reason}
            />
            <Table<DeviceReleaseReportDTO['steps'][number]>
              size="small"
              rowKey={(_, index) => String(index)}
              dataSource={report.steps}
              pagination={false}
              scroll={{ y: 300 }}
              columns={[
                { title: t('system.lvm.device.step'), dataIndex: 'step', key: 'step', width: 110 },
                {
                  title: t('system.lvm.device.stepTarget'),
                  dataIndex: 'target',
                  key: 'target',
                  width: 160,
                  render: (v: string) => v || '-'
                },
                {
                  title: t('common.status'),
                  key: 'status',
                  width: 90,
                  render: (_: unknown, s) =>
                    s.ok ? (
                      <Tag color="green">{t('system.lvm.device.stepDone')}</Tag>
                    ) : s.skipped ? (
                      <Tag>{t('system.lvm.device.stepSkipped')}</Tag>
                    ) : (
                      <Tag color="red">{t('system.lvm.device.stepFailed')}</Tag>
                    )
                },
                {
                  title: t('system.lvm.device.stepDetail'),
                  dataIndex: 'detail',
                  key: 'detail',
                  render: (v: string) => (v ? <Typography.Text type="secondary">{v}</Typography.Text> : '-')
                }
              ]}
            />
          </>
        ) : null}
      </Modal>
    </>
  )
}

export interface DeviceRefreshButtonProps {
  /** 正在重新枚举设备：图标转起来并停止响应点击，避免连点。 */
  loading?: boolean
  /** 重新枚举设备；不传则不渲染（只读场景没有"刷新"可言）。 */
  onRefresh?: () => void
}

/**
 * 选盘器的"刷新"图标按钮：挂在**设置项标题后面**（如「缓存设备 ⟳」）。
 *
 * 不放在表格上方：那样每张表格都要多出一条只有按钮的空行，而"重新枚举设备"
 * 是低频动作——图标加悬停提示已经足够被发现，版面留给真正要看的磁盘列表。
 */
export function DeviceRefreshButton({ loading, onRefresh }: DeviceRefreshButtonProps): JSX.Element | null {
  const { t } = useI18n()
  if (!onRefresh) return null
  return (
    <Tooltip title={t('common.refresh')}>
      <Button
        type="text"
        size="small"
        disabled={loading}
        aria-label={t('common.refresh')}
        icon={<ReloadOutlined spin={loading} />}
        onClick={onRefresh}
      />
    </Tooltip>
  )
}
