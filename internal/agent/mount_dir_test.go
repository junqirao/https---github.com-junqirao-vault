package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// newMountDirEngine 构造一个只带本地配置与空状态存储的引擎：目录解析是纯计算，不碰 iSCSI。
//
// 状态存储是必须的：目录名里的服务端那段来自"这台服务端在本机的别名"（见 serverAlias），
// 而别名按 server_key 记在状态里 —— 没有存储就无从判断"本机只有一台服务端"。
func newMountDirEngine(t *testing.T, cfgJSON string) *mountEngine {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	if cfgJSON != "" {
		if err := os.WriteFile(path, []byte(cfgJSON), 0o600); err != nil {
			t.Fatalf("准备配置失败：%v", err)
		}
	}
	store, err := NewConfigStore(path, testLogger())
	if err != nil {
		t.Fatalf("加载配置失败：%v", err)
	}
	state, err := NewStateStore(filepath.Join(dir, "state.json"), testLogger())
	if err != nil {
		t.Fatalf("构造状态存储失败：%v", err)
	}
	a := &Agent{logger: testLogger(), cfg: store, store: state}
	a.engine = &mountEngine{a: a}
	return a.engine
}

// TestResolveMountDirAddsServerRepoLevel 锁定"本地指定的目录是父目录"这条规则：
// 实际挂载点是它下面一层 `<服务端名称>_<存储库名称>` 子目录 —— 多个库共用一个父目录
// 时天然分开，目录名也自解释（用户诉求：目录下默认多一级、名叫 服务端名称_存储库名称）。
func TestResolveMountDirAddsServerRepoLevel(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
		// sessions 是要预先登记的服务端（多服务端下别名按台记在状态里）。
		sessions []Session
		req      MountRequest
		spec     *MountSpec
		// serverKey 是本次挂载归属的服务端键（空串 = 未指定/单服务端）。
		serverKey string
		want      string
	}{
		{
			name: "对话框/每库配置填绝对目录 → 作为父目录再加一层",
			req:  MountRequest{RepoID: "repo-a", RepoName: "game-x", MountPath: `D:\Vault`},
			spec: &MountSpec{ServerName: "vault-server"},
			want: `D:\Vault\vault-server_game-x`,
		},
		{
			name: "每库配置填相对目录 → 相对默认挂载根，再加一层",
			cfg:  `{"repo_mounts":{"repo-a":{"mount_mode":"directory","mount_dir":"libs"}}}`,
			req:  MountRequest{RepoID: "repo-a", RepoName: "game-x"},
			spec: &MountSpec{ServerName: "vault-server"},
			want: `C:\Vault\libs\vault-server_game-x`,
		},
		{
			name: "本地没配 → 退化为默认根下的一层",
			req:  MountRequest{RepoID: "repo-a", RepoName: "game-x"},
			spec: &MountSpec{ServerName: "vault-server"},
			want: `C:\Vault\vault-server_game-x`,
		},
		{
			name: "服务端下发的相对路径也归一到同一命名（避免 srv\\lib 与 srv_lib 两种目录名并存）",
			req:  MountRequest{RepoID: "repo-a", RepoName: "game-x"},
			spec: &MountSpec{ServerName: "vault-server", MountPath: `vault-server\game-x`},
			want: `C:\Vault\vault-server_game-x`,
		},
		{
			name: "服务端下发绝对路径（服务端管理员的显式指定）→ 原样使用",
			req:  MountRequest{RepoID: "repo-a", RepoName: "game-x"},
			spec: &MountSpec{ServerName: "vault-server", MountPath: `E:\data\game-x`},
			want: `E:\data\game-x`,
		},
		{
			name: "本地配置了别名 → 目录名用别名",
			cfg:  `{"server_alias":"my-nas","repo_mounts":{"repo-a":{"mount_mode":"directory","mount_dir":"D:\\Vault"}}}`,
			req:  MountRequest{RepoID: "repo-a", RepoName: "game-x"},
			spec: &MountSpec{ServerName: "vault-server"},
			want: `D:\Vault\my-nas_game-x`,
		},
		{
			// 多服务端下"本机别名"是**每台**一个：全局那个名字已经不足以标识是哪一台，
			// 继续沿用会让两台服务端的不同库挤进同一个命名空间（同名目录）。
			name: "多服务端下不再用全局别名 → 用该台的服务端名称",
			cfg:  `{"server_alias":"my-nas","repo_mounts":{"repo-a":{"mount_mode":"directory","mount_dir":"D:\\Vault"}}}`,
			sessions: []Session{
				{ServerURL: "https://10.0.0.1:8443", Token: "t1"},
				{ServerURL: "https://10.0.0.2:8443", Token: "t2"},
			},
			req:       MountRequest{RepoID: "repo-a", RepoName: "game-x"},
			spec:      &MountSpec{ServerName: "vault-server"},
			serverKey: serverKeyOf("", "https://10.0.0.1:8443"),
			want:      `D:\Vault\vault-server_game-x`,
		},
		{
			name: "该台自带别名 → 优先于全局别名与服务端名称",
			cfg:  `{"server_alias":"my-nas","repo_mounts":{"repo-a":{"mount_mode":"directory","mount_dir":"D:\\Vault"}}}`,
			sessions: []Session{
				{ServerURL: "https://10.0.0.1:8443", Token: "t1", Alias: "nas-1"},
			},
			req:       MountRequest{RepoID: "repo-a", RepoName: "game-x"},
			spec:      &MountSpec{ServerName: "vault-server"},
			serverKey: serverKeyOf("", "https://10.0.0.1:8443"),
			want:      `D:\Vault\nas-1_game-x`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newMountDirEngine(t, c.cfg)
			for _, sess := range c.sessions {
				e.a.store.SetSession("", &sess)
			}
			got, err := e.resolveMountDir(c.req, c.spec, c.serverKey)
			if err != nil {
				t.Fatalf("解析挂载目录失败：%v", err)
			}
			if got != c.want {
				t.Fatalf("挂载目录 = %q，期望 %q", got, c.want)
			}
		})
	}
}

