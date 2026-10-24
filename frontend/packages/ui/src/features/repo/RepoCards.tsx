import type { CSSProperties, MouseEvent, ReactNode } from 'react'
import { useCallback, useEffect, useRef, useState } from 'react'
import { Badge, Button, Card, Progress, Space, Tag, Tooltip, Typography, message } from 'antd'
import { InfoCircleOutlined, LinkOutlined, SettingOutlined, FundOutlined } from '@ant-design/icons'
import { useMutation, useQueryClient } from '@tanstack/react-query'

import { StatusTag } from '../../components/StatusTag'
import { UsageBar, UsageValueText } from '../../components/UsageBar'
import { agentApi, createShortcut } from '../../api/agentClient'
import type { AgentMountVolumeUsage, UploadState } from '../../api/agentTypes'
import { translateError } from '../../api/errors'
import { useApi } from '../../api/provider'
import type { RepoDTO } from '../../api/types'
import { useAgent } from '../../hooks/useAgent'
import { useI18n } from '../../i18n'
import { fontSize, palette, spacing } from '../../tokens/palette'
import { formatBytes, formatTime, repoUsage } from '../../utils/format'
import { repoModeLabel } from '../../utils/labels'
import { openTargetOf } from '../mount/MountControls'
import { CacheMetricsDialog } from './CacheMetricsDialog'
import {
  RepoMountActions,
  RepoMountStateTag,
  RepoMountStatus,
  useRepoMount,
  type RepoMountController
} from './RepoMount'
import { isPlaceholderRepoId, repoStateValue, uploadPercent } from './uploadView'

export interface RepoCardProps {
  repo: RepoDTO
  currentUserId: string
  onOpen: (repoId: string) => void
  onConfigure: (repo: RepoDTO) => void
  /**
   * 该库正在上传时的实时进度（`repo` 可能是服务端记录，也可能是列表合成的占位库）。
   *
   * 卡片上**只留与上传有关的两样**：
   *   - **上传进度**（见 RepoProgress）：**就报在「已用」那一格里**（同一个元素、同一个位置，
 *     写"已传 / 总量"与条内百分比），不另起一条进度条 —— 用量那一格此刻本来也没有数可报；
   *     与条内百分比（真实反馈一路是："上传中的库就不展示进度条了" → 又"进度条加回去，
   *     改为上传进度，上面展示进度 xx GB/yy GB"）。速度/剩余/当前文件/分块仍然不上卡片：
   *     那些数字每两秒变一次，混在一片库卡里又挤又吵，完整进度在「上传任务」页；
   *   - **动作区换成「暂停 / 继续」+「取消」**（见 RepoUploadActions）：上传中的库挂不了，
   *     此刻能做的只有"停一下"和"算了"（真实反馈："上传状态下的挂载按钮改成暂停，
   *     加一个取消按钮"）。
   *
   * 它还决定**整卡点击去哪**：普通库单击进入详情，上传中的库根本没有详情可进
   * （服务端还没有这条记录），所以改去进度页。
   *
   * 其余零件（状态圆点 / 标签行 / 用量 / 次要信息）一概照旧：它在列表里就是一张普通库卡，
   * 只是状态不同。
   */
  upload?: UploadState
  /** 打开「上传任务」页；不传则整卡点击仍是普通库的行为（进入详情）。 */
  onOpenUpload?: (uploadId: string) => void
}

/**
 * 挂载状态圆点：库名前的**绿点**（本机已挂载）/ 灰点（未挂载）/ 红点（挂载失败）/
 * 蓝点（挂载中、卸载中）。
 *
 * 为什么从"磁盘徽标 + 呼吸灯"换成一个小圆点（真实反馈："图标的呼吸灯效果不要有背景，
 * 直接改成绿色圆点吧，不要在图标上呼吸了，在名称前加个绿点就可以了"）：
 *   - 原来的 40px 方块（底色 + 磁盘图标 + 外圈呼吸）在网格里比库名还抢眼，
 *     而它要说的只有一句话："这块盘正挂在本机"；
 *   - 圆点不用底色、不用图标、**也不带任何动效**：同一个状态说一遍就够，卡片一多动效反而
 *     晃眼 —— 颜色本身就够读，静止也不会漏看。
 *
 * **这颗点就是"挂没挂上"的唯一出口**：标签行里原来那句「已挂载 / 未挂载」已经撤掉
 * （真实反馈："未挂载和已挂载这个标签去掉，有绿色的指示器就够了"），所以四色得自己把话说全
 * （真实反馈："如果没有挂载则展示灰色，挂载失败展示红色"）：
 *   - 已挂载 → 浅绿（successLight，小面积色块用浅一档，免得 8px 的绿压过库名）；
 *   - 未挂载（无记录）→ 灰（textTertiary）：绝大多数库都是这个状态，必须安静但可见；
 *   - 失败（错误 / 撤销 / 断线）→ 红（danger），与标签行那个红色感叹号同源（controller.failed）；
 *   - 挂载中/卸载中 → 蓝（accent），转场中，看一眼就知道还在动。
 * 因为始终有一颗点，**所有卡片/行的库名起始位置一致**，列表纵向扫读时名字不会忽左忽右。
 *
 * 画法交给 antd `Badge`：状态点也算图标，不自己拿 div + border-radius 拼一个圆
 * （真实反馈："图标不要自己画，用组件提供的"）：
 *   - 尺寸走 theme.components.Badge.statusSize（8px，和原来的点一样大）；
 *   - 垂直位置交给 antd 自己的 `.ant-badge-status-dot`（`vertical-align: middle` + `top: -1px`），
 *     不再需要手工的光学修正量；
 *   - 颜色继续用 palette —— `color` 本来就是 Badge 给自定义色点留的口子，
 *     不该为了"用组件"把"小面积浅绿"这条反馈一起丢掉。
 */
