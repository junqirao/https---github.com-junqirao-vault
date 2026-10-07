package agent

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"vault/internal/apperr"
	"vault/internal/platform/iscsiinitiator"
)

// TestMountStageLeavesResidue 锁定"哪些阶段的失败可能在本机留下残留"。
//
// 这条判断决定失败后要不要清理、要不要保留记录（真实反馈："都错误了就不要有挂载记录了"）：
//   - Connect 之前的失败在机器上什么都没建立 → 抹掉记录（也无需跑 PowerShell 清理，省 1s+）；
//   - Connect 之后可能已经连上会话、把盘上线 → 必须清理，清理不掉就保留记录供人工卸载。
func TestMountStageLeavesResidue(t *testing.T) {
	residueStages := []string{
		"wait_connected",
		"find_disk",
		"disk_online",
		"disk_read_write",
		"mount_point",
	}
	cleanStages := []string{
		"initiator_service",
		"portal",
		"connect",
		"post_script",
		"",
		"unknown_stage",
	}
	for _, stage := range residueStages {
		if !mountStageLeavesResidue(stage) {
			t.Fatalf("阶段 %q 应判定为可能留下残留", stage)
		}
	}
	for _, stage := range cleanStages {
		if mountStageLeavesResidue(stage) {
			t.Fatalf("阶段 %q 在 Connect 之前（或未知），不应判定为有残留", stage)
		}
	}
}

// TestCleanupFailedMountSkipForCleanStages Connect 之前的失败直接判定"已清理干净"，
// 且不碰任何状态：既没有可清理的东西，也不该让用户在失败路径上多等一次 PowerShell。
func TestCleanupFailedMountSkipForCleanStages(t *testing.T) {
	a := newPhaseTestAgent(t)
	const alloc = "alloc-clean"
	a.store.PutMount(&MountState{
		AllocationID: alloc,
		TargetIQN:    "iqn.2000-01.com.vault:test",
		State:        MountStateMounting,
	}, &mountRuntime{})

	cleaned, err := a.engine.cleanupFailedMount(context.Background(), alloc, "connect")
	if err != nil {
		t.Fatalf("无残留阶段的清理不应报错：%v", err)
	}
	if !cleaned {
		t.Fatal("无残留阶段应判定为已清理干净（调用方据此抹掉记录）")
	}
	// 关键：**没有把记录删掉**（删除是调用方在 cleaned 之后做的），也没有改动状态。
	ms, _, ok := a.store.GetMount(alloc)
	if !ok || ms.State != MountStateMounting {
		t.Fatalf("无残留阶段不应改动记录，实际 state=%q ok=%v", ms.State, ok)
	}
}

// TestCleanupFailedMountReportsIncomplete 严格清理失败时必须如实回报 false ——
// 调用方据此**保留记录**，绝不把还有残留（会话/磁盘还挂在本机）的分配变成"看不见"。
func TestCleanupFailedMountReportsIncomplete(t *testing.T) {
	a := newPhaseTestAgent(t)
	// 无记录：严格卸载立刻以 not_mounted 失败，属"清理未完成"。
	cleaned, err := a.engine.cleanupFailedMount(context.Background(), "no-such-alloc", "wait_connected")
	if err == nil {
		t.Fatal("清理失败应返回错误（调用方才知道要保留记录）")
	}
	if cleaned {
		t.Fatal("清理失败必须返回 false（否则残留会变成看不见）")
	}
}

// TestUnmountFailureStateNeverRollsBackAfterTeardown 锁定卸载失败的**状态语义**。
//
// 真实反馈："点了卸载，然后提示挂载成功？磁盘状态还是已挂载，我看已经卸载成功了、盘符
// 不见了" —— 根因是卸载失败后一律回滚成 mounted：盘符其实已经被拆掉，界面却收到 mounted
// 事件（前端据此弹"已挂载"提示）并继续显示"已挂载"。
func TestUnmountFailureStateNeverRollsBackAfterTeardown(t *testing.T) {
	if got := unmountFailureState(false, MountStateMounted); got != MountStateMounted {
		t.Fatalf("什么都没拆掉时应回滚到卸载前状态，实际 %q", got)
	}
	if got := unmountFailureState(true, MountStateMounted); got != MountStateError {
		t.Fatalf("已拆掉一部分时不得回滚成 mounted（界面会谎报已挂载并弹提示），实际 %q", got)
	}
	// 记录本来就卡在"卸载中"（上次进程死在卸载中途留下的脏记录）：回滚成 unmounting 等于
	// **什么都没发生**，界面永远显示"卸载中"、点几次都一样（真实反馈："这种名存实亡的"）。
	if got := unmountFailureState(false, MountStateUnmounting); got != MountStateError {
		t.Fatalf("卸载前的状态就是'卸载中'时必须落成 error（否则用户永远出不来），实际 %q", got)
	}
}

// TestFinishInterruptedUnmountsClearsStuckRecord 锁定"重启后自动闭环"。
//
// 上次进程死在卸载中途时，记录会停在 unmounting，而它是写在本机状态文件里的 —— 重装服务端
// 也清不掉。启动收尾必须把它补完、删掉记录，而不是让界面一直显示"卸载中"（真实反馈：
// "重启之后客户端一直提示在卸载中，这种名存实亡的我要能在客户端自己闭环"）。
func TestFinishInterruptedUnmountsClearsStuckRecord(t *testing.T) {
	a := newPhaseTestAgent(t)
	const alloc = "alloc-stuck"
	// 现场：会话与挂载点都早已不在（服务端重装过、盘也没了），记录却停在"卸载中"。
	// 空 MountPath/TargetIQN/LeaseID 等价于"本机确实什么都没有了"。
	a.store.PutMount(&MountState{
		AllocationID: alloc,
		RepoID:       "repo-a",
		State:        MountStateUnmounting,
	}, &mountRuntime{})

	a.finishInterruptedUnmounts(context.Background())

	if _, _, ok := a.store.GetMount(alloc); ok {
		t.Fatal("卡在卸载中的记录应被收尾删掉（否则客户端永远显示'卸载中'，重启多少次都一样）")
	}
}

