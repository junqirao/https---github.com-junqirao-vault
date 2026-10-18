package store

import (
	"context"
	"testing"
	"time"

	"vault/internal/domain"
)

// newUploadStore 打开并迁移一个测试库（uploads 的查询都要求表已建好）。
func newUploadStore(t *testing.T) *Store {
	t.Helper()
	s := openTestStore(t)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("迁移测试库失败: %v", err)
	}
	return s
}

// seedUploadRow 建一条上传会话（uploads.user_id 没有外键，测试不必先造用户）。
func seedUploadRow(t *testing.T, s *Store, repoName, state, staging string) string {
	t.Helper()
	u := &domain.Upload{
		UserID: "u-1", RepoName: repoName, Mode: domain.UploadModeCopy,
		TotalFiles: 1, TotalBytes: 1024, StagingDir: staging, Manifest: "{}", State: state,
	}
	if err := s.CreateUpload(context.Background(), u); err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	return u.ID
}

// ageUploadRow 把会话的时间戳改到过去。
//
// 只能改库：CreateUpload 一律把两个时间戳写成 now，而 GC 的两条判定线全在"多久没动静"上。
func ageUploadRow(t *testing.T, s *Store, id string, createdAgo, updatedAgo time.Duration) {
	t.Helper()
	now := time.Now()
	if _, err := s.DB().ExecContext(context.Background(),
		`UPDATE uploads SET created_at = ?, updated_at = ? WHERE id = ?`,
		now.Add(-createdAgo).UnixMilli(), now.Add(-updatedAgo).UnixMilli(), id); err != nil {
		t.Fatalf("调整会话时间戳失败: %v", err)
	}
}

// TestListStaleUploadsByLastActivity 锁定过期判定看的是**最后一次有动静**（updated_at），
// 而不是创建时间。
//
// 真实场景：一个 100G 的目录传了一整天，刚越过 24h 那条线时还在传 —— 按 created_at 判，
// GC 会把它的暂存目录从**正在进行的上传**脚下删掉，用户看到的是"传到一半突然全失败"。
// 反过来，真被放弃的会话（分块也不来了）必须在 24h 后被回收，否则暂存只涨不落。
func TestListStaleUploadsByLastActivity(t *testing.T) {
	s := newUploadStore(t)
	ctx := context.Background()
	// cutoff 由调用方给（app.GCStaleUploads 传的就是 24h 那条线），store 不持有策略。
	cutoff := time.Now().Add(-24 * time.Hour).UnixMilli()

	// 传了一整天、还在继续：created_at 早就过线，updated_at 刚刚才动过。
	active := seedUploadRow(t, s, "active", domain.UploadStateOpen, `D:\staging\a`)
	ageUploadRow(t, s, active, 30*time.Hour, time.Minute)
	// 被放弃的：两个时间戳都老了。
	abandoned := seedUploadRow(t, s, "abandoned", domain.UploadStateOpen, `D:\staging\b`)
	ageUploadRow(t, s, abandoned, 30*time.Hour, 25*time.Hour)
	// 建库失败留下的：同样属于"没人管了"。
	failed := seedUploadRow(t, s, "failed", domain.UploadStateFailed, `D:\staging\c`)
	ageUploadRow(t, s, failed, 40*time.Hour, 30*time.Hour)
	// 已完成的：不归这一趟（它的暂存由 ListFinishedUploadsWithStaging 兜底回收）。
	finished := seedUploadRow(t, s, "finished", domain.UploadStateComplete, `D:\staging\d`)
	ageUploadRow(t, s, finished, 40*time.Hour, 30*time.Hour)

	got, err := s.ListStaleUploads(ctx, cutoff, 100)
	if err != nil {
		t.Fatalf("查询过期会话失败: %v", err)
	}
	ids := map[string]bool{}
	for _, u := range got {
		ids[u.ID] = true
	}
	if ids[active] {
		t.Fatal("仍在续传的会话（只是创建得早）不该被判为过期")
	}
	if ids[finished] {
		t.Fatal("已完成的会话不该走这一趟")
	}
	if !ids[abandoned] || !ids[failed] {
		t.Fatalf("被放弃的会话应被判为过期，实际返回 %d 条", len(got))
	}
	if len(got) != 2 {
		t.Fatalf("过期会话条数 = %d，期望 2", len(got))
	}
}

