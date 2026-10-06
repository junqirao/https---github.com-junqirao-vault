import { ExclamationCircleFilled, LoadingOutlined } from '@ant-design/icons'
import { Button, Input, Modal, Popover, Radio, Space, Tag, Tooltip, Typography } from 'antd'

import { StatusTag } from '../../components/StatusTag'
import type { AgentMountMode, AgentMountState } from '../../api/agentTypes'
import { errorCodeOf, errorHintKeyOf, mountErrorInfoOf, translateError } from '../../api/errors'
import { hasKey, useI18n } from '../../i18n'
import { palette, radius } from '../../tokens/palette'
import { effectiveMountDir } from '../../utils/mountPath'

/**
 * 挂载错误的**唯一**标记：红色圆形感叹号 + 悬浮详情（多行）。
 *
 * 为什么这样收敛（真实反馈："错误信息也不用一直摆着…把那个红色三角感叹的功能替换掉这个
 * 错误的标记，只展示一次就行了，如果有多个错误信息加多一行去展示，不要展示多个，这样很乱"）：
 *   - 错误不再以文字形式长期占据列表列与卡片（那些位置只留状态本身）；
 *   - "本次请求失败"与"本机残留错误"汇聚到同一个标记里，逐行展示，复制一次拿全。
 *
 * 图标用 `ExclamationCircleFilled`（圆形）而**不是** `WarningFilled`（三角形）：
 * 三角形在这套界面里是"系统级警告/风险"的语气，而挂载失败只是"这一项这次没成功"，
 * 每个失败的库都顶着一个红三角过于刺眼（真实反馈："挂载错误时不要用三角感叹号的图标，
 * 你换一个，那个太突兀了"）。圆形感叹号同样一眼可辨，语气平静得多。
 */
function MountErrorMarker({ lines }: { lines: string[] }): JSX.Element {
  const { t } = useI18n()
  const copyText = lines.join('\n')
  return (
    <Popover
      trigger="hover"
      placement="top"
      title={t('agent.mount.lastError')}
      content={
        // 悬浮层存在的前提就是"鼠标移进来能选中/复制这段报错"：全局默认禁选，这里显式放开。
        <Space className="selectable" direction="vertical" size={2} style={{ maxWidth: 420 }}>
          {lines.map((line, index) => (
            <Typography.Text
              key={`${index}:${line}`}
              style={{ fontFamily: index === 0 ? undefined : 'monospace', fontSize: 12 }}
            >
              {line}
            </Typography.Text>
          ))}
          <Typography.Text
            type="secondary"
            copyable={{ text: copyText, tooltips: [t('common.copy'), t('common.copied')] }}
            style={{ fontSize: 12 }}
          >
            {t('common.copy')}
          </Typography.Text>
        </Space>
      }
    >
      <ExclamationCircleFilled style={{ color: palette.danger, fontSize: 14, cursor: 'help' }} />
    </Popover>
  )
}

/**
 * 把挂载记录里的 `last_error` 转成可读文案。
 *
 * `last_error` 是稳定码（如 `lease.revoked`、`connect:platform.ps_failed`）：有对应文案
 * 就翻译，没有就原样展示（稳定码至少是可搜索的英文词，也比空白强）。
 * 典型场景：被服务端强制下线时挂载状态是 `revoked` + `lease.revoked`，用户最需要
 * 一眼看懂"盘为什么没了"。
 */
function mountLastErrorLine(code: string, t: (key: string) => string): string {
  const key = `err.${code}`
  return hasKey(key) ? t(key) : code
}

/**
 * 汇总要展示的错误行（**一个标记、多行信息**）。
 *
 * 顺序：本次操作错误在最前（最新、最相关），随后是挂载状态里的稳定码、连接信息与原始报错。
 * 连接信息里 portal / target_iqn 在失败状态中一直存在（代理在 connect 之前就写入），
 * 以前界面不显示 —— 用户只能看到"挂载失败（阶段：connect）"，无从定位（真实反馈）。
 *
 * ⚠️ 只有**真的失败**才产出错误行（真实反馈："挂载成功时就不要展示最后错误了"）：
 * 挂载成功（mounted）时 portal / target_iqn / 阶段详情本来就是正常信息，以前它们也被
 * 当错误行塞进标记里，于是每个挂好的存储库旁边都永久挂着一个红色感叹号。
 * 保留 error 与 revoked：前者是本机/挂载失败，后者是被服务端撤销（代理会写 last_error，
 * 用户正需要知道"盘为什么没了"）。
 */
