import { useEffect, useState } from 'react'
import { Alert, Button, Input, Modal, Popconfirm, Radio, Space, Switch, Typography, message } from 'antd'

import { ErrorNotice } from '../../components/ErrorNotice'
import { SectionCard } from '../../components/SectionCard'
import { agentApi } from '../../api/agentClient'
import type { AgentMountMode } from '../../api/agentTypes'
import type { RepoDTO } from '../../api/types'
import { useAgent } from '../../hooks/useAgent'
import { useServerConfig } from '../../hooks/useServerConfig'
import { useI18n } from '../../i18n'
import { fontSize, spacing } from '../../tokens/palette'
import { formatBytes } from '../../utils/format'
import { effectiveMountDir } from '../../utils/mountPath'

/** 单个存储库挂载配置的表单状态与保存动作（卡片弹窗与详情页 tab 共用一份）。 */
export interface RepoMountSettingsState {
  mode: AgentMountMode
  dir: string
  autoMount: boolean
  /** 是否为该库启用本地 iSCSI 读缓存代理（默认关闭，需用户手动开启）。 */
  cacheEnabled: boolean
  /** 是否为该库启用直连模式（跳过本机代理，默认关闭）。 */
  directMode: boolean
  setMode: (mode: AgentMountMode) => void
  setDir: (dir: string) => void
  setAutoMount: (value: boolean) => void
  setCacheEnabled: (value: boolean) => void
  setDirectMode: (value: boolean) => void
  saving: boolean
  error: unknown
  /** 本地代理不可用：控件置灰（配置存在本机，没有代理就落不下去）。 */
  disabled: boolean
  /** 保存成功返回 true（弹窗据此决定关不关）。 */
  save: () => Promise<boolean>
  /** 目录模式的最终挂载点预览：父目录 + 一层 `<服务端名称>_<存储库名称>`（留空则用默认根）。 */
  previewDir: string
  /**
   * 该库此刻的缓存占用（L1 + L2，字节）；`null` 表示本库在门户上**没有缓存目标**
   * （没挂载 / 走了直连 / 未启用缓存），此时不显示"清空缓存"入口。
   */
  cacheUsedBytes: number | null
  /** 该库分到的缓存上限（L1 + L2，字节）；只有存在缓存目标时才有意义。 */
  cacheLimitBytes: number
  /** 清空缓存请求进行中。 */
  clearing: boolean
  /** 清空该库的缓存内容（两级都清，不断会话）；结果按"清到了没有"如实提示。 */
  clearCache: () => Promise<void>
}

/**
 * 本库的挂载配置：形态 / 目录 / 启动后自动挂载，**每个库各自独立**。
 *
 * 取值优先级：本库存过的配置 → 代理的默认挂载根（后者只用来**预填**）。
 * 预填不等于隐式继承：用户点一次保存，这个库就有了自己的显式表态。
 *
 * 挂载形态与目录不再有「客户端设置」里的全局开关（那是"给所有库定默认"，用户要的恰恰是
 * 按库配置）；代理仍保留一对默认值作为兜底。
 *
 * 目录模式下，这里填的目录是**父目录**：真正挂载到它下面的
 * `<服务端名称>_<存储库名称>` 子目录（代理侧规则，见 mountDirLeaf）。界面必须把这件事
 * 说清楚 —— 否则用户会去填写的目录里找文件，而文件其实在下一级（真实反馈）。
 */
