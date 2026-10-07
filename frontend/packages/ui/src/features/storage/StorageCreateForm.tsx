import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Alert, App, Button, Collapse, Form, Input, InputNumber, Select, Space, Switch, Typography } from 'antd'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { ErrorNotice } from '../../components/ErrorNotice'
import { PageShell } from '../../components/PageShell'
import { SectionCard } from '../../components/SectionCard'
import { isApiError, translateError } from '../../api/errors'
import { useApi } from '../../api/provider'
import type { CreateStorageRequest, InitializePoolRequest, LvmPoolStatusDTO } from '../../api/types'
import { useI18n } from '../../i18n'
import { spacing } from '../../tokens/palette'
import { formatBytes } from '../../utils/format'
import { ServerDirBrowser } from '../repo/SourceDirPicker'
import { BlockDevicePicker, DeviceRefreshButton } from '../system/BlockDevicePicker'

const BYTES_PER_GB = 1024 ** 3

/** 创建模式（与后端 app.StorageMode* 一致）。 */
type StorageMode = 'thin' | 'register_dir' | 'register_lv'

/** 存储池下拉里的"新建一个池"哨兵值（不是合法的 pool_ref）。 */
const POOL_NEW = '__new_pool__'
/** 卷组下拉里的"新建一个卷组"哨兵值（不是合法的卷组名）。 */
const VG_NEW = '__new_vg__'

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
  /** 存储池键（`<vg>/<thin_pool>`）；`POOL_NEW` 表示现场新建。仅 thin 使用。 */
  poolRef?: string
  /** 新建池：卷组名，或 `VG_NEW`（要先建卷组）。 */
  newPoolVG?: string
  /** 新建池 + 新建卷组时的卷组名。 */
  newPoolVGName?: string
  /** 新建池的池名（thin pool）；留空用服务端默认名。 */
  newPoolName?: string
  /** 新建池的容量（GB）。thin pool 必须给定容量。 */
  newPoolSizeGB?: number
  /** 新建卷组时使用的块设备全路径。 */
  newPoolDevices?: string[]
  newPoolChunkSize?: string
  newPoolMetadataSize?: string
  /** 新建池时可选的 dm-cache 设备与缓存模式。 */
  newPoolCacheDevices?: string[]
  newPoolCacheMode?: string
}

export interface StorageCreateFormProps {
  /** 创建成功（存储已落盘）；由路由决定去哪（通常是回列表）。 */
  onCreated: () => void
  /** 取消创建（返回列表）。 */
  onCancel: () => void
}

/**
 * 把"新建存储池"内联表单整理成建池请求。
 *
 * `creatingVG` 为 true 表示连卷组一起建（此时才需要 hdd_devices：卷组已存在时后端会忽略它）。
 */
function buildPoolRequest(values: StorageFormValues, creatingVG: boolean): InitializePoolRequest {
  const body: InitializePoolRequest = {
    vg: (creatingVG ? values.newPoolVGName ?? '' : values.newPoolVG ?? '').trim()
  }
  const pool = (values.newPoolName ?? '').trim()
  if (pool) body.thin_pool = pool
  // thin pool 必须给定容量：0 在后端表示"占满卷组剩余空间"，表单里已强制必填。
  body.size_bytes = Math.round((values.newPoolSizeGB ?? 0) * BYTES_PER_GB)
  const chunk = (values.newPoolChunkSize ?? '').trim()
  if (chunk) body.chunk_size = chunk
  const metadata = (values.newPoolMetadataSize ?? '').trim()
  if (metadata) body.metadata_size = metadata
  if (creatingVG) body.hdd_devices = values.newPoolDevices ?? []
  const cache = values.newPoolCacheDevices ?? []
  if (cache.length > 0) {
    body.cache_devices = cache
    body.cache_mode = values.newPoolCacheMode || 'writethrough'
  }
  return body
}

/**
 * Windows 上的挂载点输入：可手动填绝对路径，也可点「浏览」在服务端目录树里挑一个。
 *
 * 为什么要有浏览：Windows 上存储就是宿主机上的一个目录，路径要求绝对、且必须在服务端
 * 能看到的盘上；让用户凭记忆手敲 `D:\...` 既容易打错，也不知道服务端到底有哪些盘。
 * 浏览走的是既有的 /v1/fs/* 接口（与「源目录选择」同一个白名单）。
 *
 * 仍保留可编辑：挂载点目录可以先不存在（服务端会 MkdirAll 建出来），而目录浏览只能
 * 挑到已存在的目录，手输是新建目录的唯一途径。
 */
