package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"vault/internal/domain"
)

// TestServerJobEventWireContract 锁定线上取值：服务端发的任务事件与代理常量/判定必须逐字相同。
//
// 与挂载阶段同理（见 TestServerMountPhaseWireContract）：字面量分布在 cmd/vault-server 与
// internal/agent 两侧，没有共享常量兜底，改了一边不改另一边就会表现为"上传卡在建库不动"——
// 一个既不报错也不推进的静默故障。
func TestServerJobEventWireContract(t *testing.T) {
	// 事件名（topic）：与 cmd/vault-server/main.go 的 jobs.OnProgress / OnFinished 必须一致；
	// 本地那条（推给渲染层）也刻意同名，见 localJobEventType 的注释。
	if serverJobEventType != "job" {
		t.Fatalf("服务端任务事件名 = %q，期望 job", serverJobEventType)
	}
	if localJobEventType != "job" {
		t.Fatalf("本地任务事件名 = %q，期望 job", localJobEventType)
	}
	// 渲染层解析器认的就是这个字面量（frontend/packages/ui/src/api/agentClient.ts 的 case 'job'）。
	if localJobEventType != serverJobEventType {
		t.Fatalf("两侧事件名不一致（%q vs %q）：界面会一条都收不到", localJobEventType, serverJobEventType)
	}

	// 载荷字段名：服务端发的是 job_id（不是 serverJob 的 id），且这条 data 会被原样转发给渲染层。
	var payload jobEvent
	raw := json.RawMessage(`{"job_id":"job-1","type":"upload_complete","ref_id":"upload-1","state":"running","progress":42}`)
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("解析任务事件失败：%v", err)
	}
	if payload.JobID != "job-1" || payload.Type != "upload_complete" || payload.RefID != "upload-1" ||
		payload.State != "running" || payload.Progress != 42 {
		t.Fatalf("字段名与解析结果不符：%+v（服务端改过 key 就会解析成空 → 等待方永远收不到进度，界面进度栏也永远不动）", payload)
	}
	// 转发给渲染层的 JSON key 必须与上面一致（改 tag 就会静默丢掉界面进度）。
	out, err := json.Marshal(jobEvent{JobID: "job-1", Type: "install_deps", RefID: "deps", State: "running", Progress: 7})
	if err != nil {
		t.Fatalf("序列化任务事件失败：%v", err)
	}
	if string(out) != `{"job_id":"job-1","type":"install_deps","ref_id":"deps","state":"running","progress":7}` {
		t.Fatalf("本地事件载荷形状变了：%s", out)
	}

	// 终态取值：与 domain.JobState 的线上表示一致（uploadJobFinished 里是字面量）。
	for _, want := range []string{string(domain.JobStateSucceeded), string(domain.JobStateFailed), string(domain.JobStateCancelled)} {
		if !uploadJobFinished(&serverJob{ID: "job-1", State: want}) {
			t.Fatalf("状态 %q 应被判为终态（否则等待方会一直等下去）", want)
		}
	}
	for _, other := range []string{string(domain.JobStatePending), string(domain.JobStateRunning)} {
		if uploadJobFinished(&serverJob{ID: "job-1", State: other}) {
			t.Fatalf("状态 %q 不是终态", other)
		}
	}
	// 大小写不同即视为非终态：服务端从不发 "Succeeded"，代理也不该"宽容"到认它。
	if uploadJobFinished(&serverJob{State: "Succeeded"}) {
		t.Fatal("终态判定必须区分大小写（宽容匹配会掩盖两侧口径漂移）")
	}
	if uploadJobFinished(nil) {
		t.Fatal("nil 任务不是终态")
	}
}

