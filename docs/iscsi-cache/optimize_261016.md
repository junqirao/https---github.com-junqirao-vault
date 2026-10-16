# iSCSI 客户端代理 CPU 优化方案

> 状态：O1、O2 已实施（见 §8 实施记录）；其余待评审
> 日期：2026-10-16
> 实测依据：仓库根目录 `cpu`（CPU profile，采集自 `iscsiproxy.test.exe` / `TestLiveWriteThroughput`，2026-10-11 22:22）
> 前置阅读：`iscsi_cache_v2_design.md` §6 数据路径、§8 性能模型

---

## 0. 一句话结论

实测 profile 显示：**代理侧 CPU 的约 76% 花在「每帧一次 socket 写系统调用」上**（`writeFrame` → `Write`/`WSASend`），而不是拷贝、分配或锁竞争。

所以降 CPU 的第一杠杆不是「少拷一次内存」，而是**把 N 个 PDU 合并成一次 `WSASend`**。

设计文档 §8 本来就写了这条调优项：

> 读缓冲 ≥ 1MiB，配合 `net.Buffers` writev 一次出多个 PDU

但实现只做了**帧内**合并（`writeFrame` 把单帧的 BHS+body+pad 拼成一次 Write），**跨帧合并从未落地**，每个 PDU 仍然各自一次系统调用。本方案的主项就是补齐它。

---

## 1. 实测证据：CPU 到底花在哪

### 1.1 profile 概况

| 项 | 值 |
|---|---|
| 场景 | `TestLiveWriteThroughput`：1MiB 顺序写 Q8T1 + 4K 随机写 Q32T1，三种链路（直连 / 纯透传 / 开缓存） |
| 墙钟 | 24.52s |
| CPU 样本合计 | 6.30s（占墙钟 25.69%，其余时间在等后端） |
| 复现 | `go test ./internal/iscsiproxy -run TestLiveWriteThroughput -cpuprofile cpu`（需真机后端） |

### 1.2 调用链分解（cum = 含子调用）

| 节点 | cum | 占比 | 说明 |
|---|---|---|---|
| `runtime.cgocall` | 4.62s | 73.33% | 全部来自 `syscall.syscalln`，即**系统调用本身** |
| `syscall.WSASend` | 3.86s | 61.27% | socket 发送（含内核拷贝与等待） |
| `iscsiproxy.(*session).guardedPump` | 2.70s | **42.86%** | **代理两侧数据泵的总 CPU** |
| ├ `iscsiproxy.(*session).writeFrame` | 2.06s | 32.70% | **代理写 socket** |
| ├ `net.(*conn).Write` | 2.04s | 32.38% | 单 buffer 写（`writeFrame` 的落点） |
| ├ `iscsiproxy.(*session).pumpTI` | 1.54s | 24.44% | 后端→发起端方向 |
| └ `iscsiproxy.(*session).pumpIT` | 1.16s | 18.41% | 发起端→后端方向 |
| `backend.(*Initiator).writePDULocked` | 2.02s | 32.06% | **测试负载生成器**自己的发送（非产品路径） |
| `net.(*Buffers).WriteTo` | 1.97s | 31.27% | 调用者 100% 是 `iscsi.WritePDU`（同上） |
| `internal/poll.(*FD).Read` | 0.84s | 13.33% | 两条链路的读侧 |
| `bufio.(*Reader).Read` | 0.60s | 9.52% | 代理读侧（已批量化） |
| `iscsi.ReadBHS` | 0.30s | 4.76% | PDU 头解析，很便宜 |
| `netpoll` + `semawakeup` + `netpollQueueTimer` | 0.73s | ~11.6% | Windows overlapped IO 完成唤醒 |

### 1.3 三条结论

**结论 1：syscall 是绝对大头，占 73%。**
`runtime.cgocall` 的 4.59s 是 flat 时间（不是 cum），意味着 CPU 真的停在系统调用里。Windows 上每次 `WSASend` 要进内核、准备 overlapped 结构，固定成本远高于 Linux 的 write。**优化目标是「减少 syscall 次数」，不是「减少 memcpy」。**

