# Windows Server 通过 API 管理 VHDX 与 iSCSI 目标

> 调研目标：在 Windows Server 上以程序化方式（Go 服务）实现
> ① 从目录创建 VHDX（复制/剪切两种语义）　② 从 VHDX 创建差异盘　③ 把 VHDX 发布为 iSCSI 目标并编辑授权
> ④ 从服务端断开已连接的 iSCSI 客户端　⑤ iSCSI / VHDX 的基础增删查
>
> 本文所有 API 名称均来自微软官方文档，标注为「需实测」的条目为官方文档未明确说明、需在目标环境验证的行为。

---

## 0. 结论速览

1. **没有一套 API 能覆盖全部 5 项能力**，需要组合使用：
   - **VHDX 层面**（创建/挂载/差异盘）→ 原生 Win32 **VirtDisk API**（`virtdisk.dll`），Go 可直接用 `github.com/Microsoft/go-winio/vhd`。
   - **iSCSI 层面**（target / 授权 / 映射）→ PowerShell **`IscsiTarget` 模块**（Windows Server 2012+ 内置），它是 `root\Microsoft\Windows\Storage` 命名空间下 CIM 类的封装。
   - **分区/格式化** → PowerShell **`Storage` 模块**（`Get-Disk`/`Initialize-Disk`/`New-Partition`/`Format-Volume`），比 `diskpart` 脚本更可靠。
2. **能力 ④「服务端断开客户端」是本次调研中唯一没有原生 API 的能力**。iSCSI Target Server 未提供「按会话强制登出」的公开接口，只能用「停用目标 / 解绑 LUN / 重启 WinTarget 服务」等间接手段实现，详见第 6 章。
3. iSCSI 目标使用的 VHDX **必须先在本机分离（Detach）**，才能被 `Import-IscsiVirtualDisk` 纳入并映射给 initiator；同一个 VHDX 不能被本机挂载与 iSCSI 发布同时持有。

---

## 1. 环境与前置条件

| 项目 | 要求 |
| --- | --- |
| OS | Windows Server 2012 / 2012 R2 / 2016 / 2019 / 2022 / 2025（VHDX 自 Server 2012 起支持） |
| 权限 | **管理员**（VirtDisk 的 `AttachVirtualDisk`、iSCSI 管理均要求提权） |
| 角色 | iSCSI 目标服务器：`Install-WindowsFeature FS-iSCSITarget-Server`（同时装管理工具） |
| 服务 | `WinTarget`（Microsoft iSCSI Target Service）；客户端侧为 `msiscsi` |
| 防火墙 | TCP **3260**（iSCSI）；远程管理还需 WMI/RPC 动态端口 |
| 可选 | Hyper-V 角色 —— **不是必须**。`New-VHD` 属于 Hyper-V 模块，需要该角色；`vhd` 包 / `New-IscsiVirtualDisk` / `Storage` 模块均不需要 |
| VHDX 存放位置 | 不能位于网络共享、压缩、稀疏（sparse）或事务性（TxF）文件夹中；父盘/子盘都要满足 |

> 部署建议：Vault 服务本身若要调用 iSCSI 管理 cmdlet，注意 `Import-Module IscsiTarget` 需要在 64 位 PowerShell 下执行。

---

## 2. 技术路线对比

| 路线 | 覆盖能力 | 依赖 | 优点 | 缺点 |
| --- | --- | --- | --- | --- |
| **A. 原生 VirtDisk API**（`virtdisk.dll`） | ① ② 部分⑤ | 无（系统自带 dll） | 无外部依赖、可异步、粒度最细、Go 有现成封装 | 不含 iSCSI；分区格式化需二次调用 |
| **B. PowerShell cmdlet** | 全部 | PowerShell 5.1 + 角色 | 覆盖面最全、语义清晰 | 进程调用开销、需解析输出、需处理提权/执行策略 |
| **C. WMI/CIM**（`root\Microsoft\Windows\Storage`） | 全部 | 无 | 无 PowerShell 依赖、可远程 | Go 侧无官方库（`go-ole`/`go-wmi` 实现成本高），方法名文档不全 |
| **D. `diskpart` / `iscsicli` 脚本** | 分区格式化、兜底 | 系统自带 | 简单直接 | 无结构化返回、错误码粗糙、`iscsicli` 属 initiator 侧 |

**推荐组合（针对 Go 项目）**

- VHDX 创建 / 挂载 / 卸载 / 差异盘 → **路线 A**（`go-winio/vhd`），零外部依赖、类型安全。
- 分区 + 格式化 → **路线 B**（`Storage` 模块，`Get-Disk` 按 Location 匹配 VHDX 路径）。
- iSCSI target / 虚拟盘 / 授权 / 映射 → **路线 B**（`IscsiTarget` 模块），必要时用 `Get-CimClass` 现场确认底层方法名后改走 **路线 C**。

