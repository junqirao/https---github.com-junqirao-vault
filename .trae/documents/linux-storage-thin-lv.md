# Linux 存储 = LVM thin LV：实施计划

## Context（为什么做这次改动）

上一轮完成了「平台后端抽象」（Windows = VHDX + WinTarget，Linux = LVM thin + LIO），但对 `storages`（存储）保留了 Windows 的目录语义：Linux 上仍要求管理员**填一个已存在的本地目录**，只用于 staging / trash / 孤儿扫描。

问题：Linux 上虚拟磁盘本身就是 LV、没有文件，存储作为「磁盘的放置位置 + 容量边界」的语义因此塌了一半——管理员在存储页做的事（填目录）与系统实际做的事（从 thin pool 分配块）完全脱节。

目标：Linux 下「创建存储」= **系统创建 thin LV → 格式化 → 挂载为目录**，把「存储」升级为该 thin pool 里一个真实可分配、可计量、可卸载的单元；Windows 行为完全不变。

## 已确认决策（用户）

1. **存储 = thin LV，格式化后挂载为目录**；其文件系统承载 staging / trash / 孤儿扫描，其虚拟大小即该存储的容量配额。
2. **挂载点：默认自动生成 + 高级可改**。
3. **保留「登记已有目录 / 已有 LV」为高级逃生入口**。

### 结构性前提（不可绕过）

LV 里不能再建 LV。因此**存储的 LV** 与**磁盘的 LV** 只能是同一个 thin pool 下的两个平行 LV，磁盘不落在存储 LV 的文件系统内。存储 LV 只承担「暂存 + 回收站 + 容量配额」，磁盘位置仍由 pool 直接分配。

## 关键取舍（含对初始设计的修正）

