package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestWebTargetApplies 覆盖"热更资源版本能否被客户端真正加载"的判定。
//
// 客户端主进程只在热更层**高于**内置（=应用）版本时才加载它；代理若不做同样的判断，
// 就会激活一个永远不被加载的资源，设置页显示"已更新到 vX，重启后生效"而界面纹丝不动。
func TestWebTargetApplies(t *testing.T) {
	cases := []struct {
		name        string
		appVersion  string
		target      string
		wantApplies bool
		wantDecided bool
	}{
		{name: "目标高于应用版本 → 可加载", appVersion: "0.1.37", target: "0.1.38", wantApplies: true, wantDecided: true},
		{name: "目标等于应用版本 → 不可加载（客户端用内置层）", appVersion: "0.1.37", target: "0.1.37", wantApplies: false, wantDecided: true},
		{name: "目标低于应用版本 → 不可加载", appVersion: "0.1.37", target: "0.1.28", wantApplies: false, wantDecided: true},
		{name: "应用版本缺失 → 不判定（保留原行为）", appVersion: "", target: "0.1.28", wantApplies: true, wantDecided: false},
		{name: "应用版本不可解析 → 不判定", appVersion: "dev", target: "0.1.28", wantApplies: true, wantDecided: false},
		{name: "目标不可解析 → 不可加载（保守拒绝）", appVersion: "0.1.37", target: "dev", wantApplies: false, wantDecided: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &Agent{version: c.appVersion, logger: testLogger()}
			applies, decided := a.webTargetApplies(c.target)
			if applies != c.wantApplies || decided != c.wantDecided {
				t.Fatalf("applies/decided = %v/%v，期望 %v/%v",
					applies, decided, c.wantApplies, c.wantDecided)
			}
		})
	}
}

// TestWebUpdateSnapshotIgnoresStalePointer 覆盖"历史遗留指针"的处理：
// 指针里的版本不高于本机应用版本时，客户端不会加载它，因此状态里**不得**报告 active_version，
// 否则设置页会显示一个根本不生效的版本号（真实工单：应用已 0.1.37，指针停在 0.1.28）。
func TestWebUpdateSnapshotIgnoresStalePointer(t *testing.T) {
	cases := []struct {
		name          string
		appVersion    string
		pointerVer    string
		wantActive    string
		wantStateKept string
	}{
		{
			name:       "指针低于应用版本 → 不报告已激活",
			appVersion: "0.1.37", pointerVer: "0.1.28",
			wantActive: "", wantStateKept: WebStateIdle,
		},
		{
			name:       "指针等于应用版本 → 同样不报告（客户端用内置层）",
			appVersion: "0.1.37", pointerVer: "0.1.37",
			wantActive: "", wantStateKept: WebStateIdle,
		},
		{
			name:       "指针高于应用版本 → 正常报告已激活",
			appVersion: "0.1.37", pointerVer: "0.1.40",
			wantActive: "0.1.40", wantStateKept: WebStateActivated,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dataDir := t.TempDir()
			t.Setenv("ProgramData", dataDir)
			webappDir := filepath.Join(dataDir, "Vault", webappDirName)
			if err := os.MkdirAll(webappDir, 0o755); err != nil {
				t.Fatalf("准备目录失败：%v", err)
			}
			payload, err := json.Marshal(webCurrentPointer{
				Version: c.pointerVer, Dir: c.pointerVer, ActivatedAt: 1,
			})
			if err != nil {
				t.Fatalf("构造指针失败：%v", err)
			}
			if err := os.WriteFile(filepath.Join(webappDir, webCurrentFileName), payload, 0o644); err != nil {
				t.Fatalf("写入指针失败：%v", err)
			}

			a := &Agent{version: c.appVersion, logger: testLogger()}
			a.web = newWebUpdateManager(a)

			state := a.web.snapshot()
			if state.ActiveVersion != c.wantActive {
				t.Fatalf("active_version = %q，期望 %q", state.ActiveVersion, c.wantActive)
			}
			if state.State != c.wantStateKept {
				t.Fatalf("state = %q，期望 %q", state.State, c.wantStateKept)
			}
			// 第二次调用不得因 seeded 而改变结论（幂等）。
			again := a.web.snapshot()
			if again.ActiveVersion != c.wantActive {
				t.Fatalf("二次快照 active_version = %q，期望 %q", again.ActiveVersion, c.wantActive)
			}
		})
	}
}