---

## 3. 能力①：从目录创建 VHDX（复制 / 剪切）

### 3.1 语义澄清与总体流程

> **前提理解**：VHDX 是块设备镜像，本身不能"装目录"。因此该需求的实际含义是——**把某个源目录的内容打包进一个新 VHDX 并格式化**。
> - **复制**：源目录保留。
> - **剪切**：源目录内容进入 VHDX 后，源目录被删除。
>
> 若你的本意是"从目录里已存在的 vhdx 文件派生"，请参见第 4 章（差异盘）。

```
创建 VHDX(动态/固定)
   ↓
挂载(Attach)为磁盘
   ↓
初始化 + 分区 + 格式化(得到盘符, 如 V:)
   ↓
拷贝源目录 → V:\  （剪切语义：拷贝成功并校验后再删源）
   ↓
分离(Detach)
```

### 3.2 步骤 A：创建 VHDX

#### 方式 1：VirtDisk API（推荐，无依赖）

关键结构（`virtdisk.h`）：

```c
typedef struct _CREATE_VIRTUAL_DISK_PARAMETERS {
  CREATE_VIRTUAL_DISK_VERSION Version;   // 1 或 2
  union {
    struct { GUID UniqueId; ULONGLONG MaximumSize; ULONG BlockSizeInBytes;
             ULONG SectorSizeInBytes; PCWSTR ParentPath; PCWSTR SourcePath; } Version1;
    struct { GUID UniqueId; ULONGLONG MaximumSize; ULONG BlockSizeInBytes;
             ULONG SectorSizeInBytes; ULONG PhysicalSectorSizeInBytes;
             PCWSTR ParentPath; PCWSTR SourcePath;
             OPEN_VIRTUAL_DISK_FLAG OpenFlags;
             VIRTUAL_STORAGE_TYPE ParentVirtualStorageType;
             VIRTUAL_STORAGE_TYPE SourceVirtualStorageType;
             GUID ResiliencyGuid; } Version2;
  };
} CREATE_VIRTUAL_DISK_PARAMETERS;
```

创建参数组合（官方推荐）：

| 磁盘类型 | `Flags` | `ParentPath` | `SourcePath` | `MaximumSize` |
| --- | --- | --- | --- | --- |
| 固定大小 | `CREATE_VIRTUAL_DISK_FLAG_FULL_PHYSICAL_ALLOCATION`(1) | 必须 NULL | 可选 | 目标大小 |
| 动态扩展（稀疏） | `CREATE_VIRTUAL_DISK_FLAG_NONE`(0) | 必须 NULL | 可选 | 目标大小 |
| **差异盘** | `CREATE_VIRTUAL_DISK_FLAG_NONE`(0) | **父盘绝对路径** | 必须 NULL | **必须为 0** |

必备常量：

```c
VIRTUAL_STORAGE_TYPE st = { VIRTUAL_STORAGE_TYPE_DEVICE_VHDX /* 2 */,
                            VIRTUAL_STORAGE_TYPE_VENDOR_MICROSOFT };
CreateVirtualDisk(&st, path, VIRTUAL_DISK_ACCESS_CREATE, NULL,
                  flags, 0, &params, NULL /*Overlapped*/, &handle);
```

**易踩的坑（官方明确列出）**：
- 使用 `CREATE_VIRTUAL_DISK_VERSION_2` 时，`VirtualDiskAccessMask` **必须为 `VIRTUAL_DISK_ACCESS_NONE`(0)**，否则返回 `ERROR_INVALID_PARAMETER`(87)。
- `MaximumSize` 必须是 512 的倍数、且 ≥ 3 MB；指定 `ParentPath` 时必须为 0。
- `ParentPath` 与 `SourcePath` 不能同时非 NULL。
- `BlockSizeInBytes` 只能是 `0`(默认 2MB)、`0x80000`(512KB)、`0x200000`(2MB)。

#### 方式 2：`Storage` / Hyper-V / IscsiTarget 模块

| cmdlet | 创建格式 | 依赖 |
| --- | --- | --- |
| `New-VHD -Path x.vhdx -Dynamic -SizeBytes 100GB` | VHD / VHDX | **Hyper-V 模块** |
| `New-VHD -Path x.vhdx -Differencing -ParentPath p.vhdx` | 差异盘 | Hyper-V 模块 |
| `New-IscsiVirtualDisk -Path x.vhdx -SizeBytes 100GB` | **仅 VHDX** | iSCSI 目标服务器角色 |
| `New-IscsiVirtualDisk -Path child.vhdx -ParentPath p.vhdx` | 差异 VHDX | iSCSI 目标服务器角色 |

