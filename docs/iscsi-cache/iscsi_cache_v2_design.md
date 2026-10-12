# iSCSI Cache v2 设计文档：透传代理 + 旁路读缓存

> 状态：设计定稿，待实施
> 日期：2026-10-11
> 前置阅读：`iscsi_cache_proxy_design.md`（v1，终止式代理，保留作参考，不再演进）

---

## 1. 背景：v1 的教训

v1 是**终止式代理**：自建 iSCSI Target 终止 initiator 会话，自建 Initiator 连接后端，中间串接缓存。2026-10-11 实测（CrystalDiskMark，真实 Microsoft initiator）暴露了该架构的结构性问题：

- 写路径为消除 R2T 往返引入 InitialR2T=No/ImmediateData=Yes 协商后，1MiB 顺序写从 8.6ms/op 恶化到 24.7ms/op（自研 bench 客户端完全复现不出来——它协商 1MiB first burst，而真实 MS 生态 first burst 只有 8KiB）；
- 即使回退写协商，整链性能仍比直连损失 50% 以上，且残留原因难以在终止式架构的众多环节（协商、StatSN 记账、Data-In 批发送、连接池预热、MRDSL 匹配）中定位；
- 根因共性：**性能下限不受控**。终止式代理让 100% 的流量都经过自研会话状态机，任一环节出错，全线命令变慢。

v2 的出发点是把性能下限锁死在直连水平：**未命中缓存的流量只做字节级透传，代理不终止会话、不生成 PDU、不参与协商**。

## 2. 设计目标与非目标

### 2.1 目标

| # | 目标 | 验收口径 |
|---|------|---------|
| G1 | 透传路径性能 ≥ 直连的 95% | CDM 1MiB SEQ 读写、4KiB RND 读写，对同一后端盘直连 vs 经代理 |
| G2 | 读命中显著加速 | CDM 第二轮起的顺序/随机读吞吐明显高于直连（L1 命中为内存速度） |
| G3 | 故障 fail-open | 缓存层任何内部错误自动退化为纯透传，不断会话、不丢数据 |
| G4 | 数据一致性 | 写命令实时失效对应缓存范围；缓存命名空间绑定后端盘指纹，换盘不串数据 |
| G5 | 代码体量可控 | 新增转发/解析/注入 ≤ 2k 行；复用现有 `cache/`、`l2/`、`iscsi/` 编解码 |

### 2.2 非目标（第一版明确不做）

- **预取 / 读放大**：透传代理不能主动发读命令（CmdSN 归属 initiator），缓存只能靠真实流量暖。首次读永远走后端速度。
- 写缓存 / writeback / write-behind：写永远透传（writearound 语义，只失效）。
- MC/S 多连接会话：观测到 `MaxConnections>1` 时退化为纯透传。
- 部分命中代答：只有整个读范围完整命中才介入；否则透传 + 填充。
- 对 v1 终止式代理的继续修补。

## 3. 总体架构

```
 initiator (Windows)                v2 proxy                       backend target
        │                    ┌──────────────────┐                      │
   TCP A│── splice I→T ─────▶│                  │───── TCP B ─────────▶│
        │   (旁观+改写ExpStatSN)│  ┌───────────┐  │                      │
        │                    │  │ 缓存引擎     │  │                      │
        │◀── splice T→I ─────│  │ L1 内存      │  │◀─────────────────────│
        │   (旁观+改写StatSN) │  │ L2 本地 SSD  │  │   (旁观+tee Data-In) │
        │                    │  └─────────────┘  │                      │
        │◀── 命中代答 ───────│  注入器           │   （命中时不经后端）  │
        │   (Data-In+状态)    └──────────────────┘                      │
```

组件：

1. **Splice 转发器**：每条 initiator 连接 1:1 对应一条后端连接，两个方向各一个拷贝循环。默认路径是纯字节搬运（大缓冲、单次 writev），等价 `io.Copy`。
2. **旁路 PDU 解析器（Sniffer）**：在拷贝路径上做 iSCSI 帧切分（48 字节 BHS + DataSegmentLength），**只读**头部字段：opcode、ITT/TTT、CDB、序列号、登录 text key。解析失败 → 该连接永久退化为纯透传（见 §7）。
3. **序列号重写器（Rewriter）**：维护 StatSN 偏移量，双向改写（§5）。
4. **注入器（Injector）**：读全命中时吞掉命令、代答 Data-In 序列（§6.3）。
5. **缓存引擎**：整体复用 v1 的 `internal/iscsicache/cache`（L1/L2、分片、扫描识别、fill worker、范围失效）与 `l2` 存储。

