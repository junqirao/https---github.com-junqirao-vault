package agent

import (
	"context"
	"testing"
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
