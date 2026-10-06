package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"vault/internal/apperr"
	"vault/internal/platform/iscsiinitiator"
)

// 挂载模式。
const (
	// mountModeLetter 盘符模式。
	mountModeLetter = "letter"
	// mountModeDirectory 目录模式。
	mountModeDirectory = "directory"
)

// 挂载管道的时间常量（见 docs/implementation.md 5.5）。
const (
	// connectWaitTimeout 等待 iSCSI 会话建立的超时（60s）。
	connectWaitTimeout = 60 * time.Second
	// postScriptTimeout 挂载后置脚本执行超时（60s）。
	postScriptTimeout = 60 * time.Second
	// labelTimeout 写入卷标的超时。卷标只是"好看"（资源管理器里显示库名），
	// 失败或超时都不影响挂载本身。
	labelTimeout = 30 * time.Second
	// volumeLabelMaxRunes NTFS 卷标最长 32 个字符（超出会被系统截断，这里主动截断以保持一致）。
	volumeLabelMaxRunes = 32
	// unmountDisconnectRetries 卸载时断开 iSCSI 会话的重试次数（仅在 0xefff0040 时重试）。
	unmountDisconnectRetries = 3
	// unmountDisconnectBackoff 每次重试前的等待。
	//
	// 0xefff0040 的含义是"会话上仍有在线设备"，最常见的成因是 Set-Disk -IsOffline 还没真正
	// 生效 —— 立刻重试会拿到同一个错误，所以要留一点时间给它生效。
	unmountDisconnectBackoff = 2 * time.Second
)

// MountRequest 是本地挂载请求（POST /agent/mount）。
type MountRequest struct {
	// AllocationID 分配 ID（必填）。
	AllocationID string `json:"allocation_id"`
	// MountMode letter | directory，缺省时按服务端下发值 / 本地默认值决定。
	MountMode string `json:"mount_mode"`
	// MountPath directory 模式的目标目录（绝对路径或相对挂载根的子路径）。
	MountPath string `json:"mount_path"`
	// RepoID / RepoName 可选：服务端 MountSpec 不含存储库信息，前端可顺带带上用于展示与变量注入。
	RepoID   string `json:"repo_id"`
	RepoName string `json:"repo_name"`
}

// mountEngine 实现挂载/卸载状态机（含幂等与串行化）。
type mountEngine struct {
	a *Agent
}

