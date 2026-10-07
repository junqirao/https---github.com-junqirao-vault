import { DownloadOutlined } from '@ant-design/icons'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Alert, App, Button, Space, Typography } from 'antd'
import { useEffect, useState } from 'react'

import { useApi } from '../api/provider'
import type { JobState, SysDepsItem } from '../api/types'
import { useErrorNotifier } from '../hooks/useErrorNotifier'
import { useI18n } from '../i18n'
import { spacing } from '../tokens/palette'

/** 关闭状态存 localStorage：关掉之后同一批缺项不再打扰（缺项集合变了会重新出现）。 */
const DISMISS_KEY = 'vault.system.deps.dismissed'

/**
 * 读取已忽略的缺项签名。
 *
 * 存的是**缺项 Key 的排序拼接**而不是布尔值：这样"又缺了新东西"时横幅会重新出现，
 * 否则用户关一次就再也看不到后续故障。localStorage 不可用时静默降级为"未忽略"。
 */
function readDismissed(): string[] {
  try {
    const raw = window.localStorage.getItem(DISMISS_KEY)
    if (!raw) return []
    const parsed: unknown = JSON.parse(raw)
    return Array.isArray(parsed) ? parsed.filter((v): v is string => typeof v === 'string') : []
  } catch {
    return []
  }
}

function writeDismissed(sigs: string[]): void {
  try {
    window.localStorage.setItem(DISMISS_KEY, JSON.stringify(sigs.slice(-8)))
  } catch {
    // 忽略：存不了只是"下次刷新还会再提示一次"，不影响功能
  }
}

/** 缺项集合的签名（排序拼接，保证与顺序无关）。 */
function signatureOf(items: SysDepsItem[]): string {
  return items
    .map((it) => it.key)
    .sort()
    .join(',')
}

/**
 * 任务是否已到终态。
 *
 * 与 JobList 的状态集合一致：pending / running 之外都算终态。
 * 轮询必须**由终态而非次数**来停：装包时长取决于软件源与包大小，
 * 用"最多轮询 N 次"会在慢网络上把还在正常安装的任务判成失败。
 */
function isJobTerminal(state: JobState | undefined): boolean {
  return state === 'succeeded' || state === 'failed' || state === 'cancelled'
}

/** 轮询间隔：本地装包通常几秒到几十秒，1.5s 足够跟手又不至于刷屏。 */
const JOB_POLL_MS = 1500

export interface SystemDepsBannerProps {
  /**
   * 当前用户能否触发"一键安装"。
   *
   * 由宿主布局传入（`isSuperAdmin`），而不是组件自己去取登录态：
   * 服务端的安装接口是超级管理员专属，非管理员渲染出按钮只会点了拿 403。
   */
  canInstall?: boolean
}

/**
 * 系统依赖横幅：服务端启动时已自动修复能修的（挂载 configfs、加载/重载 LIO 内核模块、
 * 按需安装 lvm2 等），**修不了的**在这里逐条告诉使用者"缺了什么、怎么补"。
 *
 * 为什么要有它：缺 configfs 挂载 / 内核模块这类问题时，服务端能正常启动，故障只在
 * 建库、建盘、挂载等业务路径上以 `system.unavailable` 的形式冒出来，错误信息离真正原因很远。
 *
 * 数据来自 GET /v1/system/deps，只读且每次重新探测：运维在服务器上补齐缺项后，
 * 一轮轮询（60s）内横幅自动消失，不需要重启服务端。
 *
 * 对**能靠装包解决**的缺项（当前就是 targetcli 这类命令行工具）额外给一个"安装"按钮：
 * 走 POST /v1/system/deps/install 提交后台任务，前端轮询任务到终态后刷新探测结果。
 * 之所以不在这儿直接同步等待：装包要拉软件源、解包，动辄几十秒到几分钟，同步只会顶到 HTTP 超时。
 */
