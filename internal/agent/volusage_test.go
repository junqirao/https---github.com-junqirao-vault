package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"vault/internal/apperr"
	"vault/internal/domain"
)

// testClock 是可控时钟（TTL 相关断言不依赖真实时间）。
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func newTestClock() *testClock {
	return &testClock{at: time.Unix(1_700_000_000, 0)}
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// fakeVolumeProbe 记录调用次数与路径，可返回固定结果/错误，也可阻塞模拟慢 PowerShell。
type fakeVolumeProbe struct {
	mu          sync.Mutex
	count       int
	paths       []string
	space       *domain.VolumeSpace
	err         error
	block       chan struct{}
	entered     chan struct{}
	enteredOnce bool
}

func (p *fakeVolumeProbe) probe(_ context.Context, path string) (*domain.VolumeSpace, error) {
	p.mu.Lock()
	p.count++
	p.paths = append(p.paths, path)
	if p.entered != nil && !p.enteredOnce {
		p.enteredOnce = true
		close(p.entered)
	}
	space, err := p.space, p.err
	block := p.block
	p.mu.Unlock()
	if block != nil {
		<-block
	}
	return space, err
}

func (p *fakeVolumeProbe) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.count
}

// newTestVolumeUsageCache 构造被测缓存：挂载记录固定、时钟可控。
func newTestVolumeUsageCache(mounts []MountState, probe *fakeVolumeProbe) (*volumeUsageCache, *testClock) {
	clock := newTestClock()
	c := newVolumeUsageCache(func() []MountState { return mounts }, probe.probe)
	c.now = clock.now
	return c, clock
}

// TestVolumeUsageMeasuresMountedVolumesOnly 锁定取数范围与"已用 = 容量 - 可用"。
//
// 非 mounted 的记录（挂载中/出错/被踢）没有可读的卷，不能出现在结果里；挂载点为空同理。
func TestVolumeUsageMeasuresMountedVolumesOnly(t *testing.T) {
	probe := &fakeVolumeProbe{space: &domain.VolumeSpace{
		Name: "e:", FileSystem: "NTFS", TotalBytes: 100, FreeBytes: 40,
	}}
	mounts := []MountState{
		{AllocationID: "a-mounted", RepoID: "r1", MountPath: "E:", MountMode: "letter", State: MountStateMounted},
		{AllocationID: "a-mounting", RepoID: "r2", MountPath: "F:", State: MountStateMounting},
		{AllocationID: "a-error", RepoID: "r3", MountPath: "G:", State: MountStateError},
		{AllocationID: "a-revoked", RepoID: "r4", MountPath: "H:", State: MountStateRevoked},
		{AllocationID: "a-no-path", RepoID: "r5", State: MountStateMounted},
	}
	cache, _ := newTestVolumeUsageCache(mounts, probe)

	items := cache.snapshot(context.Background())
	if len(items) != 1 {
		t.Fatalf("只该量已挂载且挂载点非空的那一条，实际 %+v", items)
	}
	got := items[0]
	if got.AllocationID != "a-mounted" || got.RepoID != "r1" || got.MountMode != "letter" || got.State != MountStateMounted {
		t.Fatalf("归属字段应原样带出，实际 %+v", got)
	}
	if got.TotalBytes != 100 || got.FreeBytes != 40 || got.UsedBytes != 60 || got.FileSystem != "NTFS" {
		t.Fatalf("用量应是 容量-可用，实际 %+v", got)
	}
	if got.CheckedAt == 0 || got.Error != "" {
		t.Fatalf("成功取数应带实测时刻且无错误，实际 %+v", got)
	}
	if probe.calls() != 1 {
		t.Fatalf("只该探测一次（非 mounted 的不测），实际 %d 次", probe.calls())
	}
	if probe.paths[0] != "E:" {
		t.Fatalf("应按挂载点取数，实际 %q", probe.paths[0])
	}
}

// TestVolumeUsageReportsProbeFailure 取数失败要如实报错、且**不能**给出 0 用量。
//
// 界面据此回退服务端数字；把失败当成"用了 0"会让卡片谎报一块空盘。
func TestVolumeUsageReportsProbeFailure(t *testing.T) {
	probe := &fakeVolumeProbe{err: apperr.New(apperr.CodeUnavailable, 503)}
	mounts := []MountState{{AllocationID: "a1", RepoID: "r1", MountPath: "E:", State: MountStateMounted}}
	cache, _ := newTestVolumeUsageCache(mounts, probe)

	items := cache.snapshot(context.Background())
	if len(items) != 1 {
		t.Fatalf("失败的这一条仍要返回（便于诊断），实际 %+v", items)
	}
	if items[0].Error == "" {
		t.Fatal("取数失败必须带错误码")
	}
	if items[0].TotalBytes != 0 || items[0].UsedBytes != 0 {
		t.Fatalf("失败时不得给出用量，实际 %+v", items[0])
	}
}

