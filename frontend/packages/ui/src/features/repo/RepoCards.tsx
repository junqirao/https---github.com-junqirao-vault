import type { CSSProperties, MouseEvent } from 'react'
import { useState } from 'react'
import { Badge, Button, Card, Progress, Space, Tag, Tooltip, Typography, message } from 'antd'
import { InfoCircleOutlined, SettingOutlined, FundOutlined } from '@ant-design/icons'

import { StatusTag } from '../../components/StatusTag'
import { UsageBar, UsageValueText } from '../../components/UsageBar'
import type { AgentMountVolumeUsage } from '../../api/agentTypes'
import type { RepoDTO } from '../../api/types'
import { useI18n } from '../../i18n'
import { fontSize, palette, spacing } from '../../tokens/palette'
import { repoUsage } from '../../utils/format'
import { repoModeLabel } from '../../utils/labels'
import { CacheMetricsDialog } from './CacheMetricsDialog'
import {
  RepoMountActions,
  RepoMountStateTag,
  RepoMountStatus,
  useRepoMount,
  type RepoMountController
} from './RepoMount'

export interface RepoCardProps {
  repo: RepoDTO
  currentUserId: string
  onOpen: (repoId: string) => void
  onConfigure: (repo: RepoDTO) => void
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
  style
}: {
  repo: RepoDTO
  controller: RepoMountController
  style?: CSSProperties
}): JSX.Element {
  return (
    <Space size={spacing.xs} wrap style={style}>
      <Tag>{repoModeLabel(repo.mode)}</Tag>
      {repo.state === 'active' ? null : <StatusTag group="repo" value={repo.state} />}
      <RepoMountStateTag controller={controller} />
    </Space>
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
 */
function RepoUsage({
  repo,
  usage,
  loading = false,
  style
}: {
  repo: RepoDTO
  /** 本机实测用量（未挂载 / 取不到时为空）。 */
  usage?: AgentMountVolumeUsage
  /** 实测用量正在取数（见 RepoMountController 的 usageLoading）：`-` 与条都改成转圈。 */
  loading?: boolean
  style?: CSSProperties
}): JSX.Element {
  const { t } = useI18n()
  const { percent, usedText, totalText, measured } = repoUsage(repo, usage, true)
  return (
    <div style={style}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', gap: spacing.xs }}>
        {measured ? (
          <Tooltip title={t('repo.usedLocalHint')}>
            <Typography.Text type="secondary" style={{ fontSize: fontSize.sm, cursor: 'help' }}>
              {t('repo.used')}
            </Typography.Text>
          </Tooltip>
        ) : (
          <Typography.Text type="secondary" style={{ fontSize: fontSize.sm }}>
            {t('repo.used')}
          </Typography.Text>
        )}
        <Typography.Text style={{ fontSize: fontSize.sm }}>
          <UsageValueText usedText={usedText} totalText={totalText} loading={loading} />
        </Typography.Text>
      </div>
      <UsageBar percent={percent} muted={!measured} loading={loading} />
    </div>
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

/** 次要信息行：共享数量 / 已预创建 / 母盘版本 / 挂载点。 */
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
      <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
        {t('repo.card.shareCount')} {repo.share_count ?? repo.max_diff_disks}
      </Typography.Text>
      {/* 「已预创建」= 差异盘与 iSCSI 目标在建库时就备好：分配到手的盘挂载即用，不必现场下发 */}
      {repo.pool ? (
        <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
          {t('repo.card.pool')}
        </Typography.Text>
      ) : null}
      <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
        {t('repo.parentVersion')} {repo.parent_version}
      </Typography.Text>
      <RepoMountStatus controller={controller} />
    </Space>
  )
}

/**
 * 右侧动作区：「详情」（列表形态才有）/「配置」/ 挂载按钮。
 *
 * 按钮一律靠右（marginLeft: auto），不被内容挤到左边；容器可点打开挂载目录，
 * 因此整块动作区都要拦住冒泡。
 */
function RepoActions({
  repo,
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
        <Tooltip title={t('repo.card.detail')}>
          <Button
            type="text"
            size="small"
            icon={<InfoCircleOutlined />}
            aria-label={t('repo.card.detail')}
            onClick={() => onOpen(repo.id)}
          />
        </Tooltip>
      ) : null}
      <Tooltip title={t('repo.settings.title')}>
        <Button
          type="text"
          size="small"
          icon={<SettingOutlined />}
          aria-label={t('repo.settings.title')}
          onClick={() => onConfigure(repo)}
        />
      </Tooltip>
      {/* 监控入口放在「设置」右侧：它开的是一个普通指标弹窗。
          必须留在动作区里 —— antd Modal 走 React portal，但合成事件仍沿 React 组件树冒泡，
          若把弹窗挂在标题行（Card 子树内），点弹窗里任何位置都会冒泡到整卡的
          onClick（打开挂载目录）。动作区外层已拦截冒泡，弹窗才不会被当成"点卡片"。 */}
      <RepoMonitorTag controller={controller} repoName={repo.name} />
      <RepoMountActions controller={controller} />
    </div>
  )
}

