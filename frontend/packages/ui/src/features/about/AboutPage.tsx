import { CopyOutlined } from '@ant-design/icons'
import { useQuery } from '@tanstack/react-query'
import { App as AntApp, Button, Descriptions, Space, Tag, Typography } from 'antd'

import { useApi } from '../../api/provider'
import { CopyableText } from '../../components/CopyableText'
import { PageShell } from '../../components/PageShell'
import { SectionCard } from '../../components/SectionCard'
import { useAgent, useAgentVersion } from '../../hooks/useAgent'
import { useServerConfig } from '../../hooks/useServerConfig'
import { useI18n } from '../../i18n'
import { fontSize, palette, radius, spacing } from '../../tokens'

/**
 * 开源许可的官方在线地址。
 *
 * 为什么是外链而不是随包分发（这段说明原在 electron/main.ts 的托盘入口，入口搬到「关于」后仍然成立）：
 * Chromium 及其第三方组件的许可清单（LICENSES.chromium.html）约 9 MB，随包分发会显著增大安装包，
 * 而它几乎不会被打开。打包时因此改为外链分发（见 scripts/after-pack.cjs）：包内只保留 Electron 的
 * MIT 许可全文 + 一份指向下列地址的 THIRD-PARTY-NOTICES.txt。
 *
 * ⚠️ 若发布方自托管了完整清单（构建时用环境变量 VAULT_THIRD_PARTY_LICENSES_URL 指定），请把该地址
 * 加到列表最前面，并与 after-pack.cjs 的 NOTICE 保持一致。
 */
const LICENSE_LINKS: ReadonlyArray<{ labelKey: string; url: string }> = [
  {
    labelKey: 'about.licenseElectron',
    url: 'https://github.com/electron/electron/blob/main/LICENSE'
  },
  {
    labelKey: 'about.licenseChromium',
    url: 'https://chromium.googlesource.com/chromium/src/+/refs/heads/main/LICENSE'
  }
]

export interface AboutPageProps {
  /**
   * 客户端版本（构建期注入，真源是 apps/client 的 package.json）。
   *
   * 由宿主传入而不是在 ui 包里读：ui 包不知道宿主用什么机制注入版本，也不该去猜。
   */
  clientVersion: string
}

/**
 * 「关于」：把**装完就不再变**的那几样东西（客户端 / 本地服务 / 服务端的版本、运行平台、开源许可）
 * 集中到一处。
 *
 * 入口：宿主侧边栏菜单的「关于」（应用端排在「配置」下方，管理端排在末尾），是**页面**不是弹窗
 * —— 这些值要能停留、能选中、能随浏览器前进后退回来，弹窗关掉就没了（本组件只管内容，
 * 路由由宿主注册）。
 *
 * 为什么值得单开一处：这些值平时没人看，但一到了排障就总要用到 —— 以前它们散在顶栏（悬停提示里的
 * 实例 ID）、托盘右键菜单（开源许可）、设置页里，用户被问"你那边是什么版本"时得挨个翻。放一起之后，
 * 右上角那个「复制诊断信息」能把它们一次带走（工单里最常见的一次性请求）。
 *
 * 这里**不显示协议版本**（客户端与服务端各自的 api_version）：它是通信内部的硬约束，两端不匹配时
 * 登录/启动阶段就会拦下并给出专门提示（见 boot.compat.*），用户既不需要读、也改不了它；
 * 摆在版本信息里只会让人以为"两边的协议版本号不一样就是有问题"。
 *
 * 取数说明：
 *   - 服务端版本走**匿名**的 GET /v1/system/info（认证前也要用），所以不依赖登录态；
 *     查询键与外壳的能力探测、系统页共用（['system-info']），打开这里不会额外多打一次请求，
 *     离开也不会把缓存丢掉。
 *   - 本地服务版本读代理快照（useAgent）：没装/没起来时是 null，显示 `-` 而不是编一个数字。
 */
