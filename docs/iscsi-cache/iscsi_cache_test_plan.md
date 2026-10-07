# iSCSI-Cache 自动化对照测试方案

## 一、方案定位

**这是一套以"对照测试"为核心的自动化测试体系。** 它的核心思想是：准备一个不经过任何代理的"正常 iSCSI 地址"（一个真实、干净的 iSCSI Target），把同一条 SCSI 命令同时发给它和 iSCSI-Cache 代理，逐字段比对双方返回结果。两者输出一致，则代理在协议层面正确；不一致，则直接定位偏差。

这套方法的价值在于**不需要理解业务的 IO 语义**——游戏、操作系统、数据库用什么模式读都不重要，重要的是代理对每一条 SCSI 命令的响应与真实 Target 是否等价。

### 1.1 什么能靠对照测出来

| 能测出 | 原因 |
|---|---|
| 缓存命中后数据是否与后端一致 | READ 响应逐字节比对 |
| 写穿 / 写回后读到的数据是否正确 | 先写真实 Target、再读代理，比结果 |
| 状态机、时序、DataSN 是否合规 | 响应字段逐项比对 |
| SCSI Status / Sense Data / Residual Count 是否一致 | 三个字段显式比对 |
| 多 Session 共享 LUN 是否脏读 | 双端并发同一 LUN，同时比对 |
| 崩溃恢复后数据是否一致 | 重启后两端各自读，比对 |

### 1.2 什么对照测不出来

| 测不出 | 怎么办 |
|---|---|
| 性能（延迟、吞吐） | 独立做基线压测，记 p50/p99 |
| 掉电后数据完整性 | dm-log-writes / 虚拟机强断电 |
| 两个实现犯同一个错 | 形式化验证补这一层（TLA+ 建模） |
| RFC 里规范本身就含糊的地方 | 人工定规则，写进字段白名单 |

## 二、核心思路：双轨对照

**对照测试的关键在区分 READ 和 WRITE。** 它们性质不同，不能同一套比法。

### 2.1 READ：直比

READ 是幂等的——同一个 LBA 读多少次结果都一样。所以可以直接比：

```
构造 READ(LBA, len)
   ├─→ 发往 真实 Target A   → 得到 Data_A, Status_A, Sense_A, Residual_A
   └─→ 发往 代理 B          → 得到 Data_B, Status_B, Sense_B, Residual_B

比对：Data_A == Data_B
      Status_A == Status_B
      Sense_A == Sense_B
      Residual_A == Residual_B
```

任何一项不一致 → 失败，记录原始 PDU。

### 2.2 WRITE：终点比

WRITE 不是幂等的——写一次后端数据就变了，第二次写同一个 LBA 结果不同，没法直接比。所以改为**同一起点、并行双写、终点校验**：

```
构造黄金数据块 G（固定 seed 的伪随机数据）

真实 Target A  ← WRITE(LBA, G) ─→  回 GOOD
代理背后 LUN B  ← WRITE(LBA, G) ─→  回 GOOD
         （两条命令可以乱序，但必须同一时刻发、同一数据）

等待两端都回 GOOD 后：

真实 Target A  ← READ(LBA, G.len) ─→ Data_A
代理背后 LUN B  ← READ(LBA, G.len) ─→ Data_B

比对 Data_A == Data_B == G
```

**为什么这样能兜住风险**：真实 Target 和代理背后的 LUN 是**两块独立的物理盘**（或不同 LUN），它们的写入路径、缓存、固件完全不同。两端同时写同一个值、读出来又都一致，说明代理**没有改数据、没有改状态、没有把脏数据写回去**。

**这一步是这套方案的精髓**——它恰好避开了"两个实现犯同一个错"的风险：共享的只有 SCSI 规范，不共享代码，不共享缓存，不共享固件。

### 2.3 混合场景：因果序

真实负载不会是纯读或纯写，而是读写交替。关键约束是**命令之间有时序因果**：

```
合法序列（保序）：
  WRITE LBA 0x1000 "A"
  READ  LBA 0x1000        → 期望读到 "A"
  WRITE LBA 0x1000 "B"
  READ  LBA 0x1000        → 期望读到 "B"

非法序列（乱序）：
  WRITE LBA 0x1000 "A"
  WRITE LBA 0x1000 "B"   ← 必须先处理完 A
  READ  LBA 0x1000
```

