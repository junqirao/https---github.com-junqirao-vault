# iSCSI-Cache 多级缓存代理系统 · 设计与测试方案

# 第一部分 · 系统设计

## 一、项目定位

**iSCSI-Cache** 是一个工作在 iSCSI 协议层的通用多级缓存代理系统。它不依赖任何操作系统的存储栈（不依赖 bcache、Storage Spaces、ZFS 等），而是以用户态应用的形式，在 iSCSI 协议数据路径中插入缓存逻辑。

```
┌──────────────┐   iSCSI    ┌──────────────────┐   iSCSI    ┌──────────────┐
│  Initiator   │ ─────────→ │  iSCSI-Cache     │ ─────────→ │  Backend     │
│  (ESXi/Win/  │  标准协议   │  (本系统)         │  标准协议   │  Target      │
│   Linux)     │            │  前端Target+缓存  │            │  (HDD/任何)  │
└──────────────┘            │  后端Initiator   │            └──────────────┘
                            └──────────────────┘
```

**核心特征：**

| 维度 | 说明 |
|---|---|
| 协议位置 | iSCSI 协议层（SCSI 命令级），非文件系统、非块设备驱动 |
| 平台 | Linux 为主实现（`O_DIRECT` / io_uring）；Windows 适配见第三部分（`FILE_FLAG_NO_BUFFERING` + IOCP）。缓存逻辑层（命中判定、扇区位图、S3-FIFO、Journal 语义）跨平台共用 |
| 前端角色 | 伪装为 iSCSI Target，接受 Initiator 连接 |
| 后端角色 | 作为 iSCSI Initiator，连接真实存储 Target |
| 缓存层级 | L1（内存）+ L2（本地高速存储） |
| 隔离粒度 | 缓存按 LUN 共享，配额与统计按 Session 计算 |

## 二、系统架构

```
┌────────────────────────────────────────────────────────────────────┐
│                         iSCSI-Cache                               │
│                                                                    │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │              Go 层：协议前端 + 编排                          │   │
│  │                                                             │   │
│  │  ┌─────────────┐  ┌──────────────┐  ┌──────────────────┐   │   │
│  │  │ TCP Listener │  │ iSCSI Target │  │  Session Manager │   │   │
│  │  │ (3260)      │→│ State Machine │→│  (会话/连接管理)  │   │   │
│  │  └─────────────┘  └──────────────┘  └──────────────────┘   │   │
│  │         │                  │                    │             │   │
│  │  ┌─────────────┐  ┌──────────────────────────────────────┐  │   │
│  │  │ SCSI Cmd    │  │  Command Dispatcher                 │  │   │
│  │  │ Parser      │  │  读命令→查缓存 写命令→写缓存+回GOOD  │  │   │
│  │  └─────────────┘  └──────────────────┬───────────────────┘  │   │
│  │                                     │                       │   │
│  │  ┌──────────────────────────────────▼───────────────────┐  │   │
│  │  │           CGO 接口（异步回调，不阻塞 Go 线程）         │  │   │
│  │  │  cache_dispatch(lba, len, buf, is_write, session_id,  │  │   │
│  │  │                  cb, cb_arg) → 立即返回              │  │   │
│  │  └──────────────────────────────────┬───────────────────┘  │   │
│  └─────────────────────────────────────┼─────────────────────┘   │
│                                        │                         │
│  ┌─────────────────────────────────────▼─────────────────────┐  │
│  │              C 层：缓存引擎（性能核心）                     │  │
│  │                                                             │  │
│  │  ┌──────────────────────────────────────────────────────┐   │  │
│  │  │           索引层 (Index)                             │   │  │
│  │  │  LUN → [Shard Hash Table] → Cache Entry             │   │  │
│  │  │  S3-FIFO 淘汰 + 分片锁 + 引用计数                    │   │  │
│  │  └──────────────────────────────────────────────────────┘   │  │
│  │         │                              │                     │  │
│  │  ┌──────▼──────┐              ┌───────▼──────┐            │  │
│  │  │ L1: 内存     │              │ L2: 本地存储  │            │  │
│  │  │ Slab分配     │              │ O_DIRECT+io_uring│           │  │
│  │  │ 按LUN共享    │              │ 范围索引(全内存)│            │  │
│  │  └──────┬──────┘              └───────┬──────┘            │  │
│  │         │                              │                     │  │
│  │  ┌──────▼──────────────────────────────▼──────┐            │  │
│  │  │   Metadata Journal (索引) + Data Journal (脏块)│            │  │
│  │  └─────────────────────────────────────────────┘            │  │
│  └─────────────────────────────────────────────────────────────┘  │
│                                        │                         │
│  ┌─────────────────────────────────────▼─────────────────────┐  │
│  │  后端：libiscsi（每 LUN 独立事件循环线程 + iscsi_service）  │  │
│  │  后端 IO 全部异步，完成回调通知缓存层                       │  │
│  └─────────────────────────────────────────────────────────────┘  │
└────────────────────────────────────────────────────────────────────┘
```

## 三、iSCSI 协议处理模型

### 3.1 支持的 SCSI 命令

| 命令 | 缓存行为 | 说明 |
|---|---|---|
| `INQUIRY` | 本地处理 | 返回代理伪造的厂商信息；Serial Number 必须按 LUN 唯一 |
| `REPORT LUNS` | 本地处理 | 返回代理的 LUN 列表；含 LUN 类型字段与寻址格式 |
| `SERVICE ACTION IN / READ CAPACITY(16)` | 本地缓存 | 含 64 位 LBA + BlockDescriptor；4TB+ 必须走此命令 |
| `READ CAPACITY(10)` | 本地缓存 | 返回容量，2TB+ 场景下值会回卷，属协议固有 |
| `READ(10)` / `READ(16)` | 缓存读 | 查 L1→L2→后端；命中逐级返回；DataSN 顺序重组 |
| `WRITE(10)` / `WRITE(16)` | 缓存写 | 写 L1（dirty）；FUA=1 必须等 Journal fsync 后才回 GOOD |
| `MODE SENSE(6/10)` | **强制透传** | 不本地模拟，避免块大小/缓存模式/写保护位与后端不一致 |
| `MODE SELECT(6/10)` | 透传 | 参数校验交给后端 |
| `SYNCHRONIZE CACHE` | 刷脏 | 将指定范围 dirty 块刷到后端；FUA 范围同步刷盘 |
| `UNMAP` | 穿透 + 失效 | 同时清缓存对应范围，已释放 LBA 读必须返回零 |
| `WRITE SAME(10/16)` | 穿透 + 失效 | 同上 |
| `PERSISTENT RESERVE IN/OUT` | **语义重写** | 代理→后端为多对一 I_T nexus，需自建 PR 状态机，禁止直接透传 |
| `COMPARE AND WRITE` | 本地原子 CAS | ESXi ATS 锁；代理需对 LUN 加互斥锁串行执行 |
| `SECURITY PROTOCOL IN/OUT` | 透传 | SED 自加密盘密钥交换；不缓存，不伪造 |
| `TEST UNIT READY` | 本地处理 | 检查后端连通性，不可达时上报 UA |
| `REQUEST SENSE` | 清 UA | 返回挂起的 Sense 信息 |
| `TASK MANAGEMENT`（ABORT TASK / LUN RESET / CLEAR TASK SET） | 本地处理 | 需跟踪在飞后端 IO，超时重传必须去重 |

### 3.2 缓存归属：按 LUN 共享

**这是多 Session / MPIO / 共享存储场景不出脏读的硬性要求。**

- `lun_t` 是缓存的真正所有者：索引、L1/L2 容量、淘汰队列都挂在 LUN 上
- `session_t` 只持有：配额上限、当前用量、统计计数
- 同一 LUN 被多个 Session 访问时，看到的是**同一份缓存数据**
- 配额超限时，淘汰的是 LUN 级别的块，而非某个 Session 的块

### 3.3 iSCSI 会话与连接映射

```
Initiator → TCP Connect → iSCSI Login → Session 建立
                                    │
                    ┌───────────────┼───────────────┐
                    ▼               ▼               ▼
              Connection 1     Connection 2     Connection 3
                    │               │               │
                    └───────────────┼───────────────┘
                                    ▼
                          Session (IQN + ISID)
                                    │
                                    ▼
                          ┌────────────────────┐
                          │  lun_t (缓存所有者) │
                          │  ├── 索引 + L1/L2   │
                          │  └── 淘汰队列       │
                          └────────────────────┘
                                    │
                    ┌───────────────┼───────────────┐
                    ▼               ▼               ▼
              session_t        session_t        session_t
              (配额+统计)      (配额+统计)      (配额+统计)
```

### 3.4 DataSN 与 R2T 语义

- 单条 READ 响应跨多个 Data-In PDU 时，必须按 DataSN 升序重组后交付上层
- 单条 WRITE 的 R2T 阶段，DataSN 必须严格递增；乱序到达需在代理侧缓冲重组
- 命令完成（SCSI Response PDU）必须在所有 Data-In 已提交后发送

### 3.5 缓存命名空间与后端身份绑定

**缓存必须绑定到"后端 LUN 的身份"，而不只是 LBA。** 否则后端磁盘被替换、重装或复用（例如 Target IQN 不变、背后的卷已换）时，L1/L2 中属于旧盘的数据会被当作新盘的缓存命中返回——这是比脏读更隐蔽的静默数据损坏。

绑定键（`cache_namespace`）由以下字段派生，任一变化即视为"另一块盘"，旧命名空间的缓存整体作废：