/**
 * 建库进度：把服务端"预创建"的三个阶段摊给用户看（建母盘 → 派生差异盘 → 建 iSCSI 目标）。
 *
 * 只在 state=creating 且拿到进度时有意义：建库是个几十秒到几分钟的异步过程，
 * 只显示"建库中"三个字，用户无法区分"在建"和"卡住了"。
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
 * 卡片形态（网格）：竖向排版，适合一眼扫过多块盘。
 *
 * 卡片上的动作分工（都能在这一张卡上完成，不必先进详情页）：
 *   - 整卡点击 = 打开文件（未挂载时如实提示）；
 *   - 库名前的状态圆点 = 本机挂载状态一眼可辨；
 *   - 「详情」图标 = 进详情页（默认停在「设置」tab）；
 *   - 「配置」图标 + 挂载按钮 = 这个库在本机怎么挂 / 挂上或卸下。
 */
export function RepoCard({ repo, currentUserId, onOpen, onConfigure }: RepoCardProps): JSX.Element {
  const { t } = useI18n()
  const mountController = useRepoMount(repo, currentUserId)
  const openMountDir = useOpenMountDir(mountController)

  return (
    <Card
      hoverable
      onClick={openMountDir}
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
        <Tooltip title={t('repo.card.detail')}>
          <Button
            type="text"
            size="small"
            icon={<InfoCircleOutlined />}
            aria-label={t('repo.card.detail')}
            onClick={(event) => {
              event.stopPropagation()
              onOpen(repo.id)
            }}
          />
        </Tooltip>
      </div>

      <RepoTagRow repo={repo} controller={mountController} style={{ marginTop: spacing.sm }} />
      <RepoUsage
        repo={repo}
        usage={mountController.usage}
        loading={mountController.usageLoading}
        style={{ marginTop: spacing.md }}
      />

      {/* 建库进度：只在这一刻有信息量（服务端正在预创建差异盘/目标），建完换回常驻信息行 */}
      <RepoPrepareProgress repo={repo} />

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
 */
export function RepoRow({ repo, currentUserId, onOpen, onConfigure }: RepoCardProps): JSX.Element {
  const mountController = useRepoMount(repo, currentUserId)
  const openMountDir = useOpenMountDir(mountController)

  return (
    <Card
      hoverable
      onClick={openMountDir}
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
              <RepoTagRow repo={repo} controller={mountController} />
            </span>
          </div>
          <RepoPrepareProgress repo={repo} />
          <RepoMetaLine repo={repo} controller={mountController} style={{ marginTop: spacing.xxs }} />
        </div>

        {/* 用量固定宽度：多行之间进度条与数值才能对齐着扫。这个宽度同时就是条的长度
            （窄了条短得像条线、填充比例看不出来），且容得下「已用」+ 最宽的一条文案
            （"- / 1000 MB" 这样的"已用 / 上限"）。 */}
        <RepoUsage
          repo={repo}
          usage={mountController.usage}
          loading={mountController.usageLoading}
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