export function useRepoMountSettings(repoId: string, repoName: string): RepoMountSettingsState {
  const agent = useAgent()
  const { t } = useI18n()
  const { servers, active } = useServerConfig()
  const config = agent.config
  const saved = config?.repo_mounts?.[repoId]

  // 生效值：本库配置优先，空串/无条目则落到代理默认（与代理侧的归一规则一致）。
  const effectiveMode: AgentMountMode = saved?.mount_mode || config?.default_mount_mode || 'letter'
  const effectiveDir = saved?.mount_dir || config?.default_mount_dir || ''
  const effectiveAuto = saved ? saved.auto_mount : Boolean(config?.auto_mount)
  // 缓存**默认关闭**：没有该库条目、或条目里没写，一律按关闭（与代理侧零值一致）。
  const effectiveCache = Boolean(saved?.cache_enabled)
  // 直连模式**默认关闭**：默认所有库都经本机代理（未开缓存也走纯透传，从而可监控）。
  const effectiveDirect = Boolean(saved?.direct_mode)

  // 目录名里的"服务端名称"与代理侧同一口径：本地别名优先，其次服务端名称，最后 vault。
  // 全局 server_alias 只在**本机只有一台**时作数：多台共用会让两台下的 `别名_库名` 撞成同一目录。
  const serverAlias =
    (servers.length <= 1 ? config?.server_alias : '')?.trim() || (active?.serverName ?? '').trim() || 'vault'

  const [mode, setMode] = useState<AgentMountMode>(effectiveMode)
  const [dir, setDir] = useState(effectiveDir)
  const [autoMount, setAutoMount] = useState(effectiveAuto)
  const [cacheEnabled, setCacheEnabled] = useState(effectiveCache)
  const [directMode, setDirectMode] = useState(effectiveDirect)
  const [saving, setSaving] = useState(false)
  const [clearing, setClearing] = useState(false)
  const [error, setError] = useState<unknown>(null)

  // 本库此刻在门户上的缓存目标：targets 按**分配**注册，挂载记录给出本库这次的分配，
  // 两者对上才有目标（GET /agent/cache 的说明：未启用/回退直连的库不出现在 targets 里）。
  // 找不到就代表本库没有缓存内容可清 —— 界面不显示清空入口，而不是摆一个点了没反应的按钮。
  const mountAllocationId = (agent.state?.mounts ?? []).find((item) => item.repo_id === repoId)?.allocation_id
  const cacheTarget = mountAllocationId
    ? agent.cache?.targets.find((item) => item.allocation_id === mountAllocationId)
    : undefined
  const cacheUsedBytes = cacheTarget ? cacheTarget.l1_used_bytes + cacheTarget.l2_used_bytes : null
  const cacheLimitBytes = cacheTarget ? cacheTarget.l1_limit_bytes + cacheTarget.l2_limit_bytes : 0

  // 代理配置是异步到齐的（也可能刚保存完被 refresh 覆盖）：生效值一变就重填表单，
  // 否则用户会看到一份"还没加载出配置"的空表单，并把空值当默认值存下去。
  useEffect(() => {
    setMode(effectiveMode)
    setDir(effectiveDir)
    setAutoMount(effectiveAuto)
    setCacheEnabled(effectiveCache)
    setDirectMode(effectiveDirect)
  }, [repoId, effectiveMode, effectiveDir, effectiveAuto, effectiveCache, effectiveDirect])

  // 打开这份设置时主动取一次缓存状态：用量随读写一直在变，而**详情页这一层没有轮询器**
  // （10s 轮询放在库列表那一层，离开列表就停了），只靠进入本页前的快照可能已经过时，
  // 甚至还没有（直接刷新详情页）。取一次就够，本页不轮询（与「客户端设置」同样的做法）。
  const refreshCache = agent.refreshCache
  useEffect(() => {
    if (!agent.available) return
    void refreshCache()
  }, [agent.available, refreshCache])

  // 预览按用户**正在编辑**的父目录算：改了输入框提示立刻跟着变，不用先保存再猜。
  const previewDir = effectiveMountDir(dir.trim() || config?.default_mount_dir || '', serverAlias, repoName)

  const save = async (): Promise<boolean> => {
    setError(null)
    setSaving(true)
    try {
      await agentApi.setRepoMountPref(repoId, {
        mount_mode: mode,
        // 目录一直存着（此后切回目录模式不用重填）；空串 = 用默认挂载根。
        // 存的是**父目录**，真正挂载点由代理在它下面加一层 <服务端名称>_<存储库名称>。
        mount_dir: dir.trim(),
        auto_mount: autoMount,
        cache_enabled: cacheEnabled,
        direct_mode: directMode
      })
      // 刷新全局配置快照：卡片、详情页读的都是它，否则界面还显示上一次的值。
      await agent.refresh()
      message.success(t('repo.settings.saved'))
      return true
    } catch (err) {
      setError(err)
      return false
    } finally {
      setSaving(false)
    }
  }

  /**
   * 清空本库的缓存内容（L1 + L2 两级），**不**改配置、不断会话：清完门户目标还在，
   * 后续读照常重新填充（见 POST /agent/cache/clear）。
   *
   * 只传 repo_id：目标按分配注册而界面手里只有库 ID，映射由代理按挂载记录自己做。
   */
  const clearCache = async (): Promise<void> => {
    setError(null)
    setClearing(true)
    try {
      const result = await agentApi.clearCache({ repo_id: repoId })
      // 清完用量立刻回落（否则界面还显示清空前的占用，看起来像没生效）。
      await agent.refreshCache()
      // 没清到东西不是失败：本库此刻没有缓存目标（没挂载/走了直连），如实说，不谎报"已清空"。
      if (result.cleared) message.success(t('repo.settings.clearCacheDone'))
      else message.info(t('repo.settings.clearCacheNone'))
    } catch (err) {
      setError(err)
    } finally {
      setClearing(false)
    }
  }

  return {
    mode,
    dir,
    autoMount,
    cacheEnabled,
    directMode,
    setMode,
    setDir,
    setAutoMount,
    setCacheEnabled,
    setDirectMode,
    saving,
    error,
    disabled: !agent.available,
    save,
    previewDir,
    cacheUsedBytes,
    cacheLimitBytes,
    clearing,
    clearCache
  }
}