| 字段 | 来源 | 作用 |
|---|---|---|
| 后端 Target IQN | 后端登录配置 | 区分不同 Target |
| LUN WWID / Serial | 后端 INQUIRY VPD 0x80 / 0x83 | **区分同一 Target 下被替换/复用的 LUN** |
| 容量（LBA 数） | 后端 READ CAPACITY(16) | 容量变化即视为换盘 |
| 逻辑块大小 | 后端 READ CAPACITY / MODE SENSE | 块语义变化即视为换盘 |

- L2 的 Super Block 记录该命名空间；每次启动探活重新读取后端身份，与之不一致即**整体丢弃 L2 缓存**（不做逐条校验），并重建索引。
- L1 是易失内存，进程重启即清空；同一进程内后端重连后若身份变化，需清空对应 LUN 的 L1/L2。
- 注意区分两个 Serial：前端返回给 Initiator 的 `Serial Number` 是**代理伪造**的（按 LUN 唯一），后端身份用的是**后端自己的** WWID/Serial，两者不可混用、不可互相推导。

## 四、缓存引擎设计

### 4.1 核心数据结构

```c
// ===== 缓存块（L1 内存 / L2 索引共用元数据）=====
struct cache_block {
    uint64_t        lba;            // 逻辑块地址（0 是合法数据 LBA，不可作空槽标记）
    uint32_t        block_size;     // 块大小（默认 64KB）
    uint16_t        valid_bitmap;   // 每 bit 代表一个 4KB sector 的 valid 状态
    uint16_t        dirty_bitmap;   // 每 bit 代表一个 4KB sector 的 dirty 状态
    uint32_t        refcount;       // 引用计数：读侧持有时阻止回收
    bool            pinned;         // IO 进行中锁定
    uint64_t        access_time_ns;
    uint32_t        hit_count;

    struct cache_block *lru_prev;   // S3-FIFO 队列（New/Mid/Old）前驱
    struct cache_block *lru_next;   // S3-FIFO 队列后继
    void               *data;       // L1 指向 slab；L2 为 NULL（索引元数据中不存数据）
};

// 开地址哈希索引：容量取 2 的幂，用 hash64 而非 lba % cap
static inline uint32_t lba_hash(uint64_t lba, uint32_t cap_mask) {
    lba = (lba ^ (lba >> 30)) * 0xbf58476d1ce4e5b9ULL;
    lba = (lba ^ (lba >> 27)) * 0x94d049bb133111ebULL;
    return (lba ^ (lba >> 31)) & cap_mask;
}

// 删除使用 tombstone 标记，不做回移（避免并发下的顺序错误）
#define TOMBSTONE_LBA  (~(uint64_t)0)   // 与合法 LBA 区分
```

**说明：LBA 0 是 MBR/GPT 所在位置，是真实会访问的数据，绝不能用作空槽标记。** 空槽用 `block_size == 0` 表示，删除用独立 tombstone 值标记。

### 4.2 L1 内存缓存

| 参数 | 建议值 | 说明 |
|---|---|---|
| 块大小 | 64KB | 平衡索引开销与空间放大 |
| sector 大小 | 4KB | 扇区级 valid/dirty 粒度 |
| 分片数 | 256 | 每片一把锁，降低竞争 |
| 哈希容量 | 容量的 2 倍（开地址） | 负载因子 ≤0.5，保证 O(1) |
| 高水位 | 90% | 达到即触发淘汰 |
| 低水位 | 70% | 淘汰到此水位停止 |
| 索引条目 | `sizeof(cache_block)` ≈ 56 字节 × 2 倍容量 | 1TB L2 ≈ 16.7M 块，索引约 1.9GB（需预先规划） |

### 4.3 扇区位图与部分覆盖语义

- **64KB 块 = 16 个 4KB sector**，bitmap 共 16 bit
- **首次写入 4KB 落到未缓存块（writeback 模式）**：必须先回后端读整块做 baseline，再改写对应 sector，其余 sector 标 valid；writethrough 按分配策略决定是否先读 baseline；writearound 直接落后端、不进缓存
- **部分命中读**：逐 sector 判断 valid，invalid 部分从后端补齐
- **跨块读**：按块边界切分，每块独立查索引与 valid 判定

### 4.4 S3-FIFO 淘汰算法

选择 S3-FIFO 的原因：单一算法，自带抗扫描能力，无需叠加 ARC/SLRU/LIRS/保护区。

```
三个队列：
  New      ：新块进入，容量占 20%
  Mid      ：New 中再次命中的块晋升到此
  Old      ：New 中未再次命中的块降级到此

淘汰顺序：
  1. 先淘汰 Old 中的未命中块
  2. Old 满则整体迁移到 Mid
  3. Mid 满则按频次淘汰

扫描防御：一次性顺序访问的块停在 New，第二次访问才进 Mid，
         连续顺序流几乎不会污染 Mid 区。
```

### 4.5 L2 本地高速存储

**L2 数据区使用 `O_DIRECT` + io_uring**，禁止 mmap：

- mmap 的缺页会引入毫秒级毛刺，且 `MAP_SHARED` 无法控制落盘顺序
- SSD IO 错误时 mmap 触发 SIGBUS 直接杀进程，与"SSD 掉盘降级为纯 L1"的目标冲突

```c
// L2 文件布局
// [Super Block][索引区（全内存常驻）][数据区（O_DIRECT 访问）]
//
// 索引：开地址哈希，全量 malloc 驻留内存，不用 mmap
// 数据：预分配，fallocate mode=0（会扩展文件大小，需监控 stat size）
// 启动时只 mmap Super Block（很小，无缺页风险）
```

**索引全驻留内存的代价**：1TB L2 对应 16.7M 个块，按 `sizeof(cache_block)`（约 56 字节）条目 × 2 倍容量估算，约需 1.9GB 索引内存，部署前必须规划 RSS。

### 4.6 扫描检测（旁路防污染）

```c
struct stream_tracker {
    uint64_t    last_end_lba;   // 上一次读到的末尾 LBA
    uint32_t    sequential_count;
    bool        is_scanning;
};

// 判定：本次起点 == 上次终点 → 连续
// 与"方向递增"判定不同：一串递增的随机请求不会被误判为扫描
// 每 Session 独立 tracker，支持 QD32 多流追踪
```

- 连续 32 个请求判定为扫描 → 旁路缓存，直接走后端
- 旁路**不是跳过缓存**：必须先查 dirty 索引，命中 dirty 块必须返回新值；写旁路同时让对应缓存条目失效
- 顺序读期间仍可做顺序预取加速（不污染缓存）

### 4.7 预取引擎

- **连续性判断**：`lba == last_end_lba` 才触发预取，不放大投机读
- **步幅检测**：记录最近 8-16 个 LBA 差值，稳定即判定 strided access
- **突发检测**：10ms 内 >50 个请求 → burst 模式，预取深度 ×8
- **投机性多读 2x**：默认关闭（与防污染目标冲突），仅在确认顺序模式后开启

## 五、写策略

### 5.1 writethrough / writeback / writearound

| 模式 | 行为 | 是否产生脏块 | 适用场景 |
|---|---|---|---|
| writethrough | 写入同时更新 L1/L2（仅更新已存在的条目或按分配策略决定）并同步落后端，后端确认后才回 GOOD | 否 | 数据安全优先 |
| writeback | 写只落 L1（dirty），立即回 GOOD，后台刷后端 | 是（依赖 Journal） | 性能优先 |
| writearound | 写完全绕过缓存直接落后端；若目标范围已有缓存条目则**使其失效** | 否 | 防止大文件污染 |

**只有 writeback 会产生脏块**，因此 Data Journal（见 5.4）只在 writeback 模式下启用；writethrough / writearound 的写路径不产生脏块，不需要 Journal。

**"写不污染缓存"是靠分配策略而非"禁止进缓存"实现的**：writethrough 更新缓存是为了让后续读命中新值，其写路径的污染由准入控制（见 4.6）约束；writearound 则是彻底绕过缓存，并在写成功后让目标范围已有条目失效（否则后续读会命中旧值）。

### 5.2 FUA 写

FUA=1 的写**不得合并、不得延迟**，按模式分别处理：

- **writeback**：先把写负载写入 Data Journal 并 fsync 完成，再更新 L1 dirty 位，然后回 GOOD（崩溃后可由 Journal 重放）。
- **writethrough**：写路径本就同步落后端，等后端回 GOOD 后即可回 GOOD（缓存与后端同时更新）。
- **writearound**：写直接落后端，等后端回 GOOD 后回 GOOD，并失效对应缓存条目。

### 5.3 写合并

> 写合并主要服务于 **writeback** 模式（把多次小写攒成一次后端写）；writethrough / writearound 亦可合并相邻同向写以减少后端往返，但合并后仍须保持提交顺序。

- 2ms 窗口内按 LBA 排序收集相邻写
- **合并不改变提交顺序**：按 Initiator 的 seqno 串行刷盘，保证 WAL 等依赖顺序的语义
- 去重：同一 LBA 多次覆盖，只保留最后一次
- 脏块降级到 L2 前必须先 fsync Data Journal（仅 writeback）

### 5.4 Journal 双日志设计

| 日志 | 内容 | 可丢？ | 截断时机 |
|---|---|---|---|
| Metadata Journal | 索引结构变更（插入/删除/迁移） | 可丢：丢则退回"快照 + 扫描 L2 数据区全量重建索引"，代价是启动变慢 | 快照完成后可清 |
| Data Journal | 脏块完整 payload + LBA + generation | **不可丢** | 脏块已刷回后端且无在飞写 |

**Data Journal 只在 writeback 模式下使用**：writethrough / writearound 不产生脏块，不写 Data Journal，崩溃恢复也没有需要重放的脏数据。

