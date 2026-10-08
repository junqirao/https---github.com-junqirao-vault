import { useEffect, useState } from 'react'
import { Alert, Button, Descriptions, Input, InputNumber, Progress, Select, Space, Switch, Typography } from 'antd'
import { CheckCircleOutlined } from '@ant-design/icons'

import { ConfirmDialog } from '../../components/ConfirmDialog'
import { CopyableText } from '../../components/CopyableText'
import { ErrorNotice } from '../../components/ErrorNotice'
import { PageShell } from '../../components/PageShell'
import { SectionCard } from '../../components/SectionCard'
import { agentApi, identityMatchesServer, relaunchClient, selectDirectory, serverKeyOf, supportsDirectoryPicker } from '../../api/agentClient'
import type { AgentConfig, AgentIdentity, UpdateInfo } from '../../api/agentTypes'
import { ApiError } from '../../api/errors'
import { useAgent } from '../../hooks/useAgent'
import { useAuth } from '../../hooks/useAuth'
import { useServerConfig } from '../../hooks/useServerConfig'
import { LANGUAGE_LABELS, LANGUAGES, useI18n, type Language } from '../../i18n'
import { fontSize, spacing } from '../../tokens/palette'
import { bytesToMegabytes, formatBytes, formatTime, megabytesToBytes } from '../../utils/format'

/**
 * 本地读缓存代理的 L1/L2 上限（客户端总量，单位 MiB）。
 *
 * 上下界必须与代理侧（internal/agent/cacheproxy.go 的 cacheMin/Max）一致 —— 否则界面放行、
 * 保存却被 PATCH 以 cache_l1_bytes/cache_l2_bytes invalid_param 拒掉，用户只看到一个对不上的报错。
 * L2 的 0 是**合法值**（关闭 L2），所以它的下界是 0 而不是 cacheMinL2Bytes。
 */
const CACHE_L1_MIN_MIB = 64
const CACHE_L1_MAX_MIB = 64 * 1024
const CACHE_L2_MAX_MIB = 1024 * 1024
/** L2 非零时的最小值（MiB）：小于它就等于没意义（放不下超级块与槽位）。 */
const CACHE_L2_MIN_MIB = 256

export interface ClientSettingsProps {
  language: Language
  onLanguageChange: (language: Language) => void
}

