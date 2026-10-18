import { useMemo, useState } from 'react'
import { Button, Popconfirm, Progress, Select, Tooltip, Typography, message } from 'antd'
import type { TableColumnsType } from 'antd'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { DataTable } from '../../components/DataTable'
import { EmptyState } from '../../components/EmptyState'
import { ErrorNotice } from '../../components/ErrorNotice'
import { PageShell } from '../../components/PageShell'
import { StatusTag } from '../../components/StatusTag'
import { translateError } from '../../api/errors'
import { useApi } from '../../api/provider'
import type { JobDTO, JobState } from '../../api/types'
import { usePermission } from '../../hooks/useAuth'
import { isJobTerminal } from '../../hooks/useJobProgress'
import { useI18n } from '../../i18n'
import { formatTime } from '../../utils/format'
import { jobStateLabel, jobTypeLabel } from '../../utils/labels'

const JOB_STATES: JobState[] = ['pending', 'running', 'succeeded', 'failed', 'cancelled']

/** 任务中心：异步任务队列与进度。 */
export function JobList(): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const { isSuperAdmin } = usePermission()
  const queryClient = useQueryClient()
  const [state, setState] = useState<JobState | undefined>(undefined)

  const jobsQuery = useQuery({
    queryKey: ['jobs', state ?? 'all'],
    queryFn: () => api.listJobs({ state, limit: 200 }),
    refetchInterval: 10000
  })

  // 取消任务：卡住的发布/拆除任务没有别的出口（真实诉求："任务一直卡着，给个取消按钮"）。
  const cancelMutation = useMutation({
    mutationFn: (id: string) => api.cancelJob(id),
    onSuccess: () => {
      message.success(t('job.cancelDone'))
      void queryClient.invalidateQueries({ queryKey: ['jobs'] })
    },
    onError: (err) => message.error(translateError(err) || t('job.cancelFailed'))
  })

  const columns = useMemo<TableColumnsType<JobDTO>>(
    () => [
      { title: t('field.type'), dataIndex: 'type', key: 'type', width: 200, render: (value: string) => jobTypeLabel(value) },
      { title: t('field.refId'), dataIndex: 'ref_id', key: 'ref_id', render: (value: string) => <span style={{ fontSize: 12 }}>{value}</span> },
      {
        title: t('common.status'),
        dataIndex: 'state',
        key: 'state',
        width: 120,
        render: (value: string) => <StatusTag group="job" value={value} />
      },
      {
        title: t('field.progress'),
        dataIndex: 'progress',
        key: 'progress',
        width: 180,
        render: (value: number, job) => (
          <Progress percent={value} size="small" status={job.failed ? 'exception' : job.state === 'succeeded' ? 'success' : 'active'} />
        )
      },
      { title: t('job.field.attempt'), dataIndex: 'attempt', key: 'attempt', width: 100 },
      { title: t('common.createdAt'), dataIndex: 'created_at', key: 'created_at', width: 170, render: (value: number) => formatTime(value) },
      { title: t('common.updatedAt'), dataIndex: 'finished_at', key: 'finished_at', width: 170, render: (value: number) => formatTime(value) },
      {
        // 失败原因（稳定错误码）直接列出来：否则用户只能看到"失败"却不知道卡在哪，
        // 又没法翻服务端日志（真实反馈："看不到任何日志"）。
        title: t('job.field.error'),
        dataIndex: 'last_error',
        key: 'last_error',
        width: 240,
        render: (value: string | undefined) =>
          value ? (
            <Tooltip title={value}>
              <Typography.Text type="danger" style={{ fontSize: 12 }} ellipsis>
                {value}
              </Typography.Text>
            </Tooltip>
          ) : null
      },
      {
        title: t('common.actions'),
        key: 'actions',
        width: 110,
        render: (_, job) =>
          // 只有超管能取消（服务端也仅超管放行）；终态任务无需取消。
          isSuperAdmin && !isJobTerminal(job.state) ? (
            <Popconfirm
              title={t('job.cancelConfirm')}
              okText={t('common.confirm')}
              cancelText={t('common.cancel')}
              onConfirm={() => cancelMutation.mutate(job.id)}
            >
              <Button size="small" danger loading={cancelMutation.isPending}>
                {t('job.cancel')}
              </Button>
            </Popconfirm>
          ) : null
      }
    ],
    [t, isSuperAdmin, cancelMutation]
  )

  return (
    <PageShell
      title={t('page.jobs.title')}
      extra={
        <>
          <Select<JobState>
            allowClear
            placeholder={t('job.filter.state')}
            style={{ width: 180 }}
            value={state}
            onChange={setState}
            options={JOB_STATES.map((value) => ({ value, label: jobStateLabel(value) }))}
          />
          <Button onClick={() => void jobsQuery.refetch()}>{t('common.refresh')}</Button>
        </>
      }
    >
      {jobsQuery.error ? <ErrorNotice error={jobsQuery.error} /> : null}
      <DataTable<JobDTO>
        columns={columns}
        rows={jobsQuery.data?.items ?? []}
        rowKey={(job) => job.id}
        loading={jobsQuery.isLoading}
        empty={<EmptyState title={t('job.empty')} />}
        scroll={{ x: 1340 }}
      />
    </PageShell>
  )
}