**所以对照测试必须严格顺序执行**：每个 LUN 一条命令流水线，前一条完成才发下一条。双端都按同一顺序发，发完按同一顺序收。

## 三、测试架构

### 3.1 拓扑

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
│                      │                                       │
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

### 3.2 三段式工作流

**阶段一：环境准备**
1. 拉起真实 Target（tgt 或 LIO），导出 LUN A
2. 拉起 iSCSI-Cache 代理，后端指向 LUN A（或另一块等价 LUN B）
3. 代理前端导出 LUN B'
4. 等待两端均进入"就绪"状态
5. 记录两端容量、块大小、Serial Number 等元信息

**阶段二：测试执行**
1. Workload Generator 按覆盖矩阵生成命令序列
2. Sequencer 严格顺序分发到双端
3. Verifier 逐条比对结果

**阶段三：清理与判定**
1. 登出两端会话
2. 汇总结果，生成报告
3. 失败则保留现场（PDU trace、core dump、journal 文件）

## 四、字段比对规则

### 4.1 逐字段清单

| 字段 | 比对方式 | 说明 |
|---|---|---|
| Data payload | 逐字节 memcmp | 核心，任何不一致即 FAIL |
| SCSI Status | 精确比对 | GOOD / CHECK CONDITION / BUSY 等 |
| Sense Data | 逐字节比对 | INQUIRY VPD、错误码、ASC/ASCQ |
| Residual Count | 精确比对 | 溢出/欠载字节数 |

### 4.2 字段白名单（跳过比对）

**这些字段两端必然不同，跳过是为了不让假阳性掩盖真问题：**

| 字段 | 原因 |
|---|---|
| Serial Number | 代理返回的是自己的 SN，不是后端的 |
| Vendor / Product / Revision | 代理可自定义厂商字符串 |
| Target Name / Target Portal | 地址信息本就不同 |
| Timestamps | 两端时间必然不同 |
| 连接相关字段 | CID、会话标识本就不同 |

### 4.3 结果判定

```
PASS   ：所有比对字段一致
FAIL   ：任一核心字段不一致（数据、Status、Sense、Residual）
FLAKY  ：结果在合法区间内抖动（如延迟波动），不影响正确性
```

## 五、覆盖矩阵

### 5.1 SCSI 命令矩阵

**每条命令按三个维度覆盖：正常路径 / 异常路径 / 边界。**

| 命令 | 正常路径 | 异常路径 | 边界 |
|---|---|---|---|
| INQUIRY | Standard / VPD 各页 | 非法 EVPD 页号 | Serial Number 唯一性 |
| REPORT LUNS | 单 LUN / 多 LUN | — | LUN 类型字段、寻址格式 |
| READ CAPACITY(10) | 返回容量 | — | 4GB / 2TB / 4TB 边界 |
| SERVICE ACTION IN / READ CAPACITY(16) | 64 位 LBA + BlockDescriptor | — | 逻辑块长度字段 |
| READ(10) | 4KB / 64KB / 1MB | LBA 越界 | 保护信息检查 |
| READ(16) | 64 位 LBA 范围 | LBA 越界 | 传输长度非 512 倍数 |
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

### 5.2 缓存状态矩阵

**同一组命令必须在每种缓存状态下各跑一遍：**

| 状态 | 触发方式 | 验证重点 |
|---|---|---|
| cold | 清空缓存后重启代理 | 首次读走后端，数据正确 |
| warm_l1 | 预热后，数据全在 L1 | L1 命中返回正确 |
| warm_l2 | 清空 L1，数据在 L2 | L2 命中返回正确 |
| dirty_writeback | 写穿后未刷盘 | 读必须返回新值，崩溃恢复后正确 |

### 5.3 场景矩阵

| 场景 | 配置 | 验证重点 |
|---|---|---|
| 单 Session 单连接 | 标准 | 基础正确性 |
| 单 Session 多连接 | MC/S（如 Initiator 支持） | 多连接命令分发 |
| 多 Session 共享 LUN | 2-4 个 Session | **缓存共享无脏读** |
| MPIO | 同主机两条路径 | 两条路径看到同一份数据 |
| 并发读写 | QD 32，读写混合 | 读写顺序与数据一致性 |
| 大数据流 | 顺序读 1GB+ | 顺序预取正确、无污染 |

