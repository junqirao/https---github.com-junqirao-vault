package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"vault/internal/apperr"
	"vault/internal/domain"
	"vault/internal/platform"

	"github.com/google/uuid"
)

// storageNameMaxLen 是存储名称的长度上限（按字符数）。
const storageNameMaxLen = 64

// 存储创建模式（CreateStorageInput.Mode）。
const (
	// StorageModeThin 用 thin pool 新建一个 thin LV，格式化后挂载为目录。
	// 仅在有存储卷后端的平台（Linux）可用，也是其默认模式。
	StorageModeThin = "thin"
	// StorageModeRegisterDir 登记一个已有目录（无底层卷，纯目录模式）；Windows 只有这一种。
	StorageModeRegisterDir = "register_dir"
	// StorageModeRegisterLV 登记一个已有 LV：只挂载、不格式化、删除时也不删卷（逃生入口）。
	StorageModeRegisterLV = "register_lv"
)

// defaultStorageMountRoot 是"自动生成挂载点"的父目录。
//
// 仅当高级选项未指定挂载点时使用（见 CreateStorageInput.Path）。
const defaultStorageMountRoot = "/var/lib/vault/storages"

// StorageService 管理"存储"（可在线维护的 VHDX 根目录）实体。
//
// 数据来源：数据库是唯一真源；config.yaml 的 storage.whitelist_roots 仅作为
// 首次启动的一次性种子（见 EnsureSeededFromConfig）。放置磁盘时只在"启用"存储中进行。
type StorageService struct{ Deps }

// MountedStorages 是"存储底层卷是否已就绪挂载"的进程内快照。
//
// 用途：storageSelection 决定哪些存储可用于落盘时必须剔除"卷未挂载"的存储（fail-closed），
// 否则写入会静默落在宿主根文件系统上。逐个存储去平台层探测代价太高（每次落盘都要查），
// 故由启动对账与定期 reconcile 一次性核对后写在这里（见 RefreshStorageMounts）。
//
// computed=false 表示**尚未核对过**：此时 Ready 一律返回 false（没有确认就当作不可用）。
// Windows 上整个 Deps.Mounted 为 nil（平台没有存储卷概念），由 storageMountReady 直接放行。
type MountedStorages struct {
	mu       sync.RWMutex
	computed bool
	blocked  map[string]bool
}

// NewMountedStorages 创建一个"尚未核对"的就绪集合。
func NewMountedStorages() *MountedStorages {
	return &MountedStorages{blocked: map[string]bool{}}
}

// Replace 用一次完整核对的结论替换就绪集合，并把状态标记为"已核对"。
func (m *MountedStorages) Replace(blockedIDs []string) {
	set := make(map[string]bool, len(blockedIDs))
	for _, id := range blockedIDs {
		set[id] = true
	}
	m.mu.Lock()
	m.blocked, m.computed = set, true
	m.mu.Unlock()
}

// Set 单条即时更新就绪状态（挂载/卸载/创建/删除后调用）。
func (m *MountedStorages) Set(id string, ready bool) {
	m.mu.Lock()
	if m.blocked == nil {
		m.blocked = map[string]bool{}
	}
	if ready {
		delete(m.blocked, id)
	} else {
		m.blocked[id] = true
	}
	m.mu.Unlock()
}

// Ready 返回该存储的底层卷是否已就绪挂载。
func (m *MountedStorages) Ready(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.computed {
		return false
	}
	return !m.blocked[id]
}

// StorageView 是存储实体及其运行期统计。
//
// 统计口径：
//   - file_system / free_bytes / total_bytes / volume_name 来自存储所在卷（经 VolumeSpaceProvider）；
//     位于同一卷的多个存储会得到相同的卷数据，这是预期行为；
//   - disk_count / used_bytes 来自该存储目录下（按目录边界匹配）的磁盘记录。
type StorageView struct {
	Storage domain.Storage
	// FileSystem 所在卷文件系统（如 NTFS）；查询失败时为空。
	FileSystem string
	// FreeBytes / TotalBytes 所在卷可用空间 / 总容量。
	FreeBytes  int64
	TotalBytes int64
	// VolumeName 所在卷标识（规范化、小写，如 "d:"）。
	VolumeName string
	// DiskCount / UsedBytes 该存储下的磁盘数量与占用。
	DiskCount int
	UsedBytes int64
	// Volume 底层卷登记（thin / lv / dir）；**为 nil 表示目录模式**（Windows，或历史存储）。
	Volume *domain.StorageVolume
	// Mounted 底层卷是否已挂载就绪；目录模式恒为 true。
	//
	// 未挂载的存储会被剔除出落盘集合（fail-closed，见 app.go storageSelection）。
	Mounted bool
}

