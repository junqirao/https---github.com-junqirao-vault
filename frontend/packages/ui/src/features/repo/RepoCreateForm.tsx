import { useEffect, useMemo, useRef, useState } from 'react'
import { Button, Col, Form, Input, InputNumber, Progress, Row, Select, Space, Typography } from 'antd'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { ErrorNotice } from '../../components/ErrorNotice'
import { PageShell } from '../../components/PageShell'
import { SectionCard } from '../../components/SectionCard'
import { StorageOption } from '../../components/StorageOption'
import { agentApi } from '../../api/agentClient'
import type { StartUploadInput, UploadState, UploadStatus } from '../../api/agentTypes'
import { ApiError } from '../../api/errors'
import { useApi } from '../../api/provider'
import type { CreateRepoRequest, RepoDTO, RepoMode, StorageDTO } from '../../api/types'
import { useAgent } from '../../hooks/useAgent'
import { useServerConfig } from '../../hooks/useServerConfig'
import { useI18n } from '../../i18n'
import { CAPACITY_UNIT, capacityNormalize, capacityValueProps, formatBytes } from '../../utils/format'
import { fontSize, palette, spacing } from '../../tokens/palette'
import { RepoGroupSelect } from './RepoGroupSelect'
import { SourceDirPicker, type SourceDirValue } from './SourceDirPicker'
import { UploadCurrentFile, UploadMeta, UploadNotice, uploadPercent, uploadStageText } from './uploadView'

export interface RepoCreateFormProps {
  isSuperAdmin: boolean
  onCreated: (repo: RepoDTO) => void
  onCancel: () => void
  /** 本地目录上传完成后打开存储库（缺省时只提示成功并刷新列表）。 */
  onOpenRepo?: (repoId: string) => void
}

/** 表单值：源目录由 SourceDirPicker 提供结构化取值。 */
type RepoFormValues = Omit<CreateRepoRequest, 'source_dir'> & { source_dir?: SourceDirValue }

/** 上传进行中的状态（可取消）。 */
const ACTIVE_UPLOAD_STATES: readonly UploadStatus[] = ['scanning', 'creating', 'uploading', 'completing']

/**
 * 存储下拉里进度条的口径：有分配容量（thin 卷）时按"已用 / 分配容量"，
 * 与存储列表的用量列同一口径（库能写到的上限是分配容量，不是底层卷的总量）；
 * 目录模式没有分配容量，退化为"卷已用 / 卷容量"（总量 - 可用）。
 */
function storageUsage(storage: StorageDTO): { usedBytes: number; totalBytes: number } {
  if (storage.size_bytes > 0) {
    return { usedBytes: storage.used_bytes, totalBytes: storage.size_bytes }
  }
  const total = storage.total_bytes
  return { usedBytes: Math.max(0, total - storage.free_bytes), totalBytes: total }
}

