# iSCSI 读缓存代理性能优化方案

> 前置：本方案建立在已完成的两项改造之上 —— **异步回填**（读未命中不再同步装 L1）与 **无锁写**（删除 `Proxy.execMu` 全局独占锁）。见 `iSCSI读缓存-异步回填与无锁写改造.md`。
>
> 本文件只描述**待评估的优化项**，不含代码改动。是否实施由评审后决定。

---

## 一、现状与实测

端到端基准（同一 LUN，代理 vs 直连吞吐占比）：

| 场景 | 读（顺序） | 写 |
|------|-----------|----|
| Q1T1 | 70% | 50% |
| Q8T1 | 75% | 100% |

> **前提**：表中的读为**顺序读**（非 4K 随机）。这个前提直接决定读路径的优化方向 —— 见第二、五节。

目标：抬高读的绝对占比；查清写为何损失一半。

---

## 二、结论先行

1. **写 Q1T1 的 50% 不是架构性的，根因是两段链路都协商了 `InitialR2T=Yes`，每次写被迫多走两次 R2T 往返。可消除（见方案 0）。**
   早前"多一跳网络 RTT"的解释不成立：客户端↔代理是 127.0.0.1，该跳几乎免费。真正原因见第三节。

2. **读的 70→75% 是【顺序读】的成绩**（非 4K 随机），约有 25% 的可攻开销，分两类：
   - **结构性（方案 A）**：缓存开启时读永不流式，后端整段读完后才整段发出，两段传输串行。**收益取决于单条命令的 payload 段数**：>256K（后端 `MaxRecvDataSegmentLength`）才有可重叠的下一段，64K 单段读收益有限（见第五节）。
   - **微开销（方案 B）**：热路径上的每次分配与每读一次的自旋锁。
   - **确定性浪费**：`fillRun` 的 L1 安装仍是同步的，夹在"后端读完"与"发送"之间（详见第五节附带）。
   > R2T 只作用于**写**（读由存储发 Data-In，不涉及 R2T），因此它解释不了读的差距 —— 这正与「写 50% / 读 70-75%」的不对称吻合。

3. **L2 开启时，写还有一处可修剪的延迟（方案 C）**：`Invalidate` 会 `fence.wait` 等**所有**在途 L2 回填（含与本次写无关的块）。bench 关闭 L2 时无影响。

---

## 三、方案 0（P0，写）：消除双重 R2T 往返

### 根因与证据

代理在**两段链路**上都公告 `InitialR2T=Yes` + `ImmediateData=No`，导致任何一段都必须先等 R2T 才能开始传写数据：

- **前端目标**（对客户端）：`frontend/session.go` 的 operational keys 协商 `InitialR2T=Yes` / `ImmediateData=No`（注释明确写"写路径总是靠 R2T 驱动整个 payload，未处理随命令到达的未请求数据"）；`receiveDataOut` 无条件对整段 payload 发 R2T 再等下一条 Data-Out。
- **后端发起端**（对存储）：`backend/initiator.go` 的 `operationalKeys` 同样 `InitialR2T=Yes` / `ImmediateData=No`；`answerR2T` 只会"应答 R2T"，从不主动发未请求 Data-Out。

一次 4K 写的实际时序：

```
客户端 CDB → 代理 R2T → 客户端 Data-Out → 代理→存储 CDB → 存储 R2T → 代理 Data-Out → 存储 GOOD → 代理 GOOD
```

**两段链路各多一个 R2T 往返**，这就是 ~50% 的来源。

### 改法

1. **前端：协商 `InitialR2T=No` + `FirstBurstLength > 0`**
   - 让客户端可把写数据（至多 FirstBurstLength）随命令立即发出，省掉一次客户端↔代理往返。
   - 需新增能力：接收「紧跟在命令 PDU 之后、没有 TTT 的未请求 Data-Out」，按 ITT/CmdSN 归入该写命令的缓冲（现状 `handleDataOut` 只按 TTT 路由）。
   - 未请求部分收满后不再发 R2T；不足部分继续走现有 R2T 流程。
2. **后端发起端：协商 `InitialR2T=No`，主动发首突发**
   - 在 CDB 之后立即发一段未请求 Data-Out（长度 = `min(本端, 存储通告) 的 FirstBurstLength`，并按对端 `MaxRecvDataSegmentLength` 切分 PDU），余量再等 R2T 并复用现有 `answerR2T`。
   - 省掉一次代理↔存储往返。
