import { useState } from 'react'
import { Alert, Button, Collapse, Descriptions, Modal, Select, Space, Typography } from 'antd'
import { useMutation } from '@tanstack/react-query'

import { CopyableText } from '../../components/CopyableText'
import { ErrorNotice } from '../../components/ErrorNotice'
import { useApi } from '../../api/provider'
import type { AllocationDTO, MountSpec } from '../../api/types'
import { useI18n } from '../../i18n'
import { formatBytes } from '../../utils/format'

export interface MountSpecDebugProps {
  allocations: AllocationDTO[]
  userName: Map<string, string>
  clientId: string
}

/**
 * 挂载参数调试入口（可折叠）。
 *
 * 直接调用服务端 `POST /v1/allocations/{id}/mount` 获取一次性 MountSpec，
 * 用于核对参数；正式挂载流程由本地代理执行。
 */
export function MountSpecDebug({ allocations, userName, clientId }: MountSpecDebugProps): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const [allocationId, setAllocationId] = useState<string | undefined>(undefined)
  const [spec, setSpec] = useState<MountSpec | null>(null)
  const [error, setError] = useState<unknown>(null)

  const mutation = useMutation({
    mutationFn: (id: string) => api.mountAllocation(id, clientId),
    onSuccess: (result) => setSpec(result),
    onError: (err) => setError(err)
  })

  return (
    <>
      {error ? <ErrorNotice error={error} /> : null}
      <Collapse
        ghost
        style={{ marginTop: 16 }}
        items={[
          {
            key: 'debug',
            label: t('agent.action.mountDebug'),
            children: (
              <Space direction="vertical" size={12} style={{ width: '100%' }}>
                <Space wrap>
                  <Select
                    value={allocationId}
                    placeholder={t('repo.assign.user')}
                    style={{ width: 260 }}
                    onChange={setAllocationId}
                    optionFilterProp="label"
                    options={allocations.map((allocation) => ({
                      value: allocation.id,
                      label: `${userName.get(allocation.user_id) ?? allocation.user_id} · ${allocation.id.slice(0, 8)}`
                    }))}
                  />
                  <Button
                    disabled={!allocationId}
                    loading={mutation.isPending}
                    onClick={() => allocationId && mutation.mutate(allocationId)}
                  >
                    {t('action.mount')}
                  </Button>
                </Space>
                <Typography.Text type="secondary">{t('repo.mount.desc')}</Typography.Text>
              </Space>
            )
          }
        ]}
      />

      <Modal
        open={spec !== null}
        title={t('repo.mount.title')}
        footer={<Button onClick={() => setSpec(null)}>{t('common.close')}</Button>}
        onCancel={() => setSpec(null)}
        width={640}
      >
        {spec ? (
          <>
            <Alert type="info" showIcon message={t('repo.mount.placeholderNote')} style={{ marginBottom: 16 }} />
            <Descriptions size="small" column={1} bordered>
              <Descriptions.Item label={t('repo.mount.target')}>{spec.target_iqn}</Descriptions.Item>
              <Descriptions.Item label={t('repo.mount.portal')}>
                {spec.portal_address}:{spec.portal_port}
              </Descriptions.Item>
              <Descriptions.Item label={t('field.mountPoint')}>{spec.mount_path}</Descriptions.Item>
              <Descriptions.Item label={t('repo.mount.diskSize')}>{formatBytes(spec.disk_size_bytes)}</Descriptions.Item>
              <Descriptions.Item label={t('repo.mount.leaseTtl')}>{spec.lease_ttl_seconds}</Descriptions.Item>
              <Descriptions.Item label={t('field.username')}>{spec.chap_user || '-'}</Descriptions.Item>
              <Descriptions.Item label="CHAP">
                <CopyableText value={spec.chap_secret} masked monospace />
              </Descriptions.Item>
            </Descriptions>
          </>
        ) : null}
      </Modal>
    </>
  )
}
