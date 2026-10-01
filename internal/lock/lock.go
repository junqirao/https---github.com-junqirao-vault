// Package lock 提供资源级互斥与进程单实例保证。
//
// 设计依据（见 docs/implementation.md 5.13）：
//   - 同一 VHDX 的挂载/发布/删除必须串行，否则会损坏磁盘；
//   - 同一存储库的配额分配需要"读-校验-写"原子性；
//   - 同一卷的 compact 任务需串行（IO 密集）。
//
// 本包只解决**进程内**互斥；跨进程由数据库事务与全局单实例锁保证。
package lock

import (
	"sort"
	"sync"
)

// Keyed 是按字符串键分桶的互斥锁。
//
// 相比 sync.Map + Mutex 的朴素实现，这里额外维护引用计数，
// 在锁完全释放后回收条目，避免长期运行后 map 无限增长。
type Keyed struct {
	mu    sync.Mutex
	items map[string]*refMutex
}

type refMutex struct {
	mu   sync.Mutex
	refs int
}

// NewKeyed 构造一个 Keyed 锁集合。
func NewKeyed() *Keyed {
	return &Keyed{items: make(map[string]*refMutex)}
}

// Lock 获取单个键的锁，返回释放函数。
//
// 用法：
//
//	unlock := locks.Lock("disk:" + id)
//	defer unlock()
func (k *Keyed) Lock(key string) func() {
	return k.Acquire(key)
}

// Acquire 一次性获取多个键的锁。
//
// 内部对键排序后依序获取，从而避免不同调用方以不同顺序加锁造成死锁。
// 返回的函数会按相反顺序释放。
func (k *Keyed) Acquire(keys ...string) func() {
	unique := dedupSorted(keys)
	held := make([]*refMutex, 0, len(unique))
	for _, key := range unique {
		held = append(held, k.acquireOne(key))
	}
	return func() {
		for i := len(held) - 1; i >= 0; i-- {
			held[i].mu.Unlock()
			k.releaseRef(held[i])
		}
	}
}

func (k *Keyed) acquireOne(key string) *refMutex {
	k.mu.Lock()
	rm, ok := k.items[key]
	if !ok {
		rm = &refMutex{}
		k.items[key] = rm
	}
	rm.refs++
	k.mu.Unlock()

	rm.mu.Lock()
	return rm
}

func (k *Keyed) releaseRef(rm *refMutex) {
	k.mu.Lock()
	rm.refs--
	if rm.refs <= 0 {
		// 此时该条目已无持有者与等待者，可安全删除。
		for key, item := range k.items {
			if item == rm {
				delete(k.items, key)
				break
			}
		}
	}
	k.mu.Unlock()
}

// Size 返回当前跟踪的键数量，用于监控与测试。
func (k *Keyed) Size() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.items)
}

func dedupSorted(keys []string) []string {
	if len(keys) == 0 {
		return nil
	}
	sorted := make([]string, len(keys))
	copy(sorted, keys)
	sort.Strings(sorted)

	out := sorted[:0]
	var last string
	for i, key := range sorted {
		if i == 0 || key != last {
			out = append(out, key)
			last = key
		}
	}
	return out
}

// 资源键构造器。统一在这里拼装，避免各处拼错导致锁不生效。

// DiskKey 磁盘级互斥键。
func DiskKey(diskID string) string { return "disk:" + diskID }

// RepoKey 存储库级互斥键。
func RepoKey(repoID string) string { return "repo:" + repoID }

// VolumeKey 卷级互斥键（用于 compact 等 IO 密集操作）。
//
// 入参应为**卷标识**而非根路径：同一卷上的多个白名单根共享同一把锁。
// 卷标识由 domain.VolumeID(路径) 得到（规范化、小写）。
// 注意：对挂载点这类非盘符卷不精确（见 domain.VolumeID 说明），属已知取舍。
func VolumeKey(volumeID string) string { return "volume:" + volumeID }

// TargetKey iSCSI 目标级互斥键。
func TargetKey(targetName string) string { return "iscsi:" + targetName }
