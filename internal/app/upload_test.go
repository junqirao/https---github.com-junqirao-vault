package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"vault/internal/domain"
)

// testPlan 造一份"拼接流 → 文件"计划：三个非空文件（含一个跨文件边界的场景）。
//
// sizes 里的 0 表示空文件：它不占拼接流，但仍在计划里（服务端在 complete 补建）。
func testPlan(t *testing.T, sizes ...int64) []uploadFile {
	t.Helper()
	dir := t.TempDir()
	files := make([]uploadFile, 0, len(sizes))
	var off int64
	for i, size := range sizes {
		rel := filepath.Join("d", string(rune('a'+i))+".bin")
		files = append(files, uploadFile{
			RelPath: rel,
			Size:    size,
			Offset:  off,
			OutPath: filepath.Join(dir, rel),
		})
		off += size
	}
	return files
}

// TestWriteChunkToTreeOutOfOrder 是"到一块写一块"的核心回归。
//
// 旧实现把每个分块先存成 N.part，等 complete 再顺序拼成文件树 —— 100GB 的库要在一次 HTTP
// 请求里复制 100GB，客户端必然超时（真实反馈：agent.server_timeout，cause 是 POST /complete）。
// 新实现让分块自带偏移（index × chunk_size）直接 WriteAt 到最终位置，于是：
//
//   - 与到达顺序无关（4 路并发 + 断点续传，顺序本来就不保证）；
//   - 一个分块跨越文件边界（前半段属 A、后半段属 B）也必须各写各的。
func TestWriteChunkToTreeOutOfOrder(t *testing.T) {
	// 布局：A=5 字节、B=7 字节、空文件、C=3 字节；拼接流 = A+B+C（15 字节）。
	files := testPlan(t, 5, 7, 0, 3)
	stream := []byte("AAAAABBBBBBBCCC")
	if int64(len(stream)) != 15 {
		t.Fatalf("测试数据长度应为 15，实际 %d", len(stream))
	}

	// 4 字节一块：offset 0/4/8/12，其中 offset 4 的块跨 A(第 5 字节) 与 B。
	type chunk struct {
		offset int64
		data   []byte
	}
	chunks := []chunk{
		{0, stream[0:4]},
		{4, stream[4:8]},
		{8, stream[8:12]},
		{12, stream[12:15]},
	}
	// 刻意乱序（含倒序）写入：任何对顺序的依赖都会在这里露出来。
	for i := len(chunks) - 1; i >= 0; i-- {
		if err := writeChunkToTree(files, chunks[i].offset, chunks[i].data); err != nil {
			t.Fatalf("写分块 offset=%d 失败：%v", chunks[i].offset, err)
		}
	}

	want := map[int]string{
		0: "AAAAA",
		1: "BBBBBBB",
		3: "CCC",
	}
	for idx, content := range want {
		got, err := os.ReadFile(files[idx].OutPath)
		if err != nil {
			t.Fatalf("读回 %s 失败：%v", files[idx].RelPath, err)
		}
		if string(got) != content {
			t.Fatalf("%s 内容应为 %q，实际 %q", files[idx].RelPath, content, got)
		}
	}
	// 空文件不该被任何分块写出来（由 complete 阶段补建，见 createEmptyFiles）。
	if _, err := os.Stat(files[2].OutPath); !os.IsNotExist(err) {
		t.Fatalf("空文件不应由分块写入创建，实际 err=%v", err)
	}
}

// TestLocateFiles 覆盖"分块落在哪些文件里"的二分定位（热路径：每写一块都要算一次）。
func TestLocateFiles(t *testing.T) {
	files := testPlan(t, 5, 7, 3) // 偏移：A=[0,5) B=[5,12) C=[12,15)
	cases := []struct {
		name   string
		offset int64
		size   int64
		want   []int
	}{
		{name: "完全落在单个文件内", offset: 0, size: 4, want: []int{0}},
		{name: "跨越文件边界 → 两个文件", offset: 4, size: 4, want: []int{0, 1}},
		{name: "边界正好重合 → 只算后一个", offset: 5, size: 2, want: []int{1}},
		{name: "跨三个文件（大块）", offset: 4, size: 11, want: []int{0, 1, 2}},
		{name: "空区间 → 无", offset: 5, size: 0, want: nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := locateFiles(files, c.offset, c.size)
			if len(got) != len(c.want) {
				t.Fatalf("locateFiles(%d, %d) = %v，期望 %v", c.offset, c.size, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("locateFiles(%d, %d) = %v，期望 %v", c.offset, c.size, got, c.want)
				}
			}
		})
	}
}

// TestRelPathAt 覆盖分块登记用的"这个分块属于哪个文件"。
func TestRelPathAt(t *testing.T) {
	files := testPlan(t, 5, 7, 3)
	cases := []struct {
		offset int64
		want   string
	}{
		{offset: 0, want: files[0].RelPath},
		{offset: 4, want: files[0].RelPath},
		{offset: 5, want: files[1].RelPath},
		{offset: 11, want: files[1].RelPath},
		{offset: 12, want: files[2].RelPath},
	}
	for _, c := range cases {
		if got := relPathAt(files, c.offset); got != c.want {
			t.Fatalf("relPathAt(%d) = %q，期望 %q", c.offset, got, c.want)
		}
	}
	// 越界（如空文件占位处）返回空串，不 panic。
	if got := relPathAt(files, 99); got != "" {
		t.Fatalf("越界应返回空串，实际 %q", got)
	}
}

