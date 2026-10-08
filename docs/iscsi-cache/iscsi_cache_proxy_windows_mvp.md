# iSCSI-Cache · Windows 只读加速 MVP

> 本文是 [iscsi_cache_proxy_design.md](./iscsi_cache_proxy_design.md)（下称"完整设计"）的裁剪版实施文档，
> 目标是在 **Windows 客户端**上落地一个**只加速读**的 iSCSI 缓存代理 MVP。
> 完整设计里的写回、Journal、崩溃恢复、PR 状态机、形式化验证等不进入 MVP。
>
> **已实装形态（以第十五节为准）**：本模块已并入 Vault-Agent（**内嵌、一个客户端一个代理**），
> 代理在**一个门户上同时服务多个库**（按 TargetName 路由到各自后端目标），命令**并发执行**
> （命令窗口见 4.3）。上文第一至十四节保留为单目标 MVP 的设计与推导过程，其中与"单 LUN /
> 单 Session / 串行执行"相关的表述已在 4.3、第九节与第十五节更正。

---

## 一、目标与范围

**目标**：在客户端与本机应用/Initiator 之间插入缓存代理，让**热区读**命中本地缓存，从而减少对服务端的重复读流量并降低读延迟。

```
应用 / 用户 Initiator ──iSCSI──▶ Windows 缓存代理 ──iSCSI──▶ 服务端 Target
                                    ├── 前端：iSCSI Target（接受连接）
                                    ├── 缓存：L1 内存 + L2 本地 NVMe
                                    └── 后端：iSCSI Initiator（纯 Go）
```

**MVP 做的事**

- 前端 iSCSI Target（纯 Go），单 LUN、单 Session、`ErrorRecoveryLevel=0`
- 后端 iSCSI Initiator（纯 Go），连接服务端真实 Target
- 读路径多级缓存：L1 内存 + L2 本地 unbuffered 文件，S3-FIFO 淘汰
- 写路径 **writearound**：写直接透传后端并让对应缓存失效（不产生脏块）
- 缓存绑定后端 LUN 身份（见第八节），后端换盘即整体丢弃缓存

**MVP 明确不做**（列入后续版本，见第十三节）

| 不做 | 原因 |
|---|---|
| writeback / Data Journal / 崩溃重放 | 会引入脏块与崩溃一致性，是完整设计里最重的部分 |
| L2 索引持久化 | 重启后冷启动可接受，换取"零崩溃恢复复杂度" |
| 多 Session / MPIO 共享同一 LUN | 一致性边界之外，需要按 LUN 共享缓存 + 锁语义 |
| PERSISTENT RESERVE / COMPARE AND WRITE | 代理→后端是多对一 I_T nexus，需自建 PR 状态机 |
| SECURITY PROTOCOL / WRITE SAME 精细语义 | 边缘命令，MVP 直接拒绝或简单透传 |
| 投机预取、步幅/突发检测 | 先保证正确性与防污染，预取后置 |
| CGO / C 缓存核心 | 纯 Go，`CGO_ENABLED=0` 单文件发布 |
| TLA+ 形式化验证 | 对照测试先覆盖 |

---

## 二、与完整设计的关键差异

| 维度 | 完整设计 | Windows MVP |
|---|---|---|
| 平台 | Linux 为主（`O_DIRECT` + io_uring） | Windows（`FILE_FLAG_NO_BUFFERING` + IOCP） |
| 后端协议栈 | libiscsi（C，事件循环） | 纯 Go iSCSI Initiator（无 CGO） |
| 缓存核心 | C（CGO 链接） | 纯 Go |
| 写策略 | writethrough / writeback / writearound + Journal | 仅 writearound（无脏块、无 Journal） |
| 缓存层级 | L1 + L2 + 索引快照 + 双 Journal | L1 + L2（索引仅在内存） |
| 崩溃恢复 | 快照 + Journal replay + CRC 校验 | 无：重启清空 L1/L2 逻辑视图 |
| 缓存归属 | 按 LUN 共享，配额按 Session | 单 LUN、单 Session，缓存绑定后端身份 |
| 命令覆盖 | 全量 SCSI（含 PR / CAS / SED） | Windows Initiator 所需最小集 |
| 淘汰 | S3-FIFO | S3-FIFO（保留，防顺序扫描污染） |
| 验证 | 全矩阵 + 混沌 + 模糊 + 形式化 | 精简对照测试 + Windows 专项 |

