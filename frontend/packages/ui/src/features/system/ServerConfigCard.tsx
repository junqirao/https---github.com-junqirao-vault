import { useMemo, useState } from 'react'
import { Alert, Button, Input, InputNumber, Select, Space, Switch, Table, Tag, Tooltip, Typography } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { ErrorNotice } from '../../components/ErrorNotice'
import { SectionCard } from '../../components/SectionCard'
import { useApi } from '../../api/provider'
import type { ConfigField, ConfigResponse } from '../../api/types'
import { useI18n } from '../../i18n'
import { fontSize } from '../../tokens/palette'

/**
 * 服务端**全量配置表**。
 *
 * 目的（用户诉求）：让用户知道有哪些可配置项、各自的含义，并能在页面上直接改；
 * 页面上不能改的项**置灰**并说明原因（而不是隐藏或让人瞎猜）。
 *
 * 三类"不能改"各有各的原因，界面必须说清楚：
 *   - `editable=false`：改了会出事（主密钥/实例 ID）、真源在别处（存储、服务端名称由运行期覆盖）、
 *     或由构建/平台决定（platform.kind、iscsi.backend）；
 *   - `sensitive=true`：值含密钥/口令，只回 `***`，不接受页面修改；
 *   - `source=env`：当前值被环境变量覆盖，**改文件不会生效**，提示去改环境变量。
 */
export function ServerConfigCard({ isSuperAdmin }: { isSuperAdmin: boolean }): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const [error, setError] = useState<unknown>(null)
  const [section, setSection] = useState<string>('all')
  const [draft, setDraft] = useState<Record<string, string>>({})

  const query = useQuery({ queryKey: ['system-config'], queryFn: () => api.serverConfig(), enabled: isSuperAdmin })

  const saveMutation = useMutation({
    mutationFn: (settings: Record<string, string>) => api.patchConfig({ settings }),
    onSuccess: (data) => {
      setDraft({})
      setError(null)
      queryClient.setQueryData(['system-config'], data)
    },
    onError: (err: unknown) => setError(err)
  })

  const data = query.data
  const fields = useMemo<ConfigField[]>(() => data?.fields ?? [], [data])
  const sections = useMemo<string[]>(() => {
    const set = new Set<string>()
    for (const field of fields) set.add(field.section)
    return [...set].sort()
  }, [fields])
  const rows = useMemo<ConfigField[]>(
    () => (section === 'all' ? fields : fields.filter((item) => item.section === section)),
    [fields, section]
  )
  const dirty = Object.keys(draft).length

  const valueOf = (field: ConfigField): string => draft[field.key] ?? field.value

  /** 值控件：按类型渲染；不可编辑的一律 disabled（置灰）。 */
  const control = (field: ConfigField): JSX.Element => {
    const disabled = !field.editable || field.sensitive
    const setValue = (next: string): void => {
      setDraft((prev) => ({ ...prev, [field.key]: next }))
    }
    if (field.kind === 'bool') {
      return (
        <Switch
          size="small"
          disabled={disabled}
          checked={valueOf(field) === 'true'}
          onChange={(checked) => setValue(checked ? 'true' : 'false')}
        />
      )
    }
    if (field.kind === 'int' || field.kind === 'float') {
      return (
        <InputNumber
          size="small"
          disabled={disabled}
          style={{ width: '100%' }}
          value={valueOf(field) === '' ? null : Number(valueOf(field))}
          onChange={(value) => setValue(value === null || value === undefined ? '' : String(value))}
        />
      )
    }
    return (
      <Input
        size="small"
        disabled={disabled}
        value={valueOf(field)}
        placeholder={field.sensitive ? '***' : t('system.config.empty')}
        onChange={(event) => setValue(event.target.value)}
      />
    )
  }

  const columns: ColumnsType<ConfigField> = [
    {
      title: t('system.config.key'),
      dataIndex: 'key',
      width: 240,
      render: (_value, field) => (
        <Space direction="vertical" size={0}>
          <Typography.Text style={{ fontFamily: 'monospace', fontSize: fontSize.sm }}>{field.key}</Typography.Text>
          <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
            {field.section}
          </Typography.Text>
        </Space>
      )
    },
    {
      title: t('system.config.value'),
      dataIndex: 'value',
      width: 260,
      render: (_value, field) => (
        <Tooltip title={field.editable ? '' : field.editable_reason || t('system.config.readonly')}>
          <span style={{ display: 'inline-block', width: '100%' }}>{control(field)}</span>
        </Tooltip>
      )
    },
    {
      title: t('system.config.default'),
      dataIndex: 'default',
      width: 120,
      render: (_value, field) => (
        <Typography.Text type="secondary" style={{ fontFamily: 'monospace', fontSize: fontSize.sm }}>
          {field.default === '' ? '—' : field.default}
        </Typography.Text>
      )
    },
    {
      title: t('system.config.desc'),
      dataIndex: 'desc',
      render: (_value, field) => (
        <Space direction="vertical" size={2}>
          <Typography.Text style={{ fontSize: fontSize.sm }}>{field.desc}</Typography.Text>
          <Space size={4} wrap>
            {field.needs_restart ? <Tag color="orange">{t('system.config.needsRestart')}</Tag> : null}
            {field.sensitive ? <Tag>{t('system.config.sensitive')}</Tag> : null}
            {field.source === 'env' ? <Tag color="blue">{t('system.config.sourceEnv')}</Tag> : null}
            {!field.editable ? <Tag>{t('system.config.readonly')}</Tag> : null}
          </Space>
        </Space>
      )
    }
  ]

  return (
    <SectionCard title={t('system.config.title')} loading={query.isLoading}>
      <Space direction="vertical" size={12} style={{ width: '100%' }}>
        <Space wrap>
          <Select
            size="small"
            style={{ width: 200 }}
            value={section}
            onChange={setSection}
            options={[{ value: 'all', label: t('system.config.allSections') }, ...sections.map((item) => ({ value: item, label: item }))]}
          />
          <Button
            type="primary"
            size="small"
            disabled={dirty === 0}
            loading={saveMutation.isPending}
            onClick={() => saveMutation.mutate(draft)}
          >
            {t('system.config.save')}
          </Button>
          {dirty > 0 ? (
            <Button size="small" onClick={() => setDraft({})}>
              {t('common.cancel')}
            </Button>
          ) : null}
          <Typography.Text type="secondary" style={{ fontSize: fontSize.sm }}>
            {t('system.config.path')}: {data?.path || '-'}
          </Typography.Text>
        </Space>

        {data?.warning ? <Alert type="warning" showIcon message={data.warning} /> : null}
        {data?.restart_required ? <Alert type="warning" showIcon message={t('system.config.restartHint')} /> : null}
        {error ? <ErrorNotice error={error} /> : null}
        {query.error ? <ErrorNotice error={query.error} /> : null}

        <Table<ConfigField>
          rowKey="key"
          size="small"
          columns={columns}
          dataSource={rows}
          pagination={false}
          scroll={{ x: 900 }}
        />
      </Space>
    </SectionCard>
  )
}

/** 供外部在保存后刷新（如初始化向导改了配置）。 */
export type SystemConfigResponse = ConfigResponse