// mount 执行完整挂载流程。
//
// 流程（见 docs/implementation.md 5.5）：
//
//	取 MountSpec → EnsurePortal → Connect（含 CHAP）→ WaitConnected →
//	磁盘上线/去只读 → 分配盘符或目录 → 执行 post_script（变量注入）→ 回写 mounted。
//
// 同一 allocation 全程串行（lock.Keyed）；已挂载时直接返回既有状态（幂等）。
//
// keepRecordOnFailure 决定失败后是否保留一条 error 记录：
//   - false（用户主动挂载 / 重新挂载）：清理这次尝试并**不留记录** —— 失败的尝试不该在挂载
//     列表与库卡片上留一条永久"错误"（真实反馈："都错误了就不要有挂载记录了"）；失败原因由
//     错误响应当场带回，界面用红色感叹号展示一次。
//   - true（启动时自动挂载）：本地记录代表"用户希望这个分配保持挂载"的**意图**，抹掉它等于
//     以后再也不自动挂回；因此保留记录（状态 error），下次启动仍会重试。
func (e *mountEngine) mount(ctx context.Context, req MountRequest, keepRecordOnFailure bool) (*MountState, error) {
	allocationID := strings.TrimSpace(req.AllocationID)
	if allocationID == "" {
		return nil, apperr.InvalidParam("allocation_id")
	}

	unlock := e.a.locks.Lock("alloc:" + allocationID)
	defer unlock()

	// 幂等：同一 allocation 已挂载、且**实测本机确实还有活动会话**时，直接返回既有状态。
	//
	// 判据必须带上实测会话，只看记录会撒谎（真实诉求："挂载和卸载的按钮应以实际为准，
	// 看对应的 iSCSI 连接是否在活动中的"）：会话被断之后记录仍是 mounted，直接返回
	// 等于告诉用户"已挂载"，而盘根本不在 —— 用户既看不到盘、也拿不到任何解释。
	// 此时应当照常走完整流程去真重连（幂等步骤会复用门户/会话，服务端也复用租约）。
	//
	// 尚未核对（刚启动，session_checked_at=0）与实测失败都**不**算命中：宁可多做一次
	// 幂等的挂载，也不谎报"已经挂好了"。
	if existing, _, ok := e.a.store.GetMount(allocationID); ok && existing.State == MountStateMounted {
		active, verified := e.a.probeMountSession(ctx, allocationID)
		if mountIdempotent(existing, active, verified) {
			if latest, _, ok := e.a.store.GetMount(allocationID); ok {
				return &latest, nil
			}
			return &existing, nil
		}
		e.a.logger.Info("记录是已挂载但本机已无活动会话，按实际未挂载重新挂载",
			"allocation_id", allocationID, "target_iqn", existing.TargetIQN)
	}
	client, err := e.a.serverClient()
	if err != nil {
		return nil, err
	}

	// 先占位一条"挂载中"再调用服务端。
	//
	// 为什么必须占位：服务端在这条路径上是**同步重活** —— 等差异盘 VHDX 就绪（最多 15s），
	// 再用 PowerShell 全量下发 iSCSI 目标（实测约 15s），全部发生在响应返回之前。
	// 以前状态在服务端返回后才写入，界面这几十秒里只有一个转圈的按钮、连"挂载中"都不显示
	// （真实反馈：希望看到"正在创建虚拟磁盘 / 正在创建 iSCSI 目标"）。占位后界面立刻有反馈，
	// 服务端推来的阶段事件（SSE event: mount）也才有状态可更新。
	placeholder := &MountState{
		RepoID:       strings.TrimSpace(req.RepoID),
		RepoName:     strings.TrimSpace(req.RepoName),
		AllocationID: allocationID,
		State:        MountStateMounting,
		Phase:        MountPhaseRequesting,
	}
	e.a.store.PutMount(placeholder, &mountRuntime{})
	e.a.publishMount(placeholder)

	// 再取 MountSpec（需要服务端会话与连通性）：
	// 错误优先级为 no_session > server_unreachable > admin_required，
	// 与 docs/agent-api.md 的错误约定保持一致。
	spec, err := client.RequestMount(ctx, allocationID, e.a.store.ClientID())
	if err != nil {
		e.discardPlaceholder(allocationID)
		return nil, err
	}

	// 管理员权限只在真正执行本地操作前校验（此前不产生任何本地副作用）。
	if !e.a.admin {
		e.discardPlaceholder(allocationID)
		return nil, errAdminRequired()
	}

	mode := e.resolveMode(req, spec)
	ms := &MountState{
		RepoID: strings.TrimSpace(req.RepoID),
		// 库名同时决定盘符模式的卷标与目录模式的目录名（见 mountAt）。
		// 请求里没有时用服务端下发值兜底：自动重挂/重装后恢复也能拿到库名。
		RepoName:     repoNameOf(req, spec),
		AllocationID: allocationID,
		LeaseID:      spec.LeaseID,
		TargetIQN:    spec.TargetIQN,
		Portal:       net.JoinHostPort(spec.PortalAddress, strconv.Itoa(portalPort(spec))),
		MountMode:    mode,
		State:        MountStateMounting,
		// 服务端阶段（准备磁盘/下发目标）已经走完，接下来是本机的会话建立与磁盘上线。
		Phase: MountPhaseConnecting,
	}
	rt := &mountRuntime{
		PortalAddress:    spec.PortalAddress,
		PortalPort:       portalPort(spec),
		AuthMode:         spec.AuthMode,
		ChapUser:         spec.ChapUser,
		ChapSecret:       spec.ChapSecret,
		PostScript:       spec.PostScript,
		HeartbeatSeconds: spec.HeartbeatSeconds,
		LeaseTTLSeconds:  spec.LeaseTTLSeconds,
	}
	e.a.store.PutMount(ms, rt)
	e.a.publishMount(ms)

	// failWithDetail 处理挂载失败：**失败的尝试不留记录**，失败原因由错误响应当场带回。
	//
	// 为什么不留记录（真实反馈："都错误了就不要有挂载记录了"）：
	//   - 一条 error 记录会永久挂在挂载列表与库卡片上（更糟的是它会写进状态文件，跨重启存活，
	//     每次启动 automount 还会去"还原"它，于是永远消不掉）；
	//   - 记录的唯一用途是给用户一个"卸载残留"的入口 —— 只有当机器上**真的还挂着东西**
	//     （会话/磁盘/挂载点）时才需要，而那种情况恰恰是清理失败的情况。
	// 因此：能清理干净就抹掉记录；清理不干净才保留一条 error 记录作为人工处理入口。
	failWithDetail := func(stage string, cause error, detail string, extra map[string]any) (*MountState, error) {
		e.a.logger.Error("挂载失败",
			"allocation_id", allocationID, "stage", stage, "detail", detail, "error", cause)

		// 原始报错（如 Connect-IscsiTarget 的 .NET 异常）也随错误响应带回：失败记录会被抹掉
		// （见下），若不带回，用户界面上就再也看不到真因（真实反馈："根本没法定位错误 /
		// 什么都看不到"）。CHAP 密钥绝不在这里 —— mountErrorDetailOf 只取 PowerShell 的
		// message 与 reason@step（见其说明），不含命令行里的 -ChapSecret。
		if raw := mountErrorDetailOf(cause); raw != "" {
			if extra == nil {
				extra = map[string]any{}
			}
			extra["detail"] = raw
		}

		// 自动挂载（restore）失败时既不清理也不删记录：那会销毁"用户希望它保持挂载"的意图
		// （下次启动就不会再尝试），而且启动阶段的严格卸载有可能把一个其实还能用的持久化
		// 会话拆掉 —— 宁可留一条可见的错误，也不静默改变用户机器上的磁盘状态。
		cleaned, cleanupErr := true, error(nil)
		if !keepRecordOnFailure {
			cleaned, cleanupErr = e.cleanupFailedMount(ctx, allocationID, stage)
		}
		if cleaned {
			e.a.store.DeleteMount(allocationID)
			// 用 unmount 事件让界面把这条（失败后已清理的）记录移除；渲染层按"原先是否真的
			// 挂载过"决定要不要提示"已卸载"（见 useAgent 的 unmount 分支），不会误报。
			e.a.hub.Publish(Event{Type: "unmount", Data: map[string]any{"allocation_id": allocationID}})
			return nil, errMountFailedWith(stage, cause, extra)
		}

		// 清理没成功：会话/磁盘可能还挂在本机 —— 此时**必须**保留记录，抹掉它会让残留变成
		// "看不见"，那比一条刺眼的错误记录糟得多（用户可以点"卸载"再清一次）。
		var out MountState
		if updated := e.a.store.UpdateMount(allocationID, func(m *MountState, _ *mountRuntime) {
			m.State = MountStateError
			m.Phase = ""
			m.LastError = describeError(stage, cause)
			// 原始报错（如 PowerShell 的 .NET 异常）一并记下，供本机诊断面板展示：
			// 界面上只看得到一个稳定码时用户完全无从下手（见 mountErrorDetailOf）。
			m.LastErrorDetail = joinDetail(detail, mountErrorDetailOf(cause),
				cleanupFailureDetail(cleanupErr))
			cp := *m
			out = cp
		}); updated {
			e.a.publishMount(&out)
		}
		return nil, errMountFailedWith(stage, cause, extra)
	}

	// fail 是不带额外诊断信息的失败路径。
	fail := func(stage string, cause error) (*MountState, error) {
		return failWithDetail(stage, cause, "", nil)
	}

	// failConnect 用于"连不上门户"类阶段（connect / wait_connected）：除稳定码外，把门户
	// 地址、目标 IQN、认证方式与 TCP 可达性一并打出来。
	//
	// 只有 stage=connect 时，用户与支持人员都无法判断是网络/防火墙、目标名写错还是 CHAP
	// 不对 —— 而这三种原因的处置方式完全不同（真实反馈："根本没法定位错误"）。
	failConnect := func(stage string, cause error) (*MountState, error) {
		portal := net.JoinHostPort(spec.PortalAddress, strconv.Itoa(portalPort(spec)))
		detail, tcp := connectDiagnostics(ctx, spec, portalPort(spec))
		return failWithDetail(stage, cause, detail, map[string]any{
			"portal":     portal,
			"target_iqn": spec.TargetIQN,
			"auth_mode":  clientAuthMode(spec.AuthMode),
			"tcp":        tcp,
		})
	}

	// 先确保 iSCSI 发起端服务在运行（见 5.5 步骤 ③a）：
	// 连接类 cmdlet 依赖该服务，服务未运行会让 connect 阶段以 connect_failed 收场，
	// 界面只显示"挂载失败（阶段：connect）"，真因不可见（真实工单）。
	if err := e.a.iscsi.EnsureService(ctx); err != nil {
		// 服务起不来：立刻刷新主机状态 —— 界面横幅当场亮起，不必等下次启动
		// （模块缺失还是服务被停用，探测能给出准确原因）。
		e.a.refreshHostState(ctx)
		return fail("initiator_service", err)
	}
	// 服务已确认在运行：主机状态直接置为就绪（界面横幅自动消失），不再跑一次 PowerShell。
	e.a.markISCSIServiceRunning()
	if err := e.a.iscsi.EnsurePortal(ctx, spec.PortalAddress, portalPort(spec)); err != nil {
		return fail("portal", err)
	}
	if err := e.a.iscsi.Connect(ctx, iscsiinitiator.ConnectOptions{
		TargetIQN:     spec.TargetIQN,
		PortalAddress: spec.PortalAddress,
		PortalPort:    portalPort(spec),
		AuthMode:      clientAuthMode(spec.AuthMode),
		ChapUser:      spec.ChapUser,
		ChapSecret:    spec.ChapSecret,
		// 不用 Windows 持久化目标（-IsPersistent）：它与 OneWayCHAP 明文密钥叠加时会把
		// 目标连成 "hidden from login"（真实事故：手动 Connect 去掉 -IsPersistent 后
		// 立即连上）。重启后的重连由本代理的 restoreMounts 自动重挂负责，无需依赖
		// Windows 的持久目标。
		Persistent: false,
	}); err != nil {
		return failConnect("connect", err)
	}
	if err := e.a.iscsi.WaitConnected(ctx, spec.TargetIQN, connectWaitTimeout); err != nil {
		return failConnect("wait_connected", err)
	}

	if err := e.a.vol.UpdateHostStorageCache(ctx); err != nil {
		e.a.logger.Warn("刷新存储缓存失败（继续尝试定位磁盘）", "allocation_id", allocationID, "error", err)
	}
	diskNumber, err := e.a.vol.FindIscsiDisk(ctx, spec.DiskSizeBytes, e.a.store.UsedDiskNumbers(allocationID))
	if err != nil {
		return fail("find_disk", err)
	}
	e.a.store.UpdateMount(allocationID, func(m *MountState, r *mountRuntime) {
		m.DiskNumber = diskNumber
		r.DiskNumber = diskNumber
		r.DiskKnown = true
	})

	e.setPhase(allocationID, MountPhaseOnline)
	if err := e.a.vol.SetOffline(ctx, diskNumber, false); err != nil {
		return fail("disk_online", err)
	}
	if err := e.a.vol.SetReadOnly(ctx, diskNumber, false); err != nil {
		return fail("disk_read_write", err)
	}

	e.setPhase(allocationID, MountPhaseMountPoint)
	mountPath, err := e.mountAt(ctx, diskNumber, mode, req, spec)
	if err != nil {
		return fail("mount_point", err)
	}

	// post_script 失败不阻塞挂载（见 5.5 步骤 h）。
	e.setPhase(allocationID, MountPhasePostScript)
	e.runPostScript(ctx, spec.PostScript, spec, mountPath, ms.RepoName)

	if err := client.ReportMounted(ctx, spec.LeaseID, e.a.store.ClientID(), mountPath); err != nil {
		// 回写失败不撤销挂载（磁盘已可用），仅记录告警。
		e.a.logger.Warn("回写挂载点失败（本地已挂载）", "allocation_id", allocationID, "error", err)
	}

	now := time.Now().UnixMilli()
	var out MountState
	e.a.store.UpdateMount(allocationID, func(m *MountState, r *mountRuntime) {
		m.MountPath = mountPath
		m.State = MountStateMounted
		// 挂载完成：清掉阶段（阶段只在 mounting 期间有含义）。
		m.Phase = ""
		m.MountedAt = now
		m.LastHeartbeatAt = now
		m.LastError = ""
		// 挂载成功：清掉上一次的原始报错，避免诊断面板显示已经解决了的旧故障。
		m.LastErrorDetail = ""
		// 会话刚由本流程建立成功（WaitConnected 已确认），如实记为实测活动，
		// 界面的"挂载/卸载"按钮据此判定（见 session_probe.go）。
		m.SessionActive = true
		m.SessionCheckedAt = now
		r.SessionProbeFailures = 0
		r.NextHeartbeatAt = now + r.heartbeatInterval().Milliseconds()
		cp := *m
		out = cp
	})
	e.a.publishMount(&out)
	e.a.logger.Info("挂载完成",
		"allocation_id", allocationID, "mount_path", mountPath,
		"disk_number", diskNumber, "mount_mode", mode)
	return &out, nil
}

