// Package app 是应用服务层，负责业务编排与事务边界。
//
// 分层约定（见 docs/implementation.md 6.2）：
//   - 本层可以调用 store / platform / job / lock / cert / secret；
//   - 本层**不允许**出现 SQL、PowerShell 脚本或 HTTP 细节；
//   - 所有长耗时操作必须通过 job.Worker 异步执行，不得在 HTTP 请求内同步等待。
package app

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"vault/internal/cert"
	"vault/internal/config"
	"vault/internal/domain"
	"vault/internal/job"
	"vault/internal/lock"
	"vault/internal/platform"
	"vault/internal/secret"
	"vault/internal/store"
)

// Deps 是应用服务层的依赖集合。
type Deps struct {
	Store  *store.Store
	Cfg    *config.Watcher
	Log    *slog.Logger
	Locks  *lock.Keyed
	Jobs   *job.Worker
	Cipher *secret.Cipher
	CA     *cert.CA

	// 平台能力（平台中性接口；具体实现由 cmd 按运行平台装配，见 internal/platform）。
	//
	// 三个后端都由同一份上层代码驱动：Windows 是 VHDX + IscsiTarget 模块，
	// Linux 是 LVM thin LV + LIO。本层**不允许**出现任何 win* 包引用。
	Disk  platform.DiskBackend
	Vol   platform.VolumeBackend
	Iscsi platform.IscsiBackend
	// Platform 存储池/缓存设备管理（Linux=LVM/dm-cache；Windows 整体返回 ErrUnsupported）。
	Platform platform.StorageAdmin
	// StorageVol 存储底层卷管理（Linux=thin LV 建/挂/卸/扩/删）。
	//
	// 装配为 nil 表示"平台没有存储卷概念"（Windows）：此时存储就是普通目录，
	// 相关 API 由 api 层返回 platform.unsupported（501），落盘逻辑零改动。
	StorageVol platform.StorageVolumeBackend
	// Mounted 存储底层卷就绪快照（见 storage.go 的 MountedStorages 说明）。
	//
	// 仅在 StorageVol 非 nil 时由 New 初始化；Windows 上保持 nil，storageMountReady 直接放行。
	Mounted *MountedStorages

	// Tokens enrollment 一次性令牌。
	Tokens *cert.TokenStore
	// Sessions 管理页会话。
	Sessions *SessionStore
	// Events 事件广播通道（进程内，用于 SSE；见 events.go 说明）。
	Events EventSink
}

// App 聚合全部应用服务。
type App struct {
	Deps

	repo   *RepoService
	disk   *DiskService
	iscsi  *IscsiService
	lease  *LeaseService
	auth   *AuthService
	user   *UserService
	upload *UploadService
	// storage 负责"存储"（可在线管理的 VHDX 根目录）的增删改查与统计。
	storage *StorageService
	// fs 负责服务端本地目录的浏览与统计（源目录选择，受 storage.source_roots 白名单约束）。
	fs *FSService
	// pool 负责存储池（LVM thin pool）与缓存设备的平台管理（Windows 下不可用）。
	pool *PoolService

	// reconcileMu 保护最近一次对账报告（HTTP 查询与定时任务并发访问）。
	reconcileMu   sync.Mutex
	lastReconcile *ReconcileReport

	// capCacheMu / capCache 缓存系统能力探测结果（见 system.go 中 capProbeCacheTTL 的说明）。
	capCacheMu sync.Mutex
	capCache   map[string]capCacheEntry
}

// New 构造 App 并完成各服务装配。
func New(d Deps) *App {
	// 存储底层卷就绪快照必须在各服务拷贝 Deps 之前建好，保证它们共享同一实例。
	if d.StorageVol != nil && d.Mounted == nil {
		d.Mounted = NewMountedStorages()
	}
	a := &App{Deps: d}
	a.user = &UserService{Deps: d}
	a.auth = &AuthService{Deps: d, Users: a.user}
	a.repo = &RepoService{Deps: d}
	a.disk = &DiskService{Deps: d, Repos: a.repo}
	a.iscsi = &IscsiService{Deps: d, Disks: a.disk}
	a.lease = &LeaseService{Deps: d, Repos: a.repo, Disks: a.disk, Iscsi: a.iscsi}
	a.upload = &UploadService{Deps: d, Disks: a.disk, Repos: a.repo}
	a.storage = &StorageService{Deps: d}
	a.fs = &FSService{Deps: d}
	a.pool = &PoolService{Deps: d}
	return a
}

// Repos 返回存储库服务。
func (a *App) Repos() *RepoService { return a.repo }

// Disks 返回磁盘服务。
func (a *App) Disks() *DiskService { return a.disk }

// Iscsi 返回 iSCSI 服务。
func (a *App) Iscsi() *IscsiService { return a.iscsi }

// Leases 返回租约服务。
func (a *App) Leases() *LeaseService { return a.lease }

// Auth 返回认证服务。
func (a *App) Auth() *AuthService { return a.auth }

// Users 返回用户服务。
func (a *App) Users() *UserService { return a.user }

// Uploads 返回分块上传服务。
func (a *App) Uploads() *UploadService { return a.upload }

// Storages 返回存储服务。
func (a *App) Storages() *StorageService { return a.storage }

// FS 返回服务端本地目录服务。
func (a *App) FS() *FSService { return a.fs }

// Pools 返回存储池（LVM thin pool / dm-cache）管理服务。
func (a *App) Pools() *PoolService { return a.pool }

// cfg 返回当前生效的配置快照。
func (d Deps) cfg() *config.Loaded {
	if d.Cfg == nil {
		return nil
	}
	return d.Cfg.Current()
}