> 注意：不带 Hyper-V、也不想依赖 iSCSI 角色时，裸机创建 VHDX 只有 VirtDisk API 一条干净路径（`diskpart create vdisk` 官方文档只描述 `.vhd` 的三类磁盘，`.vhdx` 行为未在文档中说明——**需实测**）。

### 3.3 步骤 B：挂载 + 分区 + 格式化

VirtDisk 路线：`OpenVirtualDisk` → `AttachVirtualDisk`（需提权）→ `GetVirtualDiskPhysicalPath` 拿到 `\\?\PhysicalDriveN`。

PowerShell 路线（更省事，且天然避开盘符分配问题）：

```powershell
$vhdx = 'D:\vault\vol01.vhdx'

# 1) 挂载（Native Path 可自动分配盘符；若卷已有文件系统，直接用卷 GUID 路径更稳）
$img = Mount-DiskImage -ImagePath $vhdx -PassThru
if (-not $img) { throw "Mount-DiskImage failed: $vhdx" }

# 2) 按 Location 反查磁盘号（Location 就是 VHDX 文件路径）
$disk = Get-Disk | Where-Object Location -EQ $vhdx
if (-not $disk) { Update-HostStorageCache; $disk = Get-Disk | Where-Object Location -EQ $vhdx }
if (-not $disk) { throw "disk not found for $vhdx" }

# 3) 初始化（已初始化会报错，按需捕获忽略）
if ($disk.PartitionStyle -eq 'RAW') {
    Initialize-Disk -Number $disk.Number -PartitionStyle GPT
}

# 4) 分区 + 格式化 + 分配盘符
$part = Get-Partition -DiskNumber $disk.Number -ErrorAction SilentlyContinue |
        Where-Object Type -NE 'Reserved' | Select-Object -First 1
if (-not $part) {
    $part = New-Partition -DiskNumber $disk.Number -UseMaximumSize -AssignDriveLetter
}
$drive = $part.DriveLetter
if (-not $drive) { Add-PartitionAccessPath -DiskNumber $disk.Number -PartitionNumber $part.PartitionNumber -AssignDriveLetter; $drive = (Get-Partition -DiskNumber $disk.Number -PartitionNumber $part.PartitionNumber).DriveLetter }
Format-Volume -Partition $part -FileSystem NTFS -NewFileSystemLabel 'VAULT' -Confirm:$false | Out-Null

"DRIVE=$drive"      # 交给 Go 解析
```

要点：
- 卷已有文件系统时不要重复 `Initialize-Disk` / `Format-Volume`。
- 刚挂载的磁盘可能不在缓存中，需要 `Update-HostStorageCache`。
- 建议输出结构化标记（如 `DRIVE=E`）由 Go 解析，避免依赖本地化文本。

### 3.4 步骤 C：拷贝 / 移动目录内容

```powershell
param([string]$Src, [string]$Dst, [ValidateSet('copy','move')][string]$Mode)

# /COPY:DAT 保留数据+属性+时间戳；/E 含空目录；/MT 多线程
$args = @($Src, $Dst, '/E', '/COPY:DAT', '/R:1', '/W:1', '/MT:8', '/NFL', '/NDL', '/NP')
& robocopy @args | Out-Null
$code = $LASTEXITCODE
# robocopy 约定：>= 8 才是真失败
if ($code -ge 8) { throw "robocopy failed, exit=$code" }

if ($Mode -eq 'move') {
    # 先校验再删源：校验文件数与总字节数一致
    $s = Get-ChildItem -LiteralPath $Src -Recurse -Force -File
    $d = Get-ChildItem -LiteralPath $Dst -Recurse -Force -File
    if ($s.Count -ne $d.Count -or ($s | Measure-Object Length -Sum).Sum -ne ($d | Measure-Object Length -Sum).Sum) {
        throw "verification failed, source kept"
    }
    Remove-Item -LiteralPath $Src -Recurse -Force
}
```

> **不要用 `robocopy /MOVE` 实现"剪切"**：`/MOVE` 是边拷边删，中途失败会导致源数据部分丢失。上表"先拷 → 校验 → 再删"才是可回退的安全语义。

### 3.5 步骤 D：分离

```powershell
Dismount-DiskImage -ImagePath $vhdx -Confirm:$false
```

