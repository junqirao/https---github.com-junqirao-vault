package agent

import "testing"

// TestSplitPortal 锁定门户字符串的解析：卸载后"没有别的挂载用同一门户就删门户"依赖它。
func TestSplitPortal(t *testing.T) {
	cases := []struct {
		in   string
		host string
		port int
		ok   bool
	}{
		{"10.0.0.5:3260", "10.0.0.5", 3260, true},
		{"127.0.0.1:3261", "127.0.0.1", 3261, true},
		{"[::1]:3260", "::1", 3260, true},
		{"", "", 0, false},
		{"10.0.0.5", "", 0, false},
		{":3260", "", 0, false},
		{"10.0.0.5:0", "", 0, false},
		{"10.0.0.5:abc", "", 0, false},
	}
	for _, c := range cases {
		host, port, ok := splitPortal(c.in)
		if ok != c.ok || host != c.host || port != c.port {
			t.Fatalf("splitPortal(%q) = (%q,%d,%v)，期望 (%q,%d,%v)",
				c.in, host, port, ok, c.host, c.port, c.ok)
		}
	}
}
