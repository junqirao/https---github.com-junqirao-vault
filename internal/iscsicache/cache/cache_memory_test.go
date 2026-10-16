package cache

import (
	"context"
	"testing"
	"unsafe"
)

// installQueue 用一块**测试自己持有**的底层数组填满队列并返回该数组。
//
// 这一点是关键：出队之后 shard 里的切片头指向数组的后半段，出队处那些槽位在正常
// 操作下根本看不到（这正是这类泄漏藏得住的原因）。测试留着数组的切片头，出队之后
// 还能回头检查它们。
func installQueue(sh *shard, q *[]*entry, tag uint8, n int) []*entry {
	backing := make([]*entry, 0, n)
	for i := 0; i < n; i++ {
		e := &entry{blk: uint64(tag)*1000 + uint64(i)}
		e.q = tag
		sh.blocks[e.blk] = e
		backing = append(backing, e)
	}
	*q = backing
	return backing
}

// TestDequeuedSlotsAreCleared 锁定"出队必须清槽位"：只写 q = q[1:] 的话，底层
// 数组里那个槽位仍持有 *entry，已从 map delete 的 entry 会连同块数据一直活到
// 数组被 append 重分配。实测这部分约占存活堆被低估的那 200MiB。
func TestDequeuedSlotsAreCleared(t *testing.T) {
	c := newTestCache(t, testOpts{blockSize: 16 << 10, sectorSize: 4 << 10, l1Bytes: 1 << 20, shards: 1})
	sh := c.shards[0]

	sh.mu.Lock()
	defer sh.mu.Unlock()
	// 把三条路径分开：只让 newQ 溢出，midQ 不溢出，map 不触发淘汰。
	sh.newQuota, sh.midQuota, sh.high, sh.low = 4, 1<<20, 1<<20, 1<<20

	newBacking := installQueue(sh, &sh.newQ, queueNew, 24)
	installQueue(sh, &sh.midQ, queueMid, 6)
	sh.maintainLocked()

	if len(sh.newQ) != 4 || len(sh.oldQ) != 20 {
		t.Fatalf("队列长度不符：newQ=%d oldQ=%d，期望 4/20", len(sh.newQ), len(sh.oldQ))
	}
	for i := 0; i < 20; i++ {
		if newBacking[i] != nil {
			t.Fatalf("newQ 已出队的槽位 %d 仍持有 blk=%d 的 entry：它会连同块数据被钉住",
				i, newBacking[i].blk)
		}
	}
	for i := 20; i < 24; i++ {
		if newBacking[i] == nil {
			t.Fatalf("newQ 仍在队里的槽位 %d 被清空了", i)
		}
	}

	// oldQ 的淘汰路径最要紧：这些 entry 已经从 map 里 delete，只要还有槽位指着
	// 它们，64KiB 的块数据就回收不掉。
	oldBacking := sh.oldQ
	sh.high, sh.low = 10, 0
	sh.maintainLocked()
	if len(sh.oldQ) != 0 || len(sh.blocks) != 10 {
		t.Fatalf("淘汰后 oldQ=%d blocks=%d，期望 0/10", len(sh.oldQ), len(sh.blocks))
	}
	for i, e := range oldBacking {
		if e != nil {
			t.Fatalf("oldQ 已淘汰的槽位 %d 仍持有 blk=%d 的 entry（已 delete 出 map）", i, e.blk)
		}
	}
}

// TestRemovedQueueSlotIsCleared 锁定 removeFromQueueLocked 移位之后不留重复指针。
//
// append((*q)[:i], (*q)[i+1:]...) 会把原末位元素**多留一份**在 len 之外的槽位里，
// 底层数组因此长期持有一个已出队的 entry。
func TestRemovedQueueSlotIsCleared(t *testing.T) {
	c := newTestCache(t, testOpts{blockSize: 16 << 10, sectorSize: 4 << 10, l1Bytes: 1 << 20, shards: 1})
	sh := c.shards[0]

	sh.mu.Lock()
	defer sh.mu.Unlock()
	backing := installQueue(sh, &sh.newQ, queueNew, 8)
	// 移位会就地改写底层数组，先留一份原始顺序用来对答案。
	orig := append([]*entry(nil), backing...)

	sh.removeLocked(orig[7]) // 末位
	if len(sh.newQ) != 7 {
		t.Fatalf("移除末位后长度应为 7，实际 %d", len(sh.newQ))
	}
	if backing[7] != nil {
		t.Fatalf("末位槽位仍持有 blk=%d 的 entry（已 delete 出 map）", backing[7].blk)
	}
	if !sameEntries(sh.newQ, orig[:7]) {
		t.Fatalf("移除末位后内容变了：%v", sh.newQ)
	}

	sh.removeLocked(orig[3]) // 中间：语义与 append 移位一致
	if len(sh.newQ) != 6 {
		t.Fatalf("移除中间项后长度应为 6，实际 %d", len(sh.newQ))
	}
	if backing[6] != nil {
		t.Fatalf("移位后腾出的末位槽位仍持有 blk=%d 的 entry", backing[6].blk)
	}
	want := []*entry{orig[0], orig[1], orig[2], orig[4], orig[5], orig[6]}
	if !sameEntries(sh.newQ, want) {
		t.Fatalf("移除中间项后顺序应保持不变")
	}
}

func sameEntries(got, want []*entry) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestEntryHeaderBytesMatchesStruct 守住 ResidentBytes 里的 entry 头常量，
// 结构体一变就失败，避免估算悄悄漂掉。
func TestEntryHeaderBytesMatchesStruct(t *testing.T) {
	if got := int64(unsafe.Sizeof(entry{})); got != entryHeaderBytes {
		t.Fatalf("entry 结构体变成 %d 字节，请同步 cache.go 的 entryHeaderBytes", got)
	}
}

// TestResidentBytesCoversUsage 锁定估算口径：实际占用不低于账本，Drop 后归零。
func TestResidentBytesCoversUsage(t *testing.T) {
	const bs, ss = 16 << 10, 4 << 10
	c := newTestCache(t, testOpts{blockSize: bs, sectorSize: ss, l1Bytes: 1 << 20, shards: 4})
	src := newFakeSource(ss, 4<<20)
	ctx := context.Background()

	if got := c.ResidentBytes(); got != 0 {
		t.Fatalf("空缓存的实际占用应为 0，实际 %d", got)
	}
	for i := 0; i < 8; i++ {
		if err := c.Read(ctx, int64(i*bs), make([]byte, ss), src); err != nil {
			t.Fatalf("read: %v", err)
		}
		c.Wait()
	}
	st := c.Stats()
	if st.L1UsedBytes != 8*bs {
		t.Fatalf("账本应为 8 块（%d 字节），实际 %d", 8*bs, st.L1UsedBytes)
	}
	// 账外开销：每个 entry 的头 + map 分摊（队列槽位另算，只会更多）。
	blocks := st.L1UsedBytes / bs
	if diff, want := st.L1ResidentBytes-st.L1UsedBytes, blocks*(entryHeaderBytes+mapSlotBytes); diff < want {
		t.Fatalf("账外开销估算偏低：%d，至少应有 %d", diff, want)
	}
	if st.L1ResidentBytes >= st.L1LimitBytes {
		t.Fatalf("实际占用 %d 不该超过预算 %d（8/64 块）", st.L1ResidentBytes, st.L1LimitBytes)
	}

	c.Drop()
	if got := c.ResidentBytes(); got != 0 {
		t.Fatalf("Drop 后实际占用应归零，实际 %d", got)
	}
}
