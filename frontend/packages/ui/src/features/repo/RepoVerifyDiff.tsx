import { useEffect, useMemo, useRef, useState } from 'react'
import {
  Alert,
  Button,
  Input,
  Modal,
  Progress,
  Radio,
  Space,
  Table,
  Tag,
  Typography,
  message
} from 'antd'
import type { TableColumnsType } from 'antd'
import { FolderOpenOutlined } from '@ant-design/icons'

import { agentApi, selectDirectory, supportsDirectoryPicker } from '../../api/agentClient'
import { ApiError, isApiError } from '../../api/errors'
import { useApi } from '../../api/provider'
import type { LocalManifestStatus } from '../../api/agentTypes'
import type {
  JobDTO,
  VerifyDiffEntryDTO,
  VerifyDiffProgressDTO,
  VerifyDiffResultDTO
} from '../../api/types'
import { ErrorNotice } from '../../components/ErrorNotice'
import { isJobTerminal, useJobProgress } from '../../hooks/useJobProgress'
import { useI18n } from '../../i18n'
import { fontSize, spacing } from '../../tokens/palette'
import { formatBytes } from '../../utils/format'

type VerifyMode = 'size' | 'hash'

/** 差异表的一行：在服务端原始条目的基础上补一个类别字段。 */
type DiffRow = VerifyDiffEntryDTO & { kind: 'missing' | 'extra' | 'different' }

/** 服务端阶段 → 文案键（未知阶段退回服务端 message/原始码）。 */
const SERVER_PHASE_KEYS: Record<string, string> = {
  queued: 'repo.verifyDiff.phase.queued',
  prepare: 'repo.verifyDiff.phase.prepare',
  create: 'repo.verifyDiff.phase.create',
  mount: 'repo.verifyDiff.phase.mount',
  walk: 'repo.verifyDiff.phase.walk',
  compare: 'repo.verifyDiff.phase.compare',
  done: 'repo.verifyDiff.phase.done'
}

/** 把毫秒格式化成 12:34 / 1:02:03。 */
function formatElapsed(ms: number): string {
  const total = Math.max(0, Math.floor(ms / 1000))
  const hours = Math.floor(total / 3600)
  const minutes = Math.floor((total % 3600) / 60)
  const seconds = total % 60
  const ss = String(seconds).padStart(2, '0')
  if (hours > 0) return `${hours}:${String(minutes).padStart(2, '0')}:${ss}`
  return `${minutes}:${ss}`
}

/**
 * 上传核对：从母盘派生一块**临时差异盘**，在服务端挂载后与客户端选定目录的清单
 * （路径 / 大小 / 可选 SHA-256）逐文件对比，列出缺失、多余、内容不同的文件。
 *
 * 核对 1000GB 级目录要跑很久，所以两段都有可见进度：
 *  - 本机清单（扫描 / 算哈希）由代理异步任务上报，含"当前正在处理的文件"；
 *  - 服务端（建盘 / 挂载 / 逐文件读取 / 对比）由进度文件上报，含当前盘内文件与已读字节。
 */