// CreateStorageInput 是创建存储的入参。
type CreateStorageInput struct {
	Name    string
	Path    string
	Enabled bool
	// Mode 见 StorageMode*；留空时：有存储卷后端（Linux）取 thin，否则取 register_dir。
	Mode string
	// SizeBytes 分配容量（thin 必填，>0）；register_* 忽略。
	SizeBytes int64
	// PoolRef 存储池（"<vg>/<thin_pool>"，见 PoolService.ListPools）；仅 thin 使用。
	//
	// 一台机器可以有多个存储池，创建存储时"选择或新建"其中之一（可留空 = 后端默认池）。
	// 该存储的底层卷与其下所有磁盘都会落在同一个池里。
	PoolRef string
	// FileSystem 存储卷文件系统（ext4 默认 / xfs）；仅 thin 使用。
	FileSystem string
	// Ref 已有 LV 引用（register_lv 必填，形如 /dev/mapper/<vg>-<lv>）。
	Ref string
}

// UpdateStorageInput 是更新存储的入参；指针字段为 nil 表示不修改。
type UpdateStorageInput struct {
	Name    *string
	Path    *string
	Enabled *bool
}

// List 列出全部存储及其统计。
func (s *StorageService) List(ctx context.Context) ([]StorageView, error) {
	storages, err := s.Store.ListStorages(ctx)
	if err != nil {
		return nil, err
	}
	return s.viewsFor(ctx, storages)
}

// Get 按 ID 查询单个存储及其统计。
func (s *StorageService) Get(ctx context.Context, id string) (*StorageView, error) {
	st, err := s.Store.GetStorage(ctx, id)
	if err != nil {
		return nil, err
	}
	views, err := s.viewsFor(ctx, []domain.Storage{*st})
	if err != nil {
		return nil, err
	}
	return &views[0], nil
}