**结论 2：代理自身占 42.9%，其中 76% 是写 syscall。**
`writeFrame` 2.06s ÷ `guardedPump` 2.70s = 76.3%。代理读路径（`bufio.Read` + `ReadBHS`）只占约 0.9s，已经很便宜。

**结论 3：代理转发的 CPU 与「测试工具自己生成 PDU」的 CPU 相当（2.70s vs 2.02s+）。**
理想情况下转发应显著便宜于生成。两者的共同点正是**每 PDU 一次 `WSASend`**。

### 1.4 profile 的局限（先声明，避免过度解读）

1. **混了测试工具开销**：`backend.Initiator`（`iscsi.WritePDU` 2.02s、`readLoop` 1.68s）是测试里的负载生成器，**不是产品路径**（v2 不用自研发起端）。看代理自身只看 `guardedPump` 那 2.70s。
2. **只有写场景**：读场景（尤其 L1 命中注入、4K 随机读）没有 profile。
3. **只有 6.3s 样本**：统计噪声不小，小项（<5%）不宜据此决策。
4. **无 pprof 端点**：仓库里没有任何 `net/http/pprof`（`go.sum` 中的 `pprof` 是依赖间接引用）。真机上目前**无法采集 profile**。这就是 O3 的由来。

---

## 2. 优化项总览

| 优先级 | 编号 | 项目 | 预期收益 | 风险 | 工作量 |
|---|---|---|---|---|---|
| P0 | O1 | 跨帧合并写（批量 flush）**（已实施）** | 写 syscall 次数降为 1/N，代理写侧 CPU 大幅下降 | 低 | 中 |
| P0 | O2 | 大帧改单次 gather 写**（已实施）** | >16KiB 帧每次省 1 次 syscall | 极低 | 小 |
| P0 | O3 | 可观测性：pprof 开关 + v2 bench | 后续优化不再靠猜 | 无 | 中 |
| P1 | O4 | tee 路径免拷贝 | miss 读省一次全量 memcpy | 中 | 中 |
| P1 | O5 | 命中路径两次拷贝合一 | 命中读 memcpy 减半 | 中低 | 中 |
| P2 | O6 | 热路径锁瘦身 | 每命令省 4~5 次互斥锁 | 低 | 小 |
| P2 | O7 | L1 块大小对 4K 随机 | 每次 miss 少分配/少清零 60KiB | 需实测 | 小（调参） |
| P2 | O8 | L2 回填路径 | 少 goroutine、少 fence 广播 | 中 | 中 |
| P2 | O9 | 纯透传（NoCache）路径 | 未开缓存卷的 CPU | 产品决策 | 中 |

---

## 3. P0 项详解

### O1. 跨帧合并写（主项）

**现状**

`internal/iscsiproxy/session.go:157-185`（`pumpIT`）与 `:190-217`（`pumpTI`）各自：

```go
scratch := make([]byte, writeCoalesce+8)   // 16KiB+8
for {
    f, err := fr.read()
    ...
    writeFrame(s.bt, f, scratch)           // 每帧一次 Write
}
```

`internal/iscsiproxy/frame.go:169-212`（`writeFrame`）只在**单帧内**合并：把 `hdr + body + pad` 拷进 scratch 后一次 `w.Write`。跨帧没有任何合并。

**为什么这是第一杠杆**

- 4K 随机写：每个命令 = 命令 PDU（48B）+ Data-Out PDU（4144B）+ 响应 PDU（48B），代理侧每方向各 1~2 次 `Write`。QD32 下这些 PDU 在 `bufio` 的 64KiB 窗口里**本来就成串到达**。
- 实测：`writeFrame` → `net.(*conn).Write`（单 buffer）→ 一次 `WSASend`，而 `syscall.WSASend` 占总 CPU 61%。
- 把 8 个 8KiB 帧合成一次 64KiB `Write`：**8 次 syscall → 1 次**，代价只是一次 64KiB memcpy（约 3~5µs，远低于 7 次 `WSASend` 的固定成本）。

**方案**

把「写」从一个函数改造成每个泵一个批量写器：

```go
// frame_writer.go（新增）
type frameWriter struct {
    w   io.Writer     // 目标连接
    out []byte        // 批量缓冲，建议 writeBatch（默认 256KiB）
    n   int           // 已攒字节数
}

// append 把一帧追加进批量缓冲；缓冲放不下或帧本身过大时先 flush。
func (fw *frameWriter) append(f *frame) error
func (fw *frameWriter) flush() error
```