```c
// Data Journal Entry（含完整数据负载，可重放）
struct data_journal_entry {
    uint32_t    magic;          // 0xCACECACE
    uint32_t    version;
    uint64_t    lba;
    uint32_t    data_len;
    uint64_t    generation;     // 块版本号，用于识别过期 entry
    uint64_t    timestamp_ns;
    uint32_t    session_id;
    uint32_t    checksum;       // CRC32 of data
    uint8_t     data[];         // 完整数据负载
};
```

**Journal 截断必须是原子操作**：记录截断点到独立控制块并 fsync，避免截断中途崩溃导致文件既不完整也不可识别。

## 六、崩溃恢复

**恢复顺序（全局唯一，不允许多处定义）：**

```
1. 加载 Super Block + 索引快照（此时索引可能含 stale 条目）
2. 顺序重放 Data Journal，按 (LBA, generation) 重建 dirty 块（**仅 writeback 模式**；其他模式无脏块，此步为空）
3. 恢复索引结构：优先重放 Metadata Journal；若该日志缺失或损坏，则扫描 L2 数据区全量重建（快照 + 扫描是兜底路径，两者不矛盾——快照给加速，扫描给正确性）
4. 对索引条目逐一 CRC 校验：
   - clean 条目：CRC 不符 → 标记为 invalid（可能是被复用的槽位上的旧数据）
   - dirty 条目：CRC 不符 → 严重错误，拒绝启动
5. 校验通过后才开放服务
```

**关键原则**：索引快照只记录 clean 条目的位置，不保证数据新鲜度；崩溃后槽位被复用的旧快照条目必须靠 CRC 识别并剔除。

## 七、后端代理机制

### 7.1 代理的双重角色

- **前端**：iSCSI Target，接受 Initiator 连接
- **后端**：iSCSI Initiator，连接真实 Target

启动时后端连接真实 Target，执行 REPORT LUNS 与 READ CAPACITY 探活，确认容量与 sector 大小匹配后才开放前端服务。

### 7.2 libiscsi 使用约束

> 本节适用于 **Linux 实现**；Windows 上不推荐 libiscsi（见第三部分第三十三节），后端应改用纯 Go Initiator。

**libiscsi 的 `iscsi_context` 不是线程安全的**，必须使用事件循环：

```c
// 每 LUN 一个事件循环线程
void* lun_event_loop(void *arg) {
    struct lun_ctx *l = arg;
    while (l->running) {
        // 单线程串行调用，不与其他线程共享 iscsi_context
        iscsi_service(l->iscsi, 10 /* ms */);
    }
    return NULL;
}

// 所有后端调用通过此线程的事件循环驱动
// CGO 侧立即返回，完成通过回调通知
```

**CGO 模型**：`cache_dispatch` 立即返回，不阻塞 Go runtime 线程；后端 IO 完成后通过 `//export` 回调通知。未命中时绝不能同步等待 5-10ms——否则队列深度高时 Go runtime 会不断新建 OS 线程。

### 7.3 后端调用上下文

```c
struct io_request {
    uint64_t        lba;
    uint32_t        length;
    void            *buffer;        // 调用者提供，CGO 中不持有 Go 指针
    bool            is_write;
    bool            is_fua;
    struct lun_ctx  *lun;
    void            (*on_complete)(struct io_request *, int status);
    void            *user_data;     // 回调参数，非 Go 指针
    uint64_t        seqno;          // 发起序号，用于顺序判定与去重
};
```

### 7.4 SCSI 命令映射

| 前端命令 | 后端处理 |
|---|---|
| INQUIRY | 本地响应 |
| REPORT LUNS | 本地响应 |
| READ | 缓存命中→返回；miss→后端异步读→回填 |
| WRITE | 缓存写（按模式）→ 异步刷后端 |
| SYNCHRONIZE CACHE | 刷脏后返回 |
| UNMAP / WRITE SAME | 透传 + 缓存失效 |
| MODE SENSE/SELECT | 透传 |
| PR IN/OUT | 代理侧状态机重写 |
| COMPARE AND WRITE | 代理侧原子 CAS |
| SECURITY PROTOCOL | 透传 |

### 7.5 超时与重传去重

- Initiator 超时重传同一条命令时，代理需识别（ITNexus + CmdSN）
- 在飞的写未完成时，重传的读**不得**返回覆盖中状态的中间值
- 后端响应到达后，需判断对应重传是否仍存在，避免向已超时的连接回包

## 八、L1 / L2 配置建议

| 维度 | L1（内存） | L2（本地 SSD） |
|---|---|---|
| 介质 | DRAM | NVMe SSD |
| 块大小 | 64KB | 64KB |
| sector | 4KB | 4KB |
| 索引 | 驻留内存 | 驻留内存 |
| 数据 | slab 池 | O_DIRECT + io_uring |
| 容量建议 | 1GB - 64GB（按热数据量） | 100GB - 4TB（按总数据量 10-30%） |
| 配额 | 按 Session 计算，LUN 共享 | 同上 |
| 淘汰 | S3-FIFO | 跟随 L1 淘汰决策 |
| 脏块 | writeback 模式持有 | 降级存放，受 Data Journal 保护 |
| fallocate | — | mode=0（注意 stat size 会变化） |
| 建议对齐 | 64KB 块对齐 | SSD 物理块对齐（4KB/8KB） |

**容量规划提示**：索引按 `sizeof(cache_block)`（约 56 字节）/条 × 2 倍容量估算（以实际 sizeof 为准）。若 L2 为 1TB，索引约需 1.9GB RSS；该值需在部署时纳入内存预算。

## 九、部署与运行

### 9.1 部署拓扑

```
前端网络 ──→ iSCSI-Cache ──→ 后端网络 ──→ 真实 Target
              │
              ├── L1（内存）
              └── L2（本地 SSD，建议与 Journal 分盘）
```

**物理部署要点**：
- L2 必须是本地高速设备，网络文件系统不适合做 L2
- Journal 与 L2 分盘部署，避免 SSD 故障连带丢失 Journal
- writeback 模式需企业级 SSD（带 PLP 掉电保护）或 UPS，消费级 SSD 的 DRAM 缓冲可能假回写完成

### 9.2 启动顺序

```
1. 后端 Target 就绪
2. 启动 iSCSI-Cache（后端连接 + 探活 + 加载 L2 索引）
3. 恢复完成（快照 + Journal replay + CRC 校验）
4. 开放前端 3260 端口
5. Initiator 连接前端（代理与后端 Target 不在同一端口冲突）
```

代理会重试后端连接，直到探活成功才开放服务。

### 9.3 构建

```bash
# C 核心
cd ccore && make

# Go 主程序（CGO 链接 C 核心）
export CGO_ENABLED=1
go build -o bin/iscsi-cache ./cmd/iscsi-cache
```

### 9.4 配置示例

```yaml
iscsi:
  listen_addr: "0.0.0.0:3260"
  target_iqn: "iqn.2024-01.com.iscsi-cache:lun1"
  max_sessions: 64

backend:
  target_addr: "192.168.1.100:3260"
  target_iqn: "iqn.2024-01.nas:hdd1"

cache:
  mode: "writeback"            # writethrough | writeback | writearound
  sector_size: 4096            # 显式声明，避免与后端不一致
  identity_binding: true       # 缓存绑定后端 LUN 身份（WWID/Serial + 容量 + 块大小），见 3.5

  l1:
    size: "4GB"
    block_size: "64KB"
  l2:
    path: "/ssd/cache_l2.dat"
    size: "100GB"
    fallocate_mode: 0          # 会扩展文件大小

  eviction:
    algorithm: "s3fifo"
    l1_high_watermark: 0.9
    l1_low_watermark: 0.7

  admission:
    policy: "s3fifo"           # 仅此一种，不叠加

  scan_detection:
    enabled: true
    threshold: 32
    bypass_cache: true         # 旁路仍须查 dirty 索引

  prefetch:
    enabled: true
    sequential_threshold: 4
    speculative_read: false    # 默认关闭
    stride_detection: true

  write:
    merge_window_us: 2000
    preserve_order: true       # 按 Initiator seqno 串行刷盘
    fua_must_sync: true        # FUA 写必须等 fsync

  journal:
    metadata_path: "/ssd/meta_journal.dat"
    data_path: "/ssd/data_journal.dat"
    atomic_truncate: true

  session:
    per_session_quota: true    # 配额按 Session，缓存按 LUN
```

### 9.5 监控指标

```
cache_l1_bytes_used
cache_l1_bytes_total
cache_l2_bytes_used
cache_l2_bytes_total
cache_l1_hits / cache_l1_misses
cache_l2_hits / cache_l2_misses
cache_dirty_blocks
cache_scan_bypasses
cache_evictions
backend_reads / backend_writes
backend_latency_us
journal_pending_entries
```

## 十、模块接口契约

### 10.1 缓存引擎接口

```c
// 读：立即返回结果，数据通过 out_buf 交付
// status: CACHE_HIT_L1 / CACHE_HIT_L2 / CACHE_MISS
int cache_read(struct lun_ctx *lun, uint64_t lba, uint32_t len,
               void *out_buf, uint32_t *out_len, enum cache_status *status);

// 写：立即返回，异步刷后端
// 对未完整缓存的块，内部先触发 baseline 补齐
int cache_write(struct lun_ctx *lun, uint64_t lba, uint32_t len,
                const void *in_buf, bool fua, uint64_t seqno);

// 刷脏：将指定范围 dirty 块同步刷到后端
int cache_flush(struct lun_ctx *lun, uint64_t lba, uint32_t len, bool sync);

// 失效：UNMAP / WRITE SAME 等命令调用
int cache_invalidate(struct lun_ctx *lun, uint64_t lba, uint32_t len);
```

### 10.2 错误码