分离前要确保 VHDX 内的文件没有句柄占用（否则报共享冲突）。建议：
- 不要对目标盘开着资源管理器/杀软实时扫描；
- 必要时 `Get-Process | Where-Object { $_.Path -like 'V:\*' }` 排查占用方。

### 3.6 Go 实现要点

```go
import "github.com/Microsoft/go-winio/vhd"

// 创建 VHDX（动态）。注意签名是 maxSizeInGb / blockSizeInMb
err := vhd.CreateVhdx(`D:\vault\vol01.vhdx`, 100, 2)

// 创建差异盘
err = vhd.CreateDiffVhd(`D:\vault\child.vhdx`, `D:\vault\parent.vhdx`, 2)

// 挂载 / 卸载 / 拿物理路径
handle, err := vhd.OpenVirtualDisk(`D:\vault\vol01.vhdx`,
        vhd.VirtualDiskAccessMaskAll, vhd.VirtualDiskFlagNone)
err = vhd.AttachVirtualDisk(handle, vhd.AttachVirtualDiskFlagNone, nil)
phys, err := vhd.GetVirtualDiskPhysicalPath(handle)   // \\?\PhysicalDriveN
err = vhd.DetachVirtualDisk(handle)
```

`go-winio/vhd` 已提供：`CreateVhdx`、`CreateDiffVhd`、`CreateVirtualDisk`、`OpenVirtualDisk(WithParameters)`、`AttachVhd`、`DetachVhd`、`AttachVirtualDisk`、`DetachVirtualDisk`、`GetVirtualDiskPhysicalPath`。

工程建议：
- 挂载 → `Update-HostStorageCache` → 用 `Get-Disk` 按 `Location` 匹配 VHDX 全路径取盘符；
- 拷贝/校验交给 `robocopy`，比 Go 自己递归拷贝更快且保留元数据；
- VHDX 路径、盘符都要做**路径合法性与白名单校验**（拼接裸路径会引入路径穿越风险）；
- 全流程加互斥锁（同一 VHDX 并发挂载/发布会互相破坏）。

---

## 4. 能力②：从已有 VHDX 创建差异盘

### 4.1 VirtDisk API

```c
CREATE_VIRTUAL_DISK_PARAMETERS p = {0};
p.Version = CREATE_VIRTUAL_DISK_VERSION_2;
p.Version2.UniqueId        = GUID_NULL;
p.Version2.MaximumSize     = 0;              // 差异盘必须为 0，容量继承父盘
p.Version2.ParentPath      = L"D:\\vault\\parent.vhdx";
p.Version2.SourcePath      = NULL;           // 差异盘禁止设置
p.Version2.BlockSizeInBytes= 0;              // 默认 2MB
p.Version2.SectorSizeInBytes = 512;
CreateVirtualDisk(&stVHDX, L"D:\\vault\\child.vhdx",
                  VIRTUAL_DISK_ACCESS_NONE /* Version2 时只能为 NONE */,
                  NULL, CREATE_VIRTUAL_DISK_FLAG_NONE, 0, &p, NULL, &h);
```

### 4.2 差异盘 + iSCSI 场景（推荐）

```powershell
# 父盘先纳入 iSCSI 虚拟盘登记
Import-IscsiVirtualDisk -Path 'D:\vault\parent.vhdx' -Description 'base'

# 创建差异虚拟盘（注意：-ParentPath 是差异盘专用参数集，与 -SizeBytes 互斥）
New-IscsiVirtualDisk -Path 'D:\vault\child01.vhdx' -ParentPath 'D:\vault\parent.vhdx'

# 映射给目标
Add-IscsiVirtualDiskTargetMapping -TargetName 'T1' -DevicePath 'D:\vault\child01.vhdx'
```

> **可用参数集（官方签名）**：`New-IscsiVirtualDisk` 有 4 个参数集——Dynamic（默认，`-Path -SizeBytes`）、Differencing（`-Path -ParentPath`）、Fixed（`-Path -SizeBytes -UseFixed`）、以及远程 `-ComputerName` 变体。差异盘**不能**同时指定 `-SizeBytes`。

### 4.3 约束与注意事项

| 约束 | 说明 |
| --- | --- |
| 父盘不可修改 | 一旦存在差异盘，父盘应视为只读；父盘被挂载写入会破坏子盘一致性 |
| 父盘不可移动/改名 | 差异盘按路径引用父盘，移动后 `CreateVirtualDisk`/挂载会失败（可用 `AddVirtualDiskParent` 修复自定义链，仅 Server 2012+） |
| 链深度 | 支持多层，但每层都放大 IO 与故障面，**建议单层** |
| 删除顺序 | 先 `Remove-IscsiVirtualDiskTargetMapping` → `Remove-IscsiVirtualDisk`（仅删登记对象）→ 再删 `.vhdx` 文件 |
| 权限 | 父盘与子盘都必须对 WinTarget 服务账户（默认 LocalSystem）可读写 |
| 空间 | 差异盘增长受宿主卷剩余空间限制，需监控 |

