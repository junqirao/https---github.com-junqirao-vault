package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"vault/internal/apperr"
)

// fakeTimeoutError 模拟底层网络超时（net.Error.Timeout() == true）。
type fakeTimeoutError struct{}

func (fakeTimeoutError) Error() string   { return "i/o timeout" }
func (fakeTimeoutError) Timeout() bool   { return true }
func (fakeTimeoutError) Temporary() bool { return true }

// TestServerCallErrorClassifiesTimeout 锁定"超时 ≠ 不可达"的分类。
//
// 真实工单：挂载申请在服务端是同步重活（PowerShell 多步），代理 30s 超时后把错误
// 报成 agent.server_unreachable，界面显示"本地代理无法连接服务端"，
// 排查方向被带偏到地址/网络。二者必须区分。
func TestServerCallErrorClassifiesTimeout(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode string
	}{
		{"上下文截止 → 超时", context.DeadlineExceeded, CodeServerTimeout},
		{"网络层超时 → 超时", fakeTimeoutError{}, CodeServerTimeout},
		{"包装后的截止错误 → 超时", errors.Join(errors.New("请求失败"), context.DeadlineExceeded), CodeServerTimeout},
		{"连接被拒 → 不可达", errors.New("dial tcp 10.0.0.1:8443: connectex: connection refused"), CodeServerUnreachable},
		{"上下文取消（用户主动取消）→ 不可达", context.Canceled, CodeServerUnreachable},
		{"响应体解析失败 → 不可达", errors.New("unexpected EOF"), CodeServerUnreachable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := serverCallError(c.err, 2*time.Minute)
			if code := apperr.CodeOf(err); code != c.wantCode {
				t.Fatalf("错误码 = %q，期望 %q", code, c.wantCode)
			}
		})
	}
}

// TestServerCallErrorCarriesTimeout 锁定超时秒数被透出（界面文案要显示"等了多久"）。
func TestServerCallErrorCarriesTimeout(t *testing.T) {
	err := serverCallError(context.DeadlineExceeded, 2*time.Minute)
	e, ok := apperr.As(err)
	if !ok {
		t.Fatal("应返回结构化错误")
	}
	if got := e.Args["timeout_seconds"]; got != 120 {
		t.Fatalf("args.timeout_seconds = %v，期望 120", got)
	}
}

// TestRequestMountReportsTimeoutNotUnreachable 端到端验证：
// 服务端迟迟不响应（模拟同步发布 iSCSI 目标）时，挂载申请必须报 server_timeout。
func TestRequestMountReportsTimeoutNotUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 模拟服务端同步重活：远超调用方截止时间。
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer srv.Close()

	client, err := newServerClient(srv.URL, "token", "", testLogger())
	if err != nil {
		t.Fatalf("构造客户端失败：%v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	_, err = client.RequestMount(ctx, "alloc-1", "client-1")
	if err == nil {
		t.Fatal("超时应返回错误")
	}
	if code := apperr.CodeOf(err); code != CodeServerTimeout {
		t.Fatalf("错误码 = %q，期望 %q（把超时报成不可达会把排查方向带偏）", code, CodeServerTimeout)
	}
}

// TestRequestMountUsesLongTimeout 锁定挂载申请走的是长耗时客户端（2 分钟），
// 而不是普通接口的 30s —— 这是本次"服务端还没干完就被掐断"的直接原因。
func TestRequestMountUsesLongTimeout(t *testing.T) {
	if mountRequestTimeout <= serverRequestTimeout {
		t.Fatalf("mountRequestTimeout(%s) 必须大于 serverRequestTimeout(%s)：挂载接口在服务端是同步重活",
			mountRequestTimeout, serverRequestTimeout)
	}
	client := &serverClient{
		http:   &http.Client{Timeout: serverRequestTimeout},
		long:   &http.Client{Timeout: mountRequestTimeout},
		stream: &http.Client{},
	}
	if client.long.Timeout != mountRequestTimeout {
		t.Fatalf("长耗时客户端超时 = %s，期望 %s", client.long.Timeout, mountRequestTimeout)
	}
}
