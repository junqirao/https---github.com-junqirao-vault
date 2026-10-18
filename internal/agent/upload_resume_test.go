package agent

import (
	"path/filepath"
	"testing"
)

// TestUploadResumeStoreCheckpointKeepsParams 锁定进度检查点"只动进度、不动入参"。
//
// 真实反馈："一个任务上传中的，我关掉应用之后就再没有展示那个记录了"——修法是让续传记录
// 兼作"重启后的进度检查点"，于是传输过程中要反复更新它。这里盯住两个容易写坏的点：
//   - 更新进度**不能把入参写丢**（模式 / 存储 ID / 配额 / 共享盘限制 / 目标服务端）：
//     丢了的话，重启后续传要么落到别的服务端，要么把用户填的限制静默清成"不限"；
//   - 没有对应记录时什么都不做（别凭空造一条没有入参的空记录出来）。
func TestUploadResumeStoreCheckpointKeepsParams(t *testing.T) {
	path := filepath.Join(t.TempDir(), uploadsFileName)
	store := newUploadResumeStore(path)
	rec := uploadResumeRecord{
		UploadID:   "up-1",
		LocalDir:   `D:\src\win10`,
		RepoName:   "win10-base",
		TotalBytes: 35 << 30,
		TotalFiles: 1200,
		StartedAt:  1_700_000_000_000,
		ServerKey:  "srv-a",
		RepoMode:   "shared",
		StorageID:  "st-9",
		QuotaBytes: 40 << 30,
		SourceMode: "move",
	}
	if err := store.save(rec); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := store.checkpoint(rec.LocalDir, rec.RepoName, UploadStateUploading, 12<<30, 400, 1180); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	// 重新读盘：检查点必须真的落到了文件里（不然重启后读到的是旧值）。
	got, ok := newUploadResumeStore(path).find(rec.LocalDir, rec.RepoName)
	if !ok {
		t.Fatalf("重新读盘后找不到记录")
	}
	if got.UploadedBytes != 12<<30 || got.FilesDone != 400 || got.FilesTotal != 1180 || got.Phase != UploadStateUploading {
		t.Fatalf("检查点没落盘：uploaded=%d files=%d/%d phase=%q", got.UploadedBytes, got.FilesDone, got.FilesTotal, got.Phase)
	}
	if got.RepoMode != "shared" || got.StorageID != "st-9" || got.QuotaBytes != 40<<30 ||
		got.SourceMode != "move" || got.ServerKey != "srv-a" {
		t.Fatalf("检查点把入参写丢了：%+v", got)
	}
	// 没有这条记录时静默返回：调用点在传输热路径上，不该因为"记录没了"而报错或造记录。
	if err := store.checkpoint(`D:\not-there`, "nope", UploadStateUploading, 1, 1, 1); err != nil {
		t.Fatalf("checkpoint 对不存在的记录应静默返回，实际 %v", err)
	}
	if n := len(store.all()); n != 1 {
		t.Fatalf("记录数 = %d，期望 1（不该凭空多出一条）", n)
	}
}

