import { useState } from 'react'
import { Alert, Button, Descriptions, Dropdown, Form, Input, InputNumber, Modal, Space, Tabs } from 'antd'
import type { MenuProps } from 'antd'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { ConfirmDialog } from '../../components/ConfirmDialog'
import { ErrorNotice } from '../../components/ErrorNotice'
import { LoadingState } from '../../components/LoadingState'
import { PageShell } from '../../components/PageShell'
import { SectionCard } from '../../components/SectionCard'
import { StatusTag } from '../../components/StatusTag'
import { useApi } from '../../api/provider'
import type { ParentAction, RepoDTO, UpdateRepoRequest } from '../../api/types'
import { useI18n } from '../../i18n'
import { formatBytes, formatTime } from '../../utils/format'
import { parentActionLabel, parentConditionLabel, repoModeLabel } from '../../utils/labels'
import { RepoDisks } from './RepoDisks'
import { RepoMembers } from './RepoMembers'

export interface RepoDetailProps {
  repoId: string
  isSuperAdmin: boolean
  currentUserId: string
  /** 当前用户的显示名：非管理员无法拉取用户列表，用于自己的分配列回退显示。 */
  currentUserName: string
  onBack: () => void
}

interface EditValues {
  name: string
  group?: string
  quota_bytes?: number
  max_diff_disks?: number
}

const PARENT_ACTIONS: ParentAction[] = [
  'derive',
  'temp_share',
  'unshare',
  'maintenance',
  'finish_maintenance',
  'cleanup_diffs'
]

