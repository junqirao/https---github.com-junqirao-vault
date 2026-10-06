package api

import (
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"vault/internal/apperr"
)

// LogSource 是服务端日志文件的只读访问接口（logging.Logger 天然满足）。
//
// 为什么要有它：管理端的「服务日志」页要看的是**服务端自己**的日志。客户端（本地代理）
// 日志走 GET /agent/log —— 那条路径只有本机能读，管理员在别的机器上打开管理控制台时
// 读到的只会是自己这台客户端的日志，与服务端发生了什么毫无关系。
//
// 日志按天切分（logging 的 dailyRotator），因此查询维度是"日期"而不是行号/偏移：
// 界面先列出可查日期，再取其中一天的尾部。
type LogSource interface {
	// Days 返回存在日志文件的日期（YYYY-MM-DD，升序）。
	Days() []string
	// PathForDay 返回指定日期日志文件的完整路径（可能为空）；day 为空表示当天。
	PathForDay(day string) string
	// ReadDay 读取指定日期日志文件末尾最多 maxBytes 字节；day 为空表示当天。
	ReadDay(day string, maxBytes int64) ([]byte, error)
}

// systemLogResponse 是服务端日志查询响应。
type systemLogResponse struct {
	// Day 本次返回的日期（YYYY-MM-DD）；请求未指定 day 时即当天。
	Day string `json:"day"`
	// Days 存在日志文件的日期（YYYY-MM-DD，升序），供界面做按时间切分的选择。
	Days []string `json:"days"`
	// Path 该日期日志文件路径（可能为空）。
	Path string `json:"path"`
	// Text 日志文件末尾的原始文本（JSON Lines，slog 输出）。
	Text string `json:"text"`
	// Error 读取失败时的稳定错误码（该天没有日志属于正常情况，不回错误）。
	Error string `json:"error,omitempty"`
}

// handleSystemLogs 返回服务端日志（管理端「服务日志」用，仅超级管理员）。
//
// 与本机代理的 GET /agent/log 同形（同样按天切分、同样只回尾部），差别在于读取的是
// 服务端进程自己的日志文件，并且必须经过认证与超管校验 —— 日志里含内部实现细节，
// 只对管理员开放。
//
// 可选查询参数：
//   - day=YYYY-MM-DD 指定日期（日志按天切分；省略或非法一律按"当天"处理，避免参数错误
//     直接打断界面）；
//   - tail=<字节数>   读取该文件末尾的字节数（默认 256KiB，上限 8MiB）。
func (r *Router) handleSystemLogs(w http.ResponseWriter, req *http.Request) {
	const (
		defaultTail = 256 << 10
		maxTail     = 8 << 20
	)
	tail := int64(defaultTail)
	if v := strings.TrimSpace(req.URL.Query().Get("tail")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			tail = min(n, maxTail)
		}
	}
	day := normalizeSystemLogDay(req.URL.Query().Get("day"))
	resp := systemLogResponse{Day: day, Days: []string{}}
	if r.deps.Logs != nil {
		resp.Days = r.deps.Logs.Days()
		resp.Path = r.deps.Logs.PathForDay(day)
		// 该日期没有日志文件不算错误（当天刚开始、或该天没写过日志都是正常状态），
		// 回空文本让界面显示空态；只有真的读不动才回错误码。
		if data, err := r.deps.Logs.ReadDay(day, tail); err == nil {
			resp.Text = string(data)
		} else if !errors.Is(err, os.ErrNotExist) {
			resp.Error = apperr.CodeOf(err)
		}
	}
	r.writeJSON(w, http.StatusOK, resp)
}

// normalizeSystemLogDay 把 day 参数规范成 YYYY-MM-DD；省略或非法时回退为当天。
func normalizeSystemLogDay(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Now().Format("2006-01-02")
	}
	if _, err := time.ParseInLocation("2006-01-02", raw, time.Local); err != nil {
		return time.Now().Format("2006-01-02")
	}
	return raw
}