> 结论：MVP 保留完整设计里**唯一真正产生读加速**的部分（L1/L2 读缓存 + S3-FIFO + 防污染），
> 把与"写安全"相关的全部复杂度（Journal / 回写 / 恢复）移出。

---

## 三、组件与代码落点

| 组件 | 说明 | 建议位置 |
|---|---|---|
| 主程序 | Windows 控制台程序 / Windows 服务 | `cmd/vault-iscsi-cache/` |
| 前端 Target | 纯 Go iSCSI Target（登录握手 + SCSI 命令分发） | `internal/iscsicache/frontend/` |
| 后端 Initiator | 纯 Go iSCSI Initiator（登录 + 命令执行） | `internal/iscsicache/backend/` |
| 缓存引擎 | L1/L2 + 索引 + S3-FIFO + 失效 | `internal/iscsicache/cache/` |
| L2 存储 | unbuffered 文件 + 对齐缓冲区池 | `internal/iscsicache/l2/` |
| 身份绑定 | 后端 IQN + VPD 0x80/0x83 + 容量 + 块大小 | `internal/iscsicache/identity/` |

**与仓库现有能力的关系**

- `internal/platform/iscsiinitiator`（Windows 原生发起端，PowerShell）**不用于** MVP 数据路径，但可用于对照测试与排障：把服务端 LUN 直接挂到本机，构造"无代理"对照端。
- `internal/platform/iscsitarget`（Windows iSCSI 目标服务器）**不适合**做本代理前端——它只能把 **VHDX 文件**发布为目标，无法服务"远端 LUN 的缓存视图"；因此前端必须自实现（这是 MVP 最大的工程量）。
- MVP 后端用纯 Go 自实现，不引入 C 库；Go `net` 在 Windows 上走 IOCP，天然满足异步完成侧需求。

---

## 四、前端 iSCSI Target（MVP 命令集）

### 4.1 登录与协商参数

| 键 | 取值 | 说明 |
|---|---|---|
| `AuthMethod` | `None`（可选 `CHAP`） | MVP 默认无认证；仅监听本机时足够 |
| `SessionType` | `Normal` | — |
| `MaxConnections` | `1` | 单连接，规避多连接 DataSN/命令窗口复杂度 |
| `ErrorRecoveryLevel` | `0` | **不实现重传与 digest 恢复**，这是 MVP 精简的关键 |
| `InitialR2T` | `Yes` | 写路径走标准 R2T 流程 |
| `ImmediateData` | `Yes` | 允许首突发立即数据 |
| `DataPDUInOrder` / `DataSequenceInOrder` | `Yes` | 顺序 PDU，无需乱序重组缓冲区 |
| `MaxRecvDataSegmentLength` | 65536 | 与对齐缓冲池粒度一致 |
| `FirstBurstLength` | 65536 | — |
| `MaxBurstLength` | 262144 | — |
| `HeaderDigest` / `DataDigest` | `None` | Windows Initiator 默认值；ERL=0 下无需 digest |

### 4.2 支持的命令

| 命令 | 处理 |
|---|---|
| `TEST UNIT READY` | 本地：后端会话正常回 GOOD，否则 CHECK CONDITION + NOT READY |
| `INQUIRY`（标准） | 本地：返回代理伪造的 Vendor/Product/Revision |
| `INQUIRY` VPD 0x00 / 0x80 / 0x83 / 0xB0 / 0xB1 / 0xB2 | 本地：0x80 的 Serial 必须按 LUN 唯一（Windows 用 SN 做磁盘指纹） |
| `INQUIRY` 其他 VPD 页 | 透传后端 |
| `REPORT LUNS` | 本地：返回单个 LUN 0 |
| `READ CAPACITY(10)` | 本地：返回探活时缓存的值 |
| `READ CAPACITY(16)`（`SERVICE ACTION IN` 0x10） | 本地：64 位 LBA + 逻辑块长度 |
| `READ(10)` / `READ(16)` | **缓存读**：L1 → L2 → 后端，按 DataSN 顺序发 Data-In |
| `WRITE(10)` / `WRITE(16)` | **透传 + 失效**：走 R2T 收满数据 → 转后端（FUA 透传）→ 成功后失效覆盖范围 |
| `MODE SENSE(6/10)` | 透传后端（不本地模拟，避免块大小/写保护位不一致） |
| `MODE SELECT(6/10)` | 透传后端 |
| `SYNCHRONIZE CACHE` | 本地回 GOOD（MVP 无脏块） |
| `REQUEST SENSE` | 本地：清挂起的 UA |
| `UNMAP` | 透传 + 失效对应范围（若后端支持） |
| `TASK MANAGEMENT`：`ABORT TASK` | 尽力实现（终止在飞的后端 IO） |
| 其余命令（含 PR / CAS / WRITE SAME / SECURITY PROTOCOL） | CHECK CONDITION + ILLEGAL REQUEST（ASC=0x20） |