按帧大小分流（关键取舍）：

| 帧总大小 | 处理 | 原因 |
|---|---|---|
| ≤ `writeCoalesce`（16KiB） | 拷进批量缓冲，继续攒 | 与小帧原路径成本相同（原来也拷进 scratch），但省掉后续每一帧的 syscall |
| > `writeCoalesce` | **先 flush 批量缓冲**，再按老路直发该帧（配 O2 变成一次 gather 写） | 大帧零拷贝直发，避免为攒批白拷 MiB 数据 |

泵循环形态：

```go
for {
    f, err := fr.read()
    if err != nil { break }
    ...
    rewriteIT(&f.hdr, s.offset)              // pumpTI 侧在 outMu 内
    if err := fw.append(f); err != nil { return }
    // 只在「手里已经没有下一帧」时才 flush：绝不为了凑批而等待
    if fr.r.Buffered() == 0 {
        if err := fw.flush(); err != nil { return }
    }
}
fw.flush()   // 循环退出（EOF/错误）也要 flush
```

**必须守住的约束**

1. **frame 别名 reader 缓冲，下一帧会覆盖** ⇒ `append` 必须先拷贝；这正是「大帧走直发」的原因。
2. **pumpTI 的 append/flush 必须在 `outMu` 内**（`session.go:208-212`），保持与 `injectLocked` 的相对顺序。攒批会让 `outMu` 持有时间略长，但单次写入更多字节，锁内总时间不增反降。
3. **flush 只在 `Buffered() == 0` 时触发** ⇒ 不引入额外延迟：此时再读就要阻塞在 socket 上，已攒字节本来也该发出去了。
4. **degrade/splice 分支必须先 flush**（`session.go:167-174`、`:200-206`），否则丢字节。
5. `captureTargetSeq`（`session.go:210`）仍逐帧调用、顺序不变。append 与 flush 同在 `outMu` 内、注入也必须拿 `outMu`，因此注入看到的序号始终与「已排队的字节」一致，不会错位。
6. 内存：每方向 256KiB（原 16KiB）；建议把 `frameReadBuffer`（`frame.go:61`，64KiB）一并提到 256KiB，提高「手里有下一帧」的概率（`bufio` 不会为填满而等待，只多占内存）。

**预期收益**：写侧 syscall 次数降为 1/N。4K QD32 下每方向可从 ~2 次降到 ~1 次；1MiB 顺序写（8KiB first burst，每 MiB 128 个 Data-Out）收益更大。

**风险**：低。字节顺序与内容完全不变，只是发送时机推迟到同一批的末尾。建议加配置开关 `proxy.write_batch_bytes`（0 = 退化为逐帧，即当前行为），出问题可一键回退。

**验证**：bench 统计「每 IO 的 write 调用数」（见 O3）；`statseq_test.go`、`invalidate_test.go`、真机 live 三件套不退化。

### O2. 大帧改单次 gather 写

**现状**（`frame.go:181-190`）：

```go
if f.ahsLen == 0 && f.padLen == 0 {
    if _, err := w.Write(f.hdr[:]); err != nil { return err }   // syscall 1
    if len(data) > 0 {
        _, err := w.Write(data)                                  // syscall 2
        return err
    }
    return nil
}
```

超过 `writeCoalesce` 的帧（如 64KiB MRDSL 的 Data-In）被拆成**两次 Write**。

**证据**：profile 中 `iscsi.WritePDU` → `net.Buffers.WriteTo` → `netFD.writeBuffers` → `internal/poll.(*FD).Writev` → `WSASend`，且 `Writev` 内部只有**一个** `execIO`（`Writev.func1` cum 1.89s），说明 Windows 上 Go 的 `Writev` 是**真正的 gather 写**：多个 WSABUF 一次 `WSASend`。

**这与 `frame.go:159-164` 的注释冲突**：

> Go's net.Buffers has no scatter-gather write on Windows, so a multi-slice frame costs one syscall per slice

