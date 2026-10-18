package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"time"

	"vault/internal/domain"
)

// 母盘元数据 sidecar：与 .vhdx **同目录同名**（`base.vhdx` → `base.vhdx.meta.json`）。
//
// 为什么需要它（真实诉求）：母盘在本系统里是一条数据库记录，一旦离开这台机器
// （管理员把整个 disks/ 目录搬到另一台服务端），它就只剩"一个大小"——谁也不知道
// 哪个文件是财务的 Win10 母盘、哪个是上季度的测试环境。于是迁移后只能靠人回忆，
// 或者干脆放弃这些文件重新建库。
//
// 因此每次建盘都在盘旁边写一份 sidecar，由管理端「导入磁盘」读它**原地接管**
// （不复制、不移动文件）。sidecar 是纯文本、可读、可手写、可随文件一起拷走 ——
// 这正是"元数据跟着盘走"需要的形式。
//
// 为什么是"同名 sidecar"而不是目录级的 meta.json：
//  1. 导入是按**文件**发生的（一个候选 = 一个 .vhdx），元数据必须一一对应，
//     目录级单文件在"一个目录里有多块盘"时会错配；
//  2. 孤儿清理、手工拷贝、拖拽移动都以文件为单位，跟着文件走才不会脱节。
//
// 与数据库的关系：sidecar **不是真源**（真源仍是数据库），它只是一份"出厂标签"。
// 因此写入失败只告警、绝不让建盘任务失败；导入时也只在数据库里没有这条记录时参考它。
const (
	// diskMetaSuffix sidecar 文件名后缀（接在 `.vhdx` 之后）。
	diskMetaSuffix = ".meta.json"
	// DiskMetaSchema 当前 sidecar 格式版本。
	//
	// 读到**更高**版本的 sidecar 时不报错（旧服务端应尽量读懂新标签的已知字段），
	// 只在日志里记一条，交由导入结果里提示"元数据由更新版本写入"。
	DiskMetaSchema = 1
	// diskMetaApp 写入者标识（人读，用于判断这标签是谁写的）。
	diskMetaApp = "vault-server"
)

// sidecar 里记录的"这块盘是怎么来的"（用户口中的"自定义源信息"）。
const (
	// DiskMetaSourceEmpty 空盘建库。
	DiskMetaSourceEmpty = "empty"
	// DiskMetaSourceDir 从服务端目录拷入建库。
	DiskMetaSourceDir = "dir"
	// DiskMetaSourceImport 由管理端「导入磁盘」原地接管（导入后回写，标记来历）。
	DiskMetaSourceImport = "import"
)

// DiskMetaSource 记录母盘的来源信息。
//
// 迁移场景里最有用的是 Dir：看到它就知道"这盘是用哪个目录建的"，
// 从而在没有别的手段时也能推断里面装的是什么（用户明确要的"目录名等内容"）。
type DiskMetaSource struct {
	// Kind empty / dir / import（见 DiskMetaSource* 常量）。
	Kind string `json:"kind,omitempty"`
	// Dir 建库时使用的服务端源目录绝对路径（空盘为空）。**本机路径**，
	// 换机器后可能不存在，保留原文只为追溯"内容是什么"。
	Dir string `json:"dir,omitempty"`
	// Group 建库时设置的库分组标签（RepoMeta.Group），供归类展示。
	Group string `json:"group,omitempty"`
	// RepoID 生成它的那台服务端上的存储库 ID。迁移后新库会得到新 ID，
	// 这里保留原值以便与老机器的记录/日志对照。
	RepoID string `json:"repo_id,omitempty"`
	// Host 生成它的机器名（迁移后一眼看出"这盘是从哪台机器来的"）。
	Host string `json:"host,omitempty"`
	// At 建库时间（毫秒）。
	At int64 `json:"at,omitempty"`
}

