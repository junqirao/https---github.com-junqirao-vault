import { Space, Typography } from 'antd'
import type { CSSProperties } from 'react'

import type { UploadState, UploadStatus } from '../../api/agentTypes'
import type { RepoDTO } from '../../api/types'
import { useI18n, type I18nApi } from '../../i18n'
import { fontSize, spacing } from '../../tokens/palette'
import { formatBytes, formatSpeed } from '../../utils/format'

/**
 * 上传阶段文案。
 *
 * 抽成共享函数（原本是 RepoCreateForm 里的私有实现）：上传阶段现在有两个入口 ——
 * 新建页的进度面板、从存储库列表点进来的「上传任务」页，两边必须说同一句话。
 */
export function uploadStageText(t: I18nApi['t'], state: UploadStatus): string {
  switch (state) {
    case 'scanning':
      return t('upload.stage.scanning')
    case 'creating':
      return t('upload.stage.creating')
    case 'uploading':
      return t('upload.stage.uploading')
    case 'completing':
      return t('upload.stage.completing')
    case 'done':
      return t('upload.done')
    case 'failed':
      return t('upload.failed')
    case 'canceled':
      return t('disk.copy.canceled')
  }
}

/**
 * 上传任务的进度百分比。
 *
 * 建库（completing）阶段改用建库进度：那一刻字节已经传完，但服务端正在把整棵树写进母盘，
 * 真正的长活（30G 的库就是 30G 的读 + 30G 的写）全在这里。让进度条继续钉在 100%，
 * 用户看到的就是"卡住了"；阶段文案已经换成"建库中"，进度条在这个节点重新计数才说得通。
 */
export function uploadPercent(upload: UploadState): number {
  if (upload.state === 'completing' && upload.build_progress) {
    return Math.min(100, upload.build_progress)
  }
  if (!upload.total_bytes || upload.total_bytes <= 0) return 0
  return Math.min(100, Math.round((upload.uploaded_bytes / upload.total_bytes) * 100))
}

/**
 * 剩余时间文案：秒数切成**两级**单位（"1 天 3 小时"、"3 分 12 秒"）。
 *
 * 只报两级：ETA 本身是估算值，精确到秒的长时间（"剩余 1 天 3 小时 27 分 41 秒"）
 * 既不可信也读不过来。0 / 缺失 = 不可估算，显示 `--` —— 速度还没稳下来时就是这个
 * 状态（见 internal/agent 的 etaSeconds，它刻意不给出离谱的数字）。
 */
export function uploadEtaText(t: I18nApi['t'], seconds: number | undefined): string {
  if (!seconds || seconds <= 0) return t('upload.etaUnknown')
  const total = Math.round(seconds)
  const days = Math.floor(total / 86400)
  const hours = Math.floor((total % 86400) / 3600)
  const minutes = Math.floor((total % 3600) / 60)
  const secs = total % 60
  if (days > 0) return t('duration.days', { days: String(days), hours: String(hours) })
  if (hours > 0) return t('duration.hours', { hours: String(hours), minutes: String(minutes) })
  if (minutes > 0) return t('duration.minutes', { minutes: String(minutes), seconds: String(secs) })
  return t('duration.seconds', { seconds: String(secs) })
}

/**
 * 进度元信息行：已传 / 总量 · 速度 · 预计剩余 · 文件 · 分块 · 写入母盘。
 *
 * 速度与剩余只在"还没传完"时展示：任务一旦结束（完成/失败/取消），这两个数就不再变化，
 * 留在界面上只会让人以为还在动。
 *
 * 「文件 N/M」为什么必须在这里：分块数只是字节搬运量，用户真正想知道的是"已经有多少个文件
 * 确实落盘并校验通过了"。服务端在上传过程中逐文件回报（不是等整库传完一次性校验），
 * 于是这个数字会一路往上走 —— 100GB 的任务也不会看起来像卡住。
 *
 * 「写入母盘 N%」为什么必须在这里：进入建库（completing）后字节进度已经满了，速度与剩余
 * 也随之失去意义（不再有网络传输），此时唯一还在动的是服务端把整棵树写进虚拟盘的进度。
 * 不展示它就只剩一句"建库中"钉在 100% 的进度条上 —— 30G 的库要几十分钟，用户只会认为卡死。
 */