```
CACHE_OK              成功
CACHE_MISS            未命中，需走后端
CACHE_PARTIAL         sector 部分有效，需补齐
CACHE_ERR_NO_SPACE   配额超限（触发淘汰）
CACHE_ERR_BACKEND     后端不可用
CACHE_ERR_IO          数据 IO 失败
CACHE_ERR_FUA_PENDING FUA 刷盘中，暂不能回 GOOD
```

### 10.3 cache_status → SCSI Status 映射

| cache_status | SCSI Status | 附加 Sense |
|---|---|---|
| CACHE_OK | GOOD | — |
| CACHE_MISS | —（走后端） | — |
| CACHE_ERR_BACKEND | CHECK CONDITION | NOT READY / MEDIUM ERROR |
| CACHE_ERR_IO | CHECK CONDITION | MEDIUM ERROR |
| CACHE_ERR_NO_SPACE | BUSY | — |

### 10.4 回调约定

```c
// 后端 IO 完成回调（在 libiscsi 事件循环线程上下文执行）
// 不得在此回调中直接操作 Go 对象
typedef void (*backend_cb_t)(struct io_request *req, int scsi_status,
                            uint32_t residual_count, void *sense, uint8_t sense_len);

// CGO 导出回调：由 C 侧调用，通知 Go 侧命令完成
extern void go_command_complete(uint64_t cmd_id, int scsi_status,
                                uintptr_t user_data);
```

## 十一、风险与注意事项

- **缓存不是备份**。writeback 模式下 SSD 故障或掉电可能丢脏块，重要数据层必须有快照或异地副本。
- **消费级 SSD 的 DRAM 缓冲**可能回复写完成但数据仍在自身 DRAM，断电即丢。writeback 需 DC SSD 或 UPS。
- **按 LUN 共享缓存**是多 Session / MPIO / 共享存储场景不出脏读的硬性要求，不得以"隔离"为由改回按 Session 分缓存。
- **模式选择优先级**：writethrough > writearound > writeback。writeback 只有在崩溃测试通过后才可启用。
- **索引 RSS 必须预先规划**，1TB L2 约需 1.9GB 索引内存（按 56 字节条目估算）。
- **缓存必须绑定后端 LUN 身份**（WWID/Serial + 容量 + 逻辑块大小）：后端换盘/复用而不清缓存，会把旧盘数据当作新盘命中，属于静默数据损坏，见 3.5。
- **后端容量与 sector 大小必须与代理配置一致**，否则 READ CAPACITY 与 MODE SENSE 返回的值会与真实 Target 矛盾。
- **Serial Number 必须按 LUN 唯一**，ESXi 用 SN 做设备指纹，重复会导致识别异常。

---

# 第二部分 · 自动化测试方案

## 十二、测试定位

**这是一套以"对照测试"为核心的自动化测试体系。** 核心思想：准备一个不经过代理的"正常 iSCSI 地址"（真实干净的 iSCSI Target），把同一条 SCSI 命令同时发给它和 iSCSI-Cache 代理，逐字段比对双方返回结果。两者输出一致，则代理在协议层面正确；不一致，则直接定位偏差。

这套方法的价值在于**不需要理解业务的 IO 语义**——游戏、操作系统、数据库用什么模式读都不重要，重要的是代理对每一条 SCSI 命令的响应与真实 Target 是否等价。

> **平台范围**：本章工具链（tgt / LIO、dm-log-writes、`tc netem`、`kill -9`、`/sys/block`、`stress`）面向 **Linux 部署**。被测端跑在 Windows 时，对照端与故障注入需换成本机等价手段（Windows 内置 iSCSI 目标服务器 / 进程终止 / 磁盘卸载等），并额外覆盖第三部分第三十七节列出的 Windows 专项用例；对照方法与字段比对规则（十三 / 十五节）与平台无关，可直接复用。

### 12.1 什么能靠对照测出来

| 能测出 | 原因 |
|---|---|
| 缓存命中后数据是否与后端一致 | READ 响应逐字节比对 |
| 写穿 / 写回后读到的数据是否正确 | 先写真实 Target、再读代理，比结果 |
| 状态机、时序、DataSN 是否合规 | 响应字段逐项比对 |
| SCSI Status / Sense Data / Residual Count 是否一致 | 三个字段显式比对 |
| 多 Session 共享 LUN 是否脏读 | 双端并发同一 LUN，同时比对 |
| 崩溃恢复后数据是否一致 | 重启后两端各自读，比对 |

### 12.2 什么对照测不出来

| 测不出 | 怎么办 |
|---|---|
| 性能（延迟、吞吐） | 独立做基线压测，记 p50/p99 |
| 掉电后数据完整性 | dm-log-writes / 虚拟机强断电 |
| 两个实现犯同一个错 | 形式化验证补这一层（TLA+ 建模） |
| RFC 里规范本身就含糊的地方 | 人工定规则，写进字段白名单 |

## 十三、核心思路：双轨对照

**对照测试的关键在区分 READ 和 WRITE。**

### 13.1 READ：直比

READ 是幂等的——同一个 LBA 读多少次结果都一样，可直接比：

```
构造 READ(LBA, len)
   ├─→ 发往 真实 Target A   →  Data_A, Status_A, Sense_A, Residual_A
   └─→ 发往 代理 B          →  Data_B, Status_B, Sense_B, Residual_B

比对：Data_A == Data_B
      Status_A == Status_B
      Sense_A == Sense_B
      Residual_A == Residual_B
```

任一字段不一致 → FAIL，记录原始 PDU。

### 13.2 WRITE：终点比

WRITE 不幂等——写一次后端数据就变，第二次写同一 LBA 结果不同，无法直比。改为**同一起点、并行双写、终点校验**：

```
构造黄金数据块 G（固定 seed 的伪随机数据）

真实 Target A  ← WRITE(LBA, G) ─→  回 GOOD
代理背后 LUN B  ← WRITE(LBA, G) ─→  回 GOOD
         （两条命令可乱序，但须同发、同数据）

等待两端都回 GOOD 后：

真实 Target A  ← READ(LBA, G.len) ─→ Data_A
代理背后 LUN B  ← READ(LBA, G.len) ─→ Data_B

比对 Data_A == Data_B == G
```

**精髓**：真实 Target 与代理背后 LUN 是两块独立的物理盘（或不同 LUN），写入路径、缓存、固件完全不同。两端同写同值、读出又一致，说明代理**没有改数据、没有改状态、没有把脏数据写回去**。这恰好避开了"两个实现犯同一个错"的风险——共享的只有 SCSI 规范，不共享代码、缓存、固件。

### 13.3 混合场景：因果序

真实负载是读写交替，命令间有时序因果：

```
合法序列（保序）：
  WRITE LBA 0x1000 "A"
  READ  LBA 0x1000        → 期望读到 "A"
  WRITE LBA 0x1000 "B"
  READ  LBA 0x1000        → 期望读到 "B"
```

因此对照测试必须**严格顺序执行**：每个 LUN 一条命令流水线，前一条完成才发下一条，双端按同一顺序收发。并发在 LUN 之间做，不在命令之间做。

## 十四、测试架构

```
┌─────────────────────────────────────────────────────────────┐
│                     Test Harness (Go)                       │
│                                                             │
│  ┌───────────────────────────────────────────────────────┐   │
│  │              Workload Generator                       │   │
│  │  - 黄金数据生成（固定 seed，可复现）                   │   │
│  │  - 命令序列构造（按因果序）                            │   │
│  │  - 覆盖矩阵驱动                                      │   │
│  └───────────────────┬───────────────────────────────────┘   │
│                      ▼                                       │
│  ┌───────────────────────────────────────────────────────┐   │
│  │              Command Sequencer                        │   │
│  │  - 每个 LUN 一条严格顺序流水线                         │   │
│  │  - 前一条完成才发下一条                               │   │
│  │  - 记录每条命令的发送时间、完成时间、结果             │   │
│  └───────────────┬───────────────────┬───────────────────┘   │
│                  │                   │                       │
│        ┌─────────┴────────┐          │                       │
│        ▼                  ▼          ▼                       │
│  ┌────────────┐   ┌────────────┐   ┌────────────────┐      │
│  │ 真实 Target │   │  代理      │   │  写入日志       │      │
│  │ (对照端)   │   │ (被测端)   │   │  append-only    │      │
│  │ iscsi://A  │   │ iscsi://B  │   │  (LBA,sha256)   │      │
│  └─────┬──────┘   └─────┬──────┘   └───────┬────────┘      │
│        │                │                    │               │
│        ▼                ▼                   ▼               │
│  ┌───────────────────────────────────────────────────────┐   │
│  │              Comparator / Verifier                     │   │
│  │  - READ：逐字节比对 Data                               │   │
│  │  - WRITE：比终点（Data_A == Data_B == 黄金值）         │   │
│  │  - 字段比对：Status / Sense / Residual                │   │
│  │  - 字段白名单：跳过必然不同的字段                      │   │
│  │  - 结果判定：PASS / FAIL / FLAKY                      │   │
│  └───────────────────┬───────────────────────────────────┘   │
│                      ▼                                       │
│  ┌───────────────────────────────────────────────────────┐   │
│  │              Reporter                                  │   │
│  │  - JUnit XML（CI 直接消费）                            │   │
│  │  - 失败详情：原始 PDU、两端响应、时间线                │   │
│  │  - 覆盖率汇总                                          │   │
│  └───────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────┘
```

**三段式工作流：**

1. **环境准备**：拉起真实 Target（tgt 或 LIO），导出 LUN A；拉起 iSCSI-Cache 代理，后端指向 LUN A（或另一块等价 LUN B），前端导出 LUN B'；等待两端就绪，记录容量、块大小、Serial Number 等元信息
2. **测试执行**：Generator 按覆盖矩阵生成命令序列；Sequencer 严格顺序分发到双端；Verifier 逐条比对
3. **清理与判定**：登出两端会话，汇总结果生成报告，失败保留现场（PDU trace / core dump / journal 文件）