/** 创建存储库表单页（超级管理员可指定所有者）。 */
export function RepoCreateForm({
  isSuperAdmin,
  onCreated,
  onCancel,
  onOpenRepo
}: RepoCreateFormProps): JSX.Element {
  const api = useApi()
  const { t } = useI18n()
  // 新建的库留在**当前活动**服务端上：本地目录上传必须带上归属台。
  const { active } = useServerConfig()
  const queryClient = useQueryClient()
  const [form] = Form.useForm<RepoFormValues>()
  // 独享库没有差异盘（整块盘直接给用户），服务端会把共享数量静默归零：
  // 与其让用户填一个不起作用的数字，不如当场置灰并说明原因。
  //
  // 用 onValuesChange 而不是 Form.useWatch：上传进行中表单会被进度面板替换掉，
  // 此时 useWatch 会去读一个"没有挂在任何 Form 上"的实例，开发模式会刷告警。
  const [exclusive, setExclusive] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const [sourceDirValid, setSourceDirValid] = useState(true)
  const [upload, setUpload] = useState<UploadState | null>(null)
  const { uploads, refreshUploads } = useAgent()
  // 上一次**由源目录自动写入**的库名，用来判断"这个名字还是不是我填的"（见 autoFillName）。
  const autoNameRef = useRef('')

  const usersQuery = useQuery({
    queryKey: ['users', 'options'],
    queryFn: () => api.listUsers({ limit: 200 }),
    enabled: isSuperAdmin
  })

  const storagesQuery = useQuery({
    queryKey: ['storages', 'list'],
    queryFn: () => api.listStorages()
  })

  // 可选存储：只列启用的（与后端 CreateRepo 的校验一致）。
  const storages = useMemo(
    () => (storagesQuery.data?.items ?? []).filter((storage) => storage.enabled),
    [storagesQuery.data]
  )
  // 收起态与搜索只需一个名字；展开后的富选项由 optionRender 画（见下）。
  const storageOptions = useMemo(
    () => storages.map((storage) => ({ value: storage.id, label: storage.name })),
    [storages]
  )
  // 下拉里要画的三排（名称/路径/容量条）取自存储记录本身，而 option 里只放 id，
  // 用这张索引表查回记录：Form 收到的值始终只是一个 id，optionRender 也能拿到完整数据。
  const storageById = useMemo(() => new Map(storages.map((storage) => [storage.id, storage])), [storages])

  const mutation = useMutation({
    mutationFn: (values: CreateRepoRequest) => api.createRepo(values),
    onSuccess: (response) => {
      // 新建库立刻占用用户配额（「我的存储库」顶部的总账），列表页的 ['me'] 必须一起失效，
      // 否则用户建完库回到列表，看到的还是建库前的"已用"。
      void queryClient.invalidateQueries({ queryKey: ['me'] })
      onCreated(response.repo)
    },
    onError: (err) => setError(err)
  })

  const uploadMutation = useMutation({
    mutationFn: (input: StartUploadInput) => agentApi.startUpload(input),
    onSuccess: (response) => {
      setUpload(response.upload)
      void refreshUploads().catch(() => undefined)
    },
    onError: (err) => setError(err)
  })

  const cancelMutation = useMutation({
    mutationFn: (uploadId: string) => agentApi.cancelUpload(uploadId),
    onSuccess: () => {
      void refreshUploads().catch(() => undefined)
    },
    onError: (err) => setError(err)
  })

  // 页面挂载时恢复进行中的上传（用户中途离开又回来）。
  useEffect(() => {
    void refreshUploads().catch(() => undefined)
  }, [refreshUploads])

  // 会话尚未创建时 upload_id 为空（扫描/建会话阶段）：按 local_dir 跟踪同一任务，
  // 避免真实 upload_id 到达后回退到那条空 id 的陈旧占位条目（见 useAgent 的 upsertUpload）。
  const tracked = upload
    ? (upload.upload_id === ''
        ? uploads.find((item) => item.local_dir === upload.local_dir)
        : uploads.find((item) => item.upload_id === upload.upload_id)) ?? upload
    : null

  useEffect(() => {
    if (tracked) return
    const active = uploads.find((item) => ACTIVE_UPLOAD_STATES.includes(item.state))
    if (active) setUpload(active)
  }, [uploads, tracked])

  // 上传完成：刷新存储库列表（repo_id 缺失时这是唯一的成功反馈之一）。
  //
  // ['me'] 也要刷：上传建库是配额真正落账的时刻（库一建出来，那块盘就按标称容量计入已用），
  // 只刷列表会让顶部的"已用 / 总共可用"长期停在建库前的数字。
  useEffect(() => {
    if (tracked?.state !== 'done') return
    void queryClient.invalidateQueries({ queryKey: ['repos'] })
    void queryClient.invalidateQueries({ queryKey: ['me'] })
  }, [tracked?.state, queryClient])

  /**
   * 选了源目录就顺手把库名填上（真实反馈："从文件创建库会自动填写库名为目录名"）。
   *
   * 为什么该自动：拿一个目录建库的人，十有八九就是想把那个目录做成同名库，名字几乎总是
   * 最后一段目录名。还要他先选目录、再回头敲一遍同样的几个字，纯属白费 —— 而且顺序反过来
   * （先起名、后选目录）同样常见，所以两种顺序都得能自动补上。
   *
   * 为什么只在"名字是空的 / 还停在我上次自动填的那个值"时才写：用户手改过的名字是他的，
   * 换个目录就覆盖等于把他刚打的字吃掉。他一旦改过，这个名字就归他，此后换目录不再动它。
   */
  const autoFillName = (changed: Partial<RepoFormValues>): void => {
    if (!('source_dir' in changed)) return
    const picked = repoNameFromPath(changed.source_dir?.path ?? '')
    if (!picked) return
    const current = form.getFieldValue('name') as string | undefined
    if (current && current !== autoNameRef.current) return
    autoNameRef.current = picked
    form.setFieldValue('name', picked)
  }

  const submit = async (): Promise<void> => {
    const values = await form.validateFields()
    setError(null)
    const source = values.source_dir
    if (source?.kind === 'local' && source.path) {
      uploadMutation.mutate({
        local_dir: source.path,
        repo_name: values.name,
        repo_mode: values.mode,
        storage_id: values.storage_id || undefined,
        // 不传 quota_bytes：库没有"配额"，建库只需要**容量**（容量即它占用的配额）。
        //
        // 但共享数量要传：它随上传会话的清单一起存下来，服务端在上传结束建库时写进库配置 ——
        // 不传的话这条路径建出来的库会丢掉用户填的值（服务端建库那条路径是直接建库、参数当场
        // 生效，不存在这个问题）。共享数量曾经就是这么丢的：无论填几，建出来的库都是配置默认值
        // 50（真实反馈："设置共享数量大于0时会变成50"）。
        max_diff_disks: values.max_diff_disks,
        source_mode: 'copy',
        // 存储 ID 只对**它所属的那台**服务端有意义：多服务端下必须说清建到哪台。
        server_key: active?.key,
        server_url: active?.baseUrl
      })
      return
    }
    mutation.mutate(buildCreatePayload(values, source))
  }

  return (
    <PageShell
      title={t('repo.create.title')}
      extra={<Button onClick={onCancel}>{t('common.back')}</Button>}
    >
      <SectionCard>
        {error ? <ErrorNotice error={error} /> : null}
        {tracked ? (
          <UploadPanel
            upload={tracked}
            canceling={cancelMutation.isPending}
            onCancel={() => cancelMutation.mutate(tracked.upload_id)}
            onOpenRepo={
              onOpenRepo && tracked.repo_id ? () => onOpenRepo(tracked.repo_id as string) : undefined
            }
          />
        ) : (
          <Form
            form={form}
            layout="vertical"
            initialValues={{ mode: 'shared', max_diff_disks: 1 }}
            onValuesChange={(changed, values) => {
              setExclusive(values.mode === 'exclusive')
              autoFillName(changed)
            }}
            style={{ maxWidth: 880 }}
          >
            <Row gutter={16}>
              <Col span={12}>
                <Form.Item name="name" label={t('field.name')} rules={[{ required: true }]}>
                  <Input maxLength={64} />
                </Form.Item>
              </Col>
              <Col span={12}>
                <Form.Item name="mode" label={t('field.mode')} rules={[{ required: true }]}>
                  <Select<RepoMode>
                    options={[
                      { value: 'shared', label: t('repo.mode.shared') },
                      { value: 'exclusive', label: t('repo.mode.exclusive') }
                    ]}
                  />
                </Form.Item>
              </Col>
              {isSuperAdmin ? (
                <Col span={12}>
                  <Form.Item name="owner_id" label={t('field.owner')}>
                    <Select
                      allowClear
                      loading={usersQuery.isLoading}
                      options={(usersQuery.data?.items ?? []).map((user) => ({
                        value: user.id,
                        label: `${user.username} (${user.role})`
                      }))}
                    />
                  </Form.Item>
                </Col>
              ) : null}
              <Col span={12}>
                <Form.Item name="group" label={t('field.group')}>
                  <RepoGroupSelect />
                </Form.Item>
              </Col>
              <Col span={12}>
                <Form.Item
                  name="storage_id"
                  label={t('field.storage')}
                  extra={storagesQuery.error ? t('storage.loadFailed') : undefined}
                >
                  <Select
                    allowClear
                    loading={storagesQuery.isLoading}
                    placeholder={t('storage.auto')}
                    options={storageOptions}
                    // 展开项：名称 / 路径 / 容量进度条三排（容量条上的百分比与配色见 StorageOption）。
                    // 数据按 id 现查，option 仍是轻量的 {value,label}。
                    optionRender={(option) => {
                      const storage = storageById.get(String(option.value))
                      if (!storage) return option.label
                      const { usedBytes, totalBytes } = storageUsage(storage)
                      return (
                        <StorageOption
                          name={storage.name}
                          path={storage.path}
                          usedBytes={usedBytes}
                          totalBytes={totalBytes}
                        />
                      )
                    }}
                    // 收起态保持一行，但补回"可用"——富选项只在展开时才看得到。
                    labelRender={({ value }) => {
                      const storage = storageById.get(String(value))
                      // 选中的存储已被停用/删除时列表里没有它：退回原始 id，
                      // 让"选了个已经不存在的存储"看得出来，而不是显示成空框。
                      if (!storage) return String(value)
                      return (
                        <span>
                          {storage.name}
                          <span
                            style={{
                              marginInlineStart: spacing.xs,
                              color: palette.textTertiary,
                              fontSize: fontSize.xs
                            }}
                          >
                            {`${t('storage.available')} ${formatBytes(storage.free_bytes)}`}
                          </span>
                        </span>
                      )
                    }}
                  />
                </Form.Item>
              </Col>
              <Col span={12}>
                <Form.Item
                  name="size_bytes"
                  label={t('field.size')}
                  // 不需要这个提示了
                  // extra={t('repo.create.capacityHint')}
                  getValueProps={capacityValueProps}
                  normalize={capacityNormalize}
                >
                  <InputNumber min={0} addonAfter={CAPACITY_UNIT} style={{ width: '100%' }} />
                </Form.Item>
              </Col>
              <Col span={12}>
                {/*
                  这里**不问配额**：库没有"配额"这个概念 —— 这里填的是**容量**（VHDX 标称容量），
                  而这个容量就是它占用的配额（从创建者的配额里扣除）。配额本身是**用户级**的策略值，
                  由管理员在用户表单里设置；库这边只需要给出容量。
                */}
                {/*
                  「共享数量」就是服务端的 max_diff_disks：它不只是"上限"，而是**建库时预创建几个池位**
                  （几块差异盘 + 几个 iSCSI 目标）。名字必须以"预创建"为准 —— 叫"差异盘上限"时，
                  连开发者都会以为填了只在超限时报错，于是填 0，结果每次挂载都要等服务端现场下发。
                */}
                <Form.Item
                  name="max_diff_disks"
                  label={t('field.shareCount')}
                  extra={exclusive ? t('repo.create.shareCountExclusive') : t('repo.create.shareCountHint')}
                >
                  <InputNumber min={0} disabled={exclusive} style={{ width: '100%' }} />
                </Form.Item>
              </Col>
              <Col span={12}>
                <Form.Item name="source_dir" label={t('field.sourceDirOptional')}>
                  <SourceDirPicker onValidityChange={setSourceDirValid} />
                </Form.Item>
              </Col>
            </Row>
            <Space style={{ marginTop: spacing.xs }}>
              <Button
                type="primary"
                loading={mutation.isPending || uploadMutation.isPending}
                disabled={!sourceDirValid}
                onClick={() => void submit()}
              >
                {t('common.create')}
              </Button>
              <Button onClick={onCancel}>{t('common.cancel')}</Button>
            </Space>
          </Form>
        )}
      </SectionCard>
    </PageShell>
  )
}