> 未实现命令**必须返回规范的 Sense**，不能静默丢包或挂死——Windows Initiator 会重试并可能把磁盘标记为离线。

### 4.3 最小状态机

对每条命令维护：`CDB → 数据阶段（Data-In / R2T+Data-Out）→ SCSI Response`。

ERL=0 且单连接下不需要重传去重；但**命令必须并发执行**——真实 Initiator 会保持多条命令在飞
（队列深度 QD32 等），若把命令窗口塌成 1，等于把 Initiator 的队列深度强行压到串行，顺序读吞吐
会被毁掉。实现上：

- 命令**到达即执行**，`sendMu` 只串行化 PDU 的发送与序列号（StatSN 按**命令**自增，
  多 PDU 的 Data-In 突发不得与另一条命令的响应交错），不串行化命令本身的执行。
- `MaxCmdSN` 公布一个**宽松的命令窗口**（`cmdWindow = 64`），允许最多 64 条命令同时在飞；
  `ExpCmdSN` 由连接读取协程用无锁 CAS 单独维护（记录到达命令不与正在发响应的命令抢锁）。
- 同一上游块的并发读 miss 由缓存的 **single-flight** 合并，一次后端读即可（见 6.3），
  因此放开命令窗口不会把后端打爆。

---

## 五、后端 iSCSI Initiator（纯 Go）

- 功能：Login（含文本协商）→ 执行 SCSI 命令（READ/WRITE/MODE SENSE/同步缓存等）→ Logout；断线重连。
- 协商参数与前端对齐（ERL=0、`InitialR2T=Yes`、digest=None），避免翻译层。
- **启动探活**：`REPORT LUNS` → `READ CAPACITY(16)` → `INQUIRY` VPD 0x80/0x83，得到容量、块大小与身份；此三者是前端 `READ CAPACITY` / `MODE SENSE` 返回值的唯一来源（不本地猜测）。
- 未就绪前**不开放前端 3260**（对应完整设计 9.2 启动顺序）。
- 实现要点：请求-响应按 ITT 关联；单 LUN 串行或有限并发（MVP 建议每 LUN 一个读队列 + single-flight）。

---

## 六、缓存引擎

### 6.1 参数

| 参数 | 取值 |
|---|---|
| 块大小（缓存条目） | 64KB |
| 扇区粒度（valid 记账） | 4KB（64KB 块 = 16 sector，`uint16` bitmap） |
| 索引 | 每分片一个 `map[uint64]uint32`（LBA→槽位），分片锁（如 64/256 分片） |
| 淘汰 | S3-FIFO（New / Mid / Old） |
| L1 水位 | 高 90% 触发淘汰，低 70% 停止 |
| L2 槽位 | 固定 64KB，槽位号由空闲链表分配 |
| 填充粒度 | **按请求覆盖的 sector 填充**（不放大后端读流量） |

### 6.2 只读缓存 → 没有 dirty 位图

MVP 写路径不入缓存，因此缓存条目只有 `valid_bitmap`，**没有 `dirty_bitmap`**，也不存在 baseline 回读（完整设计 4.3 的"先读整块做 baseline"仅在 writeback 下需要）。

### 6.3 读路径

```
READ(lba, len)
  1. 按 64KB 块边界切分，逐块查索引
  2. L1 命中且 sector valid → 直接命中
  3. L1 miss、L2 命中 → 读入 L1（提升热度）→ 命中
  4. 仍未命中的 sector → 合并为连续区间，向后端发起读
  5. 回填：分配/复用 L1 块并置 valid；按准入策略决定是否同步落 L2
  6. 数据就绪后按 DataSN 升序发送 Data-In，最后发 SCSI Response
```

**single-flight**：同一 64KB 块的并发 miss 合并为一次后端读，避免惊群把后端打爆。

### 6.4 防污染

- **顺序扫描检测**：每 Session 记录 `last_end_lba`，连续 32 个"起点==上次终点"的请求判定为扫描 → 该流**不填充缓存**，直接走后端。
- 扫描期间仍可顺序读加速后端 IO，但不进缓存（避免一次性大读冲掉热区）。
- 预取默认关闭（`speculative_read=false`）。

