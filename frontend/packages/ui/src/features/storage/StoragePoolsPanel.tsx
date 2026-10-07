import { useMemo, useState } from 'react'
import { Alert, App, Button, Checkbox, Empty, Modal, Progress, Space, Table, Tag, Tooltip, Typography } from 'antd'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { ErrorNotice } from '../../components/ErrorNotice'
import { translateError } from '../../api/errors'
import { useApi } from '../../api/provider'
import type { DeviceReleaseStepDTO, PoolDeleteResult, StorageDTO } from '../../api/types'
import { useI18n } from '../../i18n'
import { spacing } from '../../tokens/palette'
import { formatBytes } from '../../utils/format'
import { PoolCreateModal } from './PoolCreateModal'

/** 一行：一个存储池（LVM thin pool）。 */
interface PoolRow {
  /** antd 行键：`<vg>/<thin_pool>`。 */
  rowKey: string
  vg: string
  /** 池名，即池键的后半段；删除接口按 `vg` + `thin_pool` 定位。 */
  thinPool: string
  /** 池键 `<vg>/<thin_pool>`。 */
  key: string
  default: boolean
  exists: boolean
  sizeBytes: number
  freeBytes: number
  dataPercent: number
  metadataPercent: number
  cacheAttached: boolean
  cacheMode?: string
  /** 该池上的存储数量（按 `pool_ref` 匹配；空 `pool_ref` 的历史存储算在默认池上）。 */
  storageCount: number
  storageNames: string[]
}

export interface StoragePoolsPanelProps {
  open: boolean
  onClose: () => void
}

/**
 * 「存储池管理」面板。
 *
 * 只做管理（列出 / 新建 / 删除池），**不**在这里建存储——建存储仍走 StorageList 的新建流程，
 * 免得两条建存储的路径各自演化。删除是破坏性动作：默认池、以及挂着存储的池一律在
 * 前端就置灰（后端也会兜住，前端拦一道只是为了不用等一次失败的往返）。
 *
 * 列表里**只有存储池**，不再把"还没建池的空卷组"当成可管理的行。原先那条路径（对空卷组
 * 执行 `vgremove`）能把系统盘所在的卷组整个交给用户去点——那台机器上的卷组多半是装系统时
 * 建的，本服务从未参与，不该由本面板替用户处置（真机反馈：`ubuntu-vg` 就摆在列表里带着
 * 一个"删除卷组"按钮）。空卷组仍然可以在「新建存储池」里被选作落点，那是用户显式指定的动作。
 */