/** 各字段的展示：弹窗与详情页 tab 用同一份，避免两处配置长得不一样。 */
function RepoMountFields({ state }: { state: RepoMountSettingsState }): JSX.Element {
  const { t } = useI18n()
  const { mode, dir, autoMount, cacheEnabled, directMode, disabled, previewDir } = state

  return (
    <Space direction="vertical" size={spacing.md} style={{ width: '100%' }}>
      {disabled ? (
        <Alert type="warning" showIcon message={t('settings.agent.notAvailable')} />
      ) : (
        <Alert type="info" showIcon message={t('repo.settings.defaultsHint')} />
      )}

      <div>
        <Typography.Text style={{ display: 'block', marginBottom: spacing.xs }}>
          {t('repo.settings.mountMode')}
        </Typography.Text>
        <Radio.Group
          value={mode}
          disabled={disabled}
          onChange={(event) => state.setMode(event.target.value as AgentMountMode)}
        >
          <Radio.Button value="letter">{t('settings.mountMode.letter')}</Radio.Button>
          <Radio.Button value="directory">{t('settings.mountMode.directory')}</Radio.Button>
        </Radio.Group>
      </div>

      {/* 目录只在目录模式下有意义（与挂载对话框一致）：盘符模式由系统分配，没有目录可填 */}
      {mode === 'directory' ? (
        <div>
          <Typography.Text style={{ display: 'block', marginBottom: spacing.xs }}>
            {t('repo.settings.mountDir')}
          </Typography.Text>
          <Input
            value={dir}
            disabled={disabled}
            placeholder={t('agent.mount.dialog.dirHint')}
            onChange={(event) => state.setDir(event.target.value)}
          />
          <Typography.Text type="secondary" style={{ display: 'block', fontSize: fontSize.xs }}>
            {t('repo.settings.dirHint')}
          </Typography.Text>
          {/* 把"实际挂载到哪"直接算给用户看：目录名会自动多一级，光靠文字说明仍会有人填错 */}
          {previewDir ? (
            <Typography.Text type="secondary" style={{ display: 'block', fontSize: fontSize.xs }}>
              {t('repo.settings.dirEffective', { path: previewDir })}
            </Typography.Text>
          ) : null}
        </div>
      ) : null}

      <div>
        <Typography.Text style={{ display: 'block', marginBottom: spacing.xs }}>
          {t('repo.settings.autoMount')}
        </Typography.Text>
        <Switch checked={autoMount} disabled={disabled} onChange={state.setAutoMount} />
        <Typography.Text type="secondary" style={{ display: 'block', fontSize: fontSize.xs }}>
          {t('repo.settings.autoMountHint')}
        </Typography.Text>
      </div>

      <div>
        <Typography.Text style={{ display: 'block', marginBottom: spacing.xs }}>
          {t('repo.settings.cacheEnabled')}
        </Typography.Text>
        {/* 直连模式开着时缓存没有意义（根本不经过代理）：置灰并说明，避免用户白开 */}
        <Switch
          checked={cacheEnabled && !directMode}
          disabled={disabled || directMode}
          onChange={state.setCacheEnabled}
        />
        <Typography.Text type="secondary" style={{ display: 'block', fontSize: fontSize.xs }}>
          {t('repo.settings.cacheHint')}
        </Typography.Text>
        {/*
          清空缓存的入口：只在**本库此刻真有缓存目标**时才出现（见 state.cacheUsedBytes）。
          没有目标（没挂载 / 走了直连 / 未启用缓存）时本机没有任何本库的缓存内容，
          显示一个点了也清不掉东西的按钮只会让人以为清过了。
        */}
        {state.cacheUsedBytes !== null ? (
          <Space size={spacing.sm} style={{ marginTop: spacing.sm }} wrap>
            <Typography.Text type="secondary" style={{ fontSize: fontSize.xs }}>
              {t('repo.settings.cacheUsage', {
                used: formatBytes(state.cacheUsedBytes),
                limit: formatBytes(state.cacheLimitBytes)
              })}
            </Typography.Text>
            <Popconfirm
              title={t('repo.settings.clearCacheConfirm')}
              description={t('repo.settings.clearCacheConfirmHint')}
              okText={t('common.confirm')}
              cancelText={t('common.cancel')}
              onConfirm={() => void state.clearCache()}
            >
              {/* 用文字按钮而不是实心按钮：这行是"看用量 + 顺手清一下"的次要动作，
                  实心按钮会把旁边的缓存开关比下去；全站的动作入口也都是 type="link"。 */}
              <Button type="link" size="small" loading={state.clearing} disabled={disabled}>
                {t('repo.settings.clearCache')}
              </Button>
            </Popconfirm>
          </Space>
        ) : null}
      </div>

      <div>
        <Typography.Text style={{ display: 'block', marginBottom: spacing.xs }}>
          {t('repo.settings.directMode')}
        </Typography.Text>
        <Switch checked={directMode} disabled={disabled} onChange={state.setDirectMode} />
        <Typography.Text type="secondary" style={{ display: 'block', fontSize: fontSize.xs }}>
          {t('repo.settings.directHint')}
        </Typography.Text>
      </div>

      {state.error ? <ErrorNotice error={state.error} /> : null}
    </Space>
  )
}