## 六、黄金数据策略

### 6.1 数据生成

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

### 6.2 写入日志

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

**写入日志是校验的基准**：校验时按日志逐块判定预期值，未确认写入（超时未回 GOOD）读到新值或旧值都合法。

### 6.3 校验流程

```
对每个 4KB 块：
  1. 查写入日志，找到覆盖该块的最新已确认写入 → 预期值 = 日志中的数据
  2. 无已确认写入 → 预期值 = 初始黄金数据
  3. 读真实 Target 的该块 → Data_A
  4. 读代理背后 LUN 的该块 → Data_B
  5. 比对 Data_A == Data_B == 预期值
```

## 七、故障注入

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

## 八、并发与压力

### 8.1 并发模型

```
- 每个 LUN 一条严格顺序命令流水线（保证因果序）
- 多个 LUN 之间可以并发（验证 LUN 间隔离）
- 多 Session 共享同一 LUN 时，各 Session 独立流水线，但共享缓存
```

### 8.2 压力参数

| 参数 | 范围 |
|---|---|
| 队列深度 | 1 / 8 / 32 / 128 / 256 |
| 读写比例 | 100/0 / 75/25 / 50/50 / 25/75 / 0/100 |
| 数据块大小 | 4KB / 64KB / 256KB / 1MB |
| 并发 LUN 数 | 1 / 4 / 16 |
| 并发 Session 数 | 1 / 2 / 4 / 8 |
| 持续时间 | 1 分钟 / 10 分钟 / 1 小时 / 24 小时 |

### 8.3 长稳测试

```
- 24 小时持续对照测试
- 每小时全量校验一次数据一致性
- 监控：RSS 内存、fd 数量、goroutine 数、L1/L2 命中率
- 达标：零数据不一致 + 无内存泄漏 + fd 不增长
```

## 九、崩溃与掉电测试

### 9.1 进程崩溃（kill -9）

```
1. 持续压测中
2. kill -9 iscsi-cache
3. 重启代理
4. 全量校验数据一致性
5. 检查 Journal replay 是否 100% 成功
```

### 9.2 真掉电模拟（dm-log-writes）

**`kill -9` 只模拟进程崩溃，page cache 仍在，不能代表掉电。** 真掉电必须用：

```
1. 把 L2 / Journal 放在 dm-log-writes 设备上
2. 持续写入
3. 在随机时间点记录"掉电点"
4. 回滚到掉电点，模拟断电瞬间状态
5. 重新挂载，启动代理
6. 校验 Journal replay 后的数据一致性
```

### 9.3 虚拟机掉电

```
1. 把 L2 / Journal 放在 qcow2
2. 持续压测
3. virsh destroy <vm>（强制断电，不等 guest 响应）
4. 重新启动 vm
5. 校验数据一致性
```

## 十、互操作性

| Initiator | 验证项 |
|---|---|
| Linux open-iscsi | 发现 / 登录 / 登出、多 Session、multipath、读一致性 |
| Windows iSCSI Initiator | 发现 / 连接 / 断开、MPIO、磁盘签名 |
| ESXi | VMFS 创建、虚拟机运行、存储 vMotion、PR 冲突 |

**互操作测试也走对照**：同一 Initiator 分别连真实 Target 和代理，执行相同操作（格式化、写入文件、计算哈希），比对最终数据。

## 十一、模糊测试

- **状态化 fuzzing**：Boofuzz 维护 iSCSI Login 状态机，变异后续 PDU
- **覆盖率引导**：AFL++ / libFuzzer 针对 PDU 解析函数
- **磁盘变异**：对已保存的合法 PDU 语料做位翻转、长度变异、opcode 替换
- **达标**：72 小时无崩溃、无断言失败、无内存越界

## 十二、自动化执行

### 12.1 一键运行

```bash
# 冒烟测试（约 15 分钟，本地）
autopilot run --suite smoke

# 全量测试（约 6-10 小时）
autopilot run --suite full

# 重放某次历史运行
autopilot run --replay manifest-2024XXXX-XXXX.json

# 只跑对照层
autopilot run --layer l2 --matrix full

# 只跑某个命令
autopilot run --command READ_16 --coverage full
```