// setPhase 推进某个挂载的阶段并广播（只在 state=mounting 时生效，且只前进不回退）。
//
// 调用方有两处：挂载引擎自身（本机各步骤），以及服务端阶段事件（SSE event: mount）。
// 服务端事件是异步到达的，可能与本机步骤交错，故用 mountPhaseRank 定序。
func (e *mountEngine) setPhase(allocationID, phase string) {
	if strings.TrimSpace(phase) == "" {
		return
	}
	var out *MountState
	e.a.store.UpdateMount(allocationID, func(m *MountState, _ *mountRuntime) {
		if m.State != MountStateMounting {
			return
		}
		if mountPhaseRank(phase) <= mountPhaseRank(m.Phase) {
			return
		}
		m.Phase = phase
		cp := *m
		out = &cp
	})
	if out != nil {
		e.a.publishMount(out)
	}
}

// discardPlaceholder 撤掉"调用服务端前"写入的占位状态。
//
// 语义：服务端没有受理这次挂载（或本机没有管理员权限）时，机器上什么都没发生，
// 界面上不该留下一条永远不动的"挂载中"—— 直接抹掉即可（失败原因由错误响应带回）。
func (e *mountEngine) discardPlaceholder(allocationID string) {
	e.a.store.DeleteMount(allocationID)
	e.a.hub.Publish(Event{Type: "unmount", Data: map[string]any{"allocation_id": allocationID}})
}

// mountStageLeavesResidue 判断该阶段的失败是否可能在本机留下残留（iSCSI 会话/上线磁盘/挂载点）。
//
// 只有"Connect 已经成功"之后的阶段才可能留下东西：
//   - connect 之前的失败（发起端服务起不来、门户不可达、目标名不存在）在本机什么都没建立，
//     清理既无必要、也徒增等待（每一步都是 PowerShell，约 1s）；
//   - 之后（等待会话建立、找盘、磁盘上线、分配盘符/目录）则可能已经连上会话、把盘上线。
func mountStageLeavesResidue(stage string) bool {
	switch stage {
	case "wait_connected", "find_disk", "disk_online", "disk_read_write", "mount_point":
		return true
	default:
		return false
	}
}

