import { useEffect, useMemo, useRef } from 'react'
import { Alert, App, Collapse, Form, Input, InputNumber, Modal, Radio, Select, Space, Typography } from 'antd'
import { useMutation, useQuery } from '@tanstack/react-query'

import { ErrorNotice } from '../../components/ErrorNotice'
import { translateError } from '../../api/errors'
import { useApi } from '../../api/provider'
import type { InitializePoolRequest, VolumeGroupDTO } from '../../api/types'
import { useI18n } from '../../i18n'
import { spacing } from '../../tokens/palette'
import { formatBytes } from '../../utils/format'
import { BlockDevicePicker, DeviceRefreshButton } from '../system/BlockDevicePicker'

const BYTES_PER_GB = 1024 ** 3

/** 卷组来源：用目录里已有的空卷组，或现场新建一个卷组。 */
type PoolVgSource = 'existing' | 'new'

interface PoolFormValues {
  vgSource: PoolVgSource
  /** 已有卷组名（vgSource=existing）。 */
  vg?: string
  /** 新卷组名（vgSource=new）。 */
  vgName?: string
  /** 池名（thin pool）。 */
  thinPool?: string
  /** 池数据容量，表单以 GB 为单位（提交时换算成字节）。 */
  sizeGB?: number
  /** 新建卷组时使用的块设备全路径。 */
  devices?: string[]
  metadataSize?: string
  chunkSize?: string
  cacheDevices?: string[]
  cacheMode?: string
  cacheChunkSize?: string
  cachePolicy?: string
}

/**
 * 把弹窗表单整理成建池请求。
 *
 * `creatingVG` 为 true 时才带 `hdd_devices`（卷组已存在时后端会忽略它）。
 * 容量口径与 StorageCreateForm 完全一致（同一个 `size_bytes` 字段、同一个 1024 进制换算），
 * 不在这里另造第二套换算。
 */
function buildPoolRequest(values: PoolFormValues, creatingVG: boolean): InitializePoolRequest {
  const body: InitializePoolRequest = {
    vg: (creatingVG ? values.vgName ?? '' : values.vg ?? '').trim(),
    thin_pool: (values.thinPool ?? '').trim(),
    size_bytes: Math.round((values.sizeGB ?? 0) * BYTES_PER_GB)
  }
  const chunk = (values.chunkSize ?? '').trim()
  if (chunk) body.chunk_size = chunk
  const metadata = (values.metadataSize ?? '').trim()
  if (metadata) body.metadata_size = metadata
  if (creatingVG) body.hdd_devices = values.devices ?? []
  const cache = values.cacheDevices ?? []
  if (cache.length > 0) {
    body.cache_devices = cache
    // 默认直写（安全）：回写更快，但掉电会丢缓存里尚未落盘的数据，必须是用户显式选择。
    body.cache_mode = values.cacheMode || 'writethrough'
    const cacheChunk = (values.cacheChunkSize ?? '').trim()
    if (cacheChunk) body.cache_chunk_size = cacheChunk
    const policy = (values.cachePolicy ?? '').trim()
    if (policy) body.cache_policy = policy
  }
  return body
}

export interface PoolCreateModalProps {
  open: boolean
  /** 卷组目录（来自 GET /v1/system/pools，由面板传入，避免重复取数）。 */
  volumeGroups: VolumeGroupDTO[]
  /** 目录仍在加载：加载完成前不预选卷组来源，免得先落到"新建卷组"再纠正。 */
  loading?: boolean
  /** 创建成功（池已落盘）：面板据此刷新列表。 */
  onCreated: () => void
  onCancel: () => void
}

/**
 * 「新建存储池」弹窗。
 *
 * 与 StorageCreateForm 里的内联建池是同一件事，区别是这里**只建池、不建存储**：
 * 面板里已有全部卷组的容量口径（`pool_max_bytes`），新建卷组则走 `/pools/estimate`，
 * 两处都取后端算好的权威值——容量只能有一处权威计算，否则又是"前端放行、后端拒绝"。
 */