| # | 取舍 | 理由 |
| --- | --- | --- |
| 1 | **新增表 `storage_volumes`，不改 `storages` 列** | 现有迁移机制是「整套 `CREATE TABLE IF NOT EXISTS` + 版本号」([migrate.go](file:///c:/Users/89412/Projects/golang/vault/internal/store/migrate.go#L18-L26))，**无法给已有表加列**；新表是唯一符合既有约定且对老库幂等的方案。版本 → 3 |
| 2 | 表内**不存 mount_point**（用 `storages.path`） | `path` 已 UNIQUE 且所有目录类操作都从它派生，双写必然漂移 |
| 3 | `kind` 取 `thin`/`lv`/`dir` + `managed` 标志 | 「登记已有 LV」绝不能被 `lvremove`——`managed=false` 是删除安全性的显式判据（不能用运行时推导代替） |
| 4 | 加 `state(pending/ready)` | `lvcreate → mkfs → mount → 写库` 中途崩溃会留下无主 LV，需可识别、可补偿 |
| 5 | **挂载持久化 = 服务端启动时幂等挂载**（不写 `/etc/fstab`、不生成 systemd unit） | 避免改宿主机 `/etc`；服务端启动前无人读这些目录。**但必须 fail-closed**：挂载后核对 `/proc/self/mountinfo`（设备号 + 挂载点），失败则标 degraded 并**从落盘 guard 集合中剔除**，否则写入会静默落在宿主根 FS 上、日后挂载把数据「藏」起来。另加定期 reconcile（应对手工 `umount`） |
| 6 | `Enabled=false` **不等于**卸载 | 否则已创建的磁盘与扫描范围全部读空。停用只影响「新盘放置」；卸载是独立行操作 |
| 7 | Linux `SpaceUsageOf` 改为 **`min(statfs(path), VG free)`**，`Name` = 承载设备、`FileSystem` = statfs 实际值 | thin LV 不自增长，用满虚拟大小即 ENOSPC，故 `TotalBytes` = LV 大小、`FreeBytes` 必须取两者较小值，否则 [PathGuardSet.Pick](file:///c:/Users/89412/Projects/golang/vault/internal/domain/pathset.go#L147-L180) 会选到装不下的根；`Name` 若继续回退成 VG，[pathset.go](file:///c:/Users/89412/Projects/golang/vault/internal/domain/pathset.go#L186-L205) 会按 Name 去重把多个存储塌成一条 |
| 8 | 存储 LV 用 **ext4（默认）/ xfs**，**不用 NTFS** | NTFS 只用于对外发布的虚拟磁盘（[linuxlvm/volume.go](file:///c:/Users/89412/Projects/golang/vault/internal/platform/linuxlvm/volume.go#L22-L23) 的 `EnsureFormatted` 是**磁盘专用**且只接受 NTFS，不可复用为存储卷） |
| 9 | LV 名复用 `DiskRef("", "storages/"+storageID)` | 复用既有 `mapLVName` 折叠规则（`-` → `_`，满足 `lvNameRe`），改名/改挂载点不影响，无需新命名逻辑 |

### 已核实的「非风险」

- **跨设备移动不存在**：建盘走 [disk.go:265](file:///c:/Users/89412/Projects/golang/vault/internal/app/disk.go#L265) 的 `MountAndCopy`，是「读取 staging 的 `tree/` → 拷入新盘」，不是 `rename`。上传暂存落在存储 LV 上、磁盘是另一个 LV，**不会 EXDEV**。
- `storage.require_same_volume` 是**死配置**：仅在 [config.go:113-114](file:///c:/Users/89412/Projects/golang/vault/internal/config/config.go#L113-L114) 声明，全仓无任何读取处 → 无需改动。
- 孤儿扫描在 Linux 上目前是**文件遍历**（[reconcile.go:340-357](file:///c:/Users/89412/Projects/golang/vault/internal/app/reconcile.go#L340-L357) 找 `.vhdx`），未挂载根会 `os.Stat` 失败而跳过 → **不会产生「全部丢失」的误判**（它只是空转）。故本轮不因此受限。

## 数据模型

新增表（`sqliteSchema` 与 `mysqlSchema` 各一份、结构等价）：

```
storage_volumes (
  storage_id  TEXT/VARCHAR(64) PK REFERENCES storages(id) ON DELETE CASCADE,
  kind        TEXT NOT NULL,          -- thin | lv | dir
  managed     BOOL NOT NULL,          -- true 才允许系统删除该 LV
  state       TEXT NOT NULL,          -- pending | ready
  ref         TEXT NOT NULL,          -- /dev/mapper/<vg>-<lv>；dir 为空
  file_system TEXT NOT NULL,
  size_bytes  BIGINT NOT NULL DEFAULT 0,
  created_at / updated_at
)
```

**无行 = 目录模式**（Windows 全量，或 Linux 未登记卷信息的历史存储），保持向后兼容。
`mount_point` 一律取 `storages.path`。

## 平台契约

`internal/platform/backend.go` 新增（`StorageAdmin` 只管 pool，语义不同，故独立接口）：

```go
type StorageVolumeSpec struct { Ref string; SizeBytes int64; FileSystem, MountPoint, Label string }
type StorageVolumeStatus struct {
    Exists, Mounted bool
    Ref, MountPoint, FileSystem string
    SizeBytes, UsedBytes int64
    Health string // 空 = 正常
}
type StorageVolumeBackend interface {
    CreateStorageVolume(ctx, spec) (*StorageVolumeStatus, error) // 幂等：建 LV → mkfs → mount → 校验挂载
    MountStorageVolume(ctx, ref, mountPoint) error               // 幂等；挂后核对 mountinfo
    UnmountStorageVolume(ctx, ref, mountPoint) error             // 幂等
    ResizeStorageVolume(ctx, ref, sizeBytes) error               // 只扩不缩；在线扩 fs
    DeleteStorageVolume(ctx, ref) error                          // umount → lvremove；幂等
    StorageVolumeStatus(ctx, ref, mountPoint) (*StorageVolumeStatus, error)
}
```

- **Windows**：`winbackend` 全部返回 `ErrUnsupported`（与既有 `StorageAdmin` 同模式），app 层用 `IsUnsupported` 走目录模式，**Windows 代码路径零改动**。
- **Linux**：新增 `internal/platform/linuxlvm/storagevolume.go`；`linuxbackend` 转发。复用 `run` / `lvRef` / `parseRef` / `lvSizeBytes` / `vgExists` / `checkWatermark` / `mountPointOf` / `LookPath`；新增 `lvextend`、`wipefs -a`、`statfs`、mountinfo 解析。

## 实施步骤（按「可独立验证」切分）

1. **Schema v3**：`storage_volumes` 建表（两方言）+ `migrate.go` 版本 → 3 + `internal/store/storage_volume_store.go`（CRUD）+ 老库升级幂等性检查。
2. **平台契约**：`backend.go` 新增接口与值对象；`winbackend` 实现为 `ErrUnsupported`；`apperr` 增补错误码（`storage.volume_*`、`storage.not_mounted`）。
3. **linuxlvm 实现**：`storagevolume.go` 六个方法（含 fail-closed 挂载校验、`wipefs`、在线扩 fs、`lvextend`）+ `SpaceUsageOf` 改为 `min(statfs, VG free)` 口径。此步不动 app，可用单测/最小 main 驱动验证。
4. **启动挂载与 guard 过滤**：`app.StorageService` 增「启动时幂等挂载全部启用中的受管存储」；`app.go:199 storageGuards` **剔除未成功挂载的存储**；定期 reconcile 重挂（应对手工 umount）。可手工 `umount` 验证降级与自愈。
5. **编排**：`StorageService.Create/Update/Delete` + 新增 `Mount/Unmount/Resize`；创建流程 `校验 → 建 LV → 写 pending → mkfs → mount → 校验 → 写 ready`，任一步失败**补偿回滚**（删除本次创建的 LV）；删除顺序 = 拒绝有盘（沿用 `StorageInUse`）→ 拒绝进行中的 upload/job → `umount` → `lvremove`（**仅 `managed=true`**）→ 删记录；全部写审计。
6. **API/DTO**：`POST /v1/storages/{id}/mount|unmount|resize`（仅超级管理员）；`storageDTO` 增补 `kind/mounted/ref/size_bytes`；创建请求增 `size_bytes/file_system/mount_point/mode`（`mode=thin|register_dir|register_lv`）；Windows 上 thin 创建返回 `501 platform.unsupported` 而非 500。
7. **前端**：`StorageList.tsx` 列改为 名称/类型/容量(已用÷分配)/挂载点/文件系统/磁盘数/状态(含未挂载)/操作；创建表单 = 名称 + 容量 + 文件系统 + 「高级」展开（自定义挂载点 / 切到登记已有目录或 LV）；行操作增 挂载/卸载/扩容；四份 i18n 补 key；池水位复用既有 `GET /v1/system/lvm`，不重复进 DTO。
8. **配置与文档**：`config.example.yaml` 说明 Linux 下 `storage.whitelist_roots` 的新含义；Linux 上 `EnsureSeededFromConfig` 跳过（避免种出一个误导放置的「目录」存储）；`docs/implementation.md` 增补本节（含启动挂载与 fail-closed 语义）；`docs/design.md` 更新存储节。

## 验证方案

**代码级（我来做）**
- `gofmt -l` 无输出；`CGO_ENABLED=0 go build ./...` + `go vet ./...`（Windows 目标）。
- `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./cmd/vault-server ./internal/{app,platform,api,lock,config}/...` + `go vet`。
- `pnpm -r typecheck`（ui + client）。
- 针对 `storage_volumes` 的新增/查询/删除与「老库（v2）升级到 v3」写单测；`SpaceUsageOf` 的 `min()` 口径与未挂载场景写单测。

**真机（留给你）**
- 建存储 → 确认 `lvs` 出现 `vl_st_<id>`、挂载点已挂载、`df` 容量 = 分配值；重启服务端 → 自动挂回。
- 手工 `umount` 后 → 管理页显示「未挂载」且不再被选为落盘位置（fail-closed）；等待 reconcile 自动挂回。
- 上传一个存储库 → 确认磁盘是**同池另一批 LV**、staging 树在存储 LV 上、最终 `tree/` 被清理。
- 扩容 → `lvextend` 生效且文件系统在线扩（`df` 变大）；删除（无盘）→ `lvremove` 成功；有盘 → 被拒绝。
- 逃生入口登记一个已有目录/已有 LV → 能正常落盘、删除时**不**误 `lvremove`。

## 明确不在本轮范围

- **Linux 的 LV 级孤儿/对账扫描**：当前 [reconcile.go](file:///c:/Users/89412/Projects/golang/vault/internal/app/reconcile.go#L340-L357) 是文件遍历，在 Linux 上等同空转（不危险但无效）。改为「`lvs` 扫池 + 与 disks 表比对」是独立改动，另起一轮。
- **每存储容量配额强制**：本轮只做「分配值展示 + `min()` 口径 + 池水位熔断」，不做 per-storage 硬配额。
- **第三方平台/存储迁移工具**：不做。