/** 存储库详情：概览 + 成员 + 磁盘（含分配与挂载），以及编辑/删除/母盘操作/复制。 */
export function RepoDetail({ repoId, isSuperAdmin, currentUserId, currentUserName, onBack }: RepoDetailProps): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const [error, setError] = useState<unknown>(null)
  const [editOpen, setEditOpen] = useState(false)
  const [copyOpen, setCopyOpen] = useState(false)
  const [deleteOpen, setDeleteOpen] = useState(false)
  const [editForm] = Form.useForm<EditValues>()
  const [copyForm] = Form.useForm<{ new_repo_name: string }>()

  const repoQuery = useQuery({
    queryKey: ['repo', repoId],
    queryFn: () => api.getRepo(repoId)
  })

  const refresh = (): void => {
    void queryClient.invalidateQueries({ queryKey: ['repo', repoId] })
    void queryClient.invalidateQueries({ queryKey: ['repos'] })
  }

  const updateMutation = useMutation({
    mutationFn: (values: UpdateRepoRequest) => api.updateRepo(repoId, values),
    onSuccess: () => {
      setEditOpen(false)
      refresh()
    },
    onError: (err) => setError(err)
  })

  const deleteMutation = useMutation({
    mutationFn: () => api.deleteRepo(repoId),
    onSuccess: () => {
      setDeleteOpen(false)
      // 删除是"先删记录、后台回收资源"：记录此刻已经不存在，只刷新列表。
      // 这里**不能**再 refresh() 详情 —— 会立刻 refetch 一个已删除的库并闪出 404 错误。
      queryClient.removeQueries({ queryKey: ['repo', repoId] })
      void queryClient.invalidateQueries({ queryKey: ['repos'] })
      onBack()
    },
    onError: (err) => setError(err)
  })

  const parentMutation = useMutation({
    mutationFn: (action: ParentAction) => api.parentAction(repoId, action),
    onSuccess: refresh,
    onError: (err) => setError(err)
  })

  const copyMutation = useMutation({
    mutationFn: (newName: string) => api.copyRepo(repoId, newName),
    onSuccess: () => {
      setCopyOpen(false)
      void queryClient.invalidateQueries({ queryKey: ['jobs'] })
    },
    onError: (err) => setError(err)
  })

  if (repoQuery.isLoading) return <LoadingState text={t('common.loading')} />

  const repo = repoQuery.data

  const openEdit = (target: RepoDTO): void => {
    editForm.setFieldsValue({
      name: target.name,
      group: target.group,
      max_diff_disks: target.max_diff_disks
    })
    setError(null)
    setEditOpen(true)
  }

  const parentMenu: MenuProps = {
    items: PARENT_ACTIONS.map((action) => ({ key: action, label: parentActionLabel(action) })),
    onClick: ({ key }) => {
      setError(null)
      parentMutation.mutate(key as ParentAction)
    }
  }

  return (
    <PageShell
      title={repo?.name ?? t('repo.detail.title')}
      extra={
        <>
          <Button onClick={onBack}>{t('common.back')}</Button>
          <Button onClick={() => repo && openEdit(repo)} disabled={!repo}>
            {t('common.edit')}
          </Button>
          <Dropdown menu={parentMenu}>
            <Button loading={parentMutation.isPending}>{t('repo.parent.title')}</Button>
          </Dropdown>
          <Button
            onClick={() => {
              copyForm.setFieldsValue({ new_repo_name: `${repo?.name ?? 'repo'}-copy` })
              setCopyOpen(true)
            }}
          >
            {t('action.copy')}
          </Button>
          <Button danger onClick={() => setDeleteOpen(true)}>
            {t('common.delete')}
          </Button>
        </>
      }
    >
      {error ? <ErrorNotice error={error} /> : null}
      {repoQuery.error ? <ErrorNotice error={repoQuery.error} /> : null}

      <Tabs
        items={[
          {
            key: 'overview',
            label: t('repo.detail.overview'),
            children: (
              <SectionCard title={t('repo.detail.overview')}>
                <Descriptions size="small" column={2} bordered>
                  <Descriptions.Item label={t('common.id')}>{repo?.id ?? '-'}</Descriptions.Item>
                  <Descriptions.Item label={t('field.mode')}>{repo ? repoModeLabel(repo.mode) : '-'}</Descriptions.Item>
                  <Descriptions.Item label={t('field.group')}>{repo?.group || '-'}</Descriptions.Item>
                  <Descriptions.Item label={t('common.status')}>
                    {repo ? <StatusTag group="repo" value={repo.state} /> : '-'}
                  </Descriptions.Item>
                  <Descriptions.Item label={t('repo.parentCondition')}>
                    {repo ? parentConditionLabel(repo.parent_condition) : '-'}
                  </Descriptions.Item>
                  <Descriptions.Item label={t('repo.parentVersion')}>{repo?.parent_version ?? '-'}</Descriptions.Item>
                  {/* 库**没有配额**：库只有"容量"（建库时设定），这个容量就是它占用的配额
                      （计入用户已用配额）。所以这里只展示占用了多少，不再提供配额输入。 */}
                  <Descriptions.Item label={t('repo.used')}>{formatBytes(repo?.used_bytes)}</Descriptions.Item>
                  <Descriptions.Item label={t('field.maxDiffDisks')}>{repo?.max_diff_disks ?? '-'}</Descriptions.Item>
                  <Descriptions.Item label={t('common.createdAt')}>{formatTime(repo?.created_at)}</Descriptions.Item>
                </Descriptions>
              </SectionCard>
            )
          },
          {
            key: 'members',
            label: t('repo.detail.members'),
            children: <RepoMembers repoId={repoId} isSuperAdmin={isSuperAdmin} />
          },
          {
            key: 'disks',
            label: t('repo.detail.disks'),
            children: (
              <RepoDisks
                repoId={repoId}
                isSuperAdmin={isSuperAdmin}
                currentUserId={currentUserId}
                currentUserName={currentUserName}
              />
            )
          }
        ]}
      />

      <Modal
        open={editOpen}
        title={t('repo.edit.title')}
        width={520}
        centered
        okText={t('common.save')}
        cancelText={t('common.cancel')}
        confirmLoading={updateMutation.isPending}
        onCancel={() => setEditOpen(false)}
        onOk={() => {
          void editForm.validateFields().then((values) => {
            updateMutation.mutate(values)
          })
        }}
      >
        <Form form={editForm} layout="vertical">
          <Form.Item name="name" label={t('field.name')} rules={[{ required: true }]}>
            <Input maxLength={64} />
          </Form.Item>
          <Form.Item name="group" label={t('field.group')}>
            <Input maxLength={64} />
          </Form.Item>
          <Form.Item name="max_diff_disks" label={t('field.maxDiffDisks')}>
            <InputNumber min={0} style={{ width: '100%' }} />
          </Form.Item>
        </Form>
      </Modal>

      <Modal
        open={copyOpen}
        title={t('repo.copy.title')}
        width={520}
        centered
        okText={t('common.confirm')}
        cancelText={t('common.cancel')}
        confirmLoading={copyMutation.isPending}
        onCancel={() => setCopyOpen(false)}
        onOk={() => {
          void copyForm.validateFields().then((values) => copyMutation.mutate(values.new_repo_name))
        }}
      >
        <Space direction="vertical" style={{ width: '100%' }}>
          <Alert type="info" showIcon message={t('repo.copy.desc')} />
          <Form form={copyForm} layout="vertical">
            <Form.Item name="new_repo_name" label={t('field.newName')} rules={[{ required: true }]}>
              <Input maxLength={64} />
            </Form.Item>
          </Form>
        </Space>
      </Modal>

      <ConfirmDialog
        open={deleteOpen}
        danger
        title={t('common.delete')}
        content={t('repo.delete.confirm', { name: repo?.name ?? '' })}
        loading={deleteMutation.isPending}
        onConfirm={() => deleteMutation.mutate()}
        onCancel={() => setDeleteOpen(false)}
      />
    </PageShell>
  )
}
