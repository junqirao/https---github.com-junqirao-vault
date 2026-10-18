package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"vault/internal/domain"
)

// writeStagingTree 在暂存目录里真的放一份"已上传的目录树"，返回树根。
//
// 断言必须落在**文件**上，而不是"那个空壳目录还在不在"：GC 要清的是"用完的那棵树"，
// 只把目录本身删掉、把几百 G 的文件留下，是同一个 bug 的另一种写法。
func writeStagingTree(t *testing.T, stagingDir string) string {
	t.Helper()
	tree := filepath.Join(stagingDir, "tree", "win10")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatalf("建暂存目录树失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(tree, "install.wim"), make([]byte, 4096), 0o644); err != nil {
		t.Fatalf("写暂存文件失败：%v", err)
	}
	return tree
}

// assertStagingGone 断言暂存已经不在盘上。
func assertStagingGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("期望 %s 已被清理，实际 err=%v", path, err)
	}
}

// assertStagingKept 断言暂存原样还在（正在用的树一根汗毛都不能动）。
func assertStagingKept(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("期望 %s 还在，实际 err=%v", path, err)
	}
}

// TestDiscardStagingAfterComplete 锁定"上传完了，暂存就该删掉"这条语义本身。
//
// 用例特意用 **copy** 模式的会话 —— 那正是原来漏掉的模式（收尾时只在 move 下删），
// 而客户端 source_mode 默认就是 copy：真实反馈里"服务器上几百 G 的 staging"就是这么攒出来的。
//
// 这条只管"删得干净不干净、删完的账对不对"；"收尾时一定会调它"由调用点那一行保证
// （见 runUploadComplete 末尾），而即便那里将来又被加上模式判断，GC 的兜底那趟也会在一小时内
// 把残留收掉 —— 这正是 TestUploadGCRemovesStagingOfFinishedUploads 存在的意义。
func TestDiscardStagingAfterComplete(t *testing.T) {
	svc, st := newUploadNameTestService(t)
	ctx := context.Background()
	user := seedUploadUser(t, st, "tester")

	// seedUploadSession 建的就是 copy 模式会话（见 upload_name_test.go）。
	id, dir := seedUploadSession(t, st, user, "win10-base", 1, 4096, domain.UploadStateComplete)
	tree := writeStagingTree(t, dir)

	u, err := st.GetUpload(ctx, id)
	if err != nil {
		t.Fatalf("读会话失败：%v", err)
	}
	if u.Mode != domain.UploadModeCopy {
		t.Fatalf("用例前提是 copy 模式，实际 %q", u.Mode)
	}
	svc.discardStagingAfterComplete(ctx, u)

	// 删掉的是整棵树（含文件），不只是那个空壳目录。
	assertStagingGone(t, tree)
	assertStagingGone(t, dir)
	// 会话行留着、只把路径清空：行是这次上传的记录，还参与"同名残留"的判定。
	after, err := st.GetUpload(ctx, id)
	if err != nil {
		t.Fatalf("清理暂存不该删掉会话行：%v", err)
	}
	if after.StagingDir != "" {
		t.Fatalf("staging_dir 应被清空，实际 %q", after.StagingDir)
	}
	// 幂等：完成流程可能重跑，目录本就不在（或压根没有暂存路径）时不该出错。
	svc.discardStagingAfterComplete(ctx, after)
	svc.discardStagingAfterComplete(ctx, nil)
}

