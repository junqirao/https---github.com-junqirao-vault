package iscsiinitiator

import "testing"

// TestAvailabilityReadyAndState 锁定"服务状态 → 就绪判定/界面取值"的映射。
//
// 背景：只读探测早就跑了，但结果只写日志。于是"MSiSCSI 没启动"只在挂载失败
// （阶段：connect）时才暴露，而错误里没有任何可执行信息（真实反馈：应该在客户端启动时
// 就提示）。这里把判定规则钉住：**服务未运行 ≠ 发起端不可用，但 ≠ 就绪**。
func TestAvailabilityReadyAndState(t *testing.T) {
	cases := []struct {
		name    string
		avail   Availability
		ready   bool
		service string
	}{
		{
			name:    "模块可用 + 服务运行 → 就绪",
			avail:   Availability{ModuleAvailable: true, ServiceStatus: "Running"},
			ready:   true,
			service: ServiceStateRunning,
		},
		{
			name:    "状态大小写不敏感（PowerShell 可能回 running）",
			avail:   Availability{ModuleAvailable: true, ServiceStatus: "running"},
			ready:   true,
			service: ServiceStateRunning,
		},
		{
			name:    "模块可用 + 服务停止 → 不就绪（就是本工单的场景）",
			avail:   Availability{ModuleAvailable: true, ServiceStatus: "Stopped"},
			ready:   false,
			service: ServiceStateStopped,
		},
		{
			name:    "服务启动中 → 不止确认为就绪",
			avail:   Availability{ModuleAvailable: true, ServiceStatus: "StartPending"},
			ready:   false,
			service: ServiceStateStopped,
		},
		{
			name:    "模块缺失 → 不就绪（未安装发起程序）",
			avail:   Availability{ModuleAvailable: false, ServiceStatus: "Running"},
			ready:   false,
			service: ServiceStateRunning,
		},
		{
			name:    "脚本没报状态 → 未知，绝不当作就绪",
			avail:   Availability{ModuleAvailable: true, ServiceStatus: "  "},
			ready:   false,
			service: ServiceStateUnknown,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.avail.Ready(); got != c.ready {
				t.Fatalf("Ready() = %v，期望 %v（avail=%+v）", got, c.ready, c.avail)
			}
			if got := c.avail.ServiceState(); got != c.service {
				t.Fatalf("ServiceState() = %q，期望 %q", got, c.service)
			}
		})
	}
}
