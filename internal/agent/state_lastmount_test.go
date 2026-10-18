package agent

import (
	"path/filepath"
	"testing"
)

// TestRecordRepoMountPersistsAndSurvivesUnmount 锁定「上次挂载时间」的两条关键语义：
//   - 卸载会删掉挂载记录，但每库时间表必须留着（卡片未挂载时靠它显示"上次挂载"）；
//   - 时间只前进不后退（并发的挂载/恢复不应把时间改小）；
//   - 落盘后重启代理仍能读到（数据留在本机）。
func TestRecordRepoMountPersistsAndSurvivesUnmount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := NewStateStore(path, testLogger())
	if err != nil {
		t.Fatalf("构造状态存储失败：%v", err)
	}

	// 空 repo_id / 非正时间一律忽略，避免脏记录。
	store.RecordRepoMount("", 1000)
	store.RecordRepoMount("repo-a", 0)
	if got := store.LastMounts(); len(got) != 0 {
		t.Fatalf("非法入参不应写入，实际 %v", got)
	}

	store.RecordRepoMount("repo-a", 1000)
	// 更早的时间不能把已有时间改小。
	store.RecordRepoMount("repo-a", 900)
	store.RecordRepoMount("repo-b", 2000)
	if got := store.LastMounts(); got["repo-a"] != 1000 || got["repo-b"] != 2000 {
		t.Fatalf("时间表应为 {repo-a:1000, repo-b:2000}，实际 %v", got)
	}

	// 挂载记录被删（卸载）后，时间表仍在。
	store.PutMount(&MountState{AllocationID: "alloc-a", RepoID: "repo-a", State: MountStateMounted}, nil)
	store.DeleteMount("alloc-a")
	if _, _, ok := store.GetMount("alloc-a"); ok {
		t.Fatal("卸载后挂载记录应被删除")
	}
	if got := store.LastMounts(); got["repo-a"] != 1000 {
		t.Fatalf("卸载不应清掉上次挂载时间，实际 %v", got)
	}

	// 重启：从同一份文件重新加载，时间表仍在。
	reloaded, err := NewStateStore(path, testLogger())
	if err != nil {
		t.Fatalf("重新加载状态存储失败：%v", err)
	}
	got := reloaded.LastMounts()
	if got["repo-a"] != 1000 || got["repo-b"] != 2000 {
		t.Fatalf("重启后应保留上次挂载时间，实际 %v", got)
	}
}