// TestResolveMountDirIsIdempotentForRemount 重挂/恢复会把**上次的最终挂载点**当请求传回来
// （见 recordedMountRequest），若再加一层，每重挂一次就会多套一层目录
// （D:\Vault\a_b\a_b\a_b…），用户的挂载点会越跑越深。
func TestResolveMountDirIsIdempotentForRemount(t *testing.T) {
	e := newMountDirEngine(t, "")
	spec := &MountSpec{ServerName: "vault-server"}
	req := MountRequest{RepoID: "repo-a", RepoName: "game-x", MountPath: `D:\Vault`}

	first, err := e.resolveMountDir(req, spec, "")
	if err != nil {
		t.Fatalf("首次解析失败：%v", err)
	}
	// 把上次结果当作本次请求（remount 的真实做法）：必须原样返回。
	req.MountPath = first
	second, err := e.resolveMountDir(req, spec, "")
	if err != nil {
		t.Fatalf("重挂解析失败：%v", err)
	}
	if second != first {
		t.Fatalf("重挂后挂载点漂移：%q → %q", first, second)
	}

	// Windows 文件系统不区分大小写：大小写不同也要认出"已经是最终挂载点"。
	req.MountPath = `D:\vault\VAULT-SERVER_game-x`
	third, err := e.resolveMountDir(req, spec, "")
	if err != nil {
		t.Fatalf("重挂解析失败：%v", err)
	}
	if third != req.MountPath {
		t.Fatalf("大小写不同却被当作新路径：%q → %q", req.MountPath, third)
	}
}

// TestResolveMountDirSanitizesLeaf 库名/别名可能含 `\` `/` 等字符，直接拼进目录名会让
// 路径跑到父目录之外（路径穿越）；两段都必须先清洗。
func TestResolveMountDirSanitizesLeaf(t *testing.T) {
	e := newMountDirEngine(t, "")
	got, err := e.resolveMountDir(MountRequest{
		RepoID: "repo-a", RepoName: `..\..\evil`, MountPath: `D:\Vault`,
	}, &MountSpec{ServerName: "srv/../x"}, "")
	if err != nil {
		t.Fatalf("解析挂载目录失败：%v", err)
	}
	// `..\..\evil` → `..-..-evil`，再去掉首部的点 → `-..-evil`：整体仍是**一个**目录名，
	// 不会真的退到上两级去。
	want := `D:\Vault\srv-..-x_-..-evil`
	if got != want {
		t.Fatalf("挂载目录 = %q，期望 %q", got, want)
	}
}

// TestMountDirLeafFallsBack 库名缺失时退到分配 ID（至少不会让所有库挤进同一个目录）；
// 别名缺失时退到 "vault"。
func TestMountDirLeafFallsBack(t *testing.T) {
	cases := []struct {
		alias, repo, allocation, want string
	}{
		{alias: "srv", repo: "game-x", want: "srv_game-x"},
		{alias: "", repo: "game-x", want: "vault_game-x"},
		{alias: "srv", repo: "", allocation: "alloc-7", want: "srv_alloc-7"},
		// 空库名且无分配 ID → 只剩服务端名。
		{alias: "srv", repo: "  ", want: "srv"},
		{alias: "", repo: "", want: "vault"},
	}
	for _, c := range cases {
		if got := mountDirLeaf(c.alias, c.repo, c.allocation); got != c.want {
			t.Fatalf("mountDirLeaf(%q, %q, %q) = %q，期望 %q", c.alias, c.repo, c.allocation, got, c.want)
		}
	}
}