export function RepoMountDot({ controller }: { controller: RepoMountController }): JSX.Element {
  const { mounted, pending, failed } = controller
  const color = mounted
    ? palette.successLight
    : pending
      ? palette.accent
      : failed
        ? palette.danger
        : palette.textTertiary

  return (
    <Badge
      aria-hidden
      color={color}
      style={{
        // 与库名同处一行：靠 flex 中线对齐（行里可能有比圆点高得多的兄弟元素，比如列表形态的
        // 标签、按钮），不让它跟着行高/文字基线漂移。
        alignSelf: 'center',
        flexShrink: 0,
        // 纯状态标记：不参与鼠标事件（整卡点击 = 打开挂载目录），也不做 tooltip
        // —— 挂载点就在下面的信息行里，悬浮提示只是重复。
        pointerEvents: 'none'
      }}
    />
  )
}

/**
 * 标签行：库模式 + （仅在库不"正常"时的）库状态 + （仅在有话可说时的）挂载状态。
 *
 * **正常不留痕**（真实反馈："存储库如果是正常的，默认不展示正常这个标签，只有异常时才展示
 * 异常标签"、"未挂载和已挂载这个标签去掉，有绿色的指示器就够了"）：一块正常的库，用户不需要
 * 被告知它正常 —— 满屏的「正常」「未挂载」只是噪音，还把真正要看的东西（异常、转场、错误）
 * 淹掉。所以这一行默认只有「共享库」这样一个模式标签，库名左边那颗圆点（灰 / 绿 / 红 / 蓝）
 * 单独承担"本机挂没挂上"。有异常时（建库中 / 封存中 / 删除中 / 异常、挂载中转场、已断开、
 * 报错）标签才出现，出现即值得一读。
 */
function RepoTagRow({
  repo,
  controller,
  upload,
  style
}: {
  repo: RepoDTO
  controller: RepoMountController
  upload?: UploadState
  style?: CSSProperties
}): JSX.Element {
  return (
    <Space size={spacing.xs} wrap style={style}>
      {/* 占位库（服务端还没有这条记录）拿不到模式，宁可不显示也不猜：见 uploadPlaceholderRepo。 */}
      {isPlaceholderRepoId(repo.id) ? null : <Tag>{repoModeLabel(repo.mode)}</Tag>}
      {/* 维护态是"用户此刻必须知道"的例外状态：期间不能分配/挂载，列表里必须一眼看到。
          其余父盘状态（idle/temp_shared）不在这里展示，避免正常库也挂一串标签。 */}
      {repo.parent_condition === 'maintenance' ? <StatusTag group="parentCondition" value="maintenance" /> : null}
      {/* 取值按"这个库现在到底在干什么"来定：服务端记录在 importing 时是**写母盘**（写入中），
          占位库（服务端还没记录）才是**传数据**（上传中）—— 见 repoStateValue。 */}
      {repo.state === 'active' ? null : <StatusTag group="repo" value={repoStateValue(repo, upload)} />}
      {/* 暂停是**本机**按下的一枚标志（upload.paused），不在服务端状态里：服务端那边这个库照旧
          在"建"着 —— 所以它不会自己出现在上面那枚状态标签里，必须单独说一句。不说的话，一行里
          能看见的只有按钮文案从「暂停」变成「继续」，扫一眼列表根本看不出哪几块停了
          （真实反馈："暂停状态下要在外面展示状态"）。 */}
      {upload?.paused ? <StatusTag group="repo" value="paused" /> : null}
      <RepoMountStateTag controller={controller} />
    </Space>
  )
}

/**
 * 用量 / 进度**共用的那一块**：一行"左边标签 + 右边数值"，下面一条 UsageBar。
 *
 * 为什么要抽这个外框：上传中的库**不再另起一条进度条**，而是把这一块的内容换成
 * "进度 + 已传 / 总量"（见 RepoUsage 的 upload 分支）。位置、宽度、行高、条的粗细、
 * 百分数的画法全部由这一处决定 —— 于是上传中的那一行在列表里与别的行严丝合缝地对齐，
 * 不会因为"多了一条进度条"而高出一截（真实反馈："我是指的复用存储用量那块的逻辑，
 * 不是让你另起一个啊"）。
 *
 * 左右两侧的文字样式也归这里管：调用方只给文字，不给字号 —— 「已用」和「进度」两句话
 * 必须长得一模一样，那正是同一个位置换了个数据源而已。
 */
function RepoMeter({
  label,
  hint,
  value,
  percent,
  muted,
  loading,
  color,
  style
}: {
  /** 左侧标签（「已用」/「进度」）。 */
  label: string
  /** 标签的悬浮说明；不传则标签不可悬浮。 */
  hint?: string
  /** 右侧数值（用量是「已用 / 上限」，上传是「已传 / 总量」）。 */
  value: ReactNode
  percent: number
  muted?: boolean
  loading?: boolean
  /** 填充色覆盖（上传进度必须一路蓝到底，见 UsageBar 的 color）。 */
  color?: string
  style?: CSSProperties
}): JSX.Element {
  return (
    <div style={style}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', gap: spacing.xs }}>
        {hint ? (
          <Tooltip title={hint}>
            <Typography.Text type="secondary" style={{ fontSize: fontSize.sm, cursor: 'help' }}>
              {label}
            </Typography.Text>
          </Tooltip>
        ) : (
          <Typography.Text type="secondary" style={{ fontSize: fontSize.sm }}>
            {label}
          </Typography.Text>
        )}
        <Typography.Text style={{ fontSize: fontSize.sm }}>{value}</Typography.Text>
      </div>
      <UsageBar percent={percent} muted={muted} loading={loading} color={color} />
    </div>
  )
}