## 十五、字段比对规则

### 15.1 逐字段清单

| 字段 | 比对方式 | 说明 |
|---|---|---|
| Data payload | 逐字节 memcmp | 核心，任何不一致即 FAIL |
| SCSI Status | 精确比对 | GOOD / CHECK CONDITION / BUSY |
| Sense Data | 逐字节比对 | INQUIRY VPD、错误码、ASC/ASCQ |
| Residual Count | 精确比对 | 溢出/欠载字节数 |

### 15.2 字段白名单（跳过比对）

这些字段两端必然不同，跳过是为避免假阳性掩盖真问题：

| 字段 | 原因 |
|---|---|
| Serial Number | 代理返回自己的 SN，非后端 |
| Vendor / Product / Revision | 代理可自定义厂商字符串 |
| Target Name / Target Portal | 地址信息本就不同 |
| Timestamps | 两端时间必然不同 |
| 连接相关字段 | CID、会话标识本就不同 |

### 15.3 结果判定

```
PASS   ：所有比对字段一致
FAIL   ：任一核心字段不一致（数据、Status、Sense、Residual）
FLAKY  ：结果在合法区间内抖动（如延迟波动），不影响正确性
```

## 十六、覆盖矩阵

### 16.1 SCSI 命令矩阵

每条命令按三个维度覆盖：正常路径 / 异常路径 / 边界。

| 命令 | 正常路径 | 异常路径 | 边界 |
|---|---|---|---|
| INQUIRY | Standard / VPD 各页 | 非法 EVPD 页号 | Serial Number 唯一性 |
| REPORT LUNS | 单 LUN / 多 LUN | — | LUN 类型字段、寻址格式 |
| READ CAPACITY(10) | 返回容量 | — | 4GB / 2TB 边界（2TB+ 回卷） |
| SERVICE ACTION IN / READ CAPACITY(16) | 64 位 LBA + BlockDescriptor | — | 逻辑块长度字段 |
| READ(10) | 4KB / 64KB / 1MB | LBA 越界 | 保护信息检查 |
| READ(16) | 64 位 LBA 范围 | LBA 越界 | 传输长度单位 = 逻辑块 |
| WRITE(10) | 4KB / 64KB / 1MB | FUA=1 | 保护信息检查 |
| WRITE(16) | 64 位 LBA 范围 | FUA=1 | 传输长度边界 |
| MODE SENSE(6/10) | 透传返回 | 非法页码 | 块参数一致性 |
| MODE SELECT(6/10) | 透传 | 非法参数 | — |
| SYNCHRONIZE CACHE | 全范围 / 部分 | — | 部分范围正确性 |
| UNMAP | 单段 / 多段 | 对齐边界 | 已释放 LBA 读必须返回零 |
| WRITE SAME(10/16) | 不同边界 | — | — |
| PERSISTENT RESERVE IN/OUT | REGISTER / RESERVE / RELEASE | 冲突抢占 | 多 Initiator 抢占语义 |
| COMPARE AND WRITE | 原子 CAS 成功 | CAS 失败回退 | 字节级比对 |
| SECURITY PROTOCOL IN/OUT | SED 密钥交换 | — | 透传、不缓存 |
| TEST UNIT READY | 在线 / 离线 | 后端不可达 | UA 上报 |
| REQUEST SENSE | 清 UA | 无异常挂起 | — |
| TASK MANAGEMENT | ABORT TASK / LUN RESET / CLEAR TASK SET | 在飞 IO 处理 | 超时后重传 |

### 16.2 缓存状态矩阵

同一组命令必须在每种缓存状态下各跑一遍：

| 状态 | 触发方式 | 验证重点 |
|---|---|---|
| cold | 清空缓存后重启代理 | 首次读走后端，数据正确 |
| warm_l1 | 预热后数据全在 L1 | L1 命中返回正确 |
| warm_l2 | 清空 L1，数据在 L2 | L2 命中返回正确 |
| dirty_writeback | 写穿后未刷盘 | 读必须返回新值，崩溃恢复后正确 |

### 16.3 场景矩阵

| 场景 | 配置 | 验证重点 |
|---|---|---|
| 单 Session 单连接 | 标准 | 基础正确性 |
| 多 Session 共享 LUN | 2-4 个 Session | **缓存共享无脏读** |
| MPIO | 同主机两条路径 | 两条路径看到同一份数据 |
| 并发读写 | QD 32，读写混合 | 读写顺序与数据一致性 |
| 大数据流 | 顺序读 1GB+ | 顺序预取正确、无污染 |

## 十七、黄金数据策略

### 17.1 数据生成

```go
// 固定 seed，保证可复现
rng := rand.New(rand.NewSource(0xCACECACE))

func goldenBlock(lba uint64, length uint32) []byte {
    // 用 LBA 做种子衍生，保证同一 LBA 每次生成的数据完全相同
    local := rand.New(rand.NewSource(int64(lba) ^ 0xCACECACE))
    buf := make([]byte, length)
    local.Read(buf)
    return buf
}
```

### 17.2 写入日志

```go
type WriteRecord struct {
    LBA      uint64
    Length   uint32
    Sha256   [32]byte    // 写入数据的哈希
    SentAt   time.Time
    DoneAt   time.Time
    Status   uint8       // 收到的 SCSI Status
}

// append-only，只追加不修改
var writeLog []WriteRecord
```

**写入日志是校验基准**：校验时按日志逐块判定预期值，未确认写入（超时未回 GOOD）读到新值或旧值都合法。

### 17.3 校验流程

```
对每个 4KB 块：
  1. 查写入日志，找到覆盖该块的最新已确认写入 → 预期值 = 日志中的数据
  2. 无已确认写入 → 预期值 = 初始黄金数据
  3. 读真实 Target 的该块 → Data_A
  4. 读代理背后 LUN 的该块 → Data_B
  5. 比对 Data_A == Data_B == 预期值
```

## 十八、故障注入

| 故障类型 | 注入方式 | 验证重点 |
|---|---|---|
| 网络丢包 | `tc qdisc add netem loss 5%` | 超时重传、命令不重复执行 |
| 网络延迟 | `tc qdisc add netem delay 100ms` | 慢后端下的命令序 |
| 网络乱序 | `tc qdisc add netem reorder` | 乱序 PDU 处理 |
| 连接半开 | iptables 丢探测包 | 会话超时与清理 |
| 后端崩溃 | `kill -9 iscsi-cache` | 在飞 IO 返回错误，不静默 |
| 后端不可达 | iptables 阻断 | UA 上报、不卡死 |
| L2 盘满 | `dd if=/dev/zero` 填满 | 降级为纯 L1，不崩溃 |
| SSD 拔出 | `echo 1 > /sys/block/.../device/delete` | 降级处理、错误上报 |
| 内存压力 | `stress --vm 1 --vm-bytes 80%` | OOM 保护、不崩溃 |
| CPU 饱和 | `stress --cpu $(nproc)` | 延迟上升但不出错 |
| 畸形 PDU | Boofuzz / 自定义变异 | 不崩溃、返回合法错误 |
| 会话断连 | 强制断开 TCP | 重连后状态正确 |

## 十九、并发与压力

### 19.1 并发模型

```
- 每个 LUN 一条严格顺序命令流水线（保证因果序）
- 多个 LUN 之间可以并发（验证 LUN 间隔离）
- 多 Session 共享同一 LUN 时，各 Session 独立流水线，但共享缓存
```

### 19.2 压力参数

| 参数 | 范围 |
|---|---|
| 队列深度 | 1 / 8 / 32 / 128 / 256 |
| 读写比例 | 100/0 / 75/25 / 50/50 / 25/75 / 0/100 |
| 数据块大小 | 4KB / 64KB / 256KB / 1MB |
| 并发 LUN 数 | 1 / 4 / 16 |
| 并发 Session 数 | 1 / 2 / 4 / 8 |
| 持续时间 | 1 分钟 / 10 分钟 / 1 小时 / 24 小时 |

### 19.3 长稳测试

```
- 24 小时持续对照测试
- 每小时全量校验一次数据一致性
- 监控：RSS 内存、fd 数量、goroutine 数、L1/L2 命中率
- 达标：零数据不一致 + 无内存泄漏 + fd 不增长
```

## 二十、崩溃与掉电测试

### 20.1 进程崩溃（kill -9）

```
1. 持续压测中
2. kill -9 iscsi-cache
3. 重启代理
4. 全量校验数据一致性
5. 检查 Journal replay 是否 100% 成功
```

**注意**：`kill -9` 只模拟进程崩溃，page cache 仍在，不能代表掉电。

### 20.2 真掉电模拟（dm-log-writes）

```
1. 把 L2 / Journal 放在 dm-log-writes 设备上
2. 持续写入
3. 在随机时间点记录"掉电点"
4. 回滚到掉电点，模拟断电瞬间状态
5. 重新挂载，启动代理
6. 校验 Journal replay 后的数据一致性
```

### 20.3 虚拟机掉电

```
1. 把 L2 / Journal 放在 qcow2
2. 持续压测
3. virsh destroy <vm>（强制断电，不等 guest 响应）
4. 重新启动 vm
5. 校验数据一致性
```

## 二十一、互操作性

| Initiator | 验证项 |
|---|---|
| Linux open-iscsi | 发现 / 登录 / 登出、多 Session、multipath、读一致性 |
| Windows iSCSI Initiator | 发现 / 连接 / 断开、MPIO、磁盘签名 |
| ESXi | VMFS 创建、虚拟机运行、存储 vMotion、PR 冲突 |