---

## 5. 能力③：从 VHDX 创建 iSCSI 目标 + 编辑授权

### 5.1 完整流程（PowerShell，官方推荐姿势）

```powershell
$vhdx   = 'D:\vault\vol01.vhdx'
$target = 'vault-vol01'

# 1) 把已有 VHDX 纳入 iSCSI 虚拟盘（New-* 只能建新盘；已存在的文件必须 Import-*）
Import-IscsiVirtualDisk -Path $vhdx -Description 'created by vault'

# 2) 创建 target，并授权初始 initiator
New-IscsiServerTarget -TargetName $target `
    -InitiatorIds @('IQN:iqn.1991-05.com.microsoft:client01.contoso.com')

# 3) 建立「虚拟盘 ↔ 目标」映射
Add-IscsiVirtualDiskTargetMapping -TargetName $target -DevicePath $vhdx
```

`InitiatorId` 格式为 `IdType:Value`，`IdType` 取值：`IQN` / `IPAddress` / `IPv6Address` / `DNSName` / `MACAddress`。

### 5.2 编辑授权

```powershell
# 覆盖式设置授权列表（注意：是「替换」而非「追加」！）
Set-IscsiServerTarget -TargetName $target -InitiatorIds @('IQN:...a', 'IPAddress:10.0.0.7')

# 清空全部授权（示例 1 的官方用法）
Set-IscsiServerTarget -TargetName $target -InitiatorIds @()

# 不带 IdType 前缀的简写（官方示例）
Set-IscsiServerTarget -TargetName 'Test' -InitiatorId @('IPAddress:10.10.1.1','IPAddress:10.10.1.2')
```

> **实现要点**：`IscsiTarget` 模块**没有** `Add-InitiatorId` / `Remove-InitiatorId` 这类增量 cmdlet，`-InitiatorIds` 是全量替换。所以"追加一个授权"必须先 `Get-IscsiServerTarget` 读出 `InitiatorIds`，合并后整体回写——**注意并发写会产生覆盖竞争，需加锁**。

`Set-IscsiServerTarget` 其他常用开关：

| 参数 | 作用 |
| --- | --- |
| `-Enable $true/$false` | 启用/停用目标（停用后 initiator 无法继续访问） |
| `-EnableChap -Chap user,pass` | 启用单向 CHAP |
| `-EnableReverseChap` | 启用反向 CHAP（需先 `-EnableChap`） |
| `-Description` | 备注 |
| `-MaxBurstLength` / `-FirstBurstLength` / `-MaxReceiveDataSegmentLength` | 性能调优 |
| `-EnforceIdleTimeoutDetection` | 空闲超时检测 |

### 5.3 底层 CIM/WMI 说明

- 命名空间：`root\Microsoft\Windows\Storage`
- iSCSI 目标服务器相关的 CIM 类（`IscsiTarg.mof`）：`MSFT_iSCSIServer`、`MSFT_iSCSITarget`、`MSFT_iSCSIVirtualDisk`、`MSFT_iSCSIDisk`、`MSFT_iSCSITargetPortal`、`MSFT_iSCSITargetPortalGroup`、`MSFT_iSCSIInitiatorId`、`MSFT_iSCSIMutualChap`。
- `IscsiTarget` PowerShell 模块就是这些类的封装。若要走纯 WMI 路线（不依赖 PowerShell），**先用现场命令确认方法名与签名**，不要照搬博客：

```powershell
Get-CimClass -Namespace root/Microsoft/Windows/Storage -ClassName MSFT_iSCSI* |
  Select-Object CimClassName, CimClassMethods