// TestUploadManagerRestoreTasks 锁定"关掉应用再打开，那条上传还得在列表里"。
//
// 这是本次修复的核心：任务原本只活在内存里，重启后 GET /agent/uploads 返回空，界面那行
// "上传中"整行消失 —— 服务端那一刻只有未完成的上传会话与已传分块（建库任务还没提交，
// 连库记录都没有），于是列表里既没有库也没有上传，用户看到的是"我的上传不见了"。
//
// 三条口径一起钉住：
//   - 摆回来的是**暂停态**（不自动接着传：用户可能正是为了停它才关的应用）；
//   - 带着**上次的进度检查点**（从 0 起会让人以为传上去的几十个 G 白费了）；
//   - 已经提交建库（Phase=completing）的**不摆**：那一刻服务端已经有这个库，列表里是
//     普通的"建库中"行，再摆一条就是同一个库两行、还多一个会把数据重传一遍的「继续」。
func TestUploadManagerRestoreTasks(t *testing.T) {
	path := filepath.Join(t.TempDir(), uploadsFileName)
	store := newUploadResumeStore(path)
	pending := uploadResumeRecord{
		UploadID:      "up-pending",
		LocalDir:      `D:\src\win10`,
		RepoName:      "win10-base",
		TotalBytes:    35 << 30,
		TotalFiles:    1200,
		StartedAt:     1_700_000_000_000,
		ServerKey:     "srv-a",
		RepoMode:      "shared",
		UploadedBytes: 12 << 30,
		FilesDone:     400,
		FilesTotal:    1180,
		Phase:         UploadStateUploading,
	}
	submitted := uploadResumeRecord{
		UploadID:   "up-submitted",
		LocalDir:   `D:\src\win7`,
		RepoName:   "win7-base",
		TotalBytes: 20 << 30,
		TotalFiles: 800,
		StartedAt:  1_700_000_100_000,
		Phase:      UploadStateCompleting,
	}
	broken := uploadResumeRecord{LocalDir: `D:\src\half`, RepoName: "half"}
	for _, rec := range []uploadResumeRecord{pending, submitted, broken} {
		if err := store.save(rec); err != nil {
			t.Fatalf("save(%s): %v", rec.UploadID, err)
		}
	}

	m := newUploadManager(path)
	if n := m.restoreTasks(); n != 1 {
		t.Fatalf("回填条数 = %d，期望 1（只有未提交建库的那条）", n)
	}
	items := m.list()
	if len(items) != 1 {
		t.Fatalf("列表条数 = %d，期望 1", len(items))
	}
	got := items[0]
	if got.UploadID != pending.UploadID || got.RepoName != pending.RepoName || got.LocalDir != pending.LocalDir {
		t.Fatalf("回填的身份不对：%+v", got)
	}
	if got.State != UploadStateUploading || !got.Paused {
		t.Fatalf("回填的状态 = %q paused=%v，期望 uploading + 暂停", got.State, got.Paused)
	}
	if got.UploadedBytes != pending.UploadedBytes || got.FilesDone != pending.FilesDone ||
		got.FilesTotal != pending.FilesTotal || got.TotalBytes != pending.TotalBytes {
		t.Fatalf("回填的进度检查点不对：%+v", got)
	}
	if got.StartedAt != pending.StartedAt {
		t.Fatalf("StartedAt = %d，期望 %d（列表按它排序）", got.StartedAt, pending.StartedAt)
	}
	task, ok := m.find(pending.UploadID)
	if !ok || !task.restoredTask() {
		t.Fatalf("回填的任务应能被 find 找到且标记为待继续")
	}
	// 再跑一次是幂等的（代理里只调一次，但接口本身不该把同一条摆两遍）。
	if n := m.restoreTasks(); n != 0 {
		t.Fatalf("重复回填条数 = %d，期望 0", n)
	}
	if items := m.list(); len(items) != 1 {
		t.Fatalf("重复回填后列表条数 = %d，期望 1", len(items))
	}
}

// TestUploadTaskClaimResume 锁定"待继续"任务被认领时的状态翻转。
//
// 它与"暂停中的任务 resume"必须分开：那个是打开闸门（close(gate)，对没有 goroutine 的
// 回填任务是 nil，会 panic），这个是把任务交回给新起的传输。
func TestUploadTaskClaimResume(t *testing.T) {
	task := &uploadTask{
		restored: true,
		paused:   true,
		state: UploadState{
			UploadID:   "up-1",
			State:      UploadStateUploading,
			Paused:     true,
			EtaSeconds: 42,
		},
	}
	// 暂停按钮对它是空操作：已经在暂停态，也不该给它造一个没人等的闸门。
	if task.pause() {
		t.Fatalf("待继续的任务不该能再次暂停")
	}
	// 运行中任务的 resume 也必须拒绝它（close(nil) 会 panic）。
	if task.resume() {
		t.Fatalf("待继续的任务不该走 resume()")
	}
	if !task.claimResume() {
		t.Fatalf("第一次认领应成功")
	}
	if task.restoredTask() {
		t.Fatalf("认领后不该再是待继续任务")
	}
	snap := task.snapshot()
	if snap.Paused || snap.State != UploadStateScanning {
		t.Fatalf("认领后状态 = %q paused=%v，期望 scanning + 非暂停（要重新扫目录）", snap.State, snap.Paused)
	}
	if snap.EtaSeconds != 0 {
		t.Fatalf("认领后剩余时间 = %d，期望 0（重扫之前那一秒的估算没有意义）", snap.EtaSeconds)
	}
	if task.claimResume() {
		t.Fatalf("重复认领应失败：连点两下会并行起两个传输")
	}
}