export function RepoVerifyDiff({
  repoId,
  open,
  onClose
}: {
  repoId: string
  open: boolean
  onClose: () => void
}): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const [directory, setDirectory] = useState('')
  const [mode, setMode] = useState<VerifyMode>('size')
  const [job, setJob] = useState<JobDTO | null>(null)
  const [result, setResult] = useState<VerifyDiffResultDTO | null>(null)
  const [error, setError] = useState<unknown>(null)
  const [starting, setStarting] = useState(false)
  const [beganAt, setBeganAt] = useState(0)
  // 本机清单任务的实时状态（扫描/哈希阶段）。
  const [local, setLocal] = useState<{ id: string; dir: string; status: LocalManifestStatus } | null>(null)
  // 服务端进度（建盘/挂载/读取/对比阶段）。
  const [serverProgress, setServerProgress] = useState<VerifyDiffProgressDTO | null>(null)
  const startingJobRef = useRef(false)
  // 已取过结果的任务 ID：防止 effect 因 t 等依赖变化重复拉取结果。
  const resultFetchedFor = useRef('')

  const progress = useJobProgress(job?.id, job)
  const running = job !== null && !isJobTerminal(progress?.state)
  const busy = starting || local !== null || running

  // 每秒刷新一次"已用时"，让长时间核对至少能看出时间在走。
  const [clock, setClock] = useState(Date.now())
  useEffect(() => {
    if (!busy) return
    const timer = window.setInterval(() => setClock(Date.now()), 1000)
    return () => window.clearInterval(timer)
  }, [busy])

  // 本机清单：轮询代理状态 → 完成后取结果并提交服务端任务。
  useEffect(() => {
    if (!local || local.status.state !== 'running') return
    let cancelled = false
    const tick = async (): Promise<void> => {
      try {
        const status = await agentApi.manifestStatus(local.id)
        if (cancelled) return
        setLocal((prev) => (prev && prev.id === local.id ? { ...prev, status } : prev))
        if (status.state === 'failed') {
          setLocal(null)
          setError(
            status.error_code
              ? new ApiError({
                  kind: 'business',
                  code: status.error_code,
                  message: status.error_message
                })
              : new Error(status.error_message || t('repo.verifyDiff.failed'))
          )
          return
        }
        if (status.state !== 'succeeded' || startingJobRef.current) return
        startingJobRef.current = true
        // 先取完整清单，再交给服务端；这两个请求都可能处理几 MB 数据。
        const manifest = await agentApi.manifestResult(local.id)
        if (cancelled) return
        const started = await api.startVerifyDiff(repoId, {
          directory: local.dir,
          mode,
          files: manifest.entries
        })
        if (cancelled) return
        setJob(started)
        setLocal(null)
      } catch (err) {
        if (!cancelled) {
          setLocal(null)
          setError(err)
        }
      } finally {
        startingJobRef.current = false
      }
    }
    void tick()
    // 0.7s 一档：4MiB 读块下，"当前文件/已读字节"看起来是连续走的。
    const timer = window.setInterval(() => void tick(), 700)
    return () => {
      cancelled = true
      window.clearInterval(timer)
    }
    // 只按任务 ID 重建轮询：状态更新（running→succeeded）不能触发 effect 清理，
    // 否则清理会把 cancelled 置真，正在 await 的"取清单 + 提交服务端"就会被误取消。
  }, [api, local?.id, mode, repoId, t])

  // 服务端：轮询细粒度进度（阶段 / 当前文件 / 已读字节）。失败不致命，退回任务百分比。
  useEffect(() => {
    if (!job || !running) return
    let cancelled = false
    const tick = async (): Promise<void> => {
      try {
        const p = await api.getVerifyDiffProgress(repoId, job.id)
        if (!cancelled) setServerProgress(p)
      } catch {
        // 进度接口是"锦上添花"：拿不到就继续用 useJobProgress 的百分比。
      }
    }
    void tick()
    const timer = window.setInterval(() => void tick(), 1000)
    return () => {
      cancelled = true
      window.clearInterval(timer)
    }
  }, [api, job?.id, repoId, running])

  // 任务终态：成功则取结果，失败/取消则显示原因。
  useEffect(() => {
    if (!job || !isJobTerminal(progress?.state)) return
    if (progress?.state !== 'succeeded') {
      setError(new Error(job.last_error || t('repo.verifyDiff.failed')))
      return
    }
    if (resultFetchedFor.current === job.id) return
    resultFetchedFor.current = job.id
    let cancelled = false
    let timer: number | undefined
    let attempt = 0
    const load = async (): Promise<void> => {
      try {
        const res = await api.getVerifyDiffResult(repoId, job.id)
        if (!cancelled) setResult(res)
      } catch (err) {
        if (cancelled) return
        // 任务状态与结果文件之间存在极短的时间差（文件先写、状态后落库）。
        // 这种情况重试几次即可，不要把"还没写好"当成核对失败甩给用户。
        if (isApiError(err) && err.code === 'verifydiff.result_not_ready' && attempt < 10) {
          attempt += 1
          timer = window.setTimeout(() => void load(), 1000)
          return
        }
        resultFetchedFor.current = ''
        setError(err)
      }
    }
    void load()
    return () => {
      cancelled = true
      if (timer) window.clearTimeout(timer)
    }
  }, [api, job, progress?.state, repoId, t])

  const reset = (): void => {
    setJob(null)
    setResult(null)
    setError(null)
    setServerProgress(null)
    setLocal(null)
    setBeganAt(0)
    startingJobRef.current = false
    resultFetchedFor.current = ''
  }

  const pick = async (): Promise<void> => {
    if (!supportsDirectoryPicker()) {
      message.warning(t('repo.verifyDiff.pickerUnavailable'))
      return
    }
    const selection = await selectDirectory()
    if (selection && !selection.canceled && selection.path) setDirectory(selection.path)
  }

  const start = async (): Promise<void> => {
    if (!directory.trim()) {
      message.warning(t('repo.verifyDiff.needDir'))
      return
    }
    reset()
    setStarting(true)
    setBeganAt(Date.now())
    try {
      // 只启动本机清单任务（立即返回）；后续进度由上面的轮询展示。
      const started = await agentApi.startManifest(directory.trim(), mode === 'hash')
      setLocal({
        id: started.manifest_id,
        dir: directory.trim(),
        status: {
          manifest_id: started.manifest_id,
          state: 'running',
          phase: 'scan',
          done: 0,
          total: 0,
          bytes: 0,
          total_bytes: 0
        }
      })
    } catch (err) {
      setError(err)
    } finally {
      setStarting(false)
    }
  }

  const rows = useMemo<DiffRow[]>(() => {
    if (!result) return []
    return [
      ...result.missing.map((e) => ({ ...e, kind: 'missing' as const })),
      ...result.extra.map((e) => ({ ...e, kind: 'extra' as const })),
      ...result.different.map((e) => ({ ...e, kind: 'different' as const }))
    ]
  }, [result])

  const columns: TableColumnsType<DiffRow> = [
    {
      title: t('repo.verifyDiff.kind'),
      dataIndex: 'kind',
      key: 'kind',
      width: 100,
      render: (value: DiffRow['kind']) => (
        <Tag color={value === 'missing' ? 'error' : value === 'extra' ? 'warning' : 'gold'}>
          {t(`repo.verifyDiff.kind.${value}`)}
        </Tag>
      )
    },
    {
      title: t('repo.verifyDiff.path'),
      dataIndex: 'path',
      key: 'path',
      ellipsis: true,
      render: (value: string) => <Typography.Text style={{ fontSize: fontSize.xs }}>{value}</Typography.Text>
    },
    {
      title: t('repo.verifyDiff.localSize'),
      dataIndex: 'local_size',
      key: 'local_size',
      width: 110,
      render: (value?: number) => (value === undefined ? '-' : formatBytes(value))
    },
    {
      title: t('repo.verifyDiff.diskSize'),
      dataIndex: 'disk_size',
      key: 'disk_size',
      width: 110,
      render: (value?: number) => (value === undefined ? '-' : formatBytes(value))
    },
    {
      title: t('repo.verifyDiff.reason'),
      dataIndex: 'reason',
      key: 'reason',
      width: 90,
      render: (value?: string) =>
        value === 'size' || value === 'hash' ? t(`repo.verifyDiff.reason.${value}`) : '-'
    }
  ]

  // ---- 进度展示 ----
  const localStatus = local?.status
  const localPercent =
    localStatus && localStatus.phase === 'hash' && localStatus.total_bytes > 0
      ? Math.min(99, Math.round((localStatus.bytes / localStatus.total_bytes) * 100))
      : 0
  const percent = result
    ? 100
    : local
      ? localPercent
      : (progress?.progress ?? (serverProgress?.state === 'succeeded' ? 100 : 0))

  const stageText = local
    ? localStatus?.phase === 'hash'
      ? t('repo.verifyDiff.phase.hash')
      : t('repo.verifyDiff.phase.scan')
    : serverProgress
      ? (() => {
          const key = SERVER_PHASE_KEYS[serverProgress.phase]
          return key ? t(key) : (serverProgress.message || serverProgress.phase)
        })()
      : running
        ? t('repo.verifyDiff.phase.queued')
        : starting
          ? t('repo.verifyDiff.phase.starting')
          : ''
  const displayStage = result
    ? t('repo.verifyDiff.phase.done')
    : starting && !local
      ? t('repo.verifyDiff.phase.starting')
      : stageText

  let stats = ''
  if (localStatus) {
    stats =
      localStatus.phase === 'hash'
        ? t('repo.verifyDiff.localHashStats', {
            done: String(localStatus.done),
            total: String(localStatus.total),
            bytes: formatBytes(localStatus.bytes),
            totalBytes: formatBytes(localStatus.total_bytes)
          })
        : t('repo.verifyDiff.localScanStats', {
            count: String(localStatus.done),
            bytes: formatBytes(localStatus.bytes)
          })
  } else if (serverProgress && serverProgress.phase === 'walk') {
    stats = t('repo.verifyDiff.serverStats', {
      count: String(serverProgress.done),
      bytes: formatBytes(serverProgress.bytes),
      totalBytes: formatBytes(serverProgress.total_bytes)
    })
  }

  const currentFile = localStatus?.current || serverProgress?.current || ''

  return (
    <Modal
      open={open}
      title={t('repo.verifyDiff.title')}
      width={980}
      centered
      destroyOnClose
      footer={
        <Space>
          <Button onClick={onClose}>{t('common.close')}</Button>
          <Button type="primary" loading={starting} disabled={busy} onClick={() => void start()}>
            {t('repo.verifyDiff.start')}
          </Button>
        </Space>
      }
      onCancel={onClose}
    >
      <Space direction="vertical" size={spacing.md} style={{ width: '100%' }}>
        <Alert type="info" showIcon message={t('repo.verifyDiff.hint')} />

        <Space.Compact style={{ width: '100%' }}>
          <Input
            value={directory}
            placeholder={t('repo.verifyDiff.directoryPlaceholder')}
            onChange={(event) => setDirectory(event.target.value)}
            disabled={busy}
          />
          <Button icon={<FolderOpenOutlined />} onClick={() => void pick()} disabled={busy}>
            {t('repo.verifyDiff.pick')}
          </Button>
        </Space.Compact>

        <div>
          <Typography.Text style={{ display: 'block', marginBottom: spacing.xs }}>
            {t('repo.verifyDiff.mode')}
          </Typography.Text>
          <Radio.Group value={mode} onChange={(event) => setMode(event.target.value as VerifyMode)} disabled={busy}>
            <Radio.Button value="size">{t('repo.verifyDiff.modeSize')}</Radio.Button>
            <Radio.Button value="hash">{t('repo.verifyDiff.modeHash')}</Radio.Button>
          </Radio.Group>
          <Typography.Text type="secondary" style={{ display: 'block', fontSize: fontSize.xs, marginTop: spacing.xxs }}>
            {mode === 'hash' ? t('repo.verifyDiff.modeHashHint') : t('repo.verifyDiff.modeSizeHint')}
          </Typography.Text>
        </div>

        {busy || (beganAt > 0 && result) ? (
          <Space direction="vertical" size={spacing.xxs} style={{ width: '100%' }}>
            <Progress
              percent={percent}
              size="small"
              status={local && localStatus?.phase === 'scan' ? 'active' : undefined}
            />
            <Space size={spacing.md} wrap>
              <Typography.Text strong style={{ fontSize: fontSize.sm }}>
                {displayStage}
              </Typography.Text>
              {beganAt > 0 ? (
                <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
                  {t('repo.verifyDiff.elapsed', { time: formatElapsed(clock - beganAt) })}
                </Typography.Text>
              ) : null}
            </Space>
            {stats ? (
              <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
                {stats}
              </Typography.Text>
            ) : null}
            {currentFile ? (
              <Typography.Text
                type="secondary"
                ellipsis={{ tooltip: currentFile }}
                style={{ fontSize: fontSize.xs, maxWidth: '100%' }}
              >
                {`${t('repo.verifyDiff.currentFile')} ${currentFile}`}
              </Typography.Text>
            ) : null}
          </Space>
        ) : null}

        {error ? <ErrorNotice error={error} /> : null}

        {result ? (
          <Space direction="vertical" size={spacing.sm} style={{ width: '100%' }}>
            <Space wrap size={spacing.md}>
              <Typography.Text>{t('repo.verifyDiff.localFiles', { count: result.local_files })}</Typography.Text>
              <Typography.Text>{t('repo.verifyDiff.diskFiles', { count: result.disk_files })}</Typography.Text>
              <Tag color="success">{t('repo.verifyDiff.same', { count: result.same })}</Tag>
              <Tag color="error">{t('repo.verifyDiff.missing', { count: result.missing.length })}</Tag>
              <Tag color="warning">{t('repo.verifyDiff.extra', { count: result.extra.length })}</Tag>
              <Tag color="gold">{t('repo.verifyDiff.different', { count: result.different.length })}</Tag>
            </Space>
            {result.warnings.length > 0 ? (
              <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
                {t('repo.verifyDiff.warnings', { count: result.warnings.length })}
              </Typography.Text>
            ) : null}
            {result.truncated ? (
              <Typography.Text type="danger" style={{ fontSize: fontSize.xs }}>
                {t('repo.verifyDiff.truncated')}
              </Typography.Text>
            ) : null}
            <Table<DiffRow>
              size="small"
              rowKey={(row) => `${row.kind}:${row.path}`}
              columns={columns}
              dataSource={rows}
              pagination={{ pageSize: 100, size: 'small' }}
              scroll={{ y: 360 }}
              locale={{ emptyText: t('repo.verifyDiff.noDiff') }}
            />
          </Space>
        ) : null}
      </Space>
    </Modal>
  )
}