export function UploadMeta({ upload }: { upload: UploadState }): JSX.Element {
  const { t } = useI18n()
  const building = upload.state === 'completing'
  const buildProgress = upload.build_progress ?? 0
  const running = upload.state === 'scanning' || upload.state === 'creating' ||
    upload.state === 'uploading' || upload.state === 'completing'

  return (
    <Space size={spacing.md} wrap>
      <Typography.Text type="secondary">
        {`${formatBytes(upload.uploaded_bytes)} / ${formatBytes(upload.total_bytes)}`}
      </Typography.Text>
      {running && !building ? (
        <Typography.Text type="secondary">
          {`${t('upload.speed')} ${formatSpeed(upload.speed_bytes_per_sec)}`}
        </Typography.Text>
      ) : null}
      {running && !building ? (
        <Typography.Text type="secondary">
          {`${t('upload.eta')} ${uploadEtaText(t, upload.eta_seconds)}`}
        </Typography.Text>
      ) : null}
      {building && buildProgress > 0 ? (
        <Typography.Text type="secondary">
          {t('upload.task.buildProgress', { percent: String(buildProgress) })}
        </Typography.Text>
      ) : null}
      {upload.files_total > 0 ? (
        <Typography.Text type="secondary">
          {`${t('upload.task.files')} ${upload.files_done}/${upload.files_total}`}
        </Typography.Text>
      ) : null}
      {upload.chunk_total > 0 ? (
        <Typography.Text type="secondary">
          {`${t('upload.task.chunks')} ${upload.chunk_done}/${upload.chunk_total}`}
        </Typography.Text>
      ) : null}
    </Space>
  )
}

/**
 * 「当前正在处理中的文件」一行：路径过长时省略号收尾，完整路径进 tooltip。
 *
 * 为什么单独一行而不是塞进 UploadMeta：文件路径可能很长（目录嵌套），放进横向的元信息行
 * 会把那一行撑爆、也会把速度/ETA 挤走。没有可展示的文件名（扫描中、或任务已结束）时
 * 整行不渲染 —— 显示一个空的"当前文件："比不显示更让人困惑。
 */
export function UploadCurrentFile({ upload, style }: { upload: UploadState; style?: CSSProperties }): JSX.Element | null {
  const { t } = useI18n()
  const file = upload.current_file
  if (!file) return null
  return (
    <Typography.Text
      type="secondary"
      ellipsis={{ tooltip: file }}
      style={{ fontSize: fontSize.sm, maxWidth: '100%', ...style }}
    >
      {`${t('upload.task.currentFile')} ${file}`}
    </Typography.Text>
  )
}

/** 已知的上传提示码 → 文案键。未知码不渲染（不把内部码甩到界面上）。 */
const UPLOAD_NOTICE_KEYS: Record<string, string> = {
  repo_overwritten: 'notice.repoOverwritten',
}

/**
 * 「这条任务值得你知道的事」一行（当前只有"同名库会被覆盖重建"）。
 *
 * 上传开始时服务端会清掉同名残留、或者接管一个同名库去重建 —— 这会改变服务端已有的东西，
 * 静默做完对用户不公平。用 warning 色而不是次要色：它描述的是一个**已经发生**的变更。
 */
export function UploadNotice({ upload, style }: { upload: UploadState; style?: CSSProperties }): JSX.Element | null {
  const { t } = useI18n()
  const key = upload.notice ? UPLOAD_NOTICE_KEYS[upload.notice] : undefined
  if (!key) return null
  return (
    <Typography.Text
      type="warning"
      ellipsis={{ tooltip: t(key) }}
      style={{ fontSize: fontSize.sm, maxWidth: '100%', ...style }}
    >
      {t(key)}
    </Typography.Text>
  )
}