/**
 * 用量：口径统一在 repoUsage（分母 = 库容量，客户端只认本机实测值），这里只负责画。
 *
 * 本机挂着这个库时（`usage` 有值）画的是**本机实测**的卷用量：服务端的数字是"已分配
 * 磁盘总量"（按盘标称容量计费，等于库容量），用户眼里的"用了多少"只有本机
 * 挂载着的那个卷知道。标签仍是「已用」（不给它挂「（本机）」后缀：同一列里两个口径的
 * 标签长短不一反而像两件事），口径差异由悬浮说明交代。
 *
 * **未挂载就只报上限**（真实反馈："未挂载的情况下不展示已用存储的量，那个进度条置灰就行了"、
 * "已用那里只展示一个上限就可以了"）：那一刻没有任何本机实测值，而服务端回退值又恰恰是
 * 那个等于库容量的"已分配容量"—— 报出来只会被读成"我这块盘用了这么多"。所以用量写 `-`
 * （不知道，见 repoUsage），上限照报，文案就是 `- / 1000 MB`：读起来是"已用 [不知道] / 上限"，
 * 不会把已分配容量误当成自己的用量，也不会把两个数看反。条同时**置灰且不叠百分数**：
 * 位置和形状都还在（各张卡竖向对齐不塌），挂上之后数字与填充自然出现。
 *
 * **刚挂上时那个 `-` 换成转圈**（真实反馈："挂载磁盘后要马上更新容量信息，更新的时候要有
 * loading 状态在进度条和 - 上"）：这一刻不是"不知道"，是"正在读"（见 useAgent 的
 * awaitMountUsage / repoUsageLoading）—— 用户刚点完挂载，看到 `-` 只会以为没挂上。
 *
 * **正在上传这一份时，这一格改报上传进度**（`upload` 有值，见 RepoProgress）：那一行还没有
 * 可算的用量（占位库连容量都还没有），但这块地方不能空着、更不能另找地方再插一条 ——
 * 同一格换数据源，行高列宽与别的库一致（真实反馈同 RepoMeter）。
 */
function RepoUsage({
  repo,
  usage,
  loading = false,
  upload,
  style
}: {
  repo: RepoDTO
  /** 本机实测用量（未挂载 / 取不到时为空）。 */
  usage?: AgentMountVolumeUsage
  /** 实测用量正在取数（见 RepoMountController 的 usageLoading）：`-` 与条都改成转圈。 */
  loading?: boolean
  /** 本机正在上传这个库：这一格改报上传进度。 */
  upload?: UploadState
  style?: CSSProperties
}): JSX.Element {
  const { t } = useI18n()
  if (repo.state === 'moving' && repo.move) return <RepoMoveProgress repo={repo} style={style} />
  if (upload) return <RepoProgress upload={upload} style={style} />
  const { percent, usedText, totalText, measured } = repoUsage(repo, usage, true)
  return (
    <RepoMeter
      label={t('repo.used')}
      hint={measured ? t('repo.usedLocalHint') : undefined}
      value={<UsageValueText usedText={usedText} totalText={totalText} loading={loading} />}
      percent={percent}
      muted={!measured}
      loading={loading}
      style={style}
    />
  )
}

/**
 * 「监控」入口：动作区里一个心电图图标按钮（与「设置」按钮同款），点击打开近 5 分钟的
 * 指标曲线弹窗。
 *
 * 变更（按真实反馈）：
 *   - 只保留**心电图图标**，不再带文字，和右侧「设置」按钮外观一致；
 *   - **所有已挂载的库**都显示这个入口（不再只限开了缓存的库）：未开缓存的库也经本机代理
 *     （纯透传），同样能看读写 IOPS（弹窗按 cache_enabled 分两态，见 CacheMetricsDialog）；
 *   - **直连模式**（direct_mode）下该库不经过代理、没有数据，入口**置灰**并说明原因；
 *   - 入口**始终占位**（未挂载也画，只是置灰）：它是动作区里的固定一档，跟着挂载状态出现/
 *     消失会让各库的按钮排长短不一，卡片之间对不齐（真实反馈："会导致 UI 不整齐"）。
 *     看不了就置灰，不隐藏 —— 顺便也告诉用户"挂上之后这里能看指标"。
 *
 * 整卡点击 = 打开挂载目录，所以按钮自己拦住冒泡，只负责打开弹窗。
 */
function RepoMonitorTag({
  controller,
  repoName
}: {
  controller: RepoMountController
  repoName: string
}): JSX.Element {
  const { t } = useI18n()
  const [metricsOpen, setMetricsOpen] = useState(false)
  const mount = controller.mount
  // 看不了的两态：本机没挂载这个库（压根没有这块盘）/ 直连模式（不过代理，没数据）。
  const cannotView = mount === undefined || Boolean(mount?.direct_mode)
  const tip =
    mount === undefined
      ? t('repo.monitor.unmountedHint')
      : mount.direct_mode
        ? t('repo.monitor.directHint')
        : mount.cache_enabled && !mount.cache_active
          ? `${t('repo.cache.inactive')}${mount.cache_error ? `（${mount.cache_error}）` : ''}`
          : t('repo.monitor.hint')

  return (
    <>
      <Tooltip title={tip}>
        <Button
          type="text"
          size="small"
          icon={<FundOutlined />}
          aria-label={t('repo.monitor.title')}
          disabled={cannotView}
          onClick={(event) => {
            event.stopPropagation()
            setMetricsOpen(true)
          }}
        />
      </Tooltip>
      {/* 弹窗只在能看的时候挂：没挂载时连 allocation_id 都没有，直连模式进去也只会是空图。 */}
      {mount && !mount.direct_mode ? (
        <CacheMetricsDialog
          open={metricsOpen}
          allocationId={mount.allocation_id}
          repoName={repoName}
          cacheEnabled={Boolean(mount.cache_enabled)}
          onClose={() => setMetricsOpen(false)}
        />
      ) : null}
    </>
  )
}

