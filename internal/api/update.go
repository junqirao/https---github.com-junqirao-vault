package api

import (
	"net/http"
	"os"
	"strconv"

	"github.com/go-chi/chi/v5"

	"vault/internal/apperr"
)

// 更新分发接口（见 docs/implementation.md 7.4.2）。
//
// ★ 三个接口都**匿名可访问**：客户端必须能在登录（甚至拿到证书）之前检查更新——
// 否则"版本过低"与"认证失败"会被混在一起，排障困难；且强制升级场景下用户可能
// 根本登录不上。这里是刻意的取舍：更新分发的信任根是**发布方签名**，而不是会话认证——
// 即使匿名拿到清单与产物，缺少发布方私钥也无法伪造更新。
// 代价是更新清单与安装包可被匿名读取（它们是公开的发布物，不含任何租户数据），
// 因此有一条明确约束：**发布目录里不得出现签名私钥或任何私有元数据**。
//
// 服务端在此**只做 mirror**：读文件、原样返回字节、支持 Range 断点续传。
// 不解析、不改写、不校验（哪怕"补一个字段"都会让客户端验签失败）。

// handleUpdateManifest 原样返回发布目录中的清单字节。
func (r *Router) handleUpdateManifest(w http.ResponseWriter, req *http.Request) {
	data, err := r.deps.App.UpdateManifestBytes(queryString(req, "channel"))
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	// 必须逐字节原样返回：客户端要对这些字节验签。
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(data); err != nil {
		r.deps.Log.Warn("写更新清单响应失败", "error", err)
	}
}

// handleUpdateSignature 原样返回清单签名字节（base64 文本）。
func (r *Router) handleUpdateSignature(w http.ResponseWriter, req *http.Request) {
	data, err := r.deps.App.UpdateSignatureBytes()
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(data); err != nil {
		r.deps.Log.Warn("写更新签名响应失败", "error", err)
	}
}

// handleUpdateArtifact 分发更新产物，支持 Range（断点续传）。
//
// 文件名由 app 层做白名单校验（拒绝路径分隔符与 ".."），再用 http.ServeContent
// 交给标准库处理 Range / Content-Length / 206 / 416 等细节。
func (r *Router) handleUpdateArtifact(w http.ResponseWriter, req *http.Request) {
	path, err := r.deps.App.UpdateArtifactPath(chi.URLParam(req, "filename"))
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		r.deps.Log.Warn("打开更新产物失败", "path", path, "error", err)
		r.writeError(w, req, apperr.New(apperr.CodeNotFound, http.StatusNotFound))
		return
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		r.writeError(w, req, apperr.New(apperr.CodeNotFound, http.StatusNotFound))
		return
	}
	// 产物是二进制：不设置 Content-Type 时由扩展名推断，可能得到意外的类型（如 text/plain），
	// 因此显式声明为通用二进制流。
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, req, info.Name(), info.ModTime(), f)
}