// TestListFinishedUploadsWithStaging 锁定"已结束但暂存还挂着"这一趟的口径：它是 copy 模式
// 老残留（以及任何一次删除失败）的唯一回收口，判错的代价是磁盘只涨不落。
//
//   - 只收 complete / aborted：未完成会话归另一趟，两趟的窗口不能交叉（不然会重复删行）；
//   - 只收 staging_dir 非空的：清过的不再进窗口，否则每轮 LIMIT 都是同一批、后面的饿死；
//   - 有宽限期（updated_at 早于 cutoff）：完成流程"先标记 complete、再删目录"，那一瞬间
//     目录必然还在，没有宽限期就是 GC 与收尾动作同时对着同一棵树 RemoveAll。
func TestListFinishedUploadsWithStaging(t *testing.T) {
	s := newUploadStore(t)
	ctx := context.Background()
	cutoff := time.Now().Add(-time.Hour).UnixMilli()

	done := seedUploadRow(t, s, "done", domain.UploadStateComplete, `D:\staging\done`)
	ageUploadRow(t, s, done, 5*time.Hour, 2*time.Hour)
	aborted := seedUploadRow(t, s, "aborted", domain.UploadStateAborted, `D:\staging\aborted`)
	ageUploadRow(t, s, aborted, 5*time.Hour, 2*time.Hour)
	// 刚结束的：还在宽限期内（完成收尾可能正在删）。
	fresh := seedUploadRow(t, s, "fresh", domain.UploadStateComplete, `D:\staging\fresh`)
	// 未完成的：另一趟的活。
	open := seedUploadRow(t, s, "open", domain.UploadStateOpen, `D:\staging\open`)
	ageUploadRow(t, s, open, 5*time.Hour, 2*time.Hour)
	// 已经清过 staging_dir 的：不该再进窗口。
	cleared := seedUploadRow(t, s, "cleared", domain.UploadStateComplete, `D:\staging\cleared`)
	ageUploadRow(t, s, cleared, 5*time.Hour, 2*time.Hour)
	if err := s.ClearUploadStagingDir(ctx, cleared); err != nil {
		t.Fatalf("清空暂存路径失败: %v", err)
	}

	got, err := s.ListFinishedUploadsWithStaging(ctx, cutoff, 100)
	if err != nil {
		t.Fatalf("查询残留暂存失败: %v", err)
	}
	ids := map[string]string{}
	for _, r := range got {
		ids[r.ID] = r.StagingDir
	}
	if len(got) != 2 {
		t.Fatalf("残留条数 = %d，期望 2（complete + aborted）", len(got))
	}
	if ids[done] != `D:\staging\done` || ids[aborted] != `D:\staging\aborted` {
		t.Fatalf("返回的暂存路径不对：%+v", ids)
	}
	for name, id := range map[string]string{"宽限期内": fresh, "未完成": open, "已清空": cleared} {
		if _, ok := ids[id]; ok {
			t.Fatalf("%s 的会话不该进这一趟", name)
		}
	}

	// limit 生效：一轮只处理这么多，剩下的下一轮（避免一次删几百棵树把盘打满）。
	if one, err := s.ListFinishedUploadsWithStaging(ctx, cutoff, 1); err != nil || len(one) != 1 {
		t.Fatalf("limit=1 应只返回 1 条，实际 %d 条 err=%v", len(one), err)
	}
}

// TestClearUploadStagingDirKeepsRowAndTimestamp 锁定清空暂存路径的副作用面：
// 会话行必须留着（它是这次上传的记录，也参与"同名残留"判定），且**不动 updated_at**
// —— 那一列的语义是"这个会话最后有上传活动"（过期判定、"刚活跃过"判定都读它），
// 不是通用的行修改时间。同时必须幂等：GC 与其它收尾路径可能同时收尾同一条会话。
func TestClearUploadStagingDirKeepsRowAndTimestamp(t *testing.T) {
	s := newUploadStore(t)
	ctx := context.Background()

	id := seedUploadRow(t, s, "done", domain.UploadStateComplete, `D:\staging\done`)
	ageUploadRow(t, s, id, 5*time.Hour, 2*time.Hour)
	before, err := s.GetUpload(ctx, id)
	if err != nil {
		t.Fatalf("读会话失败: %v", err)
	}

	if err := s.ClearUploadStagingDir(ctx, id); err != nil {
		t.Fatalf("清空暂存路径失败: %v", err)
	}
	after, err := s.GetUpload(ctx, id)
	if err != nil {
		t.Fatalf("清空暂存路径不该删掉会话行: %v", err)
	}
	if after.StagingDir != "" {
		t.Fatalf("staging_dir = %q，期望已清空", after.StagingDir)
	}
	if after.State != domain.UploadStateComplete {
		t.Fatalf("会话状态被改动了: %q", after.State)
	}
	if after.UpdatedAt != before.UpdatedAt {
		t.Fatalf("updated_at 被改动了：%d → %d（它的语义是「最后有上传活动」）", before.UpdatedAt, after.UpdatedAt)
	}
	// 幂等：行已经不在时静默返回。
	if err := s.ClearUploadStagingDir(ctx, "no-such-upload"); err != nil {
		t.Fatalf("对不存在的会话应静默返回，实际: %v", err)
	}
}