/** 峰值速度统一按 MB/s 展示（保留一位小数，0 也如实报 0.0 而不是 "-"）。 */
function formatPeakSpeed(bytesPerSecond: number | undefined): string {
  return `${((bytesPerSecond ?? 0) / (1024 * 1024)).toFixed(1)} MB/s`
}

/**
 * 卡片上的次要信息：把原来的「共享数量 / 母盘版本」换成用户真正关心的本地情况。
 *
 * 三态：
 *   - **未挂载**：显示「上次挂载」时间（只存本机、卸载不丢，见 GET /agent/state 的
 *     last_mounts）；本机从没挂过这个库则什么都不显示；
 *   - 已挂载但**没有缓存目标**（未开启缓存、或回退直连）：显示「缓存未开启」；
 *   - 已挂载且有缓存目标：显示 已缓存 / 命中率 / 峰值读速 / 峰值写速。
 *
 * 峰值来自代理的状态接口（近 5 分钟采样窗口内的峰值吞吐，见 GET /agent/cache 的
 * targets[].peak_*_bytes_per_sec），因此卡片不需要自己再拉历史曲线。
 */
function RepoCacheStatus({
  repoId,
  controller,
  style
}: {
  repoId: string
  controller: RepoMountController
  style?: CSSProperties
}): JSX.Element {
  const { t } = useI18n()
  const agent = useAgent()
  const mount = controller.mount
  // 未挂载时改展示"上次挂载这个库的时间"：本地根本没有这条挂载链路，
  // 原来的「缓存未开启」既没意义又会被误读成功能被关掉（真实反馈：那格空了）。
  // 时间只存在本机代理的本地状态里（卸载也不丢），没记录就什么都不显示。
  if (!mount) {
    const lastMount = agent.state?.last_mounts?.[repoId]
    if (!lastMount) return <></>
    return (
      <Typography.Text type="secondary" style={{ fontSize: fontSize.xs, ...style }}>
        {t('repo.card.lastMount', { time: formatTime(lastMount) })}
      </Typography.Text>
    )
  }
  const target = agent.cache?.targets.find((item) => item.allocation_id === mount.allocation_id)

  if (!target || !target.cache_enabled) {
    return (
      <Typography.Text type="secondary" style={{ fontSize: fontSize.xs, ...style }}>
        {t('repo.card.cacheOff')}
      </Typography.Text>
    )
  }

  const used = target.l1_used_bytes + target.l2_used_bytes
  return (
    <Typography.Text type="secondary" style={{ fontSize: fontSize.xs, ...style }}>
      {t('repo.card.cacheSummary', {
        used: formatBytes(used),
        rate: `${(target.hit_rate * 100).toFixed(1)}%`,
        read: formatPeakSpeed(target.peak_read_bytes_per_sec),
        write: formatPeakSpeed(target.peak_write_bytes_per_sec)
      })}
    </Typography.Text>
  )
}

/** 次要信息行：缓存状态 / 已预创建 / 挂载状态。 */
function RepoMetaLine({
  repo,
  controller,
  style
}: {
  repo: RepoDTO
  controller: RepoMountController
  style?: CSSProperties
}): JSX.Element {
  const { t } = useI18n()
  return (
    <Space size={spacing.md} wrap style={style}>
      <RepoCacheStatus repoId={repo.id} controller={controller} />
      {/* 「已预创建」= 差异盘与 iSCSI 目标在建库时就备好：分配到手的盘挂载即用，不必现场下发 */}
      {repo.pool ? (
        <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
          {t('repo.card.pool')}
        </Typography.Text>
      ) : null}
      <RepoMountStatus controller={controller} />
    </Space>
  )
}

/**
 * 上传中的动作区：把挂载按钮换成「暂停 / 继续」+「取消」。
 *
 * 为什么是这两个（真实反馈："上传状态下的挂载按钮改成暂停，加一个取消按钮"）：上传中的库
 * 既挂不了（服务端还没有可分配的盘），用户此刻真正想做的只有"停一下"和"算了"。
 *
 * 风格照旧：尺寸与间距都沿用动作区那一套（只换内容，不换排布），「暂停」占住原来挂载按钮
 * 那个主操作的位置；「取消」用与「上传任务」页同一款 danger 按钮 —— 同一个动作在两处长得一样。
 *
 * 「暂停」在建库阶段（completing）置灰：那一刻在干活的是服务端，本机停不住它，代理侧同样
 * 会拒绝（见 uploadTask.pause）—— 给一个按下去没反应的按钮比不给更糟。
 */