3. **协商与回退语义（必须严格）**
   - `InitialR2T` 为 **OR 协商**：只有双方都为 No 才用 No。对端（客户端 / 存储）若回 Yes，**必须保留现有 R2T 路径**，不得假设一定拿到 No。
   - `FirstBurstLength` 取 **min(本端通告, 对端通告)**；未请求数据总量不得超过对端 FirstBurstLength。
   - `ImmediateData` 可保持 No（用 FirstBurstLength 承载首突发即可），也可评估开启；保持 No 可少一条"命令 PDU 携带数据"的解析分支。
   - `DataSequenceInOrder=Yes` 下，未请求 Data-Out 必须紧跟命令 PDU 且按 offset 递增。

### 风险 / 代价

- **必须真机验证**：未请求数据与真实 Microsoft initiator / Microsoft iSCSI target 的交互只能靠真机与抓包确认，模拟环境会掩盖协议缺陷（此前已有此类教训）。
- 首突发过大需按对端 `MaxRecvDataSegmentLength` 正确分段，否则对端会拒收或出错。
- R2T 回退路径必须保留并覆盖测试。

### 预期收益

- 每段链路减少一个往返：Q1T1 写延迟近似减半，**有望收掉这 50% 的大部分**，逼近直连。
- 对任何 QD 的写都受益（更早开始传输，重叠窗口更长），尤其大块/顺序写。

### 验证

- 真机：`vault-iscsi-bench` 对比 Q1T1 写占比（目标：由 50% 明显上升）。
- 抓包：确认每段链路握手次数由 2 次降为 1 次。
- 覆盖：对端强制 `InitialR2T=Yes` 时回退到 R2T 的正确性。

---

## 四、第 0 步（读/微优化）：先度量，再动手

方案 A/B 在改前先拿真实热点，避免凭直觉优化。

**做法**：给 `cmd/vault-iscsi-bench` 增加 `-cpuprofile` / `-memprofile`（或代理进程内挂 pprof endpoint），在 Q1T1 与 Q8T1 各采一次，对照直连基线。

**需要确认**：
- **被测顺序读的单条命令大小**：它决定后端的 Data-In 段数，进而决定方案 A 的重叠窗口（见第五节）。
- **Q1T1 顺序读的延迟分解**：从收到 CDB 到发出第一段 Data-In，"后端读等待 / 安装 copy / 发送"各占多少。
- 读热路径的 CPU 时间分布：backend 收发 / memcpy / 分配 / 锁 各占多少？
- `serveMiss`、`readAligned`、`source.ReadAt`、`fillRun` 的分配是否真的进入 GC 热点（看 alloc_space）？
- Q1T1 读差距里「真实转发开销」与「2 次真实数据传递」各占多少。

**口径提醒**：性能数字必须明确是「代理端到端 SCSI 命令延迟」还是「存储访问延迟」，不可混用。

> 方案 0 不需要这一步：其根因已由代码（协商参数）直接坐实，验证方式是真机占比 + 抓包。

---

## 五、方案 A（读，结构性）：缓存开启时的流式转发

### 现状

`proxy.ExecuteStream` 只在**无缓存**时才走流式：

- `p.c == nil && streamable` → `readStream` → `backend.ExecStream`，段到即发；
- 否则退回 `p.Execute`：先把整个 payload 读进 `readBufs` 缓冲，再单段 `sink.Segment(0, data, true, ...)`。

因此**有缓存时，后端读与客户端 Data-In 严格串行**：客户端要等后端整段读完才收到第一个 Data-In。这是顺序读差距的结构性来源。

### 收益的前提：单条命令的 payload 跨几个 Data-In PDU

重叠只能发生在"段与段之间"。后端 `MaxRecvDataSegmentLength = 256K`，所以单条命令的段数决定流式收益：

| 单条命令读大小 | 后端 Data-In 段数 | 流式重叠收益 |
|----------------|-------------------|--------------|
| ≤256K（如 64K）| 1 | 小：没有"下一段"可重叠，只省"收到即转"的一次等待 |
| >256K（如 1M） | ≥2 | 大：段 N 的转发与段 N+1 的接收可并行 |

**方案 A 的收益与读大小强相关。** 先确认被测顺序读的单条命令大小（见第四、八节）再定优先级。对单段（64K）读，Q1T1 的差距主要来自 loopback RTT 与前端/后端两段 goroutine 处理，属两段式中继的固有下限，方案 A 收不回多少。

### 设计

