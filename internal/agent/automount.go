package agent

import (
	"context"
	"sync"
)

// autoMountConcurrency 是自动挂载的并发上限（避免同时连接过多 iSCSI 目标）。
const autoMountConcurrency = 2

// restoreMounts 依据本地状态文件中记录的分配，尽力恢复挂载（并发上限 2）。
//
// 注意语义：服务端没有「本机应挂载哪些分配」的清单接口，因此 auto_mount 只能恢复
// 本代理上次运行留下的记录（这也是服务端重连后恢复挂载的可行路径）。
// 单个失败只记录状态并通过 SSE 通知，不阻塞其他分配。
func (a *Agent) restoreMounts(ctx context.Context) {
	if !a.cfg.Get().AutoMount {
		return
	}
	pending := a.store.ListMounts()
	if len(pending) == 0 {
		return
	}

	a.logger.Info("开始自动挂载本地记录的分配", "count", len(pending))
	sem := make(chan struct{}, autoMountConcurrency)
	var wg sync.WaitGroup
	for i := range pending {
		item := pending[i]

		// 卸载中途代理退出（state=unmounting）：用户的最后意图是**卸载**，补完它
		// （幂等清理后删除记录），绝不重挂 —— 否则一条死记录每次启动都被复活。
		if item.State == MountStateUnmounting {
			wg.Add(1)
			safeGo(a.logger, "automount_finish_unmount", func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				if err := a.engine.unmount(ctx, item.AllocationID, true); err != nil {
					a.logger.Warn("补完未完成的卸载失败", "allocation_id", item.AllocationID, "error", err)
				}
			})
			continue
		}
		// 错误/被踢记录是诊断残留（通常是清理失败的现场，比如服务端已重装、目标不存在）。
		// 自动重挂只会每次启动都失败一遍、刷新最后错误（真实反馈："重装服务端都没用"）；
		// 留给用户手动处理（卸载 / 重新挂载）。
		if item.State == MountStateError || item.State == MountStateRevoked {
			continue
		}

		wg.Add(1)
		safeGo(a.logger, "automount_restore", func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			req := MountRequest{
				AllocationID: item.AllocationID,
				MountMode:    item.MountMode,
				MountPath:    item.MountPath,
				RepoID:       item.RepoID,
				RepoName:     item.RepoName,
			}
			if _, err := a.engine.restore(ctx, req); err != nil {
				a.logger.Warn("自动挂载失败", "allocation_id", item.AllocationID, "error", err)
				a.markMountError(item.AllocationID, "automount", err)
			}
		})
	}
	wg.Wait()
}

// markMountError 把挂载置为 error 状态并推送本地事件。
func (a *Agent) markMountError(allocationID, stage string, err error) {
	var out *MountState
	a.store.UpdateMount(allocationID, func(m *MountState, _ *mountRuntime) {
		m.State = MountStateError
		m.LastError = describeError(stage, err)
		m.LastErrorDetail = mountErrorDetailOf(err)
		cp := *m
		out = &cp
	})
	if out != nil {
		a.publishMount(out)
	}
}
