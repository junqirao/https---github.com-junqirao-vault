package app

import "testing"

// TestPortalFromConfig 覆盖"下发给客户端的 iSCSI 门户地址"的选择规则。
//
// 背景：此前门户地址只能从 http.listen 自动推导（多网卡服务器上可能派发到客户端
// 不可达的网卡），而 platform.iscsi.portals 配置从未被消费。本测试锁定新的优先级：
// 显式配置（第一条）> 监听地址 > 首个非回环 IP > 主机名；端口取配置值，缺省 3260。
func TestPortalFromConfig(t *testing.T) {
	cases := []struct {
		name     string
		portals  []string
		listen   string
		wantHost string
		wantPort int
	}{
		{
			name: "显式地址与端口直接生效", portals: []string{"192.168.1.10:3261"},
			listen: "0.0.0.0:8443", wantHost: "192.168.1.10", wantPort: 3261,
		},
		{
			name: "显式地址不带端口时用默认端口", portals: []string{"10.0.0.5"},
			listen: "0.0.0.0:8443", wantHost: "10.0.0.5", wantPort: 3260,
		},
		{
			name: "多条只取第一条", portals: []string{"10.0.0.5:3260", "10.0.0.6:3260"},
			listen: "0.0.0.0:8443", wantHost: "10.0.0.5", wantPort: 3260,
		},
		{
			name: "跳过空条目", portals: []string{"", "   ", "10.0.0.7:3262"},
			listen: "0.0.0.0:8443", wantHost: "10.0.0.7", wantPort: 3262,
		},
		{
			name: "通配配置：主机用监听地址、端口用配置值", portals: []string{"0.0.0.0:3265"},
			listen: "192.168.1.20:8443", wantHost: "192.168.1.20", wantPort: 3265,
		},
		{
			name: "配置为空：退回监听地址", portals: nil,
			listen: "10.1.2.3:8443", wantHost: "10.1.2.3", wantPort: 3260,
		},
		{
			name: "非法端口被忽略（退回默认端口）", portals: []string{"10.0.0.9:99999"},
			listen: "0.0.0.0:8443", wantHost: "10.0.0.9", wantPort: 3260,
		},
		{
			name: "IPv6 带方括号也能解析", portals: []string{"[fd00::1]:3261"},
			listen: "0.0.0.0:8443", wantHost: "fd00::1", wantPort: 3261,
		},
		{
			name: "监听地址带方括号的 IPv6：取主机部分", portals: nil,
			listen: "[fd00::2]:8443", wantHost: "fd00::2", wantPort: 3260,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			host, port := portalFromConfig(c.portals, c.listen)
			if host != c.wantHost || port != c.wantPort {
				t.Fatalf("portalFromConfig(%v, %q) = (%q, %d)，期望 (%q, %d)",
					c.portals, c.listen, host, port, c.wantHost, c.wantPort)
			}
		})
	}
}

// TestPortalFromConfigAutoDerive 覆盖"通配监听 + 通配配置"的自动推导分支：
// 主机由本机网卡决定（不可硬编码），但必须非空、且端口取自配置。
func TestPortalFromConfigAutoDerive(t *testing.T) {
	host, port := portalFromConfig([]string{"0.0.0.0:3267"}, "0.0.0.0:8443")
	if host == "" {
		t.Fatal("自动推导的主机不能为空（否则客户端无法连接门户）")
	}
	if isWildcardHost(host) {
		t.Fatalf("自动推导结果不能仍是通配地址：%q", host)
	}
	if port != 3267 {
		t.Fatalf("端口 = %d，期望取配置值 3267", port)
	}
}

// TestIsWildcardHost 锁定通配地址的判定集合。
func TestIsWildcardHost(t *testing.T) {
	for _, value := range []string{"", "  ", "0.0.0.0", "::", "[::]", "*"} {
		if !isWildcardHost(value) {
			t.Fatalf("%q 应判定为通配地址", value)
		}
	}
	for _, value := range []string{"127.0.0.1", "10.0.0.1", "fd00::1", "server.local"} {
		if isWildcardHost(value) {
			t.Fatalf("%q 不应判定为通配地址", value)
		}
	}
}