// raw 返回当前配置原值；无配置时返回零值。
func (d Deps) raw() config.Config {
	if l := d.cfg(); l != nil {
		return l.Raw
	}
	return config.Config{}
}

// iqnPrefix 返回目标 IQN 前缀（完整 IQN = <prefix>:<target_name>）。
//
// 取自配置 platform.iscsi.iqn_prefix；未配置时回退到 defaultIQNPrefix，
// 保证"空配置也能算出合法 IQN"。尾部 ':' 会被去掉，避免拼出 "iqn.x::name"。
func (d Deps) iqnPrefix() string {
	p := strings.TrimSuffix(strings.TrimSpace(d.raw().Platform.Iscsi.IQNPrefix), ":")
	if p == "" {
		return defaultIQNPrefix
	}
	return p
}

// listStorages 读取全部存储记录（DB 为唯一真源）。
//
// 未装配 Store 时返回空列表（用于单元测试等场景，此时放置逻辑会回退配置种子根）。
func (d Deps) listStorages(ctx context.Context) ([]domain.Storage, error) {
	if d.Store == nil {
		return nil, nil
	}
	return d.Store.ListStorages(ctx)
}

// storageSelection 返回当前可用于放置新磁盘的根，以及其对应的存储实体。
//
// 语义（DB 为唯一真源）：
//   - DB 有存储记录 → 仅取 Enabled 的存储路径（storages 与之对应）；
//   - DB 没有任何存储记录（例如首次种子失败）→ 回退配置的种子根（storages 为空），
//     保证老部署不会立刻失效；
//   - 读取 DB 失败 → 返回错误（**绝不静默使用空根集**，否则会表现为"无处可放"）。
//
// fail-closed：底层卷未成功挂载的存储会被剔除（见 storageMountReady）。这是因为
// Linux 上存储是"格式化后挂载到 storages.path 的 thin LV"，若挂载没生效，写入会
// 静默落在宿主根文件系统上；宁可暂时"无处可放"也不能写错地方。
//
// 性能说明：每次调用查一次 storages 小表（不做进程内缓存，避免 CRUD 后缓存失效的复杂度）；
// 该表记录数为个位数，开销可忽略。
func (d Deps) storageSelection(ctx context.Context) (roots []string, storages []domain.Storage, err error) {
	all, err := d.listStorages(ctx)
	if err != nil {
		return nil, nil, err
	}
	if len(all) == 0 {
		return d.raw().Storage.EffectiveRoots(), nil, nil
	}
	roots = make([]string, 0, len(all))
	for i := range all {
		if !all[i].Enabled {
			continue
		}
		if !d.storageMountReady(all[i].ID) {
			continue
		}
		roots = append(roots, all[i].Path)
		storages = append(storages, all[i])
	}
	return roots, storages, nil
}

// storageMountReady 返回某存储的底层卷是否已就绪挂载。
//
// 未装配存储卷后端（Windows）或未装配就绪快照时一律放行——那意味着"存储就是普通目录"，
// 不存在"挂载没生效"的风险。
func (d Deps) storageMountReady(id string) bool {
	if d.StorageVol == nil || d.Mounted == nil {
		return true
	}
	return d.Mounted.Ready(id)
}

// storageRoots 返回当前生效的放置根（见 storageSelection）。
func (d Deps) storageRoots(ctx context.Context) ([]string, error) {
	roots, _, err := d.storageSelection(ctx)
	return roots, err
}

// storageGuards 返回当前生效的多根路径守卫（以 DB 存储为准，见 storageSelection）。
func (d Deps) storageGuards(ctx context.Context) (domain.PathGuardSet, error) {
	roots, err := d.storageRoots(ctx)
	if err != nil {
		return domain.PathGuardSet{}, err
	}
	return domain.NewPathGuardSet(roots), nil
}

// volumeProvider 返回卷空间查询器；未装配卷管理器时返回真正的 nil（而非 typed nil）。
func (d Deps) volumeProvider() domain.VolumeSpaceProvider {
	if d.Vol == nil {
		return nil
	}
	return d.Vol
}

// platformKind 返回当前平台种类；未装配磁盘后端时返回空串。
func (d Deps) platformKind() platform.Kind {
	if d.Disk == nil {
		return ""
	}
	return d.Disk.Kind()
}

// compat 返回当前解析后的客户端兼容区间。
func (d Deps) compat() config.ResolvedCompat {
	if l := d.cfg(); l != nil {
		return l.Compat
	}
	return config.ResolvedCompat{}
}

// RegisterJobHandlers 把磁盘、iSCSI 与上传相关任务注册到 worker。
//
// 由 main 在启动阶段调用，保证 worker 启动前处理器已就绪。
func (a *App) RegisterJobHandlers() {
	a.disk.RegisterHandlers(a.Jobs)
	a.iscsi.RegisterHandlers(a.Jobs)
	a.upload.RegisterHandlers(a.Jobs)
}

// Reconcile 执行一次对账（服务端启动后与定时触发，见 docs/implementation.md 5.11）。
//
// 覆盖检查项：
//   - 任务恢复：进程重启后 running 的任务退回 pending；
//   - iSCSI 目标期望状态与实际状态收敛；
//   - 母盘临时共享的崩溃恢复（5.6 步骤 ④）；
//   - 存储底层卷幂等重挂（Linux thin LV；挂载持久化完全靠这一步）；
//   - 孤儿 iSCSI 目标（只报告）、孤儿 VHDX（移入 meta/orphan）、母盘指纹校验、
//     差异盘 parent_version 比对、悬空分配等（详见 reconcile.go）。
//
// 危险操作默认只报告；单次运行受 60s 预算约束，超时截断并在报告中标注。
func (a *App) Reconcile(ctx context.Context) error {
	return a.runReconcile(ctx, reconcileBudget)
}