注释与实测不符（至少在当前 Go 版本上不成立）。**这是本方案里最「便宜」的一项**：删掉那个分支，统一走 `net.Buffers`：

```go
bufs := net.Buffers{f.hdr[:], data}
_, err := bufs.WriteTo(w)
```

**收益**：每个 >16KiB 帧省 1 次 `WSASend`（1MiB 读按 64KiB 分段 = 每 MiB 省 16 次）。

**风险**：极低，但**必须先复核注释**：用 bench 统计两种写法的 syscall 次数确认（若某 Windows 版本上 `Writev` 退化为多次 Write，则保留分支并加注释说明版本）。

### O3. 可观测性：pprof 开关 + v2 自研 bench

**问题**

- 仓库里没有 `net/http/pprof`（`go.sum` 中的 `pprof` 只是依赖间接引用），真机上无法采 profile。
- v1 有完整 e2e bench（`internal/iscsicache/frontend/e2e_bench_test.go`），**v2 没有**；`live_test.go` 的 live 测试需要真机后端，20~30 分钟一轮，不适合做优化迭代的回归口径。
- 没有 profile 就只能靠猜，而本次 profile 证明「直觉上的热点（memcpy、锁、分配）」并不是真正的热点。

**方案 A：agent 增加按需 pprof 端点**

在 agent 启动时读环境变量（如 `VAULT_PPROF=127.0.0.1:6060`），非空才 `net/http/pprof` 起一个**只监听回环**的 HTTP 服务，并写一条 Info 日志说明已开启。默认关闭，零成本。用途：用户机器上直接 `curl '127.0.0.1:6060/debug/pprof/profile?seconds=30' -o cpu`。

**方案 B：新增 `internal/iscsiproxy/bench_test.go`（全进程内，可复现）**

后端不依赖真机：复用 v1 的内存 LUN + `frontend.New` 作为「真后端」，即 `e2e_bench_test.go:176` 的 `e2eStartStorage` 那套构造方式（`internal/iscsicache/{proxy,frontend}` 在同一 module 内可导入）。链路：

```
backend.Initiator(测试发起端) → iscsiproxy Portal(被测) → v1 frontend target(内存 LUN)
```

负载矩阵：

| 维度 | 取值 |
|---|---|
| IO 大小 | 4KiB、1MiB |
| 队列深度 | QD1、QD8、QD32 |
| 方向 | 读、写 |
| 缓存 | 关闭（纯透传）、L1 开、L1+L2 开 |
| 场景 | 随机、顺序 |

**关键指标（比 CPU 秒更能指导优化）**：

1. `ns/op`、`allocs/op`（`go test -bench`）；
2. **每 IO 的 read / write 系统调用次数** —— 用一个计数型 `net.Conn` 包装两侧连接统计，这是 O1/O2 的验收依据；
3. `-cpuprofile` 后的 `go tool pprof -top`。

> 待确认：`iscsiproxy.Portal` 是否暴露监听地址（bench 需要连上它）。若未暴露，补一个仅供测试/内部使用的 `Addr()`。

### O4. tee 路径免拷贝（miss 读）

**现状**：`session.go:805-843`（`collectTI`）把 Data-In 的数据段 `copy(p.buf[off:], data)` 拷进 pending 缓冲（`:816-822`），而这份数据刚刚才被 `readBody`（`frame.go:123-139`）读进 `fr.buf`。**每次 miss 读白拷一遍全量载荷**（1MiB 读 = 1MiB memcpy）。

**关键点**：`BufOffset` 与 `DataSegmentLength` **都在 BHS 里**，读完 48 字节头就已经知道这段数据该落到 pending 缓冲的哪个位置。所以可以让 `frameReader` 支持「直读进指定目标」：命中 pending 的 ITT 时，把数据段直接 `io.ReadFull` 进 `p.buf[bufOffset:]`，`frame` 的 data 就指向 pending 缓冲，一次多余拷贝都不花。

**风险（必须处理）**：