### 6.5 L2 的生命周期（MVP 关键简化）

- L2 文件布局：`[SuperBlock 4KB][数据槽位区]`，`FILE_FLAG_NO_BUFFERING | FILE_FLAG_OVERLAPPED` 打开。
- **索引不持久化**：进程启动时索引为空 → 逻辑上 L2 为空，从头填充。
- 因为"只写整块中实际命中的 sector、且只读 valid sector"，槽位复用**不需要显式清零**（读到的一定是本次写入的数据）。
- 代价：重启后 L2 冷启动。对"读热区"负载，冷启动后很快重新预热，MVP 接受。

---

## 七、写路径（writearound + 失效）

```
WRITE(lba, len, fua)
  1. 收满数据（ImmediateData + R2T 补齐）
  2. 原样转发后端（FUA 透传，不合并、不延迟）
  3. 后端回 GOOD 后，失效 [lba, len) 覆盖的 sector：
       - 部分失效 → 清对应 valid 位
       - 整块失效 → 从索引摘除，槽位归还空闲链表
  4. 回 GOOD
```

- `SYNCHRONIZE CACHE` 直接回 GOOD（无脏块）。
- 失效**必须在回 GOOD 之前**完成，否则紧随其后的读会命中旧值。
- 这是唯一保证"写后读一致"的机制，必须作为对照测试的 P0 用例。

---

## 八、缓存身份绑定（MVP 必做）

`cache_namespace = H(后端 IQN ‖ VPD0x80/0x83 WWID ‖ LBA 数 ‖ 逻辑块大小)`

- 启动/重连探活后计算并与内存中记录比对；不一致 → **整体丢弃 L1/L2**（不清零文件，只清索引与游标）。
- 存在意义：后端 LUN 被替换或复用而 Target IQN 不变时，防止把旧盘数据当新盘命中（静默数据损坏）。
- 注意：前端返回给 Initiator 的 Serial 是**代理伪造**的；后端身份用的是**后端自己的** WWID/Serial，两者不可混用。

（完整设计见 3.5 节。）

---

## 九、一致性边界（必须写入产品说明）

- 会话层面：每个上游 LUN 仍按 **单写者**（本代理）建模；一个门户可同时承载**多个**上游
  目标（每个库一个目标），但**每个目标各自独立**，不共享缓存。
- 不支持多个 Initiator / MPIO 共享同一 LUN。
- 若同一 LUN 被第二个 Initiator 写入，MVP **无法保证**缓存一致性——需升级到完整设计的"按 LUN 共享缓存 + 多写者失效协议"。
- 由于写路径 writearound 且带范围失效，单写者场景下"写后读"是安全的。
- **命令并发不等于多写者**：放开命令窗口（4.3）只是让同一条 iSCSI 会话里多条命令并行，
  写路径的失效仍按命令串行裁决（见 proxy 的读写锁），不影响上述一致性结论。

---

## 十、Windows 实现要点

| 事项 | 做法 |
|---|---|
| 网络 IO | Go `net.Conn`（Windows 下走 IOCP），无 CGO |
| L2 文件 | `CreateFileW` + `FILE_FLAG_NO_BUFFERING \| FILE_FLAG_OVERLAPPED`，偏移/长度/地址三者对齐到物理扇区 |
| 对齐缓冲池 | `_aligned_malloc` 或 `VirtualAlloc`；对齐值取 `IOCTL_STORAGE_QUERY_PROPERTY` + `StorageAccessAlignmentProperty` 的 `BytesPerPhysicalSector` |
| 落盘 | **不依赖** `FILE_FLAG_WRITE_THROUGH`（不保证到介质）；MVP 无脏块，正常不需要 `FlushFileBuffers` |
| L1 内存 | slab 预分配；`VirtualLock` 失败则降级为"只锁索引"，不做强依赖 |
| 发布 | `CGO_ENABLED=0` 单个 exe；以管理员运行（L2 文件 + 可选 `SeLockMemoryPrivilege`） |
| 监听 | 默认 `127.0.0.1:3260`（仅本机）；如需远端连接改 `0.0.0.0` 并启用 CHAP + 防火墙规则 |

Windows 侧 API 语义与坑见完整设计第三部分（30–34 节）。

---

## 十一、配置示例（MVP）