**互操作测试同样走对照**：同一 Initiator 分别连真实 Target 和代理，执行相同操作（格式化、写入文件、计算哈希），比对最终数据。

## 二十二、模糊测试

- **状态化 fuzzing**：Boofuzz 维护 iSCSI Login 状态机，变异后续 PDU
- **覆盖率引导**：AFL++ / libFuzzer 针对 PDU 解析函数
- **磁盘变异**：对已保存的合法 PDU 语料做位翻转、长度变异、opcode 替换
- **达标**：72 小时无崩溃、无断言失败、无内存越界

## 二十三、覆盖度分析

**结论：完全覆盖做不到。** 原因不是工程问题，是数学问题——

- iSCSI PDU 首部 48 字节，每个字节 256 种可能，**纯输入空间就有 2^384 种**
- READ/WRITE 的 LBA 是 64 位，传输长度 32 位，组合数 2^96
- 状态空间是组合爆炸：CDB 取值 × 会话状态 × 连接状态 × 缓存状态 × 后端状态 × 错误类型

**但"不能完全覆盖"不等于"协议保证不了"。** 真正能给出数学级保证的是形式化验证——缓存一致性协议本质上是分布式共识问题，这类问题的正确性可以用 TLA+ 严格证明。如果证明"多 Session 共享缓存时不存在违反顺序的调度序列"，该具体缺陷就被彻底排除，与输入空间大小无关。

| 手段 | 能证明什么 | 证不出的 |
|---|---|---|
| 形式化验证（TLA+/Coq） | 特定抽象模型下无死锁、无非法调度 | 模型本身漏了什么（抽象漏洞） |
| 模型检查（SPIN） | 有限状态空间内穷举 | 状态空间爆炸，只能限范围 |
| 模糊测试 | 跑到的路径无崩溃 | 没跑到的路径 |
| 协议对照 | 实现与参考实现行为一致 | 两个实现犯同一个错 |
| 参照式一致性 | 数据未被静默损坏 | 协议状态机是否合规 |

**形式化验证与对照测试是互补关系，不是替代关系**：前者管逻辑正确性，后者管实现是否忠实反映规范。两者都过才算闭环。

**结构性、无法测试的缺陷**：
- 规范本身有歧义（RFC 里 should/may 措辞）
- 参考实现有 bug（两份实现同时错）
- 未定义行为（RFC 未规定的状态组合）
- 模型与实现的鸿沟
- 形式化模型本身的漏洞

**量化验收门槛**：

| 项目 | 标准 |
|---|---|
| 核心状态机覆盖 | ≥95% |
| 消息类型覆盖 | ≥99% |
| 命令矩阵 | 100% |
| 已知 CVE 回归 | 100% |
| 模糊测试 | 72 小时无崩溃 |
| 形式化模型 | 100% 检查通过 |
| 对照组 | ≥10 万次命令零差异 |

## 二十四、自动化执行

### 24.1 一键运行

```bash
# 冒烟测试（约 15 分钟，本地）
autopilot run --suite smoke

# 全量测试（约 6 小时）
autopilot run --suite full

# 重放某次历史运行
autopilot run --replay manifest-2024XXXX-XXXX.json

# 只跑对照层
autopilot run --layer contrast --matrix full

# 只跑某个命令
autopilot run --command READ_16 --coverage full
```

### 24.2 配置

```yaml
test:
  control:
    type: "local"
    cleanup: true
    preserve_on_failure: true

  targets:
    reference:              # 真实 Target（对照端）
      address: "iscsi://10.0.0.10:3260/iqn.2024-01.ref:lun1"
    agent:                  # iSCSI-Cache 代理（被测端）
      address: "iscsi://127.0.0.1:3260/iqn.2024-01.cache:lun1"

  workloads:
    - name: "read_sequential"
      type: "read"
      lba_start: 0
      lba_end: 0x100000
      block_size: 65536
      order: "sequential"
    - name: "read_random"
      type: "read"
      lba_start: 0
      lba_end: 0x100000
      block_size: 4096
      order: "random"
    - name: "write_mixed"
      type: "write"
      lba_start: 0
      lba_end: 0x100000
      block_size: 4096
      fua_ratio: 0.1

  cache_states:
    - cold
    - warm_l1
    - warm_l2
    - dirty_writeback

  commands:
    - INQUIRY
    - REPORT_LUNS
    - READ_CAPACITY_10
    - READ_CAPACITY_16
    - READ_10
    - READ_16
    - WRITE_10
    - WRITE_16
    - MODE_SENSE
    - SYNCHRONIZE_CACHE
    - UNMAP
    - WRITE_SAME
    - PERSISTENT_RESERVE_IN
    - PERSISTENT_RESERVE_OUT
    - COMPARE_AND_WRITE
    - SECURITY_PROTOCOL_IN
    - SECURITY_PROTOCOL_OUT
    - TASK_MANAGEMENT

  concurrency:
    queue_depths: [1, 32, 256]
    sessions: [1, 2, 4]
    luns: [1, 4]

  chaos:
    rounds: 1000
    fault_types:
      - "network_loss"
      - "network_delay"
      - "backend_crash"
      - "agent_crash"
      - "l2_full"
      - "memory_pressure"
    inject_at_load: [0.3, 0.5, 0.7]

  verifier:
    compare_fields:
      - "data"
      - "scsi_status"
      - "sense_data"
      - "residual_count"
    whitelist_fields:
      - "serial_number"
      - "vendor"
      - "product"
      - "target_name"
      - "timestamp"

  reporter:
    format: "junit"
    output: "./report.xml"
    preserve_artifacts: true
    artifact_dir: "./artifacts"
```

### 24.3 声明式用例

```yaml
# cases/read_16_boundary.yaml
name: "READ(16) 4TB 边界"
layer: contrast
priority: P0
tags: [boundary, 64bit-lba]
matrix:
  cache_state: [cold, warm_l1, warm_l2]
  block_size: [512, 4096, 65536]
steps:
  - command: READ_16
    lba: 0xFFFFFFFFFFFFFFFF
    length: 1
    expect:
      scsi_status: GOOD
      data_match: reference
  - command: READ_16
    lba: 0x100000000
    length: 8
    expect:
      scsi_status: GOOD
```

### 24.4 报告格式

```xml
<?xml version="1.0" encoding="UTF-8"?>
<testsuites>
  <testsuite name="iSCSI-Cache-Contrast-Test" tests="1247" failures="0" errors="0" time="342.5">
    <testcase classname="READ_16" name="read_16_boundary_cold" time="0.12"/>
    <testcase classname="WRITE_10" name="write_10_fua_warm_l1" time="0.08"/>
    <testcase classname="CACHE_DIRTY" name="dirty_readback" time="0.15"/>
    <testcase classname="MODE_SENSE" name="mode_sense_page_0x3F">
      <failure message="Sense Data mismatch">
        expected: 0x00000000000000
        actual:   0x00000000000001
        pdu:      "base64 encoded raw PDU"
        ref_resp: "base64 encoded reference response"
        agent_resp: "base64 encoded agent response"
      </failure>
    </testcase>
  </testsuite>
</testsuites>
```

## 二十五、发布闸门

**任一不满足即阻断发布：**

```
✗ 对照测试数据不一致（Data 比对失败）
✗ SCSI Status / Sense / Residual 比对失败
✗ 多 Session 共享 LUN 出现脏读
✗ 混沌测试出现数据不一致
✗ Journal replay 非 100% 成功
✗ 模糊测试出现崩溃
✗ 内存泄漏（ASAN / valgrind）
✗ 文件描述符泄漏
✗ 掉电恢复后返回旧数据（dm-log-writes 验证）
✗ 互操作全流程未通过
```

## 二十六、CI 集成

### 26.1 触发策略

| 触发 | 套件 | 耗时 | 说明 |
|---|---|---|---|
| PR 提交 | smoke | ~15 分钟 | 核心命令对照 + 冷启动 |
| 合并到 main | full | ~6 小时 | 全矩阵 + 混沌 + 互操作 |
| 每日定时 | full + 模糊 8h | ~14 小时 | 深度验证 |
| 发布前 | full + 模糊 72h | ~72 小时 | 发布闸门 |

### 26.2 GitHub Actions 示例

```yaml
name: iSCSI-Cache Test

on:
  pull_request:
    branches: [main]
  push:
    branches: [main]
  schedule:
    - cron: '0 2 * * *'

jobs:
  smoke:
    if: github.event_name == 'pull_request'
    runs-on: ubuntu-22.04
    steps:
      - uses: actions/checkout@v4
      - name: Setup iSCSI environment
        run: |
          sudo apt-get install -y tgt open-iscsi sg3-utils fio
          sudo systemctl start tgt
      - name: Build
        run: make build
      - name: Run smoke tests
        run: ./bin/autopilot run --suite smoke

  full:
    if: github.event_name == 'push' || github.event_name == 'schedule'
    runs-on: ubuntu-22.04
    timeout-minutes: 360
    steps:
      - uses: actions/checkout@v4
      - name: Setup full environment
        run: |
          sudo apt-get install -y tgt open-iscsi sg3-utils fio \
            dmsetup thin-provisioning-tools stress-ng \
            afl++ libasan8 valgrind
          sudo modprobe target_core_mod
          sudo modprobe target_core_iblock
          sudo modprobe iscsi_target_mod
          sudo systemctl start tgt
      - name: Build with sanitizers
        run: make build-sanitizers
      - name: Run full contrast tests
        run: ./bin/autopilot run --suite full
      - name: Run chaos tests
        run: ./bin/autopilot run --layer chaos
      - name: Run fuzzing (limited)
        run: timeout 360m ./bin/fuzzer --duration 6h || true
      - name: Upload artifacts
        if: failure()
        uses: actions/upload-artifact@v4
        with:
          name: test-artifacts
          path: ./artifacts/
      - name: Publish JUnit report
        uses: mikepenz/action-junit-report@v4
        with:
          report_paths: '**/report.xml'
```

