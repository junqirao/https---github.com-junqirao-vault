import { useEffect, useState } from 'react'
import { Alert, Button, Descriptions, Input, Progress, Select, Space, Switch, Typography } from 'antd'
import { CheckCircleOutlined } from '@ant-design/icons'

import { ConfirmDialog } from '../../components/ConfirmDialog'
import { CopyableText } from '../../components/CopyableText'
import { ErrorNotice } from '../../components/ErrorNotice'
import { PageShell } from '../../components/PageShell'
import { SectionCard } from '../../components/SectionCard'
import { agentApi, identityMatchesServer, relaunchClient } from '../../api/agentClient'
import type { AgentConfig, AgentIdentity, UpdateInfo } from '../../api/agentTypes'
import { ApiError } from '../../api/errors'
import { useAgent } from '../../hooks/useAgent'
import { useAuth } from '../../hooks/useAuth'
import { useServerConfig } from '../../hooks/useServerConfig'
import { LANGUAGE_LABELS, LANGUAGES, useI18n, type Language } from '../../i18n'
import { spacing } from '../../tokens/palette'
import { formatBytes, formatTime } from '../../utils/format'

export interface ClientSettingsProps {
  language: Language
  onLanguageChange: (language: Language) => void
}

/** 客户端本地设置：真源为本地代理的 /agent/config。 */
export function ClientSettings({ language, onLanguageChange }: ClientSettingsProps): JSX.Element {
  const { t } = useI18n()
  const agent = useAgent()
  const { active } = useServerConfig()
  const { user } = useAuth()
  const [config, setConfig] = useState<AgentConfig | null>(null)
  const [error, setError] = useState<unknown>(null)
  const [saving, setSaving] = useState(false)
  const [notice, setNotice] = useState<string | null>(null)
  const [update, setUpdate] = useState<UpdateInfo | null>(null)
  const [checking, setChecking] = useState(false)
  const [applying, setApplying] = useState(false)
  const [identity, setIdentity] = useState<AgentIdentity | null>(null)
  const [identityError, setIdentityError] = useState<unknown>(null)
  const [identityBusy, setIdentityBusy] = useState(false)
  const [autoLoginSaving, setAutoLoginSaving] = useState(false)
  const [removeOpen, setRemoveOpen] = useState(false)

  const disabled = !agent.available

  // 自更新：进度真源在 agent.state.update（来自 SSE / GET /agent/state，页面刷新后可恢复）。
  const updateState = agent.state?.update
  const updateTotal = updateState?.total_bytes ?? 0
  const updateReceived = updateState?.received_bytes ?? 0
  const progressTotal = updateTotal > 0 ? updateTotal : update?.size_bytes ?? 0
  const progressReceived = updateTotal > 0 ? updateReceived : 0
  const targetVersion = update?.version ?? updateState?.available_version
  const hasUpdate = Boolean(update?.available || updateState?.available_version)
  const updatePercent =
    progressTotal > 0 ? Math.min(100, Math.round((progressReceived / progressTotal) * 100)) : undefined
  // 服务端不提供更新时的可诊断信息（如 update.no_manifest · manifest_missing）。
  const diagnostics =
    update && !update.available && update.server_code
      ? `${update.server_code}${update.server_reason ? ` · ${update.server_reason}` : ''}`
      : null

  // 客户端资源热更：进度真源在 agent.webUpdate（来自 web_update 事件 / GET /agent/update/web）。
  const webState = agent.webUpdate
  const webTotal = webState?.total_bytes ?? 0
  const webReceived = webState?.received_bytes ?? 0
  const webPercent = webTotal > 0 ? Math.min(100, Math.round((webReceived / webTotal) * 100)) : undefined
  const webBusy =
    webState !== null &&
    (webState.state === 'downloading' || webState.state === 'verifying' || webState.state === 'extracting')
  const webStageKey =
    webState?.state === 'downloading'
      ? 'update.web.downloading'
      : webState?.state === 'verifying'
        ? 'update.web.verifying'
        : webState?.state === 'extracting'
          ? 'update.web.extracting'
          : null
  // 无可用资源产物：上次热更失败且明确"服务端没有可下发的资源包"（网络类失败不算）。
  const webNoArtifact =
    webState !== null &&
    webState.state === 'failed' &&
    webState.error !== 'server_unreachable' &&
    (webState.error === 'no_artifact' || !webState.available_version)

  useEffect(() => {
    if (!agent.available) {
      setConfig(null)
      return
    }
    let cancelled = false
    agentApi
      .getConfig()
      .then((value) => {
        if (!cancelled) setConfig(value)
      })
      .catch((err: unknown) => {
        if (!cancelled) setError(err)
      })
    return () => {
      cancelled = true
    }
  }, [agent.available])

  useEffect(() => {
    if (!agent.available) {
      setIdentity(null)
      return
    }
    let cancelled = false
    agentApi
      .getIdentity()
      .then((value) => {
        if (!cancelled) setIdentity(value)
      })
      .catch((err: unknown) => {
        if (!cancelled) setIdentityError(err)
      })
    return () => {
      cancelled = true
    }
  }, [agent.available])

  const patch = (values: Partial<AgentConfig>): void => {
    setConfig((prev) => (prev ? { ...prev, ...values } : prev))
  }

  const save = async (): Promise<void> => {
    if (!config) return
    // 与服务端 validateAbsDir 对齐：非空且为绝对路径；失败时沿用服务端错误码文案。
    if (!isAbsoluteDir(config.default_download_dir)) {
      setNotice(null)
      setError(
        new ApiError({
          kind: 'business',
          code: 'system.invalid_param',
          args: { field: 'default_download_dir' }
        })
      )
      return
    }
    setSaving(true)
    setError(null)
    setNotice(null)
    try {
      // 逐项提交本页**真正可改**的字段，不用 { ...config }：那样会把只读字段
      // （default_mount_mode / default_mount_dir / repo_mounts）一起回写，
      // 让人误以为挂载默认值也能从这里改（代理侧已不接受它们）。
      const result = await agentApi.patchConfig({
        auto_mount: config.auto_mount,
        auto_login: config.auto_login,
        default_download_dir: config.default_download_dir,
        language,
        start_at_login: config.start_at_login,
        update_channel: config.update_channel,
        server_alias: config.server_alias
      })
      setConfig(result.config)
      setNotice(t('settings.saved'))
      await agent.refresh()
    } catch (err) {
      setError(err)
    } finally {
      setSaving(false)
    }
  }

  // "检查更新"一键完成整个流程：整包更新检查 + 客户端资源热更（进度经 web_update 事件驱动）。
  // 不再拆成两个按钮：两者本来就是同一次"检查并更新"（真实反馈："功能重复了，做完整个功能"）。
  const checkUpdate = async (): Promise<void> => {
    setChecking(true)
    setError(null)
    setNotice(null)
    try {
      setUpdate(await agentApi.checkUpdate())
    } catch (err) {
      setError(err)
    }
    try {
      await agent.applyWebUpdate()
    } catch (err) {
      setError(err)
    } finally {
      setChecking(false)
    }
  }

  const applyUpdate = async (): Promise<void> => {
    setApplying(true)
    setError(null)
    setNotice(null)
    try {
      await agentApi.applyUpdate()
      setNotice(t('settings.update.started'))
    } catch (err) {
      setError(err)
    } finally {
      setApplying(false)
    }
  }

  // 安装/重装客户端证书需要当前会话令牌与已固化的服务端证书指纹。
  const installable = Boolean(active?.token && active.certSha256 && user)
  const matched = Boolean(identity?.installed && active && identityMatchesServer(identity, active))
  const mismatched = Boolean(identity?.installed && active && !matched)

  const installIdentity = async (): Promise<void> => {
    if (!active?.token || !active.certSha256 || !user) return
    setIdentityBusy(true)
    setIdentityError(null)
    setNotice(null)
    try {
      const value = await agentApi.installIdentity({
        server_url: active.baseUrl,
        token: active.token,
        user_id: user.id,
        username: user.username,
        cert_sha256: active.certSha256
      })
      setIdentity(value)
      setNotice(t('settings.saved'))
      await agent.refresh()
    } catch (err) {
      setIdentityError(err)
    } finally {
      setIdentityBusy(false)
    }
  }

  const removeIdentity = async (): Promise<void> => {
    setIdentityBusy(true)
    setIdentityError(null)
    setNotice(null)
    try {
      await agentApi.removeIdentity()
      setIdentity({ installed: false })
      setRemoveOpen(false)
      setNotice(t('settings.saved'))
      await agent.refresh()
    } catch (err) {
      setIdentityError(err)
    } finally {
      setIdentityBusy(false)
    }
  }

  const toggleAutoLogin = async (autoLogin: boolean): Promise<void> => {
    patch({ auto_login: autoLogin })
    setAutoLoginSaving(true)
    setIdentityError(null)
    setNotice(null)
    try {
      const result = await agentApi.patchConfig({ auto_login: autoLogin })
      setConfig(result.config)
      await agent.refresh()
    } catch (err) {
      patch({ auto_login: !autoLogin })
      setIdentityError(err)
    } finally {
      setAutoLoginSaving(false)
    }
  }

  return (
    <PageShell
      title={t('page.settings.title')}
      extra={
        <>
          {notice ? <Typography.Text type="success">{notice}</Typography.Text> : null}
          <Button onClick={() => void agent.refresh()}>{t('common.refresh')}</Button>
          <Button type="primary" loading={saving} disabled={disabled || !config} onClick={() => void save()}>
            {t('common.save')}
          </Button>
        </>
      }
    >
      {disabled ? <Alert type="warning" showIcon message={t('settings.agent.notAvailable')} style={{ marginBottom: spacing.md }} /> : null}
      {error ? <ErrorNotice error={error} /> : null}

      <SectionCard title={t('settings.agent.title')}>
        <Space direction="vertical" size={spacing.md} style={{ width: '100%' }}>
          {/* 挂载形态与挂载目录**不在这里**：那是每个库自己的事（存储库 → 挂载设置），
              放一份全局默认只会让用户以为改一处就能管所有库。代理仍保留内部默认值兜底。 */}
          <div>
            <Typography.Text style={{ display: 'block', marginBottom: spacing.xs }}>{t('settings.defaultDownloadDir')}</Typography.Text>
            <Input
              value={config?.default_download_dir ?? ''}
              disabled={disabled}
              style={{ maxWidth: 420 }}
              onChange={(event) => patch({ default_download_dir: event.target.value })}
            />
          </div>
          <div>
            <Typography.Text style={{ display: 'block', marginBottom: spacing.xs }}>{t('settings.autoMount')}</Typography.Text>
            <Switch
              checked={config?.auto_mount ?? false}
              disabled={disabled}
              onChange={(checked) => patch({ auto_mount: checked })}
            />
          </div>
          <div>
            <Typography.Text style={{ display: 'block', marginBottom: spacing.xs }}>{t('settings.startAtLogin')}</Typography.Text>
            <Switch
              checked={config?.start_at_login ?? false}
              disabled={disabled}
              onChange={(checked) => patch({ start_at_login: checked })}
            />
          </div>
        </Space>
      </SectionCard>

      <SectionCard title={t('settings.language')}>
        <Space direction="vertical" size={spacing.md} style={{ width: '100%' }}>
          <Select<Language>
            value={language}
            style={{ width: 220 }}
            onChange={(value) => {
              onLanguageChange(value)
              patch({ language: value })
            }}
            options={LANGUAGES.map((value) => ({ value, label: LANGUAGE_LABELS[value] }))}
          />
          <div>
            <Typography.Text style={{ display: 'block', marginBottom: spacing.xs }}>{t('settings.serverAlias')}</Typography.Text>
            <Input
              value={config?.server_alias ?? ''}
              disabled={disabled}
              style={{ maxWidth: 320 }}
              onChange={(event) => patch({ server_alias: event.target.value })}
            />
          </div>
          <div>
            <Typography.Text style={{ display: 'block', marginBottom: spacing.xs }}>{t('settings.updateChannel')}</Typography.Text>
            <Input
              value={config?.update_channel ?? ''}
              disabled={disabled}
              style={{ maxWidth: 220 }}
              placeholder={t('settings.update.channelPlaceholder')}
              onChange={(event) => patch({ update_channel: event.target.value })}
            />
          </div>
        </Space>
      </SectionCard>

      <SectionCard title={t('settings.cert.title')}>
        <Space direction="vertical" size={spacing.md} style={{ width: '100%' }}>
          {identityError ? <ErrorNotice error={identityError} /> : null}
          {matched ? (
            <>
              <Descriptions size="small" column={1} bordered>
                <Descriptions.Item label={t('settings.cert.fingerprint')}>
                  <CopyableText value={identity?.fingerprint_sha256} monospace />
                </Descriptions.Item>
                <Descriptions.Item label={t('settings.cert.serial')}>{identity?.serial || '-'}</Descriptions.Item>
                <Descriptions.Item label={t('settings.cert.notAfter')}>{formatTime(identity?.not_after)}</Descriptions.Item>
                <Descriptions.Item label={t('field.user')}>{identity?.username || '-'}</Descriptions.Item>
              </Descriptions>
              <Space>
                <Button loading={identityBusy} disabled={disabled || !installable} onClick={() => void installIdentity()}>
                  {t('settings.cert.reinstall')}
                </Button>
                <Button danger disabled={disabled || identityBusy} onClick={() => setRemoveOpen(true)}>
                  {t('settings.cert.remove')}
                </Button>
              </Space>
            </>
          ) : (
            <>
              <Typography.Text type={mismatched ? 'warning' : 'secondary'}>
                {mismatched ? t('settings.cert.serverMismatch') : t('settings.cert.notInstalled')}
              </Typography.Text>
              <Button
                type="primary"
                loading={identityBusy}
                disabled={disabled || !installable}
                onClick={() => void installIdentity()}
              >
                {t('settings.cert.install')}
              </Button>
            </>
          )}
          <div>
            <Typography.Text style={{ display: 'block', marginBottom: spacing.xs }}>
              {t('settings.cert.autoLogin')}
            </Typography.Text>
            <Switch
              checked={config?.auto_login ?? false}
              loading={autoLoginSaving}
              disabled={disabled}
              onChange={(checked) => void toggleAutoLogin(checked)}
            />
          </div>
        </Space>
        <ConfirmDialog
          open={removeOpen}
          danger
          loading={identityBusy}
          title={t('settings.cert.removeConfirm')}
          onConfirm={() => void removeIdentity()}
          onCancel={() => setRemoveOpen(false)}
        />
      </SectionCard>

      <SectionCard
        title={t('settings.update.title')}
        extra={
          // 唯一的入口：一次点击完成"整包检查 + 资源热更"全流程；热更进行中同样锁住按钮。
          <Button
            loading={checking || webBusy}
            disabled={disabled || webBusy}
            onClick={() => void checkUpdate()}
          >
            {checking || webBusy ? t('settings.update.checking') : t('settings.update.check')}
          </Button>
        }
      >
        <Space direction="vertical" size={spacing.md} style={{ width: '100%' }}>
          {update ? (
            <Descriptions size="small" column={1} bordered>
              <Descriptions.Item label={t('field.version')}>
                {update.available
                  ? t('settings.update.available', { version: update.version ?? '-' })
                  : t('settings.update.upToDate')}
              </Descriptions.Item>
              <Descriptions.Item label={t('settings.update.source')}>{update.source || '-'}</Descriptions.Item>
              <Descriptions.Item label={t('settings.update.size')}>{formatBytes(update.size_bytes)}</Descriptions.Item>
              <Descriptions.Item label={t('settings.update.notes')}>{update.notes || '-'}</Descriptions.Item>
            </Descriptions>
          ) : (
            <Typography.Text type="secondary">{t('settings.update.notAvailable')}</Typography.Text>
          )}

          {diagnostics ? (
            <Typography.Text type="secondary">
              {`${t('update.diagnostics')}: ${diagnostics}`}
            </Typography.Text>
          ) : null}

          {hasUpdate ? (
            <Space direction="vertical" size={spacing.xs} style={{ width: '100%' }}>
              {!update?.available && targetVersion ? (
                <Typography.Text>{t('settings.update.available', { version: targetVersion })}</Typography.Text>
              ) : null}
              {updatePercent === undefined ? <Progress status="active" /> : <Progress percent={updatePercent} />}
              <Space>
                <Typography.Text type="secondary">
                  {t('update.progress', { received: formatBytes(progressReceived), total: formatBytes(progressTotal) })}
                </Typography.Text>
                <Button
                  type="primary"
                  loading={applying || (updateState?.downloading ?? false)}
                  disabled={disabled}
                  onClick={() => void applyUpdate()}
                >
                  {t('update.downloadAndInstall')}
                </Button>
              </Space>
            </Space>
          ) : null}

          <div>
            <Typography.Text style={{ display: 'block', marginBottom: spacing.xs }}>
              {t('update.web.title')}
            </Typography.Text>
            <Space direction="vertical" size={spacing.xs} style={{ width: '100%' }}>
              {webState?.active_version ? (
                <Typography.Text type="secondary">
                  {t('update.web.activeVersion', { version: webState.active_version })}
                </Typography.Text>
              ) : null}
              {webStageKey ? (
                <>
                  <Typography.Text type="secondary">{t(webStageKey)}</Typography.Text>
                  {webPercent === undefined ? <Progress status="active" /> : <Progress percent={webPercent} />}
                </>
              ) : null}
              {webState?.state === 'activated' ? (
                <Space>
                  <Typography.Text type="success">
                    {t('update.web.activated', {
                      version: webState.active_version || webState.available_version || '-'
                    })}
                  </Typography.Text>
                  <Button onClick={() => void relaunchClient()}>{t('update.web.restart')}</Button>
                </Space>
              ) : null}
              {webState?.state === 'failed' && webState.error === 'not_newer' ? (
                // 资源版本不高于当前版本 = 没有可更新的内容，是**正常结果**而非错误：
                // 绿色对勾明确告诉用户"无需更新"（真实反馈："版本相同不是错误，绿色对勾才对"）。
                <Typography.Text type="success">
                  <CheckCircleOutlined /> {t('update.web.upToDate')}
                </Typography.Text>
              ) : webState?.state === 'failed' && webState.error ? (
                <ErrorNotice error={new ApiError({ kind: 'business', code: webState.error })} />
              ) : null}
              {webNoArtifact ? <Typography.Text type="secondary">{t('update.web.noArtifact')}</Typography.Text> : null}
            </Space>
          </div>
        </Space>
      </SectionCard>
    </PageShell>
  )
}

/**
 * 是否为绝对路径（对齐 Go filepath.IsAbs 在 Windows 下的判定）。
 *
 * 接受：`C:\dir` / `C:/dir`、UNC（`\\host\share`）、盘符根（`\dir`）。
 */
function isAbsoluteDir(value: string): boolean {
  const trimmed = value.trim()
  if (trimmed === '') return false
  if (/^[A-Za-z]:[\\/]/.test(trimmed)) return true
  return trimmed.startsWith('\\')
}
