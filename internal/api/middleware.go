package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"vault/internal/app"
	"vault/internal/apperr"
	"vault/internal/domain"
)

type ctxKey int

const (
	ctxRequestID ctxKey = iota
	ctxRawToken
)

// requestIDHeader 是请求追踪头，客户端可透传，服务端缺失时生成。
const requestIDHeader = "X-Request-Id"

// requestIDFrom 从 context 取出请求 ID。
func requestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxRequestID).(string); ok {
		return v
	}
	return ""
}

// rawTokenFrom 取出本次请求使用的 Bearer 令牌（登出时使用）。
func rawTokenFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxRawToken).(string); ok {
		return v
	}
	return ""
}

// middlewareRequestID 生成/透传 X-Request-Id 并注入 context。
func middlewareRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		id := strings.TrimSpace(req.Header.Get(requestIDHeader))
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), ctxRequestID, id)))
	})
}

// newRequestID 生成 16 字节随机 ID（十六进制）。
func newRequestID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "req-unknown"
	}
	return hex.EncodeToString(buf[:])
}

// statusRecorder 记录响应状态码与字节数，供访问日志与审计使用。
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(p)
	s.bytes += n
	return n, err
}

// Flush 透传 Flush（SSE 需要）。
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// middlewareRecover panic 兜底：记录堆栈、落一条审计（让 panic 在审计/日志页可见）并返回 500。
func (r *Router) middlewareRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				stack := string(debug.Stack())
				r.deps.Log.Error("请求处理发生 panic",
					"request_id", requestIDFrom(req.Context()),
					"method", req.Method, "path", req.URL.Path,
					"panic", rec, "stack", stack)
				r.auditPanic(req, rec, stack)
				// 用已写入的响应无法回退，这里只在尚未写头时输出统一错误。
				r.writeError(w, req, apperr.New(apperr.CodeInternal, http.StatusInternalServerError))
			}
		}()
		next.ServeHTTP(w, req)
	})
}

// auditPanic 把一次请求级 panic 落进审计表。
//
// 为什么必须在这里落库：middlewareAudit 位于本中间件的**内侧**，panic 会向上穿过它，
// 其 "next.ServeHTTP 之后写审计" 的代码永远不会执行，导致 panic 请求在审计页完全不可见
// （只有日志文件里有）——这正是"创建磁盘出问题时日志页看不到"的根因。
//
// 注：authn 同样在内层，因此这里取不到 Principal，user_id 只能是空串；
// 定位靠 action=system.panic + resource(路由模板) + detail 中的 request_id。
func (r *Router) auditPanic(req *http.Request, panicValue any, stack string) {
	if r.deps.App == nil {
		return
	}
	userID := ""
	if p, ok := app.PrincipalFrom(req.Context()); ok {
		userID = p.UserID
	}
	r.deps.App.AuditWithIP(req.Context(), userID, "system.panic", routePattern(req),
		"panic="+headLines(fmt.Sprint(panicValue), 1)+
			" request_id="+requestIDFrom(req.Context())+
			" stack="+headLines(stack, 3),
		clientIP(req), domain.AuditResultError)
}

// headLines 取文本前 n 行并拼接，用于把可能很长的 panic 值/堆栈折叠成适合落库的摘要。
//
// 完整堆栈已经写进日志文件，审计里只保留足以定位问题的头部，避免把审计表撑爆。
func headLines(s string, n int) string {
	if s == "" {
		return ""
	}
	lines := strings.SplitN(strings.TrimRight(s, "\n"), "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, " | ")
}

// middlewareAccessLog 记录 method / path / status / duration / request_id。
func (r *Router) middlewareAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(rec, req)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		r.deps.Log.Info("http",
			"request_id", requestIDFrom(req.Context()),
			"method", req.Method,
			"path", req.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"bytes", rec.bytes,
			"ip", clientIP(req),
		)
	})
}

// middlewareAudit 对所有写操作落审计（调用 store.AppendAudit）。
//
// 要求位于 authn 之后，才能拿到 Principal。
func (r *Router) middlewareAudit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, req)

		if !isMutating(req.Method) {
			return
		}
		result := domain.AuditResultOK
		switch {
		case rec.status >= 500:
			result = domain.AuditResultError
		case rec.status >= 400:
			result = domain.AuditResultDenied
		}
		userID := ""
		if p, ok := app.PrincipalFrom(req.Context()); ok {
			userID = p.UserID
		}
		pattern := routePattern(req)
		r.deps.App.AuditWithIP(req.Context(), userID, "http."+strings.ToLower(req.Method), pattern,
			"status="+itoa(rec.status), clientIP(req), result)
	})
}

