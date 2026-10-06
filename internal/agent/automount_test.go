package agent

import (
	"strings"
	"sync"
	"testing"
)

// TestRecordedMountRequestPrefPolicy 锁定"本地记录 + 每库配置"的处置规则：
//
//	没有该库的配置   → 按记录恢复（全局 auto_mount 的老行为）；
//	配置里关了自动挂载 → 不恢复（用户明确表态优先于历史记录）；
//	配置里开了自动挂载 → 恢复，且形态/目录以配置为准（记录里是上次的旧值）。
//
// 真实反馈驱动的点：只在配置里改了形态却没重挂时，下次启动不能被记录里的旧形态带回去。
func TestRecordedMountRequestPrefPolicy(t *testing.T) {
	record := MountState{
		AllocationID: "alloc-1",
		RepoID:       "repo-a",
		RepoName:     "样品库",
		MountMode:    mountModeLetter,
		MountPath:    `E:\`,
	}
	cases := []struct {
		name      string
		prefs     map[string]RepoMountPref
		wantMount bool
		wantMode  string
		wantPath  string
	}{
		{
			name:      "没有该库的配置 → 按记录恢复",
			prefs:     map[string]RepoMountPref{"repo-b": {AutoMount: true}},
			wantMount: true,
			wantMode:  mountModeLetter,
			wantPath:  `E:\`,
		},
		{
			name:      "配置关掉自动挂载 → 不恢复",
			prefs:     map[string]RepoMountPref{"repo-a": {MountMode: mountModeDirectory, AutoMount: false}},
			wantMount: false,
		},
		{
			name: "配置开了自动挂载并指定形态目录 → 以配置为准",
			prefs: map[string]RepoMountPref{"repo-a": {
				MountMode: mountModeDirectory,
				MountDir:  `D:\vault\样品库`,
				AutoMount: true,
			}},
			wantMount: true,
			wantMode:  mountModeDirectory,
			wantPath:  `D:\vault\样品库`,
		},
		{
			name:      "配置开了自动挂载但没表态形态 → 沿用记录",
			prefs:     map[string]RepoMountPref{"repo-a": {AutoMount: true}},
			wantMount: true,
			wantMode:  mountModeLetter,
			wantPath:  `E:\`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, ok := recordedMountRequest(record, c.prefs)
			if ok != c.wantMount {
				t.Fatalf("是否恢复 = %v，期望 %v", ok, c.wantMount)
			}
			if !ok {
				return
			}
			if req.AllocationID != record.AllocationID {
				t.Fatalf("allocation_id = %q，期望 %q", req.AllocationID, record.AllocationID)
			}
			if req.MountMode != c.wantMode || req.MountPath != c.wantPath {
				t.Fatalf("形态/目录 = %q/%q，期望 %q/%q", req.MountMode, req.MountPath, c.wantMode, c.wantPath)
			}
		})
	}
}

// TestConfiguredAutoMountTargetsSkipsManuallyUnmounted 锁定"用户手动卸载过的库本次运行不再自动挂载"：
//
// 卸载成功会连本地挂载记录一起删掉（见 mountEngine.unmountLocked），于是这个库在配置眼里
// 又变回"从来没挂过、但 auto_mount 开着"，任何一次会话建立（客户端推会话 / 证书免密登录 /
// 令牌定时续期都走 setSession）都会把它立刻挂回来 —— 用户看到的是"点了卸载没用"。
// 手动卸载过之后，configuredAutoMountTargets 必须把这个库排除掉。
func TestConfiguredAutoMountTargetsSkipsManuallyUnmounted(t *testing.T) {
	var guard manualUnmountGuard
	guard.block("repo-manual")

	prefs := map[string]RepoMountPref{
		"repo-auto":     {AutoMount: true},
		"repo-off":      {AutoMount: false},
		"repo-recorded": {AutoMount: true},
		"repo-manual":   {AutoMount: true},
	}
	// repo-recorded 已有本地记录 → 由 restoreRecordedMounts 负责，这里不重复处理。
	recorded := map[string]struct{}{"repo-recorded": {}}

	if got := joinAutoMountTargetIDs(configuredAutoMountTargets(prefs, recorded, guard.blocked)); got != "repo-auto" {
		t.Fatalf("手动卸载 repo-manual 后该自动挂载的库 = %q，期望 %q", got, "repo-auto")
	}

	// 没手动卸载过的库必须照旧自动挂载：修复不能演变成"自动挂载整体失效"。
	want := "repo-auto,repo-manual,repo-recorded"
	if got := joinAutoMountTargetIDs(configuredAutoMountTargets(prefs, map[string]struct{}{}, func(string) bool { return false })); got != want {
		t.Fatalf("未手动卸载时该自动挂载的库 = %q，期望 %q", got, want)
	}

	// 只挡被点过卸载的那一个库，别的库不受牵连。
	if got := joinAutoMountTargetIDs(configuredAutoMountTargets(
		map[string]RepoMountPref{"repo-manual": {AutoMount: true}, "repo-other": {AutoMount: true}},
		map[string]struct{}{},
		guard.blocked,
	)); got != "repo-other" {
		t.Fatalf("黑名单只应挡 repo-manual，实际自动挂载 = %q，期望 %q", got, "repo-other")
	}
}

// TestManualUnmountGuard 锁定黑名单本身的行为：按库 ID 记、前后空格归一、重复记账幂等、
// 没有库 ID 时记不下（也不能反过来挡住所有库）。
func TestManualUnmountGuard(t *testing.T) {
	var guard manualUnmountGuard
	if guard.blocked("repo-a") {
		t.Fatal("没卸载过的库不应该被挡住")
	}
	if guard.blocked("") || guard.blocked("   ") {
		t.Fatal("空 ID 不该被判定为已卸载")
	}

	guard.block("  repo-a  ") // 记录与查询都做 TrimSpace 归一
	guard.block("repo-a")     // 重复卸载同一个库是幂等的
	guard.block("")
	guard.block("   ")

	if !guard.blocked("repo-a") {
		t.Fatal("手动卸载过的库应被挡住")
	}
	if guard.blocked("repo-b") {
		t.Fatal("别的库不该被牵连")
	}

	// 卸载是 HTTP 请求并发进来的，记账/查询必须并发安全（配合 -race 跑）。
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				guard.block("repo-concurrent")
				_ = guard.blocked("repo-concurrent")
			}
		}()
	}
	wg.Wait()
	if !guard.blocked("repo-concurrent") {
		t.Fatal("并发记账后应被挡住")
	}
}

// joinAutoMountTargetIDs 把选出来的库按 ID 拼成一个字符串，便于整组比较（函数已保证顺序稳定）。
func joinAutoMountTargetIDs(targets []autoMountTarget) string {
	ids := make([]string, 0, len(targets))
	for _, item := range targets {
		ids = append(ids, item.repoID)
	}
	return strings.Join(ids, ",")
}
