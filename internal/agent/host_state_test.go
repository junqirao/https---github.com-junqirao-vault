package agent

import (
	"errors"
	"testing"

	"vault/internal/apperr"
	"vault/internal/platform/iscsiinitiator"
)

// TestHostStateFromAvailability 锁定"探测结果 → 界面横幅所需状态"的映射。
//
// 目的是启动即告知（而不是等挂载失败）：
//   - 未安装发起程序 → module_missing（要去启用 Windows 功能）
//   - 已安装但服务没运行 → service_stopped（管理员 Start-Service MSiSCSI 即可）
//   - 探测本身失败 → probe_failed（**不许猜**成"就绪"，猜错的代价是用户点挂载才发现）
func TestHostStateFromAvailability(t *testing.T) {
	cases := []struct {
		name       string
		avail      iscsiinitiator.Availability
		err        error
		ready      bool
		service    string
		reason     string
		wantErrArg string
	}{
		{
			name:    "服务运行中 → 就绪、无原因码",
			avail:   iscsiinitiator.Availability{ModuleAvailable: true, ServiceStatus: "Running"},
			ready:   true,
			service: iscsiinitiator.ServiceStateRunning,
		},
		{
			name:    "服务已停止 → 不就绪 + service_stopped",
			avail:   iscsiinitiator.Availability{ModuleAvailable: true, ServiceStatus: "Stopped"},
			ready:   false,
			service: iscsiinitiator.ServiceStateStopped,
			reason:  HostReasonServiceStopped,
		},
		{
			name:    "模块缺失 → 不就绪 + module_missing",
			avail:   iscsiinitiator.Availability{ModuleAvailable: false, ServiceStatus: "Stopped"},
			ready:   false,
			service: iscsiinitiator.ServiceStateStopped,
			reason:  HostReasonModuleMissing,
		},
		{
			name:       "探测失败 → 未知 + probe_failed（不许猜成就绪）",
			err:        apperr.New(apperr.CodeUnavailable, 500).WithArg("component", "iscsiinitiator"),
			ready:      false,
			service:    iscsiinitiator.ServiceStateUnknown,
			reason:     HostReasonProbeFailed,
			wantErrArg: apperr.CodeUnavailable,
		},
		{
			name:       "探测超时 → 同样是 probe_failed",
			err:        errors.New("context deadline exceeded"),
			ready:      false,
			service:    iscsiinitiator.ServiceStateUnknown,
			reason:     HostReasonProbeFailed,
			wantErrArg: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := hostStateFromAvailability(c.avail, c.err)
			if got.ISCSIReady != c.ready {
				t.Fatalf("ISCSIReady = %v，期望 %v", got.ISCSIReady, c.ready)
			}
			if got.ISCSIService != c.service {
				t.Fatalf("ISCSIService = %q，期望 %q", got.ISCSIService, c.service)
			}
			if got.Reason != c.reason {
				t.Fatalf("Reason = %q，期望 %q", got.Reason, c.reason)
			}
			if got.CheckedAt == 0 {
				t.Fatal("CheckedAt 必须落值（界面要显示探测时间）")
			}
			if c.wantErrArg != "" && got.Error != c.wantErrArg {
				t.Fatalf("Error = %q，期望 %q", got.Error, c.wantErrArg)
			}
		})
	}
}

// TestUpdateHostStatePublishesOnlyOnChange 锁定 host 事件的推送时机。
//
// 未就绪期间每 30s 复检一次；若每次探测都发事件，界面会每半分钟收到一条无变化的通知
// （CheckedAt 每次都变，必须排除在比较之外）。
func TestUpdateHostStatePublishesOnlyOnChange(t *testing.T) {
	a := newPhaseTestAgent(t)
	_, events := a.hub.Subscribe()

	a.updateHostState(HostState{ISCSIService: iscsiinitiator.ServiceStateStopped, Reason: HostReasonServiceStopped})
	ev, ok := takeMountEvent(t, events)
	if !ok || ev.Type != "host" {
		t.Fatalf("首次写入应广播 host 事件，实际 %+v (ok=%v)", ev, ok)
	}

	// 复检：语义字段不变，只有 CheckedAt 变 → 不得再发事件。
	a.updateHostState(HostState{ISCSIService: iscsiinitiator.ServiceStateStopped, Reason: HostReasonServiceStopped, CheckedAt: 12345})
	if ev, ok := takeMountEvent(t, events); ok {
		t.Fatalf("状态未变化不应广播事件，实际 %+v", ev)
	}

	// 服务被启动 → 状态变化 → 必须广播（界面横幅据此自动消失）。
	a.markISCSIServiceRunning()
	if state := a.hostState(); !state.ISCSIReady || state.Reason != "" {
		t.Fatalf("服务就绪后应清掉 Reason，实际 %+v", state)
	}
	if ev, ok := takeMountEvent(t, events); !ok || ev.Type != "host" {
		t.Fatalf("状态变化应广播 host 事件，实际 %+v (ok=%v)", ev, ok)
	}
}