function mountErrorLines(
  mount: AgentMountState | undefined,
  requestError: unknown,
  t: (key: string, params?: Record<string, unknown>) => string
): string[] {
  const lines: string[] = []
  if (requestError) {
    lines.push(translateError(requestError))
    const hintKey = errorHintKeyOf(requestError)
    if (hintKey) lines.push(t(hintKey))
    const code = errorCodeOf(requestError)
    if (code) lines.push(code)
    for (const item of mountErrorInfoOf(requestError)) {
      lines.push(`${t(`mount.info.${item.key}`)}: ${item.value}`)
    }
  }
  const failed = Boolean(requestError) || mount?.state === 'error' || mount?.state === 'revoked'
  if (!failed) return lines
  if (mount) {
    if (mount.last_error) lines.push(mountLastErrorLine(mount.last_error, t))
    const connection = [
      { key: 'portal', value: mount.portal },
      { key: 'target_iqn', value: mount.target_iqn },
      { key: 'mount_mode', value: mount.mount_mode }
    ]
    for (const item of connection) {
      if (typeof item.value === 'string' && item.value.trim() !== '') {
        lines.push(`${t(`mount.info.${item.key}`)}: ${item.value}`)
      }
    }
    if (mount.last_error_detail) lines.push(mount.last_error_detail)
  }
  return lines
}

/**
 * 实测的本机 iSCSI 会话状态：**按钮与状态标签一律以它为准**，不看记录。
 *
 * 返回三态，缺一不可：
 *   - true  —— 代理实测本机还有活动会话，盘确实挂着；
 *   - false —— 代理实测确认**没有**活动会话（会话被系统/用户断开、服务重启、长时间断网）：
 *              记录还是 mounted，但盘根本不在，界面必须显示"已断开"并把主按钮换回"挂载"；
 *   - undefined —— **尚未核对**（代理刚启动，session_checked_at 缺省）或旧代理没有这个字段：
 *              此时按记录状态展示，绝不能把"没有证据"当成"断线"（那会让一块正常挂着的盘
 *              显示成已断开，用户点"挂载"反而去重连一个好好的盘）。
 *
 * 背景（真实诉求）："在客户端挂载和卸载的按钮应以实际为准，看对应的 iSCSI 连接是否在活动中的"。
 */
export function mountSessionLive(mount: AgentMountState | undefined): boolean | undefined {
  if (!mount || !mount.session_checked_at) return undefined
  return mount.session_active === true
}

/**
 * 挂载状态的统一展示：无记录时显示"未挂载"。
 *
 * 两类失败合并成**一个**红色感叹号：
 *   - 本次操作的请求错误（requestError，如刚点挂载失败）；
 *   - 本机残留的错误（mount.state = error，如心跳连续失败）。
 * state = error 时不再另外显示"错误"标签 —— 图标与标签说的是同一件事，两个标记很乱。
 * 悬浮层用 Popover 而非 Tooltip：鼠标要能移进弹层里选中/复制那段报错。
 */