// cleanupFailedMount 清理一次失败的挂载尝试，返回"是否已清理干净"与清理错误。
//
// 无残留阶段直接返回 true（不碰任何东西）；有残留阶段调 unmountLocked 清理：
// 报"卸载失败"（只有挂载点还在、卷被占用时才会）就说明清理没做干净 ——
// 调用方据此保留一条 error 记录，供用户关掉占用后重试。
func (e *mountEngine) cleanupFailedMount(ctx context.Context, allocationID, stage string) (bool, error) {
	if !mountStageLeavesResidue(stage) {
		return true, nil
	}
	if err := e.unmountLocked(ctx, allocationID); err != nil {
		e.a.logger.Warn("失败的挂载尝试清理未完成，保留记录供人工卸载",
			"allocation_id", allocationID, "stage", stage, "error", err)
		return false, err
	}
	return true, nil
}

// cleanupFailureDetail 把清理失败的原因写成可读的一行（附在失败详情后面）。
func cleanupFailureDetail(err error) string {
	if err == nil {
		return ""
	}
	return "cleanup_failed:" + apperr.CodeOf(err)
}

// unmount 执行卸载流程 —— **只有一种卸载，没有"普通/强制"之分**。
//
// 顺序：Unmount(移除挂载点) → SetOffline(true) → Disconnect(必要时重试) → Unregister → 回写 release。
//
// ⚠️ "先移除挂载点、再下线磁盘"是刻意的，**不要颠倒回去**（真实事故）：
// Set-Disk -IsOffline 一旦生效，盘符/目录会立刻从系统里消失，此时再执行
// Remove-PartitionAccessPath 只会失败 —— 卸载就此中断，而磁盘其实已经下线。
// 用户看到的正是这个组合：点了卸载 → 盘符不见了 → 卸载却报错、状态还回到"已挂载"、
// iSCSI 发起程序里会话仍在（后面的 Disconnect 压根没执行到）。
//
// 唯一必须保持的强序是 **SetOffline 早于 Disconnect**：否则 Disconnect-IscsiTarget
// 返回 0xefff0040（会话上仍有在线设备），见 docs/implementation.md 5.5。
//
// 失败语义也只有一种（这正是"强制卸载"入口被去掉的原因）：
//
//   - 挂载点还在（盘符/目录仍属于该卷，Windows 认为卷正被占用）→ 整次卸载失败，
//     记录回滚成卸载前的状态，界面如实显示"还挂着"。用户关掉占用它的程序再点一次即可，
//     不需要一个额外的"强制卸载"按钮。
//   - 挂载点已经移除（含报错但实际已不存在）→ 本机已经"看不出还挂着东西"，
//     后面每一步（下线、断开会话、取消持久化、回写 release）都只是清残留，**一律尽力而为**：
//     任一步失败只记日志、不中断，记录照样删除。过去这些情况要靠 force 才能收尾，
//     结果就是"盘符没了、状态还写着已挂载、还得多点一次强制卸载"（真实反馈）。
//
// 断开会话仍然会以 device_in_use 补一次下线+重试，真失败时按 warn 记录：
// 服务端 release 后会停用目标把会话踢掉，且④已取消持久化，不会重启后重连。
func (e *mountEngine) unmount(ctx context.Context, allocationID string) error {
	allocationID = strings.TrimSpace(allocationID)
	if allocationID == "" {
		return apperr.InvalidParam("allocation_id")
	}

	unlock := e.a.locks.Lock("alloc:" + allocationID)
	defer unlock()

	return e.unmountLocked(ctx, allocationID)
}