1. 现在 `collectTI` 在 `writeFrame` **之前**跑，最后一步会把缓冲所有权交给 cache 的 fill worker（`session.go:864` 的 `FillObservedRelease` 带 release 回调 → `readBufPut`）。若 data 直接就是该缓冲，**必须先写 socket、再交所有权**，否则可能写到刚被 pool 回收的内存。因此 TT 泵的顺序要改成：`rewriteTI` → `writeFrame`（在 `outMu` 内）→ 释放 `outMu` → `collectTI` 完成 pending。
2. `fr.read()` 需新增「上一层 frame 的 body 借用了外部缓冲」的状态，并在读下一帧前不回收。
3. 与 O1 的交互：直读进目标只适用于「数据段 ≤ 单帧」的情况；若该帧还要攒批（O1 会拷贝），则直读到 pending 缓冲仍成立（O1 从 pending 缓冲拷走）。**建议 O4 排在 O1 之后**，先拿到 O1 的收益再动数据所有权。

**收益**：miss/回填路径载荷拷贝减半。**风险**：中（所有权 + 顺序），靠 `invalidate_test.go`、`statseq_test.go`、live 测试兜。

### O5. 命中路径两次拷贝合一

**现状**：

1. `handleRead` 先取池缓冲（`readBufGet`），`ReadCachedBoth` 从 L1 拷进去；
2. `injectLocked`（`session.go:714-766`）再把整段拷进装配缓冲（`:748-749` 的 `copy(out[pos:], data[seg:seg+chunk])`）。

**每个命中读拷两遍**，外加一次池往返（`injectBufGet`/`injectBufPut` 与 `readBufGet`/`readBufPut` 各一次）。

**方案**：装配循环改为逐段向 cache 要数据，直接拷进装配缓冲的对应位置（`out[pos:pos+chunk]`）。做法：新增一个不走计数器、允许调用方指定 dst 的 cache 直读接口（等价于现有 `readCachedL1` / `readCachedL1L2`，只是「写进调用方给的 dst 且不涨 Stats」）。某段落空就整条放弃、退回转发 —— 此时**还没往 socket 写过任何字节**，安全。

**收益**：命中路径载荷拷贝从 2 次降到 1 次，省掉池往返；1MiB 级命中明显，4K 命中收益有限。

**风险**：中低。注意不要破坏 L2 命中的 promote 语义（`cache/cache.go:766-770`）与 `RequestHits` 计数口径。

---

## 4. P2 项详解

### O6. 热路径锁瘦身

profile 里锁的占比很小（`runtime.lock2` 只有 0.04s），所以这几项是**顺手清理**，不是收益主项。但每条命令省 4~5 次互斥锁，在高 IOPS 下仍有意义。

| 位置 | 现状 | 改法 |
|---|---|---|
| `session.go:314,318` → `target.go:303,263` | 每条命令取 `lunBlockSize()` + `getCache()` 各一次互斥锁 | 改 `atomic.Uint32` / `atomic.Pointer[cache.Cache]`，绑定后基本不变 |
| `session.go:768-779`（`maxSendSegment`） | 每次命中注入锁一次 `negMu`，但值在登录后就固定 | 写进 `atomic.Int32`，命中路径直接 Load |
| `target.go:624-638`（扫描检测） | 每次读命令一次 bucket 互斥锁 | `atomic.Int64` 存 `lastEnd` / 计数；每会话只有 1 个读 goroutine |
| `session.go:365,396,403`（pending） | 每次 miss 读 `&pending{}` 一次堆分配 | `sync.Pool` 复用，`collectTI` 出队后归还 |

### O7. L1 块大小对 4K 随机读

**现状**：默认几何是 `BlockSize = 64KiB`、`SectorSize = 4KiB`（`cache/cache.go:27-28`），agent 没有显式配置几何（`agent/cacheproxy.go:413-428` 只传 L1/L2 字节数与路径），全部走默认。

**问题**：`installValidated`（`cache/cache.go:1125-1174`）在新建块时 `make([]byte, c.blockSize)`（`:1141`）——**每次 4K miss 都会分配并清零 64KiB，而只填进 4K**。随机 4K 负载下这是纯浪费：分配、清零、驱逐、L2 记账全按 64KiB 计。

**方案**：把块大小做成可配置项，并按负载 A/B：`4KiB / 16KiB / 64KiB` vs「CPU 秒每 IO + 命中率」两项一起看。

**约束（必须实测，不能只算账）**：

