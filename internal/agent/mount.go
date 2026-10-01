package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
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

	// 幂等：同一 allocation 已挂载时直接返回既有状态。
	if existing, _, ok := e.a.store.GetMount(allocationID); ok && existing.State == MountStateMounted {
		return &existing, nil
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
		RepoID:       strings.TrimSpace(req.RepoID),
		RepoName:     strings.TrimSpace(req.RepoName),
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
		Persistent:    true,
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
// 无残留阶段直接返回 true（不碰任何东西）；有残留阶段走**严格卸载**（force=false）：
// 任一阶段失败就停手并如实回报 —— 调用方据此保留一条 error 记录作为人工清理入口。
func (e *mountEngine) cleanupFailedMount(ctx context.Context, allocationID, stage string) (bool, error) {
	if !mountStageLeavesResidue(stage) {
		return true, nil
	}
	if err := e.unmountLocked(ctx, allocationID, false); err != nil {
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

// unmount 执行卸载流程（严格逆序，不可颠倒）。
//
// 顺序：SetOffline(true) → Unmount(移除挂载点) → Disconnect → Unregister → 回写 release。
// force=true 时忽略各阶段错误继续推进（用于清理残留与踢下线）。
func (e *mountEngine) unmount(ctx context.Context, allocationID string, force bool) error {
	allocationID = strings.TrimSpace(allocationID)
	if allocationID == "" {
		return apperr.InvalidParam("allocation_id")
	}

	unlock := e.a.locks.Lock("alloc:" + allocationID)
	defer unlock()

	return e.unmountLocked(ctx, allocationID, force)
}

// unmountLocked 是 unmount 的**不加锁**版本：调用方必须已经持有该 allocation 的锁。
//
// 存在的原因：挂载失败后的清理发生在 mount 的锁内，直接调 unmount 会在同一把 keyed 锁上
// 自锁死（挂载失败时永远卡住，比错误本身严重得多）。
func (e *mountEngine) unmountLocked(ctx context.Context, allocationID string, force bool) error {
	ms, rt, ok := e.a.store.GetMount(allocationID)
	if !ok {
		return errNotMounted(allocationID)
	}

	// 记住卸载前的状态：卸载失败时必须恢复它。
	//
	// 为什么必须：状态一旦写成 unmounting 而后续步骤失败，若不回写，记录就**永远冻结在
	// "卸载中"**（真实事故：目标/磁盘早已不存在，下线或移除挂载点报错，非 force 直接
	// return —— 界面上永远挂着一条"卸载中"，重装服务端也清不掉，因为记录在本机状态文件里）。
	prevState := ms.State

	var unmounting MountState
	e.a.store.UpdateMount(allocationID, func(m *MountState, _ *mountRuntime) {
		m.State = MountStateUnmounting
		cp := *m
		unmounting = cp
	})
	e.a.publishMount(&unmounting)

	// fail 恢复卸载前的状态并返回错误（只用于"什么都没拆掉"的非 force 失败路径）。
	fail := func(err error) error {
		var out *MountState
		e.a.store.UpdateMount(allocationID, func(m *MountState, _ *mountRuntime) {
			m.State = prevState
			cp := *m
			out = &cp
		})
		if out != nil {
			e.a.publishMount(out)
		}
		return err
	}

	// ① 先把磁盘 Offline，否则 Disconnect 会返回 0xefff0040。
	if rt.DiskKnown {
		if err := e.a.vol.SetOffline(ctx, rt.DiskNumber, true); err != nil {
			if !force {
				return fail(errUnmountFailed(err))
			}
			e.a.logger.Warn("下线磁盘失败（force 继续卸载）", "allocation_id", allocationID, "error", err)
		}
	}

	// ② 移除挂载点（盘符或目录）。
	if strings.TrimSpace(ms.MountPath) != "" {
		if err := e.a.vol.Unmount(ctx, ms.MountPath); err != nil {
			if !force {
				return fail(errUnmountFailed(err))
			}
			e.a.logger.Warn("移除挂载点失败（force 继续卸载）", "allocation_id", allocationID, "error", err)
		}
	}

	// ③ 断开 iSCSI 会话。
	if strings.TrimSpace(ms.TargetIQN) != "" {
		if err := e.a.iscsi.Disconnect(ctx, ms.TargetIQN); err != nil {
			// 0xefff0040 说明仍有用例占用磁盘：非 force 时明确上报，force 时继续。
			if !force {
				return fail(errUnmountFailed(err))
			}
			e.a.logger.Warn("断开 iSCSI 会话失败（force 继续卸载）",
				"allocation_id", allocationID, "target_iqn", ms.TargetIQN, "error", err)
		}
		// ④ 取消会话持久化，避免重启后自动重连。
		if err := e.a.iscsi.Unregister(ctx, ms.TargetIQN); err != nil {
			e.a.logger.Warn("取消 iSCSI 会话持久化失败（继续）",
				"allocation_id", allocationID, "target_iqn", ms.TargetIQN, "error", err)
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
	if err := e.unmount(ctx, allocationID, true); err != nil {
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
		if err := e.unmount(ctx, req.AllocationID, true); err != nil {
			e.a.logger.Warn("清理残留挂载状态失败（继续重挂）", "allocation_id", req.AllocationID, "error", err)
		}
	}
	// 自动挂载：失败保留记录（见 mount 的 keepRecordOnFailure 说明）。
	return e.mount(ctx, req, true)
}

// mountAt 按模式完成挂载点分配，返回最终挂载路径。
func (e *mountEngine) mountAt(ctx context.Context, diskNumber int, mode string, req MountRequest, spec *MountSpec) (string, error) {
	if mode != mountModeDirectory {
		return e.a.vol.MountToDriveLetter(ctx, diskNumber)
	}
	dir, err := e.resolveMountDir(req, spec)
	if err != nil {
		return "", err
	}
	// 目录挂载要求目录已存在且为空；这里按 <挂载根>\<别名>\<库名> 逐级创建。
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", apperr.InvalidParam("mount_path").WithArg("path", dir).WithCause(err)
	}
	if err := e.a.vol.MountToDirectory(ctx, diskNumber, dir); err != nil {
		return "", err
	}
	return dir, nil
}

// resolveMountDir 计算目录模式的绝对挂载路径。
func (e *mountEngine) resolveMountDir(req MountRequest, spec *MountSpec) (string, error) {
	raw := strings.TrimSpace(req.MountPath)
	if raw == "" && spec != nil {
		raw = strings.TrimSpace(spec.MountPath)
	}
	if raw != "" && filepath.IsAbs(raw) {
		return filepath.Clean(raw), nil
	}

	base := strings.TrimSpace(e.a.cfg.Get().DefaultMountDir)
	if base == "" {
		return "", apperr.InvalidParam("default_mount_dir")
	}
	rel := raw
	if rel == "" {
		// 服务端未下发目录时退化为 <别名>\<allocation_id>，避免所有分配挤进同一个目录。
		rel = filepath.Join(e.serverAlias(spec), req.AllocationID)
	}
	return filepath.Clean(filepath.Join(base, rel)), nil
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

// resolveMode 确定本次挂载模式：请求 > 服务端下发 > 本地默认。
func (e *mountEngine) resolveMode(req MountRequest, spec *MountSpec) string {
	if mode := normalizeMountMode(req.MountMode); mode != "" {
		return mode
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