```

> 注意区分：`MSFT_iSCSISession` 属于**发起端（initiator）** 侧的 WMI 接口，只有 `Register` / `SetCHAPSecret` / `Unregister` 三个方法，**没有任何"断开/登出"能力**（见第 6 章）。

---

## 6. 能力④：从服务端断开已连接的 iSCSI 客户端 ⚠️

### 6.1 结论

**Windows Server 的 iSCSI Target Server 没有提供"按会话强制登出/踢掉指定 initiator"的公开 API。**

- `IscsiTarget` 模块**没有**任何 session 相关 cmdlet（无 `Get-IscsiServerSession`、无 `Disconnect-IscsiServerSession`）。
- 服务端 WMI/CIM 侧也没有会话枚举/登出方法。
- 发起端侧的 `MSFT_iSCSISession` 只有 `Register` / `Unregister`(取消持久化，**不是断开**) / `SetCHAPSecret`。

因此只能通过**改变目标/授权状态**来间接让客户端掉线，按下表取舍：

### 6.2 可用的间接手段

| # | 手段 | 命令 | 效果 | 影响面 |
| --- | --- | --- | --- | --- |
| 1 | **停用目标**（推荐） | `Set-IscsiServerTarget -TargetName T1 -Enable $false` | 目标不可用，已建立的连接会被断开；`-Enable $true` 恢复 | 仅该 target 的所有 initiator |
| 2 | 清空/改写授权 | `Set-IscsiServerTarget -TargetName T1 -InitiatorIds @()` | 取消授权。**对已建立会话是否立即生效，官方未说明，需实测** | 该 target |
| 3 | 解绑 LUN | `Remove-IscsiVirtualDiskTargetMapping -TargetName T1 -DevicePath x.vhdx` | 磁盘从 initiator 消失（对端通常表现为 surprise removal） | 单块盘 |
| 4 | 删除目标 | `Remove-IscsiServerTarget -TargetName T1` | 目标被删，initiator 无法访问，需重新创建 | 该 target，重 |
| 5 | 重启服务 | `Restart-Service WinTarget` | 该节点**全部** iSCSI 会话断开 | 全区，最重 |
| 6 | 客户端主动断开 | `Disconnect-IscsiTarget -NodeAddress 'iqn:...'` | 在 initiator 侧断开 | 需能远程下发指令 |

**建议实现**：Vault 暴露"踢下线"能力时，内部走 **手段 1（disable → 短暂等待 → enable）**，这是最贴近"断开客户端但保留配置"的做法；对需要彻底隔离的场景叠加手段 2。

> 方案 6 的坑（官方 Q&A 实证）：`Disconnect-IscsiTarget` 在会话上有设备在线时会失败并返回
> `HRESULT 0xefff0040`（"session cannot be logged off because a device on that session is currently being used"），
> 需要先把磁盘置为 Offline 再断开。

### 6.3 如何观测"当前有哪些客户端连着"

服务端**没有**会话枚举 API，可用以下替代方式：

```powershell
# a) TCP 层：谁在连 3260
netstat -ano | findstr ':3260'

# b) 事件日志（日志名以现场为准）
Get-WinEvent -ListLog *iSCSI* | Select-Object LogName, RecordCount
Get-WinEvent -LogName 'Microsoft-Windows-iSCSITarget-Server/Operational' -MaxEvents 50   # 需实测日志名

