package agent

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"vault/internal/apperr"
)

// 本地目录扫描的上限（见 docs/agent-api.md「本地目录扫描与上传建库」）。
//
// 之所以设限：服务端 manifest 与后续分块上传都按此规模规划内存/时间，
// 超大目录应在扫描阶段就明确拒绝，而不是拖垮代理或把请求挂死。
const (
	// scanMaxFiles 允许的常规文件数上限。
	scanMaxFiles = 200000
	// scanMaxBytes 允许的总字节上限（2 TiB）。
	scanMaxBytes = int64(2) << 40
)

// fsScanRequest 是 POST /agent/fs/scan 的请求体。
type fsScanRequest struct {
	Path string `json:"path"`
}

// scanEntry 是一个被扫描到的常规文件。
type scanEntry struct {
	// Abs 本地绝对路径（仅代理内部使用，绝不出现在响应中）。
	Abs string
	// Rel 相对源目录的路径（斜杠分隔），即 manifest 中的 path。
	Rel string
	// Size / Mtime 文件大小与修改时间（毫秒）。
	Size  int64
	Mtime int64
}

// scanResult 是一次目录扫描的完整结果（文件清单只在代理内部使用）。
type scanResult struct {
	Root       string
	Entries    []scanEntry
	TotalFiles int
	TotalBytes int64
	Warnings   []string
}

// handleFSScan 扫描本地目录，返回文件数/总字节与告警（不返回完整清单）。
func (a *Agent) handleFSScan(w http.ResponseWriter, r *http.Request) {
	var req fsScanRequest
	if err := decodeJSON(r, &req); err != nil {
		a.writeError(w, err)
		return
	}
	res, err := scanLocalDir(req.Path)
	if err != nil {
		a.writeError(w, err)
		return
	}
	// warnings 必须始终是数组：Go 的 nil 切片会被序列化成 null，
	// 而前端会直接读 `.length`，null 会导致渲染期抛异常（整页白屏）。
	warnings := res.Warnings
	if warnings == nil {
		warnings = []string{}
	}
	a.writeJSON(w, http.StatusOK, map[string]any{
		"path":        res.Root,
		"file_count":  res.TotalFiles,
		"total_bytes": res.TotalBytes,
		"warnings":    warnings,
	})
}

// scanLocalDir 递归枚举目录下的常规文件：
//   - 只收常规文件（跳过目录项与设备/管道等特殊文件）；
//   - 遇符号链接记入 warnings 并跳过（绝不跟随，也不因此失败）；
//   - 空目录（无常规文件）返回 agent.scan_empty（服务端不接受空 manifest）；
//   - 文件数或总字节超阈值返回 agent.scan_too_large。
//
// 结果按 Rel 升序排序，该顺序即后续分块上传的字节流顺序（稳定且可复现）。
func scanLocalDir(raw string) (*scanResult, error) {
	root, err := validateAbsDir(raw, "path")
	if err != nil {
		return nil, err
	}
	root = filepath.Clean(root)
	info, err := os.Lstat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, apperr.InvalidParam("path")
		}
		return nil, errScanFailed("stat", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, apperr.InvalidParam("path")
	}

	res := &scanResult{Root: root}
	tooLarge := false
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			// 不可读的子树：记为告警并跳过，尽量给出可用结果。
			res.Warnings = append(res.Warnings, "unreadable:"+relSlash(root, p))
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			res.Warnings = append(res.Warnings, "symlink_skipped:"+relSlash(root, p))
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if res.TotalFiles >= scanMaxFiles || res.TotalBytes > scanMaxBytes {
			tooLarge = true
			return filepath.SkipAll
		}
		fi, iErr := d.Info()
		if iErr != nil {
			res.Warnings = append(res.Warnings, "stat_skipped:"+relSlash(root, p))
			return nil
		}
		res.Entries = append(res.Entries, scanEntry{
			Abs:   p,
			Rel:   relSlash(root, p),
			Size:  fi.Size(),
			Mtime: fi.ModTime().UnixMilli(),
		})
		res.TotalFiles++
		res.TotalBytes += fi.Size()
		return nil
	})
	if walkErr != nil {
		return nil, errScanFailed("walk", walkErr)
	}
	if tooLarge {
		return nil, errScanTooLarge(res.TotalFiles, res.TotalBytes)
	}
	if res.TotalFiles == 0 {
		return nil, errScanEmpty()
	}
	sort.SliceStable(res.Entries, func(i, j int) bool { return res.Entries[i].Rel < res.Entries[j].Rel })
	return res, nil
}

// relSlash 返回 p 相对 root 的斜杠分隔路径（失败时退回原路径）。
func relSlash(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(rel)
}

// uploadPlanFile 描述某个文件在拼接流中的全局偏移。
type uploadPlanFile struct {
	Abs    string
	Offset int64
	Size   int64
}

// buildUploadPlan 按扫描顺序（Rel 升序）生成"拼接流 → 文件"的偏移计划。
//
// 空文件不占字节，从计划中排除（但仍保留在 manifest 中，由服务端创建空文件）。
func buildUploadPlan(entries []scanEntry) []uploadPlanFile {
	plan := make([]uploadPlanFile, 0, len(entries))
	var off int64
	for _, e := range entries {
		if e.Size > 0 {
			plan = append(plan, uploadPlanFile{Abs: e.Abs, Offset: off, Size: e.Size})
		}
		off += e.Size
	}
	return plan
}

// sanitizeScanRootBase 返回用于 manifest.root 的展示名（避免把客户端完整本地路径上送）。
func sanitizeScanRootBase(root string) string {
	base := strings.Trim(filepath.Base(filepath.Clean(root)), `\/`)
	if base == "" || base == "." || base == string(filepath.Separator) {
		if v := filepath.VolumeName(root); v != "" {
			return strings.TrimRight(v, ":")
		}
		return "root"
	}
	return base
}