**v1 下线的组件**（代码保留在仓库中作参考，不再接线）：`frontend/`（自研 Target）、`backend/`（自研 Initiator 及连接池）。v2 不需要自研 Initiator——透传意味着后端会话由 initiator 自己建立。

## 4. 连接与登录

### 4.1 监听与拨号

- 代理在本机监听 `127.0.0.1:<port>`，agent 挂载流程把 initiator 指到该地址（现有"下发 → 代理 → 本地建联"链路不变）。
- 每个启用缓存的库对应一个独立监听端口（懒启动，首个连接到来时拨后端）。这样无需解析 TargetName 做路由，映射关系由 agent 静态下发。
- 接受 initiator 连接后立即拨后端（`backend.address`）。**后端不可达 → 拒绝本地连接**，agent 侧按现有逻辑回退直连挂载。

### 4.2 登录透传与旁观

登录 PDU（Login Request/Response，opcode 0x03/0x23）原样透传，CHAP 握手对代理完全透明。旁路解析双方的 text key，记录：

| 键 | 用途 |
|----|------|
| `MaxRecvDataSegmentLength`（双方） | 注入器分段大小 = min(两端宣告值) |
| `HeaderDigest` / `DataDigest` | 若非 None，注入 PDU 需计算 CRC32C（castagnoli），开销可忽略 |
| `MaxConnections` | >1 → 本会话永久纯透传 |
| `ErrorRecoveryLevel` | >0 → 本会话永久纯透传（不做 within-connection 重传记账） |
| `TargetName` / `SessionType` | 仅日志；Discovery 会话全程纯透传 |

登录阶段偏移量恒为 0（登录期间不存在代答），序列号改写自然无操作。

## 5. 序列号偏移重写（核心机制）

### 5.1 原理

iSCSI 会话中 target→initiator 方向存在一条递增的 StatSN 流（SCSI Response、Data-In 末段、R2T、NOP-In、Text/Login Response、Async Message、Reject 都携带并消耗 StatSN，位于 BHS 字节 24-27）。代理每代答一个读，initiator 视角的 StatSN 多消耗一个，而后端不知情。

维护每会话单调计数器 `injected`（代答次数）：

```
T→I 方向：凡携带 StatSN 的 PDU，StatSN ← StatSN + injected
I→T 方向：凡携带 ExpStatSN 的 PDU（BHS 字节 28-31），ExpStatSN ← ExpStatSN - injected
```

CmdSN / ExpCmdSN / MaxCmdSN 属于 initiator 的域，原样透传不改。

这是一个**加法器，不是重排器**：不缓冲、不重排、不跟踪窗口，单点改写。

### 5.2 改写点清单

| 方向 | opcode | 处理 |
|------|--------|------|
| T→I | 0x21 SCSI Response / 0x25 Data-In / 0x31 R2T / 0x20 NOP-In / 0x24 Text Resp / 0x23 Login Resp / 0x32 Async / 0x3F Reject | StatSN += injected（当字段有效时） |
| I→T | 0x01 SCSI Command / 0x05 Data-Out / 0x00 NOP-Out / 0x02 Task Mgmt / 0x04 Text / 0x06 Logout | ExpStatSN -= injected |
| I→T | 0x03 Login Req | 不改（登录期 injected=0） |

注意：NOP-Out 回复 target 的 NOP-In（ping）时携带的 ExpStatSN 同样参与改写；TTT=0xffffffff 的 NOP-Out ping 回显也带 ExpStatSN，统一按规则处理即可。

### 5.3 为什么不需要抑制后端响应

代答的前提是**吞掉的读命令从未发给后端**，因此后端不会产生需要拦截的响应。偏移量自然吸收序号差。这消除了"拦截-丢弃"类逻辑。

## 6. 数据路径

### 6.1 写命令：纯透传 + 失效

I→T 解析到 SCSI Command（0x01）且 CDB 为 WRITE(6/10/12/16)、WRITE SAME、UNMAP：

1. 按 CDB 解出 (LBA, length)，转字节范围；
2. 调用缓存 `Invalidate(off, len)`（v1 已有）；
3. PDU 原样转发（含 ExpStatSN 改写）。

其后的 R2T / Data-Out 全流程对代理只是字节流。

### 6.2 读未命中：透传 + tee 填充

I→T 解析到 READ(10/16)，缓存查询 `ReadCached(off, len)` 未全命中：

