import { useQuery } from '@tanstack/react-query'
import { Alert, Space, Typography } from 'antd'
import { useState } from 'react'

import { useApi } from '../api/provider'
import type { SysDepsItem } from '../api/types'
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
 * 系统依赖横幅：服务端启动时已自动修复能修的（挂载 configfs、加载/重载 LIO 内核模块、
 * 按需安装 lvm2 等），**修不了的**在这里逐条告诉使用者"缺了什么、怎么补"。
 *
 * 为什么要有它：缺 configfs 挂载 / 内核模块这类问题时，服务端能正常启动，故障只在
 * 建库、建盘、挂载等业务路径上以 `system.unavailable` 的形式冒出来，错误信息离真正原因很远。
 *
 * 数据来自 GET /v1/system/deps，只读且每次重新探测：运维在服务器上补齐缺项后，
 * 一轮轮询（60s）内横幅自动消失，不需要重启服务端。
 */
export function SystemDepsBanner(): JSX.Element | null {
  const api = useApi()
  const { t } = useI18n()
  const [dismissed, setDismissed] = useState<string[]>(readDismissed)

  const { data } = useQuery({
    queryKey: ['system-deps'],
    queryFn: () => api.systemDeps(),
    // 缺项修复通常不需要重启服务端，轮询即可让横幅自动消失。
    refetchInterval: 60_000,
    // 拉不到（未登录阶段、代理未就绪）时不重试、不报错——这是横幅，不是主流程。
    retry: false
  })

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
          {[...required, ...optional].map((it) => (
            <div key={it.key}>
              <Typography.Text strong>{it.title}</Typography.Text>
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                {' '}
                {t(it.required ? 'system.deps.required' : 'system.deps.optional')}
              </Typography.Text>
              {it.detail ? (
                <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                  {' '}
                  — {it.detail}
                </Typography.Text>
              ) : null}
              {it.hint ? (
                <div>
                  <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                    {t('system.deps.fix_label')}: {it.hint}
                  </Typography.Text>
                </div>
              ) : null}
            </div>
          ))}
          {data.fixed.length > 0 ? (
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              {t('system.deps.fixed', { keys: data.fixed.join(', ') })}
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