function RepoUploadActions({ upload }: { upload: UploadState }): JSX.Element {
  const { t } = useI18n()
  const { refreshUploads } = useAgent()
  // 暂停的标志位在代理那边（UploadState.paused），不在 state 里：暂停不改阶段，只停住推进。
  const paused = Boolean(upload.paused)
  const building = upload.state === 'completing'

  // 两个动作成功后都主动拉一次快照：SSE 一般已经推过来了，但代理没连上时（事件通道断了）
  // 就只剩这条路 —— 不然点了暂停按钮还显示"暂停"，用户只能以为点空了。
  const reload = (): void => {
    void refreshUploads().catch(() => undefined)
  }
  const fail = (error: unknown): void => {
    // 文案口径与 ErrorNotice 一致（业务错误按 code 本地化，见 api/errors.translateError）；
    // 卡片里摆不下那一整块提示，所以只弹一句。
    message.error(translateError(error) || t('upload.actionFailed'))
  }

  const toggle = useMutation({
    mutationFn: () => (paused ? agentApi.resumeUpload(upload.upload_id) : agentApi.pauseUpload(upload.upload_id)),
    onSuccess: reload,
    onError: fail
  })
  const cancel = useMutation({
    mutationFn: () => agentApi.cancelUpload(upload.upload_id),
    onSuccess: reload,
    onError: fail
  })

  return (
    <>
      <Tooltip title={building ? t('upload.pauseBlocked') : undefined}>
        <span style={{ display: 'inline-block' }}>
          <Button
            type="primary"
            size="small"
            disabled={building}
            loading={toggle.isPending}
            onClick={() => toggle.mutate()}
          >
            {paused ? t('upload.resume') : t('upload.pause')}
          </Button>
        </span>
      </Tooltip>
      <Button danger size="small" loading={cancel.isPending} onClick={() => cancel.mutate()}>
        {t('upload.cancelShort')}
      </Button>
    </>
  )
}

/**
 * 迁移中的动作区：把挂载按钮换成「中止迁移」。
 *
 * 迁移期间库不可挂载（服务端同样会拒绝），能做的只有中止；中止只会清理目标侧
 * 半成品并把库恢复为可用，不会丢源数据。
 */
function RepoMoveActions({ repo }: { repo: RepoDTO }): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const cancel = useMutation({
    mutationFn: () => api.cancelRepoMove(repo.id),
    onSuccess: () => {
      message.success(t('repo.move.canceled'))
      void queryClient.invalidateQueries({ queryKey: ['repos'] })
      void queryClient.invalidateQueries({ queryKey: ['repo', repo.id] })
    },
    onError: (error) => message.error(translateError(error) || t('repo.move.cancelFailed'))
  })
  // 收尾阶段（路径已切换、正在重建目标/删除源文件）已经无法安全回滚，不再允许终止。
  const finalizing = repo.move?.phase === 'finalizing'
  return (
    <Button
      danger
      size="small"
      disabled={finalizing}
      loading={cancel.isPending}
      onClick={() => cancel.mutate()}
    >
      {t('repo.move.cancel')}
    </Button>
  )
}

/**
 * 右侧动作区：「详情」（列表形态才有）/「配置」/ 挂载按钮。
 *
 * 按钮一律靠右（marginLeft: auto），不被内容挤到左边；容器可点打开挂载目录，
 * 因此整块动作区都要拦住冒泡。
 *
 * 上传中的**占位库**（服务端还没有这条记录，见 uploadPlaceholderRepo）没有可用的 id：
 * 「详情」会导航到一个不存在的库、「配置」保存时会把补丁打到不存在的库上。但这两个入口
 * **照样画、只是置灰** —— 少画两个按钮会让这一行的动作区短一截，跟别的行对不齐
 * （真实反馈："详情和挂载设置按钮不要不展示，这样会导致对不齐"）。做法与「监控」入口
 * 未挂载时一致：看不了就置灰，位置留着，顺手也告诉用户"这东西存在，只是现在还点不了"。
 *
 * 挂载按钮那一位则换成「暂停 / 继续」+「取消」（见 RepoUploadActions）——upstream 的库此刻
 * 挂不了，但两个动作的**尺寸与位置**照旧占着原来那一档。
 */
function RepoActions({
  repo,
  upload,
  controller,
  onOpen,
  onConfigure,
  showDetail,
  style
}: RepoCardProps & {
  controller: RepoMountController
  showDetail?: boolean
  style?: CSSProperties
}): JSX.Element {
  const { t } = useI18n()
  const stop = (event: MouseEvent): void => event.stopPropagation()
  const placeholder = isPlaceholderRepoId(repo.id)
  return (
    <div
      onClick={stop}
      style={{
        marginLeft: 'auto',
        flexShrink: 0,
        display: 'flex',
        alignItems: 'center',
        gap: spacing.xs,
        ...style
      }}
    >
      {showDetail ? (
        <Tooltip title={placeholder ? t('repo.uploadNotReady') : t('repo.card.detail')}>
          {/* 置灰的按钮不派发鼠标事件，Tooltip 得靠外层这层 span 才挂得住。 */}
          <span style={{ display: 'inline-block' }}>
            <Button
              type="text"
              size="small"
              icon={<InfoCircleOutlined />}
              aria-label={t('repo.card.detail')}
              disabled={placeholder}
              onClick={() => onOpen(repo.id)}
            />
          </span>
        </Tooltip>
      ) : null}
      <Tooltip title={placeholder ? t('repo.uploadNotReady') : t('repo.settings.title')}>
        <span style={{ display: 'inline-block' }}>
          <Button
            type="text"
            size="small"
            icon={<SettingOutlined />}
            aria-label={t('repo.settings.title')}
            disabled={placeholder}
            onClick={() => onConfigure(repo)}
          />
        </span>
      </Tooltip>
      {/* 监控入口放在「设置」右侧：它开的是一个普通指标弹窗。
          必须留在动作区里 —— antd Modal 走 React portal，但合成事件仍沿 React 组件树冒泡，
          若把弹窗挂在标题行（Card 子树内），点弹窗里任何位置都会冒泡到整卡的
          onClick（打开挂载目录）。动作区外层已拦截冒泡，弹窗才不会被当成"点卡片"。 */}
      <RepoShortcutButton repoName={repo.name} controller={controller} />
      <RepoMonitorTag controller={controller} repoName={repo.name} />
      {/* 上传中的条目：挂载按钮换成「暂停 / 继续」+「取消」（见 RepoUploadActions）。 */}
      {repo.state === 'moving' ? (
        <RepoMoveActions repo={repo} />
      ) : upload ? (
        <RepoUploadActions upload={upload} />
      ) : (
        <RepoMountActions controller={controller} />
      )}
    </div>
  )
}

