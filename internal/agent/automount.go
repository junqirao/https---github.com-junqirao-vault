package agent

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// autoMountConcurrency 是自动挂载的并发上限（避免同时连接过多 iSCSI 目标）。
const autoMountConcurrency = 2

// autoMountWaitTimeout 是"等服务端连上"的上限：连上之前一律不挂载（见 restoreMountsWhenConnected）。
//
// 取 60s 的依据：拿到会话后的首次 SystemInfo 最长 10s，事件流建立与心跳确认都在秒级，
// 留出充裕余量即可；到期仍连不上就整轮跳过 —— 无会话时的自动重连循环每 30s 会再登录一次，
// 下一次会话建立时自然会重新走一遍，不需要在这里无限等。
const autoMountWaitTimeout = 60 * time.Second

// autoMountWaitInterval 是等待连接期间的轮询间隔（连接状态由事件流/心跳/SystemInfo 三处写入，
// 这里只是低频看一眼，代价可忽略）。
const autoMountWaitInterval = 500 * time.Millisecond

// manualUnmountGuard 记住"本次运行期间被用户手动卸载过"的存储库：这些库不再参与自动挂载。
//
// 为什么需要它（真实反馈："卸载设置了自动挂载的存储库，立马又自己挂回来了"）：
// 卸载成功会删掉本地挂载记录（见 mountEngine.unmountLocked 末尾），而每库配置里的
// auto_mount 还开着 —— 只要再发生一次"拿到服务端会话"（客户端推会话、证书免密登录、
// 令牌定时续期都会走 setSession → onSessionEstablished），restoreMounts 就会把它当成
// "从来没挂过、但用户要求自动挂载的库"重新挂上，用户的卸载动作等于白点。
//
// 用户的这一次点击是最明确的意图，必须压过配置文件：
//
//   - 只挡**自动**挂载。用户手动点"挂载"照常能挂（那是他自己的动作）；
//   - 只活在内存里。代理进程重启即清空 —— 客户端退出会连代理一起结束
//     （见 apps/client/electron/agent.ts 的拉起与回收），因此语义正好是
//     "客户端内当次不再自动挂载，直到下一次启动"；
//   - 重启后自动挂载恢复生效：用户上次的手动卸载已经落盘（记录被删），下次启动想挂回来
//     就得靠自己再挂一遍，或者点一次"挂载"（配置里的 auto_mount 依然为真）。
type manualUnmountGuard struct {
	mu    sync.Mutex
	repos map[string]struct{}
}

// block 记下一次"用户手动卸载了某个库"。库 ID 为空（记录里没有库信息）时忽略：按库封禁
// 的前提是知道是哪个库，记不下就没有语义。
func (g *manualUnmountGuard) block(repoID string) {
	repoID = strings.TrimSpace(repoID)
	if repoID == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.repos == nil {
		g.repos = make(map[string]struct{})
	}
	g.repos[repoID] = struct{}{}
}

// blocked 判断该库是否已被手动卸载过（本次运行内不再自动挂载）。
func (g *manualUnmountGuard) blocked(repoID string) bool {
	repoID = strings.TrimSpace(repoID)
	if repoID == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.repos[repoID]
	return ok
}

// restoreMountsWhenConnected 是自动挂载的**唯一入口**：先确认服务端已连上，再执行挂载。
//
// 为什么必须先确认（真实反馈："自动挂载要连接到服务器之后才会执行，否则会报错"）：
// 拿到会话（setSession）只代表客户端把令牌交给了代理，不代表服务端真的可达 —— 地址不通、
// 服务端还没起来、证书没被信任时 SystemInfo 就会失败。这时去挂载只会得到一串失败；而按设计，
// "启动时的自动挂载失败要**保留记录**"（本地记录代表用户希望它保持挂载的意图，见
// docs/implementation.md 5.5），这些错误记录会长在界面上，并且每次启动再失败一遍。
//
// 因此这里阻塞等到"服务端已确认可达"（Connected=true）再挂。三条确认路径都算数：
// SystemInfo 成功（updateServerFromInfo）、事件流建立、心跳成功（setServerConnected）。
// 等不到就整轮跳过，等下一次会话建立（客户端推会话 / 证书免密登录 / 令牌续期 / 手动重连）重来。
func (a *Agent) restoreMountsWhenConnected(ctx context.Context, key string) {
	session, ok := a.store.Session(key)
	if !ok {
		return
	}

	waitCtx, cancel := context.WithTimeout(ctx, autoMountWaitTimeout)
	defer cancel()
	if !a.waitServerConnected(waitCtx, key) {
		a.logger.Warn("等待服务端连接超时，本次不执行自动挂载", "server", key)
		return
	}

	// 等待期间会话可能被换掉（重新登录 / 退出登录）：那属于另一次会话的意图，
	// 旧任务不能拿着它动手，否则会被上一份配置插一脚。
	if !sameSession(a.store, key, session) {
		a.logger.Info("等待期间会话已更换，跳过本次自动挂载", "server", key)
		return
	}
	a.restoreMounts(ctx, key)
}

