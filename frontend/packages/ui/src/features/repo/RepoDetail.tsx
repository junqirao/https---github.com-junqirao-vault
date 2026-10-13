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
import { UsageBar, UsageValueText } from '../../components/UsageBar'
import { useApi } from '../../api/provider'
import type { ParentAction, RepoDTO, UpdateRepoRequest } from '../../api/types'
import { pickMountUsage, repoUsageLoading, useAgent, useAgentUsagePolling } from '../../hooks/useAgent'
import { useI18n } from '../../i18n'
import { spacing } from '../../tokens/palette'
import { formatTime, repoUsage } from '../../utils/format'
import { parentActionLabel, parentConditionLabel, repoModeLabel } from '../../utils/labels'
import { RepoDisks } from './RepoDisks'
import { RepoMembers } from './RepoMembers'
import { RepoMountActions, useRepoMount } from './RepoMount'
import { RepoMountSettingsPanel } from './RepoMountSettings'

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
  const agent = useAgent()
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

  // 概览里的「已用」也要是本机实测值（见下面 repoUsage 的说明）：页面在时按 10s 轮询。
  useAgentUsagePolling(true)

  const refresh = (): void => {
    void queryClient.invalidateQueries({ queryKey: ['repo', repoId] })
    void queryClient.invalidateQueries({ queryKey: ['repos'] })
    // 容量变更（扩容/维护/母盘更新）会改用户级用量，列表页顶部的总账也要跟着变。
    void queryClient.invalidateQueries({ queryKey: ['me'] })
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
      // 删除会释放这块库占用的配额：列表页的顶部总账必须重新拉。
      void queryClient.invalidateQueries({ queryKey: ['me'] })
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
  // 这个库我现在能不能管理：真源是服务端下发的权限等级（manage = 超管 / 库所有者 / 被授予
  // manage 的成员，见 docs/implementation.md 9.3）。老服务端不带这个字段时回退到"超管或库所有者"
  // —— 宁可少展示管理入口，也不能把它摆给只能看的成员：非库所有者点进详情页只能看，
  // 按钮却全都亮着，点下去只会撞 403。
  const canManage = repo
    ? repo.permission
      ? repo.permission === 'manage'
      : isSuperAdmin || repo.owner_id === currentUserId
    : isSuperAdmin
  // 用量口径与卡片一致（分母 = 库容量，见 repoUsage）。本机挂着这个库时用本机**实测**的
  // 卷用量：服务端的 used_bytes 是宿主侧物理占用（写过的最高水位，删了也不回落）。
  const mountUsage = pickMountUsage(agent.usage, repo?.id)
  const {
    percent: usagePercent,
    usedText: usageUsedText,
    totalText: usageTotalText,
    measured: usageMeasured
  } = repoUsage(repo, mountUsage, !isSuperAdmin)
  // 未挂载时客户端不报用量、条置灰（见 RepoCards 的 RepoUsage）：那一刻服务端回退值是
  // "删了也不回落的最高水位"，用量写 `-`；上限照报，仍是"已用 / 上限"那一行文案。
  // 管理端是例外：运维看的正是宿主侧占用，那个绝对数在这里是有效信息（与表格同口径）。
  const usageMuted = !isSuperAdmin && !usageMeasured
  // 刚挂上、实测用量还在取数：把 `-` 与条都换成转圈（见 useAgent 的 repoUsageLoading）。
  // 只在客户端那条路上成立 —— 管理端显示的是服务端口径，本机实测值到位后只是换个口径，
  // 不该拿转圈盖住一个已经有效的数字。
  const usageLoading = !isSuperAdmin && repoUsageLoading(agent.usagePending, repo?.id, usageMeasured)

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
          {/* 与列表卡片同一套"挂载/卸载"联动按钮：进了详情也不必退回列表才能挂载。
              库记录可能还没查回来（加载中/出错），此时不渲染。 */}
          {/* read 成员只能看：挂载/卸载后端都不放行，按钮就不摆出来。 */}
          {repo && repo.permission !== 'read' ? (
            <RepoDetailMountActions repo={repo} currentUserId={currentUserId} />
          ) : null}
          <Button onClick={onBack}>{t('common.back')}</Button>
          {/* 管理类操作只对能管理这个库的人展示（非库所有者点详情只能看）。 */}
          {canManage ? (
            <>
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
          ) : null}
        </>
      }
    >
      {error ? <ErrorNotice error={error} /> : null}
      {repoQuery.error ? <ErrorNotice error={repoQuery.error} /> : null}
      {/* 只能看的库（非所有者 / read、mount 成员）说清楚为什么没有管理入口。 */}
      {repo && !canManage ? (
        <Alert type="info" showIcon message={t('repo.detail.readOnly')} style={{ marginBottom: spacing.md }} />
      ) : null}

      <Tabs
        // 用户端默认落在「设置」：卡片「详情」进来看的就是"这个库在本机怎么挂"。
        // 管理端没有挂载配置（配置存在本机），保持原来的「概览」默认。
        defaultActiveKey={isSuperAdmin ? 'overview' : 'settings'}
        items={[
          ...(isSuperAdmin
            ? []
            : [
                {
                  key: 'settings',
                  label: t('repo.detail.settings'),
                  // 库记录还没到（加载中/出错）时先占位：不能因为详情没回来就少一个默认 tab。
                  children: repo ? <RepoMountSettingsPanel repo={repo} /> : <LoadingState />
                }
              ]),
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
                      （计入用户已用配额）。所以这里只展示占用了多少，不再提供配额输入。
                      口径与卡片、管理端表格一致（分母 = 库容量，见 repoUsage）。 */}
                  <Descriptions.Item label={t('repo.used')}>
                    <div style={{ minWidth: 160 }}>
                      <div>
                        <UsageValueText usedText={usageUsedText} totalText={usageTotalText} loading={usageLoading} />
                      </div>
                      <UsageBar percent={usagePercent} muted={usageMuted} loading={usageLoading} />
                    </div>
                  </Descriptions.Item>
                  <Descriptions.Item label={t('field.maxDiffDisks')}>{repo?.max_diff_disks ?? '-'}</Descriptions.Item>
                  <Descriptions.Item label={t('common.createdAt')}>{formatTime(repo?.created_at)}</Descriptions.Item>
                </Descriptions>
              </SectionCard>
            )
          },
          // 可管理成员是管理信息：只有能管这个库的人才看得到这一页。
          ...(canManage
            ? [
                {
                  key: 'members',
                  label: t('repo.detail.members'),
                  children: <RepoMembers repoId={repoId} isSuperAdmin={isSuperAdmin} />
                }
              ]
            : []),
          {
            key: 'disks',
            label: t('repo.detail.disks'),
            children: (
              <RepoDisks
                repoId={repoId}
                isSuperAdmin={isSuperAdmin}
                canManage={canManage}
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

/**
 * 详情页标题栏的挂载/卸载按钮：直接复用列表卡片的控制器与组件，行为完全一致
 * （无分配时由库主按需自建分配，再交给本机代理挂载）。
 *
 * 单独抽成一个组件，是因为 `useRepoMount` 需要一份完整的库记录，而详情页的库是异步查出来的
 * ——在父组件里调用会踩到 "hooks 必须在提前 return 之前" 的限制。
 *
 * 尺寸取中号：这一排（返回/编辑/母盘操作/复制/删除）都是 antd 默认尺寸，挂载按钮必须同款
 * （真实反馈："存储库详情里的挂载按钮要和其他按钮一样大"），卡片里的小号尺寸是紧凑排布才用的。
 */
function RepoDetailMountActions({ repo, currentUserId }: { repo: RepoDTO; currentUserId: string }): JSX.Element {
  const controller = useRepoMount(repo, currentUserId)
  return <RepoMountActions controller={controller} size="middle" />
}