export function MountStateCell({
  mount,
  requestError
}: {
  mount: AgentMountState | undefined
  /** 本次操作的请求错误（可选）；与挂载状态错误合并展示。 */
  requestError?: unknown
}): JSX.Element {
  const { t } = useI18n()
  const lines = mountErrorLines(mount, requestError, t)
  if (!mount) {
    return lines.length > 0 ? <MountErrorMarker lines={lines} /> : <Tag>{t('repo.mount.notMounted')}</Tag>
  }
  // 挂载中优先显示**细粒度阶段**（"服务端正在创建 iSCSI 目标"这类）：服务端侧的挂载是
  // 同步重活（准备虚拟磁盘 + PowerShell 下发 iSCSI 目标，实测数十秒），只显示"挂载中"
  // 用户无从知道卡在哪一步（真实反馈）。未知阶段（新代理旧界面）回退成通用"挂载中"，
  // 绝不把原始 key 露到界面上。
  const phase = mount.state === 'mounting' ? mount.phase : undefined
  const phaseKey = phase ? `mount.phase.${phase}` : ''
  // 记录说"已挂载"、但代理实测本机已无活动会话：如实显示"已断开"。
  // 显示"已挂载"就是界面在撒谎（真实诉求："按钮应以实际为准，看对应的 iSCSI 连接是否在活动中的"）。
  const sessionLost = mount.state === 'mounted' && mountSessionLive(mount) === false
  const tag = phaseKey && hasKey(phaseKey) ? (
    <Tag
      icon={<LoadingOutlined spin />}
      color="processing"
      bordered={false}
      style={{ borderRadius: radius.sm, marginInlineEnd: 0 }}
    >
      {t(phaseKey)}
    </Tag>
  ) : sessionLost ? (
    <Tooltip title={t('agent.mount.sessionLost')}>
      <StatusTag group="mount" value="disconnected" />
    </Tooltip>
  ) : (
    <StatusTag group="mount" value={mount.state} />
  )
  // state = error 时只留标记（标签与标记表达同一件事）。
  if (mount.state === 'error' && lines.length > 0) return <MountErrorMarker lines={lines} />
  if (lines.length === 0) return tag
  return (
    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 4 }}>
      {tag}
      <MountErrorMarker lines={lines} />
    </span>
  )
}

/** 打开挂载点的目标路径：盘符模式补根斜杠，目录模式直接用挂载目录。 */
export function openTargetOf(mount: AgentMountState): string {
  if (mount.mount_mode === 'letter') return `${mount.mount_path.replace(/[\\/]+$/, '')}\\`
  return mount.mount_path
}

export interface MountActionButtonsProps {
  mount: AgentMountState | undefined
  /** 是否为当前用户自己的分配（他人分配不可挂载）。 */
  canMount: boolean
  /** 代理是否可用。 */
  available: boolean
  busy?: boolean
  onMount: () => void
  onUnmount: () => void
  onRemount: () => void
  onOpenPath: (path: string) => void
}

/** 行内挂载操作：挂载 / 卸载 / 重新挂载 / 打开目录（代理不可用时统一禁用）。 */
export function MountActionButtons({
  mount,
  canMount,
  available,
  busy,
  onMount,
  onUnmount,
  onRemount,
  onOpenPath
}: MountActionButtonsProps): JSX.Element {
  const { t } = useI18n()
  if (!canMount) return <Typography.Text type="secondary">-</Typography.Text>

  // mounting/unmounting 都是"转场中"，重新挂载在两者期间都不可点。
  const transitioning = mount?.state === 'mounting' || mount?.state === 'unmounting'
  // 卸载：只在 mounting 中禁用；unmounting（含卡死的脏记录）**必须可点**——
  // 记录卡在"卸载中"时，重试卸载是唯一的出口；按钮一旦是灰的，用户就再也点不动了
  // （真实反馈："卡在卸载中，也没有卸载的选项"）。
  // 现在代理侧只有一种卸载语义（尽力清理干净并删除记录），重试永远有意义。
  const unmountDisabled = !available || mount?.state === 'mounting'
  // 记录在、但实测没有活动会话：盘根本不在，此时"卸载"不是用户要的入口（点了只是清记录），
  // 该给的是"挂载"—— 代理会先清理残留再真重连（服务端/代理自己闭环），用户不必先卸载再挂载。
  const sessionLost = mount !== undefined && mountSessionLive(mount) === false
  const buttons = (
    <Space size={0}>
      {mount ? (
        sessionLost ? (
          <>
            <Button type="link" size="small" disabled={!available} loading={busy} onClick={onMount}>
              {t('action.mount')}
            </Button>
            {/* 记录清理入口保留：用户也可能就是想把这条断线的记录收掉。 */}
            <Button type="link" size="small" disabled={unmountDisabled} loading={busy} onClick={onUnmount}>
              {t('action.unmount')}
            </Button>
          </>
        ) : (
          <>
            <Button type="link" size="small" disabled={unmountDisabled} loading={busy} onClick={onUnmount}>
              {t('action.unmount')}
            </Button>
            <Button type="link" size="small" disabled={!available || transitioning} loading={busy} onClick={onRemount}>
              {t('agent.action.remount')}
            </Button>
            <Button
              type="link"
              size="small"
              disabled={!available || mount.state !== 'mounted'}
              onClick={() => onOpenPath(openTargetOf(mount))}
            >
              {t('agent.action.openDirectory')}
            </Button>
          </>
        )
      ) : (
        <Button type="link" size="small" disabled={!available} loading={busy} onClick={onMount}>
          {t('action.mount')}
        </Button>
      )}
    </Space>
  )

  if (available) return buttons
  return <Tooltip title={t('agent.hostMissing')}>{buttons}</Tooltip>
}