```yaml
listen_addr: "127.0.0.1:3260"
target_iqn: "iqn.2024-01.local.iscsi-cache:disk0"

backend:
  address: "192.168.1.100:3260"
  target_iqn: "iqn.2024-01.nas:disk0"
  auth: "none"                # none | chap（单向 CHAP）
  username: ""                # auth=chap 时必填（CHAP_N）
  secret: ""                  # auth=chap 时必填（共享密钥）

cache:
  mode: "writearound"         # MVP 仅此一种
  block_size: "64KB"
  sector_size: 4096
  identity_binding: true

  l1:
    size: "2GB"
    lock_memory: true
    lock_fallback: "pin_index_only"

  l2:
    path: "D:\\iscsi-cache\\l2.dat"
    size: "100GB"
    persist_index: false      # MVP：重启清空
    alignment: 4096           # 自动探测，可显式覆盖

  eviction:
    algorithm: "s3fifo"
    l1_high_watermark: 0.9
    l1_low_watermark: 0.7

  scan_detection:
    enabled: true
    threshold: 32

  prefetch:
    enabled: false
```

---

## 十二、验证（精简对照测试）

对照方法与字段比对规则沿用完整设计第十三 / 十五章（与平台无关）：

- **READ 直比**：同一 `READ(LBA,len)` 分别发对照端与代理，比 Data / Status / Sense / Residual。
- **WRITE 终点比**：同发同值，待两端回 GOOD 后各自读回，比 `Data_A == Data_B == 黄金值`。
- **对照端**：`internal/platform/iscsiinitiator` 把服务端 LUN 直连挂到本机（无代理路径）；或另接一块等价 LUN。

**MVP 必过用例**

| 用例 | 验证点 |
|---|---|
| cold / warm_l1 / warm_l2 下 READ(10/16) | 命中与未命中数据均与对照一致 |
| **写后立即读同 LBA** | 必须返回新值（失效有效），不允许命中旧值 |
| 部分 sector 命中 + 部分未命中 | 跨块/部分 valid 的补齐正确 |
| 顺序读 1GB | 顺序扫描不污染热区，数据正确 |
| 重启后读 | L2 逻辑清空，仍返回正确数据 |
| `kill` 进程后重启 | 无残留脏状态，读一致 |
| 后端身份变化（换 LUN） | L2 缓存整体丢弃，不返回旧盘数据 |
| Windows Initiator 全流程 | 发现 / 连接 / 分区 / 格式化 / 文件读写一致 |

Windows 专项补充用例见完整设计第三十七章。

---

## 十三、里程碑

| 阶段 | 交付 | 出口判据 |
|---|---|---|
| M1 只读直通 | 前端 Target + 后端 Initiator，无缓存全透传 | Windows Initiator 能发现/连接/读写，对照测试全绿 |
| M2 L1 缓存 | 内存读缓存 + 失效路径 | 写后读一致；命中率可观测 |
| M3 L2 + 淘汰 | unbuffered L2 + S3-FIFO + 防污染 | 重启后正确；顺序读不污染 |
| M4 身份绑定 | `cache_namespace` + 整体丢弃 | 换盘用例通过 |
| M5 稳定性 | 长稳 + Windows 专项 | 24h 无数据不一致、无泄漏 |

---

## 十四、风险与后续

| 风险 | 应对 |
|---|---|
| **前端 iSCSI Target 自实现是最大工程量与风险** | M1 先做"无缓存直通"打通协议互通，再叠缓存；命令集保持最小化，ERL=0 |
| 未实现命令的 Sense 不规范导致 Windows 掉盘 | 建立"必须返回规范 Sense"的清单与对照用例 |
| 顺序读把热区冲掉 | 保留扫描检测（6.4） |
| 单写者假设被打破 | 在文档与配置中明确限制；必要时升级到完整设计的按 LUN 共享缓存 |
| L2 冷启动拖慢首次体验 | 可选后续引入索引快照（届时再评估崩溃一致性成本） |

**升级到完整设计的路径**：L2 索引快照 → writeback + Data Journal + 崩溃恢复 → 多 Session/MPIO 的按 LUN 共享缓存与 PR 状态机 → 形式化验证。

---

## 十五、多目标门户与客户端集成（已实装）

前面的单目标形态（`cmd/vault-iscsi-cache` + 一份 YAML）只用于独立验证协议互通。**实装进客户端**
后的形态如下。

### 15.1 一个客户端一个代理，一个门户多个目标

