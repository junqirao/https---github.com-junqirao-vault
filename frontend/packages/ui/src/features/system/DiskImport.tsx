import { useMemo, useState } from 'react'
import { Alert, Button, InputNumber, Select, Space, Table, Tag, Tooltip, Typography } from 'antd'
import type { TableColumnsType } from 'antd'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { ConfirmDialog } from '../../components/ConfirmDialog'
import { EmptyState } from '../../components/EmptyState'
import { ErrorNotice } from '../../components/ErrorNotice'
import { PageShell } from '../../components/PageShell'
import { useApi } from '../../api/provider'
import type {
  DiskImportCandidateDTO,
  DiskImportCapacitySource,
  DiskImportKind,
  DiskImportResultDTO,
  DiskImportSkipReason
} from '../../api/types'
import { useI18n } from '../../i18n'
import { fontSize, spacing } from '../../tokens/palette'
import { CAPACITY_UNIT, bytesToMegabytes, formatBytes, formatTime, megabytesToBytes } from '../../utils/format'

/** 候选清单的缓存键；导入成功后失效它即触发重新扫描。 */
const IMPORT_SCAN_KEY = ['system', 'disk-import']

export interface DiskImportProps {
  /** 返回存储库列表（导入是从那里进来的）。 */
  onBack: () => void
}

/** 类型配色：差异盘不可导入，用暖色让它在清单里跳出来。 */
const KIND_COLOR: Record<DiskImportKind, string> = {
  parent: 'blue',
  standalone: 'cyan',
  diff: 'orange',
  other: 'default'
}

/** 不可导入/失败原因 → 标签颜色。可导入与"已存在"都是正常态，保持中性。 */
const REASON_COLOR: Record<string, string> = {
  already_registered: 'default',
  diff_disk: 'orange',
  path_invalid: 'red',
  not_found: 'red',
  meta_unreadable: 'red',
  capacity_invalid: 'red',
  create_failed: 'red'
}

/** 每行可改的字段（只在用户真正改过时才有值；没改过就交给服务端按元数据/文件大小取值）。 */
interface RowOverride {
  name?: string
  /** 容量按 M 录入（与全站容量输入一致，见 CAPACITY_UNIT）。 */
  capacityM?: number
}

/**
 * 导入已有母盘（仅超级管理员）：把存储根里已存在的 VHDX 文件**原地**登记成存储库。
 *
 * 为什么单独一页而不是并进「孤儿磁盘」：两者扫的是同一批文件，处置却完全相反 ——
 * 孤儿页面处理"不要的"（删除），这一页处理"要的"（接管）。换服务端时把 disks/ 整个
 * 拷过来，那些文件在系统里什么都不是；这里让管理员把它们一条条认回来。
 *
 * 三条不可让步的规则：
 *  1. **不复制、不移动文件**：几十 GB 的母盘拷一次就够了，导入只是登记；
 *  2. **已有的跳过**：数据库里已有这条路径就不重复建库；
 *  3. **容量必须对**：客户端按容量精确匹配本地盘，容量错了永远挂不上，因此容量来源
 *     （元数据 / 手填 / 按文件大小估算）要明确标出来。
 */