# c) 客户端侧（若可登录 initiator）
Get-IscsiSession | Select-Object TargetNodeAddress, InitiatorNodeAddress, IsConnected
```

因此，若 Vault 需要"实时在线客户端列表"，**只能靠 netstat/ETW 侧信道推导**，或用 `Set-IscsiServerTarget` 前后的连通性探测来判断，无法从 iSCSI 服务本身查到。这是该方案的一个已知能力缺口，需求侧需要接受。

---

## 7. 能力⑤：增删查能力汇总

### 7.1 iSCSI 虚拟盘（VHDX 的 iSCSI 登记对象）

| 操作 | cmdlet | 说明 |
| --- | --- | --- |
| 增（新建） | `New-IscsiVirtualDisk -Path -SizeBytes [-UseFixed]` | 只创建 **VHDX**；文件已存在会报错 |
| 增（新建差异） | `New-IscsiVirtualDisk -Path -ParentPath` | 与 `-SizeBytes` 互斥 |
| 增（纳管已有） | `Import-IscsiVirtualDisk -Path [-Description]` | 支持 **VHD 与 VHDX** |
| 删 | `Remove-IscsiVirtualDisk -Path` | **只删登记对象，不删 .vhdx 文件**，文件需另行 `Remove-Item` |
| 查 | `Get-IscsiVirtualDisk [-Path]` | 可查单块或全部 |
| 查（单独） | `Get-IscsiVirtualDisk -Path x.vhdx` | |
| 改 | `Set-IscsiVirtualDisk -Path [-Description]` | 描述等 |
| 扩缩容 | `Resize-IscsiVirtualDisk -Path -SizeBytes` | |
| 其他 | `Checkpoint-` / `Restore-` / `Mount-` / `Dismount-` / `Export-` + `*Snapshot` | 快照能力 |
| 转换 | `Convert-IscsiVirtualDisk` | 转 4K 扇区对齐 |
| 中止长任务 | `Stop-IscsiVirtualDiskOperation` | |

### 7.2 iSCSI 目标（target）

| 操作 | cmdlet |
| --- | --- |
| 增 | `New-IscsiServerTarget -TargetName [-InitiatorIds] [-ClusterGroupName]` |
| 删 | `Remove-IscsiServerTarget -TargetName` |
| 查 | `Get-IscsiServerTarget [-TargetName]` |
| 改（含授权） | `Set-IscsiServerTarget`（`-InitiatorIds` / `-Enable` / `-EnableChap` / `-Chap` / `-EnableReverseChap` / `-Description` …） |

### 7.3 虚拟盘 ↔ 目标映射

| 操作 | cmdlet |
| --- | --- |
| 增 | `Add-IscsiVirtualDiskTargetMapping -TargetName -DevicePath` |
| 删 | `Remove-IscsiVirtualDiskTargetMapping -TargetName -DevicePath` |
| 查 | **模块无独立查询 cmdlet**；从 `Get-IscsiServerTarget` / `Get-IscsiVirtualDisk` 的属性侧推，或走 CIM 查询 |

### 7.4 全局设置与配置迁移

| 操作 | cmdlet |
| --- | --- |
| 查全局 | `Get-IscsiTargetServerSetting` |
| 改全局 | `Set-IscsiTargetServerSetting` |
| 导出/导入配置 | `Export-IscsiTargetServerConfiguration` / `Import-IscsiTargetServerConfiguration` |

### 7.5 VHDX 本体（不含 iSCSI）

| 操作 | 方式 A（VirtDisk / Go） | 方式 B（PowerShell） |
| --- | --- | --- |
| 创建 | `CreateVirtualDisk` | `New-VHD`（需 Hyper-V）/ `New-IscsiVirtualDisk` |
| 打开 | `OpenVirtualDisk` | — |
| 挂载 | `AttachVirtualDisk` / `AttachVhd` | `Mount-DiskImage` |
| 卸载 | `DetachVirtualDisk` / `DetachVhd` | `Dismount-DiskImage` |
| 查询 | `GetVirtualDiskPhysicalPath` | `Get-DiskImage` / `Get-Disk`(Location) |
| 删除 | `os.Remove` | `Remove-Item`（先确认已分离） |
| 扩容 | `ResizeVirtualDisk` | `Resize-VHD`（Hyper-V）/ `Resize-IscsiVirtualDisk` |
| 分区格式化 | — | `Initialize-Disk` / `New-Partition` / `Format-Volume` |

---

## 8. Go 落地建议

```
internal/winvhd        // 封装 go-winio/vhd：Create / CreateDiff / Attach / Detach / PhysicalPath
internal/winps         // 通用 PowerShell 执行器：提权、-NoProfile -NonInteractive、
                       //  输出 JSON 或 KEY=VALUE 标记、错误码归一化
