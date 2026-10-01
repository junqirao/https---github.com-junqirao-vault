import { Alert, Table, Tag, Typography } from 'antd'

import type { BlockDeviceDTO } from '../../api/types'
import { useI18n } from '../../i18n'
import { formatBytes } from '../../utils/format'

export interface BlockDevicePickerProps {
  /** 期望用途：vg（组成卷组的数据盘）| cache（SSD 加速盘）。仅影响提示文案。 */
  purpose: 'vg' | 'cache'
  devices: BlockDeviceDTO[]
  loading?: boolean
  /** 已选中的设备全路径。 */
  value: string[]
  onChange: (paths: string[]) => void
}

/**
 * 块设备选择器：列出服务端可见的整盘设备，供"组成卷组 / 选作 dm-cache 缓存盘"。
 *
 * 不可选设备（系统盘、已挂载/有文件系统、已属其它卷组、有子分区）由服务端在
 * `reason` 字段给出原因并**禁用勾选**——前端不重复实现判定逻辑，避免两边规则漂移。
 */
export function BlockDevicePicker({ purpose, devices, loading, value, onChange }: BlockDevicePickerProps): JSX.Element {
  const { t } = useI18n()
  return (
    <>
      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 12 }}
        message={t('system.lvm.device.systemWarn')}
        description={purpose === 'cache' ? t('system.lvm.init.cacheHint') : t('system.lvm.init.hddHint')}
      />
      <Table<BlockDeviceDTO>
        size="small"
        rowKey={(d) => d.path}
        loading={loading}
        dataSource={devices}
        pagination={false}
        scroll={{ y: 260 }}
        locale={{ emptyText: t('common.empty') }}
        rowSelection={{
          selectedRowKeys: value,
          onChange: (keys) => onChange(keys.map(String)),
          getCheckboxProps: (d) => ({ disabled: Boolean(d.reason) })
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
            title: t('common.status'),
            key: 'status',
            render: (_: unknown, d) =>
              d.reason ? (
                <Typography.Text type="secondary">{d.reason}</Typography.Text>
              ) : (
                <Tag color="blue">{t('system.lvm.device.selectable')}</Tag>
              )
          }
        ]}
      />
    </>
  )
}