export function StoragePoolsPanel({ open, onClose }: StoragePoolsPanelProps): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const { message } = App.useApp()

  const [createOpen, setCreateOpen] = useState(false)
  /** 待确认删除的行；非空即弹确认框。 */
  const [deleteTarget, setDeleteTarget] = useState<PoolRow | null>(null)
  /**
   * 确认框里的"同时删除卷组"：默认勾选。
   *
   * 卷组删不删必须是用户**看着复选框**决定的，不能由前端按"卷组里只有一个池"悄悄推断出来
   * ——那个判断留给后端（拿不准就别猜）。
   */
  const [removeVg, setRemoveVg] = useState(true)
  /**
   * 确认框里的"释放磁盘"（pvremove 清 PV 标签）：默认勾选。
   *
   * 它只在"同时删除卷组"勾上时才有意义——卷组没删就没有 PV 可清，所以 UI 上随 removeVg 联动置灰，
   * 而不是让用户勾了、提交后被服务端忽略（勾选框要么真的会被执行，要么就别让勾）。
   */
  const [releaseDevices, setReleaseDevices] = useState(true)
  // 删除返回的逐步结果：非空即弹结果弹窗（已执行的步骤不会回滚，必须让用户看到卡在哪）。
  const [deleteResult, setDeleteResult] = useState<PoolDeleteResult | null>(null)

  const poolsQuery = useQuery({
    queryKey: ['pools'],
    queryFn: () => api.listPools(),
    enabled: open
  })
  // 存储列表只为"这个池上有几个存储"服务：池上有存储就删不掉，得先让用户看见原因。
  const storagesQuery = useQuery({
    queryKey: ['storages', 'list'],
    queryFn: () => api.listStorages(),
    enabled: open
  })

  const catalog = poolsQuery.data
  /** 目录加载完才开始用缓存/写回值，避免把空目录当成"后端说没有池"。 */
  const loading = poolsQuery.isLoading

  const refresh = (): void => {
    void queryClient.invalidateQueries({ queryKey: ['pools'] })
    void queryClient.invalidateQueries({ queryKey: ['block-devices'] })
  }

  const rows = useMemo<PoolRow[]>(() => {
    const storages: StorageDTO[] = storagesQuery.data?.items ?? []
    const byPool = new Map<string, StorageDTO[]>()
    const legacyDefault: StorageDTO[] = []
    for (const storage of storages) {
      // `pool_ref` 为空 = 服务端默认池（历史存储建池信息缺失时的口径，与后端一致）。
      if (!storage.pool_ref) {
        legacyDefault.push(storage)
        continue
      }
      const list = byPool.get(storage.pool_ref)
      if (list) list.push(storage)
      else byPool.set(storage.pool_ref, [storage])
    }

    return (catalog?.items ?? []).map((pool) => {
      const attached = byPool.get(pool.key) ?? (pool.default ? legacyDefault : [])
      return {
        rowKey: pool.key,
        vg: pool.vg,
        thinPool: pool.thin_pool,
        key: pool.key,
        default: pool.default,
        exists: pool.exists,
        sizeBytes: pool.size_bytes,
        freeBytes: pool.free_bytes,
        dataPercent: pool.data_percent,
        metadataPercent: pool.metadata_percent,
        cacheAttached: pool.cache_attached,
        cacheMode: pool.cache_mode,
        storageCount: attached.length,
        storageNames: attached.map((storage) => storage.name)
      }
    })
  }, [catalog, storagesQuery.data])

  const deleteMutation = useMutation({
    mutationFn: (row: PoolRow) =>
      api.deletePool({
        vg: row.vg,
        thin_pool: row.thinPool,
        remove_volume_group: removeVg,
        // 释放磁盘只在卷组被删时才有 PV 可清；置灰的勾选框不该再被读进来，这里再兜一道。
        release_devices: removeVg && releaseDevices
      }),
    onSuccess: (result) => {
      message.success(t('storage.pools.deleted'))
      setDeleteTarget(null)
      setDeleteResult(result)
      refresh()
    }
    // 失败不关确认框：服务端会带 code + args 回来（pool_in_use / pool_protected / not_found），
    // 由框内的 ErrorNotice 经错误映射翻译后展示，用户能直接看到"为什么删不掉"。
  })

  const closeDelete = (): void => {
    deleteMutation.reset()
    setDeleteTarget(null)
  }

  /** 关闭结果弹窗时顺带清掉错误状态，免得下次打开还带着上一次的失败信息。 */
  const closeResult = (): void => {
    setDeleteResult(null)
  }

  // `released_devices` 后端是 omitempty（没释放任何盘时字段不出现），steps 正常都会带上；
  // 两个都兜成数组，免得字段缺失时把结果弹窗渲染崩掉。
  const releasedDevices = deleteResult?.released_devices ?? []
  const deleteSteps = deleteResult?.steps ?? []

  return (
    <Modal
      open={open}
      title={t('storage.pools.manage')}
      width={960}
      centered
      maskClosable={false}
      // 关闭只保留右上角的 X（走 onCancel）：底部再放一个「关闭」是重复入口，
      // 而这里原先既没给 onCancel、又只留了底部按钮，X 点了毫无反应（真机反馈）。
      onCancel={onClose}
      footer={null}
    >
      {/* 「新建」放在内容区右上角（Modal 没有 extra 槽位，antd 只有 Drawer 有）。 */}
      <Space
        align="start"
        style={{ width: '100%', justifyContent: 'space-between', marginBottom: spacing.sm }}
      >
        <Typography.Text type="secondary" style={{ maxWidth: 620 }}>
          {t('storage.pools.subtitle')}
        </Typography.Text>
        <Button type="primary" onClick={() => setCreateOpen(true)}>
          {t('storage.pools.create')}
        </Button>
      </Space>

      {poolsQuery.isError ? (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: spacing.sm }}
          message={t('storage.pools.loadFailed')}
          description={translateError(poolsQuery.error)}
        />
      ) : null}

      <Table<PoolRow>
        size="small"
        rowKey="rowKey"
        loading={loading}
        dataSource={rows}
        pagination={false}
        locale={{ emptyText: <Empty description={t('storage.pools.empty')} /> }}
        columns={[
          {
            title: t('storage.pools.column.pool'),
            dataIndex: 'key',
            key: 'key',
            render: (_: unknown, row) => <Typography.Text code>{row.key}</Typography.Text>
          },
          {
            title: t('storage.pools.column.tags'),
            key: 'tags',
            render: (_: unknown, row) => (
              <Space size={4} wrap>
                {row.default ? <Tag color="blue">{t('storage.pools.tag.default')}</Tag> : null}
                {!row.exists ? <Tag color="orange">{t('storage.pools.tag.missing')}</Tag> : null}
              </Space>
            )
          },
          {
            title: t('storage.pools.column.size'),
            key: 'size',
            render: (_: unknown, row) => formatBytes(row.sizeBytes)
          },
          {
            title: t('storage.pools.column.free'),
            key: 'free',
            render: (_: unknown, row) => formatBytes(row.freeBytes)
          },
          {
            title: t('storage.pools.column.data'),
            key: 'data',
            render: (_: unknown, row) => (
              <Tooltip
                title={t('storage.pools.metadataPercent', {
                  percent: Math.round(row.metadataPercent)
                })}
              >
                <Progress
                  percent={Math.min(100, Math.max(0, Math.round(row.dataPercent)))}
                  size="small"
                  style={{ minWidth: 90, marginBottom: 0 }}
                />
              </Tooltip>
            )
          },
          {
            title: t('storage.pools.column.cache'),
            key: 'cache',
            render: (_: unknown, row) =>
              row.cacheAttached ? (
                <Tag color="green">
                  {t('storage.pools.tag.cacheOn')}
                  {row.cacheMode ? ` · ${row.cacheMode}` : ''}
                </Tag>
              ) : (
                <Typography.Text type="secondary">—</Typography.Text>
              )
          },
          {
            title: t('storage.pools.column.storages'),
            key: 'storages',
            render: (_: unknown, row) =>
              row.storageCount > 0 ? (
                <Tooltip title={t('storage.pools.storagesOnPool', { names: row.storageNames.join(', ') })}>
                  <Tag color="gold">{row.storageCount}</Tag>
                </Tooltip>
              ) : (
                <Typography.Text type="secondary">0</Typography.Text>
              )
          },
          {
            title: t('common.actions'),
            key: 'actions',
            render: (_: unknown, row) => {
              // 前端先拦一道：默认池删掉服务就没了，池上还有存储的删了会丢数据。
              // 后端也会兜住（pool_protected / pool_in_use），这里只是不用等一次失败的往返。
              const disabledReason = row.default
                ? t('storage.pools.deleteDefaultTooltip')
                : row.storageCount > 0
                  ? t('storage.pools.deleteInUseTooltip')
                  : undefined
              const button = (
                <Button
                  danger
                  size="small"
                  disabled={disabledReason !== undefined}
                  onClick={() => {
                    deleteMutation.reset()
                    setRemoveVg(true)
                    setReleaseDevices(true)
                    setDeleteTarget(row)
                  }}
                >
                  {t('storage.pools.delete')}
                </Button>
              )
              // 禁用按钮不派发鼠标事件，Tooltip 要包一层 span 才拿得到 hover。
              return disabledReason ? (
                <Tooltip title={disabledReason}>
                  <span style={{ display: 'inline-block' }}>{button}</span>
                </Tooltip>
              ) : (
                button
              )
            }
          }
        ]}
      />

      {/*
        确认框单独写而不是用 Modal.confirm：要在框内展示后端的错误码（删不掉的原因，
        如"该池上还有存储"），静态 confirm 拿不到 mutation 的状态。危险按钮文案必须写"删除"。
      */}
      <Modal
        open={deleteTarget !== null}
        title={t('storage.pools.deleteTitle')}
        okText={t('common.delete')}
        cancelText={t('common.cancel')}
        okButtonProps={{ danger: true, loading: deleteMutation.isPending }}
        onOk={() => {
          if (deleteTarget) deleteMutation.mutate(deleteTarget)
        }}
        onCancel={closeDelete}
        maskClosable={false}
        centered
      >
        <div>{t('storage.pools.deleteConfirm', { key: deleteTarget?.key ?? '' })}</div>
        <Space direction="vertical" size={4} style={{ marginTop: spacing.sm, display: 'flex' }}>
          <Checkbox checked={removeVg} onChange={(event) => setRemoveVg(event.target.checked)}>
            {t('storage.pools.deleteRemoveVg')}
          </Checkbox>
          <Checkbox
            checked={releaseDevices}
            // 卷组没删就没有 PV 可清：勾选框联动置灰，而不是提交后被服务端忽略。
            disabled={!removeVg}
            onChange={(event) => setReleaseDevices(event.target.checked)}
          >
            {t('storage.pools.releaseDevices')}
          </Checkbox>
        </Space>
        {deleteMutation.error ? (
          <div style={{ marginTop: spacing.sm }}>
            <ErrorNotice error={deleteMutation.error} />
          </div>
        ) : null}
      </Modal>

      {/* 删除结果：被释放的磁盘与逐步明细（与"释放设备"同一套展示，含失败/跳过的步骤）。 */}
      <Modal
        open={deleteResult !== null}
        title={t('storage.pools.deleteResult')}
        width={720}
        centered
        onCancel={closeResult}
        footer={<Button onClick={closeResult}>{t('common.close')}</Button>}
      >
        {deleteResult ? (
          <>
            <Alert
              type="success"
              showIcon
              style={{ marginBottom: spacing.sm }}
              message={
                deleteResult.thin_pool
                  ? `${deleteResult.vg}/${deleteResult.thin_pool}`
                  : deleteResult.vg
              }
              // 逐行列出"卷组是否被删掉/哪些盘被释放"，不用标点拼接（各语言标点习惯不同）。
              description={
                <>
                  {deleteResult.removed_volume_group ? (
                    <div>{t('storage.pools.resultRemovedVg', { vg: deleteResult.vg })}</div>
                  ) : null}
                  {releasedDevices.length > 0 ? (
                    <div>
                      {t('storage.pools.resultReleased', {
                        devices: releasedDevices.join(', ')
                      })}
                    </div>
                  ) : null}
                </>
              }
            />
            {deleteSteps.length > 0 ? (
              <Table<DeviceReleaseStepDTO>
                size="small"
                rowKey={(_, index) => String(index)}
                dataSource={deleteSteps}
                pagination={false}
                scroll={{ y: 300 }}
                columns={[
                  { title: t('system.lvm.device.step'), dataIndex: 'step', key: 'step', width: 110 },
                  {
                    title: t('system.lvm.device.stepTarget'),
                    dataIndex: 'target',
                    key: 'target'
                  },
                  {
                    title: t('common.status'),
                    key: 'status',
                    width: 90,
                    render: (_: unknown, step) =>
                      step.ok ? (
                        <Tag color="green">{t('system.lvm.device.stepDone')}</Tag>
                      ) : step.skipped ? (
                        <Tag>{t('system.lvm.device.stepSkipped')}</Tag>
                      ) : (
                        <Tag color="red">{t('system.lvm.device.stepFailed')}</Tag>
                      )
                  },
                  {
                    title: t('system.lvm.device.stepDetail'),
                    dataIndex: 'detail',
                    key: 'detail'
                  }
                ]}
              />
            ) : (
              <Typography.Text type="secondary">
                {t('storage.pools.resultNoSteps')}
              </Typography.Text>
            )}
          </>
        ) : null}
      </Modal>

      <PoolCreateModal
        open={createOpen}
        volumeGroups={catalog?.volume_groups ?? []}
        loading={loading}
        onCancel={() => setCreateOpen(false)}
        onCreated={() => {
          setCreateOpen(false)
          refresh()
        }}
      />
    </Modal>
  )
}
