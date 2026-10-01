import { useMemo, useState } from 'react'
import { Button, Progress, Select } from 'antd'
import type { TableColumnsType } from 'antd'
import { useQuery } from '@tanstack/react-query'

import { DataTable } from '../../components/DataTable'
import { EmptyState } from '../../components/EmptyState'
import { ErrorNotice } from '../../components/ErrorNotice'
import { PageShell } from '../../components/PageShell'
import { StatusTag } from '../../components/StatusTag'
import { useApi } from '../../api/provider'
import type { JobDTO, JobState } from '../../api/types'
import { useI18n } from '../../i18n'
import { formatTime } from '../../utils/format'
import { jobStateLabel, jobTypeLabel } from '../../utils/labels'

const JOB_STATES: JobState[] = ['pending', 'running', 'succeeded', 'failed', 'cancelled']

/** 任务中心：异步任务队列与进度。 */
export function JobList(): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const [state, setState] = useState<JobState | undefined>(undefined)

  const jobsQuery = useQuery({
    queryKey: ['jobs', state ?? 'all'],
    queryFn: () => api.listJobs({ state, limit: 200 }),
    refetchInterval: 10000
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
      { title: t('common.updatedAt'), dataIndex: 'finished_at', key: 'finished_at', width: 170, render: (value: number) => formatTime(value) }
    ],
    [t]
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
        scroll={{ x: 1100 }}
      />
    </PageShell>
  )
}