// unmountLocked 是 unmount 的**不加锁**版本：调用方必须已经持有该 allocation 的锁。
//
// 存在的原因：挂载失败后的清理发生在 mount 的锁内，直接调 unmount 会在同一把 keyed 锁上
// 自锁死（挂载失败时永远卡住，比错误本身严重得多）。
func (e *mountEngine) unmountLocked(ctx context.Context, allocationID string) error {
	ms, rt, ok := e.a.store.GetMount(allocationID)
	if !ok {
		return errNotMounted(allocationID)
	}

	// 记住卸载前的状态：卸载失败、且**什么都没拆掉**时要恢复它。
	//
	// 为什么必须：状态一旦写成 unmounting 而后续步骤失败，若不回写，记录就**永远冻结在
	// "卸载中"**（真实事故：目标/磁盘早已不存在，下线或移除挂载点报错就直接 return ——
	// 界面上永远挂着一条"卸载中"，重装服务端也清不掉，因为记录在本机状态文件里）。
	prevState := ms.State

	var unmounting MountState
	e.a.store.UpdateMount(allocationID, func(m *MountState, _ *mountRuntime) {
		m.State = MountStateUnmounting
		cp := *m
		unmounting = cp
	})
	e.a.publishMount(&unmounting)

	// teardown 记录"本机是否已经有东西被真的拆掉"（挂载点被移除 / 磁盘被下线）。
	//
	// 它决定卸载失败后的状态语义（见 unmountFailureState）：
	//   - false：什么都没动 → 回滚成卸载前的状态（磁盘确实还挂着，状态如实）；
	//   - true：已经拆掉一部分 → **绝不回滚**。回滚等于向界面谎报"还挂着"，而且前端收到
	//     mounted 事件会弹出"已挂载"提示（真实反馈："点了卸载，然后提示挂载成功？磁盘状态
	//     还是已挂载，但我看已经卸载成功了、盘符不见了"）。
	//
	// 统一卸载语义下，唯一还会失败的阶段（挂载点还在，见 ①）必然在 teardown 置位之前，
	// 因此 teardown=true 之后不再有失败分支；这里保留判断是给后续改动留的安全网。
	teardown := false

	// fail 上报某一阶段失败，并按 teardown 决定回滚还是保留错误记录。
	fail := func(stage string, cause error) error {
		target := unmountFailureState(teardown, prevState)

		var out *MountState
		e.a.store.UpdateMount(allocationID, func(m *MountState, _ *mountRuntime) {
			m.State = target
			if target != MountStateError {
				cp := *m
				out = &cp
				return
			}
			m.Phase = ""
			// 稳定码形如 unmount_mount_point:platform.ps_failed，界面可直接翻译。
			m.LastError = describeError("unmount_"+stage, cause)
			m.LastErrorDetail = joinDetail(mountErrorDetailOf(cause))
			cp := *m
			out = &cp
		})
		if out != nil {
			e.a.publishMount(out)
		}
		if target == MountStateError {
			e.a.logger.Error("卸载未完成，保留记录供重试",
				"allocation_id", allocationID, "stage", stage, "error", cause)
		}
		return errUnmountFailed(stage, cause)
	}

	// ① 移除挂载点（盘符或目录）。必须在磁盘下线之前做：盘一旦 Offline，盘符就没了。
	if path := strings.TrimSpace(ms.MountPath); path != "" {
		if err := e.a.vol.Unmount(ctx, path); err != nil {
			// 幂等兜底，两种"挂载点确实不在了"都必须视为已移除 —— 否则会为了一个不存在的
			// 挂载点中断整次卸载，把后面的断开会话、回写释放、删记录一起拖没：
			//
			//   - 磁盘早已知晓（进程内记着磁盘号）却查不到挂载点；
			//   - **本机已无活动会话**：盘随会话一起消失了，挂载点不可能还在。这条专治
			//     "会话被断后记录仍是已挂载"的场景（进程重启后磁盘号无从得知，前一条
			//     兜底会失效，于是用户既挂不上又卸不掉 —— 见 session_probe.go）。
			pointGone := e.mountPointGone(ctx, ms, rt)
			active, verified := false, false
			if !pointGone {
				active, verified = e.a.probeMountSession(ctx, allocationID)
			}
			if !mountPointRemoved(pointGone, active, verified) {
				// 挂载点真的还在（盘符/目录仍属于该卷，Windows 认为卷正被占用）：
				// 此时磁盘确实还挂着，如实失败并回滚状态才是对的 —— 继续往下做只会让
				// 下线、断开跟着一起失败，最后留下"盘还挂着但记录已删"的假象。
				return fail("mount_point", err)
			}
			reason := "挂载点已不存在"
			if !pointGone {
				reason = "本机已无活动会话（盘随会话消失）"
			}
			e.a.logger.Info("挂载点已不存在，视为已移除",
				"allocation_id", allocationID, "mount_path", path, "reason", reason)
		}
	}
	// 从这一步起，本机已经"看不出还挂着东西"了 —— 后续失败一律不回滚状态。
	teardown = true

	// ② 磁盘 Offline —— Disconnect 的前置条件（否则 Disconnect-IscsiTarget 报 0xefff0040）。
	//
	// 磁盘号优先取运行时值，回退到挂载记录里的值：运行时信息只在内存中，进程重启后为空，
	// 若因此跳过下线，下一步的断开必然因"设备在线"失败。
	var diskOfflineErr error
	if diskNumber, known := teardownDiskNumber(ms, rt); known {
		if err := e.a.vol.SetOffline(ctx, diskNumber, true); err != nil {
			// 不在这里中断：会话断开才是卸载的关键（磁盘会随会话断开一并消失）。
			// 若断开也失败，错误会以更贴近真因的阶段上报（见下）。
			diskOfflineErr = err
			e.a.logger.Warn("下线磁盘失败（继续尝试断开会话）",
				"allocation_id", allocationID, "disk_number", diskNumber, "error", err)
		}
	}

	// ③ 断开 iSCSI 会话（device_in_use 时补下线并重试）。
	if targetIQN := strings.TrimSpace(ms.TargetIQN); targetIQN != "" {
		if err := e.disconnectSession(ctx, targetIQN, ms, rt); err != nil {
			// 阶段归因：下线失败才是真因时别报成 disconnect，否则用户按"会话占用"去排查，
			// 而真正没做到的是磁盘下线（两者的处置方式完全不同）。
			stage := "disconnect"
			if diskOfflineErr != nil && isDeviceInUse(err) {
				stage = "disk_offline"
				err = diskOfflineErr
			}
			// 走到这里挂载点已经移除了（用户在资源管理器里已经看不到这块盘），
			// 断开会话只是清残留：失败不中断、不报错，但必须留下日志。
			// 残留会话会在服务端 release 停用目标后被踢掉，④也会取消持久化避免重启重连。
			e.a.logger.Warn("断开 iSCSI 会话失败（继续卸载，残留会话由服务端停用目标后踢掉）",
				"allocation_id", allocationID, "stage", stage, "target_iqn", targetIQN, "error", err)
		}
		// ④ 取消会话持久化，避免重启后自动重连。
		if err := e.a.iscsi.Unregister(ctx, targetIQN); err != nil {
			e.a.logger.Warn("取消 iSCSI 会话持久化失败（继续）",
				"allocation_id", allocationID, "target_iqn", targetIQN, "error", err)
		}
	}

	// ⑤ 回写 release（失败不影响本地已卸载的事实）。
	if ms.LeaseID != "" {
		if client, err := e.a.serverClient(); err == nil {
			if err := client.Release(ctx, ms.LeaseID, e.a.store.ClientID()); err != nil {
				e.a.logger.Warn("回写租约释放失败（本地已卸载）", "allocation_id", allocationID, "error", err)
			}
		}
	}

	e.a.store.DeleteMount(allocationID)
	e.a.hub.Publish(Event{Type: "unmount", Data: map[string]any{"allocation_id": allocationID}})
	e.a.logger.Info("卸载完成", "allocation_id", allocationID, "mount_path", ms.MountPath)
	return nil
}

// disconnectSession 断开 iSCSI 会话；遇到 0xefff0040（会话上仍有在线设备）时补一次磁盘
// 下线并重试若干次。
//
// 为什么要重试（真实反馈："点了卸载，盘符不见了，但 iSCSI 发起程序里还是显示的连接中"）：
// 卸载失败留在本机的会话会持续重连，用户看到的磁盘状态与真实状态就此分叉。该错误绝大多数
// 情况下只是磁盘离线尚未生效，等一两秒再试即可断开。
func (e *mountEngine) disconnectSession(ctx context.Context, targetIQN string, ms MountState, rt mountRuntime) error {
	err := e.a.iscsi.Disconnect(ctx, targetIQN)
	if err == nil || !isDeviceInUse(err) {
		return err
	}

	if diskNumber, known := teardownDiskNumber(ms, rt); known {
		if offlineErr := e.a.vol.SetOffline(ctx, diskNumber, true); offlineErr != nil {
			e.a.logger.Warn("重试断开前下线磁盘失败",
				"target_iqn", targetIQN, "disk_number", diskNumber, "error", offlineErr)
		}
	}
	for attempt := 1; attempt <= unmountDisconnectRetries; attempt++ {
		if !sleepOrDone(ctx, unmountDisconnectBackoff) {
			return err
		}
		retryErr := e.a.iscsi.Disconnect(ctx, targetIQN)
		if retryErr == nil {
			e.a.logger.Info("断开 iSCSI 会话成功（重试后）",
				"target_iqn", targetIQN, "attempt", attempt)
			return nil
		}
		err = retryErr
		if !isDeviceInUse(retryErr) {
			return err
		}
	}
	return err
}

// mountPointGone 判断挂载点是否确实已经不存在（移除报错后的幂等兜底）。
//
// 需要磁盘号才能查询（CurrentMountPath 按磁盘号取当前挂载点）；拿不到磁盘号时返回 false ——
// 保守起见按"移除失败"处理，宁可让用户重试，也不要谎报卸载完成。
func (e *mountEngine) mountPointGone(ctx context.Context, ms MountState, rt mountRuntime) bool {
	diskNumber, known := teardownDiskNumber(ms, rt)
	if !known {
		return false
	}
	current, err := e.a.vol.CurrentMountPath(ctx, diskNumber)
	if err != nil {
		return false
	}
	return strings.TrimSpace(current) == ""
}