## 二十七、测试已知限制

1. **对照测试依赖真实 Target 的正确性**。若真实 Target 本身有 bug，对照结果会把 bug 当成"正确"。缓解：可选多个不同实现的 Target（tgt + LIO）交叉对照。
2. **性能不在对照范围内**。对照只验证正确性，延迟/吞吐需独立压测。
3. **掉电测试需要特殊环境**（dm-log-writes 或虚拟机），无法在普通 CI Runner 上跑。
4. **4TB+ 边界、64 位 LBA 等场景需要大容量 LUN**，测试环境需预留空间。
5. **互操作测试需要真实 Initiator**，Windows / ESXi 需物理机或虚拟化环境。
6. **完全覆盖不可达**，需依靠形式化验证 + 对照测试 + 模糊测试三层互补。

## 二十八、验收标准

| 项目 | 标准 |
|---|---|
| 命令矩阵覆盖 | 100%（所有命令 × 正常/异常/边界 × 四种缓存状态） |
| 对照一致性 | 100% 通过（零 FAIL） |
| 混沌测试 | 1000 轮，零数据不一致 |
| 模糊测试 | 72 小时无崩溃 |
| 内存泄漏 | ASAN / valgrind 无报告 |
| fd 泄漏 | 测试前后 fd 数差 ≤ 2 |
| 掉电恢复 | dm-log-writes 100 轮，零数据损坏 |
| 互操作 | Linux / Windows / ESXi 全流程通过 |
| 长稳测试 | 24 小时，零数据不一致、无泄漏 |

---

# 第三部分 · Windows 适配方案

## 二十九、适配定位

**Windows 上能跑，且读路径的速度提升接近 Linux 版。** 适配的代价主要在 **L2 存储接口**与**内存管理**上；后端协议栈需从 libiscsi 换成纯 Go 实现（一次性改造，见第三十三节），这不是性能瓶颈所在。

第一部分「一、项目定位」声明 Linux 为主实现，那是基于 `O_DIRECT` + io_uring 的原始设计。本章给出在 Windows 上构建功能等价实现的工程方案，适用于让整个代理在 Windows 主机上运行的部署。

**先分清两个层面：**

| 层面 | 内容 | 跨平台性 |
|---|---|---|
| 缓存逻辑层 | 命中判断、扇区位图、S3-FIFO 淘汰、回写策略、Journal 语义 | **纯算法，全平台通用** |
| 存储接口层 | `O_DIRECT`、io_uring、mmap、fallocate、madvise | Linux 专有，需 Windows 替代 |

真正带来速度提升的是第一层。L1 命中的路径是纯内存拷贝，全程不经过磁盘，`O_DIRECT` 与 io_uring 在此完全不参与。

## 三十、Windows 与 Linux 接口对照

| Linux 接口 | Windows 替代 | 语义差异与处理 |
|---|---|---|
| `O_DIRECT`（绕过 page cache） | `FILE_FLAG_NO_BUFFERING` | 强制要求：文件偏移对齐、读写长度对齐、缓冲区地址对齐（对齐值取设备逻辑/物理扇区大小，通常 4KB，NVMe 可能更大）。需自建对齐缓冲区池 |
| `io_uring`（批量提交 + 零 syscall 提交） | IOCP（完成端口） | IOCP 只覆盖完成侧，提交侧仍需逐次 syscall，且没有 io_uring 的批量提交能力。差距与队列深度、请求大小强相关，需在目标环境实测，不宜套用固定百分比 |
| `mmap` / `MAP_SHARED` | `CreateFileMapping` + `MapViewOfFile` | 仍经过系统缓存，仅用于加载 Super Block 等小数据；大数据 IO 一律走 unbuffered |
| `fallocate`（预分配） | `SetFileInformationByHandle`（`FileAllocationInfo` / `FileEndOfFileInfo`） | 预分配应走此接口；`SetFileValidData` **不是**预分配，它只调整"有效数据长度"并需特权，见 31.3 |
| `fdatasync` / `fsync` | `FlushFileBuffers` | 开销通常大于 fdatasync，高频小写场景需实测 |
| `fork` 语义（`MADV_DONTFORK` 阻止页被继承） | 无等价物 | Windows 无 fork；若要禁止子进程继承句柄，用 `SetHandleInformation(h, HANDLE_FLAG_INHERIT, 0)`，与 `MADV_DONTFORK` 不是同一件事 |
| `posix_fadvise(POSIX_FADV_DONTNEED)` | 无直接等价 | 无法主动丢弃系统缓存页，需靠 `FILE_FLAG_NO_BUFFERING` 规避系统缓存 |
| `mlock` / `munlock` | `VirtualLock` / `VirtualUnlock` | 受**进程工作集配额**与 `SeLockMemoryPrivilege` 限制（与 Linux 的 `RLIMIT_MEMLOCK` 不是同一机制），需评估默认值 |
| `pthread_rwlock` | `SRWLOCK`（Vista+） | 读多写少场景性能优于临界区 |
| `sched_setaffinity` | `SetThreadGroupAffinity` / `SetThreadIdealProcessor` | 可用于 NUMA 绑核（`SetThreadAffinityMask` 不能指定处理器组） |

## 三十一、L2 存储适配

### 31.1 必须用 FILE_FLAG_NO_BUFFERING

L2 数据文件句柄必须带 `FILE_FLAG_NO_BUFFERING` 打开，绕过系统缓存避免二次缓存。**不要**用 `FILE_FLAG_WRITE_THROUGH` 来"保证落盘"——它只表示写入不在系统缓存中滞留，**并不保证数据已到介质**（设备自带的 DRAM 缓存仍可能滞留）。真正的落盘保证只能靠 `FlushFileBuffers`，或依赖带 PLP 的设备。

因此：L2 数据文件用 `FILE_FLAG_NO_BUFFERING | FILE_FLAG_OVERLAPPED`；Journal 文件在每条 entry 之后显式 `FlushFileBuffers`（见 31.4），而非依赖 `FILE_FLAG_WRITE_THROUGH`。

```c
HANDLE h = CreateFileW(
    path,
    GENERIC_READ | GENERIC_WRITE,
    FILE_SHARE_READ,            // 不允许其他进程写入，保证独占
    NULL,
    OPEN_ALWAYS,
    FILE_FLAG_NO_BUFFERING | FILE_FLAG_OVERLAPPED,
    NULL
);
```

### 31.2 对齐缓冲区池

`FILE_FLAG_NO_BUFFERING` 要求缓冲区地址、偏移、长度三者均对齐到物理扇区大小（通常是 4KB，NVMe 可能更大）。从 slab 池或 Go 堆拿到的指针都不保证对齐，必须自建池：

```c
// 按 4KB（或设备实际扇区大小）对齐分配
// Windows 上用 _aligned_malloc / VirtualAlloc（天然页对齐）
void *aligned_alloc(size_t size, size_t alignment) {
    // alignment 取设备物理扇区大小：
    //   DeviceIoControl(IOCTL_STORAGE_QUERY_PROPERTY, StorageAccessAlignmentProperty)
    //   → STORAGE_ACCESS_ALIGNMENT_DESCRIPTOR.BytesPerPhysicalSector
    //   退化取 GetDiskFreeSpace 的 lpBytesPerSector（逻辑扇区）
    return _aligned_malloc(size, alignment);
}

struct aligned_iov {
    void    *buf;        // 对齐后的指针，用于 ReadFile/WriteFile
    size_t  alignment;   // 记录分配时的对齐值，用于 _aligned_free
};
```

**读路径多一次拷贝**：L2 读先到对齐缓冲区，再 `memcpy` 到调用者提供的目标地址。这是 Windows 上无法避免的开销，但相比 NVMe 的 0.05-0.1ms 延迟，一次内存拷贝可忽略。

### 31.3 槽位复用必须显式清零

`SetFileValidData` 只扩展"有效数据长度"，不填零。L2 槽位被淘汰后重新分配给新数据时，旧数据残留在磁盘上，读出来就是上一个用户的垃圾数据。因此：

```c
// 槽位复用前，显式写入全零
// 或者在淘汰时清零（淘汰延迟换写入延迟，二选一）
BOOL zero_result = WriteFile(h, zero_buf, slot_size, &written, &ov);
// 务必等该写完成后再把槽位标记为可分配
```

**不要依赖 `SetFileValidData` 来做预分配**——它只是告诉文件系统"这个范围是有效的"，并不保证内容为零。

### 31.4 Journal 落盘

```c
// Data Journal 每条 entry 追加写后必须刷盘
WriteFile(h_journal, entry, entry_size, &written, &ov);
// 等该写完成
GetOverlappedResult(h_journal, &ov, &written, TRUE);
// 强制刷到介质
FlushFileBuffers(h_journal);
```

**注意**：`FlushFileBuffers` 在 SSD 上的行为取决于驱动和固件。消费级 SSD 若带 DRAM 缓冲且无 PLP（掉电保护），可能回报成功而数据仍在自身缓存中。Windows 没有统一的"掉电保护能力"查询接口（`IOCTL_STORAGE_QUERY_PROPERTY` 只提供型号/固件等描述信息），应以厂商型号 + 额定 PLP 能力人工确认，并在文档中明确建议企业级 SSD（或依赖 UPS）。

## 三十二、L1 内存保护

### 32.1 VirtualLock 防 trim