// TestUploadGCRemovesStagingOfFinishedUploads 锁定"上传完了就用完了，暂存该删掉"。
//
// 真实反馈："staging 目录并没有清空，这个目录的逻辑是什么啊，现在在我的服务器占用了几百 G，
// 我认为上传完了用完了就应该删掉啊"。
//
// 两处根因：
//   - 完成收尾只在 move 模式删暂存（见 runUploadComplete），而客户端 source_mode 默认就是
//     copy —— 几乎每一次成功的上传都在存储根下留下一整棵树；
//   - 24h 的过期扫描**跳过 complete**，于是上面留下的目录谁也不管，只涨不落。
//
// 本用例管第二条（第一条由 runUploadComplete 的收尾负责）：已结束会话残留的暂存必须被回收。
func TestUploadGCRemovesStagingOfFinishedUploads(t *testing.T) {
	svc, st := newUploadNameTestService(t)
	ctx := context.Background()
	user := seedUploadUser(t, st, "tester")

	// 已完成、暂存还挂着的会话：copy 模式留下的那类残骸。
	doneID, doneDir := seedUploadSession(t, st, user, "win10-base", 1200, 35<<30, domain.UploadStateComplete)
	doneTree := writeStagingTree(t, doneDir)
	// 已放弃的会话同理：Abort 那次删除失败（目录被占用）就会留下这种。
	abortID, abortDir := seedUploadSession(t, st, user, "win7-base", 800, 20<<30, domain.UploadStateAborted)
	abortTree := writeStagingTree(t, abortDir)
	// 正在传的会话不归这一趟管：它是"过期会话"那一趟的活，而且此刻就是有人在往里写。
	liveID, liveDir := seedUploadSession(t, st, user, "live", 10, 1<<20, domain.UploadStateOpen)
	liveTree := writeStagingTree(t, liveDir)

	// 刚结束的会话有一小时宽限期：完成流程是"先标记 complete、再删目录"，那一瞬间目录必然还在，
	// 没有宽限期就是 GC 和收尾动作同时对着同一棵树 RemoveAll（Windows 上会报共享冲突）。
	if n, err := svc.GCStaleUploads(ctx); err != nil || n != 0 {
		t.Fatalf("宽限期内不该回收任何东西，n=%d err=%v", n, err)
	}
	assertStagingKept(t, doneTree)

	// 时间往前走 2h（越过 1h 宽限期，但离 24h 的过期线还很远）。
	svc.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	n, err := svc.GCStaleUploads(ctx)
	if err != nil {
		t.Fatalf("GC 失败：%v", err)
	}
	if n != 2 {
		t.Fatalf("回收条目数 = %d，期望 2（complete + aborted 各一条）", n)
	}
	assertStagingGone(t, doneTree)
	assertStagingGone(t, abortTree)
	assertStagingKept(t, liveTree)

	// 会话行**保留**，只把 staging_dir 清空：行是这次上传的记录，还参与"同名残留"的判定
	// （见 prepareUploadName）。清空同时让这条会话离开 GC 的查询窗口 —— 否则每轮 LIMIT
	// 都是同一批（它们的目录早就不存在了），后面的永远轮不上。
	done, err := st.GetUpload(ctx, doneID)
	if err != nil {
		t.Fatalf("已完成会话的行不该被删掉：%v", err)
	}
	if done.StagingDir != "" {
		t.Fatalf("staging_dir 应被清空，实际 %q", done.StagingDir)
	}
	if done.State != domain.UploadStateComplete {
		t.Fatalf("会话状态被改动了：%q", done.State)
	}
	if aborted, err := st.GetUpload(ctx, abortID); err != nil || aborted.StagingDir != "" {
		t.Fatalf("已放弃会话的 staging_dir 应被清空：%+v err=%v", aborted, err)
	}
	if live, err := st.GetUpload(ctx, liveID); err != nil || live.StagingDir != liveDir {
		t.Fatalf("正在传的会话不该被动过：%+v err=%v", live, err)
	}

	// 幂等：下一轮无事可做（清了 staging_dir 之后再查是空的）。
	if n, err := svc.GCStaleUploads(ctx); err != nil || n != 0 {
		t.Fatalf("重复 GC 应为空转，n=%d err=%v", n, err)
	}
}

// TestUploadGCSkipsSessionWithLiveBuildJob 锁定"建库任务还在跑时不碰它的暂存"。
//
// 那棵树正是任务此刻在拷的源（robocopy staging tree → 盘），删了就是"从活人脚下抽地毯"：
// 用户会在"建库中"看到上传整行消失，任务随即因为源目录不见而失败。
//
// 这道判断不是可有可无的：过期线按"最后一次收到分块"（updated_at）算，而字节传完之后
// 建库本身可能还要跑很久（源多大就要写多大的盘），一个卡住的建库任务完全可能拖过 24h。
func TestUploadGCSkipsSessionWithLiveBuildJob(t *testing.T) {
	svc, st := newUploadNameTestService(t)
	ctx := context.Background()
	user := seedUploadUser(t, st, "tester")

	// 字节已传完、建库中（verifying）的会话。
	id, dir := seedUploadSession(t, st, user, "building", 100, 30<<30, domain.UploadStateVerifying)
	tree := writeStagingTree(t, dir)
	jobID, _, err := st.CreateJob(ctx, &domain.Job{
		Type:    domain.JobUploadComplete,
		RefID:   id,
		IdemKey: uploadCompleteIdemKey(id),
		State:   domain.JobStateRunning,
	})
	if err != nil {
		t.Fatalf("建建库任务失败：%v", err)
	}

	// 时间推过 24h：会话"看着"早就废了，但任务还活着 —— 一律不碰。
	svc.now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	n, err := svc.GCStaleUploads(ctx)
	if err != nil {
		t.Fatalf("GC 失败：%v", err)
	}
	if n != 0 {
		t.Fatalf("建库中的会话不该被回收，实际回收 %d 条", n)
	}
	assertStagingKept(t, tree)
	if u, err := st.GetUpload(ctx, id); err != nil || u.StagingDir != dir {
		t.Fatalf("建库中的会话行不该被动过：%+v err=%v", u, err)
	}

	// 任务跑到终态之后，同一棵树就该被回收了：这道判断只挡"任务还活着"的那段时间。
	//
	// 挡它的不再是上面那条（会话是 verifying、不在 pass 2 的窗口里），而是"24h 没有动静的
	// 未完成会话"那一趟：字节传完就再没有分块进来，任务结束那一刻 updated_at 已经停在 25h 前
	// —— 连会话行一起清掉，客户端那边留着的续传记录下次重传会重新开会话。
	if err := st.FinishJob(ctx, jobID, domain.JobStateSucceeded, ""); err != nil {
		t.Fatalf("结束任务失败：%v", err)
	}
	if n, err := svc.GCStaleUploads(ctx); err != nil || n != 1 {
		t.Fatalf("任务结束后应回收该会话，n=%d err=%v", n, err)
	}
	assertStagingGone(t, tree)
	if _, err := st.GetUpload(ctx, id); err == nil {
		t.Fatalf("超期未完成的会话应连行一起清掉")
	}
}