/** 库名长度上限：与名称输入框的 maxLength 一致（服务端只要求非空，长度在这里把关）。 */
const REPO_NAME_MAX_LEN = 64

/**
 * 从源目录路径取库名：最后一段目录名，例如 `D:\data\photos` 与 `/srv/data/photos` 都得 `photos`。
 *
 * 同时兼容 Windows 反斜杠与 POSIX 斜杠（服务端目录与本地目录都可能出现在这一个输入里），
 * 并忽略尾随分隔符 —— 路径常是从地址栏或资源管理器里抄来的，尾巴上带不带 `\` 不由我们决定。
 * 取不出名字时返回空串（根路径 `D:\`、`/`、以及 `.` / `..` 这种没有名字的段），调用方据此不填。
 */
function repoNameFromPath(path: string): string {
  const parts = path.split(/[\\/]/).filter((part) => part !== '')
  const last = (parts[parts.length - 1] ?? '').trim()
  if (last === '.' || last === '..') return ''
  return last.length > REPO_NAME_MAX_LEN ? last.slice(0, REPO_NAME_MAX_LEN) : last
}

/** 组装服务端建库请求：仅服务端目录作为 source_dir 传给 /v1/repos。 */
function buildCreatePayload(values: RepoFormValues, source: SourceDirValue | undefined): CreateRepoRequest {
  const payload: CreateRepoRequest = {
    name: values.name,
    mode: values.mode,
    owner_id: values.owner_id,
    storage_id: values.storage_id,
    size_bytes: values.size_bytes,
    max_diff_disks: values.max_diff_disks,
    group: values.group
  }
  if (source?.kind === 'server' && source.path) payload.source_dir = source.path
  return payload
}