为 cache 增加「流式读」入口（示意）：

```
ReadStream(ctx, off, length int64, src Source, sink DataInSink) (status, residual, ...)
```

按命中情况分段执行：

1. **全命中**：直接从 L1 分片写出（可多段），零后端读。
2. **全未命中**：读前按 block 捕获各 shard epoch → 调后端 `ExecStream` → **每收到一段：既 `sink.Segment` 发给客户端，又写入对应 block 的 staging**；段结束时交 `installValidated`（epoch 校验）安装 L1，并按块 `writeL2`。
3. **部分命中**：命中段直接发；缺失段走后端流，规则同 2。

关键点：**「发给客户端」与「灌 L1/L2」共用同一批 buffer 的读取结果**，不再各走一遍。

### 一致性处理

- 不变量不变：**写失效 bump epoch 必须 happens-after 后端写完成**；**回填 install 必须在安装前校验捕获的 epoch**。
- 流式下 epoch 捕获点：进入某 block 的后端读**之前**（与现有 `serveMiss`/`fillRun` 一致）。中途若该 shard 被 bump，install 被拒（丢弃该段），只损失一次预取。
- 「已发给客户端的数据是写前数据」与直连语义一致：同一 SCSI 命令的读结果本就可能落在写前/写后，取决于命令排序；代理不引入新的可见性偏差。
- L2 仍走 `l2Gate` + `fence` 保护，与现状相同。

### 风险 / 代价

- 需要 cache 与 `DataInSink` 协作；**客户端命令范围与缓存 block 边界可能不对齐**，分段逻辑要处理首尾半块。
- install 失败（epoch 变化）不得影响已发出的数据，需保证 sink 的错误传播路径清晰。
- 改动面较大，必须补流式一致性测试（含「流中途失效」）。
- 单段（≤256K，如 64K）读收益有限；多段（>256K）读收益随段数增加而增大。

### 预期收益

- 单条命令 payload >256K（≥2 个后端 Data-In 段）时，读延迟下降「后端读与客户端发重叠」的那部分，段越多收益越大；
- 大块/顺序传输在任何 QD 都受益（更早首字节、更长重叠窗口）；
- 单段（≤256K，如 64K）命令收益仅来自"收到即转"，量级有限。

### 附带（低风险，收益待测）：`fillRun` 的 L1 安装异步化

`serveMiss` 已把 L1 安装交给 `enqueueFill` 的异步 worker；但批量/顺序未命中走的 `fillRun`（`cache.go`）仍是**同步**安装 —— 后端读完后，在读的关键路径上逐块 `installValidated`（把 payload 再 copy 一遍进 L1）才返回，顺序读每条命令都要付一次串行 memcpy。

- 改法：与 `serveMiss` 对齐，把逐块安装交给 fill worker，使"发送"与"安装 copy"重叠。
- **注意代价**：`fillRun` 现在直接读进 `dst`（客户端缓冲，发送后即释放），要异步安装就得先把数据拷进 worker 拥有的池化缓冲 —— 即**多一次拷贝**换取把拷贝移出关键路径。净收益需实测，可能只是"延迟换吞吐"。
- 与方案 A 独立，可单独落地；风险低（复用既有异步安装路径）。

---

## 六、方案 B（微优化，低风险，可叠加）

均为「已定位的局部浪费」，逐条独立、可单独验证：

| 编号 | 位置 | 现状 | 改法 | 收益 | 风险 |
|------|------|------|------|------|------|
| B1 | `cache.go` `serveMiss`（`buf := make([]byte, n*sec)`） | 每次未命中堆分配 + 清零，**绕过 `readBufs` 池** | 为 sector 级缓冲建 `sync.Pool`（复用 `readBufs` 的容器技巧） | 去掉每次 miss 的分配，降 GC 压力 | 低（需保证归还时机 = 安装完成后） |
| B2 | `proxy.go` `source.ReadAt`（`cdb := make([]byte, 16)`） | 每次后端读堆分配 16B | 改栈上 `var cdbArr [16]byte` 再切片 | 去掉每次后端读的小分配 | 极低 |
| B3 | `cache.go` `fillRun`（`epochs := make([]uint64, …)`） | 每次调用堆分配 | 常见 ≤16 块用栈数组，超出再 `make` | 去掉批量读路径的分配 | 极低 |
| B4 | `cache.go` `readAligned`（`buf := make([]byte, …)`） | scan 旁路路径每次读堆分配 | 同 B1 池化 | 去掉旁路读的分配 | 低 |
| B5 | `cache.go` `scanDetector.observe` | 开启 scan 检测时**每次读取桶锁** | 随机负载下用 `atomic` CAS 维护 `lastEnd/run`，免锁 | 去掉每读一次的互斥 | 中（需保持既有 scan 判定语义与测试） |
| B6 | `proxy.go` `putReadBuf` | 一次归还做两次 `sync.Pool` 操作（取容器 + Put） | 用有界 `chan []byte` 或直接以切片头入池，单次操作 | 减半池操作 | 中（需保证零分配语义不回退） |