export function PoolCreateModal({
  open,
  volumeGroups,
  loading,
  onCreated,
  onCancel
}: PoolCreateModalProps): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const { message } = App.useApp()
  const [form] = Form.useForm<PoolFormValues>()

  const vgSource = Form.useWatch('vgSource', form)
  const devices = Form.useWatch('devices', form)
  const cacheDevices = Form.useWatch('cacheDevices', form)
  const metadataSize = Form.useWatch('metadataSize', form)

  // 可用作"已有的空卷组"：一个卷组只允许一个 thin pool，已经有池的不能再拿来建池。
  const reusableVgs = useMemo(
    () => volumeGroups.filter((vg) => (vg.thin_pools ?? []).length === 0),
    [volumeGroups]
  )
  const selectedVgName = Form.useWatch('vg', form)
  const selectedVG = useMemo(
    () => reusableVgs.find((vg) => vg.name === selectedVgName) ?? null,
    [reusableVgs, selectedVgName]
  )
  const creatingVG = vgSource === 'new'

  // 选盘：新建卷组要选盘，配缓存也要选盘，因此弹窗一打开就把块设备拉出来。
  const devicesQuery = useQuery({
    queryKey: ['block-devices'],
    queryFn: () => api.listBlockDevices(),
    enabled: open
  })
  const deviceList = devicesQuery.data?.items ?? []
  const pickedDevices = devices ?? []
  const pickedCacheDevices = cacheDevices ?? []

  /**
   * 新建卷组时的可建池容量上限：卷组还不存在，目录里没有 `pool_max_bytes`，只能问后端。
   *
   * 元数据大小一并传过去：填小了元数据，可建的数据容量就变大，估算要跟着变。
   * 查询键与 StorageCreateForm 一致，两处共享同一份估算结果。
   */
  const estimateQuery = useQuery({
    queryKey: ['pool-size-estimate', pickedDevices, (metadataSize ?? '').trim()],
    queryFn: () =>
      api.estimatePoolSize({
        hdd_devices: pickedDevices,
        metadata_size: (metadataSize ?? '').trim()
      }),
    enabled: open && creatingVG && pickedDevices.length > 0
  })

  /** 打开时清空上一次填的值：弹窗复用同一个 form 实例，不清就会带出上次的池名与容量。 */
  useEffect(() => {
    if (!open) return
    form.resetFields()
  }, [form, open])

  /**
   * 预选卷组来源：优先"有容量的空卷组"，一个都没有时直接落到"新建卷组"。
   *
   * 用 effect 而不是 initialValues：目录是异步到的（initialValues 只在挂载那一刻生效，
   * 那时还不知道有哪些卷组）。等目录就绪后再写，免得先预选成"新建"再纠正。
   */
  useEffect(() => {
    if (!open || loading) return
    if (form.getFieldValue('vgSource')) return
    const preferred = reusableVgs.find((vg) => vg.pool_max_bytes > 0)
    form.setFieldsValue(preferred ? { vgSource: 'existing', vg: preferred.name } : { vgSource: 'new' })
  }, [form, loading, open, reusableVgs])

  /**
   * 池容量上限（GB）：池的元数据要从**同一个卷组**另划，"填满剩余空间"必被 LVM 拒绝。
   *
   * 两种场景都取后端算好的权威值（已有卷组 `pool_max_bytes` / 新建卷组走 estimate），
   * 前端不重推公式；向下取整到 0.01 GB，保证换算回字节后不会越过上限。
   */
  const poolSizeMaxGB = useMemo<number | undefined>(() => {
    const maxBytes = creatingVG ? estimateQuery.data?.pool_max_bytes : selectedVG?.pool_max_bytes
    if (maxBytes === undefined || maxBytes <= 0) return undefined
    return Math.floor((maxBytes / BYTES_PER_GB) * 100) / 100
  }, [creatingVG, estimateQuery.data, selectedVG])

  /** 已选容量盘的原始容量合计：只用于文案（告诉用户"你选了 2T，但只建得出 1.9T 的池"）。 */
  const pickedCapacityBytes = useMemo(
    () =>
      deviceList
        .filter((device) => pickedDevices.includes(device.path))
        .reduce((sum, device) => sum + device.size_bytes, 0),
    [deviceList, pickedDevices]
  )

  /** 容量输入框下方的说明：讲清上限从哪来、为什么磁盘容量不等于可建池容量。 */
  const poolSizeExtra = useMemo<string | undefined>(() => {
    const capacity = formatBytes(pickedCapacityBytes)
    if (!creatingVG) {
      return poolSizeMaxGB === undefined ? undefined : t('storage.pools.sizeMaxHint', { max: poolSizeMaxGB })
    }
    if (pickedDevices.length === 0) return undefined
    if (estimateQuery.isFetching) return t('storage.poolSizeGbEstimating')
    // 估算失败的**真实原因**直接摆出来（服务端过旧会 404，前端自己猜不出来）。
    if (estimateQuery.isError) {
      return t('storage.poolSizeGbEstimateError', { reason: translateError(estimateQuery.error) })
    }
    if (poolSizeMaxGB === undefined) return t('storage.poolSizeGbCannotBuild', { capacity })
    return t('storage.poolSizeGbAuto', { capacity, max: poolSizeMaxGB })
  }, [
    pickedCapacityBytes,
    creatingVG,
    estimateQuery.error,
    estimateQuery.isError,
    estimateQuery.isFetching,
    pickedDevices.length,
    poolSizeMaxGB,
    t
  ])

  // 上次自动填进去的池容量：用户一旦手改（当前值不再等于它）就不再覆盖，
  // 免得"改完又被自动改回去"。放 ref 而非 state——它不参与渲染。
  const autoPoolSizeRef = useRef<number | undefined>(undefined)

  // 自动填池容量：换卷组 / 换磁盘 / 改元数据大小都会重算上限，跟着更新（除非用户已手改）。
  useEffect(() => {
    if (!open || poolSizeMaxGB === undefined) return
    const current = form.getFieldValue('sizeGB') as number | undefined | null
    if (current !== undefined && current !== null && current !== autoPoolSizeRef.current) return
    autoPoolSizeRef.current = poolSizeMaxGB
    form.setFieldsValue({ sizeGB: poolSizeMaxGB })
  }, [form, open, poolSizeMaxGB])

  // 容量盘与缓存盘互斥：同一块盘不能既承载 thin pool 又承载 cache pool。
  // 选择器里已互相排除，这里兜住"先勾的缓存盘随后又被勾成容量盘"的替换场景。
  useEffect(() => {
    const usedAsPool = new Set(pickedDevices)
    const kept = pickedCacheDevices.filter((path) => !usedAsPool.has(path))
    if (kept.length !== pickedCacheDevices.length) form.setFieldsValue({ cacheDevices: kept })
  }, [form, pickedCacheDevices, pickedDevices])

  const createMutation = useMutation({
    mutationFn: (values: PoolFormValues) => api.createPool(buildPoolRequest(values, creatingVG)),
    onSuccess: () => {
      message.success(t('storage.pools.created'))
      autoPoolSizeRef.current = undefined
      form.resetFields()
      onCreated()
    }
    // 失败不关弹窗：错误实体交给弹窗内的 ErrorNotice 展示（后端会把 code 与参数带回来）。
  })

  const submit = (): void => {
    void form.validateFields().then((values) => createMutation.mutate(values))
  }

  // 已有卷组下拉：剩余空间装不下池元数据的卷组直接禁用并写明原因，省得提交后才被拒。
  const vgOptions = useMemo(
    () =>
      reusableVgs.map((vg) => ({
        value: vg.name,
        disabled: vg.pool_max_bytes <= 0,
        // 带上可用空间：光看卷组名选不出"哪个还装得下"。
        label: `${vg.name} · ${formatBytes(vg.free_bytes)}`
      })),
    [reusableVgs]
  )

  return (
    <Modal
      open={open}
      title={t('storage.pools.createTitle')}
      width={720}
      centered
      okText={t('common.create')}
      cancelText={t('common.cancel')}
      confirmLoading={createMutation.isPending}
      onOk={submit}
      onCancel={onCancel}
      maskClosable={false}
    >
      {createMutation.error ? (
        <div style={{ marginBottom: spacing.md }}>
          <Typography.Text strong type="danger">
            {t('storage.pools.createFailed')}
          </Typography.Text>
          <ErrorNotice error={createMutation.error} />
        </div>
      ) : null}

      {!loading && reusableVgs.length === 0 ? (
        <Alert
          type="info"
          showIcon
          style={{ marginBottom: spacing.md }}
          message={t('storage.pools.noReusableVg')}
        />
      ) : null}

      <Form form={form} layout="vertical">
        <Form.Item name="vgSource" label={t('storage.pools.vgSource')} rules={[{ required: true }]}>
          <Radio.Group>
            <Radio value="existing" disabled={reusableVgs.length === 0}>
              {t('storage.pools.vgSourceExisting')}
            </Radio>
            <Radio value="new">{t('storage.pools.vgSourceNew')}</Radio>
          </Radio.Group>
        </Form.Item>

        {creatingVG ? (
          <>
            <Form.Item
              name="vgName"
              label={t('storage.poolVgName')}
              rules={[{ required: true, message: t('storage.pools.vgNameRequired') }]}
            >
              <Input spellCheck={false} maxLength={64} />
            </Form.Item>
            <Form.Item
              name="devices"
              label={
                <Space size={4}>
                  {t('storage.poolDevices')}
                  <DeviceRefreshButton
                    loading={devicesQuery.isFetching}
                    onRefresh={() => void devicesQuery.refetch()}
                  />
                </Space>
              }
              rules={[{ type: 'array', min: 1, message: t('storage.poolDevicesRequired') }]}
            >
              <BlockDevicePicker
                devices={deviceList}
                loading={devicesQuery.isLoading}
                allowRelease
                onReleased={() => void devicesQuery.refetch()}
                // 缓存盘不能再作容量盘：同一块盘不能既承载 thin pool 又承载 cache pool。
                usedPaths={pickedCacheDevices}
                usedReason={t('storage.poolCacheDeviceUsedAsPool')}
              />
            </Form.Item>
          </>
        ) : (
          <Form.Item
            name="vg"
            label={t('storage.poolVg')}
            rules={[{ required: true, message: t('storage.pools.vgRequired') }]}
          >
            <Select
              loading={loading}
              options={vgOptions}
              placeholder={t('storage.poolVg')}
              onChange={() => {
                // 换了卷组就换了容量上限：已填的容量若超出，交给下面的校验规则提示。
                autoPoolSizeRef.current = undefined
              }}
            />
          </Form.Item>
        )}

        <Form.Item
          name="thinPool"
          label={t('storage.poolName')}
          extra={t('storage.poolNameHint')}
          rules={[{ required: true, message: t('storage.pools.poolNameRequired') }]}
        >
          <Input spellCheck={false} maxLength={64} />
        </Form.Item>

        <Form.Item
          name="sizeGB"
          label={t('storage.poolSizeGb')}
          extra={poolSizeExtra}
          rules={[
            { required: true, message: t('storage.pools.sizeRequired') },
            // 上限就摆在输入框上：用户刚填完就能看到"这块卷组建不出这么大的池"，不必等提交。
            ...(poolSizeMaxGB !== undefined
              ? [
                  {
                    type: 'number' as const,
                    max: poolSizeMaxGB,
                    message: t('storage.pools.sizeTooLarge', { max: poolSizeMaxGB })
                  }
                ]
              : [])
          ]}
        >
          <InputNumber min={0.1} max={poolSizeMaxGB} precision={2} style={{ width: '100%' }} addonAfter="GB" />
        </Form.Item>

        <Collapse
          ghost
          items={[
            {
              key: 'pool-advanced',
              label: t('storage.poolAdvanced'),
              children: (
                <>
                  <Form.Item name="chunkSize" label={t('storage.poolChunkSize')}>
                    <Input spellCheck={false} placeholder="256K" />
                  </Form.Item>
                  <Form.Item name="metadataSize" label={t('storage.poolMetadataSize')} extra={t('storage.poolMetadataSizeHint')}>
                    <Input spellCheck={false} placeholder={t('storage.poolMetadataSizePlaceholder')} />
                  </Form.Item>
                  <Form.Item
                    name="cacheDevices"
                    label={
                      <Space size={4}>
                        {t('storage.poolCacheDevices')}
                        <DeviceRefreshButton
                          loading={devicesQuery.isFetching}
                          onRefresh={() => void devicesQuery.refetch()}
                        />
                      </Space>
                    }
                  >
                    <BlockDevicePicker
                      devices={deviceList}
                      loading={devicesQuery.isLoading}
                      // 容量盘不能再作缓存盘（块设备选择器里直接禁用并说明）。
                      usedPaths={pickedDevices}
                      usedReason={t('storage.poolDeviceUsedAsCache')}
                    />
                  </Form.Item>
                  <Form.Item name="cacheMode" label={t('storage.poolCacheMode')} initialValue="writethrough">
                    <Select
                      options={[
                        { value: 'writethrough', label: t('storage.poolCacheMode.writethrough') },
                        { value: 'writeback', label: t('storage.poolCacheMode.writeback') }
                      ]}
                    />
                  </Form.Item>
                  <Form.Item name="cacheChunkSize" label={t('storage.pools.cacheChunkSize')}>
                    <Input spellCheck={false} placeholder="256K" />
                  </Form.Item>
                  <Form.Item name="cachePolicy" label={t('storage.pools.cachePolicy')}>
                    <Input spellCheck={false} placeholder="smq" />
                  </Form.Item>
                </>
              )
            }
          ]}
        />
      </Form>
    </Modal>
  )
}
