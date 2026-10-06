package agent

import (
	"context"
	"strings"
	"sync"
)

// autoMountConcurrency 是自动挂载的并发上限（避免同时连接过多 iSCSI 目标）。
const autoMountConcurrency = 2

// restoreMounts 在拿到服务端会话后尽力恢复挂载，分两件事：
//
//  1. 全局 auto_mount 打开时，恢复本机状态文件里留下的挂载记录（见 restoreRecordedMounts）；
//  2. 配置里**显式**为某个库打开"启动后自动挂载"的，把它挂上（见 mountConfiguredRepos）——
//     这一项每个库独立，不受全局开关约束（全局开关的语义只是"恢复上次的挂载"）。
//
// 单个失败只记录状态并通过 SSE 通知，不阻塞其他分配。
func (a *Agent) restoreMounts(ctx context.Context) {
	cfg := a.cfg.Get()
	if cfg.AutoMount {
		a.restoreRecordedMounts(ctx, cfg.RepoMounts)
	}
	a.mountConfiguredRepos(ctx, cfg.RepoMounts)
}

// restoreRecordedMounts 依据本地状态文件中记录的分配，尽力恢复挂载（并发上限 2）。
//
// 注意语义：服务端没有「本机应挂载哪些分配」的清单接口，因此这里只能恢复本代理上次运行
// 留下的记录（这也是服务端重连后恢复挂载的可行路径）；"从来没挂过的库"由 per-repo 配置
// 负责（见 mountConfiguredRepos）。
func (a *Agent) restoreRecordedMounts(ctx context.Context, prefs map[string]RepoMountPref) {
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
		// 这条与自动挂载开关无关：它是"把没收尾的动作做完"。
		if item.State == MountStateUnmounting {
			wg.Add(1)
			safeGo(a.logger, "automount_finish_unmount", func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				if err := a.engine.unmount(ctx, item.AllocationID); err != nil {
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
		// 用户在库级配置里明确关掉了"启动后自动挂载"：不恢复这条记录（记录留着，
		// 界面仍能看到这个库的状态，等用户手动挂载或卸载）。
		req, ok := recordedMountRequest(item, prefs)
		if !ok {
			continue
		}

		wg.Add(1)
		safeGo(a.logger, "automount_restore", func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			if _, err := a.engine.restore(ctx, req); err != nil {
				a.logger.Warn("自动挂载失败", "allocation_id", item.AllocationID, "error", err)
				a.markMountError(item.AllocationID, "automount", err)
			}
		})
	}
	wg.Wait()
}

// recordedMountRequest 决定一条本地挂载记录在本次启动时的处置：
//   - ok=false：用户在库级配置里明确关掉了该库的自动挂载，这条记录不恢复；
//   - ok=true：返回可直接交给 mountEngine.restore 的请求，其中库级配置优先于记录里的旧值
//     （记录里的形态/目录是上次用的，用户在配置里改过就该以配置为准）。
func recordedMountRequest(item MountState, prefs map[string]RepoMountPref) (MountRequest, bool) {
	req := MountRequest{
		AllocationID: item.AllocationID,
		MountMode:    item.MountMode,
		MountPath:    item.MountPath,
		RepoID:       item.RepoID,
		RepoName:     item.RepoName,
	}
	pref, hasPref := prefs[item.RepoID]
	if !hasPref {
		return req, true
	}
	if !pref.AutoMount {
		return MountRequest{}, false
	}
	// 配置里没表态的字段（空串）沿用记录，避免把明确的形态/目录改回默认值。
	if pref.MountMode != "" {
		req.MountMode = pref.MountMode
	}
	if pref.MountDir != "" {
		req.MountPath = pref.MountDir
	}
	return req, true
}

// mountConfiguredRepos 挂载配置里显式打开"启动后自动挂载"的库（每库独立）。
//
// 与恢复记录不同，这些库在本机可能从来没挂过（没有本地记录、甚至没有分配），
// 所以要先向服务端问出"我在这个库里的分配"，没有就建一个（见 ensureMyAllocation）。
func (a *Agent) mountConfiguredRepos(ctx context.Context, prefs map[string]RepoMountPref) {
	if len(prefs) == 0 {
		return
	}
	userID := strings.TrimSpace(a.store.User().ID)
	if userID == "" {
		return
	}

	// 已经有本地记录的库交给 restoreRecordedMounts（那边会用同一份配置），这里只补没有记录的。
	recorded := make(map[string]struct{})
	for _, m := range a.store.ListMounts() {
		if m.RepoID != "" {
			recorded[m.RepoID] = struct{}{}
		}
	}
	type target struct {
		repoID string
		pref   RepoMountPref
	}
	todo := make([]target, 0, len(prefs))
	for repoID, pref := range prefs {
		if !pref.AutoMount {
			continue
		}
		if _, ok := recorded[repoID]; ok {
			continue
		}
		todo = append(todo, target{repoID: repoID, pref: pref})
	}
	if len(todo) == 0 {
		return
	}

	client, err := a.serverClient()
	if err != nil {
		a.logger.Warn("自动挂载配置指定的存储库失败：本地没有可用会话", "error", err)
		return
	}
	// 库名只用于卷标与目录命名；查不到也照挂（目录名退化为分配 ID，见 resolveMountDir）。
	names, err := a.repoNames(ctx, client)
	if err != nil {
		a.logger.Warn("自动挂载：查询存储库列表失败（将用分配 ID 作为目录名）", "error", err)
	}

	a.logger.Info("开始自动挂载配置指定的存储库", "count", len(todo))
	sem := make(chan struct{}, autoMountConcurrency)
	var wg sync.WaitGroup
	for i := range todo {
		item := todo[i]
		wg.Add(1)
		safeGo(a.logger, "automount_repo", func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			req, err := a.repoMountRequest(ctx, client, item.repoID, item.pref, userID)
			if err != nil {
				a.logger.Warn("自动挂载：准备分配失败", "repo_id", item.repoID, "error", err)
				return
			}
			req.RepoName = names[item.repoID]
			if _, err := a.engine.restore(ctx, req); err != nil {
				a.logger.Warn("自动挂载失败", "repo_id", item.repoID, "allocation_id", req.AllocationID, "error", err)
				a.markMountError(req.AllocationID, "automount", err)
			}
		})
	}
	wg.Wait()
}

