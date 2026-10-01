package agent

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"vault/internal/apperr"
)

// TestConnectDiagnosticsReportsReachablePortal 门户端口通时，诊断文本必须给出完整连接信息，
// 并把 TCP 结论标为 reachable —— 这样用户就能判定"网络没问题，问题在目标名/CHAP"。
func TestConnectDiagnosticsReportsReachablePortal(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起监听失败：%v", err)
	}
	defer func() { _ = listener.Close() }()
	port := listener.Addr().(*net.TCPAddr).Port

	spec := &MountSpec{
		PortalAddress: "127.0.0.1",
		PortalPort:    port,
		TargetIQN:     "iqn.2000-01.com.vault:test",
		AuthMode:      "chap",
		ChapUser:      "vault-abc",
		ChapSecret:    "secret-must-not-leak",
	}
	detail, tcp := connectDiagnostics(context.Background(), spec, port)

	if !strings.HasPrefix(tcp, portalTCPReachable) {
		t.Fatalf("tcp = %q，期望 reachable(...)", tcp)
	}
	for _, want := range []string{
		"portal=127.0.0.1:" + strconv.Itoa(port),
		"target=iqn.2000-01.com.vault:test",
		"auth=chap",
		"chap_user=vault-abc",
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("诊断文本缺少 %q：%q", want, detail)
		}
	}
	// 安全：CHAP 密钥绝不能进诊断文本（它会被写进状态、经 SSE 推送、并可被用户复制）。
	if strings.Contains(detail, spec.ChapSecret) {
		t.Fatalf("CHAP 密钥泄漏进了诊断文本：%q", detail)
	}
}

// TestConnectDiagnosticsReportsUnreachablePortal 端口不通时必须明确报出来。
//
// 这正是"连不上门户（网络/防火墙/门户地址派发错）"与"端口通但登录/认证失败"的分水岭，
// 也是用户排查时第一个要问的问题（真实反馈："根本没法定位错误"）。
func TestConnectDiagnosticsReportsUnreachablePortal(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起监听失败：%v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	// 关掉监听：该端口不再有人接。
	_ = listener.Close()

	spec := &MountSpec{PortalAddress: "127.0.0.1", PortalPort: port, TargetIQN: "iqn.test", AuthMode: "none"}
	start := time.Now()
	detail, tcp := connectDiagnostics(context.Background(), spec, port)

	if strings.HasPrefix(tcp, portalTCPReachable) {
		t.Fatalf("端口已关闭，不应判定为可达：%q", tcp)
	}
	if !strings.Contains(detail, "tcp=") {
		t.Fatalf("诊断文本缺少 tcp 结论：%q", detail)
	}
	if elapsed := time.Since(start); elapsed > portalProbeTimeout+2*time.Second {
		t.Fatalf("探测耗时 %s，超出预期：失败路径上不该让用户多等", elapsed)
	}
}

// TestConnectDiagnosticsToleratesCanceledContext 界面侧取消/超时后，探测仍必须给出结论。
//
// 探测刻意用 context.WithoutCancel：失败恰恰发生在 ctx 已被取消的时刻，此时若放弃探测，
// 用户看到的结论是"探测被取消"——等于没说。
func TestConnectDiagnosticsToleratesCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	spec := &MountSpec{PortalAddress: "127.0.0.1", PortalPort: 1, TargetIQN: "iqn.test", AuthMode: "none"}
	detail, tcp := connectDiagnostics(ctx, spec, 1)
	if tcp == "" {
		t.Fatal("即使 ctx 已取消，也必须给出 TCP 结论")
	}
	if !strings.Contains(detail, "tcp=") {
		t.Fatalf("诊断文本缺少 tcp 结论：%q", detail)
	}
}

// TestProbePortalTCPGuardsEmptyHost 空门户地址不得 panic、也不该挂死，直接判不可达。
func TestProbePortalTCPGuardsEmptyHost(t *testing.T) {
	if got, _ := probePortalTCP(context.Background(), "   ", 3260); got != portalTCPFailed {
		t.Fatalf("空地址应判 unreachable，实际 %q", got)
	}
}

// TestJoinDetailTruncates 诊断信息与原始报错拼起来后必须仍受 maxMountErrorDetailRunes 约束。
//
// 该字段会经 SSE 推送并写进本地状态文件，不能让它被异常文本撑爆。
func TestJoinDetailTruncates(t *testing.T) {
	long := strings.Repeat("啊", maxMountErrorDetailRunes)
	got := joinDetail("portal=10.0.0.5:3260", long)
	if runes := []rune(got); len(runes) > maxMountErrorDetailRunes+1 {
		t.Fatalf("拼接后长度 = %d 字符，期望不超过 %d(+省略号)", len(runes), maxMountErrorDetailRunes)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("超长时应以省略号结尾，实际结尾 %q", string([]rune(got)[len([]rune(got))-5:]))
	}
	// 诊断前缀在最前，必须被保留（它是定位的关键信息）。
	if !strings.HasPrefix(got, "portal=10.0.0.5:3260") {
		t.Fatalf("诊断前缀应被保留，实际 %q", got)
	}

	// 空串参与拼接时不应产生多余分隔符。
	if got := joinDetail("", "  ", "only"); got != "only" {
		t.Fatalf("joinDetail = %q，期望 only", got)
	}
}

// TestMountFailedWithDiagnostics 锁定挂载失败错误携带的诊断参数（界面据此显示连接信息）。
func TestMountFailedWithDiagnostics(t *testing.T) {
	err := errMountFailedWith("connect", errors.New("boom"), map[string]any{
		"portal":     "10.0.0.5:3260",
		"target_iqn": "iqn.2000-01.com.vault:test",
		"tcp":        "timeout",
	})
	e, ok := apperr.As(err)
	if !ok {
		t.Fatal("应返回结构化错误")
	}
	if code := apperr.CodeOf(err); code != CodeMountFailed {
		t.Fatalf("错误码 = %q，期望 %q", code, CodeMountFailed)
	}
	if e.Args["stage"] != "connect" {
		t.Fatalf("args.stage = %v，期望 connect", e.Args["stage"])
	}
	if e.Args["portal"] != "10.0.0.5:3260" || e.Args["tcp"] != "timeout" {
		t.Fatalf("诊断参数缺失：%+v", e.Args)
	}

	// 空值必须被忽略：args 里出现 portal=<nil> 只会让界面显示成垃圾。
	empty := errMountFailedWith("connect", errors.New("boom"), map[string]any{"portal": nil, "": "x", "tcp": "  "})
	ee, _ := apperr.As(empty)
	if _, exists := ee.Args["portal"]; exists {
		t.Fatalf("nil 值不应进 args：%+v", ee.Args)
	}
}