// DiskMetaFile 是一个母盘 sidecar 的内容。
//
// 字段刻意都是"人/别的机器能直接读懂"的裸值（没有内部枚举、没有外键），
// 因为这个文件的主战场是"数据库不在身边"的时候。
type DiskMetaFile struct {
	// Schema 格式版本（见 DiskMetaSchema）。
	Schema int `json:"schema"`
	// App 写入者（恒为 vault-server，便于人识别这标签的来历）。
	App string `json:"app,omitempty"`
	// Name 存储库名称 —— 迁移后要恢复的首要信息（"财务的 Win10 母盘"）。
	Name string `json:"name,omitempty"`
	// Note 说明备注（用户自定义的长文本）。
	Note string `json:"note,omitempty"`
	// RepoID / Mode / Kind 原记录的关键属性。
	RepoID string `json:"repo_id,omitempty"`
	Mode   string `json:"mode,omitempty"`
	Kind   string `json:"kind,omitempty"`
	// ClientOS 该盘面向的客户端操作系统（windows / linux）。
	//
	// 由建库时的选择决定（它已经写进了盘里的分区表，事后改不了），
	// 迁移后必须原样恢复，否则客户端挂载上来的盘可能格式化成了错的文件系统。
	ClientOS string `json:"client_os,omitempty"`
	// FileSystem 盘内分区上的文件系统（ntfs / ext4）。
	FileSystem string `json:"file_system,omitempty"`
	// CapacityBytes 盘的**标称容量**（VHDX VirtualSize / LV size）。
	//
	// ⚠️ 这是导入时最关键的字段：客户端按容量匹配本地 iSCSI 盘
	// （`[long]$_.Size -eq $Size`，见 winps/scripts/volume_find_iscsi_disk.ps1），
	// 容量差一个字节就找不到盘。没有它就只能按文件大小估，而 VHDX 是稀疏文件，
	// 估出来的值几乎必然偏小。
	CapacityBytes int64 `json:"capacity_bytes,omitempty"`
	// VHDType 磁盘类型（dynamic / fixed / differencing）。
	VHDType string `json:"vhd_type,omitempty"`
	// ParentVersion 母盘版本号（共享库每次维护 +1）。
	ParentVersion int `json:"parent_version,omitempty"`
	// MaxDiffDisks 共享库的差异盘上限（= 界面上的"共享数量"）。
	MaxDiffDisks int `json:"max_diff_disks,omitempty"`
	// Source 来源信息（见 DiskMetaSource）。
	Source *DiskMetaSource `json:"source,omitempty"`
	// WrittenAt / UpdatedAt 首次写入与最后更新时间（毫秒）。
	WrittenAt int64 `json:"written_at,omitempty"`
	UpdatedAt int64 `json:"updated_at,omitempty"`
}

// diskMetaPath 返回某块盘的 sidecar 路径。
func diskMetaPath(vhdxPath string) string { return vhdxPath + diskMetaSuffix }

// readDiskMetaFile 读取 sidecar。
//
// 三种结果刻意区分开，因为导入界面要分别呈现：
//   - (nil, nil)：**没有** sidecar —— 这是正常情况（老盘、别处来的盘），不是错误；
//   - (nil, err)：有 sidecar 但读不出来（JSON 坏了、权限不足）—— 界面要提示
//     "元数据存在但无法解析"，绝不静默当成"没有元数据"（那样用户会以为文件坏了）；
//   - (meta, nil)：读到了。
func readDiskMetaFile(vhdxPath string) (*DiskMetaFile, error) {
	data, err := os.ReadFile(diskMetaPath(vhdxPath))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var m DiskMetaFile
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// writeDiskMetaFile 原子地写 sidecar（临时文件 + rename）。
//
// 必须原子：sidecar 可能被"边写边读"（管理员在导入扫描时正好有任务在建盘），
// 直接覆盖会读到一个半截 JSON，而半截 JSON 在导入侧表现为"元数据无法解析"。
// rename 在同一目录内是原子的，读者要么看到旧内容、要么看到新内容。
func writeDiskMetaFile(vhdxPath string, m DiskMetaFile) error {
	m.Schema = DiskMetaSchema
	m.App = diskMetaApp
	if m.WrittenAt == 0 {
		m.WrittenAt = time.Now().UnixMilli()
	}
	m.UpdatedAt = time.Now().UnixMilli()

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	dst := diskMetaPath(vhdxPath)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// diskMetaForRepo 依据存储库与母盘记录拼出一份 sidecar 内容。
//
// 建盘完成与母盘维护（版本 +1）后都走这里：两处的字段来源完全一致，
// 分开写必然漂移（例如维护后忘了更新 ParentVersion，迁移回来的库版本就对不上）。
//
// src 表示来历：建盘时给出源目录信息；导入接管时给 DiskMetaSourceImport。
// 传 nil 表示"沿用盘上已有的来源信息"（维护场景：盘的来历没变）。
func diskMetaForRepo(repo *domain.Repository, disk *domain.Disk, src *DiskMetaSource, prev *DiskMetaFile) DiskMetaFile {
	m := DiskMetaFile{
		Name:          repo.Name,
		RepoID:        repo.ID,
		Mode:          string(repo.Mode),
		Kind:          string(disk.Kind),
		FileSystem:    disk.FileSystemOrDefault().String(),
		CapacityBytes: disk.SizeBytes,
		VHDType:       string(disk.VHDType),
		// 母盘版本：迁移后新库从 0 开始，这里保留原值供对照。
		ParentVersion: repo.ParentVersion,
		MaxDiffDisks:  repo.MaxDiffDisks,
	}
	m.ClientOS = clientOSOfFileSystem(disk.FileSystemOrDefault())
	// 备注存在 RepoMeta 里（见 domain.RepoMeta.Note）：迁移后它要跟着回来。
	m.Note = repo.Meta.Note
	// 来历：新的优先；否则沿用盘上原有（维护场景），最后的兜底是"空盘"——
	// 老盘可能连 sidecar 都没有，此时至少说明"我们不知道它是从哪来的"。
	switch {
	case src != nil:
		m.Source = src
	case prev != nil && prev.Source != nil:
		m.Source = prev.Source
	default:
		m.Source = &DiskMetaSource{Kind: DiskMetaSourceEmpty}
	}
	if m.Source.Host == "" {
		m.Source.Host = hostnameForMeta()
	}
	if prev != nil {
		m.WrittenAt = prev.WrittenAt
	}
	return m
}

// clientOSOfFileSystem 由文件系统反推客户端操作系统。
//
// 为什么不用"建库时用户选的那个值"：那个值只活在请求里，盘本身只留得下分区与文件系统
// （见 domain.Disk.FileSystem 的说明）。sidecar 是照着盘写的，所以从盘推才是一致的。
func clientOSOfFileSystem(fs domain.FileSystem) string {
	switch fs {
	case domain.FSExt4:
		return string(domain.ClientOSLinux)
	default:
		return string(domain.ClientOSWindows)
	}
}

// hostnameForMeta 取本机名（取不到就留空，绝不因此报错——这是标签，不是业务数据）。
func hostnameForMeta() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(h)
}

// removeDiskMetaFile 删除某块盘的 sidecar（文件本身被删时一并清掉）。
//
// 必须清：它描述的就是那个文件，文件没了标签只会变成"谁也认不出的孤儿 .meta.json"
// （孤儿扫描只认 .vhdx，留着它没有任何机制能再清理）。
func removeDiskMetaFile(log *slog.Logger, vhdxPath string) {
	p := diskMetaPath(vhdxPath)
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		log.Warn("删除母盘元数据文件失败（不影响磁盘删除本身）", "path", p, "error", err.Error())
	}
}