export function SystemDepsBanner({ canInstall = false }: SystemDepsBannerProps): JSX.Element | null {
  const api = useApi()
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const notifyError = useErrorNotifier()
  const { message } = App.useApp()
  const [dismissed, setDismissed] = useState<string[]>(readDismissed)

  // 正在安装的项 + 它的任务 ID：按钮的 loading 与轮询都挂在这上面。
  // 同时只允许一个：装包会改动整台宿主机，后端也用宿主机级锁把并发安装串行化了，
  // 前端再并行提交只会让用户看到两个必然互相阻塞的进度。
  const [pending, setPending] = useState<{ key: string; title: string; jobId: string } | null>(null)

  const { data } = useQuery({
    queryKey: ['system-deps'],
    queryFn: () => api.systemDeps(),
    // 缺项修复通常不需要重启服务端，轮询即可让横幅自动消失。
    refetchInterval: 60_000,
    // 拉不到（未登录阶段、代理未就绪）时不重试、不报错——这是横幅，不是主流程。
    retry: false
  })

  const install = useMutation({
    // 把 item 一起带进 onSuccess：横幅会随探测结果重渲染，届时按 key 反查会拿到 undefined。
    mutationFn: async (item: SysDepsItem) => ({ item, job: await api.installSystemDeps(item.key) }),
    onSuccess: ({ item, job }) => setPending({ key: item.key, title: item.title, jobId: job.id }),
    // 提交就被拒（未开启自动安装 / 非 root / 无包管理器）走这里：服务端返回的是稳定错误码，
    // 按 code 本地化后能直接告诉用户"是权限还是配置"，不用去翻服务端日志。
    onError: (err) => notifyError(err)
  })

  const jobQuery = useQuery({
    queryKey: ['job', pending?.jobId ?? ''],
    queryFn: () => api.getJob(pending?.jobId ?? ''),
    enabled: Boolean(pending),
    refetchInterval: (query) => (isJobTerminal(query.state.data?.state) ? false : JOB_POLL_MS)
  })

  const jobState = jobQuery.data?.state
  useEffect(() => {
    if (!pending || !isJobTerminal(jobState)) return
    if (jobState === 'succeeded') {
      message.success(t('system.deps.install_done', { title: pending.title }))
    } else {
      // 失败原因只进服务端日志（任务对外只有 failed 布尔，见服务端 toJobDTO），
      // 对用户最有用的动作是"照着「处置」手工装"，所以文案里直接指向它。
      message.error(t('system.deps.install_failed', { title: pending.title }))
    }
    setPending(null)
    // 重新探测：装成功了这一项自然从横幅上消失；失败了把 Hint 留在原地让用户照着手工做。
    void queryClient.invalidateQueries({ queryKey: ['system-deps'] })
  }, [pending, jobState, message, queryClient, t])

  // supported=false 表示平台没有这类"进程外依赖"（Windows），不显示。
  if (!data?.supported) return null

  // 注意这里按**逐项**判定而不是看 report.ok：ok 只反映必需项（服务端能否正常工作），
  // 而横幅要回答的是"还缺什么"——可选项（ntfs-3g 等）缺了同样是使用者需要知道的事，
  // 只是降级成 warning，并且可以关掉。
  const missing = data.items.filter((it) => it.status === 'missing')
  if (missing.length === 0) return null

  const signature = signatureOf(missing)
  if (dismissed.includes(signature)) return null

  const hasRequired = missing.some((it) => it.required)
  const required = missing.filter((it) => it.required)
  const optional = missing.filter((it) => !it.required)

  return (
    <Alert
      type={hasRequired ? 'error' : 'warning'}
      showIcon
      closable
      onClose={() => {
        const next = [...dismissed, signature]
        setDismissed(next)
        writeDismissed(next)
      }}
      style={{ margin: `${spacing.md}px ${spacing.md}px 0` }}
      message={t(hasRequired ? 'system.deps.title' : 'system.deps.title_optional')}
      description={
        // 处置命令是用户要复制到服务器上执行的原文：单独放开文本选择（全局默认禁选）。
        <Space className="selectable" direction="vertical" size={4} style={{ display: 'flex' }}>
          {[...required, ...optional].map((it) => {
            const installing = pending?.key === it.key
            return (
              <div key={it.key}>
                <Space size={8} wrap>
                  <Typography.Text strong>{it.title}</Typography.Text>
                  <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                    {t(it.required ? 'system.deps.required' : 'system.deps.optional')}
                  </Typography.Text>
                  {it.detail ? (
                    <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                      — {it.detail}
                    </Typography.Text>
                  ) : null}
                  {/* 按钮只在"服务端判定此刻能装"且"当前用户是超管"时出现：
                      两者缺一都会变成一个点下去必然失败的按钮。 */}
                  {canInstall && it.installable ? (
                    <Button
                      size="small"
                      type="primary"
                      ghost
                      icon={<DownloadOutlined />}
                      loading={installing}
                      disabled={pending !== null && !installing}
                      onClick={() => install.mutate(it)}
                    >
                      {t(installing ? 'system.deps.installing' : 'system.deps.install')}
                    </Button>
                  ) : null}
                  {installing && jobQuery.data ? (
                    <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                      {t('system.deps.install_progress', { percent: jobQuery.data.progress })}
                    </Typography.Text>
                  ) : null}
                </Space>
                {it.hint ? (
                  <div>
                    <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                      {t('system.deps.fix_label')}: {it.hint}
                    </Typography.Text>
                  </div>
                ) : null}
              </div>
            )
          })}
          {data.fixed.length > 0 ? (
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              {t('system.deps.fixed', { keys: data.fixed.join(', ') })}
            </Typography.Text>
          ) : null}
          {/* 没有安装按钮时得说清"为什么没有"：缺的是权限（非 root）、还是被配置关掉了。
              不说的话用户只会反复找那个不存在的按钮。 */}
          {!data.install_enabled ? (
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              {t('system.deps.install_disabled')}
            </Typography.Text>
          ) : null}
          {/* 非 root 时"修不了"的原因很反直觉（缺的是权限不是依赖），必须说清楚，
              否则运维会照着日志去查内核模块，而真正要做的是用 root 重启服务端。 */}
          {!data.root ? (
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              {t('system.deps.not_root')}
            </Typography.Text>
          ) : null}
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            {t('system.deps.doctor')}
          </Typography.Text>
        </Space>
      }
    />
  )
}