// TestMountPointGoneTreatsMissingLetterAsRemoved 锁定盘符模式的硬证据：
// 盘符自己没了 ⇒ 挂载点确实已不在。
//
// 这条**不需要磁盘号、也不需要发起端** —— 进程重启后磁盘号无从得知（运行时信息只在内存里），
// 发起端没跑时连会话都问不出来，此时它是唯一的出路；缺了它，一条"盘符早没了"的记录会永远
// 卡在卸载里（真实反馈："重启之后客户端一直提示在卸载中"）。
func TestMountPointGoneTreatsMissingLetterAsRemoved(t *testing.T) {
	a := newPhaseTestAgent(t)

	// 非盘符形态一律不判"已消失"：目录模式必须走磁盘号那条证据（挂载目录本身总是存在，
	// 从目录上看不出里面有没有挂着卷）。
	for _, path := range []string{"", "libs", `relative\dir`, "/mnt/libs"} {
		if mountLetterGone(path) {
			t.Fatalf("路径 %q 不是盘符形态，不该被判定为'盘符已消失'", path)
		}
	}

	letter := unusedDriveLetter()
	if letter == "" {
		t.Skip("本机没有空闲盘符可用于模拟'盘符已消失'")
	}
	gone := a.engine.mountPointGone(context.Background(), MountState{
		MountMode: mountModeLetter,
		MountPath: letter,
	}, mountRuntime{})
	if !gone {
		t.Fatalf("盘符 %s 不存在时应判定挂载点已不在（否则记录永远卡在卸载里）", letter)
	}
}

// unusedDriveLetter 返回一个本机不存在的盘符（形如 "Z:"）；全被占用时返回空串。
func unusedDriveLetter() string {
	for c := 'D'; c <= 'Z'; c++ {
		letter := string(c) + ":"
		if mountLetterGone(letter) {
			return letter
		}
	}
	return ""
}

// TestTeardownDiskNumberFallsBackToRecord 锁定"卸载时磁盘号从哪来"。
//
// 运行时信息（mountRuntime）只在内存里，进程重启后为空；若因此拿不到磁盘号就会跳过
// Set-Disk -IsOffline，随后 Disconnect-IscsiTarget 必然以 0xefff0040（设备在线）失败 ——
// 卸载卡在最后一步，盘符已消失、会话却还在重连。
func TestTeardownDiskNumberFallsBackToRecord(t *testing.T) {
	if n, ok := teardownDiskNumber(MountState{DiskNumber: 3}, mountRuntime{}); !ok || n != 3 {
		t.Fatalf("运行时信息缺失时应回退到记录里的磁盘号，实际 n=%d ok=%v", n, ok)
	}
	if n, ok := teardownDiskNumber(MountState{DiskNumber: 3}, mountRuntime{DiskKnown: true, DiskNumber: 7}); !ok || n != 7 {
		t.Fatalf("运行时信息存在时应优先使用它，实际 n=%d ok=%v", n, ok)
	}
	// 磁盘 0 是合法磁盘号，不能因为零值被当成"未知"。
	if n, ok := teardownDiskNumber(MountState{}, mountRuntime{DiskKnown: true}); !ok || n != 0 {
		t.Fatalf("磁盘 0 必须被视为已知磁盘号，实际 n=%d ok=%v", n, ok)
	}
	if _, ok := teardownDiskNumber(MountState{}, mountRuntime{}); ok {
		t.Fatal("两处都拿不到磁盘号时应返回 false（调用方跳过下线，由断开重试兜底）")
	}
}

// TestIsDeviceInUseMatchesPlatformCode 锁定 device_in_use（HRESULT 0xefff0040）的识别。
//
// 卸载时的断开重试完全依赖它：认不出来就会立刻失败，留给用户一个"盘符没了、iSCSI 里
// 会话还在重连"的残留会话。
func TestIsDeviceInUseMatchesPlatformCode(t *testing.T) {
	if !isDeviceInUse(iscsiinitiator.ErrDeviceInUse()) {
		t.Fatal("platform.iscsi_device_in_use 应被识别为设备占用")
	}
	if isDeviceInUse(apperr.New(CodeUnmountFailed, http.StatusInternalServerError)) {
		t.Fatal("其它错误不应被误判为设备占用")
	}
	if isDeviceInUse(nil) {
		t.Fatal("nil 不应被判为设备占用")
	}
}

// TestErrUnmountFailedCarriesStage 卸载失败必须带上失败阶段（界面文案用它插值，
// 否则用户只有一个"卸载失败"，无从判断卡在挂载点/下线/断开哪一步）。
func TestErrUnmountFailedCarriesStage(t *testing.T) {
	err := errUnmountFailed("disconnect", errors.New("boom"))
	if err.Code != CodeUnmountFailed {
		t.Fatalf("错误码 = %q，期望 %q", err.Code, CodeUnmountFailed)
	}
	if got, _ := err.Args["stage"].(string); got != "disconnect" {
		t.Fatalf("stage 参数 = %v，期望 disconnect", err.Args["stage"])
	}
}