// unmountFailureState 返回卸载阶段失败后，那条记录应该处于什么状态。
//
//   - 什么都没拆掉（teardown=false）→ 回滚到卸载前的状态：磁盘确实还挂着，状态如实；
//   - 已经拆掉一部分（teardown=true）→ error。**不能回滚成 mounted**：回滚等于向界面谎报
//     "还挂着"，前端收到 mounted 事件还会弹一条"已挂载"提示（真实反馈："点了卸载，然后提示
//     挂载成功？磁盘状态还是已挂载，但盘符已经不见了"）。
func unmountFailureState(teardown bool, prevState string) string {
	if teardown {
		return MountStateError
	}
	return prevState
}

// teardownDiskNumber 返回卸载时要下线的磁盘号。
//
// 运行时信息（rt）只在内存里，进程重启后为空；此时回退到挂载记录里的磁盘号 ——
// 拿不到磁盘号就会跳过下线，而 Disconnect-IscsiTarget 会因为"设备在线"而失败，
// 于是卸载卡在最后一步（盘符已消失、会话还在）。
func teardownDiskNumber(ms MountState, rt mountRuntime) (int, bool) {
	if rt.DiskKnown && rt.DiskNumber >= 0 {
		return rt.DiskNumber, true
	}
	if ms.DiskNumber > 0 {
		return ms.DiskNumber, true
	}
	return 0, false
}

// isDeviceInUse 判断错误是否为「会话上仍有在线设备，无法断开」（HRESULT 0xefff0040）。
func isDeviceInUse(err error) bool {
	return apperr.CodeOf(err) == iscsiinitiator.CodeDeviceInUse
}

// sleepOrDone 等待 d；ctx 已结束时立刻返回 false（不阻塞调用方退出）。
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// remount 先卸载再按原参数重新挂载。
func (e *mountEngine) remount(ctx context.Context, allocationID string) (*MountState, error) {
	allocationID = strings.TrimSpace(allocationID)
	if allocationID == "" {
		return nil, apperr.InvalidParam("allocation_id")
	}
	ms, _, ok := e.a.store.GetMount(allocationID)
	if !ok {
		return nil, errNotMounted(allocationID)
	}
	if err := e.unmount(ctx, allocationID); err != nil {
		return nil, err
	}
	return e.mount(ctx, MountRequest{
		AllocationID: allocationID,
		MountMode:    ms.MountMode,
		MountPath:    ms.MountPath,
		RepoID:       ms.RepoID,
		RepoName:     ms.RepoName,
	}, false)
}

// restore 恢复本地记录中的挂载：先清理残留状态，再走完整挂载流程。
//
// 说明：服务端 MountSpec 不含「本机应挂载哪些分配」的清单，因此自动挂载只能依据
// 本地状态文件中记录的分配逐个恢复（见 docs/agent-api.md 的 auto_mount 语义）。
func (e *mountEngine) restore(ctx context.Context, req MountRequest) (*MountState, error) {
	if ms, _, ok := e.a.store.GetMount(req.AllocationID); ok && ms.State == MountStateMounted {
		// 上次运行残留的 mounted 状态：真实会话可能已不存在，先尽力清理再重挂。
		if err := e.unmount(ctx, req.AllocationID); err != nil {
			e.a.logger.Warn("清理残留挂载状态失败（继续重挂）", "allocation_id", req.AllocationID, "error", err)
		}
	}
	// 自动挂载：失败保留记录（见 mount 的 keepRecordOnFailure 说明）。
	return e.mount(ctx, req, true)
}

// mountAt 按模式完成挂载点分配，返回最终挂载路径。
//
// 命名的诉求（真实反馈："盘符或者是目录名应该等于存储库的名称"）：
//   - 盘符模式：Windows 只肯给一个盘符（E:、F:…），没有"用库名做盘符"这回事，
//     于是把库名写到**卷标**上 —— 资源管理器里该盘就显示存储库名称；
//   - 目录模式：目录名就是 `<服务端名称>_<存储库名称>`（见 resolveMountDir 与 3.4.5）。
func (e *mountEngine) mountAt(ctx context.Context, diskNumber int, mode string, req MountRequest, spec *MountSpec) (string, error) {
	if mode != mountModeDirectory {
		letter, err := e.a.vol.MountToDriveLetter(ctx, diskNumber)
		if err != nil {
			return "", err
		}
		e.labelVolume(ctx, diskNumber, repoNameOf(req, spec))
		return letter, nil
	}
	dir, err := e.resolveMountDir(req, spec)
	if err != nil {
		return "", err
	}
	// 目录不在 Go 侧创建，交给挂载脚本（不存在时它会建，等价 mkdir -p）。
	//
	// 为什么必须交给脚本（真实事故：挂载点自指 → 资源管理器无限嵌套）：
	// 用户填的父目录本身可能是**本磁盘自己的残留挂载点**（老版本把卷直接挂到父目录上，
	// Remove-PartitionAccessPath 之后 junction 还留在原地，卷于是依旧从该路径可达）。
	// 此时在 Go 侧建目录，目录会被建进**这个卷的内部**；脚本紧接着把卷挂到这个"卷内路径"上，
	// 挂载点就指向了自己 —— C:\Vault\lib\lib\lib… 无限递归（用户截图）。
	// 判定"路径是否落在本磁盘自己的卷里"只有脚本做得到（要读 junction 的 Target 比卷 GUID），
	// 所以创建也一并交给它：先识别并清掉残留，再在真实目录里建挂载点（见 volume_mount_dir.ps1）。
	if err := e.a.vol.MountToDirectory(ctx, diskNumber, dir); err != nil {
		return "", err
	}
	return dir, nil
}

// labelVolume 把存储库名称写进卷标。
//
// best effort：卷标只是给人看的，失败（不支持、权限、卷刚上线还没就绪）只告警，
// 绝不因此判定挂载失败 —— 磁盘已经可用，为了一个名字把整次挂载标成错误得不偿失。
func (e *mountEngine) labelVolume(ctx context.Context, diskNumber int, repoName string) {
	label := volumeLabelOf(repoName)
	if label == "" {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, labelTimeout)
	defer cancel()
	if err := e.a.vol.SetLabel(callCtx, diskNumber, label); err != nil {
		e.a.logger.Warn("设置卷标失败（不影响挂载）",
			"disk_number", diskNumber, "label", label, "error", err)
		return
	}
	e.a.logger.Info("已把存储库名称写入卷标", "disk_number", diskNumber, "label", label)
}

