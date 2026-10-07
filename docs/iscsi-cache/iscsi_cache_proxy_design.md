# iSCSI-Cache 设计文档

基于 iSCSI 协议的多级缓存代理系统

- **版本**：v2.0
- **日期**：2026-09-29
- **状态**：设计稿（已修正全部已知数据一致性缺陷）

---

## 目录

- [一、项目定位](#一项目定位)
- [二、总体架构](#二总体架构)
- [三、iSCSI 协议处理](#三iscsi-协议处理)
- [四、L1 / L2 缓存设计](#四l1--l2-缓存设计)
- [五、缓存污染防御](#五缓存污染防御)
- [六、预取引擎](#六预取引擎)
- [七、写策略与写一致性](#七写策略与写一致性)
- [八、iSCSI 协议级加速与可靠性](#八iscsi-协议级加速与可靠性)
- [九、请求调度](#九请求调度)
- [十、下游代理机制（怎么连后端）](#十下游代理机制怎么连后端)
- [十一、整个系统怎么跑起来](#十一整个系统怎么跑起来)
- [十二、配置示例](#十二配置示例)
- [十三、性能预期](#十三性能预期)
- [十四、模块接口契约](#十四模块接口契约)
- [十五、iSCSI 协议完备性测试方案](#十五iscsi-协议完备性测试方案)
- [十六、风险与注意事项](#十六风险与注意事项)

---

## 一、项目定位

### 1.1 一句话定义

iSCSI-Cache 是一个工作在 iSCSI 协议层的通用多级缓存代理系统。它以用户态应用的形式，在 iSCSI 协议数据路径中插入缓存逻辑，前端伪装为 iSCSI Target 接受 Initiator 连接，后端作为 iSCSI Initiator 连接真实存储 Target。

### 1.2 设计约束（重要）

- **不入侵业务**：系统无法预知 Initiator 侧的业务语义（不知道游戏在哪里 loading、OS 处于哪个启动阶段），所有决策仅基于纯 IO 行为特征（LBA 序列、顺序性、访问频率、IO 大小）。
- **核心诉求**：服务到大部分的读请求，且不能因一次偶尔的冷数据读取而污染 / 顶替缓存中的热数据。
- **与操作系统存储栈无关**：不依赖 bcache、Storage Spaces、ZFS 等任何系统机制，纯用户态协议层实现。
- **数据正确性是第一优先级**：任何优化不得改变数据语义；宁可拒绝服务，不得返回错误数据。

### 1.3 维度说明

| 维度 | 说明 |
|---|---|
| 协议位置 | iSCSI 协议层（SCSI 命令级），非文件系统、非块设备驱动 |
| 平台 | **仅支持 Linux**（x86_64 / aarch64）。依赖 `O_DIRECT`、`io_uring`、`fallocate(2)`、`madvise(2)`、POSIX 线程；这些接口在 Windows 上不可用，libiscsi 的 Windows 支持也不完整 |
| 前端角色 | 伪装为 iSCSI Target，接受 Initiator 连接 |
| 后端角色 | 作为 iSCSI Initiator，连接真实存储 Target |
| 缓存层级 | L1（进程内内存）+ L2（本地 SSD，O_DIRECT + io_uring） |
| 隔离粒度 | 缓存按 **LUN** 共享；配额与统计按 Session 计算 |
| 一致性范围 | 单机，不做分布式缓存一致性 |

---

## 二、总体架构

### 2.1 数据流

```
Initiator (ESXi / Windows / Linux)
        │
        ▼  iSCSI（标准协议，TCP 3260）
┌──────────────────────────────────────────────────┐
│  Go 层：iSCSI Target 前端                        │
│  连接管理 · PDU 编解码 · 会话状态机 · 命令分发    │
└──────────────────┬───────────────────────────────┘
                   ▼  CGO（纯异步，立即返回，不阻塞）
┌──────────────────────────────────────────────────┐
│  C 层：缓存引擎                                  │
│  L1 内存 + L2 SSD · 索引 · 淘汰 · 准入 · 预取     │
└──────────────────┬───────────────────────────────┘
                   ▼  回调驱动
┌──────────────────────────────────────────────────┐
│  C 层：后端封装（libiscsi 异步 READ / WRITE）     │
│  ── 单线程事件循环（per-LUN）                     │
└──────────────────┬───────────────────────────────┘
                   ▼  iSCSI
              后端 Target（HDD / 任意 iSCSI 存储）
```

### 2.2 分层职责

| 层次 | 技术选型 | 职责 |
|---|---|---|
| iSCSI 前端 | Go（纯 Go 实现 RFC 3720 子集） | TCP 监听、PDU 编解码、会话状态机、SCSI 命令分发、Task Management |
| 缓存引擎 | C（手写高性能核心） | L1 slab、L2 O_DIRECT、索引、淘汰、准入、预取、Journal |
| 后端连接 | C（libiscsi） | per-LUN 单线程事件循环；连接后端 Target、异步发送 READ / WRITE |
| 编排层 | Go | 配置热加载、metrics 采集、日志、信号处理、进程生命周期 |

### 2.3 CGO 调用模型（不阻塞 Go runtime）

每个 SCSI 命令触发一次 CGO 调用，但**只在 C 侧注册请求后立即返回**，不等待后端 IO：

```
Go handler ──CGO──▶ cache_submit(req)
                       ├─ 查 L1/L2（命中则填充 req->result，唤醒完成器）
                       └─ Miss：注册到待发队列，返回 PENDING
                    ◀── 返回（不等待）
Go handler 把 req 挂到该连接/命令的等待表

... 后端数据到达，libiscsi 事件循环触发回调 ...
backend_cb() ──▶ cache_fill() ──▶ complete_request(req)  （另一 CGO 调用，或 //export 回调 Go）
```

**关键点**：
- 后端 IO 未完成时，CGO 调用**立即返回**，不持有 OS 线程。Go runtime 不会因等待磁盘而无限扩增线程。
- 缓存命中路径同样立即完成，不走后端。
- C 侧持有 Go 分配的 buffer 时，必须用 `runtime.Pinner` 或 `C.malloc` 固定，禁止让 Go GC 回收正在被 C 侧引用的内存。

---

## 三、iSCSI 协议处理

### 3.1 支持的 SCSI 命令

| 命令 | 缓存行为 | 说明 |
|---|---|---|
| READ(10) / READ(16) | 缓存读 | 查 L1 → L2 → 后端，命中逐级返回 |
| WRITE(10) / WRITE(16) | 缓存写 | 写 L1（标记 per-sector dirty）；FUA=1 必须同步持久化 |
| INQUIRY | 本地处理 | 返回伪造厂商信息，**Serial Number 必须按 LUN 唯一** |
| REPORT LUNS | 本地处理 | 返回本代理导出的 LUN 列表（含 LUN 类型字段） |
| READ CAPACITY(10) | 本地缓存 | 返回 32 位 LBA（限 2TB/512B 逻辑块） |
| SERVICE ACTION IN / READ CAPACITY(16) | 本地缓存 | 返回 64 位 LBA + **BlockDescriptor** 逻辑块长度与拓扑 |
| MODE SENSE(10/16) | **强制透传** | 返回后端真实块参数（逻辑块长度、缓存模式、写保护），不得模拟 |
| MODE SELECT(10/16) | 透传 | 下发后端，成功后才应用 |
| TEST UNIT READY | 本地处理 | 检查后端连通性 |
| SYNCHRONIZE CACHE(10/16) | 刷脏 | 将指定范围 dirty 块持久化到后端，完成才回 GOOD |
| UNMAP / WRITE SAME | 穿透 + 失效 | 同时清缓存对应范围；UNMAP 后读必须返回零 |
| PERSISTENT RESERVE IN/OUT | 透传 | **不可缓存 PR 状态**，每次从后端查询 |
| COMPARE AND WRITE | 透传 | ESXi ATS 锁，必须原子、必须按后端真实结果 |
| SECURITY PROTOCOL IN/OUT | 透传 | SED 自加密盘解锁/密钥管理，不得缓存、不得伪造 |
| TUR / REQUEST SENSE / INQUIRY EVPD | 本地/透传 | 状态清理与 VPD 页 |

### 3.2 会话与缓存命名空间（按 LUN 共享，按 Session 配额）

**这是数据正确性的关键约束**：同一个 LUN 可能被多个 Session 访问（MPIO 的多条路径、ESXi 多主机共享 VMFS）。若每个 Session 持有独立缓存副本，Session A 的写入对 Session B 不可见，会形成**脏读**。

**规则**：

- **缓存归属 LUN**：`lun_t` 拥有该 LUN 的 L1/L2 索引、淘汰队列、dirty 集合。所有访问该 LUN 的 Session 看到的是**同一份**缓存数据。
- **配额与统计归属 Session**：`session_t` 只持有配额计数（用于流量隔离、限流、统计），不持有数据块。
- **写屏障按 LUN 串行化**：同一 LBA 的写命令按接收顺序提交，读命令若与未完成写冲突则等待该写完成（见 7.4）。

```
session_table:  session_id → session_t   （配额、统计、连接状态）
lun_table:      lun_id     → lun_t       （L1、L2、索引、dirty set、淘汰队列）
```

### 3.3 命令处理流程

```
SCSI Command PDU 到达
        │
        ▼
解析 SCSI CDB（提取 Opcode、LBA、Transfer Length、FUA、TaskAttr）
        │
        ▼
命令分发
   ├── 读命令  → cache_submit_read()    （异步，立即返回 PENDING/READY）
   ├── 写命令  → cache_submit_write()   （按写顺序队列串行化）
   ├── 管理命令 → 本地处理 / 透传
   └── Task Management → 取消在飞命令、清理状态
        │
        ▼
完成回调 → 构造 SCSI Response（Status + Sense + Residual） → Data-In / Data-Out
```

### 3.4 Data-In / Data-Out 与 DataSN

- 单条 READ 的响应若超过 `MaxBurstLength`，必须拆分为多个 Data-In PDU，**按 DataSN 递增、顺序发送**，最后一个 PDU 置 `F` 位。
- 单条 WRITE 的 Data-Out 阶段，R2T 之后 Initiator 发送的数据必须按 DataSN 重组；若收到重复或乱序 DataSN，按 RFC 3720 处理（重复 DataSN 的 PDU 可丢弃或重传）。
- 序列错误（DataSN 不连续、Final 位缺失）必须返回相应 Reject PDU，不得静默吞掉。

---

## 四、L1 / L2 缓存设计

### 4.1 L1（内存）

在进程堆外预分配一块大内存，切成固定大小的块（block），用 slab 分配器管理，不走 Go GC。

**关键参数**：

| 参数 | 值 | 说明 |
|---|---|---|
| 块大小（block_size） | 64KB | 与后端逻辑块对齐；内部按 4KB sector 维护 valid/dirty 位图 |
| L1 总容量 | 2GB ~ 32GB | 建议不超过物理内存 50% |
| 分片数（shard） | 256 | 锁粒度；容量为 2 的幂，哈希用 `hash64(lba) & (cap-1)` |
| 高低水位 | high 90% / low 70% | 超过 high 开始淘汰，降到 low 停止 |

**为什么用 slab**：固定块大小 → 分配/释放 O(1) 无碎片；元数据与数据区分离 → CPU 缓存友好；空闲链表原子 pop/push。

**扇区级 valid/dirty 位图（必须，不可省略）**：

一个 64KB 块对应 16 个 4KB sector。块粒度缓存无法正确表达"4KB 写落在未缓存的 64KB 块"这类部分覆盖场景：

```c
#define SECTORS_PER_BLOCK  (BLOCK_SIZE / 4096)   // 16

struct sector_bits {
    uint16_t    valid;   // bit i = 1 表示该 4KB sector 内容有效
    uint16_t    dirty;   // bit i = 1 表示该 4KB sector 内容相对后端已修改
};

struct l1_block {
    uint64_t        lba;            // 该块覆盖的起始 LBA（block 对齐）
    uint32_t        refcount;       // 引用计数，供无锁读侧安全持有
    struct sector_bits sectors;     // 扇区级 valid/dirty
    uint64_t        access_time_ns;
    struct l1_block *lru_prev, *lru_next;
    struct l1_block *hash_next;
    uint8_t         data[BLOCK_SIZE];
};
```

**部分覆盖语义**（以 4KB 写 LBA 0x1001 落在未缓存的 64KB 块为例）：

```
1. 计算 block_lba = align_down(0x1001, SECTORS_PER_BLOCK)  → 0x1000
2. 若 block_lba 不在缓存中：
     a. 先从后端读整块 0x1000..0x100F（补齐 16 个 sector 的 baseline）
     b. 在 block->data 中覆盖 sector 1（LBA 0x1001）的新数据
     c. block->sectors.valid = 0x0002（只有刚写的 sector 有效）
     d. block->sectors.dirty = 0x0002
   若 block_lba 已在缓存但目标 sector 为无效：
     同上，先补齐 baseline 再覆盖
3. 若块已完整缓存（全部 valid=1）：直接覆盖目标 sector，置对应 dirty 位
```

**部分读语义**：读 LBA 0x1001 一个 sector 时，仅当 `valid & (1<<1)` 为 1 才算命中，否则视为 miss，走后端补齐。跨 block 读（如从块中间读到跨 2~4 个块）必须**按 sector 逐个判断 valid**，不得拼接未初始化数据。

**数据生命周期与回收**：

- 读侧通过 `refcount++` 持有块指针，读完 `refcount--`；淘汰线程只回收 `refcount == 0` 的块。
- 禁止在持锁期间 `memcpy` 大块后释放锁再使用——必须 refcount 持有。
- 淘汰前若 `dirty` 非全零，先把脏 sector 刷到后端（或写 Journal），刷完清零 dirty 位，方可回收。

### 4.2 L2（本地高速存储，O_DIRECT）

**核心原则：L2 文件只存数据，索引全程驻留内存。** 查找路径完全在用户态内存中完成，零系统调用、零 page fault。

**为什么不用 mmap**：
- mmap 是懒加载，首次访问触发 page fault 串行排队，造成毫秒级毛刺。
- `MAP_SHARED` 写回无法控制落盘顺序，SSD 层出错会以 `SIGBUS` 杀掉进程，无法降级为 L1-only。
- 改用 `O_DIRECT` 绕过 page cache，由 io_uring 提交，落盘顺序可控、错误可捕获。

**L2 索引（全驻留内存，开地址哈希）**：

```c
#define L2_INDEX_ENTRY_SIZE  32   // 不含动态成员

struct l2_index_entry {
    uint64_t    lba;            // 0 不作为空槽标记；见下方 sentinel 方案
    uint64_t    offset;         // 在 L2 数据区中的偏移
    uint32_t    data_len;       // 已缓存的字节数（允许 < BLOCK_SIZE）
    uint32_t    flags;          // bit0=dirty bit1=valid bit2=tombstone
    uint32_t    hit_count;
    uint32_t    checksum;       // CRC32
};

// 哈希：避免 LBA 局部性导致主簇化
static inline uint32_t lba_hash(uint64_t lba) {
    lba ^= lba >> 33;
    lba *= 0xff51afd7ed558ccdULL;
    lba ^= lba >> 33;
    return (uint32_t)(lba & 0xffffffffULL);
}

// 空槽标记：用 (lba == 0 && offset == 0 && data_len == 0) 三字段联合判定，
// 或单独引入 L2_ENTRY_EMPTY 哨兵（推荐，更清晰）。
// LBA 0 是真实数据区（MBR/GPT），必须允许存入。
```

**L2 数据区布局**：

```
L2 文件（O_DIRECT，块设备对齐）:
┌─────────────────────────────────────────────────┐
│  Super Block（magic/version/capacity/block_size）│
├─────────────────────────────────────────────────┤
│  数据区（BLOCK_SIZE 对齐的槽位，追加分配）        │
│  slot[i] = BLOCK_SIZE 字节，含实际数据 + padding │
└─────────────────────────────────────────────────┘
```

**分配策略**：追加式分配器（append-only），淘汰时把槽位加入 free list；定期（如容量 < 20% 时）做段压缩（segment compaction）回收空洞。**禁止**覆盖式复用槽位而不更新索引 generation——见 4.4 崩溃恢复。

**fallocate 用法**：预分配用 `fallocate(fd, 0, offset, len)`（mode=0，文件 size 随之增长）；不要用 `FALLOC_FL_KEEP_SIZE`，否则后续 `pwrite` 可能返回 `ENOSPC` 或触发 SIGBUS。若需固定上限，先 `ftruncate` 到目标大小再 mode=0 预分配全部 extent。

**mmap 的保留用途**：仅在**启动加载 Super Block** 时短期 mmap 一次，读完即 `munmap`，运行期不再持有映射。

### 4.3 L2 淘汰与 dirty 处理

L1 块被淘汰时：

```
1. 若块 dirty 非全零：
     a. 先把脏 sector 写入 L2 槽位
     b. 更新 L2 索引（置 dirty 位、写 Journal 记录"该 LBA 现在是脏的"）
     c. 确认 Journal 已 fsync 后，块在 L2 的副本才算权威脏副本
2. 若块 clean：直接丢弃（L2 中若有旧副本保留，valid 位仍有效）
```

**关键点**：L1 dirty 块降级到 L2 前，**必须**先把"该 LBA 是脏的"这一事实记入 Journal 并 fsync，然后才能把 L1 块的 dirty 位清零。否则崩溃后 L2 里有脏数据、Journal 里没有对应记录，重启会把它当 clean 数据返回，造成**静默数据损坏**。

### 4.4 L2 崩溃恢复

**两条独立日志**（职责不重叠）：

| 日志 | 内容 | 崩溃后处理 |
|---|---|---|
| **Metadata Journal** | L2 索引变更：分配/释放槽位、LBA→offset 映射、dirty 位变更 | 重建索引快照；可丢（丢失的是索引结构，数据本身未变） |
| **Data Journal** | **完整 data payload** + LBA + length + generation + checksum | 必须重放；承载 writeback 的持久性承诺 |

**恢复顺序（严格）**：

```
1. 校验 Super Block magic/version/checksum
2. 加载最近一次索引快照（若存在）
3. 扫描 Data Journal：
     a. 校验 entry checksum 与 generation
     b. 将 payload 写回 L2 槽位（或后端，见 7.4）
     c. 标记 L2 索引对应条目为 dirty
4. 扫描 Metadata Journal：
     a. 应用槽位分配/释放
     b. 对 generation 不匹配的条目视为 stale，置 tombstone
5. 校验所有 dirty 条目的 CRC32：
     a. 失败 → 该 LBA 标记为无效（宁可冷启动，不返回错误数据）
6. 回收 generation 不一致的陈旧槽位
7. 恢复完成，开放服务
```

**stale 条目问题**：索引快照每 10s 写一次，期间槽位会被淘汰后复用。旧快照里一个 clean 条目可能指向已被复用的槽位。用 `(lba, generation)` 联合判断：槽位 header 存 generation，索引条目存写入时的 generation，不匹配即 stale。

**Journal 截断**：Data Journal 只能在"该 entry 覆盖的所有 sector 都已刷回后端"之后截断。截断本身必须是**原子操作**——在 Journal 头部写一个 `truncated_seq` 字段并 `fdatasync`；重启时从 `truncated_seq` 之后的位置开始扫描。禁止用 `ftruncate` 直接截零（截断一半崩溃会导致 Journal 文件损坏、无法定位有效起点）。

### 4.5 L1 与 L2 分工

```
读请求：
  L1 命中（sector valid）→ 返回（μs 级）
  L1 miss / L2 命中     → memcpy → 异步晋升 L1，返回
  全 miss                → 后端读 → 填 L2 → 填 L1 → 返回

写请求（writeback）：
  Journal payload 落盘 + fsync
  → 更新 L1 dirty sector 位图
  → 立即回 GOOD
  → 后台刷后端，刷完清 dirty 位
```

---

## 五、缓存污染防御

### 5.1 扫描检测（连续性判断 + per-stream tracker）

**旧方案的错误**：仅判断 `lba > last_lba`（方向），一串递增的随机请求也会被判成扫描；多流交错（QD32、多个 VM）时完全失效。

**修正**：按连续性判断，并对每个并发流单独追踪。

```c
#define STREAM_MAX_QDEPTH  64

struct io_stream {
    uint64_t    expected_next_lba;   // 期望的下一个 LBA
    uint32_t    contiguous_hits;     // 连续命中次数
    bool        scanning;            // 是否处于扫描模式
};

struct scan_detector {
    struct io_stream streams[STREAM_MAX_QDEPTH];
    uint32_t        threshold;       // 默认 32
};

// 判断逻辑：本次请求的起点 == 上次请求的终点，才算"连续"
void on_read(struct scan_detector *sd, uint64_t lba, uint32_t len_sectors) {
    struct io_stream *s = find_or_create_stream(sd, lba);
    if (lba == s->expected_next_lba) {
        s->contiguous_hits++;
    } else {
        s->contiguous_hits = 0;
        s->scanning = false;
    }
    s->expected_next_lba = lba + len_sectors;
    if (s->contiguous_hits >= sd->threshold) {
        s->scanning = true;
    }
}
```

**旁路路径必须先查脏**：扫描旁路让读直接走后端，但若 L1 里有该 LBA 的 **dirty 块**，后端数据已是旧版本，直接读后端会**读到过期数据**。因此旁路前必须：

```
若 L1/L2 中存在该 LBA 的 dirty 副本 → 不走旁路，走正常缓存读（读到新值）
否则                              → 旁路读后端
```

写旁路同理：若缓存中存在目标范围的脏块，必须**先失效/刷盘**再让后端写入，否则后续刷脏会用旧值覆盖后端新值。

### 5.2 准入门槛（单一算法）

采用 S3-FIFO 思路（**只选一个算法，不堆叠**）：

| 访问次数 | 行为 |
|---|---|
| 第一次 | 进 Small Queue（小对象试用区） |
| 第二次 | 晋升到 Main Queue |
| 第三次及以后 | 提升频率计数 |

Small Queue 满时淘汰未再次访问的块；Main Queue 淘汰频率最低的块。一次扫描的块几乎不会在 Small Queue 中被再次访问，自然被淘汰。

**不叠加 ARC/SLRU/LIRS/保护区**：S3-FIFO 本身具备抗扫描能力，叠加多个算法只会让命中率互相抵消、调参面爆炸，且行为难以测试。

---

## 六、预取引擎

全部基于纯 IO 行为驱动，零业务入侵。

| 预取机制 | 触发条件 | 行为 |
|---|---|---|
| 顺序检测 + 预读 | 连续请求超过阈值 | 预取后续 LBA 到 L1，深度自适应 |
| 步幅检测 | 差值序列稳定 | 按步幅预取 |
| 读合并 | 并发到达的相邻读请求 | 合并为一次后端读，结果分发 |
| 突发检测 + 爆发式预取 | 10ms 内请求数超阈值 | 扩大窗口与深度 |
| 空间局部性放大 | 请求落在同一窗口 | 窗口整块预取 |

**约束**：
- **投机性大读（多读 2x）必须移除**——它绕过了污染防御，会把大量无关数据塞进缓存。仅在确认顺序模式后再放大。
- 预取读优先级低于真实请求（P2/P3），不得挤占前台 IO。
- 预取不得污染主缓存：可进 Small Queue，或进独立预取缓冲区（命中后才晋升 Main Queue）。

---

## 七、写策略与写一致性

### 7.1 三种写模式

| 模式 | 语义 | 性能 | 掉电风险 | 适用场景 |
|---|---|---|---|---|
| writethrough | 写后端确认后才回 GOOD；**写不进 L1/L2** | 仅读加速 | 极低 | 起步推荐 |
| writeback | 先写 Journal + L1 dirty，立即回 GOOD，后台刷后端 | 读写均加速 | 中高 | 有 UPS + 掉电保护 SSD |
| writearound | 写绕过缓存直落后端 | 仅读加速 | 无 | 防止大文件污染 |

**writethrough 下写不进缓存**：避免缓存中的脏数据与后端不一致，也避免大文件写污染。

### 7.2 FUA 与持久化语义

- `WRITE(10/16)` 带 FUA=1，或 `SYNCHRONIZE CACHE`：
  - **必须同步**把目标范围的所有 dirty sector 刷到后端；
  - **必须等待 Journal 的 fsync 完成**；
  - 两者都完成后才能回 GOOD。
- FUA 写可以进 L1 dirty，但回 GOOD 前必须已持久化。
- 不得把 FUA 写放进异步合并窗口。

### 7.3 写合并的顺序语义

合并窗口内的写按 **LBA 排序**以减少后端 IO 次数，但**合并不改变写入顺序**——按 Initiator 发出的先后顺序提交，不能因排序让上层依赖顺序的日志（如 WAL 的 LSN）乱序落盘。实现方式：每个写带 `seqno`，排序仅用于合并，提交严格按 `seqno` 顺序。

### 7.4 写与读的并发顺序（不得静默乱序）

Initiator 写 LBA X 后立即读 LBA X，必须读到新值。处理规则：

- 读命令到达时，若目标 LBA 有**未完成写**，读必须等待该写完成（或读该写缓冲中的数据）后再返回。
- 若用乐观路径（读缓存 dirty 块），必须保证 dirty 块在被刷盘/失效前不会被回收。
- **超时重传场景**：Initiator 超时后重发同一条 READ，缓存必须返回**同一时刻的数据**——即必须能识别"同一条命令"并去重，不能让重传读到中途被覆盖的状态。建议用 `(ITNexus, CmdSN)` 做请求去重表，未完成请求重传直接挂到原请求上。

### 7.5 Writeback 持久化流程

```
1. 分配 Data Journal entry（含完整 payload + LBA + length + generation + checksum）
2. pwrite 到 Data Journal，fdatasync
3. 更新 L1 sector dirty 位 / 分配 L2 槽位并更新索引
4. 回 GOOD（此后该数据被视为已持久化）
5. 后台：按 seqno 顺序刷后端 → 刷完清零 dirty 位 → 更新 Metadata Journal → 推进 Data Journal truncated_seq
```

**generation 的作用**：同一 LBA 多次写会有多个 Journal entry，generation 单调递增。崩溃恢复时只应用 generation 最大的那条，旧 entry 视为已覆盖、直接丢弃。

---

## 八、iSCSI 协议级加速与可靠性

| 机制 | 做法 | 收益 |
|---|---|---|
| 调大 MaxBurstLength / MaxRecvDataSegmentLength | 减少 PDU 分段 | 减少协议开销 |
| ImmediateData=Yes | 小数据塞进 SCSI Command PDU | 省一次 Data-Out 交换 |
| io_uring 提交 L2 IO | 批量提交/完成，减少系统调用 | 高队列深度下延迟降低 |
| 命令队列化 | per-LUN 串行化依赖命令，无依赖命令并行 | 提升并发 |

**已移除的机制**：
- **MC/S**：open-iscsi 与 ESXi 均不支持多连接单会话，仅 Windows 支持，性价比低。
- **DPDK / RIO**：仅在网络栈被实测为瓶颈后考虑，初期用标准 socket。
- **madvise(MADV_DONTFORK)**：已不使用 mmap，删除。

---

## 九、请求调度

### 9.1 按命令类型分级

| 优先级 | 命令类型 |
|---|---|
| P0 | FUA WRITE、SYNCHRONIZE CACHE |
| P1 | 真实读（延迟敏感） |
| P2 | 顺序读 |
| P3 | 预取读、后台刷写 |

### 9.2 按 Initiator 分级

不同 IQN 可配优先级和配额，例如 ESXi 的 VMFS 数据 LUN 优先级高于备份 LUN。

### 9.3 压缩与去重

不做。L2 的压缩会引入 CPU 开销与数据损坏难以定位的问题，去重则破坏 per-sector 的 dirty 语义（多个 LBA 共享一块时无法独立刷脏）。

---

## 十、下游代理机制（怎么连后端）

### 10.1 后端连接模型

缓存代理在后端是 **Initiator**——主动连接真实 iSCSI Target，把 cache miss 的 IO 转发过去。

### 10.2 libiscsi 单线程事件循环

**libiscsi 的 `iscsi_context` 不是线程安全的**，所有调用必须在同一线程上完成。必须为每个 LUN（或每个后端连接）分配**独立事件循环线程**，串行调用 libiscsi API：

```c
// 每 LUN 一个事件循环线程
void* backend_event_loop(void *arg) {
    struct backend_ctx *be = arg;
    while (!be->stopped) {
        struct pollfd pfd = { .fd = iscsi_get_fd(be->iscsi),
                              .events = iscsi_which_events(be->iscsi) };
        poll(&pfd, 1, 100);
        iscsi_service(be->iscsi, pfd.revents);  // 串行调用
    }
    return NULL;
}
```

- 命令提交：主线程把请求推入 `be->submit_queue`，事件循环线程取出并调用 `iscsi_read16` / `iscsi_write16`。
- 回调：`iscsi_service` 触发完成回调，回调里填充 cache 并唤醒完成器，**回调仍在事件循环线程上执行**，不得跨线程直接操作 Go 对象。
- 跨线程唤醒 Go 侧：用 `runtime.LockOSThread` + `//export` 回调，或 eventfd 通知 Go runtime。

### 10.3 异步后端 IO 模型

```c
struct io_request {
    uint64_t            lun_id;
    uint64_t            lba;
    uint32_t            len;
    void               *buf;
    uint32_t            seqno;          // 顺序语义
    enum io_status      status;
    uint64_t            submitted_at_ns;
    void               *private_data;  // 回调上下文
    void (*on_complete)(struct io_request *);
};

// 回调签名必须匹配 libiscsi：
//   void cb(struct iscsi_context *iscsi, int status,
//           void *data, size_t data_len, void *private_data)
// 因此 private_data 必须指向 io_request，而不是 buf
void backend_read_cb(struct iscsi_context *iscsi, int status,
                    void *data, size_t data_len, void *private_data) {
    struct io_request *req = private_data;   // 正确：指向 request
    req->status = (status == 0) ? IO_OK : IO_ERR;
    req->on_complete(req);
}
```

**注意**：libiscsi 回调的 `data` 指针指向其内部缓冲区，可能在下次 `iscsi_service` 时被覆盖，必须立即 `memcpy` 到 `req->buf`。

### 10.4 前端到后端的命令映射

| 前端命令 | 后端行为 |
|---|---|
| READ Miss | 异步 backend_read，完成后填缓存，返回 Initiator |
| WRITE (writeback) | 写 Journal + L1 dirty → 立即回 GOOD；后台刷后端 |
| WRITE (writethrough) | 同步 backend_write，确认后回 GOOD |
| SYNCHRONIZE CACHE | 刷该 LUN 所有 dirty sector，完成后回 GOOD |
| INQUIRY / REPORT LUNS / READ CAPACITY | 本地处理，不转发 |
| MODE SENSE/SELECT | 强制透传 |
| PERSISTENT RESERVE IN/OUT | 透传，每次查后端 |
| COMPARE AND WRITE | 透传，原子保证由后端提供 |

---

## 十一、整个系统怎么跑起来

### 11.1 部署架构

```
┌────────────────────────────────────────────────────┐
│                     服务器                          │
│                                                    │
│  ┌──────────────┐                    ┌──────────┐  │
│  │ iSCSI-Cache  │                    │ 后端     │  │
│  │              │ ─── iSCSI ──────→ │ Target   │  │
│  │ 监听 :3260   │                    │ (HDD)    │  │
│  │ L1: 内存     │                    │          │  │
│  │ L2: /dev/nvme│                    │          │  │
│  └──────┬───────┘                    └──────────┘  │
│         │                                           │
│         │ iSCSI :3260                               │
│         ▼                                           │
│  Windows iSCSI Initiator / ESXi / Linux open-iscsi  │
└────────────────────────────────────────────────────┘
```

**物理部署要点**：
- 代理与后端 Target 在同网段（延迟低）。
- L2 必须用**本地块设备**（`/dev/nvme0n1p1` 等），禁止文件落在网络盘或 HDD。
- Journal 与 L2 **物理分盘**，避免互相抢带宽与影响 fsync 延迟。
- writeback 模式必须用带掉电保护的 DC SSD + UPS。

### 11.2 构建步骤

```bash
# 1. 拉代码
git clone <repo> iscsi-cache && cd iscsi-cache

# 2. 构建 C 核心
cd ccore
make   # 依赖：gcc/clang + pthread + libiscsi-dev

# 3. 构建 Go 前端
cd ../cmd/proxy
export CGO_ENABLED=1
export CGO_CFLAGS="-I../../ccore/include"
export CGO_LDFLAGS="-L../../ccore -lcache -liscsi -lpthread -luring"
go build -o iscsi-cache-proxy

# 4. 准备 L2 块设备（首次）
#    L2 建议用裸分区，避免文件系统元数据开销
sudo mkfs.ext4 -F /dev/nvme0n1p1   # 或直接用裸设备
```

### 11.3 运行步骤

```bash
sudo ./iscsi-cache-proxy --config config.yaml
# 日志：
# [INFO] iSCSI-Cache v2.0 starting (Linux only)
# [INFO] L1 pool: 4GB (65536 blocks x 64KB)
# [INFO] L2 device: /dev/nvme0n1p1 (100GB)
# [INFO] Journal: /dev/nvme0n2p1
# [INFO] Connecting backend: 192.168.1.100:3260
# [INFO] Backend login OK, LUN capacity: 8TB
# [INFO] Listening on 0.0.0.0:3260
# [INFO] Ready
```

### 11.4 Initiator 侧连接

**Windows**：iSCSI 发起程序 → 目标填代理 IP:3260 → 快速连接 → 磁盘管理初始化。

**Linux（open-iscsi）**：

```bash
iscsiadm -m discovery -t st -p <cache-server-ip>
iscsiadm -m node -T iqn.2026-01.com.iscsi-cache:lun1 -p <cache-server-ip>:3260 --login
lsblk   # 看到代理导出的盘
```

**ESXi**：存储 → 软件 iSCSI → 动态发现填代理 IP:3260 → 重新扫描 → 新建数据存储。

### 11.5 启动顺序

```
1. 后端 iSCSI Target 先启动并确保可达
2. 启动 iSCSI-Cache（连后端、探活 LUN 容量）
3. Initiator 连接代理
代理启动时后端不可达：每 5s 重试后端，前端 :3260 保持监听，恢复后自动重连。
```

### 11.6 监控

```
iscsi_cache_l1_hits_total{lun="lun1"}
iscsi_cache_l2_hits_total{lun="lun1"}
iscsi_cache_backend_reads_total
iscsi_cache_dirty_sectors         # 未刷后端的脏 sector 数
iscsi_cache_journal_pending       # 未截断的 Journal entry 数
iscsi_cache_scan_bypassed_total
iscsi_cache_reject_total{reason=...}   # 协议拒绝计数
```

### 11.7 优雅关闭

```
SIGTERM / SIGINT
  1. 停止接受新连接
  2. 等待在途 IO 完成（最多 30s）
  3. 刷所有 dirty sector 到后端
  4. fsync Data Journal + 推进 truncated_seq
  5. 写 Metadata Journal 快照
  6. 释放 L1/L2 资源
  7. 断开后端连接
  8. 退出（exit 0）
```

---

## 十二、配置示例

```yaml
iscsi:
  listen_addr: "0.0.0.0:3260"
  target_iqn: "iqn.2026-01.com.iscsi-cache:lun1"
  max_sessions: 64
  max_connections_per_session: 4

backend:
  target_addr: "192.168.1.100:3260"
  target_iqn: "iqn.2026-01.nas:hdd1"
  auth:
    user: ""
    password: ""
  max_in_flight: 32
  event_loop: "per_lun"     # 每 LUN 独立线程

cache:
  mode: "writeback"          # writethrough | writeback | writearound

  l1:
    size: "4GB"
    block_size: "64KB"
    sector_size: "4KB"       # 扇区位图粒度，必须等于后端逻辑块大小
    shard_count: 256         # 2 的幂
    high_watermark: 0.9
    low_watermark: 0.7

  l2:
    path: "/dev/nvme0n1p1"
    size: "100GB"
    journal_path: "/dev/nvme0n2p1"
    o_direct: true
    io_engine: "io_uring"    # io_uring | libaio

  eviction:
    algorithm: "s3fifo"

  session:
    default_l1_quota: "1GB"
    default_l2_quota: "10GB"
    per_session_quota: true   # 只限配额与统计，缓存按 LUN 共享

  scan_detection:
    enabled: true
    threshold: 32
    max_streams: 64

  prefetch:
    enabled: true
    sequential_threshold: 4
    depth: 8
    speculative_read: false   # 禁止无条件多读 2x

  write:
    merge_window_us: 2000
    sort_before_flush: true
    fua_must_sync: true
    sequential_bypass: false  # 旁路路径必须先查脏

metrics:
  enabled: true
  listen_addr: "127.0.0.1:9090"

logging:
  level: "info"
  format: "json"
```

---

## 十三、性能预期

**以下为待实测项，非承诺值。** 必须在目标负载上实测后回填真实数据。

| 指标 | 实测方法 |
|---|---|
| L1 命中读延迟 | `fio --ioengine=io_uring --bs=4k --rw=randread --norandommap`，统计 p50/p99 |
| L2 命中读延迟 | 同上，预热后测量 |
| 后端 miss 读延迟 | 直通后端基准，减去网络 RTT 得缓存增益 |
| 写延迟（writeback） | 写 Journal 的 `fdatasync` p99 |
| 命中率 | `l1_hits / (l1_hits + l2_hits + backend_reads)` 分负载统计 |
| 扫描旁路命中率 | `scan_bypassed_total / total_reads` |

**基线对比实验**：用相同后端、相同负载，分别跑 LIO + bcache 与 SPDK + OCF，与本系统对比命中率与 p99 延迟，确认自研收益后再投入开发。

---

## 十四、模块接口契约

### 14.1 缓存引擎对外 API

```c
/* 错误码 */
typedef enum {
    CACHE_OK = 0,
    CACHE_PENDING,        /* 异步，结果稍后通过 callback 返回 */
    CACHE_ERR_NO_MEMORY,
    CACHE_ERR_INVALID,    /* LBA 越界、长度非法 */
    CACHE_ERR_IO,         /* 后端 IO 失败 */
    CACHE_ERR_JOURNAL,    /* Journal 写失败 */
    CACHE_ERR_REJECT,     /* 协议层拒绝（如任务管理） */
} cache_status_t;

/* 读请求：若命中立即填 buf 返回 OK；若 miss 返回 PENDING，完成时调 cb */
cache_status_t cache_submit_read(
    uint64_t lun_id,
    uint64_t lba,
    uint32_t len,
    void *buf,
    uint64_t cmd_identity,    /* (ITNexus, CmdSN) 去重用 */
    void (*cb)(cache_status_t, void *buf, uint32_t len, void *arg),
    void *cb_arg);

/* 写请求：返回 PENDING（异步刷盘）或 OK（writethrough 同步完成） */
cache_status_t cache_submit_write(
    uint64_t lun_id,
    uint64_t lba,
    uint32_t len,
    const void *buf,
    bool fua,
    uint32_t seqno,
    void (*cb)(cache_status_t, void *arg),
    void *cb_arg);

/* 刷脏：刷 lun_id 上 [lba, lba+len) 范围内所有 dirty sector */
cache_status_t cache_flush(uint64_t lun_id, uint64_t lba, uint32_t len,
                           void (*cb)(cache_status_t, void *arg), void *arg);

/* 失效：UNMAP / WRITE SAME 后清缓存范围 */
cache_status_t cache_invalidate(uint64_t lun_id, uint64_t lba, uint32_t len);

/* 启动 / 关闭 */
cache_status_t cache_start(const struct cache_config *cfg);
cache_status_t cache_shutdown(int flush_dirty, int write_snapshot);
```

### 14.2 iSCSI 前端与缓存引擎的约定

- 前端不得持有 `l1_block *` 跨 CGO 调用；所有数据通过 `buf` 拷贝传递。
- 后端 IO 未完成时，`cache_submit_*` **不得**阻塞等待。
- 完成回调可能在任意线程触发；前端必须自行做线程安全（如使用 Go channel 或锁）。

### 14.3 后端与缓存引擎的约定

- 后端完成回调**只在事件循环线程**上触发。
- `buf` 生命周期：回调返回前有效，之后 libiscsi 可能复用，必须立即 memcpy。
- 后端错误（连接断开、IO 超时）必须透传给前端，转为对应 SCSI Status / Sense，不得静默重试到旧数据。

### 14.4 错误码到 SCSI 状态的映射

| cache_status | SCSI Status | Sense Key | ASC/ASCQ |
|---|---|---|---|
| OK | GOOD | — | — |
| PENDING | —（未完成） | — | — |
| ERR_INVALID | CHECK CONDITION | ILLEGAL REQUEST | 0x21/0x00 |
| ERR_IO | CHECK CONDITION | MEDIUM ERROR | 0x11/0x00 |
| ERR_JOURNAL | CHECK CONDITION | ABORTED COMMAND | 0x47/0x00 |
| ERR_REJECT | 对应 Task Management Response | — | — |

---

## 十五、iSCSI 协议完备性测试方案

### 15.1 测试分层

| 层 | 内容 |
|---|---|
| L0 | 环境拉起与依赖检查 |
| L1 | 一致性（参照式校验） |
| L2 | 协议对照（与真实 Target 比对） |
| L3 | 异常路径与故障注入 |
| L4 | 互操作性（真实 Initiator） |
| L5 | 模糊测试 |
| L6 | 会话并发与压力 |
| L7 | 混沌工程与端到端校验 |

### 15.2 L1 一致性：参照式校验

**SHA256 全量比对只在"初始状态未变"时有意义**。写入后数据本就不同，必须改为**参照式校验**：

```
1. 生成伪随机黄金数据（固定 seed，可复现），写入后端 LUN 并记录写入日志
2. 每个写请求：收到 GOOD 后，把 (LBA, length, data_sha256) 追加到 append-only 写入日志
3. 校验时：
     a. 对每个已确认写入的范围，按日志读出，比对 sha256
     b. 对未确认写入的范围（超时未回 GOOD），读到新值或旧值都合法
4. 全量校验：把 LUN 按 4KB 切块，每块按写入日志判定预期值，逐块比对
```

**必须在每种缓存状态后重跑**：cold（空缓存）、warm_l1、warm_l2、dirty_writeback。

### 15.3 L2 协议对照测试架

```
Initiator ──同一条命令──▶ 分支器 ─┬─→ 真实 Target (tgt/LIO)
                                 └─→ iSCSI-Cache 代理
                                      比对 Data / Status / Sense / Residual
```

**SCSI 命令矩阵**（强制覆盖）：

| 命令 | 正常路径 | 异常路径 | 边界 |
|---|---|---|---|
| INQUIRY | Standard / VPD | 非法 EVPD 页 | Serial Number 唯一性 |
| REPORT LUNS | 单/多 LUN | — | LUN 类型字段、寻址格式 |
| READ CAPACITY(10/16) | 返回容量 | 返回 32 位截断 | 4TB+ 边界 |
| SERVICE ACTION IN / READ CAPACITY(16) | 64 位 LBA + BlockDescriptor | — | 逻辑块长度字段 |
| READ(10/16) | 各种长度 | LBA 越界 | 保护信息检查 |
| WRITE(10/16) | 各种长度 | FUA=1 | 保护信息检查 |
| MODE SENSE/SELECT | 透传返回 | 非法页码 | 块参数一致性 |
| SYNCHRONIZE CACHE | 全范围/部分 | — | 部分范围正确性 |
| UNMAP | 单段/多段 | 对齐边界 | 已释放 LBA 读必须返回零 |
| WRITE SAME | 不同边界 | — | — |
| PERSISTENT RESERVE IN/OUT | REGISTER/RESERVE/RELEASE | 冲突抢占 | 多 Initiator 抢占 |
| COMPARE AND WRITE | 原子 CAS | 失败回退 | 字节级比对 |
| SECURITY PROTOCOL IN/OUT | SED 密钥交换 | — | 透传、不缓存 |
| TEST UNIT READY | 在线/离线 | 后端不可达 | UA 上报 |
| REQUEST SENSE | 清 UA | 无异常挂起 | — |
| TASK MANAGEMENT | ABORT TASK / LUN RESET / CLEAR TASK SET | 在飞 IO 处理 | 超时后重传 |

### 15.4 L3 异常路径与故障注入

| 故障类型 | 注入方式 |
|---|---|
| 网络丢包/延迟/乱序 | `tc qdisc add netem` |
| 连接半开 | iptables 丢探测包 |
| 后端进程崩溃 | `kill -9` |
| 代理进程崩溃 | `kill -9` |
| L2 盘满 | `dd` 填满 |
| SSD 拔出 | `echo 1 > /sys/block/.../device/delete` |
| 内存压力 | `stress --vm` |
| CPU 饱和 | `stress --cpu` |
| 畸形 PDU | Boofuzz / 自定义 |

### 15.5 L4 互操作性

| Initiator | 验证项 |
|---|---|
| Linux open-iscsi | 发现/登录/登出、多 session、multipath |
| Windows iSCSI Initiator | 发现/连接/断开、MPIO、磁盘签名 |
| ESXi | VMFS 创建、虚拟机运行、存储 vMotion、PR 冲突 |

### 15.6 L5 模糊测试

- **状态化 fuzzing**：Boofuzz 维护 iSCSI Login 状态机，变异后续 PDU。
- **覆盖率引导**：AFL++ 或 libFuzzer 针对 PDU 解析函数。
- **磁盘变异**：对已保存的合法 PDU 语料做位翻转、长度变异、opcode 替换。
- 72 小时无崩溃为达标。

### 15.7 L6 会话并发与压力

- 单 session 多连接（若支持）、多 session 共享同一 LUN（**验证缓存共享无脏读**）。
- 队列深度 1~256，混合读写比例 100/0 ~ 0/100。
- 长稳测试 24 小时，检查泄漏（ASAN / valgrind / fd 计数）。

### 15.8 L7 混沌与掉电

**`kill -9` 只模拟进程崩溃，page cache 仍在，不能代表掉电**。掉电必须用：

- `dm-log-writes`：记录每次写顺序，可在任意点回放，精确复现掉电瞬间状态。
- 虚拟机方案：把 L2/Journal 放在 qcow2，测试中途 `virsh destroy` 强制断电。

**混沌循环**（连续 1000 轮，零数据不一致为达标）：

```
每轮：
  1. 选定故障类型与注入时机（负载 30%~70% 时段）
  2. 注入故障，持续 5~60s
  3. 恢复故障，等待自愈
  4. 校验：参照式 sha256 一致 + 进程存活 + 无断言日志 + dirty 归零
  5. 失败立即 dump core / PDU trace / journal / metrics
```

### 15.9 发布闸门（任一不满足即阻断）

```
✗ 参照式一致性校验不一致
✗ 混沌测试出现数据不一致
✗ Journal replay 非 100% 成功
✗ 模糊测试出现崩溃
✗ 内存泄漏（ASAN / valgrind）
✗ 文件描述符泄漏
✗ 多 Session 共享 LUN 出现脏读
✗ 掉电恢复后返回旧数据（dm-log-writes 验证）
✗ 性能 p99 回退 > 10%（相对基线）
✗ 互操作全流程未通过
```

---

## 十六、风险与注意事项

- **缓存不是备份**。writeback 模式下 SSD 故障或掉电可能丢脏块，重要数据层必须有快照或异地副本。
- **消费级 SSD 的 DRAM 缓冲**：可能回复写完成但数据仍在自身 DRAM，断电即丢。writeback 需 DC SSD（带掉电保护 PLPEL）或 UPS。
- **跨平台不成立**：`O_DIRECT`、`io_uring`、`fallocate(2)`、`madvise(2)`、`pthread_rwlock` 在 Windows 上不存在，`libiscsi` 的 Windows 支持不完整。已明确仅支持 Linux。
- **按 LUN 共享缓存**：这是多 Session / MPIO 场景不出脏读的硬性要求，不得以"隔离"为由改回按 Session 分缓存。
- **per-sector valid/dirty 位图不可省略**：64KB 块粒度无法正确处理 4KB 部分覆盖。
- **旁路路径必须先查脏**：否则读到过期后端数据，或让旧脏块覆盖新后端数据。
- **Journal 必须有完整 payload**：metadata-only 的 Journal 无法在崩溃后重放写入内容。
- **Journal 截断必须原子**：`truncated_seq` + `fdatasync`，禁止直接 `ftruncate`。
- **FUA 写必须同步持久化**：不得异步处理。
- **写合并不改变提交顺序**：必须按 Initiator 发出的 `seqno` 顺序刷盘。
- **LBA 0 是真实数据区**：索引空槽标记不得用 `lba == 0`。
- **序列号防重复**：Data Journal entry 带 generation，崩溃恢复只应用最大 generation。
- **L2 必须用本地块设备**：文件落在网络盘或 HDD 上会让缓存比后端还慢，且 fsync 语义不可靠。
- **MC/S 支持度低**：open-iscsi 与 ESXi 不支持，已移除该优化项。
- **压缩/去重已禁用**：破坏 per-sector dirty 语义且引入定位困难的数据损坏风险。
- **`kill -9` ≠ 掉电**：掉电测试必须用 `dm-log-writes` 或虚拟机强制断电。