// TestVolumeUsageReportsEmptyVolume 卷在但没有容量（还没就绪/未格式化）也算"读不到"。
func TestVolumeUsageReportsEmptyVolume(t *testing.T) {
	probe := &fakeVolumeProbe{space: &domain.VolumeSpace{Name: "e:", FileSystem: "NTFS"}}
	mounts := []MountState{{AllocationID: "a1", RepoID: "r1", MountPath: "E:", State: MountStateMounted}}
	cache, _ := newTestVolumeUsageCache(mounts, probe)

	items := cache.snapshot(context.Background())
	if len(items) != 1 || items[0].Error == "" {
		t.Fatalf("容量为 0 应报错而不是当成「用了 0」，实际 %+v", items)
	}
}

// TestVolumeUsageReusesSnapshotWithinTTL 锁定 TTL：界面 10s 轮询不该每次都起 PowerShell。
func TestVolumeUsageReusesSnapshotWithinTTL(t *testing.T) {
	probe := &fakeVolumeProbe{space: &domain.VolumeSpace{TotalBytes: 100, FreeBytes: 40}}
	mounts := []MountState{{AllocationID: "a1", RepoID: "r1", MountPath: "E:", State: MountStateMounted}}
	cache, clock := newTestVolumeUsageCache(mounts, probe)

	if items := cache.snapshot(context.Background()); len(items) != 1 {
		t.Fatalf("首次取数应实测，实际 %+v", items)
	}
	cache.snapshot(context.Background())
	if probe.calls() != 1 {
		t.Fatalf("TTL 内应复用快照，实际探测 %d 次", probe.calls())
	}

	clock.advance(volumeUsageTTL + time.Second)
	cache.snapshot(context.Background())
	if probe.calls() != 2 {
		t.Fatalf("TTL 过后应重测，实际探测 %d 次", probe.calls())
	}
}

// TestVolumeUsageSingleFlight 并发请求只让一路真的去测，其余拿上一份快照立即返回。
//
// 否则多开几个页面就会把 PowerShell 排成队（每次约 1s）。
func TestVolumeUsageSingleFlight(t *testing.T) {
	probe := &fakeVolumeProbe{
		space:   &domain.VolumeSpace{TotalBytes: 100, FreeBytes: 40},
		block:   make(chan struct{}),
		entered: make(chan struct{}),
	}
	mounts := []MountState{{AllocationID: "a1", RepoID: "r1", MountPath: "E:", State: MountStateMounted}}
	cache, _ := newTestVolumeUsageCache(mounts, probe)

	first := make(chan []MountVolumeUsage, 1)
	go func() { first <- cache.snapshot(context.Background()) }()
	<-probe.entered

	second := make(chan []MountVolumeUsage, 1)
	go func() { second <- cache.snapshot(context.Background()) }()
	select {
	case items := <-second:
		if len(items) != 0 {
			t.Fatalf("第二路应立刻拿到空快照（首测还没结束），实际 %+v", items)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("第二路被首测阻塞了：并发请求必须各自立即返回")
	}
	if probe.calls() != 1 {
		t.Fatalf("并发只该真测一次，实际 %d 次", probe.calls())
	}

	close(probe.block)
	if items := <-first; len(items) != 1 {
		t.Fatalf("首测应拿到实测结果，实际 %+v", items)
	}
	// 快照落定后，后续请求走 TTL 命中，不再探测。
	if items := cache.snapshot(context.Background()); len(items) != 1 {
		t.Fatalf("应复用刚落定的快照，实际 %+v", items)
	}
	if probe.calls() != 1 {
		t.Fatalf("落定后的请求不该再探测，实际 %d 次", probe.calls())
	}
}

// TestUsageSnapshotWithoutCache 未初始化（如单测里直接构造 Agent）时给空列表而不是 panic/报错。
func TestUsageSnapshotWithoutCache(t *testing.T) {
	a := &Agent{}
	if items := a.usageSnapshot(context.Background()); len(items) != 0 {
		t.Fatalf("未初始化应给空列表，实际 %+v", items)
	}
}
