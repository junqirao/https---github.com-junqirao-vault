import { useEffect, useMemo, useState } from 'react'
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
import { useApi } from '../../api/provider'
import type { JobDTO, VerifyDiffEntryDTO, VerifyDiffResultDTO } from '../../api/types'
import { ErrorNotice } from '../../components/ErrorNotice'
import { isJobTerminal, useJobProgress } from '../../hooks/useJobProgress'
import { useI18n } from '../../i18n'
import { fontSize, spacing } from '../../tokens/palette'
import { formatBytes } from '../../utils/format'

type VerifyMode = 'size' | 'hash'

/** 差异表的一行：在服务端原始条目的基础上补一个类别字段。 */
type DiffRow = VerifyDiffEntryDTO & { kind: 'missing' | 'extra' | 'different' }

/**
 * 上传核对：从母盘派生一块**临时差异盘**，在服务端挂载后与客户端选定目录的清单
 * （路径 / 大小 / 可选 SHA-256）逐文件对比，列出缺失、多余、内容不同的文件。
 *
 * 临时盘不登记、不计配额，任务结束即删除；现有库与差异盘完全不受影响。
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

  const progress = useJobProgress(job?.id, job)
  const running = job !== null && !isJobTerminal(progress?.state)

  useEffect(() => {
    if (!job || !isJobTerminal(progress?.state)) return
    if (progress?.state !== 'succeeded') {
      setError(new Error(job.last_error || t('repo.verifyDiff.failed')))
      return
    }
    let cancelled = false
    void api
      .getVerifyDiffResult(repoId, job.id)
      .then((res) => {
        if (!cancelled) setResult(res)
      })
      .catch((err) => {
        if (!cancelled) setError(err)
      })
    return () => {
      cancelled = true
    }
  }, [api, job, progress?.state, repoId, t])

  const reset = (): void => {
    setJob(null)
    setResult(null)
    setError(null)
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
    try {
      // 先在**本机**生成清单（hash 模式会读完整个目录），再把清单交给服务端。
      const manifest = await agentApi.fsManifest(directory.trim(), mode === 'hash')
      const started = await api.startVerifyDiff(repoId, {
        directory: directory.trim(),
        mode,
        files: manifest.entries
      })
      setJob(started)
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
          <Button type="primary" loading={starting} disabled={running} onClick={() => void start()}>
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
            disabled={running}
          />
          <Button icon={<FolderOpenOutlined />} onClick={() => void pick()} disabled={running}>
            {t('repo.verifyDiff.pick')}
          </Button>
        </Space.Compact>

        <div>
          <Typography.Text style={{ display: 'block', marginBottom: spacing.xs }}>
            {t('repo.verifyDiff.mode')}
          </Typography.Text>
          <Radio.Group value={mode} onChange={(event) => setMode(event.target.value as VerifyMode)} disabled={running}>
            <Radio.Button value="size">{t('repo.verifyDiff.modeSize')}</Radio.Button>
            <Radio.Button value="hash">{t('repo.verifyDiff.modeHash')}</Radio.Button>
          </Radio.Group>
          <Typography.Text type="secondary" style={{ display: 'block', fontSize: fontSize.xs, marginTop: spacing.xxs }}>
            {mode === 'hash' ? t('repo.verifyDiff.modeHashHint') : t('repo.verifyDiff.modeSizeHint')}
          </Typography.Text>
        </div>

        {running ? (
          <Space direction="vertical" style={{ width: '100%' }}>
            <Progress percent={progress?.progress ?? 0} size="small" />
            <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
              {t('repo.verifyDiff.running')}
            </Typography.Text>
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