// TestHandleServerEventDispatchesJobProgress 锁定"服务端任务事件 → 订阅者（等待方）"的接线。
//
// 这是"上传不再刷屏服务端日志"的入口：进度由事件推来，而不是代理每 2 秒去问一次。
func TestHandleServerEventDispatchesJobProgress(t *testing.T) {
	a := &Agent{logger: testLogger(), hub: NewEventHub(), jobWatch: newJobWatch()}

	subID, events := a.jobWatch.subscribe("job-1")
	defer a.jobWatch.unsubscribe("job-1", subID)

	// 同一条事件也必须进本地事件流（渲染层的数据来源，见 TestHandleServerEventBroadcastsJobToHub）。
	a.handleServerEvent("https://10.0.0.1:8443", serverJobEventType,
		json.RawMessage(`{"job_id":"job-1","type":"upload_complete","ref_id":"upload-1","state":"running","progress":37}`))

	select {
	case got := <-events:
		if got.ID != "job-1" || got.State != "running" || got.Progress != 37 {
			t.Fatalf("事件未按预期透传：%+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("订阅者没收到任务事件（说明此刻代理只能靠轮询，服务端日志会被刷屏）")
	}

	// 缺 job_id 的事件必须丢弃：不知道是谁的任务，投给谁都是错的。
	a.handleServerEvent("https://10.0.0.1:8443", serverJobEventType, json.RawMessage(`{"state":"running"}`))
	select {
	case got := <-events:
		t.Fatalf("缺 job_id 的事件不应投递，实际收到 %+v", got)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestHandleServerEventBroadcastsJobToHub 锁定"服务端任务事件 → 渲染层"的广播。
//
// 界面（"一键安装系统依赖"那类任务）过去每 1.5s 查一次 GET /v1/jobs/{id}，服务端访问日志
// 同样被刷屏。渲染层订阅的是本地事件流 GET /agent/events，这条广播就是它的数据来源：
// 少了它，界面会一直停在初始进度上（既不报错也不前进）。
func TestHandleServerEventBroadcastsJobToHub(t *testing.T) {
	a := &Agent{logger: testLogger(), hub: NewEventHub(), jobWatch: newJobWatch()}

	subID, events := a.hub.Subscribe()
	defer a.hub.Unsubscribe(subID)

	a.handleServerEvent("https://10.0.0.1:8443", serverJobEventType,
		json.RawMessage(`{"job_id":"job-9","type":"install_deps","ref_id":"deps","state":"running","progress":64}`))

	select {
	case got := <-events:
		if got.Type != localJobEventType {
			t.Fatalf("本地事件类型 = %q，期望 %q（渲染层按这个名字分发）", got.Type, localJobEventType)
		}
		payload, ok := got.Data.(jobEvent)
		if !ok {
			t.Fatalf("本地事件载荷类型 = %T，期望 jobEvent", got.Data)
		}
		if payload.JobID != "job-9" || payload.Type != "install_deps" || payload.Progress != 64 {
			t.Fatalf("广播的载荷不对：%+v", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("渲染层没收到任务事件广播")
	}

	// 缺 job_id 的事件同样不该广播（渲染层会把它记成一条空 ID 的任务）。
	a.handleServerEvent("https://10.0.0.1:8443", serverJobEventType, json.RawMessage(`{"state":"running","progress":1}`))
	select {
	case got := <-events:
		t.Fatalf("缺 job_id 的事件不应广播，实际收到 %+v", got)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestJobWatchDeliversOnlyToMatchingJob 订阅只认自己的任务 ID。
//
// 代理是常驻进程：同一个服务端上别人的建库进度也会广播过来。串台会让界面把别人的进度
// 显示成自己的（"我上传的那个库怎么自己走到 80% 又退回 10% 了"）。
func TestJobWatchDeliversOnlyToMatchingJob(t *testing.T) {
	w := newJobWatch()
	id1, ch1 := w.subscribe("job-1")
	defer w.unsubscribe("job-1", id1)
	id2, ch2 := w.subscribe("job-1")
	defer w.unsubscribe("job-1", id2)
	idOther, chOther := w.subscribe("job-2")
	defer w.unsubscribe("job-2", idOther)

	w.publish(serverJob{ID: "job-1", Progress: 5})

	for i, ch := range []<-chan serverJob{ch1, ch2} {
		select {
		case got := <-ch:
			if got.ID != "job-1" || got.Progress != 5 {
				t.Fatalf("订阅者 %d 收到 %+v", i, got)
			}
		default:
			t.Fatalf("订阅者 %d 没收到自己任务的事件", i)
		}
	}
	select {
	case got := <-chOther:
		t.Fatalf("别的任务的事件串台了：%+v", got)
	default:
	}

	// 退订后不再投递（等待方返回时必须清干净，否则会攒住无人读取的通道）。
	w.unsubscribe("job-1", id1)
	w.publish(serverJob{ID: "job-1", Progress: 6})
	select {
	case got := <-ch1:
		t.Fatalf("已退订的订阅者仍收到事件：%+v", got)
	default:
	}
	if got := <-ch2; got.Progress != 6 {
		t.Fatalf("未退订的订阅者应继续收到，实际 %+v", got)
	}
}

// TestJobWatchDropsWhenSubscriberStalls 订阅通道满时丢弃而不是阻塞。
//
// 投递发生在 SSE 读取协程里：这里一阻塞，**整台服务端**的所有事件（含踢下线、挂载阶段）
// 都会排在后面烂掉 —— 一个不读进度的界面不该拖垮整条通道。代价由兜底查询兜住。
func TestJobWatchDropsWhenSubscriberStalls(t *testing.T) {
	w := newJobWatch()
	id, ch := w.subscribe("job-1")
	defer w.unsubscribe("job-1", id)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < jobEventBuffer+5; i++ {
			w.publish(serverJob{ID: "job-1", Progress: i})
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("publish 被阻塞了：满通道必须丢弃，否则会拖住整条 SSE 读取协程")
	}

	// 通道里装的是"最早的那几条"（满即丢新），数量恰好等于缓冲容量。
	n := 0
	for {
		select {
		case <-ch:
			n++
			continue
		default:
		}
		break
	}
	if n != jobEventBuffer {
		t.Fatalf("应恰好留下 %d 条，实际 %d", jobEventBuffer, n)
	}
}

// TestJobWatchNilSafe 零值/未初始化的 jobWatch 不得 panic。
//
// 测试里构造裸 Agent 是常态（见 upload_progress_test.go），事件到达时 jobWatch 可能没被
// New 初始化；那时"没有订阅者"是正确行为，而不是崩溃。
func TestJobWatchNilSafe(t *testing.T) {
	var w *jobWatch
	if id, ch := w.subscribe("job-1"); id != 0 || ch != nil {
		t.Fatalf("nil 订阅应返回 (0, nil)，实际 (%d, %v)", id, ch)
	}
	w.unsubscribe("job-1", 1)
	w.publish(serverJob{ID: "job-1"})

	// 空 jobID 同样不订阅：否则会造出一个永远收不到事件的通道。
	if id, ch := w.subscribe("  "); id != 0 || ch != nil {
		t.Fatalf("空 job_id 不应订阅，实际 (%d, %v)", id, ch)
	}
	// 未初始化 subs 的零值结构（非 nil）也要能用。
	zero := &jobWatch{}
	id, ch := zero.subscribe("job-1")
	zero.publish(serverJob{ID: "job-1", State: "succeeded"})
	select {
	case got := <-ch:
		if got.State != "succeeded" {
			t.Fatalf("零值结构投递异常：%+v", got)
		}
	default:
		t.Fatal("零值结构（subs 为 nil）必须能正常订阅与投递")
	}
	zero.unsubscribe("job-1", id)
}

// TestWaitUploadJobEventDriven 锁定等待建库任务的请求次数：事件驱动，不轮询。
//
// 真实反馈："上传时会因为客户端拉上传状态而刷屏服务日志"—— 过去是无条件每 2 秒查一次
// GET /v1/jobs/{id}，一个 10 分钟的建库任务在访问日志里留下 300 行同一件事的流水。
// 这里把请求次数钉死为 2：① 进入时查一次（事件是变化驱动的，不查就只能一直显示 0%）；
// ② 收到终态事件后再查一次（要的是权威对象：失败原因、摘要）。
func TestWaitUploadJobEventDriven(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	// 首查先"挂住"不响应：这样测试能确定"等待方已订阅、但还没进入事件循环"，
	// 事件一定晚于首查被处理，断言不会踩到"事件先到、又被首查覆盖"的时序假象。
	firstRequest := make(chan struct{})
	releaseFirst := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/jobs/job-1" {
			t.Errorf("意外的请求路径 %s", r.URL.Path)
		}
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			close(firstRequest)
			<-releaseFirst
			_, _ = fmt.Fprint(w, `{"id":"job-1","state":"running","progress":10}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"id":"job-1","state":"succeeded","progress":100}`)
	}))
	defer srv.Close()

	client := &serverClient{base: srv.URL, http: srv.Client(), long: srv.Client(), logger: testLogger()}
	a := &Agent{logger: testLogger(), hub: NewEventHub(), jobWatch: newJobWatch()}
	task := &uploadTask{}
	task.update(func(s *UploadState) { s.State = UploadStateCompleting })

	const jobID = "job-1"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	type result struct {
		job *serverJob
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		job, err := a.waitUploadJob(ctx, client, task, jobID)
		resCh <- result{job: job, err: err}
	}()

	// 首查已在途中 = 等待方已订阅（subscribe 在首查之前）。
	select {
	case <-firstRequest:
	case <-time.After(2 * time.Second):
		t.Fatal("等待方没有发出首次状态查询（没有它进度会一直停在 0%）")
	}

	// 中间过程由事件驱动：进度实时透传，且这段期间一次请求都不该发。
	a.handleServerEvent(srv.URL, serverJobEventType, json.RawMessage(`{"job_id":"job-1","state":"running","progress":42}`))
	close(releaseFirst)
	deadline := time.Now().Add(time.Second)
	for task.snapshot().BuildProgress != 42 {
		if time.Now().After(deadline) {
			t.Fatalf("进度事件未透传给任务，实际 %d", task.snapshot().BuildProgress)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := countCalls(&mu, &calls); n != 1 {
		t.Fatalf("进度事件驱动的过程中不应发请求，实际已发 %d 次", n)
	}

	// 终态事件：等待方再查一次拿权威对象后返回。
	a.handleServerEvent(srv.URL, serverJobEventType, json.RawMessage(`{"job_id":"job-1","state":"succeeded","progress":100}`))
	select {
	case res := <-resCh:
		if res.err != nil {
			t.Fatalf("等待建库任务失败：%v", res.err)
		}
		if res.job == nil || res.job.State != "succeeded" {
			t.Fatalf("应返回权威任务对象，实际 %+v", res.job)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("收到终态事件后等待方就该结束了")
	}
	if n := countCalls(&mu, &calls); n != 2 {
		t.Fatalf("整个等待过程只应发生 2 次查询（首查 + 终态确认），实际 %d 次", n)
	}
}

// TestWaitUploadJobStopsWhenContextCancelled 取消（退出页面/关掉代理）时必须立刻返回。
func TestWaitUploadJobStopsWhenContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"job-1","state":"running","progress":1}`)
	}))
	defer srv.Close()

	client := &serverClient{base: srv.URL, http: srv.Client(), long: srv.Client(), logger: testLogger()}
	a := &Agent{logger: testLogger(), hub: NewEventHub(), jobWatch: newJobWatch()}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := a.waitUploadJob(ctx, client, &uploadTask{}, "job-1")
		done <- err
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("取消后应返回错误，而不是假装成功")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("取消后等待方没有退出")
	}
}

// TestWaitUploadJobRejectsEmptyJobID 服务端没给 job_id 时立刻失败，不要无意义地等。
func TestWaitUploadJobRejectsEmptyJobID(t *testing.T) {
	a := &Agent{logger: testLogger(), hub: NewEventHub(), jobWatch: newJobWatch()}
	if _, err := a.waitUploadJob(context.Background(), &serverClient{}, &uploadTask{}, "  "); err == nil {
		t.Fatal("空 job_id 应报错")
	}
}

func countCalls(mu *sync.Mutex, calls *int) int {
	mu.Lock()
	defer mu.Unlock()
	return *calls
}