// volumeLabelOf 把存储库名称规整成合法的卷标；无法规整出内容时返回空串（调用方跳过）。
func volumeLabelOf(repoName string) string {
	trimmed := strings.TrimSpace(repoName)
	if trimmed == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range trimmed {
		if r < 0x20 || strings.ContainsRune(volumeLabelInvalidChars, r) {
			b.WriteRune('-')
			continue
		}
		b.WriteRune(r)
	}
	// Windows 不允许卷标以空格或点结尾（资源管理器会静默改写）。
	out := strings.Trim(b.String(), " .")
	if out == "" {
		return ""
	}
	if runes := []rune(out); len(runes) > volumeLabelMaxRunes {
		out = strings.TrimSpace(string(runes[:volumeLabelMaxRunes]))
	}
	return out
}

// volumeLabelInvalidChars 是 Windows 卷标不允许包含的字符（Set-Volume 会直接报错）。
const volumeLabelInvalidChars = `\/:*?"<>|,;+=[]`

// repoNameOf 解析存储库名称：本地请求优先，其次服务端下发值。
func repoNameOf(req MountRequest, spec *MountSpec) string {
	if name := strings.TrimSpace(req.RepoName); name != "" {
		return name
	}
	if spec != nil {
		return strings.TrimSpace(spec.RepoName)
	}
	return ""
}

// resolveMountDir 计算目录模式的绝对挂载路径。
//
// 规则（界面在挂载设置里如实告知同一条规则，两处必须一致）：
//  1. **本地指定的目录是父目录**：请求 mount_path（挂载对话框）> 每库配置 mount_dir。
//     真正的挂载点是它下面一层自动生成的 `<服务端名称>_<存储库名称>` 子目录 ——
//     多个库共用一个父目录也不会互相挤占，且目录名自解释（哪个服务端的哪个库）。
//  2. 本地没指定时，落到服务端下发的 mount_path：**绝对路径**是服务端管理员的显式指定，
//     原样使用；**相对路径**（服务端自动拼的 `<服务端名称>\<库名>`）也归一到上面那条命名
//     —— 否则同一个库会因为"从哪挂"得到两种目录名（srv\lib 与 srv_lib），
//     界面按一种口径提示、实际却挂到另一种（真实反馈就是这么来的）。
//  3. 都没有时退化为 `<挂载根>\<服务端名称>_<存储库名称>`。
//
// 幂等：父目录末段已经等于 `<服务端名称>_<存储库名称>` 时原样返回。重挂与恢复
// （remount / restore）会把**上次的最终挂载点**当请求传回来（见 recordedMountRequest），
// 没有这条兜底，每重挂一次就会多套一层目录（D:\Vault\a_b\a_b\a_b…）。
func (e *mountEngine) resolveMountDir(req MountRequest, spec *MountSpec) (string, error) {
	base := strings.TrimSpace(e.a.cfg.Get().DefaultMountDir)
	leaf := mountDirLeaf(e.serverAlias(spec), repoNameOf(req, spec), req.AllocationID)

	local := strings.TrimSpace(req.MountPath)
	if local == "" {
		if pref, ok := e.repoPref(req.RepoID); ok {
			local = strings.TrimSpace(pref.MountDir)
		}
	}
	if local != "" {
		root := local
		if !filepath.IsAbs(root) {
			if base == "" {
				return "", apperr.InvalidParam("default_mount_dir")
			}
			root = filepath.Join(base, root)
		}
		root = filepath.Clean(root)
		// 已经就是最终挂载点（重挂/恢复把上次的结果传回来了）：不再加层。
		// 比较不区分大小写：Windows 文件系统不区分。
		if strings.EqualFold(filepath.Base(root), leaf) {
			return root, nil
		}
		return filepath.Join(root, leaf), nil
	}

	if spec != nil {
		if raw := strings.TrimSpace(spec.MountPath); raw != "" && filepath.IsAbs(raw) {
			return filepath.Clean(raw), nil
		}
	}
	if base == "" {
		return "", apperr.InvalidParam("default_mount_dir")
	}
	return filepath.Join(base, leaf), nil
}

// mountDirLeaf 返回目录模式下自动生成的那一级目录名：`<服务端名称>_<存储库名称>`。
//
// 两段都过 sanitizePathSegment：库名/别名可能含 `\` `/` 等字符，直接拼接会让路径跑到
// 挂载根之外（路径穿越）。库名缺失时退到分配 ID（至少不会让所有库挤进同一个目录）。
func mountDirLeaf(serverAlias, repoName, allocationID string) string {
	server := sanitizePathSegment(serverAlias)
	if server == "" {
		server = "vault"
	}
	repo := sanitizePathSegment(repoName)
	if repo == "" {
		repo = sanitizePathSegment(allocationID)
	}
	if repo == "" {
		return server
	}
	return server + "_" + repo
}

// sanitizePathSegment 把库名这类用户输入规整为可安全用作**单层**目录名的片段。
//
// 必要性：库名里出现 \ / : 等字符时，filepath.Join 会让目录跑到挂载根之外
// （路径穿越）；纯展示名字换成短横线即可，不改变可读性。
func sanitizePathSegment(name string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return ""
	}
	replaced := strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`/\:*?"<>|`, r) {
			return '-'
		}
		return r
	}, trimmed)
	return strings.Trim(replaced, " .")
}

// serverAlias 返回目录模式使用的服务端别名（本地配置优先，其次服务端名称）。
func (e *mountEngine) serverAlias(spec *MountSpec) string {
	if alias := strings.TrimSpace(e.a.cfg.Get().ServerAlias); alias != "" {
		return alias
	}
	if spec != nil && strings.TrimSpace(spec.ServerName) != "" {
		return strings.TrimSpace(spec.ServerName)
	}
	return "vault"
}

// runPostScript 渲染并执行挂载后置脚本；失败只记录告警，不阻塞挂载。
func (e *mountEngine) runPostScript(ctx context.Context, script string, spec *MountSpec, mountPath, repoName string) {
	rendered := strings.TrimSpace(e.renderPostScript(script, spec, mountPath, repoName))
	if rendered == "" {
		return
	}
	runCtx, cancel := context.WithTimeout(ctx, postScriptTimeout)
	defer cancel()

	e.a.logger.Info("执行挂载后置脚本", "target_iqn", spec.TargetIQN, "mount_path", mountPath)
	if _, err := e.a.ps.RunInline(runCtx, rendered); err != nil {
		e.a.logger.Warn("挂载后置脚本执行失败（不阻塞挂载）",
			"target_iqn", spec.TargetIQN, "error", err)
	}
}

// renderPostScript 先做变量替换，再由调用方执行（见 5.5：脚本中的路径必须用注入变量）。
//
// 支持的变量：{MOUNT_PATH} / {REPO_NAME} / {SERVER_ALIAS} / {SERVER_NAME}。
func (e *mountEngine) renderPostScript(script string, spec *MountSpec, mountPath, repoName string) string {
	if strings.TrimSpace(script) == "" {
		return ""
	}
	serverName := ""
	if spec != nil {
		serverName = spec.ServerName
	}
	replacer := strings.NewReplacer(
		"{MOUNT_PATH}", mountPath,
		"{REPO_NAME}", repoName,
		"{SERVER_ALIAS}", e.serverAlias(spec),
		"{SERVER_NAME}", serverName,
	)
	return replacer.Replace(script)
}

