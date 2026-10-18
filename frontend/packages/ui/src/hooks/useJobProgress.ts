import { useEffect, useRef, useState } from 'react'

import { useApi } from '../api/provider'
import type { JobState } from '../api/types'
import { useAgentJob } from './useAgent'

/**
 * 任务是否已到终态（与 JobList 的状态集合一致）。
 *
 * pending / running 之外都算终态。判据必须**由终态而非次数/时长**来定：
 * 装包、建库的耗时取决于软件源与数据量，用"最多等 N 次"会把还在正常跑的任务判成失败。
 */
export function isJobTerminal(state: JobState | undefined): boolean {
  return state === 'succeeded' || state === 'failed' || state === 'cancelled'
}

/**
 * 事件沉默多久才兜底查一次（毫秒）。
 *
 * 与代理侧同口径（internal/agent 的 uploadJobFallbackInterval）：进度是**变化驱动**的，
 * 正常情况下一条查询都不发；只有在事件真的断了（代理与服务端失联、代理在跑旧版本、
 * 服务端不发 job 事件）时，才靠它把界面从"卡住"里救回来。
 */
const JOB_SILENCE_MS = 30_000

/** 任务进度的最小形状（事件与 GET /v1/jobs/{id} 都有）。 */
export interface JobProgress {
  state: JobState
  progress: number
}

/**
 * 跟踪一个服务端任务的进度：**进度来自事件推送**，只在事件沉默过久时兜底查一次。
 *
 * 为什么不用轮询：装包、建库这类任务动辄几十秒到几十分钟，界面过去每 1.5s 查一次
 * `GET /v1/jobs/{id}`，服务端访问日志被同一件事的流水刷满（真实反馈："上传时会因为客户端
 * 拉上传状态而刷屏服务日志"）。服务端本来就在推进度事件，代理把它经本地事件流
 * （`event: job`）转发过来，没有变化就一条请求都不发。
 *
 * 兜底为什么不能省：
 *   ① 事件是"变化驱动"的，进入页面时那一条可能早就推过（甚至从没推过），不查就可能一直
 *      停在初始值上；
 *   ② 投递是尽力而为的（代理缓冲满会丢、断链期间全丢），没有兜底就会"卡着不动且不报错"。
 *
 * @param jobId   任务 ID；为空表示当前没有在跟的任务。
 * @param initial 已知的初始快照（如提交任务时 202 响应里带回的 JobDTO）——有它就不必为
 *                "进入时查一次"额外发请求。
 */
export function useJobProgress(jobId: string | undefined, initial?: JobProgress | null): JobProgress | null {
  const api = useApi()
  const live = useAgentJob(jobId)
  const [fetched, setFetched] = useState<JobProgress | null>(null)
  // 最近一次拿到数据（事件或查询）的时刻：兜底只在沉默时触发。
  const lastActivityAt = useRef(0)

  useEffect(() => {
    if (!live) return
    lastActivityAt.current = Date.now()
    // 事件一定比兜底查询新：收到事件就把兜底值丢掉，避免界面在两个来源之间来回跳。
    setFetched(null)
  }, [live])

  const current: JobProgress | null = live
    ? { state: live.state, progress: live.progress }
    : (fetched ?? initial ?? null)
  const settled = isJobTerminal(current?.state ?? undefined)

  useEffect(() => {
    if (!jobId || settled) return
    lastActivityAt.current = Date.now()
    const timer = window.setInterval(() => {
      if (Date.now() - lastActivityAt.current < JOB_SILENCE_MS) return
      lastActivityAt.current = Date.now()
      void api
        .getJob(jobId)
        .then((job) => setFetched({ state: job.state, progress: job.progress }))
        // 兜底失败不改判定：保留上一次的显示，等下一次（或事件）再说。
        .catch(() => undefined)
    }, JOB_SILENCE_MS)
    return () => window.clearInterval(timer)
  }, [api, jobId, settled])

  return current
}
