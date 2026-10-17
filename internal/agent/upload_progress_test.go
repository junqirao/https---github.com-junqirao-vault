package agent

import (
	"testing"
	"time"
)

// TestIsTerminalServerUploadState 锁定"哪些服务端会话不能再续传"。
//
// failed 必须算终态：那种会话的暂存已被判定没救，本地记录还留着的话，agent 会一直
// 往一个死会话里续传 —— 用户看到的正是"同一个名字点了就失败"。
func TestIsTerminalServerUploadState(t *testing.T) {
	cases := []struct {
		state string
		want  bool
	}{
		{state: "complete", want: true},
		{state: "aborted", want: true},
		{state: "failed", want: true},
		{state: "open", want: false},
		{state: "verifying", want: false},
		{state: "", want: false},
	}
	for _, c := range cases {
		if got := isTerminalServerUploadState(c.state); got != c.want {
			t.Fatalf("isTerminalServerUploadState(%q) = %v，期望 %v", c.state, got, c.want)
		}
	}
}

// TestUploadFileAt 覆盖"这个分块属于哪个文件"的展示用定位。
//
// 它只服务于界面上那一行「当前文件」：4 路并发下必然是近似的，但必须便宜（二分）且绝不越界。
func TestUploadFileAt(t *testing.T) {
	plan := []uploadPlanFile{
		{Rel: "a.bin", Offset: 0, Size: 5},
		{Rel: "b.bin", Offset: 5, Size: 7},
		{Rel: "c.bin", Offset: 12, Size: 3},
	}
	cases := []struct {
		offset int64
		want   string
	}{
		{offset: 0, want: "a.bin"},
		{offset: 4, want: "a.bin"},
		// 边界归属后一个文件：这是"块 → 文件"的唯一定义（见 app.locateFiles 的口径）。
		{offset: 5, want: "b.bin"},
		{offset: 11, want: "b.bin"},
		{offset: 12, want: "c.bin"},
		{offset: 15, want: ""},
	}
	for _, c := range cases {
		if got := uploadFileAt(plan, c.offset); got != c.want {
			t.Fatalf("uploadFileAt(offset=%d) = %q，期望 %q", c.offset, got, c.want)
		}
	}
	if got := uploadFileAt(nil, 0); got != "" {
		t.Fatalf("空计划应返回空串，实际 %q", got)
	}
}

