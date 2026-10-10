# iSCSI 读缓存：异步回填 + 无锁写改造

## Context（为什么做）

真机实测（`172.18.28.200:3260`，IQN `iqn.1991-05.com.microsoft:win-0e595h11jss-test-target`，qd16，`cmd/vault-iscsi-bench`）：

- **随机 4K 冷读**：经代理 30,124 IOPS，直连 37,290 IOPS（**−19%**）。原因是 `Cache.Read` 未命中时**同步**走 `readSegment → fillBlock`：
  - `scanDetector.observe` 每次读抢一把**全局 mutex**（[cache.go](file:///c:/Users/89412/Projects/golang/vault/internal/iscsicache/cache/cache.go#L866-L878)）
  - `flights.do` 又一把**全局 mutex**（[cache.go](file:///c:/Users/89412/Projects/golang/vault/internal/iscsicache/cache/cache.go#L830-L851)）
  - `sh.insert` 为一个 4K 读分配并清零 **64KiB** entry（[cache.go](file:///c:/Users/89412/Projects/golang/vault/internal/iscsicache/cache/cache.go#L717-L729)）+ map 插入 + 淘汰簿记
- **写**：经代理被锁死在 **~146 MiB/s**（`proxy.execMu.Lock()` 全局独占，[proxy.go](file:///c:/Users/89412/Projects/golang/vault/internal/iscsicache/proxy/proxy.go#L173-L174)），直连可达 200+ MiB/s。

目标：**冷读不劣于直连**，**写吞吐随队列深度扩展**，不给现有功能引入回归。

---

## 核心机制：shard 级 epoch（乐观并发）

一个机制同时支撑两件事。

- `shard` 增加 `epoch uint64`，**受 `sh.mu` 保护**、单调递增；**只有 `Invalidate` / `Drop` 会 bump**（淘汰不 bump）。
- 回填在**读后端之前**记录 `captured = sh.epoch`；在**安装前**于 `sh.mu` 内校验 `sh.epoch == captured`，不一致即**丢弃本次回填**。
- 只要「校验 + copy 到 `e.data` + `setValid`」在**同一个 `sh.mu` 临界区**内完成，就不会出现「校验通过 → 失效清位 → 才置有效」的窗口。
- 必须用 **shard 级**而非 entry 级 epoch：miss 时 entry 可能尚不存在，且淘汰后可重建，entry 级 token 不稳定。

---

## A. 回填异步化（事件消费）

**改动文件**：[cache.go](file:///c:/Users/89412/Projects/golang/vault/internal/iscsicache/cache/cache.go)

- `Config` 新增 `FillWorkers`（默认 4）、`FillQueueDepth`（默认 1024），在 `normalize()` 里补默认值（**必须非 0**，否则 worker 永不消费）。
- `Cache` 新增：
  - `fillCh chan fillReq`（有界）、`fillWG sync.WaitGroup`
  - `pendingMu sync.Mutex` + `pending map[uint64]struct{}`（key = 全局 sector 索引 `blk*sectorsPer+sector`，去重）
  - `closed atomic.Bool`
  - `fillReq{blk uint64, blockOff int, data []byte, epoch uint64}`
- **未命中路径**（单块 / 子块 miss）：
  1. `readAligned(ctx, off, dst, src)` 直读请求范围并直接交给客户端（**不经缓存块路径**）；
  2. 把本次命中的 sector 段 `copy` 进事件缓冲，带上 `captured` epoch；
  3. 去重后**非阻塞**入队（`select { case fillCh <- req: default: 丢弃 }`），入队失败则清除 pending 标记。
- **保留** `fillRun` 处理**多连续缺失块**的批量读（大读延迟由传输主导，且避免"服务一次 + 回填再读一次"导致后端读翻倍）。
- **worker**：从 `fillCh` 取出 → `installValidated(blk, blockOff, data, epoch)`（下面的新函数）→ 清除 pending。
- **不预取整块**：事件只携带请求命中的 sector 段（4K miss 只 4KiB），保持现有"按需填充"契约，不额外增加后端读。
- `Cache.Close()`（当前**不存在**，需新增）：`closed=1` → `close(fillCh)` → `fillWG.Wait()` → `fence.waitAll()`。必须在 [portal.go](file:///c:/Users/89412/Projects/golang/vault/internal/iscsicache/portal.go#L92-L101) 的 `rt.close()` 里、`l2.Sync()/Close()` **之前**调用。

**新增统一安装函数**（替换 `fillBlock` L464-470 的无锁 copy、`install` L737-757、`fillRun` L411 的直接调用）：

```
// installValidated：sh.mu 内完成 校验+copy+setValid+分配 L2 序号
func (c *Cache) installValidated(blk uint64, blockOff int, data []byte, epoch uint64)
```

在 `sh.mu` 内：`if sh.epoch != epoch { return }` → 复用/新建 entry（`make([]byte, blockSize)`）→ 跳过已 valid 的 sector → `copy + setValid` → **在同一临界区内**非阻塞获取 `l2Sem` 并 `seq = fence.enqueue()`（拿不到就跳过 L2）。出临界区后起 goroutine 写 L2，写完 `fence.complete(seq)`。

---

## B. 写不加锁

**改动文件**：[proxy.go](file:///c:/Users/89412/Projects/golang/vault/internal/iscsicache/proxy/proxy.go)

- 删除 `Proxy.execMu`（L84）以及 `Execute`（L168-174）、`ExecuteStream`（L231-234）里的全部加锁。读不再需要 RLock；`write`/`unmap`/`passthrough` 之间无需互斥（后端 Pool 本就支持并发）。
- `write`（L451）/ `unmap`（L485）保持「后端完成 → `Invalidate` → 返回 GOOD」的顺序不变。

**`Invalidate` 新顺序**（[cache.go](file:///c:/Users/89412/Projects/golang/vault/internal/iscsicache/cache/cache.go#L515-L551)）：

1. 逐块 `sh.mu` 内：`epoch++` + `clearValid` + （必要时）`removeLocked` —— **bump 与清位同临界区**；
2. `target = fence.last()`（此时所有携带写前数据的 enqueue 都已分配序号 ≤ target），`fence.wait(target)`；
3. `l2.InvalidateSectors(...)`。

**L2 栅栏**（替换全局 `l2Pending sync.WaitGroup` + `drainL2`，避免去锁后被持续 enqueue 拖成活锁）：

```
type l2Fence struct { mu sync.Mutex; cond *sync.Cond; next, done uint64 }
// enqueue(): 在 sh.mu 内调用，next++，返回分配的 seq
// complete(seq): done++（慢完成用累计计数，或用最小堆）+ cond.Broadcast
// wait(target): 等到 done >= target
```

正确性依据：enqueue 与 epoch 校验同处 `sh.mu` ⇔ 携带写前数据的 enqueue 序列号一定 ≤ bump 后读取的 `target`；bump 之后能安装成功的回填只可能带**写后**数据（seq > target，无需等待）。`target` 固定 ⇒ 不会活锁。`enqueueL2Write` 的「满则丢弃」仍安全，但**必须同步从 pending 去重集移除**，否则该块永不重入队。

---

## C. scan 检测器分桶

**改动文件**：[cache.go](file:///c:/Users/89412/Projects/golang/vault/internal/iscsicache/cache/cache.go#L853-L878)

把单个 `scanDetector` 换成 N（建议 64）个桶，`key = off >> 20`（每桶负责 1MiB 区间），`observe` 只锁对应桶的 mutex，消除读路径上的全局争用。语义（连续 `start == previous end` 达阈值即判定 scan）保持不变。

---

## 配置面

- `cache.Config`：`FillWorkers` / `FillQueueDepth`。
- [iscsicache.go](file:///c:/Users/89412/Projects/golang/vault/internal/iscsicache/iscsicache.go#L77-L113) `CacheConfig` 新增：
  ```go
  Fill struct {
      Workers    int `yaml:"workers"`
      QueueDepth int `yaml:"queue_depth"`
  } `yaml:"fill"`
  ```
  `applyDefaults()` 里默认 4 / 1024。
- [portal.go](file:///c:/Users/89412/Projects/golang/vault/internal/iscsicache/portal.go#L207-L218) `cache.New(...)` 透传。

---

## 验证

**确定性并发测试**（新增，fake Source 用 channel/gate 精确制造交错）：

- `TestAsyncFillDropsOnInvalidate`：gate 住回填的"读后端 → 安装"之间 → 触发 `Invalidate` → 放行 → 断言 L1 无 valid 且重读得到新值。
- `TestL2FenceNoResurrect`：gate 住旧 L2 写 → 断言 `Invalidate` 等其完成后再 drop（旧数据不复活）。
- `TestFillQueueDropKeepsServing`：队列打满 → 断言读仍正确返回、仅损失预取。
- `Cache.Wait()`（等 `fillWG` + `fence`）作为测试断言的同步点。

**既有测试需适配**（异步化后"紧跟重读"可能再 miss）：`cache_test.go` 中 `TestReadFillsL1AndHits`、`TestPartialFillThenRemainingSectors`、`TestReadPopulatesL2InBackground`、`TestInvalidateWaitsForBackgroundL2Write`，以及 [iscsicache_live_test.go](file:///c:/Users/89412/Projects/golang/vault/internal/iscsicache/iscsicache_live_test.go#L106-L119) 的第二次读（读前加 `Wait()`）。

**真机端到端**：

```
go run ./cmd/vault-iscsi-bench -config <yaml> -qd 32          # 代理
go run ./cmd/vault-iscsi-bench -config <yaml> -qd 32 -direct  # 直连基线
```

- 随机 4K cold IOPS：目标 **≥ 直连**。
- `-allow-write` 在 QD1/8/32 看写吞吐是否**随 QD 上升**（当前锁死 146 MiB/s）。
- live 测试：`VAULT_ISCSI_TEST_ADDR/IQN` + `go test ./internal/iscsicache -run Live`。

**回归**：`gofmt -l`、`go vet ./internal/iscsicache/...`、`go test -race ./internal/iscsicache/...`、`GOOS=linux CGO_ENABLED=0 go build ./...`。

---

## 风险与取舍

- **冷 miss 不再同步填 L1**：紧跟的重复读可能再 miss 一次（大工作集随机流无影响，重复读命中率短暂下降后恢复）。`Fills`/`L2Hits` 的计数时点移到 worker。
- **纯 miss 失去 `flights` 合并**：同块并发冷读可能瞬时多发后端命令（随机 4K 基本无影响）。
- **写 ack 会短暂等待** `seq ≤ target`（L2 在途上限 8）；换来写吞吐随 QD 扩展。
- **内存**：回填队列占用 ≤ `QueueDepth × 段长`，需在文档中计入 L1 预算说明。
- **L2 关闭顺序**：必须 `Cache.Close()` 先于 `l2.Close()`，否则 worker 可能写到已关闭的 L2。