- 模块整体并入 **Vault-Agent 进程**（内嵌），随客户端启停；不单独部署。
- 代理在 `127.0.0.1:3261` 开**唯一一个** iSCSI 门户，**同时服务多个**启用了缓存的库：
  每个库在门户上注册一个本地 IQN，`SendTargets=all` 列出全部已注册 IQN，登录时按
  **TargetName 路由**到各自的后端目标与缓存实例；未注册的 TargetName 返回
  `class=0x02 / detail=0x03`（未找到目标）。
- **懒启动**：没有任何库启用缓存时不开门户；第一个库启用并挂载时才创建监听（避免无用端口占用）。
- **本地 IQN 命名**：`iqn.2024-01.local.vault:vcache-<sanitize(allocation_id)>`。
  短名必须做字符清洗且**不得与服务端 IQN 短名互为子串**，否则两个目标的短名会串台
  （真实 initiator 的发现/重连可能按短名匹配）。
- **代理盘标识**：向本机 Initiator 显式覆盖 `Vendor=VAULT`、`Product=Vault Cache Disk`，
  与服务端目标区分开，便于用户与排障时辨认。

### 15.2 配额：客户端总量、按启用库均分

- L1/L2 预算**只在客户端设置面配置**（`PATCH /agent/config` 的 `cache_l1_bytes` /
  `cache_l2_bytes`），语义是**客户端总量**，不是每库上限。
- 总量 ÷ **已启用缓存的库数** = 每个库分到的 L1/L2 配额；**挂载时定下，运行中不重算**
  （避免运行中改总量导致已挂载库的配额漂移）。
- `cache_l2_bytes = 0` 表示关闭 L2（只做内存缓存）；0 与"没配"必须区分（见 agent 配置的
  `cacheL2Set`）。默认 L1 = 512MiB、L2 = 4GiB。
- **L2 落盘目录也由客户端设置**（`cache_l2_dir`）：空串用默认 `<DataDir>\iscsi-cache`，
  非空必须是绝对路径（目录按需创建）。改动在下次挂载该库时生效。

### 15.3 挂载链路与回退

- 库启用缓存且缓存生效时，挂载链路变为 **本机 → 本地缓存代理 → 服务端目标**；
  挂载记录里的 `target_iqn` / `portal` 记的是**本地门户**公示的值（`127.0.0.1:3261`）。
- 未启用缓存的库仍是 本机 → 服务端目标 **直连**。
- **回退直连**：门户建不起来（如 3261 被占用）或后端不可用时，库**照常挂载**，只是回退直连
  服务端目标，原因写入挂载状态的 `cache_error` —— 绝不因缓存导致挂载失败。
- **卸载即回收**：卸载流程在**断开会话之后、回写 release 之前**注销该库在门户上的目标并回收
  其 L2 文件；顺序不能反，否则仍在飞的命令会失败。
- **后端认证**：服务端下发 `auth_mode=chap` 时，代理用**单向 CHAP**（RFC 7143 §11.2.2）与后端
  目标完成登录握手——`chap_user` / `chap_secret` 随挂载参数下发（明文仅本次使用，不落盘客户端），
  后端发起端在该阶段用 `CHAP_A=5`（MD5）应答挑战。认证失败与端口占用、后端不可达一样**回退直连**，
  原因写入 `cache_error`。反向 CHAP 不在支持范围（服务端自动下发路径不产生反向密钥）。
  本机门户（面向 `127.0.0.1`）仍只提供 `AuthMethod=None`：回环链路无需认证，因此**缓存生效时
  本机发起端一律以 None 连本地门户**，绝不能把服务端下发的 chap 继续传给这一环（否则本机会对
  本地门户做 CHAP 并以 `Authentication Failure` 收场）。

### 15.4 状态与界面

- `GET /agent/cache` 返回门户状态、客户端总量预算与每库配额、以及**按库**汇总的用量
  （L1/L2 占用）与命中情况（读命令数、整命令命中数、命中率）。字段见
  [agent-api.md](../agent-api.md) 的「本地读缓存」。
- 存储库卡片据此展示"缓存用量/命中情况"：`cache_enabled=true` 但 `cache_active=false` 时
  显示"缓存未生效"+ 原因 tooltip。

### 15.5 独立验证入口（保留）

`cmd/vault-iscsi-cache`（YAML 配置 + `-check` 自测）**保留**，用于脱离 Vault-Agent 验证前端
Target 与后端 Initiator 的协议互通；示例配置见 `cmd/vault-iscsi-cache/iscsi-cache.example.yaml`。
它**不引用** vault agent / client 代码，Linux 侧以占位实现（`l2.ErrUnsupported`）自动降级为
仅 L1。