// TestTreeMatchesRejectsSizeMismatch 覆盖 complete 阶段的核对口径：
// 大小不一致（= 内容不完整）与符号链接都必须判为对不上，否则会把半截数据当成成品建库。
func TestTreeMatchesRejectsSizeMismatch(t *testing.T) {
	files := testPlan(t, 5, 7)
	if err := writeChunkToTree(files, 0, []byte("AAAAABBBBBBB")); err != nil {
		t.Fatalf("写分块失败：%v", err)
	}
	if !treeMatches(files) {
		t.Fatal("内容齐全时 treeMatches 应为 true")
	}

	// 截断成 6 字节：大小对不上 → 必须判 false（不能只看"文件存在"）。
	if err := os.Truncate(files[1].OutPath, 6); err != nil {
		t.Fatalf("截断失败：%v", err)
	}
	if treeMatches(files) {
		t.Fatal("大小与清单不符时 treeMatches 必须为 false")
	}
}

// TestRequeueFailedCompleteJob 是"重复创建必定失败"的回归测试。
//
// 事故现场：agent 对同一 local_dir + repo_name 重试上传时会**复用同一 upload_id**
// （续传记录只在建库成功后删除），而上传完成建库任务的幂等键固定为 upload_id。
// 任务一旦失败，Complete 里的 EnqueueWith 就原样返回那条 failed 任务，而 worker 只领取
// pending 任务 —— 终态任务永远不会再跑。于是用户怎么重试都是**秒失败**：
// agent.upload_failed / stage=job / cause="建库任务状态: failed"，而暂存目录其实还完整。
//
// 修复后：failed / cancelled 的任务在 Complete 时被重置回 pending（且 attempt 归零，
// 否则一条已用尽重试预算的任务重置后一失败就又终结）；succeeded / running / pending
// 必须原样保留 —— 前者是真幂等命中，后者正在被 worker 执行，重置会造成重复投递。
func TestRequeueFailedCompleteJob(t *testing.T) {
	const uploadID = "up-1"

	cases := []struct {
		name string
		// seed 为空串表示不预置任务（首次提交）。
		seed string
		// wantState 为空串表示"不应存在任务"。
		wantState string
		// wantAttempt 仅在有任务时有意义。
		wantAttempt int
		// wantRequeue 表示是否应当真的重置（决定返回值与清理断言）。
		wantRequeue bool
	}{
		{
			name:      "首次提交（还没有任务）→ 不凭空创建",
			seed:      "",
			wantState: "",
		},
		{
			name:      "failed（重试用尽）→ 重置为 pending 并归还重试预算",
			seed:      string(domain.JobStateFailed),
			wantState: string(domain.JobStatePending), wantAttempt: 0, wantRequeue: true,
		},
		{
			name:      "cancelled → 同样允许重跑",
			seed:      string(domain.JobStateCancelled),
			wantState: string(domain.JobStatePending), wantAttempt: 0, wantRequeue: true,
		},
		{
			name:      "succeeded → 真幂等命中，保持不动",
			seed:      string(domain.JobStateSucceeded),
			wantState: string(domain.JobStateSucceeded), wantAttempt: 3,
		},
		{
			name:      "running → 正在执行，保持不动",
			seed:      string(domain.JobStateRunning),
			wantState: string(domain.JobStateRunning), wantAttempt: 3,
		},
		{
			name:      "pending → 已排队，保持不动",
			seed:      string(domain.JobStatePending),
			wantState: string(domain.JobStatePending), wantAttempt: 3,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			st := openAppTestStore(t)
			svc := &UploadService{Deps: Deps{
				Store: st,
				Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
			}}

			var jobID string
			if c.seed != "" {
				// 预置一条"重试预算已用尽"（attempt=3 = worker 默认 MaxAttempt）的任务，
				// 并带上进度/结束时间/错误，用来验证重置会把这些一起清干净。
				id, created, err := st.CreateJob(ctx, &domain.Job{
					Type:       domain.JobUploadComplete,
					RefID:      uploadID,
					IdemKey:    uploadCompleteIdemKey(uploadID),
					Payload:    "{}",
					State:      domain.JobState(c.seed),
					Progress:   100,
					Attempt:    3,
					LastError:  "platform.access_denied",
					FinishedAt: 123,
				})
				if err != nil || !created {
					t.Fatalf("预置任务失败: created=%v err=%v", created, err)
				}
				jobID = id
			}

			gotID, err := svc.requeueFailedCompleteJob(ctx, uploadID)
			if err != nil {
				t.Fatalf("requeueFailedCompleteJob 失败: %v", err)
			}
			if c.wantRequeue && gotID != jobID {
				t.Fatalf("应重置任务 %s，实际返回 %q", jobID, gotID)
			}
			if !c.wantRequeue && gotID != "" {
				t.Fatalf("不应重置任何任务，实际返回 %q", gotID)
			}

			if c.wantState == "" {
				if _, err := st.GetJobByIdemKey(ctx, uploadCompleteIdemKey(uploadID)); err == nil {
					t.Fatal("首次提交不应凭空创建任务")
				}
				return
			}

			j, err := st.GetJob(ctx, jobID)
			if err != nil {
				t.Fatalf("回查任务失败: %v", err)
			}
			if string(j.State) != c.wantState {
				t.Fatalf("任务状态应为 %s，实际 %s", c.wantState, j.State)
			}
			if j.Attempt != c.wantAttempt {
				t.Fatalf("attempt 应为 %d，实际 %d", c.wantAttempt, j.Attempt)
			}
			if c.wantRequeue {
				if j.Progress != 0 || j.FinishedAt != 0 || j.LastError != "" {
					t.Fatalf("重置必须清空进度/结束时间/上次错误，实际 %+v", j)
				}
			}
		})
	}
}