export interface RepoMountSettingsModalProps {
  repo: RepoDTO
  open: boolean
  onClose: () => void
}

/** 卡片「配置」图标弹出的挂载配置弹窗（只改当前这一个库）。 */
export function RepoMountSettingsModal({ repo, open, onClose }: RepoMountSettingsModalProps): JSX.Element {
  const { t } = useI18n()
  const state = useRepoMountSettings(repo.id, repo.name)

  return (
    <Modal
      open={open}
      title={`${t('repo.settings.title')} · ${repo.name}`}
      width={520}
      centered
      okText={t('common.save')}
      cancelText={t('common.cancel')}
      confirmLoading={state.saving}
      onCancel={onClose}
      onOk={() => {
        // 保存失败不关窗：错误提示就在表单里，用户改完可以直接重试。
        void state.save().then((saved) => {
          if (saved) onClose()
        })
      }}
    >
      <RepoMountFields state={state} />
    </Modal>
  )
}

/**
 * 详情页「设置」tab 的挂载配置：与卡片弹窗同一份数据、同一套控件。
 *
 * 弹窗是"不改库、只改本机怎么挂"的快捷入口，详情页 tab 是同一件事的完整页，
 * 两者不引入第二套语义。
 */
export function RepoMountSettingsPanel({ repo }: { repo: RepoDTO }): JSX.Element {
  const { t } = useI18n()
  const state = useRepoMountSettings(repo.id, repo.name)

  return (
    <SectionCard title={t('repo.settings.title')}>
      <RepoMountFields state={state} />
      <Button
        type="primary"
        loading={state.saving}
        disabled={state.disabled}
        style={{ marginTop: spacing.md }}
        onClick={() => void state.save()}
      >
        {t('common.save')}
      </Button>
    </SectionCard>
  )
}