// waitServerConnected 轮询**某台**"服务端已确认可达"：就绪返回 true，ctx 结束返回 false。
//
// 先同步查一次：绝大多数情况下客户端推会话时服务端就是通的（SystemInfo 已在同一条链路上
// 成功过），这里零延迟返回 —— 不给正常启动路径平白加一次等待。
func (a *Agent) waitServerConnected(ctx context.Context, key string) bool {
	connected := func() bool {
		state, ok := a.store.Server(key)
		return ok && state.Connected
	}
	if connected() {
		return true
	}
	ticker := time.NewTicker(autoMountWaitInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if connected() {
				return true
			}
		}
	}
}

// sameSession 判断**某台**的会话是否仍是等待开始时的那一个（服务端地址与令牌都相同才算同一个）。
//
// 只比地址不够：在同一台服务端上重新登录会换令牌，那同样是"会话已更换"。
func sameSession(store *stateStore, key string, want *Session) bool {
	got, ok := store.Session(key)
	if !ok || got == nil {
		return false
	}
	return got.ServerURL == want.ServerURL && got.Token == want.Token
}

// restoreMounts 在服务端确认连上后尽力恢复挂载，分三件事：
//
//  1. 全局 auto_mount 打开时，恢复本机状态文件里留下的挂载记录（见 restoreRecordedMounts）；
//  2. 配置里**显式**为某个库打开"启动后自动挂载"的，把它挂上（见 mountConfiguredRepos）——
//     这一项每个库独立，不受全局开关约束（全局开关的语义只是"恢复上次的挂载"）；
//  3. 上面两条都要让开"用户本次运行手动卸载过"的库（见 manualUnmountGuard）。
//
// 单个失败只记录状态并通过 SSE 通知，不阻塞其他分配。
func (a *Agent) restoreMounts(ctx context.Context, key string) {
	cfg := a.cfg.Get()
	if cfg.AutoMount {
		a.restoreRecordedMounts(ctx, key, cfg.RepoMounts)
	}
	a.mountConfiguredRepos(ctx, key, cfg.RepoMounts)
}