function MountPointInput({
  value,
  onChange,
  placeholder
}: {
  value?: string
  onChange?: (value: string) => void
  placeholder?: string
}): JSX.Element {
  const { t } = useI18n()
  const [browserOpen, setBrowserOpen] = useState(false)

  return (
    <>
      <Space.Compact style={{ width: '100%' }}>
        <Input
          spellCheck={false}
          // Form.Item 初始值是 undefined，直接透给受控 Input 会让 React 报"非受控变受控"。
          value={value ?? ''}
          placeholder={placeholder}
          onChange={(event) => onChange?.(event.target.value)}
        />
        <Button onClick={() => setBrowserOpen(true)}>{t('storage.mountPointBrowse')}</Button>
      </Space.Compact>
      <ServerDirBrowser
        open={browserOpen}
        initialPath={value}
        title={t('storage.mountPointBrowseTitle')}
        onCancel={() => setBrowserOpen(false)}
        onConfirm={(path) => {
          onChange?.(path)
          setBrowserOpen(false)
        }}
      />
    </>
  )
}

/**
 * 创建存储表单**页**。
 *
 * 逻辑链是「选/建存储池 → 设定存储 → （存储库再用它）」，链条本身要一步接一步看，
 * 塞进弹窗里既放不下（内联建池要带块设备表格）也不好看，因此独立成页：
 * 保存成功后由路由跳回列表（`onCreated`），不再有"弹窗里跳弹窗"的层级。
 */