- `sectorsPer = BlockSize/SectorSize` 必须在 1..32（`cache/cache.go:135-137`），4KiB 块 = 1 扇区合法；
- 块变小 ⇒ L1 索引条目 ×16、每分片队列变长，而 `removeFromQueueLocked`（`cache/cache.go:1504-1518`）是**线性扫描**；
- L2 槽位大小 = 块大小 ⇒ 小块的索引页与写次数都会变多（见 O8）。

### O8. L2 回填路径

**现状**：每个回填 `go c.writeL2(...)`（`cache/cache.go:1167`）——**每次回填一个 goroutine**；`writeL2` → `l2.WriteSectors` 全程持有 **L2 文件的单个全局写锁**（`l2/l2.go:759-779`）并做一次 `WriteAt`；`fence.enqueue/complete` 每次一对互斥锁 + 一次 `cond.Broadcast()`（`cache/cache.go:1568-1580`）。

**方案**：

1. 把「一次回填一个 goroutine」改成常驻 worker 池（复用现有 `l2WriteConcurrency = 8` 的信号量语义即可）；
2. `fence.complete` 只在确有 waiter 时 `Broadcast`（无 waiter 时跳过）；
3. 观察项：L2 索引页刷盘频率与 `writeIndexPage`（`l2/l2.go:589-617`）的整页 CRC + 重写成本，在随机 4K 负载下是否成为噪声源。

**风险**：中。L2 的失效顺序（fence）是与正确性绑定的机制，改动必须让 `cache` 包的既有测试全绿。

### O9. 纯透传（NoCache）路径

**现状**：未开缓存的库也走代理，注册成 `NoCache` 目标（`agent/cacheproxy.go:388-442` 与 `target.go`），目的是让每块盘的读写 IOPS 都可被观测。这类会话目前仍**逐帧解析 + 逐帧 write + 每命令 2 次 `target.mu` + 逐帧 body 缓冲**。

**注意**：O1 落地后，这类会话的 syscall 数已经大幅下降（**O1 是共用收益**），收益大头自动拿到。

**剩两个可选动作**：

1. `NoCache` 会话从第一帧起就走 `fr.splice`（`frame.go:70` 的 `io.Copy`，32KB 批处理，天然合并写）；
2. 或对未开缓存的库干脆**绕过代理、直连挂载**。

**代价**：两者都会**丢掉该卷的读写 IOPS 计数**，而 `agent/cacheproxy.go:390-391` 明确说这是有意为之的产品行为。**因此 O9 需要产品拍板，不作为技术方案默认推荐项。**

---

## 5. 明确不做的

| 想法 | 为什么不做 |
|---|---|
| 手写 PDU 解析替换 `bufio` | 读侧已经很便宜（`bufio.Read` 0.60s + `ReadBHS` 0.30s），换不来收益 |
| 合并/取消 `readBufPools`、`injectBufPools` 分档池 | 设计正确（尺寸分档避免大缓冲被小请求占死），热路径零分配 |
| 给命中路径加并行 goroutine | 单会话每方向只有一个泵，并行不解决问题，只加锁与调度成本 |
| 动 StatSN/CmdSN 重写逻辑 | `rewrite.go`、`captureTargetSeq` 整篇都是踩坑记录，与本方案无收益交集 |
| 热路径日志清理 | 已确认无 per-PDU 日志（只有错误路径 `Warn`），无需处理 |
| 调大 `GOGC` | 整条路径基本无小对象分配，先拿到 O1 再看 GC 是否真的进榜 |

---

## 6. 实施顺序与验收口径

### 6.1 顺序（每步独立可交付、独立可回退）

| 步骤 | 内容 | 前置 |
|---|---|---|
| 1 | O3（bench + 可选 pprof 开关）——**先建口径再动手** | 无 |
| 2 | O2（大帧 gather 写）——最小改动，先拿到一点收益并验证 bench 可信 | O3 |
| 3 | O1（跨帧合并写）——主项，带配置开关 | O2 |
| 4 | O6（锁瘦身）——顺手清理 | O1 |
| 5 | O5（命中拷贝合一） | O1 |
| 6 | O4（tee 免拷贝）——动数据所有权，排在数据流改动最后 | O1/O5 |
| 7 | O7 / O8（块几何、L2 回填）——按 bench 结果决定是否值得做 | 全部 |