export function DiskImport({ onBack }: DiskImportProps): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const queryClient = useQueryClient()

  const [ownerID, setOwnerID] = useState<string>()
  const [selected, setSelected] = useState<string[]>([])
  const [overrides, setOverrides] = useState<Record<string, RowOverride>>({})
  const [error, setError] = useState<unknown>(null)
  const [confirming, setConfirming] = useState(false)
  const [result, setResult] = useState<DiskImportResultDTO | null>(null)

  // 进页面即现扫一遍：管理员要看的是"此刻磁盘上真实有什么"，而不是某次对账的旧快照。
  const scanQuery = useQuery({
    queryKey: IMPORT_SCAN_KEY,
    queryFn: () => api.scanDiskImports(),
    refetchOnMount: 'always'
  })
  const scan = scanQuery.data
  const candidates = scan?.candidates ?? []
  const skipped = scan?.skipped ?? []

  const usersQuery = useQuery({
    queryKey: ['users', 'options'],
    queryFn: () => api.listUsers({ limit: 200 })
  })
  const ownerOptions = useMemo(
    () =>
      (usersQuery.data?.items ?? [])
        .filter((user) => user.enabled)
        .map((user) => ({ value: user.id, label: user.username })),
    [usersQuery.data]
  )

  const byPath = useMemo(() => {
    const map = new Map<string, DiskImportCandidateDTO>()
    for (const candidate of candidates) map.set(candidate.path, candidate)
    return map
  }, [candidates])

  /** 只勾选可导入的（不可导入的行复选框已禁用，这里再兜一层，防止清单刷新后残留选择）。 */
  const importableSelected = useMemo(
    () => selected.filter((path) => byPath.get(path)?.importable),
    [selected, byPath]
  )

  const patchOverride = (path: string, patch: RowOverride): void => {
    setOverrides((prev) => ({ ...prev, [path]: { ...prev[path], ...patch } }))
  }

  const importMutation = useMutation({
    mutationFn: () => {
      const items = importableSelected.map((path) => {
        const candidate = byPath.get(path)
        const override = overrides[path]
        const capacityM = override?.capacityM
        return {
          path,
          // 只在用户改过时才带上：没改就让服务端按元数据取，来源才是诚实的。
          name: override?.name?.trim() ? override.name.trim() : undefined,
          capacity_bytes:
            capacityM !== undefined && capacityM > 0 ? megabytesToBytes(capacityM) : undefined,
          // 备注在导入页不编辑：它属于"建好之后慢慢补"的信息，详情页的编辑框更顺手。
          note: undefined,
          _skipIfUnchanged: candidate === undefined
        }
      })
      const payload = items
        .filter((item) => !item._skipIfUnchanged)
        .map(({ _skipIfUnchanged: _ignored, ...rest }) => rest)
      return api.importDisks({ owner_id: ownerID ?? '', items: payload })
    },
    onSuccess: (res) => {
      setError(null)
      setResult(res)
      setConfirming(false)
      // 已经接管的盘在下次扫描里应变成"已存在（跳过）"：只有重新扫描才看得见。
      setSelected([])
      setOverrides({})
      void queryClient.invalidateQueries({ queryKey: IMPORT_SCAN_KEY })
      void queryClient.invalidateQueries({ queryKey: ['repos'] })
    },
    onError: (err) => setError(err)
  })

  const canSubmit = Boolean(ownerID) && importableSelected.length > 0

  const columns: TableColumnsType<DiskImportCandidateDTO> = [
    {
      title: t('diskImport.col.path'),
      dataIndex: 'path',
      key: 'path',
      render: (value: string, row) => (
        <Space direction="vertical" size={0} style={{ maxWidth: 460 }}>
          <Tooltip title={value}>
            <Typography.Text
              code
              style={{ maxWidth: 460 }}
              ellipsis
              copyable={{ text: value, tooltips: [t('common.copy'), t('common.copied')] }}
            >
              {value}
            </Typography.Text>
          </Tooltip>
          {/* 元数据文件路径无论如何都展示：导入失败时管理员要照着它去手工补一份。 */}
          {row.meta_path ? (
            <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }} ellipsis>
              {t('diskImport.metaPath')}: {row.meta_path}
            </Typography.Text>
          ) : null}
        </Space>
      )
    },
    {
      title: t('diskImport.col.kind'),
      dataIndex: 'kind',
      key: 'kind',
      width: 140,
      render: (kind: DiskImportKind) =>
        kind === 'diff' ? (
          <Tooltip title={t('diskImport.kindHint.diff')}>
            <Tag color={KIND_COLOR[kind]}>{t(`diskImport.kind.${kind}`)}</Tag>
          </Tooltip>
        ) : (
          <Tag color={KIND_COLOR[kind]}>{t(`diskImport.kind.${kind}`)}</Tag>
        )
    },
    {
      title: t('diskImport.col.meta'),
      key: 'meta',
      render: (_value, row) => {
        const meta = row.meta
        if (row.meta_error) {
          return (
            <Tooltip title={row.meta_error}>
              <Typography.Text type="danger" style={{ fontSize: fontSize.sm }}>
                {t('diskImport.status.meta_unreadable')}
              </Typography.Text>
            </Tooltip>
          )
        }
        if (!meta) {
          return (
            <Typography.Text type="secondary" style={{ fontSize: fontSize.sm }}>
              {t('diskImport.noMeta')}
            </Typography.Text>
          )
        }
        return (
          <Space direction="vertical" size={0} style={{ maxWidth: 320 }}>
            <Typography.Text strong ellipsis style={{ maxWidth: 320 }}>
              {meta.name || row.suggested_name}
            </Typography.Text>
            {meta.note ? (
              <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }} ellipsis>
                {meta.note}
              </Typography.Text>
            ) : null}
            {meta.source?.dir ? (
              <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }} ellipsis>
                {t('diskImport.sourceDir')}: {meta.source.dir}
              </Typography.Text>
            ) : null}
          </Space>
        )
      }
    },
    {
      title: t('diskImport.col.capacity'),
      key: 'capacity',
      width: 260,
      render: (_value, row) => {
        const override = overrides[row.path]
        const source: DiskImportCapacitySource = override?.capacityM !== undefined ? 'manual' : row.capacity_source
        return (
          <Space direction="vertical" size={2} style={{ width: '100%' }}>
            {row.importable ? (
              <InputNumber
                min={0}
                addonAfter={CAPACITY_UNIT}
                style={{ width: '100%' }}
                // 表单里存 M、接口收字节：与全站容量输入同一套换算（见 utils/format）。
                value={override?.capacityM ?? bytesToMegabytes(row.suggested_capacity_bytes)}
                onChange={(next) => patchOverride(row.path, { capacityM: next ?? undefined })}
              />
            ) : (
              <Typography.Text>{formatBytes(row.suggested_capacity_bytes)}</Typography.Text>
            )}
            <Tooltip title={t('diskImport.capacityHint')}>
              <Tag color={source === 'file' ? 'orange' : source === 'meta' ? 'green' : 'blue'}>
                {t(`diskImport.capacity.${source}`)}
              </Tag>
            </Tooltip>
          </Space>
        )
      }
    },
    {
      title: t('diskImport.col.size'),
      dataIndex: 'size_bytes',
      key: 'size_bytes',
      width: 110,
      render: (value: number) => formatBytes(value)
    },
    {
      title: t('diskImport.col.modifiedAt'),
      dataIndex: 'modified_at',
      key: 'modified_at',
      width: 170,
      render: (value: number) => formatTime(value)
    },
    {
      title: t('diskImport.col.status'),
      key: 'status',
      width: 150,
      render: (_value, row) => {
        if (row.importable) {
          return <Tag color="green">{t('diskImport.status.importable')}</Tag>
        }
        const reason = (row.skip_reason ?? 'path_invalid') as DiskImportSkipReason
        return <Tag color={REASON_COLOR[reason] ?? 'default'}>{t(`diskImport.status.${reason}`)}</Tag>
      }
    }
  ]

  /** 已导入/跳过/失败的清单，逐条列出（迁移时几十块盘，汇总数字看不出是哪块出了问题）。 */
  const renderResult = (): JSX.Element | null => {
    if (!result) return null
    const estimated = result.imported.filter((item) => item.capacity_estimated).length
    const lines: Array<{ label: string; items: string[]; color: string }> = [
      {
        label: t('diskImport.result.imported'),
        items: result.imported.map((item) => `${item.path} → ${item.name}`),
        color: 'green'
      },
      {
        label: t('diskImport.result.skipped'),
        items: result.skipped.map((item) => `${item.path}（${t(`diskImport.status.${item.code}`)}）`),
        color: 'orange'
      },
      {
        label: t('diskImport.result.failed'),
        items: result.failed.map((item) => `${item.path}（${t(`diskImport.status.${item.code}`)}）`),
        color: 'red'
      }
    ]
    return (
      <Alert
        type={result.failed.length > 0 ? 'warning' : 'success'}
        showIcon
        style={{ marginBottom: spacing.md }}
        message={t('diskImport.resultTitle')}
        description={
          <Space direction="vertical" size={spacing.xs}>
            <Typography.Text>
              {t('diskImport.resultSummary', {
                imported: result.imported.length,
                skipped: result.skipped.length,
                failed: result.failed.length
              })}
            </Typography.Text>
            {estimated > 0 ? (
              <Typography.Text type="warning" style={{ fontSize: fontSize.sm }}>
                {t('diskImport.resultEstimated', { count: estimated })}
              </Typography.Text>
            ) : null}
            {lines
              .filter((line) => line.items.length > 0)
              .map((line) => (
                <div key={line.label}>
                  <Typography.Text strong style={{ fontSize: fontSize.sm }}>
                    {line.label}
                  </Typography.Text>
                  <Space direction="vertical" size={0} style={{ display: 'flex', marginTop: 2 }}>
                    {line.items.map((item) => (
                      <Typography.Text
                        key={item}
                        type={line.color === 'red' ? 'danger' : 'secondary'}
                        style={{ fontSize: fontSize.xs }}
                      >
                        {item}
                      </Typography.Text>
                    ))}
                  </Space>
                </div>
              ))}
          </Space>
        }
      />
    )
  }

  return (
    <PageShell
      title={t('diskImport.title')}
      subtitle={t('diskImport.subtitle')}
      extra={
        <>
          <Button onClick={onBack}>{t('diskImport.back')}</Button>
          <Button
            loading={scanQuery.isFetching}
            onClick={() => {
              setError(null)
              // 重新扫描后旧的勾选与手改值不一定还对得上，清掉最稳。
              setSelected([])
              setOverrides({})
              void scanQuery.refetch()
            }}
          >
            {t('diskImport.rescan')}
          </Button>
        </>
      }
    >
      {error ? <ErrorNotice error={error} /> : null}
      {scanQuery.error ? <ErrorNotice error={scanQuery.error} /> : null}
      {renderResult()}

      <Alert type="info" showIcon message={t('diskImport.hint')} style={{ marginBottom: spacing.sm }} />
      <Alert type="warning" showIcon message={t('diskImport.capacityHint')} style={{ marginBottom: spacing.md }} />

      {skipped.length > 0 ? (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: spacing.md }}
          message={t('diskImport.skipped')}
          description={
            <Space direction="vertical" size={0}>
              {skipped.map((reason) => (
                <Typography.Text key={reason} style={{ fontSize: 12 }}>
                  {reason}
                </Typography.Text>
              ))}
            </Space>
          }
        />
      ) : null}

      <Space wrap style={{ marginBottom: spacing.sm }}>
        <Space size={spacing.xs}>
          <Typography.Text>{t('diskImport.owner')}</Typography.Text>
          <Select
            style={{ width: 220 }}
            placeholder={t('diskImport.ownerPlaceholder')}
            options={ownerOptions}
            loading={usersQuery.isLoading}
            value={ownerID}
            onChange={setOwnerID}
            showSearch
            optionFilterProp="label"
          />
        </Space>
        <Button
          type="primary"
          disabled={!canSubmit}
          onClick={() => {
            setError(null)
            setConfirming(true)
          }}
        >
          {t('diskImport.import', { count: importableSelected.length })}
        </Button>
        {scan ? (
          <Typography.Text type="secondary" style={{ fontSize: fontSize.sm }}>
            {candidates.length > 0
              ? t('diskImport.summary', {
                  checked: scan.checked,
                  count: scan.importable_count,
                  selected: importableSelected.length
                })
              : t('diskImport.checkedOnly', { checked: scan.checked })}
          </Typography.Text>
        ) : null}
      </Space>

      <Table<DiskImportCandidateDTO>
        rowKey={(row) => row.path}
        columns={columns}
        dataSource={candidates}
        loading={scanQuery.isLoading}
        size="small"
        scroll={{ x: 1400 }}
        pagination={{ pageSize: 20, hideOnSinglePage: true, showSizeChanger: false }}
        locale={{ emptyText: <EmptyState title={t('diskImport.empty')} description={t('diskImport.emptyDesc')} /> }}
        rowSelection={{
          selectedRowKeys: selected,
          onChange: (keys) => setSelected(keys as string[]),
          // 不可导入的行不许勾：勾了也只会在导入时被跳过，徒增困惑。
          getCheckboxProps: (row) => ({ disabled: !row.importable })
        }}
      />

      <ConfirmDialog
        open={confirming}
        title={t('diskImport.confirmTitle')}
        content={
          <>
            <div>{t('diskImport.confirmContent', { count: importableSelected.length })}</div>
            <Typography.Text type="secondary" style={{ display: 'block', marginTop: spacing.xs, fontSize: fontSize.sm }}>
              {t('diskImport.confirmOwner', { name: ownerOptions.find((option) => option.value === ownerID)?.label ?? '-' })}
            </Typography.Text>
            {error ? (
              <div style={{ marginTop: spacing.sm }}>
                <ErrorNotice error={error} />
              </div>
            ) : null}
          </>
        }
        loading={importMutation.isPending}
        onConfirm={() => {
          if (!canSubmit) return
          importMutation.mutate()
        }}
        onCancel={() => setConfirming(false)}
      />
    </PageShell>
  )
}