// writeDiskMetaAfterBuild 在盘建好后贴 sidecar（建盘流水线的收尾动作）。
//
// 只查一次库取仓库名/备注/分组：这些"人读"信息必须来自数据库（sidecar 不是真源）。
// 取不到就不写 —— 一份没有名称的标签只会误导迁移后的导入。
func (s *DiskService) writeDiskMetaAfterBuild(ctx context.Context, disk *domain.Disk, sourceDir string) {
	if disk == nil || s.Store == nil {
		return
	}
	repo, err := s.Store.GetRepository(ctx, disk.RepoID)
	if err != nil {
		s.Log.Warn("建盘后读取存储库失败，跳过写入元数据文件", "disk_id", disk.ID, "error", err.Error())
		return
	}
	kind := DiskMetaSourceEmpty
	if strings.TrimSpace(sourceDir) != "" {
		kind = DiskMetaSourceDir
	}
	writeDiskMetaForDisk(s.Log, repo, disk, &DiskMetaSource{
		Kind: kind,
		Dir:  strings.TrimSpace(sourceDir),
		At:   time.Now().UnixMilli(),
	})
} // writeDiskMetaForDisk 组装并写入 sidecar，失败只告警。
//
// 建盘/维护/导入调用它时**不得**因为 sidecar 写失败而失败：盘已经建好了，
// 把任务判失败会让一块完好的盘卡在重试链上。最坏后果只是"这个盘迁移时少一份标签"。
//
// 接收 logger 而不是 *App：建盘流程跑在 DiskService 里（那里没有 App），
// 而 sidecar 必须由建盘那一方写 —— 谁建盘谁贴标签，导入侧只负责读。
func writeDiskMetaForDisk(log *slog.Logger, repo *domain.Repository, disk *domain.Disk, src *DiskMetaSource) {
	if repo == nil || disk == nil || strings.TrimSpace(disk.VHDXPath) == "" {
		return
	}
	prev, err := readDiskMetaFile(disk.VHDXPath)
	if err != nil {
		// 读不出来（半截 JSON / 手工改坏）时不带 prev 继续写：**重写一份正确的**比留着坏文件好。
		log.Warn("母盘元数据文件无法解析，将重写", "path", diskMetaPath(disk.VHDXPath), "error", err.Error())
		prev = nil
	}
	meta := diskMetaForRepo(repo, disk, src, prev)
	if err := writeDiskMetaFile(disk.VHDXPath, meta); err != nil {
		log.Warn("写入母盘元数据文件失败（不影响磁盘本身，仅影响迁移时的可读性）",
			"path", diskMetaPath(disk.VHDXPath), "error", err.Error())
		return
	}
	log.Info("已写入母盘元数据文件", "path", diskMetaPath(disk.VHDXPath), "name", meta.Name)
}
