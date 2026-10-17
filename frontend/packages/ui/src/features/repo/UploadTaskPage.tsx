import { useEffect } from 'react'
import { Button, Progress, Space, Typography } from 'antd'
import { useMutation } from '@tanstack/react-query'

import { EmptyState } from '../../components/EmptyState'
import { ErrorNotice } from '../../components/ErrorNotice'
import { PageShell } from '../../components/PageShell'
import { SectionCard } from '../../components/SectionCard'
import { agentApi } from '../../api/agentClient'
import { ApiError } from '../../api/errors'
import { useAgent } from '../../hooks/useAgent'
import { useI18n } from '../../i18n'
import { fontSize, spacing } from '../../tokens/palette'
import { formatTime } from '../../utils/format'
import { UploadCurrentFile, UploadMeta, UploadNotice, uploadPercent, uploadStageText } from './uploadView'

export interface UploadTaskPageProps {
  uploadId: string
  onBack: () => void
  onOpenRepo: (repoId: string) => void
}

/**
 * 上传任务页：一块"上传中的存储库"点进来的落地页。
 *
 * 为什么需要单独一页（真实反馈："在上传中的存储库，要有一个独立的状态……点进去是上传任务
 * 页面，这个页面要实时展示速度，和 ETA"）：
 *   - 上传中的库在列表里只有一张卡片，摆不下"传到哪了、还剩多久、从哪个目录来"；
 *   - 长任务（几十 GB、几个小时）期间，用户需要一个能放着不管、随时回来瞄一眼进度的页面，
 *     而这份进度只存在**本机代理**内存里（此时服务端还不知道这个库），只能由客户端承载。
 *
 * 数据来自 useAgent 的上传快照：代理的 SSE `upload` 事件已带速率与 ETA，界面不自己轮询
 * —— 代理推一次进度，这里跟着动一次。刷新页面后靠 GET /agent/uploads 恢复。
 */
export function UploadTaskPage({ uploadId, onBack, onOpenRepo }: UploadTaskPageProps): JSX.Element {
  const { t } = useI18n()
  const { uploads, refreshUploads } = useAgent()
  const upload = uploads.find((item) => item.upload_id === uploadId)

  // 直接打开/刷新本页时快照可能还没拉回来，主动补一次，免得把"还没加载"显示成"任务不存在"。
  useEffect(() => {
    void refreshUploads().catch(() => undefined)
  }, [refreshUploads])

  // 取消走本机代理（不是服务端）：任务此时的进度与生命周期都还在客户端手里 ——
  // 分块还没传完时，服务端根本不知道这个库的存在。
  const cancelMutation = useMutation({
    mutationFn: (id: string) => agentApi.cancelUpload(id),
    onSuccess: () => {
      void refreshUploads().catch(() => undefined)
    }
  })

  // 暂停 / 恢复同样走本机代理（见 internal/agent/upload.go 的 uploadTask.pause）：只停住
  // "继续往下传"，服务端会话与已传分块原样保留，随时能接着传 —— 与存储库卡片上那两个按钮
  // 是同一个接口，两处都能操作。
  const pauseMutation = useMutation({
    mutationFn: (id: string) => (upload?.paused ? agentApi.resumeUpload(id) : agentApi.pauseUpload(id)),
    onSuccess: () => {
      void refreshUploads().catch(() => undefined)
    }
  })

  if (!upload) {
    return (
      <PageShell
        title={t('upload.task.title')}
        extra={<Button onClick={onBack}>{t('upload.task.back')}</Button>}
      >
        <EmptyState title={t('upload.task.notFound')} description={t('upload.task.notFoundDesc')} />
      </PageShell>
    )
  }

  const active =
    upload.state !== 'done' && upload.state !== 'failed' && upload.state !== 'canceled'

  return (
    <PageShell
      title={upload.repo_name || t('upload.task.title')}
      extra={<Button onClick={onBack}>{t('upload.task.back')}</Button>}
    >
      <SectionCard
        title={uploadStageText(t, upload.state)}
        extra={
          active ? (
            <Space size={spacing.xs}>
              <Button
                size="small"
                disabled={upload.state === 'completing'}
                loading={pauseMutation.isPending}
                onClick={() => pauseMutation.mutate(upload.upload_id)}
              >
                {upload.paused ? t('upload.resume') : t('upload.pause')}
              </Button>
              <Button
                danger
                size="small"
                loading={cancelMutation.isPending}
                onClick={() => cancelMutation.mutate(upload.upload_id)}
              >
                {t('upload.cancel')}
              </Button>
            </Space>
          ) : upload.state === 'done' && upload.repo_id ? (
            <Button type="primary" size="small" onClick={() => onOpenRepo(upload.repo_id as string)}>
              {t('upload.openRepo')}
            </Button>
          ) : undefined
        }
      >
        <Space direction="vertical" size={spacing.sm} style={{ width: '100%' }}>
          <Progress
            percent={uploadPercent(upload)}
            status={
              upload.state === 'failed'
                ? 'exception'
                : upload.state === 'done'
                  ? 'success'
                  : // 暂停时不要 active 那条流动动画：它表达的是"正在传"，而此刻恰恰是停住的。
                    active && !upload.paused
                    ? 'active'
                    : 'normal'
            }
          />
          <UploadMeta upload={upload} />
          {/* 「正在处理中的文件」是这一页最有用的实时信息：它证明库不是卡住了，
              而且服务端是**逐文件**校验的，这个数字会一路往前走（真实反馈）。 */}
          <UploadCurrentFile upload={upload} />
          {/* 「同名库被本次上传覆盖重建」这类提示：它是服务端在上传开始时做的事，
              用户应当知道，而不是等库变了才发现。 */}
          <UploadNotice upload={upload} />
          <Typography.Text type="secondary" style={{ fontSize: fontSize.sm }}>
            {`${t('upload.task.source')} ${upload.local_dir}`}
          </Typography.Text>
          <Typography.Text type="secondary" style={{ fontSize: fontSize.sm }}>
            {`${t('field.createdAt')} ${formatTime(upload.started_at)}`}
          </Typography.Text>
          {active ? <Typography.Text type="secondary">{t('upload.task.pendingRepo')}</Typography.Text> : null}
          {upload.state === 'failed' ? (
            <ErrorNotice error={new ApiError({ kind: 'business', code: upload.error || 'system.internal' })} />
          ) : null}
          {cancelMutation.error ? <ErrorNotice error={cancelMutation.error} /> : null}
          {pauseMutation.error ? <ErrorNotice error={pauseMutation.error} /> : null}
        </Space>
      </SectionCard>
    </PageShell>
  )
}
