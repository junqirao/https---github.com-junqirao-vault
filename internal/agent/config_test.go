package agent

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestAutoLoginDefaultsOn 覆盖"缺省即开启"这条默认行为。
//
// 背景：服务端会话只存在内存里，服务端一重启全部令牌失效。若免密登录默认关闭，
// 用户每次重启服务端都要重新输密码（真实反馈）。
// 关键点：**检测的是"配置里有没有 auto_login 这一项"，而不是它的零值** ——
// 旧版本写下的配置文件没有该键，必须同样按开启处理。
func TestAutoLoginDefaultsOn(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "旧配置缺少 auto_login 键 → 开启",
			content: `{"auto_mount":false,"language":"zh-CN"}`,
			want:    true,
		},
		{
			name:    "显式关闭 → 关闭",
			content: `{"auto_login":false}`,
			want:    false,
		},
		{
			name:    "显式开启 → 开启",
			content: `{"auto_login":true}`,
			want:    true,
		},
		{
			name:    "空对象 → 开启",
			content: `{}`,
			want:    true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent-config.json")
			if err := os.WriteFile(path, []byte(c.content), 0o600); err != nil {
				t.Fatalf("准备配置失败：%v", err)
			}
			store, err := NewConfigStore(path, testLogger())
			if err != nil {
				t.Fatalf("加载配置失败：%v", err)
			}
			if got := store.Get().AutoLogin; got != c.want {
				t.Fatalf("AutoLogin = %v，期望 %v", got, c.want)
			}
		})
	}
}

// TestAutoLoginPatchRespectsExplicitChoice 显式关闭后必须一直保持关闭（含重启后）。
func TestAutoLoginPatchRespectsExplicitChoice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-config.json")
	store, err := NewConfigStore(path, testLogger())
	if err != nil {
		t.Fatalf("加载配置失败：%v", err)
	}
	if !store.Get().AutoLogin {
		t.Fatal("全新安装应默认开启免密登录")
	}

	off := false
	if _, err := store.Patch(ConfigPatch{AutoLogin: &off}); err != nil {
		t.Fatalf("关闭免密登录失败：%v", err)
	}
	if store.Get().AutoLogin {
		t.Fatal("显式关闭后不应仍为开启")
	}

	// 重新加载（模拟重启）：显式 false 已落盘，不能被"缺省即开启"覆盖。
	reloaded, err := NewConfigStore(path, testLogger())
	if err != nil {
		t.Fatalf("重新加载配置失败：%v", err)
	}
	if reloaded.Get().AutoLogin {
		t.Fatal("显式关闭必须持久化，重启后仍为关闭")
	}

	// 再打开一次并重新加载，确认双向都持久化。
	on := true
	if _, err := reloaded.Patch(ConfigPatch{AutoLogin: &on}); err != nil {
		t.Fatalf("开启免密登录失败：%v", err)
	}
	again, err := NewConfigStore(path, testLogger())
	if err != nil {
		t.Fatalf("再次加载配置失败：%v", err)
	}
	if !again.Get().AutoLogin {
		t.Fatal("显式开启必须持久化")
	}
}

// TestRepoMountPrefIsIndependentPerRepo 每库挂载偏好各存各的：写入两个库后互不影响、
// 能落盘、并且 Get() 返回的副本被改动不会污染 store 内部状态。
func TestRepoMountPrefIsIndependentPerRepo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-config.json")
	store, err := NewConfigStore(path, testLogger())
	if err != nil {
		t.Fatalf("加载配置失败：%v", err)
	}
	if _, err := store.SetRepoMountPref("repo-a", RepoMountPref{
		MountMode: mountModeDirectory,
		MountDir:  `D:\vault\a`,
		AutoMount: true,
	}); err != nil {
		t.Fatalf("写入 repo-a 偏好失败：%v", err)
	}
	if _, err := store.SetRepoMountPref("repo-b", RepoMountPref{
		MountMode: mountModeLetter,
		AutoMount: false,
	}); err != nil {
		t.Fatalf("写入 repo-b 偏好失败：%v", err)
	}

	// 副本不可写穿：界面拿到配置后随手改一改，不能影响真正落盘的值。
	cfg := store.Get()
	cfg.RepoMounts["repo-a"] = RepoMountPref{AutoMount: false}
	if got := store.Get().RepoMounts["repo-a"]; !got.AutoMount || got.MountMode != mountModeDirectory {
		t.Fatalf("Get() 副本被外部改动污染了 store：%+v", got)
	}

	reloaded, err := NewConfigStore(path, testLogger())
	if err != nil {
		t.Fatalf("重新加载配置失败：%v", err)
	}
	got := reloaded.Get().RepoMounts
	if len(got) != 2 {
		t.Fatalf("应有两个库的偏好，实际 %d 个：%+v", len(got), got)
	}
	if a := got["repo-a"]; !a.AutoMount || a.MountMode != mountModeDirectory || a.MountDir != `D:\vault\a` {
		t.Fatalf("repo-a 偏好未正确落盘：%+v", a)
	}
	if b := got["repo-b"]; b.AutoMount || b.MountMode != mountModeLetter {
		t.Fatalf("repo-b 偏好未正确落盘：%+v", b)
	}
}

// TestSetRepoMountPrefRejectsBadInput 非法形态/空库 ID 必须报错，且不落盘。
func TestSetRepoMountPrefRejectsBadInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-config.json")
	store, err := NewConfigStore(path, testLogger())
	if err != nil {
		t.Fatalf("加载配置失败：%v", err)
	}
	if _, err := store.SetRepoMountPref("repo-a", RepoMountPref{MountMode: "volume"}); err == nil {
		t.Fatal("非法挂载形态应报错")
	}
	if _, err := store.SetRepoMountPref("   ", RepoMountPref{AutoMount: true}); err == nil {
		t.Fatal("空库 ID 应报错")
	}
	if got := store.Get().RepoMounts; len(got) != 0 {
		t.Fatalf("报错的写入不应落盘，实际：%+v", got)
	}
}

// TestConfigMarshalKeepsAutoLoginBool 落盘/透出的 JSON 里 auto_login 必须是布尔值，
// 不能因为内部用"指针 + 是否显式设置"表示默认值就变成 null 或消失（前端读的是布尔）。
func TestConfigMarshalKeepsAutoLoginBool(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-config.json")
	store, err := NewConfigStore(path, testLogger())
	if err != nil {
		t.Fatalf("加载配置失败：%v", err)
	}
	data, err := json.Marshal(store.Get())
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	value, ok := doc["auto_login"]
	if !ok {
		t.Fatal("auto_login 字段缺失")
	}
	if _, isBool := value.(bool); !isBool {
		t.Fatalf("auto_login 必须是布尔值，实际 %T(%v)", value, value)
	}
	// 内部标记不得落盘。
	if _, leaked := doc["autoLoginSet"]; leaked {
		t.Fatal("内部标记不应出现在 JSON 中")
	}
	if _, leaked := doc["AutoLoginSet"]; leaked {
		t.Fatal("内部标记不应出现在 JSON 中")
	}
}