1. 在会话的 ITT 表中登记 `itt → {off, len, 收集缓冲}`，PDU 转发；
2. T→I 方向按 ITT 匹配 Data-In（0x25），按 BufOffset 把数据段拷入收集缓冲（旁路拷贝，不影响转发）；
3. 末段 Data-In（F 位 + status）到达且状态 GOOD、字节收齐 → 交给 fill worker **异步**写 L1/L2（复用 v1 `BeginFill`/`StreamFill` 语义）；
4. 命令完成（含错误、超时、ITT 表满）→ 清理登记项。任何异常丢弃半成品，不产生脏缓存（v1 既有约束：CRC 不符丢条目冷启动，不拒服务）。

tee 的拷贝成本仅在读未命中时支付，且是一次内存拷贝，相对网络 RTT 可忽略。

### 6.3 读全命中：代答注入

缓存全命中时：

1. **吞掉该 SCSI Command PDU**（不进后端 socket），从 ITT 表不登记；
2. 注入器按 `min(双方 MRDSL)` 把缓存数据切成 Data-In 序列，字段照 RFC 7143：ITT 原样、TTT=0xffffffff、DataSN 从 0 递增、BufOffset 递增、末段 F 位置位并捎带 status=GOOD（S 位 + Status，与 MS target 习惯一致：不单独发 SCSI Response）；
3. StatSN = 当前 T→I 方向下一个应发值（即"后端最后所发 StatSN + injected + 1"），发完后 `injected++`；
4. 若协商了 digest，对注入 PDU 计算 CRC32C；
5. **注入过程任何一步失败 → 放弃代答，把原命令补发给后端**（该命令按未命中处理）。永远有退路。

内存分配：注入缓冲走 `sync.Pool`，分段发送不等 ACK（initiator 的接收窗口由 TCP 流控保证）。

### 6.4 扫描识别

复用 v1 的 scan detector：顺序大流（默认连续 32 个块）不填充缓存，避免 CDM 这类一次性大顺序读冲刷热数据。tee 路径照常观察，只是不落盘。

## 7. 失败与降级矩阵

| 故障 | 行为 |
|------|------|
| 后端拨号失败 | 拒绝本地连接；agent 回退直连（现有逻辑） |
| PDU 解析失败（帧错乱/未知形状） | 该连接永久纯透传，记 warn 日志 |
| 协商出 MC/S、ERL>0、 digest 之外的不支持特性 | 该会话永久纯透传 |
| 缓存查询/填充内部错误 | 该命令按未命中透传；连续错误超阈值 → 缓存整体下线为纯透传 |
| 注入中途失败 | 补发原命令给后端，按未命中完成 |
| L2 文件损坏/CRC 不符 | 丢条目冷启动（v1 既有约束），不拒服务 |
| 会话断连（任一方向 EOF/错误） | 双向关闭，清 ITT 表与偏移状态；**缓存内容保留**（按盘指纹命名空间），重连继续用 |
| 代理进程整体崩溃 | initiator 会话断开，Windows 磁盘报错；agent  watchdog 重启代理或回退直连 |

原则：**透传是默认态，缓存是增量**。任何一步拿不准，就退到透传。

## 8. 性能模型

| 路径 | 成本 | 预期 |
|------|------|------|
| 写 / 未命中读 | 双向各一次用户态拷贝 + 每 PDU 48 字节头部解析（无堆分配） | ≥95% 直连（瓶颈是 socket 拷贝，不是协议） |
| 命中读（L1） | 内存拷贝 + 注入 PDU 发送 | 内存速度，延迟 ~µs 级，显著高于网络直连 |
| 命中读（L2） | 本地 SSD 读 | 本地盘速度 |
| 冷读 | 后端速度 + 一次 tee 拷贝 | ≈ 直连 |

splice 循环的调优项（实现期注意）：

- 读缓冲 ≥ 1MiB，配合 `net.Buffers` writev 一次出多个 PDU；
- `TCP_NODELAY` 两侧都开；
- socket 缓冲按 v1 经验 4MiB；
- 解析器零分配：头部字段直读字节，text key 仅登录期解析。

## 9. 一致性

- **写失效**：见 §6.1，观察 WRITE/WRITE SAME/UNMAP 即失效。失效在命令**转发前**执行，保证后续读不会取到旧数据。
- **盘指纹绑定**：沿用 v1 约束——缓存命名空间 = (server, allocation, 后端盘指纹)。代理在首个连接建立后通过旁路观察 INQUIRY（VPD 0x83）响应提取序列号/WWID；与登记指纹不符 → 清空命名空间重来。**指纹未确认前会话纯透传**（不填充也不代答）。
- **多客户端**：同一后端盘被多个客户端挂载时，本代理只看到本机的写。跨客户端写一致性由服务端分配语义保证（现行约束：一个 allocation 同一时间只挂一个客户端），不在 v2 范围。

