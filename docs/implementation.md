# Vault 存储管理系统 — 实施文档

> 本文基于 [design.md](./design.md) 的原始设计意图，结合 **Windows Server 与 Linux 两个平台**的实际能力边界（见 [windows-iscsi-vhdx-api.md](./windows-iscsi-vhdx-api.md) 与 §2.4 / §5.14）展开，
> 目标是给出**可直接进入开发的完整实施方案**，并对原设计做完整性、可行性与缺陷评审。
>
> 阅读顺序建议：第 1~2 章（边界）→ 第 13 章（评审结论，最重要）→ 第 3~12 章（方案细节）。
>
> **平台说明**：虚拟磁盘与 iSCSI 已抽象为可替换后端 —— Windows 用 VHDX + WinTarget，Linux 用 **LVM thin + LIO**。
> 两平台**并列共存**，上层（存储库 / 分配 / 租约 / 对账 / 上传）代码完全一致。

---

## 目录

1. [文档说明](#1-文档说明)
2. [能力边界（可行性基线）](#2-windows-侧能力边界可行性基线)（§2.1-2.3 Windows / §2.4 **Linux**）
3. [总体架构](#3-总体架构)
4. [领域模型与数据模型](#4-领域模型与数据模型)
5. [核心技术方案](#5-核心技术方案)（§5.14 **Linux：LVM thin + LIO**）
6. [服务端实现](#6-服务端实现)
7. [客户端实现](#7-客户端实现)
8. [管理页与前端工程](#8-管理页与前端工程)
9. [安全设计](#9-安全设计)
10. [选型决策](#10-选型决策)
11. [实施路线图](#11-实施路线图)
12. [构建与发布](#12-构建与发布)
13. [设计评审：不足与改进建议](#13-设计评审不足与改进建议)
14. [待实测清单与风险登记册](#14-待实测清单与风险登记册)（§14.1 Windows T1-T19 / **Linux T20-T35**）
15. [附录](#15-附录)

---

## 1. 文档说明

### 1.1 范围

| 包含 | 不包含 |
| --- | --- |
| 系统架构、领域模型、数据模型 | 具体 UI 视觉稿 |
| VHDX / iSCSI / 挂载 / 上传的技术方案 | 游戏内容本身的合法性判定 |
| **平台后端抽象：Windows（VHDX + WinTarget）与 Linux（LVM thin + LIO）并列** | 商业化计费体系 |
| 服务端、客户端、管理页的模块划分与接口 | 容器 / 编排（K8s、CSI）形态的部署 |
| 安全模型、并发模型、故障恢复 | |

### 1.2 关键前提与决策（需确认）

| # | 决策点 | 结论 | 影响 / 备注 |
| --- | --- | --- | --- |
| D1 | 服务端平台 | **Windows Server 与 Linux 并列共存**（同一代码库，分平台构建产物；上层不感知差异） | 见 §2.4 与 §5.14：Linux 侧虚拟磁盘用 **LVM thin LV**、iSCSI 用 **LIO**；平台由构建产物决定，`platform.kind` 仅做一致性校验（不符直接拒绝启动） |
| D2 | 数据库 | **SQLite(WAL) 与 MySQL 双驱动**，默认 SQLite（见 10.2） | 已确认放弃 etcd 方案 |
| D3 | 客户端外壳 | **Electron**（已定），管理页与客户端**共用同一套 UI 组件** | 见 8.1；Wails 方案作废 |
| D4 | 服务端是否用 cgo | **不使用**（已核查：本项目零处需要，见 10.4） | 统一 `CGO_ENABLED=0`；注意勿误引入 `mattn/go-sqlite3` |
| D5 | 卷文件系统 | **仅 NTFS**（已定） | **块克隆不可用** → 复制 VHDX 为全量物理拷贝（见 5.9），需重新设计省空间策略 |
| D6 | 存储库容量配额 | **应用层记账**，且**母盘占用由 owner 独担** | NTFS 有原生配额，但本系统配额是"按库/按用户"口径，仍走应用层 |
| D7 | 母盘保护方式 | **应用层状态标记**（不强制只读挂载） | 因母盘需支持更新；需辅以指纹校验兜底（见 5.4） |
| D8 | 在线状态权威来源 | **客户端心跳 / 租约**（design.md 已补充"心跳"章节） | 服务端无会话枚举 API，唯一可行路径（见 5.7 / 5.8） |
| D9 | 多服务端 · 版本判定 | **兼容区间**（`client_compat.min/max`），**禁止版本相等**；区间写在**服务端 `config.yaml`**，支持**关闭检查**（`enabled: false`） | 注意：关闭检查仅关"客户端版本区间"，`api_version` 协议校验不可关闭。见 3.4.2 |
| D10 | 多服务端 · 更新源 | **主服务端（显式指定）+ 失败回退**；无回退会产生"永远升不上去"的死锁 | 见 3.4.3 |
| D11 | 多服务端 · 更新包签名 | **统一发布方签名**，客户端编译期内置公钥；服务端仅做 mirror | 若每台各自签名，攻击面随服务端数量放大。见 3.4.4 |
| D12 | 多服务端 · 分组与来源 | 分组数据仍存各服务端；客户端**加服务端来源标识**，**同名分组可聚合展示** | 保留跨设备同步，同时满足按用途归类。见 3.4.6 |
| D13 | **服务端职责** | **只提供 REST API**，不再承载任何 Web 静态资源 | 管理界面与用户界面合并进客户端。见 8.1 |
| D14 | **前端形态** | **单一客户端应用**（Electron），管理功能作为其中的「管理模块」 | 取消独立管理页 SPA；组件级复用改为同一应用内复用。见 8.1 |
| D15 | **超级管理员** | 账号**存放在数据库**，由初始化流程创建；配置文件仅保留 `super_admin_enabled` 开关 | 避免"口令哈希写在配置文件里"这一反模式。见 9.1 |
| D16 | **初始化方式** | 新增**类路由器初始化向导**：无超级管理员且无数据时开放；一次性、并发安全 | 见 3.5 |

### 1.3 术语

| 术语 | 含义 |
| --- | --- |
| **母盘 / Parent VHDX** | 包含完整数据的基准 VHDX，供派生差异盘 |
| **差异盘 / Diff VHDX** | 仅记录相对母盘改动的 VHDX，读写时叠加母盘 |
| **存储库 / Repository** | 面向用户的概念，封装 VHDX + iSCSI + 授权 + 密钥 |
| **共享库** | 母盘 + 差异盘模式，一个母盘派发给多个用户 |
| **独享库** | 单 VHDX + 单 iSCSI 目标，等价于"裸盘直通" |
| **租约 / Lease** | 客户端与服务端的连接状态契约，是"在线状态"的权威来源 |
| **发布 / Publish** | 把 VHDX 通过 iSCSI 暴露出去的动作 |
| **对账 / Reconcile** | 比对 DB 期望状态与 Windows 实际状态并修正 |

---

## 2. Windows 侧能力边界（可行性基线）

这一章是整套设计的**硬约束**。任何绕过这些边界的方案都会在实现阶段返工。

### 2.1 能力矩阵

| 需求（来自 design.md） | 可行性 | 实现手段 | 关键限制 |
| --- | --- | --- | --- |
| 创建 VHDX（含差异盘） | ✅ | VirtDisk API（`go-winio/vhd`） | 提权；差异盘 `MaximumSize=0` 且禁用 `SourcePath` |
| 从目录创建（拷贝/剪切内容） | ✅ | 建盘 → 挂载 → 格式化 → 拷入 → 分离 | 发布前必须分离；VHDX 不能放压缩/稀疏/事务目录 |
| 查询/删除 VHDX | ✅ | `Get-DiskImage` / `os.Remove` | 删除前必须未挂载且未被 iSCSI 占用 |
| 复制 VHDX | ✅ 但**费空间** | `CopyFileEx` / `robocopy` 全量物理拷贝 | **NTFS 无块克隆**（`FSCTL_DUPLICATE_EXTENTS_TO_FILE` 仅 ReFS 支持）；耗时与源盘同量级 |
| 母盘维护（更新内容） | ✅ | 清理差异盘 → 挂载母盘读写 → 更新 → 卸载 | **必须先把差异盘子盘全部清理**，否则新老子盘数据不一致（见 5.4） |
| 母盘临时只读共享 | ✅ | 母盘发布为只读 iSCSI target | 与"派生差异盘"互斥；状态需落盘，重启后由 Reconciler 收敛 |
| 从 VHDX 建 iSCSI | ✅ | `Import-IscsiVirtualDisk` + `New-IscsiServerTarget` + `Add-IscsiVirtualDiskTargetMapping` | 虚拟盘名/路径唯一；需先分离 |
| iSCSI 鉴权 | ⚠️ **部分** | `-EnableChap -Chap <user,secret>` | **CHAP 是"每个 target 一组账号"，无法在同一 target 内区分多用户**（见 5.3） |
| iSCSI 授权编辑 | ✅ | `Set-IscsiServerTarget -InitiatorIds`（IQN/IP/MAC/DNS 白名单） | **全量替换语义**，无增量接口 |
| 查询/管理 iSCSI 配置 | ✅ | `Get-IscsiServerTarget` / `Get-IscsiVirtualDisk` | 无映射查询 cmdlet，需侧推 |
| **查询在线 iSCSI 连接** | ❌ **不可行** | 只能用 `netstat :3260` / 事件日志 / 客户端上报 | 服务端无会话枚举 API |
| **服务端强制踢客户端** | ❌ **不可行** | 只能 `Set-IscsiServerTarget -Enable $false` 等间接手段 | 无按会话登出 API；无法只踢单个 initiator |
| iSCSI 读写监控（按会话） | ❌ **不可行** | 只能整机/整盘计数器 | 无 per-session IO 统计 |
| 客户端挂载 iSCSI | ✅ | `msiscsi` + `Connect-IscsiTarget` + `Set-Disk -IsOffline $false` | 会话上有设备在线时无法断开 |
| 挂载为盘符 / 目录 | ✅ | `Add-PartitionAccessPath` / `SetVolumeMountPoint` | 目录挂载点要求目标为空目录、同级 NTFS |
| 母盘写入保护 | ⚠️ **仅应用层** | 状态标记 + 母盘指纹校验 + 流程约束 | Windows 不会阻止误写；需靠 Reconciler 与指纹比对兜底（见 5.4） |

### 2.2 由边界推导出的四条架构性原则

> **原则 1：连接状态的权威来源是"客户端租约/心跳"，不是 Windows iSCSI 服务。**
> 因为服务端查不到会话，Vault 必须自己维护 `在线/离线`：客户端挂载后持续心跳，服务端用租约超时判离线，`netstat` 只做交叉校验。

> **原则 2：所有"踢下线"都是"停用目标"，不是"断开会话"。**
> 语义上要降级为"拒绝服务 + 期望客户端自行断开"，客户端收到服务端的 `revoke` 指令后主动 `Disconnect-IscsiTarget`。**服务端做不到单方面断开。**

> **原则 3：Windows 实际状态是"最终真相"，DB 是"期望状态"。**
> 因为 PowerShell 操作可能部分成功、进程可能崩溃，必须有 Reconciler 定期对账，DB 不能当作事实来源。

> **原则 4：在 NTFS 上，"省空间"只能靠差异盘，不能靠文件系统。**
> 块克隆是 ReFS 独占能力，NTFS 没有等价的本地 CoW 复制。因此**复制 VHDX 必然是物理全量拷贝**，"省空间"必须通过**产品语义**引导到差异盘，而不是期待文件系统魔法。

### 2.3 服务端运行前置条件

```powershell
# 一次性初始化（安装脚本执行）
Install-WindowsFeature FS-iSCSITarget-Server -IncludeManagementTools
Set-Service WinTarget -StartupType Automatic
Start-Service WinTarget

# 防火墙
New-NetFirewallRule -DisplayName 'Vault iSCSI' -Direction Inbound -Protocol TCP -LocalPort 3260 -Action Allow

# 建议：规划专用 iSCSI 网段，并确保 Vault 服务账户在 Administrators 组
```

### 2.4 Linux 侧能力边界（LVM thin + LIO）

Linux 后端是 Windows 后端的**并列等价实现**，同一套上层代码（存储库 / 分配 / 租约 / 对账 / 上传）不改逻辑。
两平台的能力对照见下表；**关键差异是 Linux 侧没有 §2.1 里的那两条硬伤**（会话不可枚举、不可强踢）。

| 需求 | Windows 实现 | Linux 实现 | 关键限制 / 备注 |
| --- | --- | --- | --- |
| 创建虚拟磁盘 | VHDX（VirtDisk API） | `lvcreate --type thin -V <size>B -T <vg>/<pool>` | 必须显式带 **`B`** 单位，否则被当作 MB 解析 |
| 创建差异盘 | VHDX 差异盘（`MaximumSize=0`） | `lvcreate -s`（**thin 快照**） | 快照命令**绝不能带 `-L/-V`**；thin 快照天然省空间 |
| 删除虚拟磁盘 | 卸载 + `os.Remove` | `lvremove` | 删除前**必须先下线 LUN** 并 `lvchange -an`；thin LV **无法**用 `vgcfgrestore` 恢复 |
| 查询容量 | `Get-Item` / `Get-DiskImage` | `lvs` / `vgs` / `thin_ls` | 配额统计**不能用** `lvs -o data_percent` 相加（共享块会重复计数），须用 `thin_ls` |
| 发布为 iSCSI | `Import-IscsiVirtualDisk` + target + mapping | LIO：`iblock_0` backstore + TPG `tpgt_1` + `lun` 符号链接 | 直接写 `/sys/kernel/config/target/`，**无需 targetcli / 无需服务进程** |
| iSCSI 鉴权 | `-EnableChap -Chap <user,secret>` | TPG `auth` + ACL `chap` | CHAP 密钥**内核硬限制 12–16 字节纯 ASCII**（Base64 后的 16 字符正好落在边界内） |
| 只读发布（母盘临时共享） | iSCSI 只读映射 | per-ACL `write_protect=1` | 只读是**按 ACL** 而非按 LUN，务必确认 ACL 生效 |
| 授权（initiator 白名单） | `Set-IscsiServerTarget -InitiatorIds`（全量替换） | ACL 目录即白名单（增删目录 = 增删授权） | 语义一致：一个分配 = 一个 target |
| **查询在线会话** | ❌ 不支持 | ✅ `sessions/` 目录可枚举 | 平台能力接口显式暴露（`iscsi_sessions`） |
| **强制踢下线** | ❌ 不支持（只能停用 target） | ✅ LIO 可强制登出 | 仍保留客户端心跳为主，会话仅作交叉校验 |
| 缓存加速 | 无原生能力 | **dm-cache / lvmcache**（SSD 加速 HDD） | 只在 thin pool 的 **`_tdata`** 上操作；改 cachemode 也必须针对 `<vg>/<pool>_tdata` |
| 空间回收 | `Optimize-VHD` / `Optimize-Volume` | `fstrim` / `blkdiscard`（thin LV 支持 discard） | thin pool 需启用 discard 才能把释放的块还给池 |
| 平台停机机制 | 命名事件 `Global\VaultServerStop` | PID 文件 + `SIGTERM`（`signal.Notify`） | 语义等价，实现分开 |
| 单实例锁 | `CreateMutex` | `flock` 锁文件（进程退出内核自动释放） | 锁目录回退顺序 `/run/lock` → `/var/lock` → `$TMPDIR` |

> **零 cgo 可行性**：LIO 走 configfs **文件操作**，LVM 走 **CLI exec**，二者都不需要 cgo，
> 因此 Linux 侧同样可以在 `CGO_ENABLED=0` 下交叉编译（见 §10.4 与 §12.2 的 CI 守卫）。

> ⚠️ **交叉编译范围**：`internal/agent` 与 `cmd/vault-agent` 是 **Windows 专属**（依赖 `x/sys/windows` 且无 build tag），
> 因此 Linux 侧只编译**服务端相关包**：`./cmd/vault-server ./internal/{app,platform,api,lock,config}/...`。

**Linux 运行前置条件**

```bash
# 内核模块：SCSI target 的 LIO 前端 + iblock 后端
modprobe target_core_mod iscsi_target_mod target_core_iblock
# 持久化（重启后自动加载）
echo -e "target_core_mod\niscsi_target_mod\ntarget_core_iblock" > /etc/modules-load.d/vault-lio.conf

# configfs 挂载（多数发行版默认已挂载）
mount -t configfs none /sys/kernel/config

# LVM2 工具链（lvcreate / lvconvert / lvs / vgs / thin_ls / lsblk）
apt install -y lvm2 util-linux      # Debian/Ubuntu
dnf install -y lvm2 util-linux      # RHEL/CentOS

# 防火墙
firewall-cmd --permanent --add-port=3260/tcp && firewall-cmd --reload

# 服务端以 systemd 托管（Linux 无双击启动器，--detached/runLauncher 会提示改用 systemd）
```

> **启动期只探测、不阻断**：LVM 或 LIO 不可用时仅打 WARN 日志（虚拟磁盘功能降级），
> 服务端仍可正常启动——避免"工具链缺失导致整个服务不可用"。

---

## 3. 总体架构

### 3.1 组件图

```
┌───────────────────────────────┐         ┌──────────────────────────────────────────┐
│  客户端 (Windows 终端用户)      │         │  服务端 (Windows Server)                  │
│                               │         │                                          │
│  ┌─────────────────────────┐  │  HTTPS  │  ┌────────────────────────────────────┐  │
│  │ Electron 渲染进程        │  │  mTLS   │  │ HTTP API 层 (单端口 443/8443)       │  │
│  │  - React + antd         │◄─┼────────►│  │  - 认证/鉴权/审计/限流              │  │
│  │  - 共享 UI 组件库        │  │         │  └────────────┬───────────────────────┘  │
│  └──────────┬──────────────┘  │         │               │                          │
│             │ 本地 HTTP+RPC    │         │  ┌────────────▼───────────────────────┐  │
│  ┌──────────▼──────────────┐  │  SSE    │  │ 应用服务层                          │  │
│  │ Go Sidecar (Vault-Agent)│◄─┼────────►│  │  Repo / Disk / Iscsi / User / Job  │  │
│  │  - iSCSI initiator 驱动 │  │  长连接  │  └────────────┬───────────────────────┘  │
│  │  - 挂载/卸载/托盘/自更新 │  │         │               │                          │
│  └─────────────────────────┘  │         │  ┌────────────▼───────────────────────┐  │
│                               │         │  │ 平台驱动层 (Windows only)           │  │
└───────────────────────────────┘         │  │  winvhd | winps | iscsitarget       │  │
                                          │  │  volume | upload | monitor          │  │
        ┌───────────────────────┐         │  └────────────┬───────────────────────┘  │
        │  管理页 (浏览器)        │ HTTPS   │               │                          │
        │  React + antd          │◄───────►│  ┌────────────▼───────────────────────┐  │
        └───────────────────────┘         │  │ 后台常驻：Job Worker / Reconciler   │  │
                                          │  │           Lease Reaper / Metrics    │  │
                                          │  └────────────┬───────────────────────┘  │
                                          │               │                          │
                                          │  ┌────────────▼───────────────────────┐  │
                                          │  │ 存储：SQLite / MySQL              │  │
                                          │  │       + 白名单目录(NTFS)           │  │
                                          │  └────────────────────────────────────┘  │
                                          └──────────────────────────────────────────┘
                                                          │
                                             Windows 存储子系统
                                        (VirtDisk / WinTarget / NTFS)
```

### 3.2 部署形态

- **服务端**：Windows Server，是所辖资源的"存储权威"，**单实例**部署。
- **多服务端拓扑（新增能力）**：一个客户端可同时连接 **N 个相互独立**的服务端；服务端之间**不通信、不共享数据**，彼此完全隔离（各自的 DB、CA、白名单卷、iSCSI target 命名空间）。
- **单端口**：API、管理页静态资源、SSE 共用同一端口（符合"单端口架构"的偏好）。
- **服务端单实例**：**必须加单实例锁**（命名互斥体 + DB 主键），原因：
  - 同一 VHDX 被两个进程操作会损坏磁盘；
  - iSCSI target 名称全局唯一，双实例会产生竞态。

> 多服务端的详细设计见 **3.4**。核心原则：**服务端保持现状（不感知其他服务端），复杂度全部收敛在客户端聚合层。**

### 3.3 通信协议

| 通道 | 协议 | 用途 |
| --- | --- | --- |
| 客户端 → 服务端 | HTTPS + mTLS（客户端证书） | 全部业务 API；**多服务端下每服务端一条独立通道**（见 3.4.1） |
| 服务端 → 客户端 | SSE（Server-Sent Events） | 反向指令：`revoke` / `config_changed` / `unmount` |
| 客户端 → 服务端 | 心跳 `POST /v1/leases/{id}/heartbeat` | 租约续期，30s 一次 |
| 管理页 → 服务端 | HTTPS + Cookie(Session) 或 Bearer | 管理 API |
| Electron ↔ Go Sidecar | 本地 HTTP `127.0.0.1:<随机端口>` + 一次性 Token | UI 与本地能力交互 |
| 客户端上传 | HTTPS 分块 `PUT /v1/uploads/{id}/chunks/{n}` | 大目录上传 |

> 反向指令为什么用 SSE 而非 WebSocket：客户端只需要"服务端推、客户端收"，SSE 更简单、可走 HTTP/2、断线重连语义清晰。若后续需要双向，再升级 WS。

### 3.4 多服务端支持

对应 design.md「多服务端」章节。**服务端侧零改动**，全部复杂度在客户端聚合层。

#### 3.4.1 客户端聚合模型

```
Vault-Agent (Go)
  └─ ServerRegistry                       # 服务端注册表（本地持久化）
       ├─ Endpoint A (主服务端)  ─┬─ 独立 mTLS 通道（A 的 CA + A 的客户端证书）
       │                          ├─ 独立 SSE 长连接
       │                          ├─ 独立租约心跳
       │                          ├─ 独立挂载状态集
       │                          └─ 独立版本兼容状态
       ├─ Endpoint B             ─┴─ （同上，完全隔离）
       └─ Endpoint N             ─── …
```

**隔离原则（硬性）**：不同服务端的连接、凭据、心跳、挂载状态、错误重试**全部独立**，禁止共享任何会话对象。

| 维度 | 是否隔离 | 说明 |
| --- | --- | --- |
| CA 与客户端证书 | ✅ | 每个服务端一套（见 9.1） |
| mTLS / SSE 连接 | ✅ | 一条连接对应一个服务端 |
| 租约与心跳 | ✅ | 服务端各自记账，互不可见 |
| 挂载状态 | ✅ | 但客户端 UI 需**汇总展示** |
| 版本兼容状态 | ✅ | 单台不兼容只禁用该台 |
| 用户身份 | ✅ | 同一用户在 A、B 可能是不同账号或只有一侧有账号 |
| 熔断/重试 | ✅ | B 故障不得影响 A（见 3.4.7） |

#### 3.4.2 版本兼容协商（决策：兼容区间 + 配置化 + 可关闭）

兼容区间**由服务端配置文件提供**（不是硬编码、也不是必须自动推导），并支持整体关闭检查。

**服务端配置（`config.yaml`）**

```yaml
server:
  instance_id: "srv-7f3a..."        # 首次启动生成并持久化（重装才变）
  name: "家中的存储服务器"            # 展示用别名，管理页可改

client_compat:
  enabled: true                     # ★ 版本兼容检查总开关；false = 关闭检查
  min: "1.4.0"                      # 最低支持的客户端版本（含）；留空 = 不设下限
  max: "1.9.x"                      # 最高支持的客户端版本（含，支持 x 通配尾段）；留空 = 不设上限
  # 仅当 enabled=true 且对应字段非空时才参与判定
```

**配置解析规则（按优先级）**

| 情况 | 行为 |
| --- | --- |
| `enabled: false` | **完全跳过客户端版本区间检查**（客户端版本不再参与可用性判定） |
| `enabled: true` + 字段留空 | 该端不设限制（如只填 `min` 则等于"只设下限"） |
| `enabled: true` + 字段缺失 | 用**服务端版本推导**默认区间（保持开箱即用），并在启动日志中打印推导结果 |
| `enabled: true` + **显式写了非法值** | **启动失败并明确报错**（fail fast），如 `min > max`、版本格式非法、`max` 通配位非法 |

> 为什么非法值用 fail-fast 而不是降级：版本区间配错会让**所有客户端被判不兼容、资源整体不可见**（见 R15），比"服务启动失败"严重得多。缺失给默认值、写错就报错，是兼顾可用性与安全性的边界。

**`/v1/system/info` 返回（匿名可访问）**

```jsonc
{
  "server_instance_id": "srv-7f3a...",
  "server_name": "家中的存储服务器",
  "api_version": 1,                       // ★ 协议版本，始终返回、不可关闭
  "server_version": "1.6.2",
  "client_compat": {
    "enabled": true,                      // ★ 关闭时返回 false
    "min": "1.4.0",
    "max": "1.9.x"
  },
  "features": ["multi_server", "temp_share_readonly", "..."]
}
```

**客户端判定逻辑**

```
① 先判协议版本（不可关闭，硬性）
   api_version ∉ 客户端支持的协议版本集合 → INCOMPATIBLE_PROTOCOL（不加载，提示原因）

② 再判客户端版本区间（enabled=false 时跳过）
   enabled == false                      → COMPATIBLE（跳过区间判定，UI 标注"该服务端已关闭版本检查"）
   localVersion < min                    → NEEDS_CLIENT_UPGRADE（提示升级，该服务端暂不可用）
   min <= localVersion <= max            → COMPATIBLE（正常加载）
   localVersion > max                    → NEEDS_SERVER_UPGRADE（提示该服务端需升级，仅禁用这一台）
```

> ⚠️ **"关闭检查"只关闭客户端版本号区间，不关闭 API 协议版本校验。**
> 协议版本（`api_version`）是通信的硬前提——用 v1 客户端连 v2 服务端会导致请求/响应结构不匹配，产生难以定位的解析错误。
> 因此 `api_version` **始终返回、不可配置关闭**；客户端也需始终校验。

**关键约束**

| # | 约束 |
| --- | --- |
| 1 | **必须用区间，禁止用版本相等**。相等会导致"服务端已升级但客户端未升级"时资源整体不可见，运维不可行 |
| 2 | 版本比较用 **semver 语义 + `max` 支持通配尾段**（`1.9.x`），禁止字符串比较 |
| 3 | 兼容性检查在**认证之前**完成（`/v1/system/info` 匿名可访问），否则证书/账号不匹配会与版本问题混淆，排障困难 |
| 4 | **不兼容 ≠ 删除**。客户端保留该服务端配置，仅置为不可用状态并给出可操作的提示（升级客户端 / 联系管理员升级服务端） |
| 5 | UI 上必须**显式呈现**"该服务端版本不兼容"标识，**绝不能静默显示空列表**（会被用户当成数据丢失报障） |
| 6 | 客户端需内置一个**兜底最低协议/版本支持集**，不盲信服务端下发内容（防畸形配置） |
| 7 | `features` 能力开关用于**灰度**：客户端按服务端声明决定是否显示某功能，避免"UI 有入口但服务端不支持" |
| 8 | **配置支持热重载**（`fsnotify`）：改 `client_compat` 后立即生效于新的 `system/info` 响应与后续连接；**已建立的连接不强制断开**；每次变更写审计日志（改前值 → 改后值） |
| 9 | **管理页提供全量配置表**（`GET /v1/system/config`）：展示当前生效区间、配置文件路径与每项说明；`editable=true` 的项可在线修改并**写回 config.yaml**（`PATCH /v1/system/config`，写前跑一遍校验 + 自动备份 `.bak`），`editable=false` 的项**置灰并注明原因**（密钥/身份/由构建或数据库决定的项）；被环境变量覆盖的项标 `source=env` 提示"改文件不生效"；`needs_restart=true` 的项保存后提示重启 |
| 10 | `enabled: false` 属于**降低保护**，服务端启动时打 **WARN 日志**，管理页显示醒目告警条，并在审计中记录开启/关闭操作 |
| 11 | 关闭检查会同时暴露在匿名的 `system/info` 中，攻击者可据此选择不兼容的旧客户端接入 → **生产环境不建议关闭**，仅用于灰度/排障 |

#### 3.4.3 更新源策略（决策：主服务端 + 失败回退）

```
配置：servers[] + primary_server_id（用户显式指定，默认 = 首次添加的那台）
取包顺序：primary → 其他 COMPATIBLE 服务端（按配置顺序）→ 全部失败则报"暂时无法检查更新"
```

**为什么不能"固定第一项且无回退"**（design.md 原表述的风险）：
> 若主服务端版本较旧，而客户端连接其他服务端需要更高版本，则**只能从旧服务端取包 → 永远升不上去 → 死锁**。
> 且主服务端离线时客户端**完全无法更新**。

**约束**

| # | 约束 |
| --- | --- |
| 1 | **主服务端语义必须显式化**：用户可在设置中切换"主服务端"，而不是隐式依赖"列表第 1 项"。删除主服务端时**必须让用户重新选择**，不得静默切换 |
| 2 | **回退的前提是签名域一致**：所有服务端分发的更新包必须由统一发布方签名（见 3.4.4），否则回退等于信任降级 |
| 3 | 回退时需**优先选版本更高的包**；若各服务端包版本不一致，记录审计（说明分发不同步） |
| 4 | 更新源选择结果要**可见**：设置页显示"当前更新源：<服务端名>"，便于排障 |

#### 3.4.4 更新包签名（决策：统一发布方签名）

```
发布方（你）持有私钥 → 对安装包签名 → 分发到各服务端（服务端仅作 mirror，不参与签名）
客户端内置发布方公钥（编译期固化）→ 校验签名 → 才允许安装
```

| # | 约束 |
| --- | --- |
| 1 | **服务端绝不持有更新签名私钥**。否则任一服务端被攻陷即可推恶意更新，攻击面随服务端数量线性放大 |
| 2 | 客户端公钥**编译期固化**，不通过任何网络下发（否则可被中间人替换） |
| 3 | **版本单调性检查**：拒绝版本号低于当前版本的安装包，防降级攻击（被控服务端下发含漏洞的旧版） |
| 4 | 校验链：发布方签名 → 包 SHA256 → 版本单调性 → 目标版本兼容性，**四者全过才安装** |
| 5 | 服务端下发的更新元数据（版本、URL、哈希）**不可信**，仅作索引；一切以签名验证结果为准 |

#### 3.4.5 挂载点与命名冲突

多服务端下**同名存储库必然出现**，必须解决。

| 挂载形态 | 规则 |
| --- | --- |
| **目录模式** | 路径强制为 `<root>\<server-alias>\<repo-name>`，用服务端别名作命名空间隔离 |
| **盘符模式** | 客户端统一维护盘符分配表，检测冲突；冲突时按"服务端配置顺序 + 库名"确定性分配，或让用户指定 |

**服务端别名（alias）规则**
- 添加服务端时生成（默认取服务端主机名或 `server_name`），用户可改；
- 别名须**唯一且符合文件系统命名规范**（去除 `\/:*?"<>|` 等非法字符，处理 Windows 保留名 `CON/PRN/AUX/NUL`）；
- 别名变更需提供**挂载点迁移**（否则已挂载的路径失效）。

**对"挂载后执行脚本"的影响（重要）**
> `挂载后执行脚本`（design.md 客户端-存储库-4）里若写了绝对路径，会因命名空间变化而失效。
> **脚本必须使用注入变量而非硬编码路径**：`{MOUNT_PATH}`、`{REPO_NAME}`、`{SERVER_ALIAS}`、`{SERVER_NAME}`。
> 服务端保存脚本模板，客户端渲染变量后执行（渲染在客户端做，避免服务端知道本地路径）。

#### 3.4.6 分组展示与来源标识（决策：加来源标识 + 同名可聚合）

对应 design.md 客户端-存储库-8 与「多服务端」。

**数据位置不变**：分组标签仍存在**各服务端**的存储库逻辑记录里（保持跨设备同步能力）。客户端在**展示层**做来源标识与聚合。

```
服务端侧（不变）：repositories.meta.group = "游戏"

客户端展示层：
  默认视图（按服务端分组）
    ▼ 服务端 A · 家中的存储服务器
        └ 分组「游戏」   → 3 个库
        └ 分组「工具」   → 1 个库
    ▼ 服务端 B · 公司测试机
        └ 分组「游戏」   → 2 个库
        └ 分组「临时」   → 4 个库

  聚合视图（同名分组合并，用户可切换）
    ▼ 分组「游戏」  (A + B)
        └ [A] 库1 / [A] 库2 / [A] 库3 / [B] 库4 / [B] 库5
    ▼ 分组「工具」  (A)
    ▼ 分组「临时」  (B)
```

| # | 规则 |
| --- | --- |
| 1 | 每个**存储库条目**前始终显示**服务端来源标识**（别名缩写或图标），无论哪种视图 |
| 2 | 提供视图切换：**按服务端分组** / **同名分组聚合**，默认后者（更符合用户"按用途归类"的心智） |
| 3 | 聚合判定：**分组名完全匹配**（区分大小写，用 NFC 规范化后比较，避免 Unicode 等价字符导致漏聚合） |
| 4 | 聚合组内排序：先按服务端配置顺序，再按服务端返回的组内顺序 |
| 5 | 未分组的库统一归入隐含组「未分组」，同样带服务端标识 |
| 6 | 聚合仅影响**展示**，不改变任何服务端数据，也不产生跨服务端的写操作 |
| 7 | 同名但属于不同服务端的分组，其**成员权限互不影响**（A 的"游戏"组可管理列表与 B 的完全独立） |

#### 3.4.7 启动与故障隔离

| # | 要求 |
| --- | --- |
| 1 | 启动时**并发**探测所有服务端，总耗时 = max(单个超时)，不是 sum |
| 2 | 单服务端连接超时建议 10s，失败**不阻塞**其他服务端与整体启动 |
| 3 | 每服务端独立熔断器：连续失败 N 次后进入退避（指数，上限如 5min），避免无脑重试打爆网络 |
| 4 | 客户端 UI 必须在**离线状态下仍可用**（展示缓存的服务端/库列表，标注"离线"） |
| 5 | 服务端被移除时，需处理其上的活跃挂载：提示用户先卸载，或强制卸载并明确告知 |

#### 3.4.8 服务端侧需要的改动（极小）

| 改动 | 说明 |
| --- | --- |
| `GET /v1/system/info` **匿名可访问** | 返回版本契约（见 3.4.2），供客户端在认证前做兼容判定 |
| 新增 `server.instance_id` | 首次启动生成并持久化到配置；用于客户端识别"同一台服务端"（重启/改名/换 IP 后仍能识别） |
| 新增 `server.name`（别名） | 展示用，管理页可改 |
| **新增 `client_compat` 配置段** | 位于 `config.yaml`：`enabled`（可关闭检查）+ `min` + `max`；支持**热重载**；非法值 **fail-fast**；管理页「服务端配置」表中可在线修改（属 `editable` 项） |
| `api_version` 常量 | 随代码版本走，**不可配置、不可关闭**（协议硬前提） |
| `features` 能力列表 | 由代码按版本声明，用于客户端灰度开关 |
| **其余全部不变** | 服务端**不知道**自己是否是"主服务端"，也**不感知**其他服务端 |

### 3.5 初始化（Bootstrap）

对应 design.md「初始化页面」。**服务端不承载 Web 页面**，向导由客户端呈现，服务端只提供两个接口。

#### 3.5.1 接口

```
GET  /v1/system/bootstrap      匿名 → 初始化状态（只返回状态，不含任何敏感信息）
POST /v1/system/bootstrap      匿名（仅未初始化时可用）← {username, password, server_name?}
                                → 201 {token, expires_at, user}   # 直接下发会话，无需再登录
```

`GET` 响应：

```jsonc
{
  "initialized": false,          // 是否已完成初始化
  "needs_bootstrap": true,       // 是否需要且允许进入向导
  "has_super_admin": false,      // 是否已存在超级管理员
  "has_data": false,             // 是否已有业务数据（存储库）
  "bootstrap_enabled": true,     // 配置是否允许自助初始化
  "super_admin_enabled": true,   // 内置超级管理员开关是否开启
  "blocked_reason": "",          // already_initialized | bootstrap_disabled | bootstrap_window_expired
  "server_name": "家中的存储服务器",
  "server_instance_id": "srv-7f3a...",   // 供用户核对"是否连对了机器"
  "api_version": 1
}
```

#### 3.5.2 开放条件与安全约束

| # | 约束 | 理由 |
| --- | --- | --- |
| 1 | **仅在"无超级管理员 且 无业务数据"时开放** | 等价于"确认系统从未被使用过"。任一条不满足即返回 409 |
| 2 | **并发安全**：用 `settings` 表主键约束原子抢占一次性标记 | 两个并发请求只有一个能成功；抢占标记与创建管理员在**同一事务**内，失败整体回滚，不留半初始化状态 |
| 3 | 抢占标记写入后，**再次复核**用户表与存储库表为空 | 防止标记被手工删除后对已投产系统二次开放初始化 |
| 4 | 受配置开关 `security.bootstrap_enabled` 约束 | 可彻底关闭自助初始化，改用 CLI 引导（离线/高安全场景） |
| 5 | 支持初始化**时间窗口** `security.bootstrap_window`（自首次启动计时） | 降低"部署后长期未初始化、被局域网内他人抢注"的风险 |
| 6 | 口令强度**服务端强制校验**（8~128 字符，必须同时含字母与数字），argon2id 哈希落库 | 前端预校验只是体验优化，不可作为安全边界 |
| 7 | 用户名规则：3~64 字符，仅字母/数字/`_`/`-`/`.` | 避免注入与文件系统问题 |
| 8 | 初始化成功后写审计 `system.bootstrap`，并以 **WARN** 级别记录日志 | 这是权限最高的账号诞生时刻，必须可追溯 |
| 9 | 该接口**受登录限流约束** | 防止被用于探测与暴力尝试 |

#### 3.5.3 客户端启动状态机

```
应用启动
 ├─ 读取本地服务端配置（无 → 显示"配置服务端地址"页，默认 http://127.0.0.1:8443，可"测试连接"）
 ├─ POST /v1/system/client-check（提交 api_version 与本端版本）
 │    ├─ incompatible_protocol → 硬性提示页（协议不匹配，不可继续）
 │    └─ needs_client_upgrade / needs_server_upgrade → 提示页 + 允许"仍然继续（只读查看）"
 ├─ GET  /v1/system/bootstrap
 │    ├─ 网络失败 → "无法连接服务端" + 重试/修改地址
 │    ├─ needs_bootstrap = true → 【初始化向导】
 │    ├─ blocked_reason = bootstrap_disabled / bootstrap_window_expired → 对应提示页
 │    └─ 否则 → 有本地有效 token 则校验后进主界面，无 token 则进【登录页】
 └─ 登录/初始化成功 → 保存 token 与用户信息 → 进入主界面（管理模块仅对 super_admin 可见）
```

**初始化向导 5 步**（antd `Steps`，无动效）：

| 步骤 | 内容 |
| --- | --- |
| 1 欢迎 | 说明"只做一次、将创建最高权限账号"；展示 `server_name` 与 `server_instance_id` 前 8 位供核对 |
| 2 账号 | 用户名（默认 `admin`）+ 密码 + 确认密码；实时强度提示（长度/字母/数字） |
| 3 服务端名称 | 可选，用于在多服务端场景下区分 |
| 4 确认 | 汇总（口令遮蔽）+ 必勾"我已知晓该账号拥有全部权限" |
| 5 完成 | 调用初始化接口 → 保存会话 → "进入控制台" |

**已知取舍**：服务端不再承载 Web 页面，因此**首次部署必须先获得客户端安装包**，且**没有浏览器兜底**。
若客户端不可用（例如客户端本身出问题）将无法初始化。缓解手段：保留 CLI 引导路径作为应急（见 12.3）。

---

## 4. 领域模型与数据模型

### 4.1 实体关系

```
User 1──N Certificate
User 1──N Lease
User 1──N Allocation

Repository ─┬─ 1 ParentDisk (共享库) / 1 Disk (独享库)
            ├─ N DiffDisk (共享库)
            ├─ N Membership (可管理列表)
            └─ N IscsiTarget

Disk (VHDX 元数据) ─┬─ N Allocation
                    └─ 1..N IscsiTargetMapping

IscsiTarget 1──N IscsiTargetMapping
IscsiTarget 1──1 IscsiAuth (CHAP 密钥 / IP 白名单)

Upload 1──N UploadChunk
Job       (通用异步任务)
AuditLog
Setting  (全局配置 KV)
```

### 4.2 表设计（要点）

```sql
-- 用户与凭据
CREATE TABLE users (
  id            TEXT PRIMARY KEY,
  username      TEXT NOT NULL UNIQUE,
  role          TEXT NOT NULL,              -- super_admin | user
  password_hash TEXT,                       -- argon2id；super_admin 可为空（仅配置密码）
  enabled       INTEGER NOT NULL DEFAULT 1,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);

CREATE TABLE certificates (
  id          TEXT PRIMARY KEY,
  user_id     TEXT NOT NULL REFERENCES users(id),
  serial      TEXT NOT NULL UNIQUE,
  fingerprint TEXT NOT NULL UNIQUE,         -- 证书 DER 的 SHA-256（排障/吊销用）
  spki_sha256 TEXT NOT NULL,                -- 公钥(SPKI) SHA-256，★ 身份绑定与续期比对用，不用证书指纹
  status      TEXT NOT NULL,                -- active | revoked
  bound_ip    TEXT,                         -- 辅助绑定，非权威
  bound_mac   TEXT,                         -- 辅助绑定，非权威
  not_before  INTEGER NOT NULL,
  not_after   INTEGER NOT NULL,
  created_at  INTEGER NOT NULL
);

-- 存储（可在线管理的 VHDX 根目录；配置里的 whitelist_roots 仅作首次种子）
CREATE TABLE storages (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL UNIQUE,          -- 展示名，应用层按大小写不敏感去重
  path       TEXT NOT NULL UNIQUE,          -- 绝对路径，应用层按大小写不敏感去重
  enabled    INTEGER NOT NULL DEFAULT 1,    -- 停用后不再参与新盘放置
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

-- 存储底层卷（schema v3；见 §5.15）。**无行 = 目录模式**（Windows / 历史存储）。
CREATE TABLE storage_volumes (
  storage_id  TEXT PRIMARY KEY REFERENCES storages(id) ON DELETE CASCADE,
  kind        TEXT NOT NULL,                -- thin | lv | dir
  managed     INTEGER NOT NULL,             -- 1=系统创建（删除时可 lvremove）；0=登记已有卷（绝不删）
  state       TEXT NOT NULL,                -- pending | ready
  ref         TEXT NOT NULL,                -- 设备引用，如 /dev/mapper/vg-lv；dir 模式为空
  file_system TEXT NOT NULL,                -- ext4 | xfs
  size_bytes  INTEGER NOT NULL DEFAULT 0,   -- 分配容量；dir 模式为 0
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
);

-- 磁盘（VHDX 元数据；同时覆盖母盘/差异盘/独享盘）
CREATE TABLE disks (
  id            TEXT PRIMARY KEY,
  repo_id       TEXT REFERENCES repositories(id),
  kind          TEXT NOT NULL,              -- parent | diff | standalone
  vhdx_path     TEXT NOT NULL UNIQUE,       -- 存储（Storage）根下的绝对路径
  parent_id     TEXT REFERENCES disks(id),  -- 差异盘指向母盘
  parent_version INTEGER,                   -- 差异盘：创建时母盘的版本号
  content_fingerprint TEXT,                 -- 母盘：size+mtime+头部哈希，用于检测"母盘被改过"
  size_bytes    INTEGER NOT NULL,           -- 逻辑大小
  physical_bytes INTEGER,                   -- 实际占用（定期采样）
  vhd_type      TEXT NOT NULL,              -- dynamic | fixed | differencing
  state         TEXT NOT NULL,              -- creating|ready|published|maintenance|deleting|error
  desired_state TEXT NOT NULL,              -- 期望状态（对账用）
  observed_state TEXT,                      -- 上次对账观测到的实际状态
  mounted       INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);

-- 存储库
CREATE TABLE repositories (
  id           TEXT PRIMARY KEY,
  name         TEXT NOT NULL UNIQUE,
  mode         TEXT NOT NULL,               -- shared | exclusive
  owner_id     TEXT NOT NULL REFERENCES users(id),
  parent_disk_id TEXT REFERENCES disks(id), -- shared 模式
  parent_version INTEGER NOT NULL DEFAULT 0,-- 母盘版本号，每次维护更新 +1
  parent_condition TEXT,                    -- idle | derived | temp_shared | maintenance（见 5.4）
  max_diff_disks INTEGER NOT NULL DEFAULT 50, -- ★ 创建时由 owner 配置的单母盘差异盘上限
  quota_bytes  INTEGER,                     -- 应用层配额（owner 独担，见 13.2-⑧）
  used_bytes   INTEGER NOT NULL DEFAULT 0,
  state        TEXT NOT NULL,               -- active|sealing|deleting|error
  meta         TEXT NOT NULL DEFAULT '{}',  -- JSON：分组标签、纯客户端展示配置
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL
);

-- 可管理列表（仅 owner 可改）
CREATE TABLE repo_members (
  repo_id TEXT NOT NULL REFERENCES repositories(id),
  user_id TEXT NOT NULL REFERENCES users(id),
  perm    TEXT NOT NULL,                    -- read | mount | manage
  PRIMARY KEY (repo_id, user_id)
);

-- 分配：用户 ↔ 差异盘/独享盘
CREATE TABLE allocations (
  id         TEXT PRIMARY KEY,
  repo_id    TEXT NOT NULL REFERENCES repositories(id),
  disk_id    TEXT NOT NULL REFERENCES disks(id),
  user_id    TEXT NOT NULL REFERENCES users(id),
  state      TEXT NOT NULL,                 -- allocated|mounting|mounted|releasing|released
  created_at INTEGER NOT NULL
);

-- iSCSI 目标
CREATE TABLE iscsi_targets (
  id             TEXT PRIMARY KEY,
  target_name    TEXT NOT NULL UNIQUE,      -- 同时也是 IQN 后缀
  disk_id        TEXT REFERENCES disks(id),
  purpose        TEXT NOT NULL,             -- user | temp_parent
  auth_mode      TEXT NOT NULL,             -- none | ip | chap
  chap_user      TEXT,
  chap_secret_enc BLOB,                     -- AES-GCM 加密存储，绝不明文
  reverse_chap_secret_enc BLOB,
  enabled        INTEGER NOT NULL DEFAULT 1,
  desired_enabled INTEGER NOT NULL DEFAULT 1,
  created_at     INTEGER NOT NULL,
  updated_at     INTEGER NOT NULL
);

-- initiator 白名单（授权）
CREATE TABLE iscsi_initiator_ids (
  target_id TEXT NOT NULL REFERENCES iscsi_targets(id),
  id_type   TEXT NOT NULL,                  -- IQN | IPAddress | DNSName | MACAddress | IPv6Address
  value     TEXT NOT NULL,
  PRIMARY KEY (target_id, id_type, value)
);

-- 租约（在线状态权威来源）
CREATE TABLE leases (
  id            TEXT PRIMARY KEY,
  allocation_id TEXT REFERENCES allocations(id),
  target_name   TEXT NOT NULL,
  user_id       TEXT NOT NULL,
  client_id     TEXT NOT NULL,
  owner_token   TEXT NOT NULL,              -- 防止抢占
  mount_point   TEXT,                       -- 'E:' 或 'C:\vault\repo-x'
  state         TEXT NOT NULL,              -- active | expired | revoked | released
  expires_at    INTEGER NOT NULL,
  last_seen_at  INTEGER NOT NULL,
  created_at    INTEGER NOT NULL
);

-- 上传会话（大目录分块续传）
CREATE TABLE uploads (
  id            TEXT PRIMARY KEY,
  user_id       TEXT NOT NULL,
  repo_name     TEXT NOT NULL,
  mode          TEXT NOT NULL,              -- copy | move
  total_files   INTEGER,
  total_bytes   INTEGER,
  received_bytes INTEGER NOT NULL DEFAULT 0,
  staging_dir   TEXT NOT NULL,
  manifest      TEXT,                       -- JSON：文件列表+大小+校验和
  state         TEXT NOT NULL,              -- open|verifying|complete|failed|aborted
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);

CREATE TABLE upload_chunks (
  upload_id TEXT NOT NULL REFERENCES uploads(id),
  chunk_index INTEGER NOT NULL,
  rel_path  TEXT NOT NULL,
  offset    INTEGER NOT NULL,
  size      INTEGER NOT NULL,
  checksum  TEXT NOT NULL,
  received_at INTEGER NOT NULL,
  PRIMARY KEY (upload_id, chunk_index)
);

-- 通用异步任务
CREATE TABLE jobs (
  id          TEXT PRIMARY KEY,
  type        TEXT NOT NULL,                -- create_vhdx|copy_vhdx|diff_create|publish|reclaim|compact|reconcile
  ref_id      TEXT,                          -- 关联实体
  idem_key    TEXT UNIQUE,                   -- 幂等键
  payload     TEXT NOT NULL,
  state       TEXT NOT NULL,                -- pending|running|succeeded|failed|cancelled
  progress    INTEGER NOT NULL DEFAULT 0,
  attempt     INTEGER NOT NULL DEFAULT 0,
  last_error  TEXT,
  created_at  INTEGER NOT NULL,
  started_at  INTEGER,
  finished_at INTEGER
);

-- 审计
CREATE TABLE audit_logs (
  id         TEXT PRIMARY KEY,
  user_id    TEXT,
  action     TEXT NOT NULL,
  resource   TEXT,
  detail     TEXT,                          -- JSON
  ip         TEXT,
  result     TEXT NOT NULL,                 -- ok | denied | error
  created_at INTEGER NOT NULL
);
```

> **所有 `*_enc` 字段**：使用 AES-256-GCM，密钥来自服务端配置文件中的 `master_key`（首次启动生成，权限锁定）。**iSCSI CHAP 密钥禁止明文落库、禁止出现在日志与 API 响应中**。

### 4.3 状态机

**母盘条件（`repositories.parent_condition`）—— 共享模式的核心约束**

```
                    ┌──────────────────────────────────────────┐
                    │  IDLE                                    │
                    │  无差异盘 / 未发布 / 未挂载               │
                    └──┬───────────────┬───────────────┬───────┘
        建差异盘        │               │ 临时只读共享   │ 进入维护
        (分配用户)      │               │ (发 iSCSI)     │ (需无差异盘)
                        ▼               ▼               ▼
                 ┌───────────┐   ┌─────────────┐  ┌──────────────┐
                 │  DERIVED  │   │ TEMP_SHARED │  │ MAINTENANCE  │
                 │ 存在≥1差异 │   │ 只读共享中   │  │ 读写挂载更新  │
                 └─────┬─────┘   └──────┬──────┘  └──────┬───────┘
     清理全部差异盘 ◄────┘               │ 撤销共享        │ 完成更新
     (含"维护"前置)                     └────────────────►│
                        └────────────────────────────────► IDLE
```

| 迁移 | 前置校验 | 副作用 |
| --- | --- | --- |
| `IDLE → DERIVED` | 母盘未发布、未挂载 | 创建差异盘，`parent_version` 写入子盘 |
| `DERIVED → IDLE` | **所有差异盘已卸载**（无 active lease） | 删除全部差异盘与 target；`used_bytes` 回收 |
| `IDLE → TEMP_SHARED` | 无差异盘、未挂载 | 发布只读 iSCSI target（落盘登记，崩溃可恢复） |
| `TEMP_SHARED → IDLE` | 无 active lease | 撤销共享、删除 target |
| `IDLE → MAINTENANCE` | 无差异盘、未共享 | 允许读写挂载母盘 |
| `MAINTENANCE → IDLE` | 母盘已卸载 | `parent_version += 1`，重算指纹 |
| `DERIVED → MAINTENANCE` | **必须先显式"清理差异盘"**（二次确认） | 等价于 `DERIVED → IDLE → MAINTENANCE` |

> ⚠️ **禁止的迁移**：`DERIVED → TEMP_SHARED`（母盘被引用时不可再共享）、`MAINTENANCE` 期间派生差异盘。
>
> ⚠️ **`IDLE → DERIVED` 的数量闸门**：每次派生都需校验 `diff_count < repositories.max_diff_disks`。该上限**由 owner 在创建存储库时配置**（默认 50），创建后可在库设置中调整（调低时若当前数量已超，仅禁止新增、不强制删除）。

**Disk**

```
creating → ready → published → (attached 仅出现在 TEMP_SHARED / MAINTENANCE)
             │
             └→ deleting → (移除)
任何步骤失败 → error（由 Reconciler 收敛或人工介入）
```

**Lease**

```
active ──(心跳超时)──► expired ──(reaper 停用 target)──► revoked
   └──(客户端正常卸载)──► released
```

---

## 5. 核心技术方案

### 5.1 存储布局规范

**存储（Storage）实体**（见 `domain.Storage` / `store.storage_store.go`）

"VHDX 存放根目录"已从**配置文件**升级为**可在线管理的实体**：

- **数据库是真源**：新增 `storages` 表（`id / name / path / enabled / created_at / updated_at`），
  由管理端接口 `GET/POST/PATCH/DELETE /v1/storages` 维护；读取对任何登录用户开放（建库表单需要），
  写操作仅超级管理员。
- **配置仅作首次种子**：服务端启动时若 `storages` 表为空，则把 `storage.whitelist_roots`
  （或旧的 `whitelist_root`）每个根写成一条存储（名称取路径最后一段、冲突追加序号、默认启用）；
  之后再改配置文件**不生效**，请在管理端维护。
- **兜底**：若数据库里一条存储都没有（例如种子失败），放置逻辑回退到配置的 `EffectiveRoots()`，
  保证老部署不会立刻失效；若读取 `storages` 报错则返回明确错误，**绝不静默使用空根集**。
- **平台差异（重要）**：上面描述的是 **Windows** 语义——存储就是普通目录。
  **Linux 上存储升级为受管的 thin LV**（服务端 `lvcreate → mkfs → mount`，挂载点目录再当作根用），
  且配置里的 `whitelist_roots` **不再作为种子**（会种出一个让落盘悄悄落到宿主根文件系统上的
  「目录」存储）；详见 §5.15。

**放置策略**（`app.Deps.storageSelection` + `domain.PathGuardSet`）

- 新建设置为：只在 `enabled=true` 的存储里选根；差异盘优先与母盘同根（同卷）；
  在所有可用根中选择"所在卷可用空间最大"的那个，同卷并列时取顺序靠前者。
- 新建存储库时可在请求体带 `storage_id` 指定目标存储（存储不存在 → `storage.not_found`，
  已停用 → `storage.disabled`）；不带则在启用存储中自动选。选中的存储与目标根会写入日志。
- 迁移（`disks.vhdx_path`）与对账（孤儿扫描范围）随生效存储自动变化，逻辑不变。

**删除保护**

- 删除存储**只删记录、绝不删除磁盘文件**；该存储下仍有磁盘记录时拒绝删除（`storage.in_use`，409，
  带 `disk_count`）。修改存储 `path` 不会搬迁已有磁盘，仅影响后续放置与扫描范围。
- Linux 上另有一层：删除时会卸载并**仅删除由系统创建的 thin LV**（`managed=true`）；
  登记进来的已有卷只解除登记，**绝不 `lvremove`**（见 §5.15.5）。

```
存储根目录（Storage，NTFS，见 D5）/
├── disks/
│   ├── parents/<repo_id>/base.vhdx
│   ├── diffs/<repo_id>/<allocation_id>.vhdx
│   └── standalone/<repo_id>/data.vhdx
├── staging/                          # 上传暂存，与 disks 同卷（重要：原子 rename）
│   └── <upload_id>/
└── meta/                             # 日志、快照元数据
```

**硬性规则**
- `disks/` 与 `staging/` **必须在同一卷**，否则上传完成后无法 `atomic rename`，会产生大文件二次拷贝。
- 路径一律由服务端根据 ID 生成，**绝不接受用户传入路径片段**；所有读写前用 `filepath.EvalSymlinks` + 前缀校验防穿越。
- 存储根目录需校验：非压缩、非稀疏、非事务性、非网络共享（禁止 UNC）、非 EFS 加密。

### 5.2 VHDX 生命周期

```
create (VirtDisk API, dynamic)
   ↓
attach  (AttachVirtualDisk，提权)
   ↓
Update-HostStorageCache → Get-Disk -Location <vhdx 全路径> → 盘号
   ↓
Initialize-Disk GPT → New-Partition -AssignDriveLetter → Format-Volume NTFS
   ↓
[可选] 拷贝目录内容（robocopy）
   ↓
Dismount-DiskImage
   ↓
Import-IscsiVirtualDisk   ← 此时才算"可发布"
```

**尺寸计算规则（对应 design.md 第 15 条）**

```
required = max(用户填写的 size, ceil(目录实际字节数, 1MB) + 预留)
预留 = max(16MB, 目录大小的 10%)         // 文件系统元数据 + 碎片
minimum = 3MB                            // VirtDisk 硬下限
最终 size 向上取整到 1MB
若目录为空且未填 → 64MB 兜底
```

> **粒度为什么是 MB 而不是 GB**（真实反馈："我一点空间都没用就占了 1G"）：
> VHDX 是**动态盘**，写入前几乎不占物理空间，标称容量只是"上限"；但分配时是
> **按母盘标称容量预留用量**的（差异盘建完后才按实测物理占用校正）。GB 粒度 + 512MB 预留
> 下限 + 1GB 空目录兜底会让空目录/小目录也推导出 1GB 标称容量，于是每个分配一上来就占
> 1GB 配额（校正前的这段时间一直挂着）。按 MB 取整 + 10% 预留既不会写不下，也不会凭空
> 吃掉配额。配置项为 `storage.size_granularity_mb`（默认 1）；旧字段
> `size_granularity_gb` 已废弃（保留仅为兼容旧配置文件，不参与计算）。

### 5.3 iSCSI 发布与鉴权设计 ⚠️

**核心约束（必须理解）**：`Set-IscsiServerTarget -Chap` 接收一个 `PSCredential`，即 **一个 target 只能配置一组 CHAP 账号**。因此：

| 场景 | 能否用 CHAP 区分用户 | 正确做法 |
| --- | --- | --- |
| 独享库（1 库 1 用户） | ✅ 可以 | 该库的 target 配独立 CHAP 密钥 |
| 共享库（1 母盘 N 差异盘） | ❌ **不能** | 每个分配**单独建一个 target**（1 diff 盘 1 target），各自配 CHAP |
| 多用户共用同一 target | ❌ 不可能 | 平台层禁止该用法 |

**结论：授权模型必须改成「一个分配 = 一个 iSCSI target」。** 这比"一个 target 多个 initiator"更安全，也更契合 CHAP。

**iSCSI 命名规范**

```
target_name = vault-{repo_id短}_{allocation_id短}
IQN         = iqn.2026-01.com.vault:{target_name}
                                ↑ 服务端启动时从配置读取，需全局唯一且稳定
```

**鉴权模式（三选一，按库配置）**

| 模式 | 服务端动作 | 客户端动作 | 适用 |
| --- | --- | --- | --- |
| `none` | `Set-IscsiServerTarget -InitiatorIds @('IQN:...')` | `Connect-IscsiTarget -NodeAddress` | 内网可信 |
| `ip` | `-InitiatorIds @('IPAddress:10.0.0.7')` | 同上 | 固定 IP 环境 |
| `chap`（**推荐**） | `-EnableChap -Chap <user>,<secret>` + IQN 白名单 | `-AuthenticationType OneWayCHAP -ChapUsername -ChapSecret` | 默认 |

**密钥托管流程**

```
分配时：
  1. 服务端生成 24 字节随机密钥（Base64），落库时 AES-GCM 加密
  2. Set-IscsiServerTarget -EnableChap -Chap "$user,$secret"
  3. 客户端 GET /v1/allocations/{id}/credentials  ← mTLS + 租约持有者校验
     服务端解密后仅通过该通道下发一次，响应头 Cache-Control: no-store
  4. 客户端调用 Connect-IscsiTarget 时使用，不落盘（内存态）
吊销时：
  1. 重新生成密钥（使旧密钥失效）
  2. 移除 initiator 白名单
  3. Set-IscsiServerTarget -Enable $false
```

> CHAP 密钥强度：Windows CHAP secret 需 **12~16 字节**（用于单向 CHAP），建议 16 字节 Base64。

> **目标下发是幂等的，并且"期望状态没变化就不下发"**（`IscsiService.pushTarget`）：
> 先比"期望状态指纹"（含 CHAP 密钥，密钥只作为哈希输入、绝不进日志/DB），再用 `GetTarget` 读一次现状，
> 两者都一致才跳过。原因：Windows 上一次全量下发（建/改目标 + 授权 + CHAP + 映射 + 启用）实测约 **15 秒**，
> 而**每次挂载**都会走到这步（真实工单："每次挂载都要等十几秒"）。
> `TargetInfo` 只能读回 启用状态 / 授权 / 映射盘，**读不回 CHAP 与只读标志**，所以必须叠加指纹；
> 进程重启后指纹缓存为空 → 首次仍会全量下发（安全侧）；读不到现状（目标不存在/查询失败）一律老实下发。

### 5.4 存储库状态机、维护状态与母盘保护

对应 design.md 第 7、28 条。共享模式的核心是**母盘同一时刻只能处于一种"用途"**，四种用途互斥（详见 4.3 状态机）。

**已确认的母盘语义（来自 design.md 更新）**
1. 只有在**没有任何差异盘**、且母盘**本身没有被挂载**时，母盘才可以被临时挂载；
2. 临时挂载状态**需要落盘**（本文用 `parent_condition` + 持久 target 登记实现）；
3. 引入 **`MAINTENANCE`（维护）状态**：因为母盘内容需要更新，进入维护态前**必须支持清理差异盘**；
4. 只有**没有共享出去**的母盘才可以生成差异盘；
5. 母盘**有可能以只读方式挂载给客户端**（即 `TEMP_SHARED` 支持只读共享）。

**母盘保护采用"应用层标记"（D7 决策）**

因为母盘需要支持更新（维护态要读写挂载），**不能**用 `ATTACH_VIRTUAL_DISK_FLAG_READ_ONLY` 硬性锁死。代价是：Windows 层无法阻止"绕过 Vault 直接挂载母盘写入"。用以下三重兜底降低风险：

| 兜底手段 | 做法 |
| --- | --- |
| **指纹校验** | 每次挂载差异盘、每次挂载差异盘创建前，比对母盘 `content_fingerprint`（size + LastWriteTime + 头部 1MB 哈希）；不一致 → **拒绝挂载并告警** |
| **版本号** | 差异盘记录其基于的母盘版本（`disks.parent_version`）；挂载/分配时与 `repositories.parent_version` 比对：**一致 → 复用该差异盘与其 iSCSI 目标**（不重建、不重下发）；**不一致 → 作废旧差异盘与其目标并重建**（母盘**不共存多版本**，见 5.10）。<br>⚠️ 与本文早期版本的差异：不再"拒绝挂载并要求用户手动重新分配"——那种做法会把用户卡死在一个他已经无权修复的状态里 |
| **目录 ACL** | `disks/parents/` 目录授予 Vault 服务账户独占写权限；VHDX 文件名不含业务含义，降低被人工触碰的概率 |

**维护状态（MAINTENANCE）完整流程**

```
进入维护：POST /v1/repos/{id}/maintenance
  ① 前置校验：parent_condition ∈ {IDLE}
     - 若为 DERIVED → 拒绝，并提示"需先清理差异盘"（前端提供"清理差异盘"入口）
     - 若为 TEMP_SHARED → 拒绝，需先撤销共享
  ② 二次确认：列出将被删除的差异盘 / 受影响的分配与用户（若走"清理+维护"路径）
  ③ 事务：
       parent_condition = 'MAINTENANCE'
       所有相关 target 停用（Set-IscsiServerTarget -Enable $false）
  ④ 服务端本地挂载母盘（读写）、返回挂载点或让管理员通过管理页操作
  ⑤ 管理员更新内容（拷入新版本游戏文件等）
  ⑥ 退出维护：POST /v1/repos/{id}/maintenance/complete
       - 卸载母盘 → 校验已卸载
       - parent_version += 1
       - 重算 content_fingerprint
       - parent_condition = 'IDLE'
       - 审计记录：本次更新影响了 N 个被清理的差异盘
```

**清理差异盘（`DERIVED → IDLE`）是危险操作，必须强调**

| 风险 | 处理 |
| --- | --- |
| 用户数据丢失 | 差异盘内含用户改动；清理前**必须所有客户端卸载**（无 active lease），且需 owner 二次确认并输入库名 |
| 用户存档 | ⚠️ **产品决策点**：若差异盘承载用户存档，清理即丢档。design.md 未定义。建议明确"共享库仅承载可再生数据（缓存/日志/临时配置），用户存档应放独立独享库" |
| 误删 | 清理默认走 `meta/trash/` 软删除，保留 7 天 |
| 中途失败 | Job 幂等；`parent_condition` 保持 `DERIVED` 直到全部子盘删除成功 |

### 5.5 客户端挂载流程

```
① 用户点击"挂载" → POST /v1/repos/{id}/mount
   ├─ 服务端校验：权限、配额、是否已被自己占用
   ├─ 幂等：若已有该 user+repo 的 active lease 且 client_id 相同 → 直接返回凭据
   └─ 创建/刷新 Lease（TTL 120s，客户端 30s 心跳）
② 服务端返回 MountSpec：
   { target_iqn, portal_ip, portal_port, chap_user, chap_secret,
     disk_unique_id, mount_mode: 'letter'|'directory', mount_path, post_script }
③ 客户端 Go agent 执行本地挂载管道：
   a. Start-Service msiscsi / 确保服务运行
   b. New-IscsiTargetPortal -TargetPortalAddress <portal>（幂等，存在则跳过）
   c. Connect-IscsiTarget -NodeAddress <iqn> -AuthenticationType OneWayCHAP ...
      -IsPersistent $true
   d. 轮询 Get-IscsiSession 直到 IsConnected；若 60s 未连上 → 失败回滚
   e. Get-Disk | Where-Object { $_.BusType -eq 'iSCSI' } 找到对应盘
   f. Set-Disk -Number N -IsOffline $false; Set-Disk -Number N -IsReadOnly $false
   g. 挂载：
      - letter 模式：Add-PartitionAccessPath -AssignDriveLetter（若盘已有盘符则复用）
      - directory 模式：确认为空目录 → Add-PartitionAccessPath -AccessPath 'C:\Vault\<server-alias>\<repo>'
      - ★ 目录路径必须带服务端别名命名空间，避免多服务端同名库冲突（见 3.4.5）
   h. 执行 post_script（如存在），超时 60s，失败不阻塞挂载但记录警告
      - ★ 脚本中的路径必须用注入变量（{MOUNT_PATH}/{REPO_NAME}/{SERVER_ALIAS}），不得硬编码（见 3.4.5）
④ 回写 POST /v1/leases/{id}/mounted { mount_point }
⑤ 进入心跳循环
```

**卸载流程（严格逆序）**

```
① 若要断开：先把盘 Offline（否则 Disconnect-IscsiTarget 报 0xefff0040）
   Set-Disk -Number N -IsOffline $true
② 移除挂载点：Remove-PartitionAccessPath
③ Disconnect-IscsiTarget -NodeAddress <iqn>
④ Unregister-IscsiSession（可选，去除持久化）
⑤ POST /v1/leases/{id}/release → 服务端标记 released
```

> **目录挂载的坑**：`Add-PartitionAccessPath -AccessPath` 要求目标目录**已存在且为空**，且其父路径所在卷为 NTFS。客户端启动时需预检并给出明确错误提示。
>
> **多服务端**：目录模式的目标路径为 `<default_mount_dir>\<server-alias>\<repo-name>`（见 3.4.5）；盘符模式由客户端统一分配表管理并检测冲突。
>
> **门户地址（portal_address）从哪来**：服务端**从不派发自己的 HTTP 地址**（客户端地址由用户填写并经
> `POST /agent/session` 推给代理）；唯一由服务端计算的是挂载时的门户地址。优先级为：
> `platform.iscsi.portals` 的**第一条** `<host>[:port]`（显式指定，多网卡服务器必须这样配）
> → `http.listen` 的主机部分 → 本机第一个非回环 IPv4 → 主机名；端口取配置值，缺省 3260
> （见 `internal/app/lease.go` 的 `portalFromConfig`）。填通配地址（默认 `0.0.0.0:3260`）
> 表示"地址自动推导"，只取其端口。
>
> **超时口径必须区分"超时"与"连不上"**：挂载申请 `POST /v1/allocations/{id}/mount` 在服务端是
> **同步重活**（发布 iSCSI 目标 / CHAP / initiator 全走 PowerShell，单次约 10s，多步常超 30s）。
> 因此代理侧对它的超时单独放宽为 **2 分钟**（`mountRequestTimeout`），并把超时映射为
> `agent.server_timeout`（504，args.timeout_seconds）而不是 `agent.server_unreachable`——
> 后者会把排查方向直接带偏到地址/网络（真实工单：界面显示"无法连接服务端"，实际是 30s 超时）。
> 渲染层等待挂载的上限相应放宽为 5 分钟（`MOUNT_TIMEOUT_MS`）。
>
> **挂载阶段必须逐个反馈给界面**：上面两步（等服务端准备磁盘、等 PowerShell 下发目标）合计
> 常达数十秒，期间客户端只有一个 HTTP 请求在等待 —— 只显示"挂载中"用户无从知道卡在哪一步
> （真实反馈：希望看到"正在创建虚拟磁盘 / 正在创建 iSCSI 目标 / 正在分配资源"）。因此：
>
> | 阶段 | 含义 | 谁推进 |
> |---|---|---|
> | `requesting` | 已向服务端申请挂载 | 代理（调服务端**之前**写占位状态，界面立刻有反馈） |
> | `allocating` | 服务端正在分配资源 | 服务端（`LeaseService.RequestMount`） |
> | `preparing_disk` | 服务端正在准备虚拟磁盘（等差异盘/VHDX 就绪） | 服务端（`IscsiService.Publish`） |
> | `configuring_target` | 服务端正在创建/下发 iSCSI 目标（最慢） | 服务端（`IscsiService.Publish`） |
> | `connecting` | 本机正在建立 iSCSI 会话 | 代理（挂载引擎） |
> | `online` | 本机正在让磁盘上线 | 代理 |
> | `mount_point` | 本机正在挂载到盘符/目录 | 代理 |
> | `post_script` | 本机正在执行后置脚本 | 代理 |
>
> 通道复用服务端**进程内事件总线**（`GET /v1/system/events`，SSE；topic 即 SSE 的 `event:` 名，
> 桥接见 `cmd/vault-server/main.go` 的 `eventSinkAdapter`）：服务端在自己的同步 mount 请求内
> `emit("mount", {action:"progress", allocation_id, phase})` → 代理经 SSE 订阅
> （`internal/agent/sse.go`）写入本机挂载状态 →
> 经 `event: mount` 广播给渲染层（阶段文案见 `mount.phase.*`）。**阶段只前进不回退**
> （`mountPhaseRank` 定序：服务端事件异步到达，可能晚于本机阶段）。事件是尽力而为的，
> 丢了不影响挂载本身；权威结果以 mount 响应与磁盘状态为准。
>
> **本机预检（启动即告知）**：代理启动时**只读探测** iSCSI 发起端
> （`iscsiinitiator.Probe`：IscsiInitiator 模块 + MSiSCSI 服务状态，**绝不启动服务** ——
> 启动系统服务属于部署决策，只由用户点挂载时的 `EnsureService` 触发），结果写进
> `/agent/state` 的 `host` 并广播 `host` 事件。界面在本机未就绪时挂横幅
> （`agent.host.*`：区分未安装发起程序 / 服务未运行 / 探测失败），**不必等挂载失败
> （阶段：connect）才知道**。未就绪期间每 30s 复检一次 → 管理员执行
> `Start-Service MSiSCSI` 后横幅自动消失，无需重启客户端；挂载成功也会顺手置为就绪。
>
> **失败诊断（"根本没法定位错误"的修复）**：`connect` / `wait_connected` 失败时，代理把
> **门户地址、目标 IQN、认证方式**写进错误参数（`args.portal/target_iqn/auth_mode`）与状态详情
> （`last_error_detail`），并对门户做一次 **3s TCP 探测**（`args.tcp` =
> `reachable/timeout/refused/unreachable`）。TCP 结论能一刀切开两类完全不同的原因：
> 不可达 → 网络/防火墙，或服务端派发的门户地址本机到不了（多网卡常见）；
> 可达 → 目标名、CHAP，或服务端尚未发布目标。**CHAP 密钥绝不进入诊断信息**
>
> **下发给客户端的 IQN 必须以平台实际存储的目标名为准**：下发目标时用的名字（
> `platform.TargetSpec.Name`）一律是**完整 IQN**（`iqn.<前缀>:<短名>`），不是短名。
> 历史上这里传的是短名，而 Windows 目标服务器会按自己的命名权给目标起名
> （`iqn.1991-05.com.microsoft:<短名>`，见 `iscsi_set_target.ps1` 的兼容性判断），
> 于是客户端拿到的 IQN 在平台上根本不存在 —— 表现为 **TCP 3260 可达、但
> Connect-IscsiTarget 登录失败**（真实工单）。现在：① 下发用完整 IQN；
> ② 下发后读回平台实际名字（`IscsiService.discoverActualTargetName`），不一致时以实际值为准；
> ③ 旧命名的残留目标在发布时清理，避免同一个 VHDX 被两个目标争抢映射。
> （只存在于 `mountRuntime`）。界面在挂载失败悬浮层显示"连接信息"（`mount.info.*`）。
>
> **失败后的记账（真实反馈："都错误了就不要有挂载记录了"）**：用户主动挂载失败时，代理先
> 清理这次尝试（只有 Connect 之后的阶段才需要：严格卸载，force=false），然后**删除挂载记录**
> —— 失败原因由错误响应当场带回，界面用红色感叹号展示一次。清理失败则**保留** error 记录
> （会话/磁盘可能还挂在本机，必须留一个"卸载"入口，绝不能让残留变成"看不见"）。
> 启动时的自动挂载（restore）失败同样保留记录：本地记录代表"用户希望它保持挂载"的意图，
> 抹掉它等于以后再也不会自动挂回。
>
> **卸载绝不允许冻结在"卸载中"，启动恢复只认"还挂着的"记录**（真实事故：服务端重装后
> 目标不存在，卸载时下线磁盘/移除挂载点报错，非 force 直接 return —— 记录永远停在
> "卸载中"，重装服务端也清不掉，因为记录在本机状态文件里，且每次启动还会被自动重挂、
> 无限刷新失败）。因此：
>   - `unmountLocked` 失败时**恢复卸载前的状态**（mounted/error）再返回错误，
>     绝不把记录留在 unmounting；
>   - 启动恢复只处理 state=mounted/mounting 的记录；state=unmounting（代理死在卸载中途）
>     视作用户最后意图是卸载，**补完卸载（force）后删除记录**；state=error/revoked 是
>     诊断残留，**不自动重挂**（自动重挂只会每次启动都失败一遍），留给用户手动处理。
>
> **错误只用一个标记展示**：`MountStateCell` 把"本次请求错误"与"本机残留错误"合并成
> **一个**红色感叹号 + 多行悬浮详情；挂载列表不再单列"最后错误"文本列，`state="error"` 时
> 也不再叠加"错误"标签（两个标记表达同一件事，很乱）。

### 5.6 母盘临时共享（只读）与崩溃恢复 ⚠️

这是 design.md 第 28 条的核心诉求，也是最容易出问题的地方。

**语义澄清**：母盘的"临时挂载"发生在 `TEMP_SHARED` 状态。此时母盘以 **iSCSI 只读方式发布给客户端**（对应你提到的"有可能会挂载为可读到客户端"），而不是让管理员在服务端本地挂载。管理员的"本地读写挂载更新母盘"属于另一个状态 `MAINTENANCE`（见 5.4），二者互斥。

**临时共享的生命周期（带显式登记 + 对账）**

```
① 发起：POST /v1/repos/{id}/parent/temp-mount
   ├─ 校验 parent_condition == 'IDLE'
   │   （等价于：diff_count == 0 && 未发布 && 未挂载）
   ├─ 事务内：
   │    repo.parent_condition = 'TEMP_SHARED'
   │    disks[parent].desired_state = 'published_temp'
   │    创建 iscsi_targets(purpose='temp_parent', disk_id=parent, readonly=1)
   │    创建 allocations(special user='@temp', ...)  ← 显式登记，不依赖内存
   └─ 异步 Job：Import + New-IscsiServerTarget + Add-Mapping + Enable
② 挂载：走 5.5 的客户端流程，lease.user_id = 发起人（或 owner 授权的成员）
③ 正常结束：DELETE /v1/repos/{id}/parent/temp-mount
   Job：Remove-Mapping → Set-Enable $false → Remove-IscsiServerTarget
        → parent_condition = 'IDLE'，desired_state = 'ready'
④ 崩溃恢复（服务端重启）：
   Reconciler 启动时：
     - 扫描 repo.parent_condition == 'TEMP_SHARED' 的记录
     - 对每条：读取 DB 中的 target_name
       - 若 Windows 侧存在该 target → 说明进程崩溃前已发布
         → 用租约（heartbeat）判断是否仍有人连接
           - 有 active lease → 保持（并通知客户端）
           - 无 active lease → 执行 ③ 的清理
       - 若 Windows 侧不存在 → 直接置 parent_condition='IDLE'
     - 记录审计：'temp share auto-reaped after restart'
```

**只读保证**：母盘在 `TEMP_SHARED` 下应通过 iSCSI 侧限制为只读（Windows iSCSI Target 的 VHDX 可设为只读虚拟盘；具体参数需实测，见 14.1-T13）。若无法在 target 层强制，则退化为**客户端侧只读挂载 + 指纹校验**，并在审计中标记该风险。

**关键设计点**
- **不依赖内存标志位**，`parent_condition = 'TEMP_SHARED'` 是 DB 中的持久状态，因此重启后必然可见 → 满足"重启后还能管理这个临时共享"。
- 临时 target 用固定命名 `vault-{repo_id}_temp`，便于识别与清理，避免产生垃圾 target。
- 所有清理动作**幂等**（target 不存在视为成功）。
- **进入 `TEMP_SHARED` 前必须先自增 `parent_version` 并重算指纹**，确保之后派生的差异盘都基于最新内容。

### 5.7 租约与连接监控

| 项目 | 方案 |
| --- | --- |
| 租约 TTL | 120s |
| 心跳间隔 | 30s（客户端定时器，即使 UI 关闭也由 Go agent 维持） |
| 过期策略 | 超过 TTL 未续期 → `expired` |
| Reaper | 每 60s 扫描：`expired` 的 lease → 执行"踢下线"（5.8）→ `revoked` |
| 在线列表 | **来自 leases 表**（权威）；`netstat :3260` 结果作为 `cross_check` 字段展示（说明差异） |
| 强制释放 | 管理员可在管理页对某 lease 执行"强制下线"，立即置 `revoked` 并触发清理 |
| **多服务端（新增）** | `client_id` 由客户端生成并**全局唯一且稳定**（基于设备 + 安装实例，不用 MAC/IP）；**每台服务端各自独立记账**，互不可见。同一客户端可同时持有 N 条互相独立的租约（每服务端一条），心跳与过期判定**按服务端分别计算**，B 的心跳失败不得影响 A 的租约 |

**遥测指标（可采集部分）**

| 指标 | 采集方式 | 粒度 |
| --- | --- | --- |
| VHDX 逻辑/物理占用 | `file size` + `fsutil` / `Get-VHD` | 每盘，每 5min |
| 卷剩余空间 | `Get-Volume` | 每卷，每 1min |
| 母盘/差异盘数量 | DB | 实时 |
| iSCSI 网络吞吐 | `Get-NetAdapterStatistics` | 网卡级 |
| 磁盘 IO | `Get-Counter '\PhysicalDisk(*)\*'` 按 PhysicalDrive 过滤 | 盘级 |
| **单会话 IO / 单租户流量** | ❌ **无法采集** | - |

> 结论：**"按用户/按会话的读写监控"在当前技术栈下不可行**，只能做到"按盘 / 按网卡"。这一点建议在 UI 上明确降级为"资源占用视图"。

### 5.8 踢下线（Revoke）

```
服务端（唯一可行路径）：
  1. Set-IscsiServerTarget -TargetName T -Enable $false
     → 目标停用，已建立的连接会被中断（时延待实测）；**保持停用即防重连**
     （⚠️ 不要用 `-InitiatorIds @()` 来"清空白名单防重连"：空列表在 Windows 上 = 拒绝所有
       initiator，下发脚本会把空列表转成通配 `IQN:*`（= 任意 initiator，开放），
       见 iscsi_set_target.ps1；防重连的正解就是 Enabled=$false）
  2. SSE 推送 {type:'revoke', reason:'admin'|'lease_expired'} 给该客户端
  3. 客户端收到指令后：执行 5.5 卸载流程 → 随后 Delete-IscsiTarget（清理发现缓存）
  4. 客户端重连前需重新走 POST /mount，若授权已撤销则被拒
```

**必须向业务方说明的语义降级**
> "踢下线"= *服务端拒绝服务 + 通知客户端自行断开*，**不是** *服务端强制撕掉 TCP 会话*。
> 若客户端进程被杀（收不到 SSE），Windows iSCSI initiator 会**持续重连**（`-IsPersistent $true` 时更甚），直到服务端 target 保持 disabled。因此：
> - target 必须**保持 disabled**，不能踢完立刻 enable；
> - 需要"重连抑制窗口"（如 5 分钟）后再允许该用户重新挂载。

### 5.9 复制 VHDX 与存储优化 ⚠️

**这是 design.md 第 18 条要求"补充调研"的部分。**

#### 5.9.1 NTFS 下的省空间手段（D5 决策：仅用 NTFS）

| 采用 | 手段 | 效果 | 代价 / 限制 |
| --- | --- | --- | --- |
| ★★★★★ **核心** | **差异盘** | N 用户共享一份母盘，每人只存增量 | 依赖母盘一致；母盘 IO 叠加 |
| ★★★★☆ **默认** | **动态扩展 VHDX** | 未写入块在 NTFS 上表现为稀疏，零占用 | 碎片化、性能略低 |
| ★★★☆☆ 定时 | **Retrim / Compact** | 回收已删除文件占用的块 | 需磁盘未挂载或只读挂载 → 需停机窗口 |
| ❌ **不可用** | ~~ReFS 块克隆~~ | — | **NTFS 不支持**：`FSCTL_DUPLICATE_EXTENTS_TO_FILE` 仅 ReFS |
| ❌ 不推荐 | NTFS 数据去重（Dedup） | 去重可省空间 | 官方对"承载 VHD/VHDX 的卷"存在支持限制说明，且对活跃盘有明显 IO 开销与兼容风险 |
| ❌ 禁用 | NTFS 文件系统压缩 | 有限 | 官方要求 VHDX 不能放在压缩目录 |
| ⚠️ 条件可用 | 硬链接（Hard Link） | 同 inode，零额外占用 | **写入即双向可见**，不能用于需要各自演化的副本；仅适合"只读引用" |

#### 5.9.2 关键结论：「复制 VHDX 省空间」在 NTFS 上无法靠文件系统实现

MS-FSCC 附录 B 明确：`FSCTL_DUPLICATE_EXTENTS_TO_FILE` **仅在 ReFS 文件系统上受支持**（Windows Server 2016 起）。因此：

- **复制 = 全量物理拷贝**：复制 N GB 的 VHDX 需要额外 N GB 空闲空间，耗时约等于一次磁盘顺序读写。
- **要省空间，只能改变产品语义**——把用户意图映射到更合适的实现：

| 用户的真实意图 | 推荐实现 | 额外空间开销 |
| --- | --- | --- |
| 要一份完全独立、可各自演化的副本 | 全量复制 | **100%** |
| 只是想"再多一个用户使用同一份内容" | **派生差异盘**（分配） | ≈ 0（仅增量） |
| 只想额外挂载一份只读内容 | 复用同一母盘做**只读共享**，不复制 | 0 |

- **复制前的强制校验**（对应 design.md 第 18 条"只允许复制没有被 iscsi 共享出去并被用户连接的盘"）：源盘未挂载、未被 iSCSI 发布、**无 active lease**（用租约判定，而非 iSCSI 查询）。
- **禁止复制差异盘**：差异盘按路径引用母盘，复制后链路失效，无法独立挂载。

#### 5.9.3 复制的实现与必要修正

```go
func CopyVHDX(src, dst string) error {
    // 1) 前置校验：未挂载 / 未发布 / 无 active lease / 非差异盘
    // 2) 空间预检：目标卷可用空间 >= src.FileSize * 1.1
    // 3) 复制：CopyFileEx(COPY_FILE_NO_BUFFERING) 或 robocopy /J（大文件顺序 IO 更快）
    // 4) 校验：dst.FileSize == src.FileSize
    // 5) ★ 重置 DiskIdentifier（必须，见下）
    return resetDiskIdentifier(dst)
}
```

> ⚠️ **必须重置 DiskIdentifier**。直接文件复制得到的 VHDX 会**继承相同的磁盘标识**，Windows 将报
> **Event ID 158「Disk N has the same disk identifiers as one or more disks connected to the system」（KB2983588）**，并导致 VSS/备份失败。
>
> ```powershell
> # 方式一（需 Hyper-V 模块，官方确认可用）
> Set-VHD -Path 'D:\vault\disks\parents\<repo>\base.vhdx' -ResetDiskIdentifier
>
> # 方式二（无 Hyper-V 角色时，走 VirtDisk API 等价能力；枚举值需实测）
> #   OpenVirtualDisk + SetVirtualDiskInformation(SET_VIRTUAL_DISK_INFO_IDENTIFIER)
> #   https://learn.microsoft.com/en-us/windows/win32/api/virtdisk/nf-virtdisk-setvirtualdiskinformation
> ```

**关于 ODX**：若白名单卷底层是支持 ODX 的 SAN 阵列，`CopyFileEx`/`robocopy` 可能被自动 offload 到阵列执行（不占用主机 CPU 与内存带宽）。这是 NTFS 下**唯一可能"免费加速"**的路径，但**不省容量**，且需阵列支持 → 列为待实测项（14.1-T14）。

#### 5.9.4 定时回收（Retrim / Compact）

```powershell
# 仅对"空闲"的动态扩展盘执行（必须未挂载或只读挂载）
Mount-VHD -Path $vhdx -ReadOnly
Optimize-VHD -Path $vhdx -Mode Full     # 或 -Mode Retrim（需先只读挂载）
Dismount-VHD -Path $vhdx
```

调度约束：
- 只处理 `state == 'ready' && !mounted && 无 active lease` 的盘；
- 每卷同一时刻只跑一个 compact 任务（磁盘 IO 密集）；
- 记录 compact 前后的 `FileSize`，用于统计回收效果；
- 若目标环境无 Hyper-V 角色（`Optimize-VHD` 属 Hyper-V 模块），回退方案：
  ```powershell
  Mount-DiskImage -ImagePath $vhdx   # 挂载后可对卷执行 Optimize-Volume -ReTrim
  Optimize-Volume -DriveLetter V -ReTrim -Verbose
  Dismount-DiskImage -ImagePath $vhdx
  ```

### 5.10 上传与分块续传

对应 design.md 客户端第 2 条"支持存储库从非服务器目录创建（可中断的分块传输）"。

```
① POST /v1/uploads
   body: { repo_name, mode: 'copy'|'move',
           manifest: { root, files: [{path,size,mtime?,sha256?}] },
           repo_mode?: 'shared'|'exclusive',   // 默认 shared
           storage_id?: '<目标存储 ID>',        // 默认按可用空间自动选根
}
   → 服务端校验总大小 vs **用户配额**（库的**容量**即它占用的配额；库自身不再有"配额"字段） → 返回 { upload_id, chunk_size: 8MiB, missing_chunks: [...], received_bytes, total_bytes }

② 客户端并发（建议 4 路）PUT /v1/uploads/{id}/chunks/{index}
   Header: X-Chunk-SHA256（必填，小写 hex）
   Body: 原始字节
   → 服务端写入 staging/<upload_id>/<index>.part，校验 SHA256，登记 upload_chunks
   → 幂等：同 index 重复上传直接返回 200（已存在且校验一致）
   → 分块规则：整段字节流 = 按 manifest.files 顺序把各文件内容串起来再按 8MiB 切块（分块可跨文件）；
     除最后一块外必须恰好 8MiB

③ 断点续传：客户端持久化 upload_id 后调用 GET /v1/uploads/{id}
   → 服务端返回 { state, total_files, total_bytes, received_bytes, missing_chunks }
   → 客户端只重传 missing_chunks（注意：POST /v1/uploads 每次都会新建会话，缺失清单一律为全量，
     因此续传必须由客户端自行持久化 upload_id）

④ POST /v1/uploads/{id}/complete（无请求体）
   → 服务端串行重组文件树到 staging/<upload_id>/tree/
   → 校验文件数与总字节
   → 投递 Job: upload_complete（异步建库任务）
   → 响应 202 { job_id }

⑤ Job（upload_complete）执行：
   create VHDX(尺寸按 5.2 规则) → attach → format → robocopy(staging tree → V:)
   → 校验（文件数+总字节） → detach → Import → 建 repo
   → 服务端**不删除客户端本地源目录**；mode=='move' 时只清理服务端 staging，
     由**客户端**在确认建库成功后删除本地源目录（见 agent-api.md）

⑥ 失败清理：Job 失败 → 保留 staging 供重试（24h），到期 GC
```

要点：
- **大目录必须先传后建**（design.md 第 54 条），不要在服务端直接读客户端目录。
- staging 与 disks 同卷（原子 rename）。`storage_id` 显式指定时，创建会话即把 staging 建在该存储根下；
  完成建库时再次校验"暂存与目标同根"，不一致返回 `storage.staging_mismatch`（409）。
- 上传需**独立配额与限流**，否则单用户可打满磁盘。
- `move` 语义的删除顺序：**服务端 VHDX 创建并校验成功 → 客户端删源**，绝不能先删。
- manifest 为空（无文件）直接 400；符号链接会被拒绝；同名路径（大小写不敏感）会被拒绝。
- `source_dir`（POST /v1/repos 的本地目录建库）与 `/v1/fs/*` 一样受 `storage.source_roots` 白名单约束：

  | 配置 | 行为 |
  | --- | --- |
  | `storage.source_roots` 留空 | 回退为**当前生效的存储根**（storages 表启用的存储路径；DB 无记录时为 whitelist_roots 种子根） |
  | 非空 | 只允许这些目录及其子目录；其它目录返回 403 `source_dir.not_allowed` |

  ⚠️ **行为变化**：改动前 `source_dir` 只校验"绝对路径 + 存在"，任意本地目录都可用；
  改动后**默认仍兼容**（所有存储根可用），但**存储根之外**的目录（如未纳入 source_roots 的 `D:\Games`）
  会被拒绝——要使用这类目录，请显式写入 `storage.source_roots`。

### 5.11 对账 Reconciler

**为什么必须有**：Windows 侧操作是"一串不可回滚的副作用"，DB 事务无法覆盖。任一步崩溃都会造成不一致。

| 检查项 | DB 期望 | Windows 实际 | 修正动作 |
| --- | --- | --- | --- |
| VHDX 文件存在性 | `disks.vhdx_path` 存在 | 文件不存在 | 标 `error`，告警 |
| iSCSI target | `iscsi_targets` 有记录 | `Get-IscsiServerTarget` 无 | 重建 target 或标 `error` |
| target 启用状态 | `desired_enabled` | `Get-IscsiServerTarget.IsEnabled` | `Set-IscsiServerTarget -Enable` |
| target ↔ 盘映射 | `disk_id` | `Add/Remove-Mapping` 实际 | 补映射或补解绑 |
| initiator 白名单 | `iscsi_initiator_ids` | 实际 `InitiatorIds` | 全量回写 |
| initiator id 映射 | `iscsi_initiator_ids` 行数 | 实际 `InitiatorIds.Count` | 全量回写 |
| 母盘临时共享 | `parent_condition='TEMP_SHARED'` | target 是否存在 | 见 5.6 步骤 ④ |
| 孤儿 target | `iscsi_targets` 无记录 | Windows 存在 `vault-*` | 删除（需两次连续确认） |
| 孤儿 VHDX | `disks` 无记录 | `disks/` 下有文件 | 移入 `meta/orphan/`，30d 后删 |
| 孤儿差异盘 | 不存在子盘 | 存在 | 告警，不自动删（可能数据丢失） |
| 悬空分配 | `allocations` | - | 释放配额，通知用户 |
| **母盘指纹** | `disks.content_fingerprint` | 实际 size+mtime+头部哈希 | 不一致 → **告警 + 停用所有子盘 target**（母盘被绕过改写，全部差异盘已不可信） |
| **差异盘版本** | `disks.parent_version` | `repositories.parent_version` | 不一致 → 拒绝挂载并要求重新分配 |
| **DiskIdentifier 唯一性** | — | 抽样 `Get-VHD` 的 `DiskIdentifier` | 有重复 → 对复制产生的盘执行 `ResetDiskIdentifier` |

**运行时机**：
- 服务端启动后 30s（首次全量）；
- 之后每 5min（增量，只查未收敛的记录）；
- 危险操作（删盘、删 target）默认**只报告不执行**，管理页提供"一键修复"按钮。

> **启动对账不挡 HTTP（真实反馈"启动特别慢"）**：iSCSI 目标的删除/登记/建目标/建映射各需
> 10~20 秒的 PowerShell，同步跑会把"服务可对外服务"推迟一分钟以上。首次对账在**后台**执行
> （HTTP 监听立即可用），之后的周期对账接手收敛，因此不丢工作、只挪出启动关键路径。
> 另外，虚拟盘登记（`ImportVirtualDisk`，脚本内要枚举 `Get-IscsiVirtualDisk`，约 10 秒）在
> 进程内**记忆已登记路径**跳过重复执行；登记被移除或后续映射步骤失败时会主动失效，
> 避免"以为登记过其实没有"的隐性故障。

### 5.12 任务系统（Job）

| 特性 | 设计 |
| --- | --- |
| 执行器 | 单进程内固定大小 worker pool（默认 4），**同一 disk_id / repo_id 的 job 串行**（按 key 哈希到固定 worker） |
| 幂等 | `jobs.idem_key` 唯一索引；重复提交返回已有 job |
| 持久化 | 状态在 DB，进程重启后 `running` 的 job 重置为 `pending`（attempt+1） |
| 取消 | `state=cancelled` + 协作式检查点；Windows 命令不可中断，只能整条命令跑完再退出 |
| 进度 | Job 内部上报 `progress 0-100`；通过 SSE 推给发起者 |
| 重试 | 最多 3 次，指数退避；第 3 次失败 → `failed` + 告警 |
| 超时 | 每类 job 单独配置（如 `create_vhdx` 30min，`copy_vhdx` 按大小动态算） |

**关键：串行化边界**

```
需要互斥的粒度：
  同一 VHDX 文件        → 全局互斥（跨 Job、跨 API）
  同一 repo            → 串行
  同一卷的 compact     → 串行（IO 密集）
实现：
  进程内 sync.Map[string]*sync.Mutex（按资源 key）
  + DB 层面的唯一索引兜底（防止多实例）
  + 命名互斥体（全局单实例锁）
```

### 5.13 并发与锁

| 层级 | 手段 | 目的 |
| --- | --- | --- |
| 进程 | Windows：`CreateMutexW` 命名互斥体 `Global\VaultServer`；Linux：`flock` 锁文件 | 服务端单实例 |
| 资源 | 按 `disk_id` 的进程内互斥锁 | 防止同盘并发操作 |
| DB | 事务 + `SELECT ... FOR UPDATE`（PG）/ `BEGIN IMMEDIATE`（SQLite） | 防止分配超卖 |
| 文件 | 操作前 `CreateFile` with share mode=0 试探 | 探测 VHDX 是否被占用 |
| 客户端 | 同机单实例 + 每个 mount_point 互斥 | 防止重复挂载 |

**分配操作的防超卖（共享库）**

```sql
BEGIN IMMEDIATE;                       -- SQLite；MySQL 用 BEGIN + SELECT ... FOR UPDATE
SELECT used_bytes FROM repositories WHERE id=? ;
-- 校验 used + new_size <= quota_bytes
INSERT INTO allocations(...);
UPDATE repositories SET used_bytes = used_bytes + ? WHERE id=?;
COMMIT;
```

### 5.14 Linux（LVM thin + LIO）

本节是 §5.1–5.3 在 Linux 上的等价实现。上层（存储库 / 分配 / 租约 / 对账 / 上传）**代码不变**，
差异全部收敛在 `internal/platform` 的两个后端实现里。

**代码结构**

```
internal/platform/
├── backend.go            # 统一契约：DiskBackend / VolumeBackend / IscsiBackend / StorageAdmin
├── winbackend/           # Windows 聚合：winvhd(VHDX) + volume + iscsitarget(PowerShell)
├── linuxbackend/         # Linux 聚合：linuxlvm(薄卷) + liotarget(配置fs)
│                          #   → IscsiBackend 转发前先 Activate LV；DetachLun 刻意不停用 LV
├── linuxlvm/             # LVM：exec.go / thin.go / pool.go(含 dm-cache) / blockdev.go / volume.go
└── liotarget/            # LIO：configfs.go / target.go / session.go
```

装配入口在 `cmd/vault-server/platform_{windows,linux}.go` 的 `buildPlatform`，
`main.go` 不含任何平台特有类型；`platform.kind` 与构建产物不符时 `checkPlatformKind` **直接拒绝启动**。

**设备引用（ref）的不透明化**

`DiskRef` 是上层持久化的不透明标识，两平台各自解释：

| 平台 | ref 形态 | 说明 |
| --- | --- | --- |
| Windows | VHDX 绝对路径 | 与既有实现一致 |
| Linux | `/dev/mapper/<vg>-<lv>` | LV 名由 `mapLVName` 从 `repo_id/allocation_id` 派生 |

> **名字映射**：`/`、`\`、`-` 一律折叠为 `_`（避免与 LVM 的 `vg-lv` 连接符歧义），
> 并强制 `^[A-Za-z0-9_+.]+$`；因此同一 repository/allocation 在两平台得到**确定且唯一**的名字。

**⚠️ `storages.path` 的语义（易错点）**

Linux 上 `storages.path` 是一个**真实本地目录**——但它是**受管 thin LV 的挂载点**（见 §5.15），
不是 `<vg>/<thin_pool>` 这类 LVM 标识，也不是宿主机上任意一个目录。
原因：上传流程会在 root 下 `MkdirAll` 暂存目录并做"同卷"校验，
若把 path 写成 VG/pool 形式会退化成 **CWD 相对路径**；而若指向未被受管的宿主目录，
写入会绕过 thin pool 静默落到根文件系统上。

因此 Linux 的放置规则是：

- **目录类操作**（staging 暂存、trash 回收站、orphan 孤儿扫描）→ 走 `storages.path`
  （挂载点目录，位于**存储 LV** 上）；
- **虚拟磁盘类操作** → `DiskRef` **忽略 storageRoot**，改用**配置的 VG** 推导 LV 名
  （位于**磁盘 LV** 上，与存储 LV 平行、互不嵌套）。

**LV 生命周期（对照 §5.2）**

```
create   lvcreate --type thin -V <size>B -T <vg>/<thin_pool> -n <lv>
   ↓
activate lvchange -ay -K                 ← thin LV 默认 skip-activation（lv_attr 第 10 位为 'k'），必须 -K
   ↓
publish  LIO：iblock_0 backstore + TPG tpgt_1 + lun/N 符号链接
   ↓
差异盘   lvcreate -s <vg>/<lv> -n <diff_lv>      ← thin 快照；绝不可带 -L/-V
   ↓
下线     iscsiadm 侧登出 → 清理 LUN 符号链接 → lvchange -an
   ↓
delete   lvremove <vg>/<lv>                       ← 必须先下线；thin LV 无法用 vgcfgrestore 恢复
```

> **发布前必须先激活**：`linuxbackend` 在 `EnsureTarget` / `EnsureVirtualDisk` / `AttachLun` 前统一调用 `Activate`，
> 否则 LIO 的 backstore 会指向一个未激活的 dm 节点而失败。

**配额统计（对照 §5.11 / ⑬）**

- ❌ **不能**把 `lvs -o data_percent` 相加：thin 池内多个 LV 共享块，重复计数会虚高。
- ✅ 用 **`thin_ls`** 读取每个 thin LV 的 `MAPPED_BLOCKS` 得到**真实物理占用**。
- 因此"库配额 / 用户占用"的统一口径是**物理占用**，与 Windows 侧按 VHDX 文件大小记账的语义对齐。

**dm-cache（缓存加速）**

```
# 缓存插在 thin pool 的「数据子卷」上，不是插在 pool 名上
lvcreate --type cache-pool --cachemode writethrough -L <size> -n <vg>/cachepool <pv...>
lvconvert --type cache --cachepool <vg>/cachepool <vg>/<pool>_tdata
```

坑点：

1. 必须作用于 **`<vg>/<pool>_tdata`**；直接对 `<vg>/<pool>` 操作无效。
2. 改 cachemode（writethrough/writeback）**也必须**针对 `<vg>/<pool>_tdata`。
3. cache pool **不支持 `autoextend`**，容量需一次给足。
4. 缓存盘与数据盘**必须在同一 VG**。
5. 缓存设备**不进配置文件**，由 LVM 自身持久持有；管理页只展示状态与健康度（`cache_health` 非空即异常）。

**池初始化与管理接口（幂等）**

```
GET  /v1/system/lvm                 # 只读现状：VG/pool 是否存在、容量、使用率、缓存统计与健康度
GET  /v1/system/block-devices       # 枚举块设备（供缓存/数据盘选择器；含 reason 说明为何不可选）
POST /v1/system/lvm/initialize      # 幂等初始化：建 VG → 建 thin pool → 建/挂 cache pool
```

三者**仅超级管理员**可访问；Windows 后端整体返回 `501 platform.unsupported`，前端据此隐藏入口。

**块设备可用性判定（真源在服务端）**

`unavailableReason` 按优先级给出原因，前端**只按 `reason` 字段置灰，不重复实现规则**：

```
系统盘（承载 / /boot /boot/efi 或 swap）
  > 已挂载 / 已存在文件系统
  > 已是其它卷组的成员
  > 存在子分区
```

> `lsblk --json` 的完整列集（`ROTA`/`MOUNTPOINTS`）在旧版 util-linux 上可能不被支持，
> 此时自动退化为最小列集，对应字段留零值，**不因此报错**。

### 5.15 Linux 存储：受管 thin LV

> §5.1 描述的「存储」在 Windows 上是 NTFS 卷上的普通目录；在 Linux 上它升级为**受管的 thin LV**——
> 服务端负责 `lvcreate → mkfs → mount`，再把挂载点目录当作 `disks/`、`staging/`、`meta/` 的根使用。
> 这不是可选优化：Linux 上磁盘由 thin LV 承载，若"存储"仍是宿主机普通目录，
> 写入会静默落到根文件系统上（既不受 thin pool 配额约束，也会在某次挂载成功后被"藏"进挂载点底下）。

#### 5.15.1 结构性前提（不可绕过）

- **LV 不能嵌套**：存储卷与磁盘卷只能是**同一 thin pool 下的两个平行 LV**。
  存储 LV 不参与磁盘布局，只承担「上传暂存 + 回收站 + 孤儿扫描目录 + 容量配额」。
- **fail-closed 挂载**：`mount` 返回成功 ≠ 真的挂上了。挂载后必须核对 `/proc/self/mountinfo`
  （挂载点 + 设备双重比对），不通过则视为未就绪，并把该存储从落盘集合中剔除。
- **`Enabled=false` ≠ 卸载**：停用只影响"新盘放置"，不影响卷是否挂载。
- **挂载持久化不写 `/etc/fstab`、不生成 systemd unit**，完全由服务端负责（见 5.15.4）。

#### 5.15.2 数据模型（schema v3：`storage_volumes`）

```
storage_volumes (
  storage_id  PK, REFERENCES storages(id) ON DELETE CASCADE,
  kind        -- thin | lv | dir
  managed     -- true=系统创建，删除时可 lvremove；false=登记已有卷，绝不删除
  state       -- pending | ready
  ref         -- 设备引用，如 /dev/mapper/vg-lv；dir 模式为空
  file_system -- ext4 | xfs
  size_bytes  -- 分配容量；dir 模式为 0
  created_at / updated_at
)
```

- **无行 = 目录模式**（Windows / 历史存储），此时挂载点一律取 `storages.path`。
- `state=pending` 标记"`lvcreate` 成功但 mkfs/mount/落库尚未走完"的中间态，崩溃后据此识别无主 LV。
- `managed=false` 的卷**绝不能被 `lvremove`**；删除存储时只解除登记 + 卸载。

#### 5.15.3 创建流程（`StorageService.Create`）

```
校验名称/挂载点 → 生成 id → 推导默认挂载点（/var/lib/vault/storages/<slug>-<短id>）
  → 写 storages 行 → 写 storage_volumes(state=pending)
  → lvcreate --type thin -V <size>B -T <pool>  → lvchange -ay -K
  → mkfs（ext4 默认 / xfs；已有同类型 fs 则跳过，存在其它 fs 则报 existing_fs，绝不覆盖）
  → mount + mountinfo 复核
  → 回填 file_system/size_bytes，state=ready
```

- 任一步失败 → **补偿回滚**：删除本次创建的卷（仅 `managed` 且仅本次创建者）与 `storages` 行，
  绝不留下半成品；若回滚本身失败，明确 Warn "可能留下无主 LV"。
- 三种 `mode`：
  - `thin`（Linux 默认）：新建 thin LV，格式化后挂载；
  - `register_lv`（高级）：登记已有 LV，**只挂载、不格式化**；
  - `register_dir`（Windows 唯一 / Linux 高级）：纯目录，无底层卷。

#### 5.15.4 挂载持久化与 fail-closed 就绪集合

- 启动时与每 5 分钟一次对账（`runReconcile`）都会调用 `RefreshStorageMounts`：
  幂等重挂未挂载的受管卷，并把**仍未就绪**的存储写入进程内屏蔽集 `MountedStorages`。
- `Deps.storageMountReady(id)` 是唯一的就绪判断入口，且 **fail-closed**：
  `computed=false`（尚未核对过）一律返回 false。`storageSelection` 据此把未挂载的存储
  从落盘集合中剔除——"未挂载"与"已停用"是两件事，前者只是暂时不参与新盘放置。
- 运维手工 `umount` 可用于验证降级与自愈：最迟 5 分钟后被重挂并重新加入落盘集合。

#### 5.15.5 挂载 / 卸载 / 扩容 / 删除

| 接口（仅超管） | 行为 |
| --- | --- |
| `POST /v1/storages/{id}/mount` | 幂等：`lvchange -ay -K` → `mount` → mountinfo 复核 |
| `POST /v1/storages/{id}/unmount` | 有进行中上传时拒绝（409 `storage.volume_busy`）→ `umount` + 复核 |
| `POST /v1/storages/{id}/resize` | 仅 `kind=thin`；只增不减；`lvextend` 后自动 `resize2fs` / `xfs_growfs` |
| `DELETE /v1/storages/{id}` | 拒绝有盘（409 `storage.in_use`）→ 拒绝进行中上传 → `umount` → **仅 `managed=true`** 才 `lvremove` → 删登记 → 删存储行 |

- **不支持缩小**：缩容需离线 `resize2fs` + `lvreduce`，风险高，明确不做。
- 改存储 `path` 即改挂载点：已绑定底层卷（`ref` 非空）时**直接拒绝**，
  否则卷与目录脱钩（旧挂载仍占旧目录、新写入落宿主根文件系统）。
- **暂存目录就在存储卷上**：卸载/删卷前必须先确认没有进行中的上传
  （`CountActiveUploadsUnderPath`，state ∈ {open, verifying}），否则写入会静默落到宿主根文件系统。
- 全部写操作均落审计（`storage.mount` / `storage.unmount` / `storage.resize` / `storage.create` / …）。

#### 5.15.6 容量口径

- `SpaceUsageOf`：`TotalBytes` 取 `statfs` 实际值，`FreeBytes = min(statfs free, VG free)`
  ——thin pool 是**超额分配**的，两者取其小更接近真实可用。
- 已用空间由 `statfs` 的 `Bavail` 反推；列表展示口径为「已用 ÷ 分配」。
- `Name` 取承载设备（mountinfo 的 source，如 `/dev/mapper/vg-storage`），
  同一卷上的多个存储按卷去重统计。

#### 5.15.7 平台差异（必须成对修改）

| 行为 | Windows | Linux |
| --- | --- | --- |
| `Deps.StorageVol` | **刻意留 nil** | thin LV 卷后端 |
| 默认创建模式 | `register_dir` | `thin` |
| `mounted` 字段 | 恒为 true | 真实挂载状态 |
| mount/unmount/resize | `501 platform.unsupported` | 正常执行 |
| `whitelist_roots` 种子 | 生效（首次） | **跳过**（避免种出误导落盘的目录存储） |

> Windows 上若装配了非 nil 的 `StorageVol`，会让 `Mounted` 非 nil 且 `computed=false`，
> 结果是**所有存储被 fail-closed 剔除、落盘彻底失效**——必须避免。

#### 5.15.8 明确不在本轮范围

- Linux LV 级孤儿/对账扫描（沿用既有的文件级扫描）。
- 每存储硬配额强制（thin LV 的 `-V` 只是名义容量，未做写满保护）。
- 第三方迁移工具。

---

## 6. 服务端实现

### 6.1 技术栈

| 层 | 选型 |
| --- | --- |
| 语言 | Go 1.23+，**`CGO_ENABLED=0`**（纯 syscall，不需要 cgo，见 10.4） |
| HTTP | `net/http` + `chi`（轻量路由）或标准库 1.22+ `ServeMux` |
| DB | `database/sql` + `sqlx`；SQLite 用 `modernc.org/sqlite`（纯 Go，免 cgo）；MySQL 用 `go-sql-driver/mysql` |
| 迁移 | `golang-migrate` 或自研版本表 |
| 日志 | `log/slog`，JSON 输出到文件（按你的偏好：只落文件） |
| 配置 | YAML + 环境变量覆盖 + 热重载（`fsnotify`） |
| 证书 | 自建 CA（`crypto/x509`），支持签发/吊销 |
| 密码 | `argon2id`（`golang.org/x/crypto/argon2`） |
| 加密 | `crypto/aes` GCM |
| PowerShell 调用 | `os/exec` + 模板化脚本 + JSON 输出（Windows） |
| LVM 调用 | `os/exec` 调 `lvm2` 工具链（`lvcreate/lvconvert/lvs/vgs/thin_ls/lsblk`），**argv 传参、无 shell**（Linux） |
| LIO 调用 | 直接读写 `/sys/kernel/config/target/`（configfs），**无 targetcli、无守护进程**（Linux） |
| 前端资源 | `embed.FS` 内嵌，单端口提供 |

### 6.2 目录结构

```
cmd/vault-server/
  main.go               # 平台无关：装配依赖、启动 HTTP、停机
  launcher_{windows,other}.go   # 双击启动器（Windows）/ systemd 提示（Linux）
  stop_{windows,other}.go       # 停机机制：命名事件（Windows）/ PID+SIGTERM（Linux）
  platform_{windows,linux}.go   # ★ buildPlatform：按构建产物装配平台后端
internal/
  api/            # HTTP handler、路由、DTO
    middleware/   # auth, ratelimit, audit, recover, requestid
  app/            # 应用服务（编排），事务边界在此
    repo_svc.go disk_svc.go iscsi_svc.go user_svc.go lease_svc.go upload_svc.go platform.go
  domain/         # 实体、值对象、状态机、领域错误
  store/          # 数据访问
    sqlite/ mysql/ migrations/
  platform/       # ★ 平台能力抽象（唯一含 OS 特定代码的层）
    backend.go    #   统一契约：DiskBackend/VolumeBackend/IscsiBackend/StorageAdmin
    winbackend/   #   Windows 聚合（build: windows）
    linuxbackend/ #   Linux 聚合（build: linux）
    winvhd/       #   VirtDisk 封装（go-winio/vhd）
    winps/        #   PowerShell 执行器 + 脚本模板
    iscsitarget/  #   Windows IscsiTarget 模块封装
    volume/       #   Windows 分区/格式化/挂载点
    linuxlvm/     #   LVM2：thin/快照/dm-cache/块设备枚举（build: linux）
    liotarget/    #   LIO：configfs 直控 target/ACL/会话（build: linux）
    fs/           #   文件复制/空间检查/DiskIdentifier 重置
    net/          #   netstat 解析、网卡统计
  lock/           # 单实例锁：CreateMutex（Windows）/ flock（Linux）
  job/            # Job worker、调度、幂等
  reconcile/      # 对账器
  lease/          # 租约管理与 Reaper
  cert/           # CA、签发、吊销
  config/         # 含 platform.{kind,lvm,iscsi} 段
  version/
```

**分层规则**：`platform/` 之下不允许出现业务概念；`app/` 之上不允许出现平台 API（`win*`/`linux*` 类型一律不出现在 `app`/`api`）。
**任何新增平台差异都必须收敛到 `buildPlatform` 的两个构建变体里**，`main.go` 保持平台无关。
这样换平台只需替换 `platform/` 的后端实现，上层（存储库/分配/租约/对账/上传）零改动。

### 6.3 API 设计（REST 摘要）

```
# 认证
POST   /v1/auth/login                     用户名密码（管理页）
POST   /v1/auth/mtls                      证书登录（客户端）
POST   /v1/auth/refresh

# 存储库
GET    /v1/repos                          列表（支持分组/搜索/分页）
POST   /v1/repos                          创建（bare / from-dir / from-upload）
GET    /v1/repos/{id}
PATCH  /v1/repos/{id}                     改名、分组、配额、meta
DELETE /v1/repos/{id}
POST   /v1/repos/{id}/copy               复制（返回 job）
GET    /v1/repos/{id}/stats               容量/占用/差异盘数
GET    /v1/repos/{id}/members
PUT    /v1/repos/{id}/members             覆盖式设置（仅 owner）

# 分配
POST   /v1/repos/{id}/allocations         分配给用户（建差异盘或独享映射）
GET    /v1/repos/{id}/allocations
DELETE /v1/allocations/{id}

# 挂载（客户端）
POST   /v1/allocations/{id}/mount         获取 MountSpec + 创建租约
POST   /v1/leases/{id}/heartbeat
POST   /v1/leases/{id}/mounted
POST   /v1/leases/{id}/release
GET    /v1/leases                         在线列表（来自 DB）
DELETE /v1/leases/{id}                    管理员强制下线

# 母盘临时共享
POST   /v1/repos/{id}/parent/temp-mount
DELETE /v1/repos/{id}/parent/temp-mount
GET    /v1/repos/{id}/parent/temp-mount

# 上传
POST   /v1/uploads
GET    /v1/uploads/{id}
PUT    /v1/uploads/{id}/chunks/{index}
POST   /v1/uploads/{id}/complete
DELETE /v1/uploads/{id}

# 目录浏览与统计（源目录选择；✅ 已实现，白名单见 storage.source_roots）
GET    /v1/fs/roots                       可浏览的源目录根 {items:[{name,path,exists}]}
GET    /v1/fs/browse?path=<绝对路径>      列出子目录（只列目录，不列文件）{path,parent?,root,exists,items[]}；path 必填
GET    /v1/fs/stat?path=<绝对路径>        递归统计 {path,exists,file_count,total_bytes,truncated}

不存在的路径返回 **200 + `exists:false`**（browse 的 items 为空、stat 的计数为 0），
首次建盘前目录尚未创建属正常状态；仅"存在但不是目录 / 符号链接"仍是 400 `system.invalid_param`。

# 存储（§5.1 / §5.15）
GET    /v1/storages                       列表（任何登录用户可读，建库表单需要下拉数据）
POST   /v1/storages                       创建（仅 super_admin）
                                          body: {name, mount_point?, enabled?,
                                                 mode?(thin|register_dir|register_lv),
                                                 size_bytes?, file_system?, ref?}
PATCH  /v1/storages/{id}                  改名 / 改挂载点 / 启停（仅 super_admin）
DELETE /v1/storages/{id}                  仅 super_admin；拒绝有盘 / 进行中上传，
                                          仅删除系统创建的卷（managed=true），见 §5.15.5
POST   /v1/storages/{id}/mount            挂载底层卷（仅 super_admin；幂等）
POST   /v1/storages/{id}/unmount          卸载底层卷（仅 super_admin；有进行中上传 → 409 storage.volume_busy）
POST   /v1/storages/{id}/resize           扩容（仅 super_admin；仅 kind=thin，只增不减）
                                          body: {size_bytes}

storage DTO 在基础字段之外增补：kind(thin|lv|dir，空=目录模式)、mounted(bool，目录模式恒 true)、
ref(设备引用)、size_bytes(分配容量)。Windows 上 mount/unmount/resize 返回 501 platform.unsupported。

# 任务
GET    /v1/jobs/{id}
GET    /v1/jobs?state=running
POST   /v1/jobs/{id}/cancel

# 用户与证书（管理员）
GET/POST/PATCH/DELETE /v1/users
GET/POST/DELETE       /v1/users/{id}/certificates
POST   /v1/users/{id}/certificates/{cid}/revoke

# 系统
GET    /v1/system/health
GET    /v1/system/info                    ★ 匿名可访问：版本契约（api_version/server_version/
                                          client_compat/features/server_instance_id/server_name）
                                          + 能力探测（platform_kind? WinTarget? LVM? Hyper-V? 卷可用空间? DB 后端?）
GET    /v1/system/bootstrap               ★ 匿名：初始化状态（见 3.5）
POST   /v1/system/bootstrap               ★ 匿名且仅未初始化时可用：创建首个超级管理员并下发会话
GET    /v1/system/events                  SSE
GET    /v1/system/audit
GET/PATCH /v1/system/settings             仅可改 server.name（别名）等运行时可改项；
                                          ★ client_compat 为**只读展示**（真源在 config.yaml，见 3.4.2）
```

> **服务端不提供任何静态资源路由**：无 `/admin`、无 `web/` 目录（见 D13）。
> 前端产物只随客户端分发，服务端不参与其分发。

**分块上传（§5.10）**

```
POST   /v1/uploads                        创建上传会话（含 manifest 校验与配额检查）
                                          body 可选扩展字段：repo_mode(shared|exclusive，默认 shared)、
                                          storage_id(默认自动选；校验同 RepoService.Create)、
                                          quota_bytes(默认 0=不限)
GET    /v1/uploads/{id}                   查询状态（断点续传：返回 missing_chunks）
PUT    /v1/uploads/{id}/chunks/{index}    上传分块（Header: X-Chunk-SHA256；幂等）
POST   /v1/uploads/{id}/complete          重组文件树 → 投递 JobUploadComplete（upload_complete）
DELETE /v1/uploads/{id}                   中止并清理暂存
```

**iSCSI 授权与会话**

```
GET    /v1/repos/{id}/iscsi                      列出该库下的目标
GET    /v1/iscsi/targets/{id}                    单目标详情（授权列表 / 映射 / 启用状态）
PUT    /v1/iscsi/targets/{id}/authorization      ★ 全量替换 initiator 白名单
POST   /v1/iscsi/targets/{id}/authorization      追加一条
DELETE /v1/iscsi/targets/{id}/authorization      移除一条
PUT    /v1/iscsi/targets/{id}/auth               设置鉴权（none | ip | chap）
POST   /v1/iscsi/targets/{id}/disable            停用目标（等价踢下线）
```

**对账**

```
GET    /v1/system/reconcile              仅 super_admin；返回最近一次对账报告
                                         {at, checked, issues[], not_checked[], truncated}
```
> 对账**默认只报告不自动修复**，危险操作（删孤儿 target/VHDX）一律不自动执行。

**平台存储池（LVM thin pool / dm-cache；仅 super_admin，Windows 返回 501）**

```
GET    /v1/system/lvm                    存储池与缓存现状（见 §5.14）
                                         {kind, vg, thin_pool, exists, size_bytes, free_bytes,
                                          data_percent, metadata_percent, cache_attached,
                                          cache_mode, cache_chunk_size, cache_policy,
                                          cache_dirty_blocks, cache_read/write_hits/misses,
                                          cache_devices[], cache_health}
GET    /v1/system/block-devices          枚举块设备（供"数据盘 / 缓存盘"选择器）
                                         items[]: {name, path, size_bytes, model, rotational,
                                                   system, has_fs, has_pv, vg_name, partitions, reason}
POST   /v1/system/lvm/initialize         幂等初始化：建 VG → 建 thin pool → 建/挂 cache pool
                                         body: {vg?, thin_pool?, hdd_devices[], chunk_size?,
                                                metadata_size?, cache_devices[], cache_chunk_size?,
                                                cache_policy?, cache_mode?}
```

> 池**不存在不是错误**：`GET /v1/system/lvm` 返回 `exists: false`，由前端引导执行一次初始化。
> 三个接口在 Windows 后端统一返回 `501 platform.unsupported`（错误码 `platform.unsupported`），
> 前端据此**隐藏入口**，而不是把裸错误暴露给用户。

**客户端本地代理**

服务端之外，客户端还有一个本机 Go 代理 `Vault-Agent`，负责 iSCSI 连接、磁盘上线、
挂载点分配、租约心跳与接收服务端「踢下线」指令。其本地 HTTP 契约见
[agent-api.md](./agent-api.md)，与上面这套服务端 API 是两条独立通道。

### 6.4 中间件与横切

| 中间件 | 职责 |
| --- | --- |
| `requestid` | 注入 trace id，贯穿日志、审计、错误响应 |
| `recover` | panic 兜底 → 500 + 堆栈入日志 |
| `authn` | 解析 mTLS 证书 / Session / Bearer，产出 `Principal` |
| `authz` | 按路由声明所需权限（`super_admin` / `user` / `repo_member`）校验 |
| `ratelimit` | 全局 + 按 IP + 按用户；登录接口更严格 |
| `audit` | 所有写操作落 `audit_logs`（含前后值） |
| `idempotency` | 读 `Idempotency-Key` 头，命中则回放响应 |
| `i18n` | 解析 `Accept-Language`，**错误响应只返回错误码 + 参数**，文案由前端翻译 |

### 6.5 错误码规范

```go
type AppError struct {
    Code    string         // 稳定标识，如 "disk.repo.not_found"
    HTTP    int
    Args    map[string]any // 供前端插值
    Cause   error          // 内部原因，不返回客户端
}
```

错误码分层：`auth.*`、`repo.*`、`disk.*`、`iscsi.*`、`lease.*`、`upload.*`、`job.*`、`storage.*`、`system.*`。
**禁止把 PowerShell 原始报错直接返回给客户端**，需映射为业务错误码，原始文本只进日志。

存储域新增（§5.15）：`storage.volume_not_found`(404)、`storage.not_mounted`(409)、
`storage.volume_busy`(409，带 `active_uploads`)、`storage.volume_failed`(500，带 `reason`)；
平台无该能力统一 `platform.unsupported`(501，Windows 上的 mount/unmount/resize 与 thin 创建)。

### 6.6 平台层关键实现片段

```go
// platform/winps/exec.go
type Runner struct{ PwshPath string }

func (r *Runner) RunScript(ctx context.Context, script string, params map[string]any) ([]byte, error) {
    // 1) 参数通过 -Args 传递，绝不做字符串拼接（防注入）
    // 2) 统一包装：$ErrorActionPreference='Stop'; try{ ... ; ConvertTo-Json -Compress } catch { ... }
    // 3) 设置超时 ctx
    // 4) 解析 stdout 的最后一行 JSON；stderr 入日志
}
```

```go
// platform/winps/scripts/iscsi_publish.ps1
param([string]$DevicePath, [string]$TargetName, [string[]]$InitiatorIds,
      [string]$ChapUser, [string]$ChapSecret)
$ErrorActionPreference = 'Stop'
try {
    if (-not (Get-IscsiVirtualDisk -Path $DevicePath -ErrorAction SilentlyContinue)) {
        Import-IscsiVirtualDisk -Path $DevicePath | Out-Null
    }
    if (Get-IscsiServerTarget -TargetName $TargetName -ErrorAction SilentlyContinue) {
        Set-IscsiServerTarget -TargetName $TargetName -InitiatorIds $InitiatorIds
    } else {
        New-IscsiServerTarget -TargetName $TargetName -InitiatorIds $InitiatorIds | Out-Null
    }
    if ($ChapUser) { Set-IscsiServerTarget -TargetName $TargetName -EnableChap `
                        -Chap (New-Object System.Management.Automation.PSCredential(
                                 $ChapUser, (ConvertTo-SecureString $ChapSecret -AsPlainText -Force))) }
    $mapping = Get-IscsiVirtualDisk -Path $DevicePath | Select-Object -ExpandProperty TargetName
    if ($mapping -notcontains $TargetName) {
        Add-IscsiVirtualDiskTargetMapping -TargetName $TargetName -DevicePath $DevicePath
    }
    Set-IscsiServerTarget -TargetName $TargetName -Enable $true
    [pscustomobject]@{ ok = $true } | ConvertTo-Json -Compress
} catch {
    [pscustomobject]@{ ok = $false; message = $_.Exception.Message } | ConvertTo-Json -Compress
    exit 1
}
```

> 注意 `Add-IscsiVirtualDiskTargetMapping` 的 `-TargetName` / `-DevicePath` 参数名请以 `Get-Command` 实测为准（不同版本存在差异）。

**Linux 侧对应实现**

```go
// platform/linuxlvm/exec.go —— LVM 一律走 CLI，参数以 argv 传递（绝不做 shell 拼接）
func (m *Manager) run(ctx context.Context, name string, args ...string) (string, error) {
    ctx, cancel := context.WithTimeout(ctx, m.timeout)
    defer cancel()
    cmd := exec.CommandContext(ctx, name, args...)   // 无 shell，无注入面
    var stdout, stderr bytes.Buffer
    cmd.Stdout, cmd.Stderr = &stdout, &stderr
    err := cmd.Run()
    // 失败时把 stderr 一并带回，便于定位（如 "Volume group not found"）
    ...
}
```

```go
// platform/liotarget/configfs.go —— LIO 完全通过 configfs 文件系统操作，无外部依赖、无守护进程
const (
    tpgName      = "tpgt_1"
    iblockPlugin = "iblock_0"
)

// 创建一个 target 的等价 shell 操作（便于排障时手工核验）：
//   mkdir  /sys/kernel/config/target/iscsi/<IQN>
//   mkdir  /sys/kernel/config/target/iscsi/<IQN>/tpgt_1
//   mkdir  /sys/kernel/config/target/core/iblock_0/<backstore>       # 写入 control 指定 /dev/mapper/<vg>-<lv>
//   ln -s  /sys/kernel/config/target/core/iblock_0/<backstore> \
//          /sys/kernel/config/target/iscsi/<IQN>/tpgt_1/lun/lun_0
//   mkdir  /sys/kernel/config/target/iscsi/<IQN>/tpgt_1/acls/<initiator IQN>
//   echo 1 > .../acls/<IQN>/write_protect                            # 只读 ACL（母盘临时共享）
```

> **initiator 名字解析**：上层 `domain.InitiatorID.String()` 形如 `"Type:Value"`，
> LIO 侧只需**纯 IQN 值**，因此按**第一个** `:` 切分（IQN 自身含 `:`，不能全切）。
>
> **target 名归一化**：`normalizeTargetName` 统一转小写 IQN —— configfs 路径**大小写敏感**，
> 若不归一化会出现"同义不同名"的重复 target。

### 6.7 可观测性

| 类别 | 内容 |
| --- | --- |
| 日志 | JSON 到文件，按日切分，保留 30 天；字段含 `trace_id, user, action, resource, duration_ms` |
| 指标 | `/metrics` 暴露 Prometheus 格式（可选）：repo 数、盘数、活跃租约、Job 队列长度、各操作耗时直方图 |
| 健康 | `/v1/system/health` 检查 DB、WinTarget 服务、白名单卷可用性 |
| 审计 | 独立表 + 管理页查询与导出 CSV |

> 按你的偏好"只输出日志到文件，忽略 OTel 与链路追踪"，这里去掉了 trace 上报，仅保留本地 `trace_id` 串联。

---

## 7. 客户端实现

### 7.1 进程架构

```
Electron Main (Node)
  ├─ 创建 BrowserWindow（加载 React 渲染进程）
  ├─ 启动/守护 Vault-Agent.exe（Go sidecar）
  │    ├─ 随机端口 + 一次性 Token 写入 stdin
  │    └─ 崩溃自动重启（最多 5 次/分钟）
  ├─ 系统托盘（Tray）：显示挂载状态、打开面板、退出
  └─ 单实例锁（app.requestSingleInstanceLock）

Vault-Agent (Go)
  ├─ 本地 HTTP on 127.0.0.1:<port>，仅接受带 Token 的请求
  ├─ ServerRegistry：多服务端注册表（每个服务端一套独立会话，见 3.4.1）
  │    ├─ 每服务端：独立 mTLS 通道 + 独立 SSE + 独立租约心跳 + 独立熔断器
  │    └─ 版本兼容协商（匿名 /v1/system/info，见 3.4.2）
  ├─ iSCSI 挂载/卸载引擎（挂载点按服务端别名命名空间隔离，见 3.4.5）
  ├─ 自更新（主服务端优先 + 失败回退；统一发布方签名校验，见 3.4.3/3.4.4）
  └─ 本地配置（%ProgramData%\Vault\config.json，servers[] 数组）
```

**为什么 Go 用 sidecar 而不是 cgo/共享库**：Electron 的 Node ABI 与 Go 的 c-shared 组合调试困难、崩溃会拖垮整个 UI；sidecar 隔离性好、可独立重启、可单测。

> **多服务端对架构的影响**：agent 从"单服务端代理"升级为"**多服务端聚合器**"。这是本轮新增能力带来的主要改动，改动点集中在 §3.4，服务端几乎不动。

### 7.2 本地挂载能力（platform 层复用服务端代码）

客户端与服务端可共享 `platform/winps`（只读部分），客户端额外需要：

| 能力 | 实现 |
| --- | --- |
| 目标门户发现 | `New-IscsiTargetPortal` / `Update-IscsiTarget` |
| 连接 | `Connect-IscsiTarget`（含 CHAP） |
| 会话检查 | `Get-IscsiSession` / `Get-IscsiConnection` |
| 磁盘上线 | `Set-Disk -IsOffline $false -IsReadOnly $false` |
| 盘符挂载 | `Add-PartitionAccessPath -AssignDriveLetter` |
| 目录挂载 | `Add-PartitionAccessPath -AccessPath <dir>` |
| 卸载 | `Remove-PartitionAccessPath` → `Disconnect-IscsiTarget` |
| 快捷驱动器 | `WScript.Shell` / `IShellLink` 创建 Shell Link |

### 7.3 自动挂载与开机启动

```
启动序列：
  1. 读本地配置 + 从服务端拉取用户配置（见 design.md 服务端-本地用户-4）
  2. 对每个标记 auto_mount 的存储库：
     - 请求 POST /allocations/{id}/mount
     - 成功 → 挂载 → 执行 post_script
     - 失败 → 记录到"待处理"列表，托盘提示，不阻塞其它库
  3. 并发限制：同时最多 2 个挂载任务（避免网络/磁盘争抢）
开机启动：注册 HKCU\...\Run 或计划任务（后者可提权，推荐）
```

### 7.4 自动更新

对应 design.md 第 67~69 条 + 「多服务端」第 2 条：**启动时自动检查** + **设置页手动检查**两条入口，共用同一套更新内核。

| 触发入口 | 行为 |
| --- | --- |
| 客户端启动 | 静默检查；有新版则按"不影响挂载"策略后台下载，就绪后提示或按配置自动重启更新 |
| `客户端-设置-更新` 手动检查 | 显式查询，展示"当前版本 / 最新版本 / 更新日志 / 包大小 / **当前更新源**"，用户点击"立即更新"后走同一流程 |
| 服务端强制 | 服务端在版本契约中声明 `client_compat.min`；低于该值的客户端在 `system/info` 阶段即被标为 `NEEDS_CLIENT_UPGRADE`，UI 引导升级 |

#### 7.4.1 多服务端下的更新源（对应 3.4.3）

```
更新源优先级：主服务端(primary) → 其他 COMPATIBLE 服务端（按配置顺序）
失效条件：全部不可用 → 提示"暂时无法检查更新"，不静默失败
选择可见：设置页展示"当前更新源：<服务端名>"
```

> ⚠️ design.md 原文"只以第一个设置的服务器为准"若**无回退**，会产生两个问题：
> ① 主服务端版本较旧时客户端**永远升不上去**（要连其他服务端需更高版本，但只能从旧源取包）；
> ② 主服务端离线时**完全无法更新**。因此落地方案为"主服务端 + 失败回退"。

#### 7.4.2 更新包签名（对应 3.4.4）

**统一发布方签名，服务端只做 mirror**：

```
[发布方私钥] 签名安装包  →  分发到各服务端（仅存储/分发，不签名）
                                    ↓
客户端：[内置发布方公钥] 验签 → 校验 SHA256 → 版本单调性 → 目标版本兼容性
```

**四重校验链（缺一不可）**

| 顺序 | 校验项 | 失败处理 |
| --- | --- | --- |
| 1 | 发布方签名（编译期内置公钥） | 拒绝安装 + 告警（疑似篡改） |
| 2 | 包 SHA256 与签名内摘要一致 | 拒绝安装 |
| 3 | **版本单调性**：新版本 > 当前版本 | 拒绝（防被控服务端下发含漏洞的旧版实施降级攻击） |
| 4 | 目标版本与**所有服务端**的兼容区间 | 若升级后会导致某服务端不兼容 → 提示用户确认（见下） |

> ⚠️ **服务端绝不持有更新签名私钥**。若每台服务端各自签名，客户端需信任 N 个公钥，攻击面随服务端数量线性放大——任一服务端被攻陷即可推恶意更新。

#### 7.4.3 升级导致服务端不兼容的处理

多服务端下，升级客户端可能使其超出**某个旧服务端**的 `client_compat.max`：

| 场景 | 处理 |
| --- | --- |
| 升级后所有服务端仍兼容 | 正常升级 |
| 升级后会导致某服务端不兼容 | **升级前明确提示**："升级后将无法连接服务端 X（需其管理员升级）"，由用户确认；并记录该服务端为 `NEEDS_SERVER_UPGRADE` |
| 用户拒绝 | 保持当前版本，不升级；该服务端仍可用 |

#### 7.4.4 已实现的发布与校验链路

```
[发布方离线机器]                          [服务端: 仅 mirror]            [客户端]
Vault-Server sign keygen ─► 私钥(离线保存)                                     内置公钥(编译期注入)
Vault-Server sign release ─► manifest.json + .sig + artifacts/  ──分发──►  artifacts_dir/
                                                          │
                                              GET /v1/system/update/manifest  ← 原样字节
                                              GET /v1/system/update/signature ← 原样字节
                                              GET /v1/system/update/artifacts/{name} ← 支持 Range
                                                          │
                                                          ▼
                                    Vault-Agent: 先对原始字节验签 → 再解析 → 版本单调性
                                                 → 下载(边下边算 SHA256) → Authenticode(尽力)
                                                 → staged → pending.json → agent 复制自身副本执行替换 + 重启
```

**四重校验链（客户端强制，任一失败即拒绝）**

| 顺序 | 校验 | 失败结果 |
| --- | --- | --- |
| 1 | Ed25519 签名（**对 manifest 原始字节，先于解析**） | `update.bad_signature` 403，硬失败 + ERROR 日志（含公钥指纹） |
| 2 | 产物 SHA256 与 manifest 一致 | 拒绝安装，删除 `.part` |
| 3 | 版本单调性（`manifest.version > 当前版本`，相等也拒绝） | `not_newer`，防降级攻击 |
| 4 | 目标版本仍落在服务端 `client_compat` 区间 | 返回 `breaks_servers` 由用户确认（见 §7.4.3） |

**发布流程（发布方）**

```powershell
# 1) 生成发布密钥（一次性；私钥离线保存，绝不入库——.gitignore 已挡 keys/ 与 *.pem）
Vault-Server sign keygen -out-dir .\keys

# 2) 构建并注入公钥（生产构建必须注入，否则回退到开发公钥、无法验签成功）
$pub = Get-Content .\keys\update-public.txt -Raw
go build -trimpath -ldflags "-s -w -X vault/internal/agent.updatePublicKeyBase64=$pub" `
    -o dist/Vault-Agent.exe ./cmd/vault-agent

# 3) 打包发布目录（签名工具挂在 vault-server 的 sign 子命令上，仅发布方使用）
Vault-Server sign release -dir .\release -channel stable -version 0.2.0 `
    -notes "修复若干问题" -key .\keys\update-private.pem `
    -artifact agent=.\dist\Vault-Agent.exe -artifact client=.\dist\vault-client-setup.exe

# 4) 发布前自检（校验签名与产物 SHA256）
Vault-Server sign verify -dir .\release -pub .\keys\update-public.txt
```

然后把 `release/` 内容放到服务端的 `update.artifacts_dir`。

**关键安全约束（实现已遵守）**

| # | 约束 |
| --- | --- |
| 1 | **服务端零信任改造能力**：只读文件、逐字节透传，**不解析、不改写、不重算摘要、不选产物**；即使服务端被攻陷也无法伪造更新 |
| 2 | **验签先于解析**：`VerifyAndParse` 对 raw 字节验签后才 `Unmarshal`，避免被篡改的 `sha256` 字段被当可信 |
| 3 | 公钥**编译期注入**（ldflags），不从网络下发；未注入时 **fail-closed**（宁可升不上去，不接受任意签名） |
| 4 | 私钥只出现在 `Vault-Server sign` 子命令；服务端运行时/代理/日志中一概不出现（日志仅记录公钥指纹） |
| 5 | **TOCTOU 防护**：`apply` 时重新拉取并**再次验签**，不复用 `check` 的结果 |
| 6 | 有活跃挂载一律 409 `agent.busy_mounts`，**绝不自动卸载**；更新退出时也不卸载（由重启后的自动挂载恢复） |
| 7 | 原子替换：`.part` 写满校验后再改名；替换器（agent 复制出的临时副本）先写 `target.new` 再 rename；替换前备份；**连续 3 次启动失败自动回滚** |

**已知降级项**

| 项 | 现状 | 原因 |
| --- | --- | --- |
| 多服务端更新源回退（§7.4.1） | 仅用当前会话的服务端；失败即 `server_unreachable` | 代理只持有单会话，缺候选列表（依赖前端推送或新增 `GET /agent/servers`） |
| Authenticode 强制校验 | `Get-AuthenticodeSignature` 已接入，失败仅 WARN | 无代码签名证书时会挡死所有更新；完整性已由 Ed25519 + SHA256 保证 |
| Electron 本体更新 | 由 `electron-updater` 负责；服务端已能镜像 `kind=client` 产物 | 按 §7.4 的分工 |
| 代理侧审计落库 | 以 ERROR 级结构化日志（`action=self_update.verify` / `self_update.rollback`）留痕 | 审计表属服务端，代理无审计存储 |

**难点与方案**

| 难点 | 方案 |
| --- | --- |
| 运行中的 exe 无法覆盖 | 双进程切换：新版下载到 staged → agent 复制**自身**为临时副本并以其执行替换 → 父进程退出 → 副本替换 + 重启 |
| Electron 本体更新 | 使用 `electron-updater`（已定 Electron 方案）+ NSIS / differential 包；Go agent 走独立更新通道 |
| 完整性 | 安装包代码签名 + 更新包 SHA256 + 服务端下发签名校验，三重校验缺一不可 |
| 回滚 | 保留上一个版本目录，连续 3 次启动失败自动回滚并上报服务端 |
| 不影响挂载 | 更新前检查是否存在活跃挂载；有则**延迟到卸载后**执行，或在手动入口明确提示"更新将中断 N 个挂载" |
| 灰度 | 服务端可配置 `channel: stable\|beta`；支持按用户/机器灰度 |

> **必须明确的产品约束**：自动更新**无法做到"零中断且不重挂载"**——Electron 与 sidecar 都需重启，iSCSI 会话会短暂断开。因此"不影响挂载行为"应理解为"**更新流程不会破坏挂载配置，重启后能自动恢复挂载**"，而不是"更新期间挂载一直在线"。建议在 UI 文案上如实表述。

#### 7.4.5 渲染层资源热更（client_web）

与"Electron 本体更新"是**两条独立通道**，务必区分：

| 维度 | Electron 本体更新（agent / client 产物） | 渲染层资源热更（client_web 产物） |
| --- | --- | --- |
| 变更内容 | Go 代理 exe / Electron 运行时代码与打包资源 | 仅渲染层静态资源（Vite 产出的 `dist/`） |
| 是否重启进程 | **是**（替换 exe/客户端后重启，挂载短暂中断） | **否**（只改磁盘上的资源目录，界面下次加载即生效） |
| 适用边界 | 涉及主进程 / 原生能力 / 依赖变更 | 纯界面逻辑、样式、文案等前端迭代 |
| 回滚 | 备份 + 连续 3 次启动失败自动回滚（§7.4.4） | 保留最近 2 个版本目录；出错时 `current.json` 仍指向旧版本 |
| 风险 | 进程重启导致 iSCSI 会话短暂断开 | 主进程 API 契约变更会导致旧渲染层调用失败；因此热更只应发布"兼容当前主进程"的资源 |

实现链路（服务端只做 mirror，验签/校验全在代理侧）：

```
Vault-Server sign release -artifact agent=... -artifact client_web=dist.zip
        │  manifest.artifacts[].kind = "client_web"（平台无关，不要求 os/arch）
        ▼
代理 POST /agent/update/web/apply（202，后台异步）
   校验清单签名(check) → 下载(边下边算 sha256) → 校验摘要 → 版本单调性(仅允许升级)
   → 解压到 <webapp>/<version>.tmp-<rand>/（拒绝 zip slip / 限制总大小）
   → 校验 index.html 存在 → 原子改名为 <webapp>/<version>/
   → 临时文件 + MoveFileEx 原子替换 <webapp>/current.json
```

**落盘契约（与 Electron 主进程冻结，不可随意改动）**：

```
%ProgramData%\Vault\webapp\<version>\index.html   # 渲染层产物
%ProgramData%\Vault\webapp\current.json           # {"version","dir","activated_at"}
```

主进程读取 `current.json` 决定加载哪个版本的资源；指针缺失时回退到本体内置资源。
代理提供 `GET /agent/update/web` 查询状态、SSE `web_update` 事件推送进度，
页面刷新后据此恢复展示（见 docs/agent-api.md「客户端资源热更」）。

**生效时机（两段）**

1. **启动时选层**：热更层版本必须**严格高于**内置层才会被采用。内置层版本取打包目录里的
   `package.json`（**不用 `app.getVersion()`**：应用 package.json 缺 `version` 字段时它会回退成
   Electron 自身的大版本号，那会把所有 `0.1.x` 热更层永久拒掉）。这样做的目的是防止旧的热更资源
   把界面永久钉在老版本上（真实工单：应用已升级到 0.1.37，指针仍停在 0.1.28）。
2. **运行中静默热更**：会话建立后（客户端每次启动都会推送会话）代理自动执行一次
   `POST /agent/update/web/apply`；激活成功后经 SSE `web_update` 通知渲染层，渲染层请求主进程
   重新解析并加载新层（`vault:apply-web-layer`），**无需重启客户端**，也不打断既有挂载。
   主进程按"解析出的入口文件是否等于已加载文件"去重（相同即返回 `applied=false`），
   因此不会形成重载循环。

### 7.5 i18n

- 语言包为纯 KV：`locales/{zh-CN,en-US,ja-JP,ko-KR}.json`，扁平 key（如 `repo.create.title`）。
- **服务端只返回错误码与参数**，前端用 `t(code, args)` 翻译；避免服务端维护多语言文案。
- 前端（客户端 + 管理页）共用同一套 locale 包。
- 语言切换即时生效（zustand/jotai 全局 store + React Context）。

### 7.6 客户端配置项

```jsonc
{
  "primary_server_id": "srv-7f3a...",     // ★ 主服务端（更新源），用户可切换
  "servers": [                            // ★ 多服务端数组
    {
      "server_instance_id": "srv-7f3a...",
      "server_name": "家中的存储服务器",
      "alias": "home",                    // ★ 挂载点命名空间用，唯一、符合文件系统规范
      "url": "https://vault.example.com:8443",
      "client_cert": "certs/home.pem",    // ★ 每服务端独立的证书/密钥
      "client_key": "certs/home.key",
      "ca": "certs/home-ca.pem",          // ★ 每服务端独立的 CA
      "auto_connect": true
    },
    {
      "server_instance_id": "srv-91cd...",
      "server_name": "公司测试机",
      "alias": "office",
      "url": "https://10.0.0.5:8443",
      "client_cert": "certs/office.pem",
      "client_key": "certs/office.key",
      "ca": "certs/office-ca.pem",
      "auto_connect": true
    }
  ],
  "language": "zh-CN",
  "default_mount_mode": "letter",          // letter | directory
  "default_mount_dir": "C:\\Vault",        // 目录模式实际路径 = <dir>\<alias>\<repo>
  "group_view": "aggregate",               // aggregate（同名聚合）| by_server
  "auto_mount": true,
  "start_at_login": true,
  "update_channel": "stable",
  "log_level": "info"
}
```

**配置迁移（单服务端 → 多服务端）**

| 旧字段 | 新位置 |
| --- | --- |
| `server_url` | `servers[0].url` |
| `client_cert` / `client_key` / `ca` | `servers[0].*`（同名保留） |
| — | 新增 `servers[0].server_instance_id`（首次连接后从 `system/info` 获取并回填） |
| — | 新增 `servers[0].alias`（默认取主机名，规范化后去重） |
| — | 新增 `primary_server_id` = `servers[0].server_instance_id` |

> 迁移在首次启动时自动完成，并**备份原配置文件**为 `config.json.bak`。

---

## 8. 前端工程（单一客户端应用）

> **架构约束（D13 / D14）**：
> 1. **服务端只提供 REST API**，不再承载任何 Web 静态资源（无 `embed.FS` 静态目录、无 `/admin` 路由）。
> 2. **前端只有一个应用**：Electron 客户端。用户功能与管理功能是它的两个模块，通过侧边栏与路由区分，**管理模块仅对 `super_admin` 可见**。
> 3. 因此"组件级复用"的含义从"两个 app 复用"变为"**同一应用内跨模块复用**"——仍然只有一份 UI 真源。

### 8.1 Monorepo 结构

```
frontend/                              # 单一 pnpm workspace
├── packages/
│   └── ui/           # ★ 唯一的 UI 真源（被客户端源码方式直接引用）
│       ├── tokens/         设计变量 → 生成 antd ConfigProvider theme
│       ├── components/     基础组件：PageShell / SectionCard / StatusTag / EmptyState /
│       │                   ConfirmDialog / DataTable / CopyableText / PasswordStrength / ErrorNotice
│       ├── features/       ★ 业务视图（按域划分，内置角色判定决定可见性）
│       │                   bootstrap/  BootstrapWizard（初始化向导）
│       │                   auth/       LoginForm
│       │                   server/     ServerEndpointForm（服务端地址配置）
│       │                   repo/ user/ lease/ job/ audit/ system/ settings/
│       ├── api/            REST 客户端 + 类型定义 + 错误码→i18n 映射
│       ├── hooks/          useApi / useAuth / usePermission / useServerConfig / useI18n
│       └── i18n/           {zh-CN,en-US}.json（扁平 key）+ t(key, args)
└── apps/
    └── client/         # ★ 唯一应用：Electron（含管理模块）
        ├── electron/   main.ts（单实例/托盘/窗口尺寸记忆）、preload.ts（最小 IPC）
        └── src/        App.tsx / routes/ / layouts/AppLayout.tsx / store/ / flow/
```

**分层规则**

1. `apps/client` **只放**路由表、布局壳、启动状态机（`flow/`）与 Electron 主进程；**不写业务组件**。
2. 业务视图全部在 `packages/ui/features/`，由路由挂载；页面按角色决定是否注册（`RequireSuperAdmin` 守卫）。
3. `packages/ui` **以源码方式**被引用（不预打包），保证样式、i18n、API 客户端单一来源。
4. 用路径别名 `@vault/ui/*` 指向 `packages/ui/src/*`。

> ⚠️ **由此产生的取舍**：服务端不再提供浏览器可访问的管理页，首次部署必须先获得客户端安装包，且没有浏览器兜底。
> 应急路径：服务端保留 CLI 初始化能力（见 12.3）。

### 8.2 前端技术要点

| 项 | 选型 |
| --- | --- |
| 构建 | Vite（应用）+ `tsc`（Electron 主进程） |
| UI | React 18 + TypeScript（strict，零 `any`）+ antd 5，主题由 `packages/ui/tokens` 统一生成 |
| 状态 | TanStack Query（服务端状态）+ zustand（本地状态：服务端配置、会话、语言） |
| 路由 | React Router 6 |
| 表单 | antd Form；校验规则与后端逐条对齐（用户名 3~64 且仅字母/数字/`_`/`-`/`.`；密码 8~128 且必须含字母与数字） |
| 实时 | 当前用**轮询**（租约 15s、任务 10s）；SSE 通道已预留未接入 |
| 主题 | 简洁、偏苹果风、**全局关闭动效与透明**（`theme.token.motion=false` + 全局 `transition/animation: none`） |
| 持久化 | Electron 下经 preload IPC 写 `%APPDATA%/Vault/config.json`；浏览器 dev 模式退回 localStorage。**preload 只暴露 getConfig/setConfig**，不开启 `nodeIntegration` |
| i18n | 单一 locale 源；服务端错误码统一映射为 `err.<code>`，缺失映射时回退展示通用文案 + code |

---

## 9. 安全设计

### 9.1 身份与鉴权

| 层 | 机制 |
| --- | --- |
| 传输 | HTTPS + **mTLS**（客户端必须持有效证书） |
| 客户端身份 | **客户端证书是唯一可信身份**（对应 design.md 第 37 条"支持证书鉴权，本地安装证书"） |
| IP 限制 | 作为**附加访问策略**（对应"限制使用的 IP 地址"）：证书绑定允许来源 IP/CIDR 段；不匹配则拒绝并告警。**注意 IP 只作为可选的收紧策略，不作为身份来源**（DHCP/VPN/NAT 下会误伤） |
| 管理页 | 用户名 + argon2id 密码 + 会话 Cookie（HttpOnly/Secure/SameSite=Strict） |
| 客户端内嵌管理页 | 复用客户端的 mTLS 会话 → 服务端签发**短期管理员 Token**（仅当该用户 role 允许） |
| 服务端间 | **不通信、不互信**。每个服务端彼此完全独立（独立 CA、独立 DB、独立身份体系），见 3.2 / 3.4.1 |

**证书生命周期（design.md 第 37 条"本地安装证书"的落地）**

| 环节 | 方案 |
| --- | --- |
| 签发 | 管理页生成一次性 enrollment token → 客户端凭 token 申请证书（CSR → 该服务端的 CA 签发） |
| 安装 | 客户端保存到 **`servers/<instance_id>/identities/<user_id>/`**；私钥以 **DPAPI 加密文件**存储（绑定当前 Windows 用户 + 机器）。若需装入 Windows 证书库，见 9.2 的取舍与 T17 |
| 绑定 | 签发时记录 `user_id`，并与允许的 IP/CIDR 段绑定；**身份判定基于 SPKI 公钥指纹**（见下） |
| **多服务端隔离** | ★ 证书写入**自定义扩展**（服务端实例 ID）。**该扩展用于"校验"（服务端拒绝非本机的证书），不作为"筛选"手段** —— 筛选靠按服务端分目录的存储结构 |
| 续期 | 有效期内自动续期：客户端**保留原密钥对**生成 CSR → 服务端签发新证书。**SPKI（公钥）指纹不变 → 无需重新审批**；证书序列号与证书指纹更新属正常现象 |
| 吊销 | `certificates.status='revoked'` + CRL；服务端每次请求校验 |
| 换机 | 管理页可为用户补发新证书；旧证书需显式吊销（避免一台用户多证书长期存活） |

> 客户端证书登录（`POST /v1/auth/cert-login`）拿到的是**短期会话令牌**：本地代理会在会话接近
> 过期时用同一份客户端证书**自动续期**（续期窗口 = `max(5min, 观测寿命*25%)`，两次续期至少间隔
> 5min；见 docs/agent-api.md「客户端证书会话自动续期」）。续期失败绝不清除会话、绝不影响挂载；
> 证书被吊销/过期等确定性错误则停止自动续期并要求重新登录。

**多服务端下的证书管理（新增，对应 3.4.1）**

**核心模型：每服务端一套独立 CA，客户端按服务端分目录存放，互不共用。**

```
服务端 A：自建 CA_A ─┬─ 签 服务端证书_A（HTTPS）
                     └─ 签 客户端证书（用户 × N）
服务端 B：自建 CA_B ─┬─ 签 服务端证书_B
                     └─ 签 客户端证书
A 与 B 互不信任：A 不认 CA_B 签发的任何证书，反之亦然
```

**客户端存储布局**

```
%ProgramData%\Vault\servers\
└── srv-7f3a.../                      ← 按【服务端实例 ID】分目录（非按 URL，换 IP/域名不受影响）
    ├── ca.pem                        ← 该服务端 CA 根证书（仅用于校验这一台）
    ├── trust.json                    ← 首次信任记录：CA 指纹 + 确认时间 + 确认人
    ├── server-info.json              ← 名称、别名、url、api_version
    └── identities\
        └── <user_id>\
            ├── client.pem            ← 客户端证书
            ├── client.key.dpapi      ← 私钥（DPAPI 加密，绑定当前 Windows 用户 + 机器）
            └── meta.json             ← SPKI 指纹、有效期、最后使用时间
```

**连接时的加载与校验（以连 A 为例）**

```
1. RootCAs    = servers/srv-A/ca.pem               （不加载 B 的 CA）
2. ClientCert = servers/srv-A/identities/<user>/    （不加载 B 的证书）
3. 双向校验：
   客户端验：服务端证书由 CA_A 签发 且 SAN 匹配配置的 url
   服务端验：客户端证书的 server_instance_id 扩展 == 自己的 instance_id
```

> ⚠️ **证书选择靠"存储路径 + 本地映射表"，不靠"遍历筛选"，更禁止"盲试"。**
> 客户端维护映射 `(server_instance_id, user_id) → 证书路径`，直接定位。
> **绝不能拿多张证书挨个发起连接试探** —— 每次失败都会消耗服务端防爆破的失败计数（5 次锁定 15min），会把用户自己锁死（见 9.4 防爆破）。这条同时修正了 design.md 客户端-用户配置-2「枚举尝试登录」的落地方式。

**首次信任建立（必需环节，否则存在 MITM 敞口）**

服务端使用自签 CA，客户端首次连接**无法通过公信 CA 验证**，必须显式建立信任：

| 方式 | 适用 | 说明 |
| --- | --- | --- |
| **TOFU + 指纹核对** | 自建 / 小规模 | 首次连接展示「CA 指纹 + 服务端证书指纹 + 服务端名称」，用户与部署方核对一致后固化到 `ca.pem` + `trust.json` |
| **带外分发 CA** | 生产推荐 | 管理员从服务端管理页导出 `ca.pem`（或由 `install.ps1` 输出），通过可信渠道导入客户端 |

**关键安全约束**
| # | 约束 |
| --- | --- |
| 1 | **TOFU 只在首次生效**。之后若检测到 CA 变化（服务端被重装/被替换），必须**显式拒绝 + 高优告警**，**绝不自动接受新 CA** —— CA 静默变更正是 MITM 的典型特征 |
| 2 | 证书"绑定"必须基于**公钥（SPKI）指纹**，而非证书指纹。续期时保留密钥对 → SPKI 不变 → **无需重新审批**；证书序列号与指纹更新属正常现象 |
| 3 | 服务端需校验客户端证书的 `server_instance_id` 扩展等于自身 `instance_id`，防止其他服务端的证书被误用（同时便于排障） |
| 4 | 服务端证书（HTTPS）的 SAN 必须与客户端配置的 `url` 匹配；建议同时写入**域名 + 固定 IP** 两个 SAN，避免换 IP/换域名导致 TLS 失败 |
| 5 | 客户端多证书共存时，UI 需展示「证书 ↔ 服务端 ↔ 用户」的对应关系，便于理解与清理 |
| 6 | 删除服务端配置时，**默认保留**其证书目录，仅提示可选清理（避免误删导致重连需重新 enrollment） |
| 7 | **CA 更换（服务端重装）** 属安全敏感事件：客户端所有旧证书失效 → 必须重新走信任建立 + enrollment，并在 UI 明确告知"服务端身份已变化" |
| 8 | 私钥存储方案（**DPAPI 加密文件** vs **Windows 证书库**）见 9.2 的取舍说明与实测项 T17 |
| 9 | v1 先支持「**每服务端一个当前身份**」；同一服务端多用户身份切换作为后续扩展（布局已预留 `identities/<user_id>/`） |

> 关于 design.md 第 37 条的落地细节见 9.1 的证书生命周期表；13.3-⑯ 说明了"IP/MAC 为何不能作为身份来源"。

### 9.2 CA 与信任链

**每台服务端独立自建 CA（多服务端 = 多套互不相干的信任根）**

```
服务端 A 的 CA_A（首次启动生成，私钥落盘 + ACL 仅服务账户可读）
  ├─ 服务端证书_A（HTTPS，含域名 + 固定 IP 两个 SAN）
  └─ 客户端证书（由 9.1 的 enrollment 流程签发）
       └─ 有效期默认 1 年，到期前 30 天提示续期；吊销走 CRL + DB 状态双校验

服务端 B 的 CA_B：完全独立，与 CA_A 无任何交叉信任
```

**客户端侧信任根管理**

| 项 | 做法 |
| --- | --- |
| 保存位置 | `servers/<instance_id>/ca.pem`（每服务端一份，按目录隔离） |
| 加载方式 | 连接某服务端时，**只把该服务端的 CA 放进 `RootCAs`**，不放其他服务端的 CA |
| 首次信任 | TOFU + 指纹核对，或带外分发（见 9.1） |
| 变更检测 | CA 指纹与 `trust.json` 记录不一致 → **拒绝连接 + 高优告警**，绝不自动接受 |
| 清理 | 删除服务端配置时默认保留 CA 文件，避免重复信任 |

**⚠️ 客户端私钥存储：两种方案取舍（实测项 T17）**

| 方案 | 优点 | 缺点 | 建议 |
| --- | --- | --- | --- |
| **A. DPAPI 加密文件**（`CryptProtectData`） | Go 侧可直接 `tls.LoadX509KeyPair` 读取，实现简单；绑定当前 Windows 用户 + 机器 | 仅在 Vault 内可用；其他程序无法复用 | ✅ **推荐**，且纯 Go syscall 即可实现 |
| **B. Windows 证书库**（`Cert:\CurrentUser\My`） | 符合 design.md "本地安装证书"的描述；可被系统/其他程序使用；私钥可标不可导出 | **Go 取私钥需 CNG/CAPI 互操作**（`CryptAcquireCertificatePrivateKey`），标准库不提供，需自研 syscall 包装 | 仅当确需与系统证书体系集成时选 |

> 若要同时满足两者，可做**双写**：私钥既存 DPAPI 文件（供 Vault 使用），也导入证书库（供系统使用），并以 DPAPI 文件为准。

**其他安全注意**
- CA 私钥是**系统信任根**，建议离线/受保护存储（至少与 DB 分离并限制 ACL）；
- 服务端必须校验证书的 **SPKI 绑定 + `status` + `server_instance_id` 扩展**，不能只依赖 TLS 握手结果；
- 服务端记录 `certificates` 时需存 **SPKI 指纹**（用于续期比对）与**证书指纹**（用于排障）两个字段，不要混用。

### 9.3 权限模型

| 角色 | 权限 |
| --- | --- |
| `super_admin` | 全部；创建/禁用用户；系统设置；审计查看 |
| `user`（库 owner） | 自己创建的库：CRUD、分配、设置成员、复制 |
| `user`（库成员，`manage`） | 该库：读写配置、分配 |
| `user`（库成员，`mount`） | 该库：仅能挂载自己被分配的盘 |
| `user`（库成员，`read`） | 该库：只读查看 |
| `user`（非成员） | 不可见该库 |

### 9.4 其他安全措施

| 项 | 措施 |
| --- | --- |
| 防爆破 | 登录失败计数（IP + 用户名双维度），5 次锁定 15min，指数退避；成功后清零 |
| API 限流 | 令牌桶：全局 1000 rps，单用户 50 rps，上传单独配额 |
| 路径安全 | 白名单根 + `EvalSymlinks` + 前缀校验；禁止 UNC、`..`、保留设备名 |
| 命令注入 | PowerShell 一律 `-Args`/参数化传值，禁止字符串拼接；脚本内置字符校验 |
| 密钥保护 | CHAP 密钥 AES-GCM 加密落库；日志/审计/API 响应过滤；下发给客户端后不落盘 |
| 上传安全 | 分块 SHA256 校验；文件名规范化；禁止符号链接与硬链接；总量配额 |
| 审计 | 所有写操作 + 权限拒绝事件入 `audit_logs`，保留 180 天 |
| 服务账户 | Vault 服务以专用账户运行，仅对白名单根授予必要权限（避免全域管理员常驻） |
| 依赖更新 | 定期扫描 Go / npm 依赖漏洞（`govulncheck` / `pnpm audit`） |

---

## 10. 选型决策

### 10.1 技术栈总表（含对原设计的调整）

| 层 | design.md 选型 | 结论 | 理由 |
| --- | --- | --- | --- |
| 服务端语言 | Go + cgo | **Go，无 cgo（已确认）** | 已逐项核查：本项目零处需要 cgo，VirtDisk 走纯 Go syscall 即可。见 10.4 |
| 客户端 | Go + TS + React + antd + Electron | **保持 Electron（已定）** | 见 10.3；重点转为"如何控制体积与统一 UI" |
| 管理页 | TS + React + antd | **与客户端共用 `packages/ui`** | 见第 8 章，禁止双实现 |
| 数据库 | MySQL / SQLite | **SQLite(WAL) 默认 + MySQL 均支持，可切换（双驱动）** | 见 10.2；etcd 方案已否决 |
| 卷文件系统 | 未提 | **NTFS（已定）** | 无块克隆 → 复制为全量拷贝，见 5.9 |

### 10.2 数据库选型论证

| 方案 | 事务 | 关系查询 | 部署成本 | 并发写 | 定位 |
| --- | --- | --- | --- | --- | --- |
| **SQLite (WAL)** | ✅ ACID | ✅ 完整 SQL | **零**（文件即库） | 单写者（写串行） | **默认**：单机部署 |
| **MySQL 8** | ✅ | ✅ | 中（独立进程） | 良好 | **受支持**：运行时可切换 |
| PostgreSQL | ✅ 强 | ✅ 最强（JSONB/部分索引/CTE） | 中 | 优秀 | 备选（本文不强制支持） |
| **etcd** | ⚠️ 仅单次 CAS/STM | ❌ 仅 KV，无二级索引/聚合 | 中 | 一般 | ❌ **不适合业务数据** |

**关于 etcd 的明确结论**：etcd 是为「少量、极关键、强一致」的配置数据设计的（Kubernetes 即此用法）。本系统数据具备：
- 需要多条件查询、分页、聚合统计（存储库列表、审计查询、容量汇总）；
- 数据量持续增长且远大于 etcd 的适用区间（租约、审计、任务、分块记录）；
- 需要关系约束（外键、唯一索引）。

用 etcd 会迫使你在应用层手写索引、二级查询、分页、事务补偿——**复杂度大幅上升且没有收益**。

**双驱动落地方式（✅ 已决定：两种都实现、运行时可切换）**

切换方式：配置文件 `database.driver: sqlite | mysql`，启动时按驱动加载对应实现；**不支持运行时热切换**（切换需停服 + 数据迁移工具）。

```
定义 Repository 接口（store 层，业务层不感知方言）
  ├─ sqlite driver（默认；modernc.org/sqlite，纯 Go 免 cgo）
  └─ mysql  driver（go-sql-driver/mysql）
SQL 方言差异封装在 driver 内部：
  - 主键：TEXT/UUID 统一由应用生成（避免 AUTOINCREMENT vs AUTO_INCREMENT 差异）
  - 时间：统一存 INTEGER（Unix 毫秒），避免 DATETIME/TIMESTAMP 语义差异
  - UPSERT：SQLite ON CONFLICT DO UPDATE vs MySQL ON DUPLICATE KEY UPDATE
  - 事务：统一用 BEGIN IMMEDIATE(SQLite) / SELECT ... FOR UPDATE(MySQL) 实现"分配防超卖"
  - 布尔：统一用 INTEGER 0/1（MySQL TINYINT(1) 语义兼容）
  - 迁移：两套迁移目录 migrations/sqlite、migrations/mysql
```

**双驱动的工程要求**（避免"只在一种库上能跑"）

| # | 要求 |
| --- | --- |
| 1 | 每个迁移脚本必须**成对提交**（sqlite + mysql），CI 校验两侧版本号一致 |
| 2 | CI 必须**对两种后端各跑一遍全部集成测试**，不允许只测 SQLite |
| 3 | 禁止在业务代码中写方言 SQL；所有方言相关逻辑只能出现在 `store/sqlite`、`store/mysql` |
| 4 | 提供 `vault-server migrate export/import` 工具，支持 SQLite ↔ MySQL 数据迁移（换库时需要） |
| 5 | 索引、外键、唯一约束必须两侧等价声明（MySQL 注意 utf8mb4 与索引长度限制） |

**SQLite 的必须配置**（易踩坑）

```sql
PRAGMA journal_mode = WAL;
PRAGMA busy_timeout = 5000;
PRAGMA foreign_keys = ON;
PRAGMA synchronous  = NORMAL;
```

- **DB 文件必须放本地磁盘**，绝不能放网络共享（SMB 上 SQLite 会损坏）；
- 单写者语义 → 写事务必须短，禁止在事务内做 VHDX/PowerShell 等长操作；
- 定时 `PRAGMA wal_checkpoint(TRUNCATE)` + 定期 `VACUUM`，防止 WAL 无限增长；
- 备份 = 停服复制 `vault.db` + `vault.db-wal`（或用 `VACUUM INTO` 在线备份）。

**MySQL 的额外要求**：字符集统一 `utf8mb4` + `utf8mb4_0900_ai_ci`；引擎 InnoDB；需保证连接池与 `innodb_flush_log_at_trx_commit` 配置符合一致性要求。

### 10.3 客户端外壳：**已决策 —— Electron**

| 决策 | 结论 |
| --- | --- |
| 外壳 | **Electron**（不用 Wails） |
| 首要诉求 | **管理页与客户端 UI 统一**（同一套组件） |
| 次要诉求 | "尽量打包成单一执行文件" → 用 `electron-builder` 的 `portable` target（自解压，外观为单 exe） |

**Electron 下控制体积的落地手段**

| 手段 | 说明 |
| --- | --- |
| 使用 `electron-builder` 的 `asar` + 仅打包必要依赖 | 剔除 devDependencies 与 source map |
| 分架构打包（x64/arm64 各自独立） | 避免双架构包体翻倍 |
| Go agent 单独编译为 `~10-15MB` 的 sidecar | 不用 cgo，`-ldflags "-s -w"` |
| 增量更新（`electron-updater` differential） | 更新包从 ~100MB 降到数 MB |
| 按需加载渲染层 | 非关键页面 `React.lazy`，与"轻量加载"的既有偏好一致 |
| 不用重型 UI 依赖 | antd 按需引入 + 避免引入额外图表库（监控图用轻量方案） |

**必须接受的取舍**：Electron 无法做到真正意义上的"单文件、小体积"，`portable` 版是自解压到临时目录后运行，首次启动会略慢。若要极致轻量只能换 Wails，但会牺牲 UI 统一生态与团队上手成本——**已按你的决策选 Electron，此处仅作风险登记**。

### 10.4 关于 cgo 的结论：**当前零处需要，确定不使用** ✅

design.md 写"服务端：golang + cgo"。逐项核查本项目**全部** Windows 原生能力需求：

| 用途 | 实现路径 | 是否需要 cgo |
| --- | --- | --- |
| VHDX 创建/打开/挂载/卸载/差异盘/取物理路径 | `github.com/Microsoft/go-winio/vhd`（已封装 `virtdisk.dll`） | ❌ 纯 Go syscall |
| 重置 DiskIdentifier | `virtdisk.dll` 的 `SetVirtualDiskInformation`（go-winio **未封装**，需自写 `syscall.NewLazyDLL` + `unsafe` 结构体） | ❌ 仍是纯 Go |
| 扩容（备用路径） | `Resize-IscsiVirtualDisk`（PowerShell） | ❌ |
| 文件复制 | `CopyFileEx`（kernel32）或 `robocopy` | ❌ |
| 分区/格式化/挂载点 | PowerShell `Storage` 模块 | ❌ |
| iSCSI 全部操作 | PowerShell `IscsiTarget` 模块 | ❌ |
| 卷信息（NTFS 判定、可用空间） | `GetVolumeInformationW` / `GetDiskFreeSpaceExW`（kernel32） | ❌ |
| 网卡统计 / netstat | `iphlpapi`（`GetIfTable2`）或 PowerShell | ❌ |
| 单实例互斥 | `golang.org/x/sys/windows` → `CreateMutexW` | ❌ |
| Windows 服务宿主 | `golang.org/x/sys/windows/svc` | ❌ |
| 证书装入 Windows 证书库 | `crypt32` syscall 或 PowerShell `Import-PfxCertificate` | ❌ |
| 事件日志写入（可选） | `golang.org/x/sys/windows/svc/eventlog` | ❌ |
| 托盘 / 快捷方式 | Electron（Node）侧实现 | ❌ 与 Go 无关 |
| SQLite 驱动 | `modernc.org/sqlite`（纯 Go 翻译版） | ❌ |
| MySQL 驱动 | `go-sql-driver/mysql`（纯 Go） | ❌ |

**结论：启用 `CGO_ENABLED=0`，不使用 cgo。**
收益：交叉编译可行、无 MinGW/MSVC 工具链依赖、单文件交付、CI 更快、无 C 运行时版本兼容问题。

#### ⚠️ 唯一会"被动引入 cgo"的坑：依赖选型

| 用途 | ✅ 必须用（纯 Go） | ❌ 禁止使用（引入 cgo） |
| --- | --- | --- |
| SQLite | `modernc.org/sqlite` | `mattn/go-sqlite3`（底层 C 库，强制 `CGO_ENABLED=1`） |
| MySQL | `go-sql-driver/mysql` | 任何 C 绑定版本 |

> 一旦误引入上述依赖，`CGO_ENABLED=0` 构建会直接失败，或被迫退回 cgo 构建（失去单文件与交叉编译能力）。
> **建议在 CI 加一道守卫**：`CGO_ENABLED=0 go build ./...` 必须成功；并在 `go.mod` review 清单中标注该约束。

#### 关于 `CGO_ENABLED` 的精确说明（为什么必须显式设为 0）

即使代码里没有任何 `import "C"`，**`CGO_ENABLED=1` 仍有副作用**：标准库 `net` 与 `os/user` 会自动选用 cgo 实现（DNS 走系统解析器、用户查询走 Win32 API），导致：
- 二进制**依赖 C 运行时**，不再是纯静态；
- DNS 解析行为与纯 Go 解析器不同（可能出现"本地开发正常、部署环境解析失败"的诡异问题）；
- 无法跨平台交叉编译，且构建机必须装 C 工具链。

因此显式 `CGO_ENABLED=0` 不是形式主义，而是**保证产物行为可预测、可复现**的必要设置。

> 未来若确实需要接入第三方 C SDK（如某个厂商的存储 SDK），再单独评估启用 cgo，并把它隔离到独立的 `platform/<xxx>cgo` 包中，通过 build tag 控制，避免污染主构建。

### 10.5 关键第三方库

| 用途 | 库 |
| --- | --- |
| VHDX | `github.com/Microsoft/go-winio/vhd` |
| SQLite 驱动 | `modernc.org/sqlite`（纯 Go，无 cgo） |
| MySQL 驱动 | `github.com/go-sql-driver/mysql` |
| 路由 | `github.com/go-chi/chi/v5` |
| 配置 | `gopkg.in/yaml.v3` |
| 迁移 | `github.com/golang-migrate/migrate/v4` |
| CA/证书 | 标准库 `crypto/x509` |
| Windows 服务 | `golang.org/x/sys/windows/svc` |
| 单实例互斥 | `golang.org/x/sys/windows`（`CreateMutex`） |
| OpenAPI | `github.com/getkin/kin-openapi`（校验）+ `openapi-typescript`（前端生成） |

---

## 11. 实施路线图

按"能早跑通端到端最小闭环"排序，每阶段产出可验证成果。

### M0：地基（探针与骨架）
- 在目标 Windows Server 上跑通 **14.1 的 19 项待实测清单（T1~T19）**，输出《平台能力验证报告》。这是所有后续设计的前提。
- 建立仓库骨架、CI、日志、配置、DB 迁移（SQLite + MySQL 双套）、单实例锁。
- **建立 cgo 守卫**：CI 强制 `CGO_ENABLED=0 go build ./...` 通过（见 12.2），并在 `go.mod` review 清单标注"禁止 C 绑定依赖"。
- 产出：`system/info` 能力探测接口（WinTarget 是否可用？Hyper-V 是否存在？NTFS 卷可用空间？DB 后端？）。

### M1：VHDX 生命周期（服务端）
- `platform/winvhd` + `platform/volume` + `platform/winps`。
- 实现：创建（空/动态）、挂载、格式化（NTFS）、分离、删除、查询、尺寸计算。
- 实现：从目录创建（服务端本地目录，先不做上传）。
- 实现：**复制**（全量拷贝 + `ResetDiskIdentifier`）。
- 产出：内部测试接口可完整走通一个 VHDX 生命周期。

### M2：iSCSI 与存储库（服务端）
- `platform/iscsitarget`：Import / target / mapping / initiator / CHAP。
- Repository 领域模型 + **独享模式**全流程。
- 任务系统 + 幂等 + 资源互斥锁。
- 产出：API 可创建独享库并发布 iSCSI；用原生 Windows iSCSI 发起端验证可连上。

### M3：客户端闭环
- Go agent：本地挂载引擎、**租约心跳**、SSE 接入。
- Electron 骨架 + 托盘 + 单实例 + `packages/ui` 骨架。
- 产出：客户端可挂载服务端发布的独享库为盘符 / 目录。

### M4：共享库（母盘 + 差异盘 + 维护态）★核心
- 差异盘创建、分配模型、CHAP 密钥托管、**每分配一 target**。
- 母盘 `parent_condition` 状态机 + 指纹校验 + `parent_version` + `max_diff_disks` 数量闸门。
- **维护（MAINTENANCE）状态**：清理差异盘 → 读写挂载更新 → 退出维护。
- **临时只读共享（TEMP_SHARED）** + 崩溃恢复 + Reconciler。
- **13.4-㉙ 的 8 项配套机制**：产品文案、二次确认、强制卸载、软删除、SSE 通知、审计、独享库引导、手动导出兜底。
- 产出：1 母盘 → N 差异盘 → N 用户各自挂载；服务端重启后可管理临时共享；母盘更新全流程（含数据丢弃提示）跑通。

### M5：上传与目录创建
- 分块上传、断点续传、staging、完成回调。
- 客户端目录选择、进度、move 语义（先校验后删源）。
- 产出：客户端本地目录 → 服务端 VHDX → 发布 → 客户端挂载。

### M6：管理页与用户体系
- 用户 / 证书 / CA / enrollment、登录、防爆破、审计。
- 管理页（复用 `packages/ui`）、监控视图、任务视图。
- 产出：浏览器可完整管理；客户端与管理页 UI 完全一致。

### M7：优化与加固
- 定时 **Retrim/Compact**、孤儿 GC（VHDX/target/staging/lease）、指标、健康检查。
- 客户端自动更新（启动检查 + 设置页手动检查）、i18n、分组标签。
- 产出：可交付版本。

### M8：验收与文档
- 故障注入测试（断电、进程 kill、网络中断、磁盘满、母盘被误写）。
- 部署文档、运维手册、升级/回滚演练。

### M9：多服务端（对应 3.4）🆕
> 建议**排在 M3 之后即可并行启动**（客户端结构改造越早越便宜），但完整功能可在 M7 之后收尾。

- **服务端（工作量极小）**：`system/info` 匿名化 + `server.instance_id` / `server.name` + **`client_compat` 配置段（`enabled`/`min`/`max`，支持热重载与关闭检查）** + `features`；`api_version` 保持代码常量。
- **客户端结构改造（主要工作量）**：
  - `ServerRegistry`：多服务端注册表 + 每服务端独立会话（mTLS / SSE / 心跳 / 熔断）
  - 版本兼容协商与三态判定（3.4.2）
  - 配置从单值迁移到 `servers[]`（含自动迁移，见 7.6）
  - 挂载点命名空间与盘符统一分配表（3.4.5）
  - 证书按服务端实例 ID 隔离（9.1）
- **更新体系升级**：主服务端 + 失败回退、统一发布方签名、版本单调性校验（7.4.1~7.4.3）
- **UI**：服务端来源标识 + 同名分组聚合视图 + 按服务端分组视图（3.4.6）
- **产出**：客户端可同时挂载来自 2 个以上服务端的存储库；单台服务端故障/版本不兼容不影响其他服务端。

---

## 12. 构建与发布

### 12.1 构建依赖

| 组件 | 依赖 |
| --- | --- |
| 服务端 | Go 1.23+（`CGO_ENABLED=0`）、Node 20+（仅构建前端）、PowerShell 脚本 |
| 客户端（Electron） | Go 1.23+、Node 20+、pnpm、electron-builder |
| 运行时（服务端） | Windows Server 2016+、FS-iSCSITarget-Server 角色、管理员权限、NTFS 数据卷 |
| 运行时（客户端） | Windows 10 1809+、msiscsi 服务（系统自带） |
| 可选 | Hyper-V 模块（仅用于 `Set-VHD -ResetDiskIdentifier` / `Optimize-VHD`，缺失时有回退路径） |

### 12.2 构建脚本（DSL 示例）

```powershell
# build.ps1
param([ValidateSet('server','client','admin','all','verify')][string]$Target = 'all')

# ★ cgo 守卫：本项目零处需要 cgo，任何一步都必须能在 CGO_ENABLED=0 下通过
function Assert-NoCgo {
    $env:CGO_ENABLED = '0'
    go build ./... ; if ($LASTEXITCODE -ne 0) {
        throw "CGO_ENABLED=0 构建失败：可能误引入了 C 依赖（如 mattn/go-sqlite3）。见文档 10.4"
    }
    # 双数据库后端构建标签也要能编过
    go build -tags 'sqlite mysql' ./... ; if ($LASTEXITCODE -ne 0) { throw "带标签构建失败" }
    # ★ Linux 侧交叉编译（服务端相关包；internal/agent 与 cmd/vault-agent 为 Windows 专属，不在范围内）
    $env:GOOS = 'linux'; $env:GOARCH = 'amd64'
    go build ./cmd/vault-server ./internal/app/... ./internal/platform/... `
             ./internal/api/... ./internal/lock/... ./internal/config/... ; if ($LASTEXITCODE -ne 0) {
        throw "Linux 交叉编译失败（服务端相关包）"
    }
    Remove-Item Env:\GOOS; Remove-Item Env:\GOARCH
}

function Build-Server {
    Push-Location frontend; pnpm install --frozen-lockfile; pnpm build:admin; Pop-Location
    Copy-Item frontend/apps/admin/dist/* cmd/vault-server/web/ -Recurse -Force
    Assert-NoCgo
    $env:CGO_ENABLED = '0'
    go build -trimpath -ldflags "-s -w -X main.version=$Version" `
        -o dist/Vault-Server.exe ./cmd/vault-server
}

function Build-ServerLinux {
    Assert-NoCgo
    $env:CGO_ENABLED = '0'; $env:GOOS = 'linux'; $env:GOARCH = 'amd64'
    go build -trimpath -ldflags "-s -w -X main.version=$Version" `
        -o dist/vault-server-linux-amd64 ./cmd/vault-server
    Remove-Item Env:\GOOS; Remove-Item Env:\GOARCH
}
function Build-Client { ... electron-builder --win nsis portable ... }

# 版本号注入、代码签名、产物哈希清单、SBOM（syft）
# CI 必跑：Assert-NoCgo + 双 DB 后端集成测试 + 前端 lint/typecheck
```

### 12.3 发布物

```
# Go 可执行文件：服务端（Windows/Linux 分构建）与客户端本地代理（Windows）
dist/
├── vault-server_<ver>_windows_amd64.zip
│   ├── Vault-Server.exe          # 单文件，内嵌管理页
│   ├── config.example.yaml       # 含 server.instance_id/name、client_compat(enabled/min/max)
│   │                             # 与 platform.{kind,lvm,iscsi}（Linux 侧使用）
│   ├── install.ps1               # 安装服务、角色、防火墙、白名单卷初始化
│   └── README.md
├── vault-server_<ver>_linux_amd64.tar.gz     # ★ 同一代码库的 Linux 构建（LVM thin + LIO）
│   ├── vault-server              # 静态单文件（CGO_ENABLED=0）
│   ├── config.example.yaml       # platform.lvm.{vg,thin_pool,...} / platform.iscsi.{backend: lio,...}
│   ├── vault-server.service      # systemd 单元（无双击启动器；停机走 SIGTERM）
│   └── README.md                 # 含 LVM2/target 内核模块前置条件（见 §2.4）
├── vault-client_<ver>_x64_setup.exe    # NSIS 安装包（resources/agent/ 下随包附带 Vault-Agent.exe）
├── vault-client_<ver>_x64_portable.exe # 便携版（若单文件要求）
├── checksums.txt
└── sbom.json
```

> Linux 侧的「虚拟磁盘 + iSCSI」能力依赖**运行环境**（LVM2 工具链 + LIO 内核模块 + configfs），
> 因此 tar.gz 内附前置条件说明；服务端启动时只**探测**这些依赖，缺失仅降级并打 WARN（见 §2.4）。

### 12.4 升级与回滚

| 组件 | 升级方式 | 回滚 |
| --- | --- | --- |
| 服务端 | 停止服务 → 备份 DB → 替换 exe → DB 迁移 → 启动 → Reconciler 自检 | 保留上一版 exe + DB 备份，降级需逆向迁移脚本 |
| 客户端 | 自更新（7.4） | 保留上一版本目录，连续 3 次启动失败自动回滚 |
| 兼容性 | API 版本前缀 `/v1`；服务端声明 `min_client_version`，低于则强制升级 | - |

---

## 13. 设计评审：不足与改进建议

> 以下按**严重级别**排列。🔴 = 会导致方案不可行或数据损坏；🟠 = 会导致实现返工；🟡 = 建议完善。

### 13.1 🔴 严重（可行性硬伤）

#### ① "查询、管理、监控 iSCSI 连接" 在服务端做不到
- **问题**：design.md 服务端-iscsi-3 要求"查询、管理、监控 iscsi 连接"。但 Windows iSCSI Target Server **没有会话枚举 API**，也没有按会话断开 API（详见调研文档第 6 章）。
- **影响**：整个"在线状态"与"踢下线"功能无法按原设想实现。
- **改进**：
  1. **引入租约（Lease）+ 心跳**作为在线状态的权威来源（本文 5.7）；
  2. "监控"降级为"按盘/按网卡的资源占用"，**明确放弃 per-session IO**；
  3. "踢下线"降级为"停用 target + 通知客户端自断"，且 target 需保持 disabled 一段时间（本文 5.8）；
  4. UI 上要如实呈现：区分"租约在线"与"TCP 连接存在"两个概念，避免误导。

#### ② "母盘只要没有差异盘被连接就可临时挂载" 不充分
- **问题**：design.md 第 28 条原文为"母盘在只有没有任何差异盘或本身没有被挂载的情况下可以被临时挂载"。判定条件必须是"**不存在任何差异盘**（无论是否在线）"，而不是"没有差异盘被连接"。
- **影响**：母盘被写入会**静默污染所有差异盘**（差异盘读取母盘时假设其内容不变），且极难排查。
- **改进（已按你的决策调整）**：
  1. 采用本文 5.4 的 `parent_condition` 状态机，**四种用途互斥**：`IDLE / DERIVED / TEMP_SHARED / MAINTENANCE`；
  2. 母盘**需要更新**，因此**不用**只读挂载硬锁，改用**应用层标记 + 三重兜底**（指纹校验 + `parent_version` 校验 + 目录 ACL），见 5.4；
  3. 要更新母盘必须先进入 `MAINTENANCE`，而进入前置条件是**清理完全部差异盘**（对应 design.md 新增的"维护状态"要求）；
  4. 母盘以只读方式共享给客户端时归入 `TEMP_SHARED`，与"派生差异盘"互斥；
  5. ⚠️ 残留风险：应用层标记无法阻止**绕过 Vault 直接挂载母盘写入**，只能靠指纹比对事后发现 → 见 R2 与 13.4-㉙。

#### ③ "一个 target 多个用户" 与 CHAP 密钥模型冲突
- **问题**：design.md 第 22 条"建议用密钥的形式做鉴权"，但共享库天然是"1 库 N 用户"。而 Windows CHAP 是 **1 target 1 组账号**（`-Chap <PSCredential>`）。
- **影响**：若按"1 库 1 target"，则 N 个用户共享同一密钥 → **任一人泄密即全员可访问**，且无法定位是谁。
- **改进**：**改为「1 分配 = 1 iSCSI target」**（本文 5.3）。代价是 target 数量随用户增长（需评估上限：Windows 上 target 数量上限较高，但需实测与监控）。

#### ④ 服务端崩溃后的"临时共享"缺少持久登记
- **问题**：design.md 第 28 条只说"重启后还能管理这个临时的共享，可以取消掉"，但没有定义"如何在重启后知道有哪些临时共享"。
- **影响**：如果临时共享状态只存在内存中，重启后变成孤儿 target，占着母盘且无法识别。
- **改进**：**持久化登记**（`repositories.parent_condition='TEMP_SHARED'` + 固定命名 `vault-{repo_id}_temp`）+ 启动时 Reconciler 扫描收敛（本文 5.6 步骤 ④）。

### 13.2 🟠 重要（会导致返工）

#### ⑤ 缺少"期望状态 vs 实际状态"模型与对账机制
- **问题**：design.md 全文没有一致性设计。而 Windows 侧操作是一串不可回滚的副作用（PowerShell 部分成功、进程被杀、磁盘占满）。
- **影响**：运行一段时间后 DB 与真实状态必然发散，出现"幽灵存储库"（DB 有记录但盘不存在）或"孤儿 target/盘"。
- **改进**：引入 `desired_state` / `observed_state` 字段 + Reconciler（本文 5.11），危险操作默认只报告。

#### ⑥ 缺少任务（Job）模型
- **问题**：创建 VHDX、拷贝目录、复制盘、compact 都是分钟级甚至小时级操作，design.md 未定义异步模型。
- **影响**：HTTP 请求超时、无法展示进度、无法取消、失败无法重试。
- **改进**：Job 表 + Worker + 幂等键 + SSE 进度（本文 5.12）。

#### ⑦ 缺少并发布局与锁的明确定义
- **问题**："同一 VHDX 不能被并发操作"这类约束没有体现。
- **影响**：并发创建/删除/挂载同一盘 → **磁盘损坏**。
- **改进**：三层锁（进程互斥、资源互斥、DB 事务）+ 全局单实例（本文 5.13）。

#### ⑧ 缺少配额语义定义
- **问题**：design.md 第 31 条"基本配额监控"未定义：配额属于谁？按什么计量？共享库怎么算？
- **影响**：无法实现，或实现后发现口径错误。
- **改进（已定稿）**：
  ```
  配额归属：每用户（跨库汇总），由超级管理员设置
  计量口径：
    - 独享库：VHDX 逻辑大小，计入库 owner
    - 共享库：★ 母盘占用由 owner 独担（已决策）；成员只计自己的差异盘物理占用
    - 另需上报"卷剩余空间"水位，超阈值时禁止创建/分配
  展示口径：管理页需同时展示"逻辑大小"与"物理占用"，避免用户困惑
  ```
  > 决策影响：母盘由 owner 独担意味着**同一份母盘不会被重复计入 N 个成员**，配额口径清晰、可解释；代价是 owner 需为母盘容量负责。
  > 注意：NTFS 虽有原生磁盘配额，但那是**按卷/按用户账户**的口径，与"按库/按业务用户"不一致，因此仍走应用层记账（见 ⑬）。

#### ⑨ "存储到存储库的逻辑记录字段" 语义不清且不安全
- **问题**：design.md 服务端-存储库-7 与客户端-存储库-4 把"挂载后执行脚本"、"客户端配置"等塞进"逻辑记录字段"。
- **影响**：脚本内容可能很大且需审计/版本控制；混在 `repositories.meta` JSON 里会失控。
- **改进**：
  - 脚本独立表 `repo_scripts(repo_id, version, content, sha256, created_by)`，**版本化 + 审计**；
  - 明确脚本是"客户端本地执行"的，需**显式提示用户风险**并让用户确认；
  - 纯客户端 UI 配置（分组标签、展示顺序）可直接放 `meta` JSON。

#### ⑩ 权限模型缺失
- **问题**：design.md 只有"超级管理员 / 普通用户 / 可管理列表"，但没定义可管理列表的**权限粒度**，也没定义"谁能创建存储库"。
- **改进**：定义权限矩阵（本文 9.3），`repo_members.perm` 分 `read|mount|manage` 三级。

#### ⑪ 自动更新的可达性未评估
- **问题**：design.md 第 67-68 条只说"完成下载和重启以及更新，不影响挂载行为"，但 Windows 上运行中的 exe 无法覆盖，Electron 的更新也需要独立通道。
- **改进**：双进程切换 + 签名校验 + 失败回滚 + 有活跃挂载时延迟更新（本文 7.4）。

#### ⑫ 客户端"挂载到某个本地目录"的实现细节缺失
- **问题**：design.md 客户端-存储库-3 提到挂载形态支持目录，但没提约束。
- **改进**：明确 `Add-PartitionAccessPath -AccessPath` 要求目标目录**已存在且为空**、父目录为 NTFS；客户端需预检 + 明确错误提示（本文 5.5）。

### 13.3 🟡 安全与健壮性

#### ⑬ 配额必须应用层实现（不能用操作系统配额）
- **问题**：本系统的配额口径是"按库 / 按业务用户"，而操作系统的磁盘配额（NTFS 磁盘配额）是"按卷 / 按 Windows 账户"——**两者口径不一致**，无法直接复用。
- **改进**：
  1. 应用层记账：`repositories.used_bytes` / `users.used_bytes`，分配与创建时在事务内校验与累加；
  2. 定时校正：每小时用 `Get-Item` / `Get-VHD` 采样的真实 `FileSize` 与 DB 对账，修正漂移；
  3. 水位告警：卷可用空间低于阈值（建议 15%）时禁止新建与分配，并告警；
  4. 部署文档需写明"配额是应用层软约束，不阻止绕过 Vault 直接写盘"。

#### ⑭ iSCSI CHAP 密钥强度与轮换
- `-Chap` 用的是 `PSCredential`，secret 需 12~16 字节；建议 16 字节随机 + Base64。
- 需要**密钥轮换**能力（当前 design.md 未提）：定期或按需重新生成，更新 target 配置并重新下发客户端。

#### ⑮ 上传路径不做校验会出事
- 分块上传的文件名/tree 必须规范化并拒绝符号链接、`..`、保留设备名（`CON`、`NUL` 等）。

#### ⑯ 鉴权模型：已澄清为「证书为主 + IP 限制为辅」✅
- **原问题**：design.md 早期写"IP + MAC + 证书鉴权"——MAC 在多网卡/虚拟网卡/跨网段/VPN 下不稳定，IP 会被 DHCP/NAT 改变，两者都容易伪造，**都不适合作为身份来源**。
- **现状**：design.md 第 37 条已改为 **"支持证书鉴权，本地安装证书，限制使用的 IP 地址"**，方向正确。
- **落地要点**（见 9.1）：
  1. **证书（mTLS）是唯一身份来源**，`user_id` 与证书指纹强绑定；
  2. **IP 限制作为附加策略**：证书上绑定允许的 IP/CIDR，用于收紧访问面；但需允许管理员配置"不限制"，否则出差/VPN/换网段会大量误伤；
  3. **MAC 不再参与鉴权**（保留采集，仅作为管理页展示与异常排查线索）。

#### ⑰ 管理页"借用客户端鉴权"的权限提升路径不清晰
- **问题**：design.md 管理页-2 说"支持在客户端中直接访问，借用客户端的鉴权能力"。但客户端是普通用户身份，管理页需要管理员权限。
- **改进**：明确：客户端内嵌管理页 → 用 mTLS 身份 → 服务端校验该用户 `role` 是否满足该管理接口所需权限 → 满足则签发短期管理 Token；**普通用户看到的应是"自己的存储库管理"，不是系统管理页**。

#### ⑱ 缺少限流与资源保护
- 登录限流之外，还需要：上传限流、Job 并发上限、单用户存储库数量上限、单卷承载的 VHDX 数量上限。

#### ⑲ 密码哈希算法未指定
- 明确用 `argon2id`（参数：m=64MB, t=3, p=4），不使用 MD5/SHA1/bcrypt（bcrypt 有 72 字节上限）。

### 13.4 🟡 需求完整性与运维

#### ⑳ 缺少快照/备份设计
- Windows iSCSI Target 自带快照能力（`Checkpoint-IscsiVirtualDisk` / `Restore-IscsiVirtualDisk`），design.md 未利用。
- 建议：为母盘提供"快照"能力，供误删恢复；并定期导出配置（`Export-IscsiTargetServerConfiguration`）作为灾备。

#### ㉑ 缺少多网卡 / MPIO / Portal 规划
- 生产环境 iSCSI 应使用**专用网段**；多路径可提升吞吐与可靠性。design.md 完全没提。
- 建议：配置中支持多个 portal IP；客户端支持 `-IsMultipathEnabled`。

#### ㉒ 缺少性能容量规划
- 共享库模式下 N 个差异盘叠加读写，母盘成为**单点热点**。需在文档中给出：
  - 单母盘建议最大差异盘数（受母盘 IOPS 限制）；
  - 母盘/差异盘分布在 SSD 卷还是 HDD 卷的建议；
  - 卷空间水位告警阈值（如 85%）。

#### ㉓ 缺少 GC（垃圾回收）设计
- 需要回收：孤儿 VHDX、孤儿 target、超期 staging、失败 Job 的临时产物、过期租约、过期证书。
- 建议：统一的 GC 定时任务 + `meta/trash/` 保留期（默认 30 天）。

#### ㉔ 客户端"枚举用户"交互在无服务端时不可用
- design.md 服务端-存储库-6 要求"支持枚举用户名，以输入框+搜索选定"。这要求服务端提供用户搜索 API，并注意**普通用户不应能枚举全部用户**（隐私/信息泄露）。
- 建议：仅 `super_admin` 或库 owner 可搜索；普通用户只能按精确用户名添加。

#### ㉕ 空目录/超小目录的边界未定义
- design.md 第 15 条"在目录有内容时默认为用户填写表单中的数，以目录当前大小为准"。
- 需明确定义：空目录且未填 → 64MB 兜底（原为 1GB，会让每个分配一上来就占 1GB 配额）；目录远小于用户填值 → 取用户填值；目录远大于用户填值 → 取目录值 + 预留（10%，下限 16MB），一律按 **1MB** 粒度向上取整。

#### ㉖ 差异盘链深度未限制
- 理论上可多层差异盘，但每层放大 IO 与故障面。建议**硬限制单层**（差异盘不能作为父盘再派生）。

#### ㉗ "存储库复制"语义（已定稿：仅复制母盘）✅
- design.md 第 18 条"复制 vhdx（只允许复制没有被 iscsi 共享出去并被用户连接的盘）"。
- **决策：只复制母盘，不复制差异盘、不复制分配关系。** 复制结果是一个新的共享库（`parent_condition='IDLE'`，成员与分配为空）。
- 理由：差异盘按路径引用母盘，复制后链路失效；分配关系与具体用户绑定，跨库复制无意义。
- **NTFS 下的重要提示**：复制是**全量物理拷贝**（NTFS 无块克隆，见 5.9.2），需在 UI 上明确提示预计耗时与占用空间；同时必须执行 `ResetDiskIdentifier`（见 5.9.3），否则触发 Event ID 158。
- 前置校验（用**租约**判定，而非 iSCSI 查询）：源盘未挂载、未被发布、无 active lease。
- 建议**同时提供"派生差异盘"入口**并放在更显眼位置——对多数"想再多一个用户用"的诉求，差异盘才是省空间的正确解。

#### ㉘ 未定义多语言下服务端返回的错误文案
- 已在 6.4 / 7.5 给出方案（错误码 + 前端翻译）。

#### ㉙ 母盘更新会丢弃全部差异盘（产品语义缺口）★
- **问题**：design.md 新增的"维护状态"要求"进入维护状态需要支持清理差异盘"，且母盘需要更新。这意味着**每次更新母盘，所有分配出去的差异盘都会被删除**。design.md 未定义这对用户意味着什么。
- **影响（必须澄清）**：
  1. 若差异盘里只放**可再生数据**（游戏缓存、Shader 缓存、日志、临时配置）→ 清理无损失，方案成立；
  2. 若差异盘里含**用户存档/用户数据** → **清理即丢档**，且用户无法自行恢复。这是**产品级风险**，不是技术问题。
- **决策：采用方案 A —— 共享库明确定位为"仅承载可再生数据"** ✅
  > 产品定位：共享库里的差异盘只存放**可再生的数据**（游戏缓存、Shader 编译缓存、日志、临时配置、索引文件等）；**用户存档 / 用户数据必须放到独立独享库**。
  >
  > 这一定位使"母盘更新 → 清理差异盘"在业务上**无损**，是把该风险从技术问题降级为产品约束的最优解。

**必须交付的配套机制**（M4 阶段实现）

| # | 机制 | 说明 |
| --- | --- | --- |
| 1 | 定位写入产品文案 | 存储库创建页、分配页、客户端挂载页均需明确提示"共享模式差异盘不保证数据留存，请勿存放重要数据" |
| 2 | 维护前强提示 | 进入 `MAINTENANCE` 前的二次确认必须列出：将被删除的差异盘数量、受影响用户数、回收空间量；要求输入库名确认 |
| 3 | 维护前强制卸载 | 存在 active lease 时**拒绝进入维护**，并给出"请通知这些用户卸载"的清单 |
| 4 | 软删除 | 差异盘删除走 `meta/trash/`，保留 7 天（可配），期间支持人工恢复 |
| 5 | 主动通知 | 通过 SSE 向所有受影响用户推送 `parent_updated` 事件，客户端提示"共享库已更新，需重新挂载" |
| 6 | 审计留痕 | 记录 `parent_version` 变更、被删差异盘清单、回收空间、操作者 |
| 7 | 引导出口 | 客户端在分配页提供"我的重要数据 → 创建独享库"的引导入口，给出方案 |
| 8 | 数据保留兜底（可选） | 若个别用户确实需保留，提供**手动导出**：一次性全量拷贝该差异盘为独立库（明确提示耗时与空间） |

- **已否决的方案**（记录以备案）：
  | 方案 | 否决原因 |
  | --- | --- |
  | B. 自动导出差异盘 | 需要与差异盘等量的额外空间，与"省空间"的核心目标直接冲突；大规模场景不可行 |
  | C. 差异盘重挂载到新母盘 | Windows 不提供该能力；仅当新旧母盘块布局完全兼容时理论可行，**实际不可行** |

### 13.5 🆕「多服务端」专项评审（本轮新增能力）

design.md 新增的「多服务端」方向没问题，但原表述有 **3 处定义缺失**与 **4 处会造成具体故障**的问题，均已在 §3.4 中给出落地方案。

#### ⓐ 🔴 "版本对应" 未定义 —— 若按"相等"实现则功能不可用
- **问题**：原文"只有版本对应的情况下才会加载对应服务端的资源"，"版本"未指明是客户端版本、API 版本还是协议版本。
- **若按最直觉的"客户端版本 == 服务端版本"实现**：任何单边升级都会让客户端拒绝加载该服务端 → 用户视角是"存储库全空了"，会被当成数据丢失报障；且无法灰度。
- **已定方案**：**兼容区间**（写在服务端 `config.yaml` 的 `client_compat`，支持 `enabled: false` 关闭检查），三态判定 `COMPATIBLE / NEEDS_CLIENT_UPGRADE / NEEDS_SERVER_UPGRADE`。
- **关闭检查的边界（重要）**：`enabled: false` **只关闭"客户端版本号区间"判定**；`api_version` 协议校验**始终生效、不可关闭**——否则 v1 客户端连 v2 服务端会产生请求/响应结构不匹配的难查错误。见 3.4.2。

#### ⓑ 🔴 "不加载资源" 的失败模式未定义
- **问题**：未说明"不加载"是禁止登录、允许登录但列表为空，还是只读。
- **风险**：若静默显示空列表，用户会当成数据丢失。
- **已定方案**：**允许进入该服务端视图但置为不可用态 + 显式"版本不兼容"标识 + 可操作提示**；不兼容**不等于删除配置**。见 3.4.2 约束 4/5。

#### ⓒ 🔴 更新源"只以第一个为准"存在**死锁**
- **问题**：主服务端版本较旧时，客户端要连其他服务端需更高版本，但只能从旧源取包 → **永远升不上去**；主服务端离线时**完全无法更新**。
- **已定方案**：**主服务端（显式指定）+ 失败回退到其他 COMPATIBLE 服务端**；删除主服务端时必须让用户重新选择，不得静默切换。见 3.4.3 / 7.4.1。

#### ⓓ 🟠 更新包签名归属未定义（安全）
- **问题**：原文未说更新包由谁签名。若每台服务端各自签名，客户端需信任 N 个公钥 → **攻击面随服务端数量线性放大**，任一服务端被攻陷即可推恶意更新。
- **已定方案**：**统一发布方签名，客户端编译期内置公钥；服务端仅做 mirror，绝不持有签名私钥**。见 3.4.4 / 7.4.2。

#### ⓔ 🟠 缺少降级攻击防护
- **问题**：被控服务端可下发**旧版本**（含已知漏洞）让客户端降级。
- **已定方案**：新增**版本单调性检查**，拒绝低于当前版本的包；形成"签名 → SHA256 → 版本单调性 → 版本兼容性"四重校验链。见 7.4.2。

#### ⓕ 🟠 多服务端 = 多套独立 CA，证书管理必须重新设计
- **问题**：客户端要同时持有多个 CA 与多套客户端证书。若无隔离机制会出现"拿 B 的证书连 A"→ mTLS 失败 → 用户看到莫名错误。
- **已定方案**：**按服务端实例 ID 分目录存储**（`servers/<instance_id>/{ca.pem, trust.json, identities/<user_id>/}`）；连接某服务端时只加载该目录下的 CA 与证书。**扩展用于校验（服务端拒绝非本机的证书），不用于筛选**。见 9.1。

#### ⓕ2 🔴 缺少"首次信任建立"环节（安全缺口）
- **问题**：服务端用自签 CA，客户端首次连接**无法通过公信 CA 验证**。文档此前完全没写这一环 → 存在 MITM 敞口。
- **已定方案**：**TOFU + 指纹核对**（自建/小规模）或**带外分发 CA**（生产推荐）。
- **关键约束**：TOFU **只在首次生效**；之后检测到 CA 变化必须**拒绝 + 高优告警，绝不自动接受**（CA 静默变更正是 MITM 特征）。见 9.1。

#### ⓕ3 🟠 "枚举证书尝试登录" 会触发防爆破锁定
- **问题**：design.md 客户端-用户配置-2 写"尝试以现有的证书+网卡去**枚举尝试登录**"。若实现为"拿多张证书挨个试探"，**每次失败都消耗服务端失败登录计数** → 撞上防爆破（5 次锁定 15min）→ 用户被自己锁死。
- **已定方案**：客户端维护映射 `(server_instance_id, user_id) → 证书路径` **直接定位**，绝不盲试。见 9.1。

#### ⓕ4 🟡 私钥存储方案未定（影响 Go 实现路径）
- **问题**：原写"装进 Windows 证书库 + 私钥不可导出"，但 Go 标准库 `tls` 只接受 PEM，从证书库取私钥需 CNG/CAPI 互操作。
- **已定方案**：**方案 A（推荐）DPAPI 加密文件** —— Go 可直接读取，纯 syscall 实现；**方案 B Windows 证书库** 仅在需与系统集成时选用。取舍见 9.2，实测项 T19。
- **顺带修正**：原写"续期保持私钥/指纹不变"是矛盾的（换证书必然换指纹）→ 改为**绑定基于 SPKI 公钥指纹**，续期保留密钥对使 SPKI 不变，从而免重新审批。

#### ⓖ 🟠 挂载点/盘符冲突（必然发生）
- **问题**：两个服务端的存储库同名时，目录模式 `C:\Vault\<repo>` 直接撞车；盘符模式会重复分配。
- **已定方案**：目录模式强制 `<root>\<server-alias>\<repo>` 命名空间；盘符模式统一分配表 + 冲突检测。**连带影响**：挂载后脚本必须改用注入变量（`{MOUNT_PATH}` 等），不得硬编码路径。见 3.4.5。

#### ⓗ 🟡 客户端架构需从"单服务端"升级为"多服务端聚合器"
- **影响范围**：§3.2 部署形态、§3.4（新增）、§5.5 挂载路径、§5.7 租约、§7.1 进程架构、§7.4 更新、§7.6 配置、§9.1 证书 —— 均已同步更新。
- **服务端侧改动极小**：仅需 `system/info` 匿名可访问 + 新增 `server_instance_id` / `server_name` / `client_compat` / `features`。见 3.4.8。

#### ⓘ 🟡 "第一个服务端"语义不稳定
- **问题**：是"列表第 1 项"、"最近成功连接"还是"用户标记的主服务端"？用户删除第 1 项会导致更新源**静默切换**。
- **已定方案**：改为显式 `primary_server_id`，UI 可切换，删除时强制重选；设置页展示"当前更新源"。见 3.4.3。

#### ⓙ 🟡 跨服务端分组（已用"来源标识 + 同名聚合"解决）
- **问题**：design.md 客户端-存储库-8 的分组数据存在**各服务端**的库记录里，原本无法跨服务端归类。
- **已定方案**（你的决策）：**分组数据位置不变**（保留跨设备同步），客户端展示层**加服务端来源标识**，并支持**同名分组聚合视图**。这同时解决了与 13.2-⑨ 的连带冲突。见 3.4.6。
- **残留小坑**：聚合判定需做 **Unicode NFC 规范化**后比较，否则"游戏"的等价字符写法会导致漏聚合；重名分组若用户不想聚合，需提供"按服务端展示"视图切换（已纳入设计）。

#### ⓚ 🟡 其余小项（已在 §3.4 覆盖）
| 项 | 方案 |
| --- | --- |
| 启动探测 | 并发探测，总耗时 = max 而非 sum；单台超时 10s 不阻塞整体 |
| 故障隔离 | 每服务端独立熔断器 + 指数退避；B 故障不得影响 A |
| 离线可用 | 客户端离线时展示缓存列表并标注"离线" |
| 管理页归属 | 客户端内嵌管理页需明确"管理哪台服务端"，按当前选中服务端路由 |
| 身份差异 | 同一用户在 A/B 可能账号不同或单侧无账号 → "我的存储库"为联邦列表，必须标注来源 |
| 移除服务端 | 需处理其上的活跃挂载（提示先卸载或强制卸载并告知） |

---

## 14. 待实测清单与风险登记册

### 14.1 必须先实测的平台行为（M0 阶段）

| # | 待验证项 | 影响 |
| --- | --- | --- |
| T1 | `Set-IscsiServerTarget -Enable $false` 是否**立即**断开已建立会话？时延多少？ | 踢下线的可用性（核心） |
| T2 | 缩小 `-InitiatorIds` 后，已建立的越权会话是否立即终止？ | 授权收紧的时效性 |
| T3 | 同一 Host 上 target 数量上限（数百/数千？），以及大量 target 的性能影响 | "1 分配 = 1 target" 的可扩展性（核心） |
| T4 | 能否把 iSCSI 虚拟盘设置为**只读**（母盘 `TEMP_SHARED` 的只读保证） | 只读共享的可靠性 |
| T5 | `Add-IscsiVirtualDiskTargetMapping` 的准确参数名（`-TargetName`/`-DevicePath`） | 脚本正确性 |
| T6 | 服务端 iSCSI 事件日志的确切 LogName（用于监控） | 可观测性 |
| T7 | **NTFS 上承载 VHD/VHDX 的卷启用去重**是否受支持（官方仅说明"并不总是受支持"） | 决定是否彻底排除 Dedup |
| T8 | `New-IscsiVirtualDisk -ParentPath` 是否要求父盘已 `Import-IscsiVirtualDisk` | 差异盘流程 |
| T9 | 无 Hyper-V 角色时如何 compact（`Optimize-VHD` 是否可用；回退用 `Optimize-Volume -ReTrim`） | 空间回收方案 |
| T10 | 改名/删除 target 后，客户端 iSCSI 缓存（`Delete-IscsiTarget`）清理是否彻底 | 同名复用的冲突 |
| T11 | 目录挂载点 `Add-PartitionAccessPath -AccessPath` 在 Server 2025 的实际行为与错误码 | 目录挂载形态 |
| T12 | CHAP secret 的合法长度范围（12~16 字节？）与非法值的报错 | 密钥生成 |
| T13 | 母盘在 `TEMP_SHARED` 下 iSCSI 只读的实际表现（客户端能否绕过写） | 母盘一致性保护 |
| T14 | `SetVirtualDiskInformation(SET_VIRTUAL_DISK_INFO_IDENTIFIER)` 的确切枚举值与可用性（无 Hyper-V 时重置 DiskIdentifier） | 复制流程的必备步骤 |
| T15 | 底层卷是否支持 ODX（判断 `CopyFileEx` 能否被 offload 加速） | 复制性能（不省容量） |
| T16 | 复制后的 VHDX 是否真的会触发 Event ID 158（验证 `ResetDiskIdentifier` 的必要性） | 复制流程正确性 |
| T17 | 证书**自定义扩展**（服务端实例 ID）用 Go `crypto/x509` 写入与解析是否可行（`ExtraExtensions` 生成 + 服务端读取校验） | 多服务端证书绑定校验（9.1）可行性 |
| T18 | `Add-PartitionAccessPath -AccessPath` 挂载到**多级子目录**（`C:\Vault\<alias>\<repo>`）的行为是否与单级一致 | 多服务端目录命名空间（3.4.5） |
| T19 | **DPAPI**（`CryptProtectData`/`CryptUnprotectData`）纯 Go syscall 调用是否可行；加密文件在**服务账户 vs 用户账户**下的解密边界（决定私钥存储方案 A/B，见 9.2） | 客户端私钥存储选型 |

**Linux 侧待实测项（LVM thin + LIO）**

| # | 待验证项 | 影响 |
| --- | --- | --- |
| T20 | `lvcreate --type thin -V <size>B` 的 **`B` 单位**是否在各发行版 LVM2 版本上都被正确解析（不带单位会被当 MB） | 盘容量正确性（**核心**，错一位差 1000 倍） |
| T21 | thin 快照（`lvcreate -s`）的**写时复制**在没有 `-L/-V` 时是否在所有版本上被接受 | 差异盘创建（核心） |
| T22 | thin LV 默认 `skip-activation`（`lv_attr` 第 10 位 `k`）在目标发行版上是否成立、`lvchange -ay -K` 是否必需 | 发布前激活（否则 backstore 指向未激活 dm 节点） |
| T23 | `thin_ls` 的 `MAPPED_BLOCKS` 语义（是否含未提交元数据）、与 `lvs data_percent` 的偏差量级 | 配额记账口径（核心） |
| T24 | LIO 的 **per-ACL `write_protect=1`** 是否真的能阻止客户端写入（而非仅提示） | 母盘只读共享的可靠性（对应 T4） |
| T25 | LIO CHAP 的 **12–16 字节纯 ASCII** 硬限制在目标内核上的实际报错行为；Base64(12B)=16 字符是否稳定落在边界内 | 密钥生成（对应 T12） |
| T26 | LIO 会话枚举（`sessions/`）与强制登出的**时延**，以及是否存在无法清理的僵死会话 | 踢下线能力（Linux 侧是优势项） |
| T27 | `lvremove` 在 LUN 仍在线时的行为（是否被拒、是否会残留 dm 节点） | 删除流程顺序（先下线再删） |
| T28 | `lvconvert --type cache --cachepool <vg>/<cp> <vg>/<pool>_tdata` 在**已有数据**的 thin pool 上是否安全、耗时量级、是否需停机 | 缓存挂载（生产可操作性，**核心**） |
| T29 | 改 `cachemode`（writethrough↔writeback）时是否会触发**全量刷脏**、能否在线进行 | 缓存策略调整 |
| T30 | `cache_health`（`lvs -o cache_health`）在各版本上的取值集合（`Fail` / `needs_check` / 空） | 缓存健康告警 |
| T31 | `lsblk --json -o ROTA,MOUNTPOINTS` 在目标发行版 util-linux 上的可用性（旧版缺列） | 块设备选择器字段完整性（已做退化，但需确认不丢关键信息） |
| T32 | `thin` LV 上的 `discard`（`fstrim`/`blkdiscard`）是否把块真正归还 thin pool（否则空间回收无效） | 空间回收（对应 T9） |
| T33 | 卷组名 / thin pool 名含 `.` `+` 时 LVM 的实际接受度（`checkLVMNames` 目前禁止 `-`、空白、`/`、`\`） | 配置校验的松紧 |
| T34 | configfs 路径下**大小写敏感**导致的重复 target（IQN 归一化前后）在真实内核上的表现 | target 唯一性 |
| T35 | `flock` 锁文件在 `/run/lock` 无写权限时回退到 `$TMPDIR` 的行为，以及**容器化**场景下 `/run/lock` 是否共享 | 单实例锁有效性 |

### 14.2 风险登记册

| ID | 风险 | 概率 | 影响 | 缓解 |
| --- | --- | --- | --- | --- |
| R1 | 无法真正强制断开客户端，用户可绕过"踢下线" | 高 | 中 | 目标保持 disabled + 缩短 TTL + 审计告警；产品语义降级并告知用户 |
| R2 | 母盘被误写（绕过 Vault）导致全部差异盘污染 | 中 | **极高** | `parent_condition` 状态机 + **指纹校验** + 版本号校验 + 目录 ACL；Reconciler 定期比对 |
| R3 | **母盘更新导致所有差异盘被删除**，用户数据丢失 | 高 | **高** | 明确"共享库仅承载可再生数据"的产品定位；软删除 + 7 天保留；强提示与二次确认（见 13.4-㉙） |
| R4 | 大量差异盘叠加导致母盘 IO 瓶颈 | 中 | 高 | 限制单母盘差异盘数；母盘放 SSD；监控母盘 IO |
| R5 | 服务端崩溃造成孤儿 target/盘/staging | 高 | 中 | Reconciler + GC + 全幂等清理 |
| R6 | 上传大目录时磁盘暂存空间耗尽 | 中 | 高 | staging 独立配额 + 上传前预检空间 + 保留水位 |
| R7 | 客户端网络抖动导致误判离线（租约过期） | 中 | 中 | TTL 留足余量（120s）；离线不立即删盘，仅停用 target |
| R8 | PowerShell 版本/语言差异导致输出解析失败 | 中 | 中 | 统一 JSON 输出；`-NoProfile -NonInteractive`；CI 覆盖多版本 |
| R9 | NTFS 下复制母盘耗时过长/空间不足（无块克隆） | **高** | 中 | UI 明确预估耗时与占用；复制前空间预检；引导用户改用差异盘 |
| R10 | 复制后未重置 DiskIdentifier → Event ID 158 / VSS 失败 | 中 | 中 | 复制流程强制 `ResetDiskIdentifier`；Reconciler 抽检 DiskIdentifier 唯一性 |
| R11 | 证书过期导致客户端集体失联 | 低 | 高 | 到期前 30 天告警 + 支持续期不换指纹 |
| R12 | SQLite 放在网络共享上导致 DB 损坏 | 低 | **极高** | 启动时检测 DB 路径所在卷是否为网络驱动器，直接拒绝启动 |
| R13 | 单母盘差异盘数量过多导致管理复杂度与故障面失控 | 中 | 中 | `repositories.max_diff_disks` 在**创建时由 owner 配置**（默认 50）+ UI 提示；监控母盘 IO 与差异盘数量，超阈值告警 |
| R14 | 共享库被用户当作"网盘"存放重要数据，母盘更新后丢失 | 中 | 高 | 产品定位为"仅可再生数据"（15.3-6）；创建/分配/挂载三处文案提示；提供"创建独享库存放重要数据"引导入口（13.4-㉙） |
| R15 | 版本区间配错（如 `min` 高于当前客户端）导致所有客户端被判不兼容、资源整体不可见 | 中 | **高** | ① 区间写在 `config.yaml`，模板提供合理默认；② 字段缺失自动按服务端版本推导；③ **显式非法值 fail-fast**（`min > max`、格式错误直接拒绝启动，避免"全部客户端不可见"这种更难查的故障）；④ 热重载时打印变更前后值并写审计；⑤ 保留 `client_compat.enabled: false` 作为应急开关 |
| R16 | 主服务端离线 → 客户端无法更新；或主服务端被移出配置 → 更新源静默变化 | 中 | 中 | 主服务端 + 失败回退（3.4.3）；删除主服务端强制重选；设置页始终显示当前更新源 |
| R17 | 误用其他服务端的证书发起连接，报错难定位 | 中 | 中 | 证书按服务端分目录存储（路径即定位）；扩展用于**服务端侧校验**并返回明确错误码"证书不属于本服务端"（9.1） |
| R18 | 多服务端同名库导致目录挂载点冲突或盘符重复分配 | **高** | 中 | 目录模式强制 `<alias>\<repo>` 命名空间；盘符统一分配表 + 冲突检测；别名生成时规范化与去重（3.4.5） |
| R19 | 被控服务端下发旧版本包实施降级攻击 | 低 | **高** | 版本单调性检查 + 统一发布方签名 + 编译期内置公钥（7.4.2） |
| R20 | 服务端证书 SAN 与实际访问 URL 不一致导致 TLS 校验失败（换 IP / 换域名） | 中 | 中 | 签发时同时写入域名与固定 IP 两个 SAN；客户端配置校验并在添加服务端时给出明确提示 |
| R21 | 为排障临时关闭版本检查（`client_compat.enabled: false`）后**忘记恢复**，导致不兼容的旧客户端长期接入 | 中 | 中 | 启动时对 `enabled: false` 打 WARN；管理页顶部常驻告警条；审计记录开启/关闭；可选：关闭超过 N 天后升级为 ERROR 并每日提醒 |
| R22 | 客户端"枚举证书尝试登录"消耗失败计数，**把用户自己锁死**（撞防爆破 5 次/15min） | 中 | 中 | 客户端用 `(server_instance_id, user_id) → 证书` 本地映射直接定位，**禁止盲试**；确认类请求不计入失败计数（服务端侧加标记） |
| R23 | 服务端 CA 被静默替换（重装/被攻陷），客户端未察觉而继续信任 → **MITM** | 低 | **极高** | 首次信任后锁定 CA 指纹（`trust.json`）；指纹变化 → **拒绝连接 + 高优告警 + 要求人工重新确认**，绝不自动接受 |
| R24 | **thin pool 数据/元数据被写满** → 所有 thin LV 集体只读/损坏（thin 的经典故障） | 中 | **极高** | 启动/定期读 `data_percent`、`metadata_percent`；超过 `watermark_percent`（默认 90）**拒绝新建盘并告警**；元数据给足（`metadata_size` 默认 4G）；`autoextend` 仅作缓冲不作为主手段 |
| R25 | thin **快照**作为差异盘时，单盘物理增长无上限（写满池拖垮全部盘） | 中 | **极高** | 必须在**每盘**建/写前检查池水位（见 R24 的水位闸门）；配 `discard` 与定期回收；文档明确"共享库水位硬约束" |
| R26 | `lvcreate -V` **漏写 `B` 单位** → 盘容量被当 MB 解析，实际为期望值的 1/1000 | 中 | **极高** | 所有尺寸统一以**字节字符串 + `B`** 生成（代码内不出现裸数字）；以 T20 实测兜底 |
| R27 | 发布时 LV **未激活**（thin 默认 skip-activation）→ backstore 指向不存在的 dm 节点 | 中 | 高 | `linuxbackend` 在 `EnsureTarget`/`EnsureVirtualDisk`/`AttachLun` 前统一 `Activate`；以 T22 实测 |
| R28 | `lvremove` 时 LUN 仍在线 → 残留 dm 节点 / 内核告警 / 需重启清理 | 中 | 中 | 删除流程强制"先登出 → 清 LUN 符号链接 → `lvchange -an` → `lvremove`"；以 T27 实测 |
| R29 | 给**已有数据**的 thin pool 挂 dm-cache 时中断 → 缓存元数据不一致 | 低 | 高 | 初始化走**幂等 + 状态可查**；操作前提示耗时与风险；失败时按 `cache_health`/`needs_check` 引导修复；以 T28/T30 实测 |
| R30 | 把 `storages.path` 误写成 `<vg>/<thin_pool>`，或指向**未被受管的宿主目录** → 上传暂存退化成 CWD 相对路径 / 写入绕过 thin pool 落到根文件系统 | 中 | 高 | 文档与代码注释双重约定（§5.15）：**path 恒为受管存储 LV 的挂载点目录**，磁盘位置由 VG 决定；Linux 上不再从配置种入目录存储；路径一律经 `EvalSymlinks` + 前缀校验 |
| R31 | 存储 LV 未真正挂上（`mount` 成功但实际未生效）→ 写入落宿主根文件系统，日后挂载成功会把数据"藏"进挂载点底下 | 中 | 高 | **fail-closed**：挂载后核对 `/proc/self/mountinfo`（挂载点 + 设备双重比对）；未确认就绪的存储被剔除出落盘集合（§5.15.4） |
| R32 | 运维手工 `umount` 或重启后失挂 → 存储静默不可用 / 写入落到宿主根文件系统 | 中 | 高 | 启动时 + 每 5 分钟对账幂等重挂，**不写 /etc/fstab、不生成 systemd unit**；可手工 umount 验证降级与自愈（§5.15.4） |
| R33 | 对**登记已有卷**（`managed=false`）执行 `lvremove` → 用户既有数据被删 | 低 | **极高** | `managed` 显式落库；删除流程仅在 `managed=true` 时 `lvremove`，否则只卸载 + 解除登记；登记路径**绝不格式化**（§5.15.5） |
| R34 | 卸载/删除存储卷时有进行中的上传 → 写入静默落到宿主根文件系统 | 中 | 高 | 卸载/删卷前用 `CountActiveUploadsUnderPath`（state ∈ {open, verifying}）把关，有则 409 `storage.volume_busy`（§5.15.5） |
| R35 | `flock` 在容器/无权限场景下失效 → 多个服务端实例同时写同一 DB | 低 | **极高** | 锁目录回退链（`/run/lock` → `/var/lock` → `$TMPDIR`）+ 获取失败即**拒绝启动**；以 T35 实测 |

---

## 15. 附录

### 15.1 与 design.md 的条目对照

| design.md 条目 | 本文对应章节 | 状态 |
| --- | --- | --- |
| 需求分析（共享模式，L7） | 5.4 / 4.3 | ✅ 已形式化为 `parent_condition` 状态机 |
| 服务端-管理 vhdx 1~5 | 5.2 / 5.9 / 6 | ✅ 覆盖；**复制方案重定义**（NTFS 无块克隆） |
| 服务端-管理 iscsi 1~3 | 5.3 / 5.7 / 5.8 | ⚠️ 第 3 条能力降级（见 13.1-①） |
| 服务端-存储库 1~8 | 4 / 5.3 / 5.4 / 5.6 / 13.2-⑨ / 13.4-㉙ | ✅ 第 2 条已补"维护态"；㉙ 为新增产品风险 |
| 服务端-本地用户 1~4 | 9.1~9.3 | ✅ 第 1 条已澄清（证书为主 + IP 限制），见 13.3-⑯ |
| 服务端-限制 1（路径白名单） | 5.1 / 9.4 | ✅ |
| 服务端-管理页 1~3 | 6 / 8 / 13.3-⑰ | ✅ 第 3 条组件级复用已给出落地规则（统一 `packages/ui`） |
| **服务端-平台后端抽象（Windows / Linux 并列）** | **2.4 / 5.14 / 6.2 / 6.6 / 12.2** | ✅ 已实现：差异全部收敛在 `internal/platform` 两后端 + `buildPlatform` 两构建变体 |
| **服务端-LVM 存储池与缓存设备选择** | **5.14 / 6.3 / 14.1(T20~T35) / 14.2(R24~R31)** | ✅ 已实现：`/v1/system/{lvm,block-devices,lvm/initialize}`；缓存用 LVM 原生 dm-cache，不自研 |
| **服务端-块设备选择器（排除系统盘）** | **5.14** | ✅ 判定真源在服务端（`unavailableReason`），前端只按 `reason` 置灰 |
| 客户端-存储库 1~5 | 5.5 / 5.10 / 7 | ✅ 目录挂载约束已补充 |
| 客户端-用户配置 1~2 | 7.3 / 7.6 / 9.1 | ⚠️ 第 2 条"枚举证书尝试登录"需改为**本地映射定位**（避免撞防爆破），见 13.5-ⓕ3 |
| 客户端-模块划分 1~2 | 7.1 / 8.1 | ✅ |
| 客户端-自动更新 1~2 | 7.4 / 13.2-⑪ | ✅ 已含"启动自动 + 设置页手动"双入口 |
| 客户端-i18n | 7.5 | ✅ |
| 客户端-可操作性 1~2 | 7.2 | ✅ |
| 客户端-心跳（L79-80） | 5.7 / 5.8 / 4.3 | ✅ **本文与该设计一致**：客户端心跳为在线状态权威来源 |
| **多服务端（L82-84）** | **3.4 / 13.5 / 7.4 / 7.6 / 9.1** | ⚠️ **需按 13.5 修正**：版本改兼容区间、更新源加回退、签名统一发布方、分组加来源标识 |
| 技术选型 | 10 | ✅ Electron 已定；DB = SQLite + MySQL 双驱动；**cgo 已确认不使用** |
| 其他（构建脚本与依赖说明） | 12 | ✅ |

### 15.2 参考文档

- 平台能力调研：[windows-iscsi-vhdx-api.md](./windows-iscsi-vhdx-api.md)
- **Linux 侧（LVM thin + LIO）**：
  - [lvmthin(7) — thin provisioning](https://man7.org/linux/man-pages/man7/lvmthin.7.html)
  - [lvmcache(7) — dm-cache / cache pool](https://man7.org/linux/man-pages/man7/lvmcache.7.html)
  - [lvcreate(8) / lvconvert(8) / lvs(8) / thin_ls(8)](https://man7.org/linux/man-pages/man8/lvcreate.8.html)
  - [LIO iSCSI target（`target_core_mod` / configfs）](https://www.kernel.org/doc/Documentation/target/tcm_mod_builder.py)
  - [configfs — 内核对象文件系统](https://www.kernel.org/doc/Documentation/filesystems/configfs/configfs.txt)
- [IscsiTarget 模块](https://learn.microsoft.com/en-us/powershell/module/iscsitarget/?view=windowsserver2025-ps)
- [Optimize-VHD](https://learn.microsoft.com/en-us/powershell/module/hyper-v/optimize-vhd?view=windowsserver2025-ps)
- [go-winio/vhd](https://pkg.go.dev/github.com/Microsoft/go-winio/vhd)
- [Mount-DiskImage](https://learn.microsoft.com/en-us/powershell/module/storage/mount-diskimage)
- [MS-FSCC 附录 B：FSCTL_DUPLICATE_EXTENTS_TO_FILE 仅 ReFS 支持](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-fscc/d4bc551b-7aaf-4b4f-ba0e-3a75e7c528f0)
- [Event ID 158：相同磁盘标识（KB2983588）](https://learn.microsoft.com/en-us/troubleshoot/windows-client/backup-and-storage/event-id-158-for-identical-disk-guids)
- [Set-VHD（含 `-ResetDiskIdentifier`）](https://learn.microsoft.com/en-us/powershell/module/hyper-v/set-vhd)
- [SetVirtualDiskInformation](https://learn.microsoft.com/en-us/windows/win32/api/virtdisk/nf-virtdisk-setvirtualdiskinformation)
- [数据去重互操作性（含"承载 VHD/VHDX 的卷并不总是受支持"）](https://learn.microsoft.com/en-us/windows-server/storage/data-deduplication/interop)

### 15.3 已决策项与遗留决策

**已决策（本轮确认，不再讨论）**

| # | 决策点 | 结论 |
| --- | --- | --- |
| 1 | D3 客户端外壳 | **Electron**，且管理页与客户端**统一 UI**（同一套 `packages/ui`） |
| 2 | ⑧ 共享库配额口径 | **母盘占用由 owner 独担**，成员只计自己的差异盘 |
| 3 | ㉗ 存储库复制语义 | **仅复制母盘**（不含差异盘与分配关系） |
| 4 | D5 卷文件系统 | **仅 NTFS** |
| 5 | D7 母盘保护方式 | **应用层状态标记**（不强制只读挂载），辅以指纹 + 版本号校验 |
| 6 | ㉙ 母盘更新时的用户数据处理 | **明确产品定位为"仅承载可再生数据"**；配套 8 项机制见 13.4-㉙ |
| 7 | 数据库 | **SQLite(WAL) 与 MySQL 双驱动，均可切换**（默认 SQLite）；工程要求见 10.2 |
| 8 | 单母盘差异盘上限 | **创建存储库时由 owner 配置**（`repositories.max_diff_disks`，默认 50） |
| 9 | 服务端是否使用 cgo | **不使用**（零处需要，统一 `CGO_ENABLED=0`）；依赖选型约束见 10.4 |
| 10 | 多服务端 · 版本判定 | **兼容区间**（`client_compat.min/max`），禁止版本相等。见 3.4.2 |
| 11 | 多服务端 · 更新源 | **主服务端（显式指定）+ 失败回退**。见 3.4.3 |
| 12 | 多服务端 · 更新包签名 | **统一发布方签名**，客户端编译期内置公钥；服务端仅 mirror。见 3.4.4 |
| 13 | 多服务端 · 分组与来源 | 分组数据位置不变；客户端**加来源标识**，**同名分组可聚合**。见 3.4.6 |
| 14 | D1 服务端平台 | **Windows 与 Linux 并列共存**：同一代码库，`internal/platform` 双后端 + `buildPlatform` 分平台构建；上层零改动。见 2.4 / 5.14 |
| 15 | Linux 侧 iSCSI 后端 | **LIO**（configfs 直控，零外部依赖、零 cgo）；不使用 targetcli / tgt / ietd |
| 16 | Linux 侧虚拟磁盘 | **LVM thin 逻辑卷**；差异盘 = **thin 快照**（天然省空间） |
| 17 | Linux 侧缓存加速 | **用 LVM 原生 dm-cache**，系统**只提供"块设备选择器 + 状态展示"**，**不自行实现任何缓存层** |
| 18 | 缓存设备是否进配置 | **不进**：由 LVM 自身持久持有；配置只描述 VG / thin pool 与建池参数 |
| 19 | 池初始化语义 | **一次性且幂等**：已存在的 VG / thin pool / 缓存一律跳过，**绝不覆盖既有数据** |

**遗留决策（工作量大但非阻塞，可在对应阶段前敲定）**

| # | 决策点 | 选项 | 本文倾向 |
| --- | --- | --- | --- |
| A | 回收站（软删除）保留期 | 7 天 / 30 天 | 差异盘 **7 天**（空间敏感）；母盘 **30 天** |
| B | 是否需要 PostgreSQL 支持 | 不需要 / 作为第三后端 | **不需要**（双驱动已覆盖，避免维护成本扩散） |
| C | 服务端别名（alias）默认来源 | 主机名 / `server_name` / 用户手填 | 主机名（本地唯一性最好），添加时允许修改 |
| D | 更新检查频率与自动下载策略 | 每次启动 / 每 N 小时 / 仅手动 | 启动检查 + 每 6 小时，自动下载但不自动重启 |