// repoNames 返回"存储库 ID -> 名称"（仅当前会话可见的库）。
func (a *Agent) repoNames(ctx context.Context, client *serverClient) (map[string]string, error) {
	repos, err := client.ListRepos(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(repos))
	for _, repo := range repos {
		out[repo.ID] = repo.Name
	}
	return out, nil
}

// repoMountRequest 组出自动挂载某个库所需的挂载参数（先确保本人在该库里有可用分配）。
func (a *Agent) repoMountRequest(ctx context.Context, client *serverClient, repoID string, pref RepoMountPref, userID string) (MountRequest, error) {
	allocID, err := a.ensureMyAllocation(ctx, client, repoID, userID)
	if err != nil {
		return MountRequest{}, err
	}
	return MountRequest{
		AllocationID: allocID,
		MountMode:    pref.MountMode,
		MountPath:    pref.MountDir,
		RepoID:       repoID,
	}, nil
}

// ensureMyAllocation 返回我在该库里的可用分配，必要时先建一个。
//
// 服务端按库返回**全部**分配（含已释放的历史），所以这里必须自己筛：
// 只认"我自己的、且 state != released"的记录。同一个用户在同一库里的可用分配应当只有一个，
// 取第一个即可（重复分配是服务端异常，挂上其中一个也符合用户预期）。
func (a *Agent) ensureMyAllocation(ctx context.Context, client *serverClient, repoID, userID string) (string, error) {
	allocs, err := client.ListAllocations(ctx, repoID)
	if err != nil {
		return "", err
	}
	for _, item := range allocs {
		if item.UserID != userID || item.State == allocationStateReleased {
			continue
		}
		if id := strings.TrimSpace(item.ID); id != "" {
			return id, nil
		}
	}
	created, err := client.Allocate(ctx, repoID, userID)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(created.ID), nil
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