## 10. 配置

在现有 agent 配置基础上平移，用户可见项不变：

```yaml
# 独立入口 vault-iscsi-cache -config v2.yaml（保留 -check 自检）
listen_addr: "127.0.0.1:3260"        # 每库一个端口由 agent 分配
backend:
  address: "172.18.28.200:3260"
  dial_timeout: "10s"
cache:
  block_size: 65536
  sector_size: 4096
  identity_binding: true
  l1: { size_bytes: 536870912 }
  l2: { enabled: true, path: "...", size_bytes: 4294967296 }
  scan_detection: { enabled: true, threshold: 32 }
  fill: { workers: 4, queue_depth: 256 }
```

约束沿用 v1：L1 ∈ [64MiB, 64GiB]，L2 = 0 或 ∈ [256MiB, 1TiB]。配置里的数是**总上限**，在所有启用缓存的库之间按**库容量占比**分摊（单库不超过自身容量），挂载时固定、运行中不重算；设置页会按本机已知的库容量给出推荐值（L2 取库容量的 5%、L1 取 L2 的 1/8）。L2 目录改动下次挂载生效。

## 11. 模块布局

```
internal/iscsicache/           # v1 保留参考，不再接线
  frontend/  backend/          # v1 终止式实现（冻结）
  cache/  l2/  iscsi/          # 复用
internal/iscsiproxy/           # v2 新增（独立包，不引用 v1 frontend/backend）
  splice.go                    # 双向拷贝循环 + 半关闭处理
  frame.go                     # PDU 帧切分（流式，零分配）
  observe.go                   # 旁路解析：登录键、CDB 登记、ITT 表
  rewrite.go                   # StatSN/ExpStatSN 偏移改写
  inject.go                    # 命中代答（Data-In 序列 + digest）
  session.go                   # 会话状态：协商结果、injected 偏移、指纹确认
  proxy.go                     # 监听器、后端拨号、降级开关
cmd/vault-iscsi-cache/         # 切到 v2 实现（-config/-check 不变）
```

agent 集成改动很小：`cacheproxy.go` 中门户对象换成 v2 proxy；mount 流程把 initiator 的 TargetName 指到**后端真实 IQN**（透传后 initiator 直接登录后端目标）；`cache_enabled` 回退直连逻辑不变。

## 12. 测试计划

1. **单元测试（表驱动）**：`rewrite.go` 用录制的 PDU 序列（含 R2T、NOP-In、多 Data-In）验证偏移改写的逐字节正确性；`inject.go` 验证分段/digest/residual。
2. **集成测试（内存后端）**：`net.Pipe` 两端分别接自研假 target（可用 v1 `backend/` 测试桩）与真实 `backend.Dial` initiator，跑读写一致性 + 命中代答 + 写失效。
3. **真机验证（唯一验收标准）**：Windows MS initiator 挂载 + CDM：
   - 透传组（缓存关）：读写 ≥ 直连 95%；
   - 缓存组：第二轮起读显著高于直连，写 ≈ 直连；
   - 挂载/卸载/断网重连/换盘指纹场景。
4. 明确禁止：以自研 bench 客户端的数值作为验收依据（v1 教训）。

## 13. 迁移步骤

1. 新增 `internal/iscsiproxy/` 与独立入口，v1 代码原样保留；
2. 单元 + 集成测试达标后，agent 灰度切到 v2（配置开关 `cache_engine: v2`，默认先 v1）；
3. 真机 CDM 验收通过后，v2 设为默认；v1 `frontend/`、`backend/` 标记 deprecated，观察一个版本周期后删除；
4. 文档更新：`iscsi_cache_proxy_design.md` 头部标注"已被 v2 取代"。

## 14. 风险清单

| 风险 | 缓解 |
|------|------|
| StatSN 偏移 off-by-one → initiator 断会话 | 表驱动测试覆盖全部带序号 opcode；注入路径只占序号流极小比例，错了可观测可回退 |
| MS initiator 的非标行为（如 unexpected NOP/Async） | 未知 opcode 一律透传 + 不记账；解析失败即纯透传 |
| 指纹确认前的窗口期被填了错盘数据 | 指纹未确认 = 纯透传，不进缓存 |
| tee 收集缓冲被大读撑爆内存 | 只收集 ≤ 阈值（如 4MiB）的读；超限丢弃登记项 |
| digest 协商开启时注入性能 | CRC32C 硬件指令（SSE4.2），1MiB 约几十 µs，可忽略 |