Windows 内存管理器会主动 trim 进程工作集，可能把 L1 缓存页回收掉，导致命中率断崖下跌。

```c
// 锁住 L1 全部 slab 页，阻止被换出或 trim
VirtualLock(slab_base, slab_size);

// 注意：可锁定页数受进程工作集配额与 SeLockMemoryPrivilege 限制
// （与 Linux 的 RLIMIT_MEMLOCK 不是同一机制）
// 需在组策略或代码中申请：
//   SeLockMemoryPrivilege（本地安全策略 → 用户权限分配 → "锁定内存页"）
```

**配额问题**：`VirtualLock` 受 `SeLockMemoryPrivilege` 与单进程锁定页数上限约束。分配 4GB L1 前需确认：

- 进程已获得 `SeLockMemoryPrivilege`（本地安全策略 → 用户权限分配 → "锁定内存页"）
- 非分页池与锁定页数总和未超系统限制

### 32.2 超出配额时的降级策略

若无法获得足够锁定配额（比如普通用户权限、内存紧张），提供降级路径：

```yaml
l1:
  lock_memory: true        # 尝试 VirtualLock
  lock_fallback: "pin_critical_only"  # 锁不住则只锁索引与热块元数据
  # 或 "accept_trim"（接受被 trim，靠 L2 兜底，命中率会下降）
```

## 三十三、后端：绕开 libiscsi 事件循环

### 33.1 libiscsi 在 Windows 上的问题

libiscsi 的 `iscsi_context` 不是线程安全的，依赖 `iscsi_service` 事件循环，其网络后端与事件循环建立在 POSIX 语义之上。Windows 上：

- libiscsi 并非 Windows 一等平台，需要额外的 Winsock / 轮询适配层，句柄数与并发行为不如 POSIX 下可控
- 每 LUN 一个事件循环线程的设计在 Windows 上开销明显
- 编译需 MinGW 或 MSVC 适配层，CGO 交叉编译困难

### 33.2 推荐：纯 Go iSCSI Initiator

Go 的 `net` 包在 Windows 上使用 IOCP 实现，无需 CGO，天然规避了所有事件循环问题：

```
前端 (Windows)                     后端 (任意 Target)
─────────────                      ────────────────
Go net.Conn (IOCP)  ──iSCSI──▶  Linux iSCSI Target
      │
      ▼
  Go 缓存引擎 (纯 Go，无 CGO)
      │
      ▼
  Windows L2 (FILE_FLAG_NO_BUFFERING + IOCP)
```

纯 Go 实现的收益：

- **零 CGO**，无跨语言内存模型风险
- **后端网络 IO 用 IOCP**，完成侧异步高效
- **编译发布简单**，单个静态二进制
- **与 Linux 版共享缓存算法与语义**（命中判断、扇区位图、S3-FIFO、Journal 语义）——注意这是**设计层面**的复用：Windows 版是纯 Go 重写，并不链接 Linux 版的 C 缓存核心（见 34.1 的 `CGO_ENABLED=0`）

### 33.3 若必须保留 C 核心

> ⚠️ 此路线与 34.1 的 `CGO_ENABLED=0` 单文件发布**互斥**：保留 C 核心必然开启 CGO，产出的 exe 依赖 MinGW 运行时，分发方式与 34.1 不同。二者只能选一条。

如果性能要求必须使用 C 实现的缓存引擎，则：

1. 后端网络 IO 交给 Go 的 `net`，通过 channel 与 C 引擎通信
2. C 引擎只负责内存与 L2 的数据路径
3. 后端返回的 SCSI Response 通过 `//export` 回调通知 Go 侧
4. 构建用 MSYS2 + MinGW-w64 工具链，避免 MSVC 与 Go 的 cgo 配合问题

## 三十四、构建与部署

### 34.1 构建

```bash
# 纯 Go 实现：单命令，无外部依赖
GOOS=windows GOARCH=amd64 go build -o iscsi-cache.exe ./cmd/iscsi-cache

# 交叉编译（在 Linux 上产出 Windows 二进制）
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o iscsi-cache.exe ./cmd/iscsi-cache
```

`CGO_ENABLED=0` 是关键——纯 Go 意味着没有 libc 依赖，单个 exe 即可分发。

### 34.2 L2 盘选择

| 介质 | 是否推荐 | 原因 |
|---|---|---|
| NVMe SSD（企业级，带 PLP） | 强烈推荐 | 落盘语义可靠，断电不丢 Journal |
| NVMe SSD（消费级） | 可用但需知风险 | DRAM 缓冲可能假回写完成，writeback 慎用 |
| SATA SSD | 可用 | 速度足够，同样需关注断电保护 |
| 机械硬盘 | 不推荐 | L2 失去意义，退化为单级缓存 |
| 网络盘 / SMB | 禁止 | unbuffered IO 行为不可控，延迟高 |

### 34.3 权限要求

Windows 版需要以下权限，安装时应以管理员身份运行并申请：

- `SeLockMemoryPrivilege`：锁定 L1 内存
- `SeManageVolumePrivilege`：仅当使用 `SetFileValidData` 时需要（不推荐，见 31.3）
- 对 L2 文件的完全控制权限

## 三十五、Windows 版配置示例

```yaml
platform: "windows"

iscsi:
  listen_addr: "0.0.0.0:3260"
  target_iqn: "iqn.2024-01.com.iscsi-cache:lun1"

backend:
  target_addr: "192.168.1.100:3260"
  target_iqn: "iqn.2024-01.nas:hdd1"
  # Windows 版建议用纯 Go Initiator，不走 libiscsi
  initiator_impl: "purego"

cache:
  # Windows 场景建议默认 writearound，避免脏数据风险
  mode: "writearound"
  sector_size: 4096
  # 缓存绑定后端 LUN 身份（WWID/Serial + 容量 + 块大小），见 3.5
  identity_binding: true

  l1:
    size: "4GB"
    block_size: "64KB"
    lock_memory: true
    lock_fallback: "pin_critical_only"

  l2:
    path: "D:\\iscsi-cache\\l2.dat"
    size: "100GB"
    unbuffered_io: true
    alignment: 4096          # 自动探测，可显式覆盖
    zero_on_reuse: true      # 槽位复用前显式清零
    use_set_file_valid_data: false  # 推荐 false，避免读到旧数据

  eviction:
    algorithm: "s3fifo"
    l1_high_watermark: 0.9
    l1_low_watermark: 0.7

  write:
    bypass_cache: true       # Windows 版默认写绕过缓存
    merge_window_us: 2000
    preserve_order: true

  journal:
    data_path: "D:\\iscsi-cache\\data.jnl"
    metadata_path: "D:\\iscsi-cache\\meta.jnl"
    flush_method: "FlushFileBuffers"  # Windows 专用
    # 检测 SSD 是否带掉电保护，不带则在启动时告警
    require_power_loss_protection: true

  windows:
    # 锁定内存配额不足时的行为
    memory_lock_policy: "strict"   # strict / pin_critical / none
    # 工作集保护：防止被内存管理器 trim
    protect_workingset: true
    # 使用 IOCP 完成端口（纯 Go 后端默认已用）
    use_iocp: true
```

## 三十六、Windows 版预期性能

> 下表为**量级估算**，非实测值；实际取决于后端介质、L2 设备（NVMe / SATA）与负载形态，需在目标环境压测。

| 场景 | 无缓存（直连 HDD） | Windows 版缓存 | 提升倍数（按区间端点估算） |
|---|---|---|---|
| 游戏资源重复加载（L1 命中） | 8-15ms | 1-2μs | 约 4,000-15,000× |
| 游戏资源首次加载（L2 命中） | 8-15ms | 0.05-0.1ms（NVMe） | 约 80-300× |
| 随机 4KB 读（L1 命中） | 8-10ms | 1-2μs | 约 4,000-10,000× |
| 顺序大块读（L2 命中） | 150MB/s | 2-3GB/s | 约 13-20× |
| OS 冷启动 → 二次启动 | 20-40s | 12-18s | 约 1.5-2×（受启动阶段 IO 模式影响大） |

**与 Linux 版的差距主要来自**：高队列深度下 L2 写入的提交开销（IOCP 无批量提交，差距需实测）、Journal 高频 fsync 的 `FlushFileBuffers` 开销、以及内存被 trim 导致的命中率下降。游戏与 OS 盘的负载以读为主，读路径（L1 纯内存拷贝、L2 unbuffered 读取）与 Linux 版差异很小。

## 三十七、Windows 版测试补充

Windows 版需额外覆盖以下测试点：

| 测试项 | 验证内容 |
|---|---|
| 对齐缓冲区边界 | 偏移/长度/地址未对齐时的 `ERROR_INVALID_PARAMETER` 处理 |
| 内存锁定失败 | `VirtualLock` 返回 `ERROR_WORKING_SET_QUOTA` 时的降级行为 |
| 工作集 trim 模拟 | 用 `EmptyWorkingSet` 或内存压力工具主动 trim 后，验证 L1 数据仍可用 |
| `FlushFileBuffers` 行为 | 验证写完成后数据真正落盘（可用写入后断电图证） |
| 槽位复用不返回旧数据 | unbuffered 读取时不会读到上一个块残留的数据（验证 valid 记账与槽位写入完整性） |
| 后端身份变化（见 3.5） | 替换后端 LUN（WWID/容量/块大小变化）后重启，验证 L2 缓存被整体丢弃、不返回旧盘数据 |
| IOCP 高 QD 稳定性 | QD 256 持续 24 小时，验证完成端口不丢完成包 |
| 句柄继承 | 子进程不会意外继承 L2 / Journal 文件句柄 |
| 纯 Go Initiator 对照 | 与 Linux 版用同一份黄金数据比对，验证后端 IO 实现一致 |