### 6.2 验收口径

**性能（对 bench，三种链路各一组）**

- 代理侧 **CPU 秒 / GB** 下降目标 **≥30%**（以 profile 结论推理，主要来自 O1+O2；40% 需 bench 确认）；
- **每 IO 的 write syscall 次数下降 ≥50%**（O1 的直接指标）；
- 吞吐 / IOPS **不低于基线**；命中率**不变**（O1/O2 不应影响命中）。

**功能（必须全绿，不可放松）**

- `go test ./internal/iscsiproxy/...`、`./internal/iscsicache/...`、`./internal/agent/...`；
- `statseq_test.go`、`invalidate_test.go`（序号与失效是这套代码最脆的地方）；
- 真机 live 三件套：`TestLiveProxyAgainstRealBackend`、`TestLiveWriteThroughput`、`TestLiveCDMProfile` 不出现连接重置 / StatSN 回退 / IOPS 塌陷。

### 6.3 回归风险清单

| 风险 | 触发点 | 兜底 |
|---|---|---|
| 字节顺序或丢字节 | O1 的 flush 时机遗漏（splice 分支、循环退出、错误路径） | 配置开关退回逐帧；bench 加「收发字节数一致」校验 |
| 注入序号错位 | O1 让「记录序号」与「真正写出」之间隔了几帧 | 全部在 `outMu` 内且顺序一致；`statseq_test.go` + live 序号校验 |
| 写到已回收内存 | O4 的缓冲所有权顺序 | 严格「先写 socket 再交所有权」；`-race` 下跑 miss 读压测 |
| 延迟增加 | O1 攒批导致小响应被推迟 | flush 只在 `Buffered()==0` 时触发（无数据立刻发）；live bench 看 P99 |
| Windows 版本差异 | O2 依赖 `Writev` 是 gather 写 | bench 统计 syscall 次数，若退化则保留分支 |

---

## 7. 附录：profile 复现与解读

```bash
# 采集（需真机后端；VAULT_ISCSI_TEST_ADDR / VAULT_ISCSI_TEST_IQN）
go test ./internal/iscsiproxy -run TestLiveWriteThroughput -cpuprofile cpu -timeout 40m

# 解读
go tool pprof -top -nodecount=40 cpu          # flat：看 CPU 停在哪
go tool pprof -top -cum -nodecount=40 cpu     # cum：看调用链
go tool pprof -peek='runtime\.cgocall' cpu    # syscall 的调用者分布
go tool pprof -peek='net\.\(\*Buffers\)\.WriteTo' cpu
```

读这份 profile 时记住三点：**① 只看 `guardedPump` 子树是代理自身；② `backend.Initiator` 是测试工具；③ 墙钟 24.5s 里只有 6.3s 是 CPU，说明瓶颈常在等后端，CPU 优化只对「本地 hop 打满 CPU」的高 IOPS 场景有意义。**

---

## 8. 实施记录（O1 + O2）

> 正文 §3 里 O1/O2 的「现状」是改动前的快照，落地方式以下面为准。

### 8.1 O2：确认 `net.Buffers` 在 Windows 上是 gather 写

原 `frame.go` 的注释断言「Go's net.Buffers has no scatter-gather write on Windows, so a multi-slice frame costs one syscall per slice」，据此把大帧拆成两次 `Write`。**这条注释是错的**，查 Go 源码即可确认：

`internal/poll/fd_windows.go` 的 `(*FD).Writev` 把**所有** slice 装进一个 `WSABuf` 数组，然后走**单个** `execIO` 调一次 `WSASend`：

```go
bufs := newWSABufs(buf)
defer freeWSABufs(bufs)
n, err := fd.execIO('w', func(o *operation) (qty uint32, err error) {
    err = syscall.WSASend(fd.Sysfd, &(*bufs)[0], uint32(len(*bufs)), &qty, 0, &o.o, nil)
    return qty, err
})
```

生产路径也确认吃得到这条路：`ic` 来自 `*net.TCPListener.Accept()`、`bc` 来自 `net.DialTimeout("tcp", ...)`，动态类型都是 `*net.TCPConn`，实现了未导出的 `writeBuffers`，正是 `net.Buffers.WriteTo` 优先走的那条。