/** 客户端本地设置：真源为本地代理的 /agent/config。 */
export function ClientSettings({ language, onLanguageChange }: ClientSettingsProps): JSX.Element {
  const { t } = useI18n()
  const agent = useAgent()
  const { active, servers } = useServerConfig()
  const { user } = useAuth()
  const [config, setConfig] = useState<AgentConfig | null>(null)
  const [error, setError] = useState<unknown>(null)
  const [saving, setSaving] = useState(false)
  const [notice, setNotice] = useState<string | null>(null)
  const [update, setUpdate] = useState<UpdateInfo | null>(null)
  const [checking, setChecking] = useState(false)
  const [applying, setApplying] = useState(false)
  const [identity, setIdentity] = useState<AgentIdentity | null>(null)
  // 本机保存的**全部**服务端身份：一个服务端一份证书（见 /agent/identities）。
  const [identities, setIdentities] = useState<AgentIdentity[]>([])
  const [identityError, setIdentityError] = useState<unknown>(null)
  const [identityBusy, setIdentityBusy] = useState(false)
  const [autoLoginSaving, setAutoLoginSaving] = useState(false)
  const [removeOpen, setRemoveOpen] = useState(false)
  // 待撤销的"其他服务端"证书；null 表示没有待确认的撤销。
  const [removeOther, setRemoveOther] = useState<AgentIdentity | null>(null)

  const disabled = !agent.available
  // 系统目录选择器（Electron）不可用时，L2 目录只能手填（按钮隐藏）。
  const pickerAvailable = supportsDirectoryPicker()

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

  // 一次性取回"当前服务端那份证书"与"本机全部证书"：
  // 前者决定本页显示"已装/未装/属于其他服务端"，后者用于列出其他服务端的证书并单独撤销。
  const loadIdentity = async (): Promise<void> => {
    const target = active ? { serverUrl: active.baseUrl, instanceId: active.instanceId } : undefined
    const [current, list] = await Promise.all([agentApi.getIdentity(target), agentApi.listIdentities()])
    setIdentity(current)
    setIdentities(list.identities ?? [])
  }

  useEffect(() => {
    if (!agent.available) {
      setIdentity(null)
      setIdentities([])
      return
    }
    let cancelled = false
    loadIdentity().catch((err: unknown) => {
      if (!cancelled) setIdentityError(err)
    })
    return () => {
      cancelled = true
    }
    // 依赖具体字段而不是 active 对象：切服务端后要重新判断"这个服务端装了证书没有"。
  }, [agent.available, active?.baseUrl, active?.instanceId])

  const patch = (values: Partial<AgentConfig>): void => {
    setConfig((prev) => (prev ? { ...prev, ...values } : prev))
  }

  // 选择 L2 缓存目录：系统目录选择器 → 写进本地 config（保存时随表单一起提交）。
  const chooseL2Dir = async (): Promise<void> => {
    setError(null)
    setNotice(null)
    try {
      const selection = await selectDirectory()
      if (!selection || selection.canceled || !selection.path) return
      patch({ cache_l2_dir: selection.path })
    } catch (err) {
      setError(err)
    }
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
    // 缓存预算的上下界与代理侧对齐（见 CACHE_* 常量）；越界就不发请求，直接给出字段级报错。
    for (const field of cacheBudgetViolations(config)) {
      setError(new ApiError({ kind: 'business', code: 'system.invalid_param', args: { field } }))
      setSaving(false)
      return
    }
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
        cache_l1_bytes: config.cache_l1_bytes,
        cache_l2_bytes: config.cache_l2_bytes,
        cache_l2_dir: config.cache_l2_dir
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
  // 其他服务端的证书：当前服务端那份由上面的区块管理，这里只列"别人的"，
  // 让用户能在不影响当前服务端的前提下单独撤销。
  const others = identities.filter((item) => !(active && identityMatchesServer(item, active)))

  /** 证书所属服务端的显示名：能从服务端列表里对上就用列表里的名字，否则退回地址。 */
  const serverNameOf = (item: AgentIdentity): string => {
    const key = item.server_key ?? ''
    const entry = servers.find((server) => serverKeyOf(server) === key)
    return entry?.serverName || item.server_url || '-'
  }

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
      // 只刷新列表：其他服务端的证书不受影响，当前服务端那份已由返回值给出。
      setIdentities((prev) => [...prev.filter((item) => item.server_key !== value.server_key), value])
      setNotice(t('settings.saved'))
      await agent.refresh()
    } catch (err) {
      setIdentityError(err)
    } finally {
      setIdentityBusy(false)
    }
  }

  // 撤销客户端证书。不传 target 表示撤销当前服务端那份；传 target 撤销该服务端那份
  // （多服务端下必须按服务端指定：代理里的"活动身份"可能是别的服务端的）。
  const removeIdentity = async (target?: AgentIdentity): Promise<void> => {
    setIdentityBusy(true)
    setIdentityError(null)
    setNotice(null)
    try {
      const scope = target
        ? { serverUrl: target.server_url, instanceId: target.server_instance_id }
        : active
          ? { serverUrl: active.baseUrl, instanceId: active.instanceId }
          : undefined
      await agentApi.removeIdentity(scope)
      await loadIdentity()
      setRemoveOpen(false)
      setRemoveOther(null)
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

      <SectionCard title={t('settings.cache.title')}>
        <Space direction="vertical" size={spacing.md} style={{ width: '100%' }}>
          <Typography.Text type="secondary">{t('settings.cache.hint')}</Typography.Text>
          <div>
            <Typography.Text style={{ display: 'block', marginBottom: spacing.xs }}>
              {t('settings.cache.l1')}
            </Typography.Text>
            <InputNumber
              min={CACHE_L1_MIN_MIB}
              max={CACHE_L1_MAX_MIB}
              step={64}
              addonAfter="MiB"
              disabled={disabled || !config}
              value={config ? bytesToMegabytes(config.cache_l1_bytes) : undefined}
              onChange={(value) =>
                patch({ cache_l1_bytes: megabytesToBytes(value) ?? config?.cache_l1_bytes ?? 0 })
              }
            />
          </div>
          <div>
            <Typography.Text style={{ display: 'block', marginBottom: spacing.xs }}>
              {t('settings.cache.l2')}
            </Typography.Text>
            <InputNumber
              min={0}
              max={CACHE_L2_MAX_MIB}
              step={256}
              addonAfter="MiB"
              disabled={disabled || !config}
              value={config ? bytesToMegabytes(config.cache_l2_bytes) : undefined}
              onChange={(value) =>
                patch({ cache_l2_bytes: megabytesToBytes(value) ?? config?.cache_l2_bytes ?? 0 })
              }
            />
            <Typography.Text type="secondary" style={{ display: 'block', fontSize: fontSize.xs }}>
              {t('settings.cache.l2Hint', { min: CACHE_L2_MIN_MIB })}
            </Typography.Text>
          </div>
          <div>
            <Typography.Text style={{ display: 'block', marginBottom: spacing.xs }}>
              {t('settings.cache.l2Dir')}
            </Typography.Text>
            <Space.Compact style={{ width: '100%', maxWidth: 520 }}>
              <Input
                value={config?.cache_l2_dir ?? ''}
                spellCheck={false}
                disabled={disabled || !config}
                placeholder={t('settings.cache.l2DirPlaceholder')}
                onChange={(event) => patch({ cache_l2_dir: event.target.value })}
              />
              {pickerAvailable ? (
                <Button disabled={disabled || !config} onClick={() => void chooseL2Dir()}>
                  {t('sourceDir.browse')}
                </Button>
              ) : null}
            </Space.Compact>
            <Typography.Text type="secondary" style={{ display: 'block', fontSize: fontSize.xs }}>
              {t('settings.cache.l2DirHint')}
            </Typography.Text>
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
          {others.length > 0 ? (
            <div>
              <Typography.Text style={{ display: 'block', marginBottom: spacing.xs }}>
                {t('settings.cert.others')}
              </Typography.Text>
              <Space direction="vertical" size={spacing.xs} style={{ width: '100%' }}>
                {others.map((item) => (
                  <Space
                    key={item.server_key ?? item.server_url}
                    style={{ width: '100%', justifyContent: 'space-between' }}
                    align="center"
                  >
                    <Space direction="vertical" size={0}>
                      <Typography.Text>{serverNameOf(item)}</Typography.Text>
                      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                        {[item.server_url, item.username, formatTime(item.not_after)].filter(Boolean).join(' · ')}
                      </Typography.Text>
                    </Space>
                    <Button danger disabled={disabled || identityBusy} onClick={() => setRemoveOther(item)}>
                      {t('settings.cert.remove')}
                    </Button>
                  </Space>
                ))}
              </Space>
            </div>
          ) : null}
        </Space>
        <ConfirmDialog
          open={removeOpen}
          danger
          loading={identityBusy}
          title={t('settings.cert.removeConfirm')}
          onConfirm={() => void removeIdentity()}
          onCancel={() => setRemoveOpen(false)}
        />
        <ConfirmDialog
          open={removeOther !== null}
          danger
          loading={identityBusy}
          title={t('settings.cert.removeOtherConfirm')}
          onConfirm={() => void removeIdentity(removeOther ?? undefined)}
          onCancel={() => setRemoveOther(null)}
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
          {/* 整包更新这一块只回答一个问题："要不要更新"。
              有可更新的版本才展开进度与安装按钮；没有就一个对勾说清楚，不再铺版本/来源/
              包大小/更新说明这些发布侧信息（用户在这一步用不到）。 */}
          {hasUpdate ? (
            <Space direction="vertical" size={spacing.xs} style={{ width: '100%' }}>
              <Typography.Text>{t('settings.update.available', { version: targetVersion ?? '-' })}</Typography.Text>
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
          ) : update ? (
            <Typography.Text type="success">
              <CheckCircleOutlined /> {t('settings.update.upToDate')}
            </Typography.Text>
          ) : disabled ? (
            <Typography.Text type="secondary">{t('settings.update.notAvailable')}</Typography.Text>
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
              {/* 资源版本不高于当前版本，或服务端根本没有可下发的资源包，都等于"没有要更新的东西"：
                  这是**正常结果**而非错误，用绿色对勾直说"无需更新"
                  （真实反馈："版本相同不是错误，绿色对勾才对"）。 */}
              {webState?.state === 'failed' && (webState.error === 'not_newer' || webNoArtifact) ? (
                <Typography.Text type="success">
                  <CheckCircleOutlined /> {t('update.web.upToDate')}
                </Typography.Text>
              ) : webState?.state === 'failed' && webState.error ? (
                <ErrorNotice error={new ApiError({ kind: 'business', code: webState.error })} />
              ) : null}
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

/**
 * 校验缓存配置，返回**非法字段名**列表（空表示全部合法）。
 *
 * L1 必须落在 [min, max]；L2 允许 0（关闭），非零则必须不小于 CACHE_L2_MIN_MIB。
 * 判定用 MiB（界面录入单位），与用户看到的数字同一口径。
 * L2 目录留空 = 默认位置（合法）；填了就必须是绝对路径，否则代理侧会以
 * cache_l2_dir invalid_param 拒掉 —— 界面提前拦下，避免一个对不上的报错。
 */
function cacheBudgetViolations(config: AgentConfig): string[] {
  const fields: string[] = []
  const l1 = bytesToMegabytes(config.cache_l1_bytes) ?? 0
  if (l1 < CACHE_L1_MIN_MIB || l1 > CACHE_L1_MAX_MIB) fields.push('cache_l1_bytes')
  const l2 = bytesToMegabytes(config.cache_l2_bytes) ?? 0
  if (l2 < 0 || l2 > CACHE_L2_MAX_MIB || (l2 > 0 && l2 < CACHE_L2_MIN_MIB)) fields.push('cache_l2_bytes')
  const dir = (config.cache_l2_dir ?? '').trim()
  if (dir !== '' && !isAbsoluteDir(dir)) fields.push('cache_l2_dir')
  return fields
}