// restoreRecordedMounts 依据本地状态文件中记录的分配，尽力恢复挂载（并发上限 2）。
//
// 注意语义：服务端没有「本机应挂载哪些分配」的清单接口，因此这里只能恢复本代理上次运行
// 留下的记录（这也是服务端重连后恢复挂载的可行路径）；"从来没挂过的库"由 per-repo 配置
// 负责（见 mountConfiguredRepos）。
func (a *Agent) restoreRecordedMounts(ctx context.Context, key string, prefs map[string]RepoMountPref) {
	// 只恢复**归属这台服务端**的记录：多服务端下每台各自恢复自己的，互不代劳
	// （拿 A 的分配 ID 去 B 上恢复必然失败）。
	pending := a.store.MountsForServer(key)
	if len(pending) == 0 {
		return
	}

	a.logger.Info("开始自动挂载本地记录的分配", "server", key, "count", len(pending))
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
		// 本次运行被用户手动卸载过：不留、不恢复。卸载成功已经删掉记录，正常走不到这里，
		// 兜住的是"记录因卸载失败残留"（比如卸载中断后状态被写回）这种边角情况。
		if a.manualUnmounts.blocked(item.RepoID) {
			a.logger.Info("该库本次运行已被手动卸载，跳过自动挂载",
				"repo_id", item.RepoID, "allocation_id", item.AllocationID)
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
		// 记录里的归属带回去：恢复必须打在**当初受理这次挂载的那台**上。
		ServerKey:  item.ServerKey,
		ServerURL:  item.ServerURL,
		ServerName: item.ServerName,
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
//
// ⚠️ 这里正是"手动卸载后马上又被挂回来"的入口：卸载删了记录，于是这个库看起来就是
// "从来没挂过、但配置要求自动挂载"，任何一次会话建立（含令牌续期）都会重新挂上。
// 因此必须先让开"用户本次运行手动卸载过"的库（见 manualUnmountGuard）。
func (a *Agent) mountConfiguredRepos(ctx context.Context, key string, prefs map[string]RepoMountPref) {
	if len(prefs) == 0 {
		return
	}
	userID := strings.TrimSpace(a.store.User(key).ID)
	if userID == "" {
		return
	}

	// 已经有本地记录的库交给 restoreRecordedMounts（那边会用同一份配置），这里只补没有记录的。
	// 记录也只看本台的：别的服务端的库不该挡住这台的同 ID 库（库 ID 全局唯一，但仍按台取更直观）。
	recorded := make(map[string]struct{})
	for _, m := range a.store.MountsForServer(key) {
		if m.RepoID != "" {
			recorded[m.RepoID] = struct{}{}
		}
	}
	todo := configuredAutoMountTargets(prefs, recorded, a.manualUnmounts.blocked)
	if len(todo) == 0 {
		return
	}

	client, err := a.serverClientFor(key)
	if err != nil {
		a.logger.Warn("自动挂载配置指定的存储库失败：本地没有该服务端的会话", "server", key, "error", err)
		return
	}
	// 库名用于卷标与目录命名；同时它还是"这个库是不是这台的"的判据（见下）。
	names, err := a.repoNames(ctx, client)
	if err != nil {
		// 多服务端的关键一步：库级配置是按**库 ID** 存的，而库 ID 只对"它所属的那台服务端"
		// 有意义。拿 A 的库 ID 去 B 上建分配只会得到一串失败记录，所以查不到列表就不再往下走
		// （连不上/无权限时也一样：宁可这一轮不挂，也不要在错的台子上瞎试）。
		a.logger.Warn("自动挂载：查询存储库列表失败，本台跳过自动挂载", "server", key, "error", err)
		return
	}
	todo = filterReposOnServer(todo, names)
	if len(todo) == 0 {
		return
	}

	a.logger.Info("开始自动挂载配置指定的存储库", "server", key, "count", len(todo))
	sem := make(chan struct{}, autoMountConcurrency)
	var wg sync.WaitGroup
	for i := range todo {
		item := todo[i]
		wg.Add(1)
		safeGo(a.logger, "automount_repo", func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			req, err := a.repoMountRequest(ctx, client, key, item.repoID, item.pref, userID)
			if err != nil {
				a.logger.Warn("自动挂载：准备分配失败", "server", key, "repo_id", item.repoID, "error", err)
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

// autoMountTarget 是一个"本次运行该自动挂载的库"及其配置。
type autoMountTarget struct {
	repoID string
	pref   RepoMountPref
}

// configuredAutoMountTargets 从每库配置里挑出**本次运行该自动挂载**的库（顺序无关，按库 ID 排序稳定输出）。
//
// 三条排除规则：
//   - 配置里 auto_mount=false：用户明确关掉了这个库的自动挂载；
//   - 本机已有记录（recorded）：交给 restoreRecordedMounts，这里只补"从来没挂过"的；
//   - blocked 判定为真（用户本次运行手动卸载过）：配置压不过用户的那一次点击
//     （见 manualUnmountGuard，这就是"卸载后马上又被挂回来"的修复点）。
//
// 抽成纯函数是因为它是"该不该自动挂"的唯一判据：策略被单独测试，调用方只负责执行。
func configuredAutoMountTargets(
	prefs map[string]RepoMountPref,
	recorded map[string]struct{},
	blocked func(repoID string) bool,
) []autoMountTarget {
	out := make([]autoMountTarget, 0, len(prefs))
	for repoID, pref := range prefs {
		if !pref.AutoMount {
			continue
		}
		if _, ok := recorded[repoID]; ok {
			continue
		}
		if blocked(repoID) {
			continue
		}
		out = append(out, autoMountTarget{repoID: repoID, pref: pref})
	}
	// 稳定的遍历顺序：日志与测试输出不会因 map 迭代顺序而变。
	sort.Slice(out, func(i, j int) bool { return out[i].repoID < out[j].repoID })
	return out
}

// filterReposOnServer 只保留"确实存在于该服务端"的自动挂载目标。
//
// 依据是服务端返回的库列表：库 ID 由服务端生成，只有它认得自己的库。库级自动挂载配置是本地
// 按库 ID 存的，多服务端下必须这一步过滤，否则会拿 A 的库 ID 去 B 上建分配（必然失败）。
func filterReposOnServer(targets []autoMountTarget, names map[string]string) []autoMountTarget {
	out := make([]autoMountTarget, 0, len(targets))
	for _, item := range targets {
		if _, ok := names[item.repoID]; ok {
			out = append(out, item)
		}
	}
	return out
}

// repoNames 返回"存储库 ID -> 名称"（仅该服务端可见的库）。
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
func (a *Agent) repoMountRequest(ctx context.Context, client *serverClient, key, repoID string, pref RepoMountPref, userID string) (MountRequest, error) {
	allocID, err := a.ensureMyAllocation(ctx, client, repoID, userID)
	if err != nil {
		return MountRequest{}, err
	}
	return MountRequest{
		AllocationID: allocID,
		MountMode:    pref.MountMode,
		MountPath:    pref.MountDir,
		RepoID:       repoID,
		// 归属写进请求：挂载记录据此把心跳/回写/释放都发给这台。
		ServerKey: key,
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