export interface MountDialogProps {
  open: boolean
  repoName?: string
  /** 目录名里用的服务端名称（本地别名优先）：用于如实告知"实际挂载到哪"。 */
  serverAlias?: string
  mode: AgentMountMode
  path: string
  submitting?: boolean
  onModeChange: (mode: AgentMountMode) => void
  onPathChange: (path: string) => void
  onConfirm: () => void
  onCancel: () => void
}

/**
 * 挂载前确认：目录模式下允许先修改目标目录。
 *
 * 这里填的是**父目录**，实际挂载点在它下面一级 `<服务端名称>_<存储库名称>`（代理侧规则）；
 * 弹窗里直接把算好的最终路径显示出来 —— 用户点挂载前就该知道文件会出现在哪。
 */
export function MountDialog({
  open,
  repoName,
  serverAlias,
  mode,
  path,
  submitting,
  onModeChange,
  onPathChange,
  onConfirm,
  onCancel
}: MountDialogProps): JSX.Element {
  const { t } = useI18n()
  const directory = mode === 'directory'
  const invalid = directory && !path.trim()
  const effectiveDir = directory ? effectiveMountDir(path, serverAlias ?? '', repoName ?? '') : ''

  return (
    <Modal
      open={open}
      title={`${t('agent.mount.dialog.title')}${repoName ? ` · ${repoName}` : ''}`}
      width={560}
      centered
      okText={t('action.mount')}
      cancelText={t('common.cancel')}
      confirmLoading={submitting}
      okButtonProps={{ disabled: invalid }}
      onOk={onConfirm}
      onCancel={onCancel}
      destroyOnClose
    >
      <Space direction="vertical" size={12} style={{ width: '100%' }}>
        <div>
          <Typography.Text style={{ display: 'block', marginBottom: 8 }}>{t('agent.mount.dialog.mode')}</Typography.Text>
          <Radio.Group value={mode} onChange={(event) => onModeChange(event.target.value as AgentMountMode)}>
            <Radio.Button value="letter">{t('settings.mountMode.letter')}</Radio.Button>
            <Radio.Button value="directory">{t('settings.mountMode.directory')}</Radio.Button>
          </Radio.Group>
        </div>
        {directory ? (
          <div>
            <Typography.Text style={{ display: 'block', marginBottom: 8 }}>{t('agent.mount.dialog.dir')}</Typography.Text>
            <Input
              value={path}
              placeholder={t('agent.mount.dialog.dirHint')}
              onChange={(event) => onPathChange(event.target.value)}
            />
            {invalid ? (
              <Typography.Text type="danger" style={{ fontSize: 12 }}>
                {t('agent.mount.dialog.dirRequired')}
              </Typography.Text>
            ) : (
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                {t('agent.mount.dialog.dirParentHint')}
              </Typography.Text>
            )}
            {effectiveDir ? (
              <Typography.Text type="secondary" style={{ fontSize: 12, display: 'block', marginTop: 4 }}>
                {t('agent.mount.dialog.dirEffective', { path: effectiveDir })}
              </Typography.Text>
            ) : null}
          </div>
        ) : null}
      </Space>
    </Modal>
  )
}