export function StorageCreateForm({ onCreated, onCancel }: StorageCreateFormProps): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const [form] = Form.useForm<StorageFormValues>()
  // 创建失败走弹窗而不是页面横幅：这个页面很长（新建卷组还要选盘），用户在最底下点"创建"，
  // 而横幅挂在页面顶部——新建之后页面一拉下去，报错就完全看不到（真机反馈）。
  const { modal } = App.useApp()

  // 能力探测：lvm=true 表示平台有"存储底层卷"（Linux），才展示 thin LV 相关的交互。
  const infoQuery = useQuery({ queryKey: ['system-info'], queryFn: () => api.systemInfo() })
  const supportsVolumes = Boolean(infoQuery.data?.capabilities.lvm)
  // 能力未知前不渲染与平台强相关的表单项，避免先闪出 Windows 的挂载点输入框再跳成「高级」。
  const platformKnown = infoQuery.isSuccess

  const formMode = Form.useWatch('mode', form) ?? 'thin'
  const poolRef = Form.useWatch('poolRef', form)
  const newPoolVG = Form.useWatch('newPoolVG', form)
  const newPoolMetadataSize = Form.useWatch('newPoolMetadataSize', form)
  const newPoolDevices = Form.useWatch('newPoolDevices', form)
  const newPoolCacheDevices = Form.useWatch('newPoolCacheDevices', form)
  const newPoolSizeGB = Form.useWatch('newPoolSizeGB', form)
  // 池水位提示只在"新建 thin 卷"时需要（与系统设置页共享缓存）。
  // 必须带上 supportsVolumes：Windows 上表单里残留的 thin 默认值不该触发 LVM 提示。
  const creatingThin = supportsVolumes && formMode === 'thin'
  // 多存储池目录：创建存储时"选择或新建"。
  const poolsQuery = useQuery({ queryKey: ['pools'], queryFn: () => api.listPools(), enabled: creatingThin })
  const pools = poolsQuery.data?.items ?? []
  const volumeGroups = poolsQuery.data?.volume_groups ?? []
  const creatingPool = creatingThin && poolRef === POOL_NEW
  const creatingVG = creatingPool && newPoolVG === VG_NEW
  // 建卷组要选盘；配缓存也要选盘，因此新建池时就把块设备拉出来。
  const devicesQuery = useQuery({
    queryKey: ['block-devices'],
    queryFn: () => api.listBlockDevices(),
    enabled: creatingPool
  })
  const devices = devicesQuery.data?.items ?? []

  /**
   * 新建卷组时的可建池容量上限：卷组还不存在，目录里没有 `pool_max_bytes`，只能问后端。
   *
   * 以前这里是前端自己估（"磁盘总容量 ÷ 1.01 − 1 GB"），算出来的值必然与后端校验对不上：
   * 真机上两块 8 GB 盘被自动填了 14 GB，提交却报"可用 11.6 GB"——池元数据
   * （tmeta + pmspare，默认配置下就是 4 GB 一份）是从**同一个卷组**另划的，
   * 前端猜不到这个口径。现在口径只留在后端一处，前端把返回值填进去即可。
   * 元数据大小一并传过去：填小了元数据，可建的数据容量就变大，估算要跟着变。
   */
  const estimateQuery = useQuery({
    queryKey: ['pool-size-estimate', newPoolDevices ?? [], newPoolMetadataSize ?? ''],
    queryFn: () =>
      api.estimatePoolSize({
        hdd_devices: newPoolDevices ?? [],
        metadata_size: (newPoolMetadataSize ?? '').trim()
      }),
    enabled: creatingVG && (newPoolDevices?.length ?? 0) > 0
  })

  // 释放设备改变了盘的状态（可能连带影响它所属的卷组/池），因此设备列表与池目录都要重取。
  const refreshAfterRelease = (): void => {
    void devicesQuery.refetch()
    void queryClient.invalidateQueries({ queryKey: ['pools'] })
  }

  const selectedPool = useMemo<LvmPoolStatusDTO | null>(
    () => pools.find((pool) => pool.key === poolRef) ?? null,
    [pools, poolRef]
  )

  const selectedVG = useMemo(
    () => volumeGroups.find((vg) => vg.name === newPoolVG) ?? null,
    [newPoolVG, volumeGroups]
  )

  /** 新建卷组时选中的磁盘总容量（字节）；不建卷组或无选择时为 0（仅用于文案展示）。 */
  const newPoolDeviceBytes = useMemo(() => {
    if (!creatingVG) return 0
    const picked = new Set(newPoolDevices ?? [])
    return devices.reduce((sum, d) => (picked.has(d.path) ? sum + d.size_bytes : sum), 0)
  }, [creatingVG, devices, newPoolDevices])

  /**
   * 池容量上限（GB）：池的元数据要从**同一个卷组**另划，"填满剩余空间"必被 LVM 拒绝。
   *
   * 两种场景都取后端算好的权威值，前端不重推公式——容量口径只能有一处权威计算，
   * 否则就是"前端放行、后端拒绝"的割裂（真机撞过两次：一次是填 1 TiB 而卷组只剩 12 GiB；
   * 一次是前端按"总容量 ÷ 1.01 − 1 GB"自动填了 14 GB，后端却认为只有 11.6 GB 可用）：
   *   - 已有卷组：`pool_max_bytes`（来自 GET /v1/system/pools）；
   *   - 新建卷组：`/v1/system/pools/estimate`，按所选磁盘与元数据大小现算。
   * 向下取整到 0.01 GB，保证换算回字节后不会越过上限。
   */
  const poolSizeMaxGB = useMemo<number | undefined>(() => {
    if (!creatingPool) return undefined
    const maxBytes = creatingVG ? estimateQuery.data?.pool_max_bytes : selectedVG?.pool_max_bytes
    if (maxBytes === undefined || maxBytes <= 0) return undefined
    return Math.floor((maxBytes / BYTES_PER_GB) * 100) / 100
  }, [creatingPool, creatingVG, estimateQuery.data, selectedVG])

  /** 容量输入框下方的说明：讲清上限从哪来、为什么磁盘容量不等于可建池容量。 */
  const poolSizeExtra = useMemo<string | undefined>(() => {
    if (!creatingPool) return undefined
    if (!creatingVG) {
      return selectedVG ? t('storage.poolSizeGbMax', { free: formatBytes(selectedVG.free_bytes) }) : undefined
    }
    if (newPoolDeviceBytes <= 0) return undefined
    if (estimateQuery.isFetching) return t('storage.poolSizeGbEstimating')
    // 估算请求本身失败：把真实错误摆到界面上。以前这里一律说"设备列表可能已变化，请刷新"，
    // 等于把**永久性**故障伪装成"刷新一下就好"——真机反馈：释放设备后一直算不出来，
    // 根因其实是服务端还是旧版本、压根没有这个接口（404），刷新一百次也不会好。
    if (estimateQuery.isError) {
      const err = estimateQuery.error
      // 唯一真属于"设备列表变了"的情形：服务端认不出这些设备路径（400 hdd_devices）。
      const deviceChanged =
        isApiError(err) &&
        err.isBusiness &&
        err.code === 'system.invalid_param' &&
        err.args.field === 'hdd_devices'
      if (deviceChanged) return t('storage.poolSizeGbEstimateFailed')
      return t('storage.poolSizeGbEstimateError', { reason: translateError(err) })
    }
    // 算出来了但为 0：不是"暂时算不出来"，而是**按当前元数据大小真的建不出池**
    // （元数据要从同一卷组另划两份：tmeta + 备用 pmspare，小盘上配大值就是这么被吃光的）。
    // 这时必须说清该怎么改，否则用户只会反复刷新设备。
    if (poolSizeMaxGB === undefined) {
      return t('storage.poolSizeGbCannotBuild', { capacity: formatBytes(newPoolDeviceBytes) })
    }
    return t('storage.poolSizeGbAuto', {
      capacity: formatBytes(newPoolDeviceBytes),
      max: poolSizeMaxGB
    })
  }, [
    creatingPool,
    creatingVG,
    estimateQuery.error,
    estimateQuery.isError,
    estimateQuery.isFetching,
    newPoolDeviceBytes,
    poolSizeMaxGB,
    selectedVG,
    t
  ])

  /**
   * "容量"输入框的上限（GB）：存储就是池里的一个 thin 卷，逻辑容量不能超过池本身。
   *
   * 两种场景都取已有的权威值，前端不自己推系数（与池容量同一个原则）：
   *   - 现场新建池：就是即将建出的池容量（用户填的 `newPoolSizeGB`；还没填时取后端给的上限）；
   *   - 已有池：池自身的数据容量 `max_storage_volume_bytes`。**不能**用 `size_bytes`
   *     （那是卷组容量）——卷组比池大得多，拿它当上限等于放行一个建不出来的存储。
   * 向下取整到 0.01 GB，保证换算回字节后不会越过上限。
   */
  const storageSizeMaxGB = useMemo<number | undefined>(() => {
    if (!creatingThin) return undefined
    if (creatingPool) {
      const poolGB = typeof newPoolSizeGB === 'number' ? newPoolSizeGB : poolSizeMaxGB
      if (poolGB === undefined || poolGB <= 0) return undefined
      return Math.floor(poolGB * 100) / 100
    }
    const maxBytes = selectedPool?.max_storage_volume_bytes
    if (maxBytes === undefined || maxBytes <= 0) return undefined
    return Math.floor((maxBytes / BYTES_PER_GB) * 100) / 100
  }, [creatingPool, creatingThin, newPoolSizeGB, poolSizeMaxGB, selectedPool])

  /**
   * "容量"输入框下方的说明：说清"填进去的就是上限"，以及 thin 卷并不立即占满池空间
   * （省得用户以为一建就把整池吃掉、不敢填）。
   */
  const storageSizeExtra = useMemo<string | undefined>(
    () => (storageSizeMaxGB === undefined ? undefined : t('storage.sizeGbAuto', { max: storageSizeMaxGB })),
    [storageSizeMaxGB, t]
  )

  // 选中的池（或"还没有任何池"）用水位提示兜住；池不存在时明确引导去初始化。
  const poolNotice = useMemo<{ type: 'info' | 'warning'; text: string } | null>(() => {
    if (!creatingThin) return null
    if (creatingPool) {
      if (pools.length === 0) return { type: 'warning', text: t('storage.poolNewHint') }
      if (selectedVG && !creatingVG && selectedVG.pool_max_bytes <= 0) {
        return { type: 'warning', text: t('storage.poolVgFull', { free: formatBytes(selectedVG.free_bytes) }) }
      }
      return { type: 'info', text: t('storage.poolSizeHint') }
    }
    if (!selectedPool || !selectedPool.exists) return { type: 'warning', text: t('storage.poolMissing') }
    return {
      type: 'info',
      text: t('storage.poolHint', {
        free: formatBytes(selectedPool.free_bytes),
        total: formatBytes(selectedPool.size_bytes),
        percent: selectedPool.data_percent.toFixed(1)
      })
    }
  }, [creatingPool, creatingThin, creatingVG, pools.length, selectedPool, selectedVG, t])

  // 存储池下拉：现有池 + "新建…"。默认池带标注，便于多池场景里一眼认出来。
  const poolOptions = useMemo(
    () =>
      pools.map((pool) => ({
        value: pool.key,
        label: pool.default ? `${pool.key} · ${t('storage.poolDefault')}` : pool.key
      })),
    [pools, t]
  )

  // 卷组下拉：已有卷组（已带 thin pool 的禁用——一个卷组只允许一个池）+ "新建…"。
  const vgOptions = useMemo(
    () => [
      ...volumeGroups.map((vg) => {
        const existing = vg.thin_pools ?? []
        return {
          value: vg.name,
          disabled: existing.length > 0,
          label:
            existing.length > 0
              ? `${vg.name} (${existing.join(', ')})`
              : `${vg.name} · ${formatBytes(vg.free_bytes)}`
        }
      }),
      { value: VG_NEW, label: t('storage.poolVgNew') }
    ],
    [t, volumeGroups]
  )

  // 预选存储池：默认池优先，其次任意一个现有池；一个都没有时直接落到"新建"。
  //
  // 要等 poolsQuery 成功（而不是 isLoading）：目录还没回来就写默认值，会先落到"新建…"，
  // 之后即使用户其实有现成的池也不会再纠正。
  const preselectedPool = useMemo(() => {
    if (!creatingThin || !poolsQuery.isSuccess) return null
    if (form.getFieldValue('poolRef')) return null
    const preferred = pools.find((pool) => pool.default) ?? pools[0]
    return preferred ? preferred.key : POOL_NEW
    // form 是稳定引用，不进依赖；这里只在目录就绪时算一次预选值。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [creatingThin, pools, poolsQuery.isSuccess])

  // 新建池时预选卷组：优先"配置里的默认卷组且还没有池"，其次任一无池卷组，否则"新建卷组"。
  const preselectedVG = useMemo(() => {
    if (!creatingPool || !poolsQuery.isSuccess) return null
    if (form.getFieldValue('newPoolVG')) return null
    const reusable = (vg: { thin_pools?: string[] }): boolean => (vg.thin_pools ?? []).length === 0
    const preferred =
      volumeGroups.find((vg) => vg.name === poolsQuery.data?.default_vg && reusable(vg)) ??
      volumeGroups.find(reusable)
    return preferred ? preferred.name : VG_NEW
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [creatingPool, poolsQuery.data?.default_vg, poolsQuery.isSuccess, volumeGroups])

  // 预选值只在"目录刚就绪且字段仍为空"时写入一次：用 effect 而不是 initialValues，
  // 因为目录是异步到的（initialValues 只在挂载那一刻生效，那时还不知道有哪些池）。
  useEffect(() => {
    if (preselectedPool) form.setFieldsValue({ poolRef: preselectedPool })
  }, [form, preselectedPool])

  useEffect(() => {
    if (preselectedVG) form.setFieldsValue({ newPoolVG: preselectedVG })
  }, [form, preselectedVG])

  // 上次自动填进去的池容量：用户一旦手改（当前值不再等于它）就不再覆盖，
  // 免得"改完又被自动改回去"。放 ref 而非 state——它不参与渲染。
  const autoPoolSizeRef = useRef<number | undefined>(undefined)

  // 自动填池容量：换磁盘 / 换卷组 / 改元数据大小都会重算上限，跟着更新（除非用户已手改）。
  // 填进去的就是"能建的最大值"——元数据那部分损耗由后端扣好，不需要用户自己算。
  useEffect(() => {
    if (!creatingPool || poolSizeMaxGB === undefined) return
    const current = form.getFieldValue('newPoolSizeGB') as number | undefined | null
    if (current !== undefined && current !== null && current !== autoPoolSizeRef.current) return
    autoPoolSizeRef.current = poolSizeMaxGB
    form.setFieldsValue({ newPoolSizeGB: poolSizeMaxGB })
  }, [creatingPool, form, poolSizeMaxGB])

  // 上次自动填进去的存储容量：口径与池容量一致（用户一旦手改就不再覆盖）。
  const autoStorageSizeRef = useRef<number | undefined>(undefined)

  // 自动填"容量"：换池 / 换磁盘 / 改池容量都会让上限变化，跟着更新（除非用户已手改）。
  // 填进去的就是可建上限——省得用户对着空输入框自己估一个数，估大了提交才被拒。
  useEffect(() => {
    if (storageSizeMaxGB === undefined) return
    const current = form.getFieldValue('sizeGB') as number | undefined | null
    if (current !== undefined && current !== null && current !== autoStorageSizeRef.current) return
    autoStorageSizeRef.current = storageSizeMaxGB
    form.setFieldsValue({ sizeGB: storageSizeMaxGB })
  }, [form, storageSizeMaxGB])

  /** "最大"按钮：把容量填回上限（用户改小之后想再要"尽量大"，不必自己回忆那个数字）。 */
  const fillStorageSizeMax = useCallback((): void => {
    if (storageSizeMaxGB === undefined) return
    // 与自动填入同一口径：之后上限再变（换池 / 换磁盘）时它还会跟着走。
    autoStorageSizeRef.current = storageSizeMaxGB
    form.setFieldsValue({ sizeGB: storageSizeMaxGB })
  }, [form, storageSizeMaxGB])

  // 容量盘与缓存盘互斥：同一块盘不能既承载 thin pool 又承载 cache pool。
  // 选择器里已互相排除，这里兜住"先勾的缓存盘随后又被勾成容量盘"的替换场景。
  useEffect(() => {
    const usedAsPool = new Set(newPoolDevices ?? [])
    const cache = newPoolCacheDevices ?? []
    const kept = cache.filter((path) => !usedAsPool.has(path))
    if (kept.length !== cache.length) form.setFieldsValue({ newPoolCacheDevices: kept })
  }, [form, newPoolCacheDevices, newPoolDevices])

  /**
   * 创建失败的提示：弹窗。
   *
   * 不用页面内的错误横幅——新建卷组时表单很长（还要选盘），用户在最底部点"创建"，
   * 横幅却挂在页面顶部，新建之后页面一拉下去就看不见报错（真机反馈）。
   * 弹窗把"哪一步失败、为什么、该怎么办"直接推到眼前。
   */
  const showCreateError = useCallback(
    (err: unknown) => {
      modal.error({
        title: t('storage.createFailed'),
        width: 520,
        okText: t('common.confirm'),
        content: <ErrorNotice error={err} />
      })
    },
    [modal, t]
  )

  const createMutation = useMutation({
    mutationFn: async (values: StorageFormValues): Promise<unknown> => {
      const mountPoint = (values.mountPoint ?? '').trim()
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
        // 顺序是"池 → 存储"：现场新建的池要先建出来，拿到它的键才能把存储落上去。
        let poolRef = (values.poolRef ?? '').trim()
        if (poolRef === POOL_NEW) {
          const created = await api.createPool(buildPoolRequest(values, creatingVG))
          poolRef = created.key
        }
        if (poolRef) body.pool_ref = poolRef
      }
      if (mode === 'register_lv') body.ref = (values.ref ?? '').trim()
      return api.createStorage(body)
    },
    onSuccess: () => {
      // 刚才可能新建了池：目录要刷新，顺便让存储列表与配额总账重取。
      void queryClient.invalidateQueries({ queryKey: ['storages'] })
      void queryClient.invalidateQueries({ queryKey: ['pools'] })
      void queryClient.invalidateQueries({ queryKey: ['me'] })
      onCreated()
    },
    onError: (err) => showCreateError(err)
  })

  const submit = (): void => {
    void form.validateFields().then((values) => createMutation.mutate(values))
  }

  return (
    <PageShell
      title={t('storage.create')}
      extra={<Button onClick={onCancel}>{t('common.back')}</Button>}
    >
      <SectionCard>
        {poolNotice ? (
          <Alert type={poolNotice.type} showIcon style={{ marginBottom: spacing.md }} message={poolNotice.text} />
        ) : null}
        <Form
          form={form}
          layout="vertical"
          initialValues={{ enabled: true, mode: 'thin', fileSystem: 'ext4' }}
          // 管理端的内容区是铺满的，表单不居中就会整块贴在左边缘、右边空一大片（真机反馈）。
          // 居中 + 限宽：读起来是常规表单宽度，宽屏上也不会把输入框拉成一条细缝。
          style={{ maxWidth: 1040, margin: '0 auto' }}
        >
          <Form.Item name="name" label={t('field.name')} rules={[{ required: true }]}>
            <Input maxLength={64} />
          </Form.Item>

          {platformKnown && !supportsVolumes ? (
            // Windows：没有底层卷，存储就是本地目录，必须显式给出（可手填，也可浏览服务端目录）。
            <Form.Item
              name="mountPoint"
              label={t('field.mountPoint')}
              rules={[{ required: true, message: t('storage.mountPointRequired') }]}
            >
              <MountPointInput placeholder={t('field.path')} />
            </Form.Item>
          ) : null}

          {supportsVolumes && formMode === 'thin' ? (
            <>
              <Form.Item
                name="poolRef"
                label={t('storage.poolSelect')}
                rules={[{ required: true, message: t('storage.poolSelectRequired') }]}
              >
                <Select
                  loading={poolsQuery.isLoading}
                  options={[...poolOptions, { value: POOL_NEW, label: t('storage.poolNew') }]}
                  onChange={(value) => {
                    // 目录就绪后如果还没预选，选中"新建"时补一次卷组预选。
                    if (value === POOL_NEW && preselectedVG) form.setFieldsValue({ newPoolVG: preselectedVG })
                  }}
                />
              </Form.Item>

              {creatingPool ? (
                <div
                  style={{
                    padding: spacing.md,
                    marginBottom: spacing.md,
                    background: 'rgba(0, 0, 0, 0.02)',
                    border: '1px solid rgba(0, 0, 0, 0.06)',
                    borderRadius: 6
                  }}
                >
                  <Typography.Text strong>{t('storage.poolNew')}</Typography.Text>
                  <Form.Item
                    name="newPoolVG"
                    label={t('storage.poolVg')}
                    style={{ marginTop: spacing.sm }}
                    rules={[{ required: true, message: t('storage.poolVgRequired') }]}
                  >
                    <Select
                      loading={poolsQuery.isLoading}
                      options={vgOptions}
                      placeholder={t('storage.poolVg')}
                      onChange={(value) => {
                        // 换了卷组就重算容量上限；已填的容量若超出，交给下面的校验规则提示。
                        if (value === VG_NEW) form.setFieldsValue({ newPoolSizeGB: undefined })
                      }}
                    />
                  </Form.Item>
                  {creatingVG ? (
                    <Form.Item
                      name="newPoolVGName"
                      label={t('storage.poolVgName')}
                      rules={[{ required: true, message: t('storage.poolVgNameRequired') }]}
                    >
                      <Input spellCheck={false} maxLength={64} />
                    </Form.Item>
                  ) : null}
                  <Form.Item name="newPoolName" label={t('storage.poolName')} extra={t('storage.poolNameHint')}>
                    <Input spellCheck={false} maxLength={64} />
                  </Form.Item>
                  <Form.Item
                    name="newPoolSizeGB"
                    label={t('storage.poolSizeGb')}
                    // 上限来自后端的权威口径（已有卷组 pool_max_bytes / 新建卷组走 estimate）：
                    // 池的元数据要另占同一个卷组，"填满剩余空间"必被 LVM 拒绝，这里提前摆出来。
                    extra={poolSizeExtra}
                    rules={[
                      { required: true, message: t('storage.poolSizeGbRequired') },
                      ...(poolSizeMaxGB !== undefined
                        ? [{ type: 'number' as const, max: poolSizeMaxGB, message: t('storage.poolSizeGbTooLarge') }]
                        : []),
                      {
                        // 池必须罩得住这次要建的存储：池比存储小的话，存储根本挂不上去。
                        validator: (_rule, value: number | undefined) => {
                          const storageGB = form.getFieldValue('sizeGB') as number | undefined
                          if (typeof value !== 'number' || typeof storageGB !== 'number') {
                            return Promise.resolve()
                          }
                          if (value >= storageGB) return Promise.resolve()
                          return Promise.reject(
                            new Error(t('storage.poolSizeBelowStorage', { size: storageGB }))
                          )
                        }
                      }
                    ]}
                  >
                    <InputNumber
                      min={0.1}
                      max={poolSizeMaxGB}
                      precision={2}
                      style={{ width: '100%' }}
                      addonAfter="GB"
                    />
                  </Form.Item>
                  {creatingVG ? (
                    <Form.Item
                      name="newPoolDevices"
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
                        devices={devices}
                        loading={devicesQuery.isLoading}
                        allowRelease
                        onReleased={refreshAfterRelease}
                        // 缓存盘不能再作容量盘：同一块盘不能既承载 thin pool 又承载 cache pool。
                        usedPaths={newPoolCacheDevices}
                        usedReason={t('storage.poolCacheDeviceUsedAsPool')}
                      />
                    </Form.Item>
                  ) : null}
                  <Collapse
                    ghost
                    items={[
                      {
                        key: 'pool-advanced',
                        label: t('storage.poolAdvanced'),
                        children: (
                          <>
                            <Form.Item name="newPoolChunkSize" label={t('storage.poolChunkSize')}>
                              <Input spellCheck={false} placeholder="256K" />
                            </Form.Item>
                            <Form.Item
                              name="newPoolMetadataSize"
                              label={t('storage.poolMetadataSize')}
                              extra={
                                newPoolMetadataSize
                                  ? t('storage.poolMetadataSizeHint')
                                  : undefined
                              }
                            >
                              <Input
                                spellCheck={false}
                                placeholder={t('storage.poolMetadataSizePlaceholder')}
                              />
                            </Form.Item>
                            <Form.Item
                              name="newPoolCacheDevices"
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
                                devices={devices}
                                loading={devicesQuery.isLoading}
                                // 容量盘不能再作缓存盘（块设备选择器里直接禁用并说明）。
                                usedPaths={newPoolDevices}
                                usedReason={t('storage.poolDeviceUsedAsCache')}
                              />
                            </Form.Item>
                            <Form.Item name="newPoolCacheMode" label={t('storage.poolCacheMode')}>
                              <Select
                                options={[
                                  { value: 'writethrough', label: t('storage.poolCacheMode.writethrough') },
                                  { value: 'writeback', label: t('storage.poolCacheMode.writeback') }
                                ]}
                              />
                            </Form.Item>
                          </>
                        )
                      }
                    ]}
                  />
                </div>
              ) : null}

              <Form.Item
                name="sizeGB"
                label={t('storage.sizeGb')}
                extra={storageSizeExtra}
                rules={[
                  { required: true, message: t('storage.sizeGbRequired') },
                  // 存储容量必须 ≤ 池容量。上限就摆在输入框上，
                  // 用户刚填完就能看到"这块池装不下"，不必等提交。
                  // 现场建池时用用户填的池容量（池比存储小就挂不上去）；
                  // 用已有池时用后端给出的池数据容量——两种都是"池装不下"，但说法不同。
                  ...(creatingPool && typeof newPoolSizeGB === 'number'
                    ? [
                        {
                          type: 'number' as const,
                          max: newPoolSizeGB,
                          message: t('storage.storageSizeAbovePool', { pool: newPoolSizeGB })
                        }
                      ]
                    : storageSizeMaxGB !== undefined
                      ? [
                          {
                            type: 'number' as const,
                            max: storageSizeMaxGB,
                            message: t('storage.sizeGbAbovePool', { max: storageSizeMaxGB })
                          }
                        ]
                      : [])
                ]}
              >
                <InputNumber
                  min={0.1}
                  max={storageSizeMaxGB}
                  precision={2}
                  style={{ width: '100%' }}
                  // "最大"按钮就贴在输入框上（复用 GB 后缀的位置）：一键填回可建上限。
                  addonAfter={
                    <Space size={4}>
                      GB
                      <Button
                        type="link"
                        size="small"
                        style={{ padding: 0, height: 'auto' }}
                        disabled={storageSizeMaxGB === undefined}
                        onClick={fillStorageSizeMax}
                      >
                        {t('common.max')}
                      </Button>
                    </Space>
                  }
                />
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

          {supportsVolumes && formMode === 'register_lv' ? (
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

          {supportsVolumes ? (
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
        </Form>
        <Space style={{ marginTop: spacing.md }}>
          <Button type="primary" loading={createMutation.isPending} onClick={submit}>
            {t('common.create')}
          </Button>
          <Button onClick={onCancel}>{t('common.cancel')}</Button>
        </Space>
      </SectionCard>
    </PageShell>
  )
}