/** 列表里为"上传中的库"合成的占位库，其 id 用的前缀。 */
export const UPLOAD_REPO_ID_PREFIX = 'upload:'

/**
 * 是否为"上传中占位库"：服务端还没有这条记录（扫描 / 建会话 / 传数据三个阶段），
 * 由列表临时合成。
 *
 * 它**不是服务端 id**：详情页、挂载链路（会拿 id 去查分配）都不能用它。这些入口对占位库
 * 一律不渲染（见 RepoCards 的 RepoActions 与 useRepoMount）；卡片本身的渲染与普通库无异。
 */
export function isPlaceholderRepoId(id: string): boolean {
  return id.startsWith(UPLOAD_REPO_ID_PREFIX)
}

/**
 * 把一条进行中的上传合成为"库"的样子，让它和其他库并排躺在列表的同一个分组里。
 *
 * 为什么需要：服务端要到"数据传完、开始建母盘"时才有这个库的记录，而发起上传的人必须马上
 * 在列表里看到"这块库正在生成" —— 否则刚点完上传的人会以为什么都没发生。
 *
 * 只填界面要读的字段：状态填 importing（服务端口径：数据已收完、正在写母盘 —— 这样"还差几秒
 * 服务端记录才出现"的那一帧不会显示成别的状态），容量与分配相关一律 0 —— 此刻还没有任何可
 * 展示的数字，编一个出来只会误导。
 *
 * 注意**标签取值另算**：占位库还在传数据时显示的是「上传中」，见 repoStateValue。
 */
export function uploadPlaceholderRepo(upload: UploadState, ownerId: string): RepoDTO {
  return {
    id: `${UPLOAD_REPO_ID_PREFIX}${upload.upload_id || upload.local_dir}`,
    name: upload.repo_name,
    // 模式只能给占位值：UploadState 里没有它（它只在 start 的入参里，之后不回传），
    // 而界面据此**不显示**占位库的模式标签 —— 显示"共享"而用户选的是独占，比不显示更糟。
    mode: 'shared',
    owner_id: ownerId,
    parent_version: 0,
    share_count: 0,
    max_diff_disks: 0,
    quota_bytes: 0,
    capacity_bytes: 0,
    used_bytes: 0,
    state: 'importing',
    pool: false,
    created_at: upload.started_at,
    updated_at: upload.started_at
  }
}

/**
 * 状态标签该显示成哪个取值（卡片与列表共用一处口径）。
 *
 * 服务端的 `importing` 是「**正在写母盘**」：这个库记录要到"数据都收完了、建库任务开跑"
 * 才存在（见 ensureRepoForUpload），所以给它贴「上传中」会让人以为数据还在传、白等一场
 * （真实反馈："写入母盘这个过程状态展示要展示写入中，如果上传中的话可能会造成误会"）。
 *
 * 「上传中」属于**本机代理**那侧的阶段：占位库（服务端还没有这条记录）正是"数据还在传"，
 * 用一个只活在展示层的取值 `uploading` 表达它。于是同一个 `importing` 在两种场景下各说各的：
 * 服务端记录 → 写入中，占位库 → 上传中。
 *
 * 唯一要接上的一帧：占位库走到 completing 时字节已经传完、建库任务已提交（见 agent 的
 * uploadResumeRecord.Phase），而服务端的库记录可能还没建出来 —— 那一刻照样报「写入中」，
 * 与服务端状态无缝接上。
 */
export function repoStateValue(repo: RepoDTO, upload?: UploadState): string {
  if (!isPlaceholderRepoId(repo.id)) return repo.state
  return upload?.state === 'completing' ? 'importing' : 'uploading'
}
