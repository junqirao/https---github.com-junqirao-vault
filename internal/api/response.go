// Package api 是 HTTP 接口层。
//
// 分层约定（见 docs/implementation.md 6.2 / 6.4）：
//   - 本层只做协议转换：解析请求 → 调用 app 层 → 序列化 DTO；
//   - 绝不直接序列化 domain 实体（避免 password_hash / chap_secret_enc / owner_token 外泄）；
//   - 失败一律返回 {"error":{"code":...,"args":{...}}}，HTTP 状态码取 apperr.HTTPStatus。
package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"vault/internal/apperr"
)

// maxRequestBodyBytes 限制请求体大小，避免被超大请求打满内存。
const maxRequestBodyBytes = 8 << 20

// errorDetail 是错误响应中的错误对象。
type errorDetail struct {
	Code string         `json:"code"`
	Args map[string]any `json:"args,omitempty"`
}

// errorBody 是统一的错误响应体。
type errorBody struct {
	Error errorDetail `json:"error"`
}

// writeJSON 输出成功响应。
func (r *Router) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		r.deps.Log.Warn("写响应失败", "error", err)
	}
}

// writeError 输出统一错误响应。
//
// 语义警告：只输出稳定的错误码与插值参数，**绝不输出 Cause 中的底层文本**
// （例如 PowerShell 原始报错），底层错误只进日志。
func (r *Router) writeError(w http.ResponseWriter, req *http.Request, err error) {
	status := apperr.HTTPStatus(err)
	body := errorBody{Error: errorDetail{Code: apperr.CodeOf(err)}}
	if e, ok := apperr.As(err); ok {
		body.Error.Args = e.Args
		r.deps.Log.Debug("请求返回业务错误",
			"method", req.Method, "path", req.URL.Path,
			"code", e.Code, "status", status, "cause", err.Error())
	} else {
		r.deps.Log.Error("请求处理失败",
			"method", req.Method, "path", req.URL.Path,
			"request_id", requestIDFrom(req.Context()), "error", err)
	}
	r.writeJSON(w, status, body)
}

// writeNoContent 输出 204。
func (r *Router) writeNoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

// apperrForbiddenReason 构造带原因的 403 业务错误。
func apperrForbiddenReason(reason string) error {
	return apperr.AuthForbidden().WithArg("reason", reason)
}

// apperrConflict 构造 409 业务错误。
func apperrConflict(code string) error {
	return apperr.New(code, http.StatusConflict)
}

// decodeJSON 解析请求体；空体视为零值结构。
func (r *Router) decodeJSON(req *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(req.Body, maxRequestBodyBytes))
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return apperr.InvalidParam("body").WithCause(err)
	}
	return nil
}

// queryInt 读取整型查询参数，非法或缺失时返回默认值。
func queryInt(req *http.Request, key string, def int) int {
	raw := strings.TrimSpace(req.URL.Query().Get(key))
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return v
}

// queryInt64 读取 64 位整型查询参数。
func queryInt64(req *http.Request, key string, def int64) int64 {
	raw := strings.TrimSpace(req.URL.Query().Get(key))
	if raw == "" {
		return def
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return def
	}
	return v
}

// queryString 读取字符串查询参数并去空白。
func queryString(req *http.Request, key string) string {
	return strings.TrimSpace(req.URL.Query().Get(key))
}

// clientIP 提取来源地址（不含端口）。
func clientIP(req *http.Request) string {
	addr := req.RemoteAddr
	if i := strings.LastIndex(addr, ":"); i > 0 {
		return strings.Trim(addr[:i], "[]")
	}
	return addr
}

// requestLogger 返回带请求上下文的日志器，便于串联排障。
func (r *Router) requestLogger(req *http.Request) *slog.Logger {
	return r.deps.Log.With(
		"request_id", requestIDFrom(req.Context()),
		"method", req.Method,
		"path", req.URL.Path,
	)
}
