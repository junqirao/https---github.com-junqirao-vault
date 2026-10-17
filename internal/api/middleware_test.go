package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestIsQuietTransfer 覆盖"分块上传成功不记日志、失败照记"的边界。
//
// 这里刻意走**真实路由**：chi 只在实际匹配出结果后才会往请求上下文里写路由模板，
// 所以用例同时锁住了"uploadChunkRoute 这个模板名写对了没有"。模板一旦改名（或路由改路径），
// 分块上传会重新开始逐块刷屏 —— 而这类回归在测试之外几乎发现不了：日志多了少了都不报错，
// 只有日志页被冲满时才看得出来（真实反馈："上传时不要那么多日志，会刷屏的"）。
func TestIsQuietTransfer(t *testing.T) {
	cases := []struct {
		name   string
		method string
		route  string
		path   string
		status int
		want   bool
	}{
		{
			name:   "分块上传成功：不记",
			method: http.MethodPut,
			route:  "/v1/uploads/{id}/chunks/{index}",
			path:   "/v1/uploads/u1/chunks/12",
			status: http.StatusOK,
			want:   true,
		},
		{
			name:   "分块上传 2xx 的其它写法（204）：不记",
			method: http.MethodPut,
			route:  "/v1/uploads/{id}/chunks/{index}",
			path:   "/v1/uploads/u1/chunks/0",
			status: http.StatusNoContent,
			want:   true,
		},
		{
			name:   "分块上传失败：照记（搬运出错正是要留证据的现场）",
			method: http.MethodPut,
			route:  "/v1/uploads/{id}/chunks/{index}",
			path:   "/v1/uploads/u1/chunks/12",
			status: http.StatusInternalServerError,
			want:   false,
		},
		{
			name:   "分块上传被限流：照记",
			method: http.MethodPut,
			route:  "/v1/uploads/{id}/chunks/{index}",
			path:   "/v1/uploads/u1/chunks/12",
			status: http.StatusTooManyRequests,
			want:   false,
		},
		{
			name:   "完成建库：低频且是真事件，照记",
			method: http.MethodPost,
			route:  "/v1/uploads/{id}/complete",
			path:   "/v1/uploads/u1/complete",
			status: http.StatusOK,
			want:   false,
		},
		{
			name:   "创建上传会话：低频且是真事件，照记",
			method: http.MethodPost,
			route:  "/v1/uploads",
			path:   "/v1/uploads",
			status: http.StatusOK,
			want:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router := chi.NewRouter()
			hit := false
			got := false
			router.Method(tc.method, tc.route, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				hit = true
				got = isQuietTransfer(req, tc.status)
				w.WriteHeader(http.StatusNoContent)
			}))

			router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(tc.method, tc.path, nil))

			if !hit {
				t.Fatalf("路由未命中（用例本身写错了）：%s %s", tc.method, tc.path)
			}
			if got != tc.want {
				t.Fatalf("isQuietTransfer=%v，期望 %v", got, tc.want)
			}
		})
	}
}