// TestReportBuildProgress 覆盖"建库进度必须透传给界面"。
//
// 真实反馈："30G 上传卡在 100% 上等了半小时"——提交建库那一刻字节进度就满了，而建库是另一件
// 活（把刚上传的整棵树再写一份进虚拟盘）。客户端此前只盯任务的 state、把 j.Progress 丢掉，
// 界面就只剩一句"校验中"钉在不动的进度条上。这里同时锁住两条口径：
//   - 数值变化 → 写入状态并推一条事件（界面才动得起来）；
//   - 数值没变 → 不推（轮询每两秒一次，重复推送只是白重渲染）；0 不覆盖已有进度。
func TestReportBuildProgress(t *testing.T) {
	a := &Agent{hub: NewEventHub()}
	subID, events := a.hub.Subscribe()
	defer a.hub.Unsubscribe(subID)
	task := &uploadTask{}
	task.update(func(s *UploadState) { s.State = UploadStateCompleting })

	a.reportBuildProgress(task, &serverJob{State: "running", Progress: 37})
	if got := task.snapshot().BuildProgress; got != 37 {
		t.Fatalf("建库进度应为 37，实际 %d", got)
	}
	select {
	case ev := <-events:
		state, ok := ev.Data.(UploadState)
		if !ok || state.BuildProgress != 37 {
			t.Fatalf("进度事件应带最新状态，实际 %+v", ev.Data)
		}
	case <-time.After(time.Second):
		t.Fatal("进度变化必须推送事件（否则界面永远停在提交时的那一帧）")
	}

	// 数值没变：不得再推。
	a.reportBuildProgress(task, &serverJob{State: "running", Progress: 37})
	select {
	case ev := <-events:
		t.Fatalf("进度未变化不应推送事件，却收到 %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}

	// 0（任务刚提交、进度还没写进来）与 nil 都不得覆盖已有进度。
	a.reportBuildProgress(task, &serverJob{State: "running"})
	a.reportBuildProgress(task, nil)
	if got := task.snapshot().BuildProgress; got != 37 {
		t.Fatalf("进度 0 / 空任务不得回退已有进度，实际 %d", got)
	}
}

// TestTransferFileCount 覆盖"已校验 N/M"里的 M：只数非空文件。
//
// 空文件不占拼接流、也不会被任何分块写成，把它们算进 M 会让进度永远差几格
// （真实观感："都传完了还显示 999/1000"）。
func TestTransferFileCount(t *testing.T) {
	entries := []scanEntry{
		{Rel: "a", Size: 10},
		{Rel: "empty", Size: 0},
		{Rel: "b", Size: 1},
	}
	if got := transferFileCount(entries); got != 2 {
		t.Fatalf("应只数非空文件（2），实际 %d", got)
	}
	if got := transferFileCount(nil); got != 0 {
		t.Fatalf("空清单应为 0，实际 %d", got)
	}
}

// TestMarkVerifiedDeduplicates 覆盖"已校验文件数"的累加口径。
//
// 去重不是可选优化：幂等重试、或服务端因树对不上而作废分块后的自愈重传，都会让同一个文件
// 再次出现在服务端的逐块回报里 —— 直接累加会让 N 越过 M（"已校验 1200/1000 个文件"）。
func TestMarkVerifiedDeduplicates(t *testing.T) {
	p := newTestUploadProgress()
	p.reset(100, 0, 0, 0, 3)

	p.markVerified("a")
	p.markVerified("a") // 同一文件被重复回报（重传/自愈）
	p.markVerified("b")
	if p.filesDone != 2 {
		t.Fatalf("同一路径只应计一次：期望 2，实际 %d", p.filesDone)
	}

	// 越过 M 的回报必须被夹住（宁可少一格，也不能显示"已校验 4/3 个文件"）。
	p.markVerified("c")
	p.markVerified("d")
	if p.filesDone != 3 {
		t.Fatalf("不得超过 M=3，实际 %d", p.filesDone)
	}
}

// newTestUploadProgress 造一个可独立驱动进度簿记的 uploadProgress（无需真实 Agent 任务）。
//
// hub 给一个空事件中心：push 会发布事件，零值 EventHub 发布是空操作，正好够用。
func newTestUploadProgress() *uploadProgress {
	return newUploadProgress(&Agent{hub: NewEventHub()}, &uploadTask{})
}

// TestUploadProgressResetRewindsRate 覆盖续传重置：速率与采样基线必须一起清零。
//
// 不清采样基线会在续传后算出负增量（累计字节被重置到更小的起点），表现为速度忽高忽低或
// 长时间不动 —— 上传任务页上就是一个明显错误的 ETA。
func TestUploadProgressResetRewindsRate(t *testing.T) {
	base := time.Now()
	p := newTestUploadProgress()
	p.reset(1000, 0, 0, 0, 1)
	p.mu.Lock()
	p.sampleAt, p.sampleVal = base, 0
	p.mu.Unlock()
	p.mu.Lock()
	p.sample(base.Add(time.Second), 1000)
	p.mu.Unlock()
	if p.speed != 1000 {
		t.Fatalf("前置条件：速率应为 1000，实际 %v", p.speed)
	}

	// 续传：起点回退到 400 字节，速率与基线必须一起重置。
	p.reset(1000, 400, 1, 0, 1)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.speed != 0 {
		t.Fatalf("续传后速率应清零，实际 %v", p.speed)
	}
	// reset 末尾会立刻推一次进度（force），这次推送顺手采样了一帧新基线：字节基线必须落在
	// 续传起点上（400），否则下一步算出的增量会包含"上一轮已传的那 600 字节"从而虚高。
	if p.sampleVal != 400 {
		t.Fatalf("采样基线应回到续传起点 400，实际 %d", p.sampleVal)
	}
}

// TestEtaSeconds 覆盖上传任务页"预计剩余"的换算口径。
//
// 关键约定：返回 0 表示"不可估算"，前端显示 "--"。宁可显示 -- 也不要给出一个
// 必然错误的数字（低速起步时 naive 计算很容易算出"剩余 400 天"）。
func TestEtaSeconds(t *testing.T) {
	cases := []struct {
		name     string
		total    int64
		uploaded int64
		speed    float64
		want     int64
	}{
		{name: "正常估算", total: 1024, uploaded: 512, speed: 128, want: 4},
		{name: "速度未知 → 不可估算", total: 1024, uploaded: 512, speed: 0, want: 0},
		{name: "已传完 → 不可估算", total: 1024, uploaded: 1024, speed: 128, want: 0},
		{name: "剩余不足 1 秒 → 向上取 1 秒", total: 1000, uploaded: 999, speed: 4096, want: 1},
		{name: "结果离谱（超 30 天）→ 不可估算", total: 1 << 50, uploaded: 0, speed: 1, want: 0},
		{name: "总字节未知（0）→ 不可估算", total: 0, uploaded: 0, speed: 128, want: 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := etaSeconds(c.total, c.uploaded, c.speed); got != c.want {
				t.Fatalf("etaSeconds(%d, %d, %v) = %d, 期望 %d", c.total, c.uploaded, c.speed, got, c.want)
			}
		})
	}
}

// TestUploadProgressSample 覆盖速率采样与平滑。
//
// 为什么必须平滑：分块并发为 4，单个采样窗口里可能一次完成 0 个、也可能完成 4 个分块，
// 瞬时速率能差好几倍；不平滑的话前端 ETA 会来回跳几十分钟。
func TestUploadProgressSample(t *testing.T) {
	base := time.Now()
	p := &uploadProgress{}

	// 第一次采样只建立基线，不产生速率（此刻尚无"两个点"可算）。
	p.sample(base, 0)
	if p.speed != 0 {
		t.Fatalf("首次采样不应算出速率，实际 %v", p.speed)
	}

	// 1 秒内传了 1000 字节 → 首次得到速率 1000 B/s。
	p.sample(base.Add(time.Second), 1000)
	if p.speed != 1000 {
		t.Fatalf("首个窗口速率应为 1000，实际 %v", p.speed)
	}
	if p.sampleVal != 1000 {
		t.Fatalf("采样基线应更新为 1000，实际 %d", p.sampleVal)
	}

	// 下一个 1 秒窗口只传了 200 字节（瞬时 200 B/s）：平滑后
	// speed = 1000*0.6 + 200*0.4 = 680，而不是直接掉到 200。
	p.sample(base.Add(2*time.Second), 1200)
	if got, want := p.speed, 680.0; got != want {
		t.Fatalf("平滑后速率应为 %v，实际 %v", want, got)
	}
}

// TestUploadProgressSampleSkipsShortWindow 覆盖"采样窗口过短不采样"。
//
// 节流允许的最小推送间隔是 200ms，更短的窗口里噪声占主导（一个分块完成就能把
// 瞬时速率放大好几倍），必须跳过而不是采一个假峰值。
func TestUploadProgressSampleSkipsShortWindow(t *testing.T) {
	base := time.Now()
	p := &uploadProgress{}
	p.sample(base, 0)

	// 距基线仅 100ms：跳过采样，基线不动。
	p.sample(base.Add(100*time.Millisecond), 1<<20)
	if p.speed != 0 || p.sampleVal != 0 {
		t.Fatalf("过短窗口不应采样：speed=%v sampleVal=%d", p.speed, p.sampleVal)
	}
}

// TestUploadProgressSampleMonotonicBaseline 覆盖"窗口内无新增字节"的场景。
//
// 并发下完全可能某个窗口一个分块都没完成（比如服务端限流、或只剩最后几个大分块在传）。
// 此时必须保留上一次速率，否则速度会瞬间归零、ETA 闪成 "--"。
func TestUploadProgressSampleMonotonicBaseline(t *testing.T) {
	base := time.Now()
	p := &uploadProgress{}
	p.sample(base, 0)
	p.sample(base.Add(time.Second), 1000) // 速率 1000 B/s

	p.sample(base.Add(2*time.Second), 1000) // 这个窗口没有新增
	if p.speed != 1000 {
		t.Fatalf("窗口无新增时应保留原速率 1000，实际 %v", p.speed)
	}

	// 续传基线回退（累计字节变小）同样不得算出负速率。
	p.sample(base.Add(3*time.Second), 500)
	if p.speed != 1000 {
		t.Fatalf("基线回退时不得算出负速率，实际 %v", p.speed)
	}
}