/**
 * 创建快捷方式：把当前挂载点做成一个系统快捷方式，放到配置的目录（默认桌面）。
 *
 * 未挂载时没有可指向的路径，按钮置灰并说明原因。
 */
function RepoShortcutButton({
  repoName,
  controller
}: {
  repoName: string
  controller: RepoMountController
}): JSX.Element {
  const { t } = useI18n()
  const agent = useAgent()
  const [busy, setBusy] = useState(false)
  const mount = controller.mount
  const disabled = !controller.mounted || !mount

  const create = async (event: MouseEvent): Promise<void> => {
    event.stopPropagation()
    if (!mount) return
    setBusy(true)
    try {
      const result = await createShortcut({
        target: openTargetOf(mount),
        name: repoName,
        dir: agent.config?.shortcut_dir
      })
      if (result.ok) message.success(t('repo.shortcut.created', { path: result.path ?? '' }))
      else message.error(t('repo.shortcut.failed'))
    } catch {
      message.error(t('repo.shortcut.failed'))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Tooltip title={disabled ? t('repo.shortcut.unmounted') : t('repo.shortcut.create')}>
      {/* 置灰按钮不派发鼠标事件，Tooltip 靠外层 span 才挂得住。 */}
      <span style={{ display: 'inline-block' }}>
        <Button
          type="text"
          size="small"
          icon={<LinkOutlined />}
          aria-label={t('repo.shortcut.create')}
          loading={busy}
          disabled={disabled}
          onClick={(event) => void create(event)}
        />
      </span>
    </Tooltip>
  )
}

/**
 * 建库进度：把服务端"预创建"的三个阶段摊给用户看（建母盘 → 派生差异盘 → 建 iSCSI 目标）。
 *
 * 只在 state=creating 且拿到进度时有意义：建库是个几十秒到几分钟的异步过程，
 * 只显示"建库中"三个字，用户无法区分"在建"和"卡住了"。
 *
 * **本机正在上传这一份时不走这里**：那一刻卡片上要的是上传进度，它报在「已用」那一格里
 * （见 RepoUsage 的 upload 分支 / RepoProgress）。一行里只留一条进度，不叠两条；
 * 看别人的库建到哪一步的人没有上传任务，仍然是这一条。
 */
function RepoPrepareProgress({ repo }: { repo: RepoDTO }): JSX.Element | null {
  const { t } = useI18n()
  const prepare = repo.prepare
  if (repo.state !== 'creating' || !prepare) return null
  const percent = prepare.total > 0 ? Math.min(100, Math.round((prepare.done / prepare.total) * 100)) : 0

  return (
    <div style={{ marginTop: spacing.sm }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', gap: spacing.xs }}>
        <Typography.Text type="secondary" style={{ fontSize: fontSize.sm }}>
          {t(`repo.prepare.${prepare.phase}`)}
        </Typography.Text>
        <Typography.Text type="secondary" style={{ fontSize: fontSize.sm }}>
          {`${prepare.done}/${prepare.total}`}
        </Typography.Text>
      </div>
      <Progress
        percent={percent}
        showInfo={false}
        size="small"
        strokeColor={palette.accent}
        trailColor={palette.neutralFill}
        style={{ marginTop: spacing.xxs, marginBottom: 0 }}
      />
    </div>
  )
}

/**
 * 上传进度：本机正在上传这个库时，**用用量那一格**报"传到哪了"（见 RepoMeter）。
 *
 * 就是一块 RepoMeter，只换三样：标签「进度」、数值"已传 / 总量"、颜色一路蓝 ——
 * 位置、宽度、条的形状、百分数的画法全部与「已用」那一块共用，所以上传中的行和别的行
 * 在列表里长得一模一样（真实反馈："进度条我是指的复用存储用量那块的逻辑，不是让你另起一个"）。
 *
 * 两处刻意的取舍（真实反馈："上面展示的是进度 xx GB/yy GB；进度条里面也展示百分比和原来的
 * 样式一致，但是不变色，都蓝色就行"）：
 *   - 上面那行左边是「进度」、右边是"已传 / 总量"：只给百分比看不出还剩多少 GB；
 *   - **不变色**（color 固定主色蓝）：完成变绿、失败变红会让卡片之间跳色，而这条要回答的
 *     只有"还在不在动"，且「传到 90%」并不是「盘快满了」那种告警（见 UsageBar 的 color）。
 */
function RepoProgress({ upload, style }: { upload: UploadState; style?: CSSProperties }): JSX.Element {
  const { t } = useI18n()
  return (
    <RepoMeter
      label={t('repo.card.progress')}
      value={`${formatBytes(upload.uploaded_bytes)} / ${formatBytes(upload.total_bytes)}`}
      percent={uploadPercent(upload)}
      color={palette.accent}
      style={style}
    />
  )
}

/**
 * 迁移进度：复用「已用」那一格的排版，把数据源换成迁移快照（与上传进度同一处理）。
 *
 * 迁移期间库不可挂载，这一格本来也没有可算的用量；把进度放在原位置，卡片高度与
 * 其它库一致，用户也不会以为进度是另一条独立的东西。
 */
function RepoMoveProgress({ repo, style }: { repo: RepoDTO; style?: CSSProperties }): JSX.Element {
  const { t } = useI18n()
  const move = repo.move
  const total = move?.total_bytes ?? 0
  const done = move?.bytes ?? 0
  const percent =
    total > 0
      ? Math.min(100, Math.round((done / total) * 100))
      : move && move.files_total > 0
        ? Math.min(100, Math.round((move.files_done / move.files_total) * 100))
        : 0
  return (
    <RepoMeter
      label={t('repo.move.progress')}
      value={
        total > 0
          ? `${formatBytes(done)} / ${formatBytes(total)}`
          : `${move?.files_done ?? 0}/${move?.files_total ?? 0}`
      }
      percent={percent}
      color={palette.accent}
      style={style}
    />
  )
}

/**
 * 打开本机挂载目录（整卡的点击行为）。
 *
 * 原来的「打开目录」小按钮撤掉了（按用户要求把"点击卡片进详情"换成"打开文件"）；
 * 未挂载时打开只会得到系统级"路径不存在"，这里直接如实提示未挂载。
 */
function useOpenMountDir(controller: RepoMountController): () => void {
  const { t } = useI18n()
  return (): void => {
    if (!controller.mounted) {
      void message.warning(t('repo.mount.notMountedHint'))
      return
    }
    void controller.openMountDir()
  }
}

/**
 * 整卡点击的去处：**单击进入详情，双击打开挂载文件夹**。
 *
 * 双击会先触发两次 click，所以单击动作要延迟一小段再执行；双击到达时取消那次单击。
 * 上传中的库没有挂载目录（点下去只会得到一句"未挂载"），单击仍去「上传任务」页。
 *
 * 详情仍保留一个显式的「详情」图标按钮（见 RepoMountCard），不依赖单击。
 */
function useRepoCardInteractions(
  upload: UploadState | undefined,
  onOpenUpload: ((uploadId: string) => void) | undefined,
  openMountDir: () => void,
  onOpen: () => void
): { onClick: () => void; onDoubleClick: () => void } {
  const timer = useRef<number | null>(null)

  const clear = useCallback(() => {
    if (timer.current !== null) {
      window.clearTimeout(timer.current)
      timer.current = null
    }
  }, [])

  useEffect(() => () => clear(), [clear])

  const onClick = useCallback(() => {
    // 第二次 click 说明这是一次双击：取消待执行的单击动作，交给 onDoubleClick。
    if (timer.current !== null) {
      clear()
      return
    }
    timer.current = window.setTimeout(() => {
      timer.current = null
      if (upload && onOpenUpload) {
        onOpenUpload(upload.upload_id)
        return
      }
      onOpen()
    }, 250)
  }, [clear, upload, onOpenUpload, onOpen])

  const onDoubleClick = useCallback(() => {
    clear()
    // 上传中的库没有挂载目录：双击仍进上传任务页，避免弹一句"未挂载"。
    if (upload && onOpenUpload) {
      onOpenUpload(upload.upload_id)
      return
    }
    openMountDir()
  }, [clear, upload, onOpenUpload, openMountDir])

  return { onClick, onDoubleClick }
}

/**
 * 卡片形态（网格）：竖向排版，适合一眼扫过多块盘。
 *
 * 卡片上的动作分工（都能在这一张卡上完成，不必先进详情页）：
 *   - 整卡单击 = 进入详情；双击 = 打开文件（未挂载时如实提示；上传中的库进「上传任务」页）；
 *   - 库名前的状态圆点 = 本机挂载状态一眼可辨；
 *   - 「详情」图标 = 进详情页（默认停在「设置」tab）；
 *   - 「配置」图标 + 挂载按钮 = 这个库在本机怎么挂 / 挂上或卸下。
 *
 * 上传中的库**就用这套渲染**，不另起一套（真实反馈："上传中状态的存储库的卡片或列表不要
 * 展示那么多东西，和原有展示逻辑一样，只是状态标签不同"、"我所有的设计就是想让这个上传中的
 * 状态和其他保持一致的展示，但是复用元素"）—— 标签行多出「上传中」（暂停时再加「已暂停」），
 * 「已用」那一格改报上传进度（见 RepoProgress），挂载按钮那一档换成「暂停 / 继续」+「取消」
 * （见 RepoUploadActions）；其余零件（圆点 / 模式标签 / 发布时间信息 / 详情与配置图标）照旧。
 */
export function RepoCard(props: RepoCardProps): JSX.Element {
  return <RepoMountCard {...props} />
}

/** 库卡片：本机挂载这条链路（状态圆点 / 用量 / 挂载按钮 / 监控）只在这一支里。 */
function RepoMountCard({ repo, currentUserId, upload, onOpenUpload, onOpen, onConfigure }: RepoCardProps): JSX.Element {
  const { t } = useI18n()
  const mountController = useRepoMount(repo, currentUserId)
  const openMountDir = useOpenMountDir(mountController)
  const interactions = useRepoCardInteractions(upload, onOpenUpload, openMountDir, () => onOpen(repo.id))

  return (
    <Card
      hoverable
      onClick={interactions.onClick}
      onDoubleClick={interactions.onDoubleClick}
      style={{ height: '100%' }}
      styles={{ body: { padding: spacing.md } }}
    >
      {/* 标题行：状态圆点紧贴库名（8px 间距），右侧「详情」图标固定靠右 */}
      <div style={{ display: 'flex', alignItems: 'center', gap: spacing.sm }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: spacing.xs, flex: 1, minWidth: 0 }}>
          <RepoMountDot controller={mountController} />
          <Typography.Text strong ellipsis={{ tooltip: repo.name }} style={{ minWidth: 0, fontSize: fontSize.lg }}>
            {repo.name}
          </Typography.Text>
        </div>
        {/* 占位库（服务端还没有这条记录）没有服务端 id，进详情页只会 404（见 RepoActions
            同一处理）—— 但入口**照样占位、只是置灰**：不画它，这张卡的标题行就比别人少一个
            图标，与其它的卡长得不一样（见 RepoActions 里同一处说明）。 */}
        <Tooltip title={isPlaceholderRepoId(repo.id) ? t('repo.uploadNotReady') : t('repo.card.detail')}>
          <span style={{ display: 'inline-block', flexShrink: 0 }}>
            <Button
              type="text"
              size="small"
              icon={<InfoCircleOutlined />}
              aria-label={t('repo.card.detail')}
              disabled={isPlaceholderRepoId(repo.id)}
              onClick={(event) => {
                event.stopPropagation()
                onOpen(repo.id)
              }}
            />
          </span>
        </Tooltip>
      </div>

      <RepoTagRow
        repo={repo}
        controller={mountController}
        upload={upload}
        style={{ marginTop: spacing.sm }}
      />
      {/* 用量那一格：上传中的库由它改报上传进度（见 RepoUsage 的 upload 分支）。
          位置、宽度、形状都不变，所以卡片高度不会跳。 */}
      <RepoUsage
        repo={repo}
        usage={mountController.usage}
        loading={mountController.usageLoading}
        upload={upload}
        style={{ marginTop: spacing.md }}
      />

      {/* 建库进度条只在**没有**本机上传时出现：上传中的库，进度已经由上面那一格在报了，
          再画一条就是同一件事说两遍，还会把这张卡撑得比别的卡高。 */}
      {upload ? null : <RepoPrepareProgress repo={repo} />}

      <RepoMetaLine repo={repo} controller={mountController} style={{ marginTop: spacing.sm }} />

      {/* 底部一行只留动作：按钮一律靠右，不被内容挤到左边 */}
      <RepoActions
        repo={repo}
        currentUserId={currentUserId}
        controller={mountController}
        onOpen={onOpen}
        onConfigure={onConfigure}
        style={{ marginTop: spacing.sm }}
      />
    </Card>
  )
}

/**
 * 列表形态（整页宽度的横向卡片）：一库一行，适合库多、需要横向对齐比对的场景。
 *
 * 两栏结构：状态圆点 + 名称 + 标签 + 次要信息 | 用量 + 动作。
 * 与网格形态共用同一批零件（状态圆点/标签行/用量/次要信息/动作），只换排版，
 * 因此两种形态下看到的状态与可执行的动作**完全一致**（不会切个视图就少一个按钮）。
 *
 * 上传中的库同样走这一套：标签行多一个「上传中」（暂停时再加「已暂停」），**用量那一格**改报
 * 上传进度（原地换数据源，左栏不会多出一条进度、行高与别的行一致），挂载按钮那一档换成
 * 「暂停 / 继续」+「取消」；详情与配置两个入口照画、只是在占位库上置灰（见 RepoActions）。
 */
export function RepoRow(props: RepoCardProps): JSX.Element {
  return <RepoMountRow {...props} />
}

/** 库行（列表形态）。 */
function RepoMountRow({ repo, currentUserId, upload, onOpenUpload, onOpen, onConfigure }: RepoCardProps): JSX.Element {
  const mountController = useRepoMount(repo, currentUserId)
  const openMountDir = useOpenMountDir(mountController)
  const interactions = useRepoCardInteractions(upload, onOpenUpload, openMountDir, () => onOpen(repo.id))

  return (
    <Card
      hoverable
      onClick={interactions.onClick}
      onDoubleClick={interactions.onDoubleClick}
      styles={{ body: { padding: `${spacing.sm}px ${spacing.md}px` } }}
    >
      <div style={{ display: 'flex', alignItems: 'center', gap: spacing.md, flexWrap: 'wrap' }}>
        <div style={{ flex: '1 1 260px', minWidth: 0 }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: spacing.xs, minWidth: 0 }}>
            <RepoMountDot controller={mountController} />
            <Typography.Text strong ellipsis={{ tooltip: repo.name }} style={{ minWidth: 0, fontSize: fontSize.lg }}>
              {repo.name}
            </Typography.Text>
            <span style={{ flexShrink: 0 }}>
              <RepoTagRow repo={repo} controller={mountController} upload={upload} />
            </span>
          </div>
          {/* 建库进度条只在**没有**本机上传时出现（与卡片同一处判断）：上传中的库，进度由右边
              那一格在报。左栏于是始终只有"名称行 + 次要信息行"，上传中的行与别的行等高，
              名字、条、次要信息三样都横向对齐。 */}
          {upload ? null : <RepoPrepareProgress repo={repo} />}
          <RepoMetaLine repo={repo} controller={mountController} style={{ marginTop: spacing.xxs }} />
        </div>

        {/* 用量固定宽度：多行之间进度条与数值才能对齐着扫。这个宽度同时就是条的长度
            （窄了条短得像条线、填充比例看不出来），且容得下「已用」+ 最宽的一条文案
            （"- / 1000 MB" 这样的"已用 / 上限"）；上传进度复用同一格，所以两条数值也
            落在同一条竖线上。 */}
        <RepoUsage
          repo={repo}
          usage={mountController.usage}
          loading={mountController.usageLoading}
          upload={upload}
          style={{ flex: '0 0 auto', width: 240 }}
        />

        <RepoActions
          repo={repo}
          currentUserId={currentUserId}
          controller={mountController}
          onOpen={onOpen}
          onConfigure={onConfigure}
          showDetail
        />
      </div>
    </Card>
  )
}