// isMutating 判断是否为写操作。
func isMutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// middlewareAuthn 解析调用者身份：① Bearer 会话令牌 ② mTLS 客户端证书。
func (r *Router) middlewareAuthn(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx := req.Context()

		if token := bearerToken(req); token != "" {
			if sessions := r.deps.App.Sessions; sessions != nil {
				if sess, ok := sessions.Get(token); ok {
					ctx = app.WithPrincipal(ctx, &app.Principal{
						UserID:     sess.UserID,
						Username:   sess.Username,
						Role:       sess.Role,
						AuthMethod: "password",
					})
					ctx = context.WithValue(ctx, ctxRawToken, token)
					next.ServeHTTP(w, req.WithContext(ctx))
					return
				}
			}
		}

		if req.TLS != nil && len(req.TLS.PeerCertificates) > 0 {
			// 把 DER 还原为 PEM 后走统一的证书认证链（签名、有效期、登记状态、绑定 IP）。
			der := req.TLS.PeerCertificates[0].Raw
			certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
			principal, err := r.deps.App.Auth().AuthenticateCert(ctx, certPEM, clientIP(req))
			if err != nil {
				r.writeError(w, req, err)
				return
			}
			next.ServeHTTP(w, req.WithContext(app.WithPrincipal(ctx, principal)))
			return
		}

		r.writeError(w, req, apperr.AuthRequired())
	})
}

// bearerToken 提取 Authorization: Bearer <token>。
func bearerToken(req *http.Request) string {
	raw := strings.TrimSpace(req.Header.Get("Authorization"))
	if raw == "" {
		return ""
	}
	const prefix = "Bearer "
	if len(raw) <= len(prefix) || !strings.EqualFold(raw[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(raw[len(prefix):])
}

// ---- 限流 ----

// rateLimiter 是简单的令牌桶限流器（全局 + 按 IP）。
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64
	burst   float64
}

type bucket struct {
	tokens float64
	last   time.Time
}

// newRateLimiter 构造限流器：每秒补充 rate 个令牌，桶容量 burst。
func newRateLimiter(rate float64, burst int) *rateLimiter {
	if rate <= 0 {
		rate = 20
	}
	if burst <= 0 {
		burst = 40
	}
	return &rateLimiter{buckets: make(map[string]*bucket), rate: rate, burst: float64(burst)}
}

// allow 判断某个键是否允许通过。
func (l *rateLimiter) allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		// 顺带做一次粗粒度清理，避免 map 无限增长。
		if len(l.buckets) > 4096 {
			for k, v := range l.buckets {
				if now.Sub(v.last) > 10*time.Minute {
					delete(l.buckets, k)
				}
			}
		}
		l.buckets[key] = &bucket{tokens: l.burst - 1, last: now}
		return true
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// middlewareRateLimit 全局 + 按 IP 的限流。
func (r *Router) middlewareRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !r.globalLimiter.allow("global") || !r.ipLimiter.allow(clientIP(req)) {
			w.Header().Set("Retry-After", "1")
			r.writeError(w, req, apperr.New("system.rate_limited", http.StatusTooManyRequests))
			return
		}
		next.ServeHTTP(w, req)
	})
}

// middlewareLoginRateLimit 对登录/注册类接口使用更严格的限流。
func (r *Router) middlewareLoginRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !r.loginLimiter.allow(clientIP(req)) {
			w.Header().Set("Retry-After", "5")
			r.writeError(w, req, apperr.New("system.rate_limited", http.StatusTooManyRequests))
			return
		}
		next.ServeHTTP(w, req)
	})
}

// middlewareUploadRateLimit 对分块上传使用独立的令牌桶限流。
//
// 比普通接口更宽松（允许 4 路并发分块的突发），但仍有上限，避免单 IP 打满磁盘或连接。
func (r *Router) middlewareUploadRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !r.uploadLimiter.allow(clientIP(req)) {
			w.Header().Set("Retry-After", "1")
			r.writeError(w, req, apperr.New("system.rate_limited", http.StatusTooManyRequests))
			return
		}
		next.ServeHTTP(w, req)
	})
}

// itoa 是极简整数转字符串，避免为少量格式化引入额外依赖。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
