import type { CSSProperties, MouseEvent } from 'react'
import { Button, Card, Progress, Space, Tag, Tooltip, Typography, message } from 'antd'
import { InfoCircleOutlined, SettingOutlined } from '@ant-design/icons'

import { StatusTag } from '../../components/StatusTag'
import type { RepoDTO } from '../../api/types'
import { useI18n } from '../../i18n'
import { fontSize, palette, radius, spacing } from '../../tokens/palette'
import { repoUsage } from '../../utils/format'
import { repoModeLabel } from '../../utils/labels'
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

/** 库名前挂载状态圆点的直径：8px —— 与 16px 的库名同处一行时不抢戏，但一眼可辨。 */
const DOT_SIZE = 8

/**
 * 挂载状态圆点：库名前的**绿点**（本机已挂载）/ 灰点（未挂载）/ 红点（挂载失败）/
 * 蓝点（挂载中、卸载中）。
 *
 * 为什么从"磁盘徽标 + 呼吸灯"换成一个小圆点（真实反馈："图标的呼吸灯效果不要有背景，
 * 直接改成绿色圆点吧，不要在图标上呼吸了，在名称前加个绿点就可以了"）：
 *   - 原来的 40px 方块（底色 + 磁盘图标 + 外圈呼吸）在网格里比库名还抢眼，
 *     而它要说的只有一句话："这块盘正挂在本机"；
 *   - 圆点不用底色、不用图标、**也不带任何动效**：呼吸灯只是把标签行的「已挂载」
 *     又重复了一遍，卡片一多反而晃眼 —— 颜色本身就够读，静止也不会漏看。
 *
 * 四色与标签行的说法**逐一对齐**（真实反馈："如果没有挂载则展示灰色，挂载失败展示红色"）：
 *   - 已挂载 → 浅绿（successLight，小面积色块用浅一档，免得 8px 的绿压过库名）；
 *   - 未挂载（无记录）→ 灰（textTertiary）：绝大多数库都是这个状态，必须安静但可见；
 *   - 失败（错误 / 撤销 / 断线）→ 红（danger），与标签行那个红色感叹号同源（controller.failed）；
 *   - 挂载中/卸载中 → 蓝（accent），转场中，看一眼就知道还在动。
 * 因为始终有一颗点，**所有卡片/行的库名起始位置一致**，列表纵向扫读时名字不会忽左忽右。
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
    <span
      aria-hidden
      style={{
        width: DOT_SIZE,
        height: DOT_SIZE,
        borderRadius: radius.pill,
        background: color,
        flexShrink: 0,
        // 与库名同处一行：显式靠行中线对齐（行里可能有比圆点高得多的兄弟元素，比如列表形态
        // 的标签、按钮），不让它跟着行高/文字基线漂移（真实反馈："要在名称行上下居中"）。
        alignSelf: 'center',
        display: 'inline-block',
        lineHeight: 1,
        // 纯状态标记：不参与鼠标事件（整卡点击 = 打开挂载目录），也不做 tooltip
        // —— 挂载点就在下面的信息行里，悬浮提示只是重复。
        pointerEvents: 'none'
      }}
    />
  )
}

/** 标签行：库模式 + 库状态 + 本机挂载状态（三个并排读，避免"看着正常却挂载失败"的困惑）。 */
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
      <StatusTag group="repo" value={repo.state} />
      <RepoMountStateTag controller={controller} />
    </Space>
  )
}

/** 用量：口径统一在 repoUsage（分母 = 库容量），这里只负责画。 */
function RepoUsage({ repo, style }: { repo: RepoDTO; style?: CSSProperties }): JSX.Element {
  const { t } = useI18n()
  const { percent, text } = repoUsage(repo)
  return (
    <div style={style}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', gap: spacing.xs }}>
        <Typography.Text type="secondary" style={{ fontSize: fontSize.sm }}>
          {t('repo.used')}
        </Typography.Text>
        <Typography.Text ellipsis={{ tooltip: text }} style={{ fontSize: fontSize.sm }}>
          {text}
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
      {/* 配置图标紧挨挂载按钮左侧：改的是"这一个库在本机怎么挂"，与挂载是同一处动作 */}
      <Tooltip title={t('repo.settings.title')}>
        <Button
          type="text"
          size="small"
          icon={<SettingOutlined />}
          aria-label={t('repo.settings.title')}
          onClick={() => onConfigure(repo)}
        />
      </Tooltip>
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
      <RepoUsage repo={repo} style={{ marginTop: spacing.md }} />

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

        {/* 用量固定宽度：多行之间进度条与数值才能对齐着扫 */}
        <RepoUsage repo={repo} style={{ flex: '0 0 auto', width: 180 }} />

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
