package agent

import "testing"

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