// repoPref 返回该库在本机的挂载偏好（卡片「配置」里存的那份）。
func (e *mountEngine) repoPref(repoID string) (RepoMountPref, bool) {
	repoID = strings.TrimSpace(repoID)
	if repoID == "" {
		return RepoMountPref{}, false
	}
	pref, ok := e.a.cfg.Get().RepoMounts[repoID]
	return pref, ok
}

// resolveMode 确定本次挂载模式：请求 > 每库配置 > 服务端下发 > 本地默认。
//
// 每库配置排在服务端下发之前：它是用户在这台机器上**确认过**的选择（弹窗里存下来的），
// 界面上显示什么形态就该挂成什么形态；服务端 client_config 里的 mount_mode 只是库的缺省建议。
func (e *mountEngine) resolveMode(req MountRequest, spec *MountSpec) string {
	if mode := normalizeMountMode(req.MountMode); mode != "" {
		return mode
	}
	if pref, ok := e.repoPref(req.RepoID); ok {
		if mode := normalizeMountMode(pref.MountMode); mode != "" {
			return mode
		}
	}
	if spec != nil {
		if mode := normalizeMountMode(spec.MountMode); mode != "" {
			return mode
		}
	}
	if mode := normalizeMountMode(e.a.cfg.Get().DefaultMountMode); mode != "" {
		return mode
	}
	return mountModeLetter
}

// normalizeMountMode 规范化挂载模式；非法值返回空串。
func normalizeMountMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case mountModeLetter:
		return mountModeLetter
	case mountModeDirectory:
		return mountModeDirectory
	default:
		return ""
	}
}

// clientAuthMode 把服务端下发的 auth_mode 映射为客户端连接模式。
//
// 服务端按 initiator 白名单（ip）过滤时，客户端侧无需 CHAP。
func clientAuthMode(authMode string) string {
	if strings.EqualFold(strings.TrimSpace(authMode), "chap") {
		return iscsiinitiator.AuthModeCHAP
	}
	return iscsiinitiator.AuthModeNone
}

// portalPort 返回有效门户端口。
func portalPort(spec *MountSpec) int {
	if spec != nil && spec.PortalPort > 0 {
		return spec.PortalPort
	}
	return iscsiinitiator.DefaultPortalPort
}

// portalProbeTimeout 是门户 TCP 可达性探测的超时。
//
// 取 3s：探测只为把"网络/防火墙不可达"与"TCP 通、但登录/认证失败"分开，不需要长等；
// 它位于失败路径上，多等一秒用户就多等一秒。
const portalProbeTimeout = 3 * time.Second

// 门户 TCP 探测结论码（进 errMountFailed 的 args.tcp，界面直接可读）。
const (
	portalTCPReachable = "reachable"
	portalTCPTimeout   = "timeout"
	portalTCPRefused   = "refused"
	portalTCPFailed    = "unreachable"
)

// connectDiagnostics 汇总"连不上门户"所需的连接信息，返回可读文本与 TCP 结论码。
//
// 为什么必须打出来：connect 阶段失败以前只回 args.stage=connect —— 门户地址是什么、
// 端口通不通、目标名对不对，用户一项都拿不到（真实反馈："根本没法定位错误"）。
// 其中 TCP 探测能一刀切开两类原因：
//   - 不可达 → 网络/防火墙拦截，或服务端派发的门户地址本机到不了（多网卡常见）；
//   - 可达 → 目标名、CHAP，或服务端还没把目标发布出来。
//
// 安全：只含地址、端口、IQN、认证方式与 CHAP 用户名；**绝不含 CHAP 密钥**
// （密钥只存在于 mountRuntime，见其字段说明）。
func connectDiagnostics(ctx context.Context, spec *MountSpec, port int) (detail, tcp string) {
	if spec == nil {
		return "", portalTCPFailed
	}
	tcp, latency := probePortalTCP(ctx, spec.PortalAddress, port)
	if tcp == portalTCPReachable {
		tcp = fmt.Sprintf("%s(%dms)", portalTCPReachable, latency.Milliseconds())
	}
	auth := strings.TrimSpace(spec.AuthMode)
	if auth == "" {
		auth = "none"
	}
	parts := []string{
		"portal=" + net.JoinHostPort(spec.PortalAddress, strconv.Itoa(port)),
		"target=" + spec.TargetIQN,
		"auth=" + auth,
		"tcp=" + tcp,
	}
	if user := strings.TrimSpace(spec.ChapUser); user != "" {
		parts = append(parts, "chap_user="+user)
	}
	return strings.Join(parts, " "), tcp
}

// probePortalTCP 对门户做一次短超时 TCP 连接，返回结论码与耗时。
//
// 刻意用 context.WithoutCancel：失败路径上挂载请求的 ctx 可能已被取消（界面超时取消等），
// 而探测恰恰是最需要跑完的一步 —— 否则用户看到的结论是"探测被取消"，等于没说。
func probePortalTCP(ctx context.Context, host string, port int) (string, time.Duration) {
	if strings.TrimSpace(host) == "" {
		return portalTCPFailed, 0
	}
	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), portalProbeTimeout)
	defer cancel()

	start := time.Now()
	conn, err := (&net.Dialer{}).DialContext(probeCtx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	latency := time.Since(start)
	if err == nil {
		_ = conn.Close()
		return portalTCPReachable, latency
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return portalTCPTimeout, latency
	}
	// Windows 的拒绝连接文案是 "connectex: No connection could be made..."，不一定含
	// "refused"；这里只做尽力归类，归不出来一律按 unreachable（信息量已足够）。
	if strings.Contains(strings.ToLower(err.Error()), "refused") {
		return portalTCPRefused, latency
	}
	return portalTCPFailed, latency
}

// joinDetail 拼接诊断信息与原始报错（忽略空串），并按状态字段上限截断。
//
// 截断理由同 mountErrorDetailOf：该字段会经 SSE 推送并写进本地状态文件。
func joinDetail(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			kept = append(kept, trimmed)
		}
	}
	joined := strings.Join(kept, " | ")
	runes := []rune(joined)
	if len(runes) <= maxMountErrorDetailRunes {
		return joined
	}
	return string(runes[:maxMountErrorDetailRunes]) + "…"
}

// publishMount 推送挂载状态事件。
func (a *Agent) publishMount(ms *MountState) {
	if ms == nil {
		return
	}
	a.hub.Publish(Event{Type: "mount", Data: ms})
}
