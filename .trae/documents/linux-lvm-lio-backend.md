# 抽象 iSCSI / 虚拟磁盘后端，新增 Linux（LVM thin + LIO）实现

## Context

当前服务端**只能跑在 Windows Server**：`internal/platform/` 下的 `winvhd`（go-winio/VirtDisk）、`iscsitarget`（IscsiTarget 模块）、`volume`（Storage 模块）、`winps`（PowerShell 执行器）是**具体类型**，被 `app.Deps` 直接持有（[app.go](file:///c:/Users/89412/Projects/golang/vault/internal/app/app.go#L27-L49)），上层 [disk.go](file:///c:/Users/89412/Projects/golang/vault/internal/app/disk.go#L245-L312) / [iscsi.go](file:///c:/Users/89412/Projects/golang/vault/internal/app/iscsi.go#L511-L560) 直接调用，`cmd/vault-server/main.go` 里硬编码装配。文档 [§6.2](file:///c:/Users/89412/Projects/golang/vault/docs/implementation.md#L1317) 早就写了「换平台只需替换 platform/」，本任务把它做实。

目标：把**虚拟磁盘**与 **iSCSI** 两块抽象成可替换后端，并落地 Linux 端实现——虚拟磁盘用 **LVM thin LV / thin snapshot**，iSCSI 用 **LIO（configfs 直控）**，并支持用 **SSD 做 dm-cache 加速 HDD 上的 thin pool**（用户明确诉求）。Windows 实现保留、行为不变。

### 已确认决策
| 项 | 决策 |
| --- | --- |
| iSCSI 后端 | **LIO**（configfs 纯 Go 直控，零 cgo，性能最好，可真实枚举会话） |
| 平台范围 | **Windows 与 Linux 并列共存**，用构建 tag 分平台编译 |
| 配额隔离 | 用户诉求是 SSD 缓存加速 HDD，故**单共享 thin pool**；隔离靠应用层精算 + pool 水位闸门 |
| 缓存 | **不自己实现**：建池时选定 SSD 块设备，直接用 LVM 原生 `dm-cache`；本系统只做**块设备枚举 + 选择器 + 状态展示** |
| cgo | 保持**零 cgo**（configfs 是文件读写，LVM 走 CLI），符合 [§10.4](file:///c:/Users/89412/Projects/golang/vault/docs/implementation.md#L2114) 决策 |

### 调研已确认的硬约束（必须写进实现，否则会返工）
1. **thin snapshot 无法限制单个快照的物理增长**（RHEL 原文），单盘写爆会拖垮整个 pool → 必须有水位闸门；
2. 建快照**绝不能带 `-L/--size`**（会退化成旧式 COW 快照，立即吃满空间）；
3. thin snapshot 默认 **skip-activation（`lv_attr` 第 10 位 `k`）**，激活必须 `lvchange -ay -K`；
4. `lvremove` 前必须先下线 LUN / `lvchange -an`（LV 被 iblock 打开时删除会被拒），且 thin LV **无法用 `vgcfgrestore` 恢复**；
5. `issue_discards` 默认 `0`，需显式开启，否则删除后 pool 占用不下降；
6. 配额统计**不能用 `lvs -o data_percent` 直接相加**（共享块重复计数）→ 优先 `thin_ls`；
7. 只读 LUN 是 **per-ACL 的 `mapped_lun write_protect`**，且必须先 `auto_add_mapped_luns=false`；
8. UNMAP 回收要在 TPG `attrib` 开 **`emulate_tpu=1`**（+ `is_nonrot`），否则 Windows 不会发 UNMAP、thin pool 不回收；
9. **LIO 没有服务端强制登出**（`targetcli sessions` 只能 list/detail）→ 「踢下线」沿用现有 `DisableTarget` + 客户端心跳的降级语义（现有 lease 逻辑可直接复用）；会话**枚举**则升级为真实可用；
10. **禁用 `target.service`**（configfs 重启即失，由 DB 重放重建，避免双份真源抢跑）；
11. **缓存对象是 thin pool 的 `_tdata`，不是 pool 名**：挂载用 `lvconvert --type cache --cachepool vg/cpool vg/tpool`，**改 cachemode 必须针对 `vg/tpool_tdata`**（对 pool 名操作会报错/无效）；
12. cache pool 与 origin **必须同 VG**，且 LVM **不支持 LVM-on-LVM** → SSD 与 HDD 必须放进**同一个 VG**；
13. **cache pool 不支持 autoextend**（只有 thin_pool / snapshot 有）→ 缓存扩容只能手动，规划时必须留余量；
14. `writeback` 下 SSD 掉盘 = 未回写数据丢失/整卷不可用；**掉电后所有 cache 块被当作脏块**，可能触发长时间回写 → 默认必须 `writethrough`；
15. cache 元数据异常（`Fail`/`needs_check`）需 `cache_repair` → `cache_check` 修复（工具同属 `thin-provisioning-tools`）；
16. cache pool 与 origin **逻辑块大小必须一致**（cache pool 默认 4096B，与 512B 混用会导致挂载失败）。

### SSD 缓存（dm-cache / lvmcache）调研结论
- **dm-cache vs dm-writecache**：thin pool 加速用 **dm-cache**（读+写热点）；dm-writecache 只缓存新写、不回缓读，不选。
- **推荐参数**：`--type cache-pool -l 100%FREE --chunksize 256K --cachemode writethrough --cachepolicy smq`；cache 容量 = 工作集 × 1.15；`cache_pool_max_chunks ≤ 1_000_000`（cachemode：**LVM 默认 writethrough**，但**内核 dm-cache 默认是 writeback**，两者不同，必须显式写死）。
- **监控字段**：`lvs -a -o +cache_mode,chunksize,cache_total_blocks,cache_used_blocks,cache_dirty_blocks,cache_read_hits,cache_read_misses,cache_write_hits,cache_write_misses`（解析优先 `lvs --reportformat json`）；`dmsetup status <vg-lv>` 可取 `cache metadata mode(ro/rw/Fail)` 与 `needs_check`。
- **拆除流程**：安全顺序 = 先 `lvchange --cachemode writethrough vg/tpool_tdata` → 等 `cache_dirty_blocks=0` → `lvconvert --uncache vg/tpool`；只想断开保留 cache pool 用 `--splitcache`。**`writeback` 下直接 `--uncache` 会全量同步且丢数据风险**。
- **冗余取舍**：`writethrough` 掉 SSD 只掉性能，可不做冗余；若开 `writeback`，**强烈建议对 cache meta 做 raid1**（`--cachedevice` 可重复指定多盘）。
- **discard**：dm-cache 默认透传 discard，但**建议关闭 inline discard**，改用 `fstrim.timer` 周期 Trim，避免 SSD cache 元数据频繁抖动。

| 现象 | 原因 | 规避 |
| --- | --- | --- |
| 给 pool 改 cachemode 报错/无效 | 目标是 `_tdata` 而非 pool 名 | `lvchange --cachemode X vg/POOL_tdata` |
| SSD 掉线后整卷不可读 | writeback 脏块未回写 | 生产用 writethrough；或 cache 做 raid1 |
| 掉电后长时间卡顿、IO 飙高 | 重启后全部 cache 块视为脏、全量回写 | 预留回写窗口；监控 `cache_dirty_blocks` |
| 挂载失败 / 块大小错误 | origin 512B 与 cache pool 4096B 不一致 | 建池时显式统一 `--chunksize` / 块大小 |
| `--uncache` 卡很久、性能骤降 | writeback 需全量同步 | 先切 writethrough、等脏块为 0 再拆 |
| 想扩 cache pool 发现无自动扩容 | dm-cache 不支持 autoextend | 手动扩容，规划留余量 |
| 跨 VG 挂 cache 失败 | 必须同 VG | SSD 与 HDD 放同一 VG |
| SSD 写放大严重 | inline discard 频繁触发 | 关 inline discard，用 `fstrim.timer` |

---

## 实施步骤

### 1. 新增 platform 层接口（无 OS 代码）
新文件 `internal/platform/backend.go`（+ `types.go`）：
- `DiskBackend`：`Kind/Available/Exists/Create/CreateDiff/Clone/Delete/PhysicalSize/Fingerprint/Optimize/Activate/Deactivate`
- `VolumeBackend`：`EnsureFormatted/ MountAndCopy`（挂载+拷入+卸载一体，对应现 [buildVHDXFromSource](file:///c:/Users/89412/Projects/golang/vault/internal/app/disk.go#L245-L312) 的「挂载→格式化→拷入→卸载」）/ `SpaceUsageOf/FileSystemOf`
- `IscsiBackend`：`Available/EnsureTarget(TargetSpec)/RemoveTarget/AttachLun(readOnly)/DetachLun/GetTarget/ListTargets/ListSessions/ForceLogout`
- 值对象：`Device`（Windows=盘符/PhysicalDrive，Linux=/dev/mapper/...）、`TargetSpec`（含 ACL 全量列表 + CHAP 单/双向 + enabled + readonly）、`Session`、`ErrUnsupported`
- 保留 `iscsitarget.SetTargetOptions` 语义（**ACL 全量替换**）以免改变上层并发模型

### 2. Windows 实现适配（**不改行为**）
- `winvhd.Manager` / `volume.Manager` / `iscsitarget.Manager` 增加满足接口的方法（薄适配，Windows-only 文件）；
- `ListSessions/ForceLogout` 返回 `platform.ErrUnsupported`；
- 现有 PowerShell 脚本、robocopy、句柄表逻辑**一律不动**。

### 3. Linux 实现（新）
`internal/platform/linuxlvm/`
- `exec.go`：LVM CLI 执行器（`lvcreate/lvremove/lvchange/lvconvert/lvs/vgs/thin_ls/dmsetup`），结构化参数、不拼字符串，复用 `winps.Runner` 的超时/日志风格；
- `thin.go`：thin pool/LV 生命周期。Create=`lvcreate --type thin`、CreateDiff=`lvcreate --snapshot`（**不带 size**）、Activate=`lvchange -ay -K`、Delete=先 `lvchange -an` 再 `lvremove`、PhysicalSize=`thin_ls`（退化 `lvs data_percent`）、Optimize=`thin_trim`/`blkdiscard`/`fstrim` 组合、Fingerprint=读首 1MiB SHA-256（与现有语义一致）；
- `cache.go`：把**建池时选定的块设备**接成 dm-cache，**不实现任何自定义缓存逻辑**，全部用 LVM 原生能力；操作对象一律是 thin pool 的 **`_tdata`**：
  - `EnsureCachePool(ssdDevs, size, chunk, policy)`：`lvcreate --type cache-pool -n <cpool> -l 100%FREE --chunksize 256K --cachepolicy smq <vg> <ssd_pv...>`（SSD 先 `pvcreate` 后加入**同一个 VG**，LVM 不支持 LVM-on-LVM）；
  - `AttachCache`：`lvconvert --type cache --cachepool <vg>/<cpool> <vg>/<tpool>`（**传 thin pool 名**，LVM 自动落到 `_tdata`）；
  - `SetCacheMode`：`lvchange --cachemode <mode> <vg>/<tpool>_tdata`（**改模式必须针对 `_tdata`**；`writethrough` 默认，`writeback` 需显式确认并告警）；
  - `CacheStatus`（只读）：`lvs --reportformat json -a -o cache_mode,chunksize,cache_total_blocks,cache_used_blocks,cache_dirty_blocks,cache_read_hits,cache_read_misses,cache_write_hits,cache_write_misses`，辅以 `dmsetup status` 读 `Fail`/`needs_check`；
  - 建池时校验：SSD 与 HDD **同 VG**、设备未被占用、块大小与 origin 一致（否则挂载失败）；
  - **不做**在线拆缓存/换盘（用户只要"选择器"），文档给出人工步骤：`lvchange --cachemode writethrough <vg>/<tpool>_tdata` → 等 `cache_dirty_blocks=0` → `lvconvert --uncache <vg>/<tpool>`。
- `blockdev.go`：**块设备枚举**（供"缓存设备选择器"用）。`lsblk --json -b -o NAME,PATH,TYPE,SIZE,ROTA,MODEL,MOUNTPOINTS,FSTYPE`，逐条标注：
  - `system=true`：挂载点含 `/`、`/boot`、`/boot/efi`、`[SWAP]`，或该盘上存在承载根文件系统的 PV → **前端默认过滤且禁止选择**；
  - `has_pv=true` / `has_fs=true`：已被占用（可作为"显示但禁用"）；
  - `rotational`：`ROTA=1` 为 HDD、`0` 为 SSD/NVMe（选择器默认只列 `ROTA=0` 且 `system=false`）；
  - 另给 `vg` / `pv_pe_count` 便于判断"能否加入目标 VG"。
- `volume_linux.go`：`mkfs.ntfs -Q` + `mount -t ntfs-3g` + 递归拷入 + 校验 + `umount` + `ntfsfix -d`；空间统计改从 `vgs -o vg_free`/`lvs -o data_percent,metadata_percent` 取。

`internal/platform/liotarget/`
- `configfs.go`：`mkdir/write/read/rmdir/symlink` 封装（含 `os.Remove` 对 configfs rmdir 的处理）；
- `target.go`：EnsureTarget（iqn 全小写；TPG attrib 设 `authentication/generate_node_acls=0/auto_add_mapped_luns=0/emulate_tpu=1/is_nonrot=1`；ACL 建 iqn 目录 + `userid/password` + `mapped_lun write_protect`）、RemoveTarget、GetTarget/ListTargets（读 configfs）；backstore 用 **`core/iblock_0`**（指 `/dev/mapper/<vg>-<lv>`）；
- `session.go`：`ListSessions` 读 `.../tpgt_1/sessions/<sid>/info`；`ForceLogout` 用 `tpgt enable=0` + 删 ACL 的降级实现（并返回"非精确登出"标记供上层展示）。

### 4. app 层去 Windows 化
- `app.Deps` 的 `PS/VHD/Vol/Iscsi` → `Disk platform.DiskBackend` / `Vol platform.VolumeBackend` / `Iscsi platform.IscsiBackend`（`PS` 移除，PowerShell 只在 win 后端内部使用）；
- `app/support.go`：删 `ProbeHyperV`（改由平台构造时内部探测）与 `setTargetEnabled`（改用 `platform.TargetSpec`）；
- `app/system.go`：`Capabilities` 保持既有字段兼容，新增平台中性字段（`platform_kind`、`lvm`、`lvm_cache`、`iscsi_sessions`），`detectHyperVModule` 移入 win 后端；
- `app/reconcile.go` / `lease.go`：`ForceLogout` 可用时优先真踢，不可用时走现有 `DisableTarget`（**保持现有降级路径**）。

### 5. LVM 池初始化与「缓存设备选择器」（Linux 专属，按你的澄清实现）
你的原话：「用 thin lv 的能力，我在创建池的时候能指定块设备作为缓存，然后我们系统只提供设置的选择器，这里要能列出块设备让我选，要排除系统盘等」「不要自己实现，用系统的能力」→ 因此：**缓存完全由 LVM/dm-cache 原生承担**，本系统只做「枚举块设备 + 在建池时选择 + 展示状态」三件事。

- **后端**（新 `internal/app/platform.go`，`PlatformService` 挂到 `app.App`）：
  - `ListBlockDevices(ctx)` → `linuxlvm.Manager.ListBlockDevices`；Windows 后端返回 `platform.ErrUnsupported`（前端据此隐藏入口）；
  - `LvmStatus(ctx)`（只读）→ VG / thin pool / 水位 / cache 状态；
  - `InitializePool(ctx, in)`（**一次性**，幂等）→ 按需 `pvcreate`(HDD) → `vgcreate` → `lvcreate --type thin-pool --chunksize <c> --poolmetadatasize <m>` → 若选了缓存设备：`pvcreate`(SSD) → `vgextend` 同一 VG → `EnsureCachePool` → `AttachCache` →（可选）`SetCacheMode`；
  - 目标 VG/Pool 已存在时跳过创建并返回现状（避免覆盖已有数据）；写操作按 VG 用 `lock.Keyed` 串行化。
- **HTTP**（新 `internal/api/platform.go`，注册在 [router.go](file:///c:/Users/89412/Projects/golang/vault/internal/api/router.go#L95-L115) 的 `requireSuperAdminMW` 组内）：
  | 方法 | 路径 | 说明 |
  | --- | --- | --- |
  | GET | `/v1/system/block-devices` | 块设备候选（含 `system` / `has_fs` / `has_pv` / `rotational` / `size` / `model` / `path` / `vg`） |
  | GET | `/v1/system/lvm` | VG / Pool / 水位 / cache 现状（只读） |
  | POST | `/v1/system/lvm/initialize` | 建池：`{ hdd_devices[], vg, thin_pool, chunk_size, metadata_size, cache:{ devices[], chunk_size?, policy?, mode? } }` |
- **管理端 UI**（`frontend/packages/ui/src/features/system/`，接入现有 [SystemSettings.tsx](file:///c:/Users/89412/Projects/golang/vault/frontend/packages/ui/src/features/system/SystemSettings.tsx) 所在页面）：
  - `LvmPoolCard.tsx`：VG/Pool/水位/缓存状态只读展示 + 「初始化」按钮；
  - `BlockDevicePicker.tsx`（**你要的选择器**）：默认**不显示** `system=true` 的设备（提供「显示系统盘」开关，但系统盘恒为不可选并标注原因），默认只列 `rotational=false`（SSD/NVMe）并可切换显示 HDD，已占用（`has_fs`/`has_pv`）显示为禁用 + 原因；
  - 选 `writeback` 时弹二次确认（文案：SSD 故障可能丢失未回写数据）；
  - 新增文案需同步 `i18n/zh-CN.json` / `en-US.json` / `ja-JP.json` / `ko-KR.json` 四份。
- **不做**：缓存的在线拆除 / 换盘 / 修复界面（你的要求只是"建池时选择器"）。文档给出人工步骤：`lvchange --cachemode writethrough <vg>/<tpool>_tdata` → 等 `cache_dirty_blocks=0` → `lvconvert --uncache <vg>/<tpool>`。

### 6. cmd 与 lock 的平台拆分
- 新 `internal/lock/single_other.go`（`flock` 文件锁，对应 [single_windows.go](file:///c:/Users/89412/Projects/golang/vault/internal/lock/single_windows.go)）；
- 新 `cmd/vault-server/launcher_other.go`（`ownsOwnConsole()=false`、`runLauncher` 提示用 systemd、`pauseForUser` 空实现）；
- 新 `cmd/vault-server/stop_other.go`（PID 文件 + `SIGTERM`，对应 [stop_windows.go](file:///c:/Users/89412/Projects/golang/vault/cmd/vault-server/stop_windows.go)）；
- 新 `cmd/vault-server/platform_windows.go` / `platform_linux.go`：各自装配平台后端，main 只调 `buildPlatform(...)`；
- `main.go` 去掉 `winvhd/winps/volume/iscsitarget` import 与「仅支持 Windows」注释。

### 7. 配置
`config.Config` 新增 `Platform PlatformConfig`（`yaml:"platform"`），默认值在 `applyDefaults` 中补齐；`config.example.yaml` 同步：
```yaml
platform:
  kind: auto            # auto|windows|linux，auto 按运行 OS
  lvm:
    vg: vg0            # 目标 VG 名（由 initialize 流程创建）
    thin_pool: vault   # 目标 thin pool 名
    chunk_size: 256K
    metadata_size: 4G
    autoextend_threshold: 80
    autoextend_percent: 20
    watermark_percent: 90   # 应用层空间闸门（见步骤 9）
  iscsi:
    backend: lio
    configfs_root: /sys/kernel/config/target
    portals: ["0.0.0.0:3260"]
    iqn_prefix: iqn.2026-01.com.vault
```
> **缓存设备不进配置文件**：它在建池时由选择器选定，之后由 LVM 自己持久持有（dm-cache 元数据在 `_cmeta`），本系统只读不存——符合你说的「不要自己实现，用系统的能力」。`vg`/`thin_pool` 只用于"瞄准哪个池"，池不存在时引导去初始化。

### 8. 兼容性与放置模型（**不改 DB schema**）
- 单机不做 Win/Linux 混用，后端种类由**配置**判定，因此**不新增列、不做迁移**：
  - `disks.vhdx_path` 在 Linux 上存后端引用 `/dev/mapper/<vg>-<lv>`（保持"全局唯一 locator"语义）；
  - `storages.path` 在 Linux 上存 `<vg>/<thin_pool>`（一个 pool 一条记录，首次启动自动种子）。
  - `domain.Disk.VHDXPath` 本轮不改名（避免大面积改动），仅在文档标注其语义为"后端不透明引用"。

### 9. 配额与空间闸门（本轮实现）
- **水位闸门**：`data_percent` / `metadata_percent` 超 `platform.lvm.watermark_percent` 即拒绝 Create/CreateDiff/分配（返回既有 `platform.insufficient_space` / `disk.*` 错误码）；`metadata_percent` 单独更早告警（元数据 100% 比数据 100% 更致命）；
- **统计口径**：物理占用用 `thin_ls`（**不能**用 `lvs -o data_percent` 直接相加，共享块会重复计数）；退化时用 `dmsetup status` 的 data 字段；
- **自动扩容**：读 `autoextend_*` 做**校验与告警**（提示宿主机 `lvm.conf` 的 `thin_pool_autoextend_threshold` 应为多少、`lvm2-monitor`/`dm-event` 是否在跑），**不自行改宿主机 `lvm.conf`**；VG 无富余 PV 时明确告警"无法自动扩容"。

### 10. 不做（明确划界）
- 不做 per-库独立 thin pool（已选单共享 pool + SSD 缓存）；
- 不实现任何自定义缓存层（一切走 LVM/dm-cache 原生能力）；
- 不做缓存在线拆除/换盘/修复（只做"建池时选择器"，人工步骤写进文档）；
- 不做目标侧 `saveconfig` 持久化（DB 重放；实现里检测 `target.service` 处于 enabled 时告警）；
- 不引入 cgo、不引入新的外部中间件。

---

## 关键文件

**新增**：`internal/platform/backend.go`、`internal/platform/linuxlvm/{exec,thin,cache,blockdev,volume_linux}.go`、`internal/platform/liotarget/{configfs,target,session}.go`、`internal/app/platform.go`、`internal/api/platform.go`、`internal/lock/single_other.go`、`cmd/vault-server/{launcher_other,stop_other,platform_windows,platform_linux}.go`、`frontend/packages/ui/src/features/system/{LvmPoolCard,BlockDevicePicker}.tsx`

**修改**：`internal/app/{app,disk,iscsi,system,support,reconcile,lease}.go`、`internal/api/router.go`、`cmd/vault-server/main.go`、`internal/config/config.go`、`config.example.yaml`、`internal/platform/winvhd/*`（适配层）、`internal/platform/volume/*`、`internal/platform/iscsitarget/*`、`frontend/packages/ui/src/i18n/*.json`、`frontend/packages/ui/src/index.ts`、`docs/implementation.md`、`docs/design.md`

**复用**：`winps.Runner` 的执行/超时/日志风格、`lock.Keyed`（configfs 与 LVM 写操作串行化）、`domain.PathGuardSet`（`storage.source_roots` 仍适用）、`apperr` 错误码体系、`secret.Cipher`（CHAP 密钥托管）、`api` 层 `requireSuperAdminMW` 与 `toStorageDTO` 式 DTO 模式。

---

## 验证

1. **交叉编译**（必须两种都过，且 `CGO_ENABLED=0`）：
   `$env:CGO_ENABLED=0; go build ./...`、`$env:GOOS='linux'; $env:GOARCH='amd64'; go build ./...`
2. `go vet ./...`，确保 `app` 包不再 import 任何 `platform/win*`。
3. `pnpm -r typecheck`（UI 有新增页面与 i18n 键，必须过）。
4. **真机验证由你自行执行**（按 `implementation.md` §14.1 扩展后的清单）：初始化建池（含选 SSD 作缓存）/ 创建 / 派生 / 删除 / 只读临时共享 / UNMAP 回收 / 会话枚举 / 缓存命中率与水位闸门。
5. 文档：`implementation.md` §2 能力边界表改为「Windows / Linux」双列；§5 增补「Linux（LVM thin + LIO）」小节；§14.1 补 Linux 待实测项；`design.md` 补「平台抽象」与「LVM 池初始化 / 缓存设备选择」两节。