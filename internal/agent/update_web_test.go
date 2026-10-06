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

// TestReconcileWebLayerCleansStale 覆盖"失效热更层启动自愈"：应用升级后，不高于应用版本的
// 激活指针与资源目录必须被**自动**清理干净，不能让用户手工去删 %ProgramData%\Vault\webapp。
func TestReconcileWebLayerCleansStale(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("ProgramData", dataDir)
	webappDir := filepath.Join(dataDir, "Vault", webappDirName)

	mkVersion := func(v string) string {
		dir := filepath.Join(webappDir, v)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("准备版本目录失败：%v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, webIndexFileName), []byte("<!doctype html>"), 0o644); err != nil {
			t.Fatalf("写入 index.html 失败：%v", err)
		}
		return dir
	}
	// 应用版本 0.1.68：0.1.28/0.1.68 都永远不会被加载，0.1.70 是唯一"活人"。
	staleDirs := []string{mkVersion("0.1.28"), mkVersion("0.1.68")}
	aliveDir := mkVersion("0.1.70")

	tmpDir := filepath.Join(webappDir, "0.1.69"+webTempPrefix+"abcd")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		t.Fatalf("准备遗留临时目录失败：%v", err)
	}
	partFile := filepath.Join(webappDir, "Vault-Web-0.1.69.zip"+partSuffix)
	if err := os.WriteFile(partFile, []byte("x"), 0o644); err != nil {
		t.Fatalf("准备遗留半包失败：%v", err)
	}

	currentPath := filepath.Join(webappDir, webCurrentFileName)
	writePointer(t, currentPath, "0.1.28")

	a := &Agent{version: "0.1.68", logger: testLogger()}
	a.web = newWebUpdateManager(a)
	a.reconcileWebLayer()

	if _, err := os.Stat(currentPath); !os.IsNotExist(err) {
		t.Fatalf("失效激活指针未被清理（stat err = %v）", err)
	}
	for _, dir := range staleDirs {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("失效资源目录未被清理：%s（stat err = %v）", dir, err)
		}
	}
	if _, err := os.Stat(filepath.Join(aliveDir, webIndexFileName)); err != nil {
		t.Fatalf("仍会生效的资源目录被误删：%s（%v）", aliveDir, err)
	}
	if _, err := os.Stat(tmpDir); !os.IsNotExist(err) {
		t.Fatalf("遗留临时目录未被清理（stat err = %v）", err)
	}
	if _, err := os.Stat(partFile); !os.IsNotExist(err) {
		t.Fatalf("遗留半包未被清理（stat err = %v）", err)
	}
	// 自愈后的快照不得再报告"已激活"那个失效版本。
	if state := a.web.snapshot(); state.ActiveVersion != "" {
		t.Fatalf("自愈后 active_version = %q，期望空", state.ActiveVersion)
	}

	// 幂等：再跑一次不得误删保留目录。
	a.reconcileWebLayer()
	if _, err := os.Stat(filepath.Join(aliveDir, webIndexFileName)); err != nil {
		t.Fatalf("二次自愈误删保留目录：%v", err)
	}
}

// TestReconcileWebLayerKeepsResourcesWhenAppVersionUnknown 覆盖"无从判定则不动手"：
// 开发构建（应用版本不可解析）时不能凭猜测删资源，否则会误删仍会被加载的热更层。
func TestReconcileWebLayerKeepsResourcesWhenAppVersionUnknown(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("ProgramData", dataDir)
	webappDir := filepath.Join(dataDir, "Vault", webappDirName)
	dir := filepath.Join(webappDir, "0.1.28")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("准备版本目录失败：%v", err)
	}
	currentPath := filepath.Join(webappDir, webCurrentFileName)
	writePointer(t, currentPath, "0.1.28")

	a := &Agent{version: "dev", logger: testLogger()}
	a.web = newWebUpdateManager(a)
	a.reconcileWebLayer()

	if _, err := os.Stat(currentPath); err != nil {
		t.Fatalf("应用版本无从判定时误删了指针：%v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("应用版本无从判定时误删了资源目录：%v", err)
	}
}

// writePointer 写出激活指针（与代理运行期同格式，便于断言磁盘状态）。
func writePointer(t *testing.T, path, version string) {
	t.Helper()
	payload, err := json.Marshal(webCurrentPointer{Version: version, Dir: version, ActivatedAt: 1})
	if err != nil {
		t.Fatalf("构造指针失败：%v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("准备指针目录失败：%v", err)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatalf("写入指针失败：%v", err)
	}
}
