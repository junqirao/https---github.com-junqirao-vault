package cache

import (
	"context"
	"testing"
)

// TestUsageReportsL1Occupancy 锁定 L1 占用/预算的统计口径。
//
// 客户端存储库页面展示的"缓存用量"就来自这里（见 internal/agent/cacheproxy.go 的
// CacheTargetStatus）：占用只算**当前真的缓存着数据的块**，预算等于容量换算出的块数 × 块大小。
// 算错会让用户看到一个与实物无关的占用（例如"刚开启就占了满额"）。
func TestUsageReportsL1Occupancy(t *testing.T) {
	const bs, ss = 16 << 10, 4 << 10
	c := newTestCache(t, testOpts{blockSize: bs, sectorSize: ss, l1Bytes: 1 << 20, shards: 4})
	src := newFakeSource(ss, 4<<20)
	ctx := context.Background()

	used, limit := c.Usage()
	if used != 0 {
		t.Fatalf("刚创建的缓存占用应为 0，实际 %d", used)
	}
	if limit <= 0 || limit%int64(bs) != 0 {
		t.Fatalf("预算是块大小的整数倍且为正，实际 %d", limit)
	}

	// 读一个扇区 → 该块被纳入索引。
	if err := c.Read(ctx, 0, make([]byte, ss), src); err != nil {
		t.Fatalf("read: %v", err)
	}
	used, _ = c.Usage()
	if used != int64(bs) {
		t.Fatalf("读一个扇区后占用应为 1 个块（%d 字节），实际 %d", bs, used)
	}

	// 同一块内的另一个扇区：不新增块。
	if err := c.Read(ctx, ss, make([]byte, ss), src); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got, _ := c.Usage(); got != used {
		t.Fatalf("同一块内的读不应新增占用：%d → %d", used, got)
	}

	// 跨到下一块：新增一块。
	if err := c.Read(ctx, int64(bs), make([]byte, ss), src); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got, _ := c.Usage(); got != used+int64(bs) {
		t.Fatalf("跨块读后占用应为 2 个块（%d 字节），实际 %d", 2*bs, got)
	}

	// Drop 之后占用归零（L2 索引也一并丢弃，见 Cache.Drop）。
	c.Drop()
	if got, _ := c.Usage(); got != 0 {
		t.Fatalf("Drop 后占用应归零，实际 %d", got)
	}

	// Stats() 内嵌同一口径，供上报一起取走。
	if st := c.Stats(); st.L1UsedBytes != 0 || st.L1LimitBytes != limit {
		t.Fatalf("Stats 的 L1 占用/预算 = %d/%d，期望 0/%d", st.L1UsedBytes, st.L1LimitBytes, limit)
	}
}

// TestRequestHitsCountsWholeCommands 锁定"整命令命中"的计数口径。
//
// 与 L1Hits 的区别：L1Hits 按块分段计数，一条跨块命令可能既命中又回源，用户无法据此判断
// "这条读命令到底有没有走网络"。RequestHits 只在**整条命令零回源**时 +1，
// 客户端展示的命中率就是 RequestHits/Reads。
func TestRequestHitsCountsWholeCommands(t *testing.T) {
	const bs, ss = 16 << 10, 4 << 10
	c := newTestCache(t, testOpts{blockSize: bs, sectorSize: ss})
	src := newFakeSource(ss, 4<<20)
	ctx := context.Background()

	// 首次读整块：必然回源。
	if err := c.Read(ctx, 0, make([]byte, bs), src); err != nil {
		t.Fatalf("read: %v", err)
	}
	if st := c.Stats(); st.RequestHits != 0 {
		t.Fatalf("首次读必然回源，RequestHits 应为 0，实际 %d", st.RequestHits)
	}

	// 再读同一整块：整条命令由缓存满足。
	if err := c.Read(ctx, 0, make([]byte, bs), src); err != nil {
		t.Fatalf("read: %v", err)
	}
	st := c.Stats()
	if st.RequestHits != 1 {
		t.Fatalf("整条命令由缓存满足时 RequestHits 应为 1，实际 %d", st.RequestHits)
	}
	if st.Reads != 2 {
		t.Fatalf("Reads 应计 2 条命令，实际 %d", st.Reads)
	}
	if st.BackendReads != 1 {
		t.Fatalf("BackendReads 应为 1（只有首读回源），实际 %d", st.BackendReads)
	}
	if got := float64(st.RequestHits) / float64(st.Reads); got != 0.5 {
		t.Fatalf("命中率应 = RequestHits/Reads = 0.5，实际 %v", got)
	}
}