export function AboutPage({ clientVersion }: AboutPageProps): JSX.Element {
  const { t } = useI18n()
  const { message } = AntApp.useApp()
  const api = useApi()
  const agent = useAgent()
  const agentVersion = useAgentVersion()
  const { active } = useServerConfig()

  const infoQuery = useQuery({
    queryKey: ['system-info'],
    queryFn: () => api.systemInfo()
  })

  const agentVersionText = agent.health?.agent_version || '-'
  const serverVersion = infoQuery.data?.server_version || '-'
  const platform = agent.health?.platform || '-'

  /**
   * 「复制诊断信息」的内容：一行一项，用语与界面标签一致（用户对着界面念得出来）。
   *
   * 含实例 ID 与服务地址：这两样是"哪一台"的唯一答案，排障时比版本号更常被问到。
   */
  const diagnostics = [
    t('about.diagnosticsTitle'),
    `${t('about.clientVersion')}: ${clientVersion}`,
    `${t('about.localServiceVersion')}: ${agentVersionText}`,
    `${t('about.serverVersion')}: ${serverVersion}`,
    `${t('about.platform')}: ${platform}`,
    `${t('about.serverName')}: ${active?.serverName || '-'}`,
    `${t('about.serverUrl')}: ${active?.baseUrl || '-'}`,
    `${t('about.serverInstanceId')}: ${active?.instanceId || '-'}`
  ].join('\n')

  const handleCopy = (): void => {
    void (async () => {
      // 剪贴板在无权限/窗口失焦时会拒绝：两条出路都要有回应，不留"点了没反应"的按钮。
      if (!navigator.clipboard) {
        message.error(t('common.failed'))
        return
      }
      try {
        await navigator.clipboard.writeText(diagnostics)
        message.success(t('common.copied'))
      } catch {
        message.error(t('common.failed'))
      }
    })()
  }

  return (
    <PageShell
      title={t('about.title')}
      subtitle={t('app.tagline')}
      extra={
        <Button icon={<CopyOutlined />} onClick={handleCopy}>
          {t('about.copyDiagnostics')}
        </Button>
      }
    >
      <SectionCard title={t('about.versionSection')}>
        {/* 品牌区：与托盘图标同一套视觉（圆角方块 + V），让"关于"第一眼就认出是谁。 */}
        <div style={{ display: 'flex', alignItems: 'center', gap: spacing.md, marginBottom: spacing.md }}>
          <div
            style={{
              width: 44,
              height: 44,
              flex: '0 0 auto',
              borderRadius: radius.md,
              background: palette.accent,
              color: palette.textInverse,
              display: 'flex',
              alignItems: 'center',
              justifyContent: 'center',
              fontSize: fontSize.xl,
              fontWeight: 600
            }}
          >
            V
          </div>
          <div>
            <Typography.Text strong style={{ display: 'block' }}>
              {t('app.name')}
            </Typography.Text>
            <Typography.Text type="secondary" style={{ display: 'block', fontSize: fontSize.xs }}>
              {t('app.tagline')}
            </Typography.Text>
          </div>
        </div>

        <Descriptions size="small" column={1} bordered>
          <Descriptions.Item label={t('about.clientVersion')}>{clientVersion}</Descriptions.Item>
          <Descriptions.Item label={t('about.localServiceVersion')}>
            <Space size={spacing.xs}>
              <span>{agentVersionText}</span>
              {/* 本地服务比客户端旧（自动更新没走完/装的是旧包）：这里就得说出来，
                  否则"版本对不对"要靠用户自己去比两个数字。 */}
              {agentVersion.mismatch ? <Tag color={palette.warning}>{t('agent.versionMismatch')}</Tag> : null}
            </Space>
          </Descriptions.Item>
          <Descriptions.Item label={t('about.serverVersion')}>
            {/* 加载中给省略号而不是 '-'：免得用户把"正在取"读成"服务端不支持"。 */}
            {infoQuery.isLoading ? '…' : serverVersion}
          </Descriptions.Item>
          <Descriptions.Item label={t('about.platform')}>{platform}</Descriptions.Item>
        </Descriptions>
      </SectionCard>

      <SectionCard title={t('about.serverSection')}>
        <Descriptions size="small" column={1} bordered>
          <Descriptions.Item label={t('about.serverName')}>{active?.serverName || '-'}</Descriptions.Item>
          <Descriptions.Item label={t('about.serverUrl')}>
            <CopyableText value={active?.baseUrl} monospace />
          </Descriptions.Item>
          <Descriptions.Item label={t('about.serverInstanceId')}>
            <CopyableText value={active?.instanceId} monospace />
          </Descriptions.Item>
        </Descriptions>
      </SectionCard>

      <SectionCard title={t('about.licenseSection')}>
        <Typography.Paragraph type="secondary" style={{ fontSize: fontSize.xs, margin: 0 }}>
          {t('about.licenseIntro')}
        </Typography.Paragraph>
        {/* 地址原文直接当链接文字：许可这事要经得起核对，"点得到的地址"比"一个漂亮的名字"重要。 */}
        <Space className="selectable" direction="vertical" size={2} style={{ marginTop: spacing.xs }}>
          {LICENSE_LINKS.map((link) => (
            <Typography.Link
              key={link.url}
              href={link.url}
              target="_blank"
              rel="noreferrer"
              style={{ fontSize: fontSize.xs }}
            >
              {`${t(link.labelKey)}：${link.url}`}
            </Typography.Link>
          ))}
        </Space>
      </SectionCard>
    </PageShell>
  )
}