// viewsFor 为给定存储批量补充统计（磁盘统计一次查询、卷空间逐个查询）。
func (s *StorageService) viewsFor(ctx context.Context, storages []domain.Storage) ([]StorageView, error) {
	paths := make([]string, 0, len(storages))
	for i := range storages {
		paths = append(paths, storages[i].Path)
	}
	stats, err := s.Store.CountDisksUnderPaths(ctx, paths)
	if err != nil {
		return nil, err
	}
	vols, err := s.Store.ListStorageVolumes(ctx)
	if err != nil {
		return nil, err
	}
	volByID := make(map[string]domain.StorageVolume, len(vols))
	for i := range vols {
		volByID[vols[i].StorageID] = vols[i]
	}

	provider := s.volumeProvider()
	out := make([]StorageView, 0, len(storages))
	for i := range storages {
		st := storages[i]
		v := StorageView{Storage: st, Mounted: s.Deps.storageMountReady(st.ID)}
		if vol, ok := volByID[st.ID]; ok {
			v.Volume = &vol
		}
		if stat, ok := stats[storagePathKey(st.Path)]; ok {
			v.DiskCount, v.UsedBytes = stat.DiskCount, stat.UsedBytes
		}
		if provider != nil {
			if space, sErr := provider.SpaceUsageOf(ctx, st.Path); sErr == nil && space != nil {
				v.FileSystem = space.FileSystem
				v.FreeBytes = space.FreeBytes
				v.TotalBytes = space.TotalBytes
				v.VolumeName = space.Name
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// Create 创建存储。
//
// 流程（见计划步骤 5）：校验 → 建底层卷（thin）→ 格式化 → 挂载 → 校验 → 落库 ready。
// 任一步失败都会**补偿回滚**：删除本次登记的卷与存储行，避免留下半成品。
func (s *StorageService) Create(ctx context.Context, in CreateStorageInput) (*StorageView, error) {
	mode, err := s.resolveCreateMode(in.Mode)
	if err != nil {
		return nil, err
	}
	name, err := s.validateName(ctx, in.Name, "")
	if err != nil {
		return nil, err
	}

	// 先生成 ID：默认挂载点与 thin 卷引用都依赖它（此时存储行尚未落库，仅本地使用）。
	id := uuid.NewString()
	path := strings.TrimSpace(in.Path)
	if path == "" {
		if mode == StorageModeRegisterDir {
			// 目录模式没有"卷"可用于推导挂载点，必须显式给出目录。
			return nil, apperr.InvalidParam("path")
		}
		path = defaultMountPoint(name, id)
	}
	path, err = s.validatePath(ctx, path, "")
	if err != nil {
		return nil, err
	}

	st := &domain.Storage{ID: id, Name: name, Path: path, Enabled: in.Enabled}
	if err := s.Store.CreateStorage(ctx, st); err != nil {
		return nil, err
	}
	if err := s.setupStorageVolume(ctx, st, mode, in); err != nil {
		s.rollbackCreate(ctx, st, mode)
		return nil, err
	}
	s.audit(ctx, "", "storage.create", "storage:"+st.ID,
		fmt.Sprintf("name=%s path=%s enabled=%t mode=%s size_bytes=%d pool_ref=%s",
			st.Name, st.Path, st.Enabled, mode, in.SizeBytes, strings.TrimSpace(in.PoolRef)), domain.AuditResultOK)
	return s.Get(ctx, st.ID)
}

// resolveCreateMode 归一化并校验创建模式；留空时按平台给默认值。
func (s *StorageService) resolveCreateMode(raw string) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(raw))
	switch mode {
	case "":
		if s.Deps.StorageVol == nil {
			return StorageModeRegisterDir, nil
		}
		return StorageModeThin, nil
	case StorageModeRegisterDir:
		return StorageModeRegisterDir, nil
	case StorageModeThin, StorageModeRegisterLV:
		if s.Deps.StorageVol == nil {
			// Windows 无"底层卷"概念：明确返回 501，而不是让平台层报 500。
			return "", apperr.PlatformUnsupported()
		}
		return mode, nil
	default:
		return "", apperr.InvalidParam("mode")
	}
}

// defaultMountPoint 按"名称 + ID 前 8 位"生成默认挂载点，保证可读且唯一。
func defaultMountPoint(name, id string) string {
	slug := storageMountSlug(name)
	short := id
	if len(short) > 8 {
		short = short[:8]
	}
	return filepath.Join(defaultStorageMountRoot, slug+"-"+short)
}

// storageMountSlug 把存储名折叠为适合做目录名的片段（小写、非字母数字折叠为 '-'）。
func storageMountSlug(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		slug = "storage"
	}
	if len(slug) > 48 {
		slug = strings.Trim(slug[:48], "-")
	}
	return slug
}

// setupStorageVolume 建立/登记底层卷并挂载就绪，成功后把该存储标记为可落盘。
//
// 三种模式：
//   - thin：新建 thin LV → 格式化 → 挂载（删卷由本系统负责，managed=true）；
//   - register_lv：登记已有 LV，只挂载、**绝不格式化**，删除时也不删卷（managed=false）；
//   - register_dir：纯目录，仅登记一条 kind=dir 记录（无底层卷）。
func (s *StorageService) setupStorageVolume(ctx context.Context, st *domain.Storage, mode string, in CreateStorageInput) error {
	vol := &domain.StorageVolume{
		StorageID: st.ID,
		State:     domain.StorageVolumeStatePending,
	}
	switch mode {
	case StorageModeThin:
		if s.Disk == nil {
			return apperr.PlatformUnsupported()
		}
		if in.SizeBytes <= 0 {
			return apperr.InvalidParam("size_bytes")
		}
		// 存储池：用户选了就落在该池（引用要在该池的 VG 里生成），没选则用后端默认池。
		poolRef, err := normalizePoolRef(in.PoolRef)
		if err != nil {
			return err
		}
		ref, err := s.storageRefFor(poolRef, st.ID)
		if err != nil {
			return err
		}
		vol.Kind = domain.StorageVolumeKindThin
		vol.Managed = true
		vol.Ref = ref
		vol.PoolRef = poolRef
		vol.FileSystem = strings.TrimSpace(in.FileSystem) // 空 = ext4，由平台层归一化
		vol.SizeBytes = in.SizeBytes
		if err := s.Store.CreateStorageVolume(ctx, vol); err != nil {
			return err
		}
		status, err := s.Deps.StorageVol.CreateStorageVolume(ctx, platform.StorageVolumeSpec{
			Ref:        ref,
			PoolRef:    poolRef,
			SizeBytes:  in.SizeBytes,
			FileSystem: vol.FileSystem,
			MountPoint: st.Path,
			Label:      st.Name,
		})
		if err != nil {
			return err
		}
		return s.markVolumeReady(ctx, vol, status)

	case StorageModeRegisterLV:
		ref := strings.TrimSpace(in.Ref)
		if ref == "" {
			return apperr.InvalidParam("ref")
		}
		vol.Kind = domain.StorageVolumeKindLV
		vol.Managed = false
		vol.Ref = ref
		vol.FileSystem = strings.TrimSpace(in.FileSystem)
		if err := s.Store.CreateStorageVolume(ctx, vol); err != nil {
			return err
		}
		if err := s.Deps.StorageVol.MountStorageVolume(ctx, ref, st.Path); err != nil {
			return err
		}
		status, err := s.Deps.StorageVol.StorageVolumeStatus(ctx, ref, st.Path)
		if err != nil {
			return err
		}
		return s.markVolumeReady(ctx, vol, status)

	default: // StorageModeRegisterDir：纯目录，只登记来源。
		vol.Kind = domain.StorageVolumeKindDir
		vol.State = domain.StorageVolumeStateReady
		if err := s.Store.CreateStorageVolume(ctx, vol); err != nil {
			return err
		}
		s.setStorageMounted(st.ID, true)
		return nil
	}
}

// storageRefFor 生成存储卷的设备引用。
//
// 用户选了存储池时，引用必须在该池的 VG 里生成（后端实现 PooledDiskBackend）；
// 平台不支持"按池生成引用"（Windows）时退化为默认引用——此时池参数在平台层本来也被忽略。
func (s *StorageService) storageRefFor(poolRef, storageID string) (string, error) {
	rel := "storages/" + storageID
	if poolRef != "" {
		if pb, ok := s.Disk.(platform.PooledDiskBackend); ok {
			return pb.DiskRefInPool(poolRef, rel)
		}
	}
	return s.Disk.DiskRef("", rel)
}

// normalizePoolRef 校验并归一化存储池键（"<vg>/<thin_pool>"）；空串表示后端默认池。
func normalizePoolRef(raw string) (string, error) {
	key := strings.TrimSpace(raw)
	if key == "" {
		return "", nil
	}
	vg, pool, err := platform.SplitPoolKey(key)
	if err != nil {
		return "", apperr.InvalidParam("pool_ref")
	}
	return platform.PoolKey(vg, pool), nil
}

// markVolumeReady 回填平台层给出的实际口径并把卷标记为 ready。
func (s *StorageService) markVolumeReady(ctx context.Context, vol *domain.StorageVolume, status *platform.StorageVolumeStatus) error {
	if status == nil || !status.Mounted {
		return apperr.StorageNotMounted()
	}
	if status.FileSystem != "" {
		vol.FileSystem = status.FileSystem
	}
	if status.SizeBytes > 0 {
		vol.SizeBytes = status.SizeBytes
	}
	vol.State = domain.StorageVolumeStateReady
	if err := s.Store.UpdateStorageVolume(ctx, vol); err != nil {
		return err
	}
	s.setStorageMounted(vol.StorageID, true)
	return nil
}

// rollbackCreate 补偿回滚一次失败的存储创建（只在 Create 内部使用）。
//
// 顺序与创建相反：先尽力删除本次创建的底层卷（仅 thin 是本系统创建的），
// 再删登记与存储行。删除失败只告警不返回——原始错误才是要暴露给调用方的。
func (s *StorageService) rollbackCreate(ctx context.Context, st *domain.Storage, mode string) {
	switch mode {
	case StorageModeThin:
		if s.Disk != nil && s.Deps.StorageVol != nil {
			// 优先用**已登记**的引用：用户可能选了非默认存储池，
			// 重新按默认口径推导会得到另一个 VG 里的 LV，删错卷/漏删卷。
			ref := ""
			if vol, err := s.Store.GetStorageVolume(ctx, st.ID); err == nil && vol != nil {
				ref = strings.TrimSpace(vol.Ref)
			}
			if ref == "" {
				if r, err := s.storageRefFor("", st.ID); err == nil {
					ref = r
				}
			}
			if ref != "" {
				if err := s.Deps.StorageVol.DeleteStorageVolume(ctx, ref); err != nil {
					s.Log.Warn("回滚存储卷失败（可能留下无主 LV，请检查）",
						"storage_id", st.ID, "ref", ref, "error", err)
				}
			}
		}
	case StorageModeRegisterLV:
		// 登记已有卷：只把本次挂上的卸载掉，绝不删卷。
		if vol, err := s.Store.GetStorageVolume(ctx, st.ID); err == nil && vol != nil && vol.Ref != "" {
			if err := s.Deps.StorageVol.UnmountStorageVolume(ctx, vol.Ref, st.Path); err != nil {
				s.Log.Warn("回滚时卸载已登记的 LV 失败", "storage_id", st.ID, "ref", vol.Ref, "error", err)
			}
		}
	}
	if err := s.Store.DeleteStorageVolume(ctx, st.ID); err != nil && !isNotFound(err) {
		s.Log.Warn("回滚存储卷登记失败", "storage_id", st.ID, "error", err)
	}
	if err := s.Store.DeleteStorage(ctx, st.ID); err != nil && !isNotFound(err) {
		s.Log.Warn("回滚存储记录失败", "storage_id", st.ID, "error", err)
	}
}

// Update 更新存储的 name / path / enabled。
func (s *StorageService) Update(ctx context.Context, id string, in UpdateStorageInput) (*StorageView, error) {
	st, err := s.Store.GetStorage(ctx, id)
	if err != nil {
		return nil, err
	}
	if in.Name != nil {
		name, nErr := s.validateName(ctx, *in.Name, id)
		if nErr != nil {
			return nil, nErr
		}
		st.Name = name
	}
	if in.Path != nil {
		path, pErr := s.validatePath(ctx, *in.Path, id)
		if pErr != nil {
			return nil, pErr
		}
		if !strings.EqualFold(path, st.Path) {
			vol, vErr := s.Store.GetStorageVolume(ctx, id)
			if vErr != nil {
				return nil, vErr
			}
			if vol != nil && strings.TrimSpace(vol.Ref) != "" {
				// 挂载点就是存储路径：直接改路径会让"卷 ↔ 目录"脱钩，
				// 之后的写入会落到宿主根文件系统上（且旧挂载仍占着旧目录）。
				return nil, apperr.InvalidParam("path").
					WithCause(errors.New("存储已绑定底层卷，请先卸载（unmount）再修改挂载点"))
			}
			// 改路径不会搬迁已有磁盘（disks.vhdx_path 不变），仅影响后续放置与扫描范围；
			// 若该存储下仍有磁盘，明确告警便于运维排查"盘还在旧根下"。
			if n, cErr := s.diskCount(ctx, st.Path); cErr == nil && n > 0 {
				s.Log.Warn("存储路径变更，但该存储下仍有磁盘（磁盘文件不会自动搬迁）",
					"storage_id", id, "old_path", st.Path, "new_path", path, "disk_count", n)
			}
		}
		st.Path = path
	}
	if in.Enabled != nil {
		st.Enabled = *in.Enabled
	}
	if err := s.Store.UpdateStorage(ctx, st); err != nil {
		return nil, err
	}
	s.audit(ctx, "", "storage.update", "storage:"+id,
		fmt.Sprintf("name=%s path=%s enabled=%t", st.Name, st.Path, st.Enabled), domain.AuditResultOK)
	return s.Get(ctx, id)
}

// Delete 删除存储。该存储下仍有磁盘、或仍有进行中的上传时拒绝删除。
//
// 顺序（见计划步骤 5）：拒绝有盘 → 拒绝进行中上传 → 卸载 → 删底层卷（**仅 managed**）
// → 删登记 → 删存储行。登记已有的卷只解除登记，绝不删除用户数据。
func (s *StorageService) Delete(ctx context.Context, id string) error {
	st, err := s.Store.GetStorage(ctx, id)
	if err != nil {
		return err
	}
	n, err := s.diskCount(ctx, st.Path)
	if err != nil {
		return err
	}
	if n > 0 {
		return apperr.StorageInUse(n)
	}
	vol, err := s.Store.GetStorageVolume(ctx, id)
	if err != nil {
		return err
	}
	// 暂存目录就建在该存储卷上：有进行中的上传时卸载/删卷会让写入落到宿主根文件系统。
	if err := s.ensureNoActiveUploads(ctx, st.Path); err != nil {
		return err
	}

	if vol != nil && s.Deps.StorageVol != nil && strings.TrimSpace(vol.Ref) != "" {
		if err := s.Deps.StorageVol.UnmountStorageVolume(ctx, vol.Ref, st.Path); err != nil {
			return err
		}
		if vol.Managed {
			if err := s.Deps.StorageVol.DeleteStorageVolume(ctx, vol.Ref); err != nil {
				return err
			}
		}
	}
	if vol != nil {
		if err := s.Store.DeleteStorageVolume(ctx, id); err != nil {
			return err
		}
	}
	if err := s.Store.DeleteStorage(ctx, id); err != nil {
		return err
	}
	s.audit(ctx, "", "storage.delete", "storage:"+id,
		fmt.Sprintf("name=%s kind=%s", st.Name, volumeKind(vol)), domain.AuditResultOK)
	return nil
}

// Mount 幂等地把存储底层卷挂载到其挂载点（storages.path）。
func (s *StorageService) Mount(ctx context.Context, id string) (*StorageView, error) {
	st, vol, err := s.requireStorageVolume(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.Deps.StorageVol.MountStorageVolume(ctx, vol.Ref, st.Path); err != nil {
		return nil, err
	}
	status, err := s.Deps.StorageVol.StorageVolumeStatus(ctx, vol.Ref, st.Path)
	if err != nil {
		return nil, err
	}
	if status == nil || !status.Mounted {
		return nil, apperr.StorageNotMounted()
	}
	s.setStorageMounted(id, true)
	s.audit(ctx, "", "storage.mount", "storage:"+id, "ref="+vol.Ref, domain.AuditResultOK)
	return s.Get(ctx, id)
}

// Unmount 幂等地卸载存储底层卷；有进行中的上传时拒绝。
func (s *StorageService) Unmount(ctx context.Context, id string) (*StorageView, error) {
	st, vol, err := s.requireStorageVolume(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.ensureNoActiveUploads(ctx, st.Path); err != nil {
		return nil, err
	}
	if err := s.Deps.StorageVol.UnmountStorageVolume(ctx, vol.Ref, st.Path); err != nil {
		return nil, err
	}
	s.setStorageMounted(id, false)
	s.audit(ctx, "", "storage.unmount", "storage:"+id, "ref="+vol.Ref, domain.AuditResultOK)
	return s.Get(ctx, id)
}

// Resize 扩容存储底层卷（**只扩不缩**）；仅系统创建的 thin 卷支持。
func (s *StorageService) Resize(ctx context.Context, id string, sizeBytes int64) (*StorageView, error) {
	_, vol, err := s.requireStorageVolume(ctx, id)
	if err != nil {
		return nil, err
	}
	if sizeBytes <= 0 {
		return nil, apperr.InvalidParam("size_bytes")
	}
	if vol.Kind != domain.StorageVolumeKindThin {
		// 登记已有的卷可能还有别的使用者，本系统不代为扩容。
		return nil, apperr.InvalidParam("size_bytes").
			WithCause(errors.New("仅系统创建的 thin 存储卷支持扩容"))
	}
	if err := s.Deps.StorageVol.ResizeStorageVolume(ctx, vol.Ref, sizeBytes); err != nil {
		return nil, err
	}
	vol.SizeBytes = sizeBytes
	if err := s.Store.UpdateStorageVolume(ctx, vol); err != nil {
		return nil, err
	}
	s.audit(ctx, "", "storage.resize", "storage:"+id,
		fmt.Sprintf("ref=%s size_bytes=%d", vol.Ref, sizeBytes), domain.AuditResultOK)
	return s.Get(ctx, id)
}

// requireStorageVolume 读取存储及其底层卷登记；平台不支持或未登记底层卷时返回明确错误。
func (s *StorageService) requireStorageVolume(ctx context.Context, id string) (*domain.Storage, *domain.StorageVolume, error) {
	if s.Deps.StorageVol == nil {
		return nil, nil, apperr.PlatformUnsupported()
	}
	st, err := s.Store.GetStorage(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	vol, err := s.Store.GetStorageVolume(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if vol == nil || strings.TrimSpace(vol.Ref) == "" {
		return nil, nil, apperr.StorageVolumeNotFound().WithArg("id", id)
	}
	return st, vol, nil
}

// ensureNoActiveUploads 确认该存储下没有进行中的上传会话。
func (s *StorageService) ensureNoActiveUploads(ctx context.Context, path string) error {
	n, err := s.Store.CountActiveUploadsUnderPath(ctx, path)
	if err != nil {
		return err
	}
	if n > 0 {
		return apperr.StorageVolumeBusy().WithArg("active_uploads", n)
	}
	return nil
}

// setStorageMounted 更新进程内就绪集合；未装配（Windows）时为空操作。
func (s *StorageService) setStorageMounted(id string, ready bool) {
	if s.Deps.Mounted == nil {
		return
	}
	s.Deps.Mounted.Set(id, ready)
}

// volumeKind 返回卷类型（nil 表示目录模式），仅用于审计描述。
func volumeKind(vol *domain.StorageVolume) string {
	if vol == nil {
		return "directory"
	}
	return vol.Kind
}

// EnsureSeededFromConfig 在 storages 表**为空**时，把配置中的白名单根写入为存储（首次种子）。
//
// 语义：配置仅作首次初始化种子；一旦表中已有记录（哪怕已全部停用），本方法不再写入，
// 后续请在管理端维护存储。返回本次新建的存储数量。
//
// Windows（无存储卷后端）才会真正种入目录存储；Linux 上直接跳过（见下方说明）。
func (s *StorageService) EnsureSeededFromConfig(ctx context.Context, roots []string) (int, error) {
	// 有存储卷后端（Linux）时不做种子：配置里的白名单根是宿主机上的普通目录，
	// 照旧种进去会得到一个可被放置磁盘的「目录」存储，写入就悄悄落在宿主根文件系统上
	// （而不是受管 thin LV），正是本设计要避免的。请改在管理端创建存储。
	if s.Deps.StorageVol != nil {
		if len(roots) > 0 {
			s.Log.Info("平台存在存储底层卷（thin LV），已跳过 config 存储种子；请在管理端创建存储", "roots", roots)
		}
		return 0, nil
	}

	existing, err := s.Store.CountStorages(ctx)
	if err != nil {
		return 0, err
	}
	if existing > 0 {
		return 0, nil
	}

	used := make(map[string]bool, len(roots))
	created := 0
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		p := filepath.Clean(strings.TrimSpace(root))
		name := uniqueStorageName(storageNameFromPath(p), used)
		st := &domain.Storage{Name: name, Path: p, Enabled: true}
		if err := s.Store.CreateStorage(ctx, st); err != nil {
			return created, err
		}
		created++
	}
	return created, nil
}

// RefreshStorageMounts 核对全部受管存储的底层卷，幂等重挂未挂载者，并刷新就绪集合。
//
// 返回"仍未就绪（被剔除出落盘集合）的存储数量"。服务端启动时与定期对账各调用一次
// （见 reconcile.go），用于应对重启后挂载丢失、运维手工 umount 等情况——挂载持久化
// 不写 /etc/fstab、也不生成 systemd unit，完全由这里负责。
//
// 平台没有存储卷概念（Windows，Deps.StorageVol 为 nil）时直接返回 0，不做任何处理。
func (s *StorageService) RefreshStorageMounts(ctx context.Context) (int, error) {
	if s.Deps.StorageVol == nil || s.Deps.Mounted == nil {
		return 0, nil
	}
	storages, err := s.Store.ListStorages(ctx)
	if err != nil {
		return 0, err
	}
	vols, err := s.Store.ListStorageVolumes(ctx)
	if err != nil {
		return 0, err
	}
	byID := make(map[string]domain.StorageVolume, len(vols))
	for i := range vols {
		byID[vols[i].StorageID] = vols[i]
	}

	blocked := make([]string, 0)
	for i := range storages {
		st := storages[i]
		vol, ok := byID[st.ID]
		if !ok || vol.Kind == domain.StorageVolumeKindDir || strings.TrimSpace(vol.Ref) == "" {
			continue // 目录模式：无底层卷，按普通目录处理
		}
		if err := s.ensureVolumeMounted(ctx, st.Path, vol); err != nil {
			s.Log.Warn("存储底层卷未就绪，已从落盘集合剔除",
				"storage_id", st.ID, "ref", vol.Ref, "error", err)
			blocked = append(blocked, st.ID)
		}
	}
	s.Deps.Mounted.Replace(blocked)
	return len(blocked), nil
}

// ensureVolumeMounted 幂等确保某个存储卷已挂载到 mountPoint。
//
// fail-closed：挂载命令返回成功不等于真的挂上了，必须再核对一次实挂状态，
// 否则写入会落在宿主根文件系统上（日后挂载再把数据"藏"起来）。
func (s *StorageService) ensureVolumeMounted(ctx context.Context, mountPoint string, vol domain.StorageVolume) error {
	st, err := s.Deps.StorageVol.StorageVolumeStatus(ctx, vol.Ref, mountPoint)
	if err != nil {
		return err
	}
	if st == nil || !st.Exists {
		return apperr.StorageVolumeFailed("volume_missing")
	}
	if st.Mounted {
		return nil
	}
	if err := s.Deps.StorageVol.MountStorageVolume(ctx, vol.Ref, mountPoint); err != nil {
		return err
	}
	st2, err := s.Deps.StorageVol.StorageVolumeStatus(ctx, vol.Ref, mountPoint)
	if err != nil {
		return err
	}
	if st2 == nil || !st2.Mounted {
		return apperr.StorageNotMounted()
	}
	return nil
}

// diskCount 统计某存储路径下的磁盘数量（按目录边界、大小写不敏感）。
func (s *StorageService) diskCount(ctx context.Context, path string) (int, error) {
	stats, err := s.Store.CountDisksUnderPaths(ctx, []string{path})
	if err != nil {
		return 0, err
	}
	return stats[storagePathKey(path)].DiskCount, nil
}

// validateName 校验并规范化存储名称：非空、长度上限、唯一（大小写不敏感）。
// excludeID 为更新时的自身 ID（允许保持原名）。
func (s *StorageService) validateName(ctx context.Context, raw, excludeID string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", apperr.InvalidParam("name")
	}
	if utf8.RuneCountInString(name) > storageNameMaxLen {
		return "", apperr.InvalidParam("name")
	}
	existing, err := s.Store.GetStorageByName(ctx, name)
	if err != nil && !isNotFound(err) {
		return "", err
	}
	if existing != nil && existing.ID != excludeID {
		return "", apperr.StorageNameTaken().WithArg("name", name)
	}
	return name, nil
}

// validatePath 校验并规范化存储路径：非空、绝对路径、非 UNC、去重（大小写不敏感）、可写性探测。
// excludeID 为更新时的自身 ID（允许保持原路径）。
func (s *StorageService) validatePath(ctx context.Context, raw, excludeID string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return "", apperr.InvalidParam("path")
	}
	if !filepath.IsAbs(p) || isUNCPath(p) {
		return "", apperr.InvalidParam("path")
	}
	p = filepath.Clean(p)

	existing, err := s.Store.GetStorageByPath(ctx, p)
	if err != nil && !isNotFound(err) {
		return "", err
	}
	if existing != nil && existing.ID != excludeID {
		return "", apperr.StoragePathTaken().WithArg("path", p)
	}
	if err := probeWritable(p); err != nil {
		return "", apperr.InvalidParam("path").WithCause(err)
	}
	return p, nil
}

// probeWritable 探测目录是否可写。
//
// 路径不存在时视为通过（首次创建由建盘流程 MkdirAll 建目录）；
// 存在但不是目录、或无法写入时返回明确错误。
func probeWritable(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("路径不是目录")
	}
	f, err := os.CreateTemp(dir, ".vault-write-test-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// storagePathKey 生成与 store.CountDisksUnderPaths 一致的查询键（Clean + 小写）。
func storagePathKey(path string) string {
	return strings.ToLower(filepath.Clean(strings.TrimSpace(path)))
}

// isUNCPath 判断是否为 UNC 路径（存储根不允许位于网络路径）。
func isUNCPath(p string) bool {
	return strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//")
}

// storageNameFromPath 由路径推导默认存储名（取最后一段；盘根回退为盘符）。
func storageNameFromPath(p string) string {
	base := strings.Trim(filepath.Base(filepath.Clean(p)), `\/`)
	if base == "" || base == "." || base == string(filepath.Separator) {
		if v := strings.TrimRight(filepath.VolumeName(p), ":"); v != "" {
			return v
		}
		return "storage"
	}
	if utf8.RuneCountInString(base) > storageNameMaxLen {
		base = string([]rune(base)[:storageNameMaxLen])
	}
	return base
}

// uniqueStorageName 在 base 与已有名称冲突时追加序号（base-2、base-3 ...）。
func uniqueStorageName(base string, used map[string]bool) string {
	name := base
	for i := 2; used[strings.ToLower(name)]; i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	used[strings.ToLower(name)] = true
	return name
}