**落地**：`writeFrame` 删掉 `writeCoalesce` 阈值与 scratch 参数，一律交给 `net.Buffers`（多 slice 一次 gather、零 memcpy）；纯头帧加一条 `w.Write(hdr)` 快速路径。

> 注意一个容易踩的点：`net.Buffers.WriteTo` 只在目标实现 `writeBuffers` 时才 gather，`bytes.Buffer` 这类普通 `io.Writer` 会**退化成逐 slice 写**。所以写测试时不能用 Write 次数去断言 `writeFrame` 的行为。

### 8.2 O1：`frameBatch` 跨帧攒批

新增 `frameBatch`（`frame.go`）：同一方向连续到达的帧拷进一个缓冲（`frameBatchCap = frameReadBuffer = 64KiB`），凑成一次 `Write`。

分流规则：

| 情形 | 处理 | 理由 |
|---|---|---|
| 帧 ≤ `frameCopyLimit`（16KiB） | 拷进批 | memcpy 几百 ns，赌一次合并省下一次 syscall |
| 批为空且帧更大 | 零拷贝直发 | 不拷也只是一次 syscall，拷贝它要等"下一帧恰好已在窗口里"才回本 |
| 帧比整批还大 | 先 flush，再零拷贝直发 | 透传帧的数据段不受注入上限约束 |
| 带 HD/DD | 先 flush，再按帧写 | 摘要会话本来就降级为纯透传 |

flush 时机：**只有读者手里再没有下一个字节（`fr.r.Buffered() == 0`）时才 flush**。此时继续读就要等 socket，已攒的字节本来就该发出去了 —— 攒批因此不引入任何额外延迟。循环退出用 `defer` 兜底 flush。

### 8.3 实现中发现并一并修掉的两个坑

1. **注入必须共用同一个批（`s.tiBatch`）**。攒批带来一个新窗口：T→I 泵 append 了帧但还没 flush 就释放 `outMu`，而注入（`handleRead` → `injectLocked`）正持 `outMu` 直接写 `s.it` —— 注入会插到批的前面，**StatSN 先大后小**，正是真机上 RST 断链那一类故障。
   修法：initiator 方向的每一次写入都经过 `s.tiBatch`，转发与代答都在 `outMu` 内追加，于是"谁先谁后"由追加顺序决定，与 flush 发生在哪一刻无关；注入结束后立刻 flush，保证代答不会滞留。
2. **`handleCommandIT` 返回 true 时的 `continue` 会跳过 flush**，把之前攒下的透传命令一直压到下一帧到达为止（QD1 下就是死等）。改成代答后同样走 flush 判断。

### 8.4 测试

新增（`proxy_test.go`）：

| 测试 | 钉住什么 |
|---|---|
| `TestFrameBatchMatchesWriteFrame` | 空帧 / 带 pad / 4K / 超拷贝上限 / 超批容量 / 带 AHS / 带摘要，攒批写出的**字节流与逐帧写完全一致** |
| `TestFrameBatchCoalescesWrites` | 8 个小帧从 16 次 Write 并成 **1 次**，且字节顺序不变 |
| `TestInjectedHitQueuesBehindForwardedFrames` | 批里压着未写出的透传帧时发生注入：透传帧仍在注入**之前**，注入立刻写出，StatSN 接续（41 → 42） |

`go vet ./internal/...` 干净；`./internal/iscsiproxy/...`、`./internal/iscsicache/...`、`./internal/agent/...` 全绿。

**验证缺口（如实记录）**：本机没有 gcc，`go test -race` 跑不起来（`-race requires cgo`），因此并发正确性目前靠上述不变量测试 + 代码审查保证。**改动涉及数据流顺序，合入前应在有 cgo 的环境补一次 `-race`**，并跑一遍真机 live 三件套确认字节流与序号。

### 8.5 尚未落地

O3（pprof 开关、v2 自研 bench）、O4、O5、O6、O7、O8、O9 都还没动。其中 **O3 建议优先**：这轮改动的收益只有 bench 能量出「每 IO 的 write 次数」，否则只能靠下一次真机 profile 反推。