### 12.2 配置

```yaml
test:
  control:
    type: "local"           # local / docker / vm
    cleanup: true           # 测试结束是否销毁环境
    preserve_on_failure: true

  targets:
    reference:              # 真实 Target（对照端）
      address: "iscsi://10.0.0.10:3260/iqn.2024-01.ref:lun1"
      auth: null
    agent:                  # iSCSI-Cache 代理（被测端）
      address: "iscsi://127.0.0.1:3260/iqn.2024-01.cache:lun1"
      auth: null

  workloads:
    - name: "read_sequential"
      type: "read"
      lba_start: 0
      lba_end: 0x100000     # 覆盖 512MB
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
      fua_ratio: 0.1        # 10% 的写带 FUA

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

### 12.3 声明式用例

```yaml
# cases/read_16_boundary.yaml
name: "READ(16) 4TB 边界"
layer: l2
priority: P0
tags: [boundary, 64bit-lba]
matrix:
  cache_state: [cold, warm_l1, warm_l2]
  block_size: [512, 4096, 65536]
steps:
  - command: READ_16
    lba: 0xFFFFFFFFFFFFFFFF  # 最大 64 位 LBA
    length: 1
    expect:
      scsi_status: GOOD
      data_match: reference
  - command: READ_16
    lba: 0x100000000  # 超过 4TB
    length: 8
    expect:
      scsi_status: GOOD
```

### 12.4 报告格式

```xml
<?xml version="1.0" encoding="UTF-8"?>
<testsuites>
  <testsuite name="iSCSI-Cache-Contrast-Test" tests="1247" failures="0" errors="0" time="342.5">
    <testcase classname="READ_16" name="read_16_boundary_cold" time="0.12"/>
    <testcase classname="WRITE_10" name="write_10_fua_warm_l1" time="0.08"/>
    <testcase classname="CACHE_DIRTY" name="dirty_readback" time="0.15"/>
    <!-- 失败用例附带详情 -->
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

## 十三、发布闸门

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

## 十四、CI 集成

### 14.1 触发策略

| 触发 | 套件 | 耗时 | 说明 |
|---|---|---|---|
| PR 提交 | smoke | ~15 分钟 | 核心命令对照 + 冷启动 |
| 合并到 main | full | ~6 小时 | 全矩阵 + 混沌 + 互操作 |
| 每日定时 | full + 模糊 8h | ~14 小时 | 深度验证 |
| 发布前 | full + 模糊 72h | ~72 小时 | 发布闸门 |

### 14.2 GitHub Actions 示例

```yaml
name: iSCSI-Cache Test

on:
  pull_request:
    branches: [main]
  push:
    branches: [main]
  schedule:
    - cron: '0 2 * * *'   # 每日 02:00

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
    timeout-minutes: 360   # 6 小时上限
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
        run: ./bin/autopilot run --layer l7
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

## 十五、已知限制

1. **对照测试依赖真实 Target 的正确性**。如果真实 Target 本身有 bug，对照结果会把 bug 当成"正确"。缓解：可选多个不同实现的 Target（tgt + LIO）交叉对照。
2. **性能不在对照范围内**。对照只验证正确性，延迟/吞吐需独立压测。
3. **掉电测试需要特殊环境**（dm-log-writes 或虚拟机），无法在普通 CI Runner 上跑。
4. **4TB+ 边界、64 位 LBA 等场景需要大容量 LUN**，测试环境需预留空间。
5. **互操作测试需要真实 Initiator**，Windows / ESXi 需物理机或虚拟化环境。

## 十六、验收标准

| 项目 | 标准 |
|---|---|
| 命令矩阵覆盖 | 100%（表内所有命令 × 三种路径 × 三种缓存状态） |
| 对照一致性 | 100% 通过（零 FAIL） |
| 混沌测试 | 1000 轮，零数据不一致 |
| 模糊测试 | 72 小时无崩溃 |
| 内存泄漏 | ASAN / valgrind 无报告 |
| fd 泄漏 | 测试前后 fd 数差 ≤ 2 |
| 掉电恢复 | dm-log-writes 100 轮，零数据损坏 |
| 互操作 | Linux / Windows / ESXi 全流程通过 |
| 长稳测试 | 24 小时，零数据不一致、无泄漏 |
