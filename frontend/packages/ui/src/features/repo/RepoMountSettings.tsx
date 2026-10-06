import { useEffect, useState } from 'react'
import { Alert, Button, Input, Modal, Radio, Space, Switch, Typography, message } from 'antd'

import { ErrorNotice } from '../../components/ErrorNotice'
import { SectionCard } from '../../components/SectionCard'
import { agentApi } from '../../api/agentClient'
import type { AgentMountMode } from '../../api/agentTypes'
import type { RepoDTO } from '../../api/types'
import { useAgent } from '../../hooks/useAgent'
import { useI18n } from '../../i18n'
import { fontSize, spacing } from '../../tokens/palette'

/** 单个存储库挂载配置的表单状态与保存动作（卡片弹窗与详情页 tab 共用一份）。 */
export interface RepoMountSettingsState {
  mode: AgentMountMode
  dir: string
  autoMount: boolean
  setMode: (mode: AgentMountMode) => void
  setDir: (dir: string) => void
  setAutoMount: (value: boolean) => void
  saving: boolean
  error: unknown
  /** 本地代理不可用：控件置灰（配置存在本机，没有代理就落不下去）。 */
  disabled: boolean
  /** 保存成功返回 true（弹窗据此决定关不关）。 */
  save: () => Promise<boolean>
}

/**
 * 本库的挂载配置：形态 / 目录 / 启动后自动挂载，**每个库各自独立**。
 *
 * 取值优先级：本库存过的配置 → 「本地服务设置」里的默认值（后者只用来**预填**）。
 * 预填不等于隐式继承：用户点一次保存，这个库就有了自己的显式表态，此后不再随全局默认变化。
 * （真实诉求："参考配置里的本地服务设置中的默认配置，默认填写，然后等用户去确认就行了"。）
 */
export function useRepoMountSettings(repoId: string): RepoMountSettingsState {
  const agent = useAgent()
  const { t } = useI18n()
  const config = agent.config
  const saved = config?.repo_mounts?.[repoId]

  // 生效值：本库配置优先，空串/无条目则落到全局默认（与代理侧的归一规则一致）。
  const effectiveMode: AgentMountMode = saved?.mount_mode || config?.default_mount_mode || 'letter'
  const effectiveDir = saved?.mount_dir || config?.default_mount_dir || ''
  const effectiveAuto = saved ? saved.auto_mount : Boolean(config?.auto_mount)

  const [mode, setMode] = useState<AgentMountMode>(effectiveMode)
  const [dir, setDir] = useState(effectiveDir)
  const [autoMount, setAutoMount] = useState(effectiveAuto)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<unknown>(null)

  // 代理配置是异步到齐的（也可能刚保存完被 refresh 覆盖）：生效值一变就重填表单，
  // 否则用户会看到一份"还没加载出配置"的空表单，并把空值当默认值存下去。
  useEffect(() => {
    setMode(effectiveMode)
    setDir(effectiveDir)
    setAutoMount(effectiveAuto)
  }, [repoId, effectiveMode, effectiveDir, effectiveAuto])

  const save = async (): Promise<boolean> => {
    setError(null)
    setSaving(true)
    try {
      await agentApi.setRepoMountPref(repoId, {
        mount_mode: mode,
        // 目录一直存着（此后切回目录模式不用重填）；空串 = 跟随本地服务设置里的默认目录。
        mount_dir: dir.trim(),
        auto_mount: autoMount
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

  return {
    mode,
    dir,
    autoMount,
    setMode,
    setDir,
    setAutoMount,
    saving,
    error,
    disabled: !agent.available,
    save
  }
}

/** 三个字段的展示：弹窗与详情页 tab 用同一份，避免两处配置长得不一样。 */
function RepoMountFields({ state }: { state: RepoMountSettingsState }): JSX.Element {
  const { t } = useI18n()
  const { mode, dir, autoMount, disabled } = state

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
  const state = useRepoMountSettings(repo.id)

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
  const state = useRepoMountSettings(repo.id)

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
