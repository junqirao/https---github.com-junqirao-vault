import { useQuery } from '@tanstack/react-query'

/**
 * 服务端往返延迟的探测间隔（毫秒）。
 *
 * 10 秒是"看得出变化、又不至于把顶栏变成一个持续打点的探针"的折中：延迟是环境指标，
 * 抖动本身没有可操作性，间隔再短只会制造噪声。
 */
export const SERVER_LATENCY_INTERVAL_MS = 10_000

/**
 * 单次探测的超时（毫秒）。
 *
 * 比间隔短：这样"连不上"最多 3 秒就落成红色 `- ms`，而不是让指标一直停在"测量中"。
 */
const SERVER_LATENCY_TIMEOUT_MS = 3000

/**
 * 探针打的接口：**专为量往返而生的极简匿名接口**（见 internal/api/system.go 的 handleSystemPing）。
 *
 * 为什么不用 /v1/system/health：它名字像探针，干的却是重活 —— ping 数据库、查 iSCSI 服务、
 * 探白名单卷的文件系统（Windows 上查 iSCSI 要起 PowerShell，单次以秒计）。拿它测出来的是
 * "服务端当时有多忙"，不是"来回一趟多远"（真机反馈：同一台机器上顶栏显示 2000ms+，随手一测
 * 不到 1ms）；而且它在本机有毛病时回 503，界面会把"服务端内部有问题"误报成"网络不通"。
 */
const LATENCY_PROBE_PATH = '/v1/system/ping'

export interface ServerLatency {
  /**
   * 最近一次测得的往返毫秒数（取整）。
   *
   * `undefined` 有两种情形：首次测量还没回来，或探测失败。用 `down` 区分 ——
   * 界面于是能给出两种不同的"没有数字"：灰色占位（在测）与红色 `- ms`（没通）。
   */
  latencyMs: number | undefined
  /** 探测失败（超时 / 网络错误 / 非 2xx；老服务端回的 404 不算，见 queryFn）：界面显示红色 `- ms`。 */
  down: boolean
}

/**
 * 测服务端往返延迟：每 10 秒一次，测不到就是"未连接"。
 *
 * 为什么在渲染进程里直接测：顶栏这个指标的语义是"用户这台机器现在打服务端有多快"，
 * 走代理链路测出来的是代理到服务端的时延，两者不是一回事。
 *
 * 为什么不用 ApiClient：它的超时是 20 秒（等它超时，指标会长时间停在"测量中"），
 * 且 401 会牵动会话续期/退回登录页 —— 这里打的是一个匿名极简探针，只需要一个
 * "通了没有 + 多久"的答案，不该有任何会话副作用。
 */
export function useServerLatency(baseUrl: string | undefined): ServerLatency {
  const query = useQuery({
    queryKey: ['server-latency', baseUrl],
    enabled: Boolean(baseUrl),
    refetchInterval: SERVER_LATENCY_INTERVAL_MS,
    // 失败就是失败：重试会把"未连接"推迟几秒才显示，而这里要的正是立即反馈。
    retry: false,
    queryFn: async () => {
      const startedAt = performance.now()
      const controller = new AbortController()
      const timer = setTimeout(() => controller.abort(), SERVER_LATENCY_TIMEOUT_MS)
      try {
        const response = await fetch(`${(baseUrl ?? '').replace(/\/+$/, '')}${LATENCY_PROBE_PATH}`, {
          method: 'GET',
          headers: { Accept: 'application/json' },
          signal: controller.signal
        })
        // 404 也算"通了"：那说明服务端是**还没有这个探针接口的老版本**，但它的路由器立刻就回了
        // 404 —— 这一趟往返的耗时同样是真的。按"非 2xx 即没连上"处理的话，版本不同步时界面会把
        // 一台好端端的服务端显示成红色，反而更难排查。
        if (!response.ok && response.status !== 404) throw new Error(`ping ${response.status}`)
        // 必须把响应体读掉：只拿到响应头就返回，测到的是"拿到首字节"而不是一次完整往返。
        await response.arrayBuffer()
        return Math.round(performance.now() - startedAt)
      } finally {
        clearTimeout(timer)
      }
    }
  })
  return { latencyMs: query.data, down: query.isError }
}