internal/iscsitarget   // 封装 IscsiTarget 模块：target / virtualdisk / mapping / initiatorIds
internal/volume        // 分区 + 格式化 + 盘符分配（Storage 模块）
internal/service       // 编排：① 打包目录→VHDX ② 差异盘 ③ 发布 iSCSI ④ 踢下线 ⑤ CRUD
```

实践要点：

1. **PowerShell 调用规范**
   ```powershell
   powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "<script>"
   ```
   统一用 `ConvertTo-Json -Compress -Depth 5` 输出，Go 侧 `json.Unmarshal`；错误用 `-ErrorAction Stop` + `try/catch` 后输出 `{"error":...}` 并返回非 0 退出码。
2. **提权**：VirtDisk 挂载、iSCSI 管理都要求管理员。若 Vault 以服务方式运行，确认服务账户在 `Administrators` 组，或用计划任务以最高权限执行。
3. **幂等性**：所有"创建"操作先查后建（`Import-IscsiVirtualDisk` 对已存在文件会失败；`New-Partition`/`Initialize-Disk` 对已初始化磁盘会失败）。
4. **并发**：同一 VHDX 的「挂载 / 发布 / 删除」必须串行化（进程内互斥 + 文件锁）。
5. **不要在服务端挂载已发布的 VHDX**：只能 `Import` 之前挂载拷贝数据，发布前必须 `Dismount-DiskImage`。
6. **路径校验**：VHDX 路径只允许白名单根目录下的规范化路径，拒绝 `..`、UNC 与符号链接，防止路径穿越。
7. **失败清理**：创建 VHDX 后若后续步骤失败，回滚顺序为 `Dismount-DiskImage` → 删除 `.vhdx`（若已 `Import`，先 `Remove-IscsiVirtualDisk`）。

---

## 9. 附录

### 9.1 常见错误码

| 错误 | 常见原因 |
| --- | --- |
| `ERROR_INVALID_PARAMETER`(87) | `Version2` 时 access mask 非 `VIRTUAL_DISK_ACCESS_NONE`；`ParentPath`+`SourcePath` 同时给；`MaximumSize` 非 512 倍数或 <3MB；`BlockSizeInBytes` 非法 |
| `ERROR_ACCESS_DENIED`(5) | 未提权 / VHDX 文件 ACL 不允许 |
| `ERROR_SHARING_VIOLATION` | VHDX 已被挂载或正被 iSCSI 使用；文件被占用无法分离/删除 |
| `0xefff0040` | `Disconnect-IscsiTarget` 时会话上有在线设备，需先 Offline 磁盘 |
| robocopy exit ≥ 8 | 拷贝真失败（1~7 为可接受状态） |

### 9.2 需在目标环境实测确认的清单

- [ ] `diskpart create vdisk file=x.vhdx` 是否直接产出 VHDX（官方文档未说明）。
- [ ] `Set-IscsiServerTarget -Enable $false` 是否**立即**断开已建立的会话（本文据此设计"踢下线"，需验证时延）。
- [ ] `Set-IscsiServerTarget -InitiatorIds` 缩小授权后，已建立的越权会话是否被立即终止。
- [ ] 服务端 iSCSI 事件日志的确切 LogName。
- [ ] `New-IscsiVirtualDisk -ParentPath` 是否要求父盘已 `Import-IscsiVirtualDisk` 登记。
- [ ] 目标 OS 上 `root\Microsoft\Windows\Storage` 下 `MSFT_iSCSI*` 类的实际方法名（用 `Get-CimClass` 输出）。

### 9.3 参考链接

- [CreateVirtualDisk 函数 (virtdisk.h)](https://learn.microsoft.com/zh-cn/windows/win32/api/virtdisk/nf-virtdisk-createvirtualdisk)
- [CREATE_VIRTUAL_DISK_PARAMETERS 结构 (virtdisk.h)](https://learn.microsoft.com/en-us/windows/win32/api/virtdisk/ns-virtdisk-create_virtual_disk_parameters)
- [AddVirtualDiskParent 函数](https://learn.microsoft.com/en-ie/windows/win32/api/virtdisk/nf-virtdisk-addvirtualdiskparent)
- [Virtual Disk API in Windows（MSDN Magazine，VHD 背景与用法）](https://learn.microsoft.com/en-us/archive/msdn-magazine/2009/april/windows-with-c-the-virtual-disk-api-in-windows-7)
- [IscsiTarget 模块 cmdlet 列表](https://learn.microsoft.com/en-us/powershell/module/iscsitarget/?view=windowsserver2025-ps)
- [New-IscsiServerTarget](https://learn.microsoft.com/en-us/powershell/module/iscsitarget/new-iscsiservertarget?view=windowsserver2025-ps)
- [Set-IscsiServerTarget](https://learn.microsoft.com/en-us/powershell/module/iscsitarget/set-iscsiservertarget?view=windowsserver2025-ps)
- [New-IscsiVirtualDisk](https://learn.microsoft.com/en-us/powershell/module/iscsitarget/new-iscsivirtualdisk?view=windowsserver2025-ps)
- [Import-IscsiVirtualDisk](https://learn.microsoft.com/en-us/powershell/module/iscsitarget/import-iscsivirtualdisk?view=windowsserver2025-ps)
- [Remove-IscsiServerTarget](https://learn.microsoft.com/en-us/powershell/module/iscsitarget/remove-iscsiservertarget?view=windowsserver2025-ps)
- [Mount-DiskImage (Storage)](https://learn.microsoft.com/en-us/powershell/module/storage/mount-diskimage)
- [MSFT_iSCSISession class（发起端 WMI，仅 Register/Unregister/SetCHAPSecret）](https://learn.microsoft.com/en-gb/previous-versions/windows/desktop/iscsidisc/msft-iscsisession)
- [Disconnect-IscsiTarget](https://learn.microsoft.com/en-us/powershell/module/iscsi/disconnect-iscsitarget?view=windowsserver2025-ps)
- [实现 Windows Server iSCSI（官方培训）](https://learn.microsoft.com/en-us/training/modules/implement-windows-server-iscsi/4-implement-internet-small-computer-systems-interface)
- [go-winio/vhd Go 包文档](https://pkg.go.dev/github.com/Microsoft/go-winio/vhd)
- [Create vdisk（diskpart）](https://learn.microsoft.com/en-us/previous-versions/windows/it-pro/windows-server-2012-r2-and-2012/gg252579(v=ws.11))
