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
	"context"
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

// refMutex 用容量为 1 的 channel 当互斥体（而不是 sync.Mutex），
// 这样等锁可以**被 context 取消**：任务在等锁期间被取消时必须立刻退出，
// 否则会一直占着 worker 与"running"状态（真实事故：任务卡几个小时都在执行中，
// 其实只是排在另一把卡死的锁后面）。
type refMutex struct {
	ch   chan struct{}
	refs int
}

func newRefMutex() *refMutex { return &refMutex{ch: make(chan struct{}, 1)} }

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

// LockContext 获取单个键的锁；等待期间 ctx 被取消时返回错误且**不获取锁**。
//
// 用于任务执行器等"可能等很久"的场景：任务被取消后不该继续排在锁后面。
func (k *Keyed) LockContext(ctx context.Context, key string) (func(), error) {
	rm := k.reserve(key)
	select {
	case rm.ch <- struct{}{}:
		return func() {
			<-rm.ch
			k.releaseRef(rm)
		}, nil
	case <-ctx.Done():
		// 没拿到锁：只回退引用计数，不触碰 channel。
		k.releaseRef(rm)
		return nil, ctx.Err()
	}
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
			<-held[i].ch
			k.releaseRef(held[i])
		}
	}
}

// reserve 在锁表里登记一次引用并返回对应的 refMutex（尚未获取锁）。
func (k *Keyed) reserve(key string) *refMutex {
	k.mu.Lock()
	rm, ok := k.items[key]
	if !ok {
		rm = newRefMutex()
		k.items[key] = rm
	}
	rm.refs++
	k.mu.Unlock()
	return rm
}

func (k *Keyed) acquireOne(key string) *refMutex {
	rm := k.reserve(key)
	rm.ch <- struct{}{}
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

// HostKey 宿主机级互斥键（key 区分不同的全局操作，如 "sysdeps"）。
//
// 用于**影响整台机器**而不属于任何业务实体的操作：包管理器（apt/dpkg/rpm）自身在宿主机
// 范围内只有一把锁，两个装包任务并行只会互相撞锁、留下装到一半的状态；
// 而这类操作既不属于某块磁盘也不属于某个库，用 DiskKey/RepoKey 表达是错的。
func HostKey(name string) string { return "host:" + name }