量级参考：按 40k IOPS、每次 4KiB，仅 `serveMiss`/`readAligned` 的分配即约 **160 MiB/s 的 GC 流量**。

---

## 七、方案 C（生产，L2 开启时的写延迟）：把 L2 失效栅栏收窄到范围

### 现状

`Cache.Invalidate` 在 L2 非空时会 `fence.wait(c.fence.target())`，等待**所有**在途 L2 回填完成，才做 `l2.InvalidateSectors` 并让写返回 GOOD。即便这些回填与本次写毫无关系，也会把写的 ACK 拖到最慢的那次 L2 磁盘写之后。

### 改法

- 把栅栏从"全局计数"改为**按块/按范围**：写只等待"覆盖本次失效扇区"的在途 L2 回填，其余不阻塞。
- 保持 `l2Gate` 语义（写独占跨越「bump + L2 失效」），正确性模型不变。
- **不改** L1 的同步清位：受影响块的 `clearValid` 只有 O(受影响块) 次位操作（4K 写 = 1 块），不值得为它引入复杂度。

### 预期收益

- L2 开启、读密集（大量在途回填）时，写的 ACK 延迟明显下降。
- bench（L2 关闭）无影响。

### 明确不做的两种取巧（已评估，不成立）

1. **"把 L2 失效整体推迟到 ACK 之后，读端事后复查 epoch"** —— 有窗口：若 epoch 已 bump、L2 失效被延后，读者会捕获到**新** epoch 却读到**旧** L2 数据，事后复查通过 → 陈旧数据被装进 L1。除非给 L2 扇区加 generation 戳，否则不成立（这正是现设计要 `l2Gate` 独占跨越「bump + L2 失效」的原因）。
2. **"L1 命中改成 epoch 校验、用 epoch 取代 valid 位，从而把清位也异步化"** —— 当前 epoch 是 **shard 级**，任一写都会 bump 整个 shard，按 epoch 判命中会把同 shard 内无关缓存全部误杀。要这么做必须把 epoch 下沉到 entry/block 级，收益不抵复杂度。

---

## 八、验证计划

0. **前置度量**：确认被测顺序读的**单条命令大小**（决定后端 Data-In 段数、进而决定方案 A 的重叠窗口）；对 Q1T1 顺序读做延迟分解 —— 从收到 CDB 到发出第一段 Data-In，"后端读等待 / 安装 copy / 发送"各占多少。
1. **单测 / 回归**：`go test ./internal/iscsicache/... -count=3`、`go vet`、`gofmt`、`GOOS=linux CGO_ENABLED=0 go build`（iscsicache 全树）。
2. **方案 0 专项**：真机占比（Q1T1 写）+ 抓包确认握手次数；`InitialR2T=Yes` 回退路径测试。
3. **新增测试**（针对 A）：流式未命中的数据正确性、流中途失效不安装、部分命中的分段拼接。
4. **真机端到端**：`vault-iscsi-bench`，直连 vs 代理，Q1/Q8/Q32，读 + 写，观察占比曲线。
5. **验收**：写占比（Q1T1）显著上升并接近直连；读占比不低于当前；无正确性回归。

---

## 九、优先级建议

1. **P0**：**方案 0（消除双重 R2T 往返）** —— 根因明确、收益最大，直接决定 Q1T1 写能否达标。
2. **P1**：方案 B（微优化，低风险，几乎无回归面）。
3. **P1/P2（条件）**：**方案 A（读流式转发）** —— 若被测顺序读单条命令 >256K（多段），提为 P1，是读路径主杠杆；若为 64K 单段，降为 P2，收益有限。实施前先做第四/八节的度量。
4. **P2**：方案 A 附带 —— `fillRun` 安装异步化（低风险，收益待测，会引入一次额外拷贝）。
5. **P3**：方案 C（L2 开启时的写延迟修剪，生产收益，bench 无感）。