/** 本地目录上传进度面板（替代表单区域，不跳转页面）。 */
function UploadPanel({
  upload,
  canceling,
  onCancel,
  onOpenRepo
}: {
  upload: UploadState
  canceling: boolean
  onCancel: () => void
  onOpenRepo?: () => void
}): JSX.Element {
  const { t } = useI18n()
  const active = ACTIVE_UPLOAD_STATES.includes(upload.state)

  return (
    <Space direction="vertical" size={spacing.sm} style={{ width: '100%' }}>
      <Typography.Title level={5} style={{ margin: 0 }}>
        {t('upload.title')}
      </Typography.Title>
      <Typography.Text strong>{upload.repo_name}</Typography.Text>
      <Typography.Text type="secondary" style={{ wordBreak: 'break-all' }}>
        {upload.local_dir}
      </Typography.Text>
      <Progress
        percent={uploadPercent(upload)}
        status={upload.state === 'failed' ? 'exception' : active ? 'active' : 'normal'}
      />
      <Space size={spacing.sm} wrap>
        <Typography.Text type="secondary">{uploadStageText(t, upload.state)}</Typography.Text>
        {active ? (
          <Button type="link" size="small" danger loading={canceling} onClick={onCancel}>
            {t('upload.cancel')}
          </Button>
        ) : null}
      </Space>
      {/* 与「上传任务」页共用同一份元信息（已传/总量 · 速度 · 预计剩余 · 文件 · 分块）：
          新建页是"刚发起的任务"、任务页是"回头查看同一件事"，两处口径必须一致。 */}
      <UploadMeta upload={upload} />
      <UploadCurrentFile upload={upload} style={{ display: 'block' }} />
      <UploadNotice upload={upload} style={{ display: 'block' }} />
      {upload.state === 'done' ? (
        <Space size={spacing.sm} wrap>
          <Typography.Text type="success">{t('upload.done')}</Typography.Text>
          {onOpenRepo ? (
            <Button type="link" size="small" onClick={onOpenRepo}>
              {t('upload.openRepo')}
            </Button>
          ) : null}
        </Space>
      ) : null}
      {upload.state === 'canceled' ? (
        <Typography.Text type="secondary">{t('disk.copy.canceled')}</Typography.Text>
      ) : null}
      {upload.state === 'failed' ? (
        <ErrorNotice error={new ApiError({ kind: 'business', code: upload.error || 'system.internal' })} />
      ) : null}
    </Space>
  )
}
