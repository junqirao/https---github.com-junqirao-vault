package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestStateStore 构造一份全新的（空）本地状态存储。
func newTestStateStore(t *testing.T) *stateStore {
	t.Helper()
	store, err := NewStateStore(filepath.Join(t.TempDir(), "state.json"), testLogger())
	if err != nil {
		t.Fatalf("构造状态存储失败：%v", err)
	}
	return store
}

// TestStateStoreKeepsServersIndependent 锁定多服务端的地基：每台各自一份状态，互不覆盖。
func TestStateStoreKeepsServersIndependent(t *testing.T) {
	store := newTestStateStore(t)
	first := store.SetSession("", &Session{ServerURL: "https://10.0.0.1:8443", Token: "t1"})
	second := store.SetSession("", &Session{ServerURL: "https://10.0.0.2:8443", Token: "t2"})
	if first == "" || second == "" || first == second {
		t.Fatalf("两台服务端应各自一把键，实际 %q / %q", first, second)
	}

	// 首台注册的自动成为主服务端（更新源等"无上下文"操作的默认目标）。
	if got := store.PrimaryKey(); got != first {
		t.Fatalf("首台注册的服务端应成为主服务端，实际 %q", got)
	}
	if keys := store.ServerKeys(); len(keys) != 2 || keys[0] != first {
		t.Fatalf("应有 2 台且主服务端在前，实际 %v", keys)
	}

	// 会话各归各的：绝不能出现"后推的会话覆盖前一台"。
	if sess, ok := store.Session(second); !ok || sess.Token != "t2" {
		t.Fatalf("第二台的令牌应各自保存，实际 %+v ok=%v", sess, ok)
	}
	if all := store.Sessions(); len(all) != 2 {
		t.Fatalf("应有 2 份会话，实际 %d 份", len(all))
	}

	// 用户按台记：B 台没推过用户，就不该拿到 A 台的人。
	store.SetUser(first, UserState{ID: "u1", Username: "alice"})
	if u := store.User(second); u.Username != "" || u.ID != "" {
		t.Fatalf("B 台没推过用户，不该读到 A 台的人，实际 %+v", u)
	}
	if u := store.User(first); u.Username != "alice" {
		t.Fatalf("A 台的用户应可读回，实际 %+v", u)
	}

	// 连接状态按台写：连上 B 不该把 A 也标成已连接。
	store.SetServerConnected(second, true, "")
	if s, _ := store.Server(first); s.Connected {
		t.Fatal("只应影响被指定的那一台")
	}
	if s, _ := store.Server(second); !s.Connected {
		t.Fatal("被指定的那一台应标记为已连接")
	}
}

// TestStateStoreMergesAddressKeyIntoInstanceIDKey 锁定"同一台服务端只能有一条记录"。
//
// 客户端第一次推会话时可能还不知道实例 ID（此时键是规范化地址），拿到 system/info 后
// 再推一次就会带上实例 ID。两次推的是同一台，必须合并，否则一台服务端会在状态里裂成两条、
// 挂载归属也会跟着分叉。
func TestStateStoreMergesAddressKeyIntoInstanceIDKey(t *testing.T) {
	store := newTestStateStore(t)
	const serverURL = "https://10.0.0.1:8443"

	byURL := store.SetSession("", &Session{ServerURL: serverURL, Token: "t1"})
	if byURL == "" {
		t.Fatal("推会话后应得到服务端键")
	}
	store.PutMount(&MountState{
		AllocationID: "alloc-1",
		ServerKey:    byURL,
		State:        MountStateMounted,
	}, nil)

	byID := store.SetSession("", &Session{ServerURL: serverURL, ServerInstanceID: "srv-a", Token: "t2"})
	if byID == byURL {
		t.Fatalf("带上实例 ID 后应以实例 ID 为键，实际仍是 %q", byID)
	}

	if keys := store.ServerKeys(); len(keys) != 1 || keys[0] != byID {
		t.Fatalf("同一台服务端在状态里只能有一条记录（键为实例 ID），实际 %v", keys)
	}
	if got := store.PrimaryKey(); got != byID {
		t.Fatalf("被合并掉的主服务端应改键到存活的那条，实际 %q", got)
	}
	// 用旧键（地址）依然能问到这一台：外部传哪种键都该落到同一台。
	if sess, ok := store.Session(byURL); !ok || sess.Token != "t2" {
		t.Fatalf("旧键（地址）应能解析到合并后的那一台，实际 %+v ok=%v", sess, ok)
	}
	// 挂载归属一并改键：否则心跳会拿着已消失的键去回写。
	if mounts := store.MountsForServer(byID); len(mounts) != 1 || mounts[0].ServerKey != byID {
		t.Fatalf("挂载归属应随服务端改键，实际 %+v", mounts)
	}
}

// TestStateStoreClearSessionPerServer 锁定"逐一登出"与"全部登出"。
func TestStateStoreClearSessionPerServer(t *testing.T) {
	store := newTestStateStore(t)
	first := store.SetSession("", &Session{ServerURL: "https://10.0.0.1:8443", Token: "t1"})
	second := store.SetSession("", &Session{ServerURL: "https://10.0.0.2:8443", Token: "t2"})

	store.ClearSession(first)
	if _, ok := store.Session(first); ok {
		t.Fatal("指定的那台应已登出")
	}
	if _, ok := store.Session(second); !ok {
		t.Fatal("登出一台不该连累另一台")
	}
	// 登出后停在"未连接"且计数顶满：既不自动重连（否则等于把用户登回来），
	// 界面也能据此显示"未连接 + 重试按钮"。
	if s, _ := store.Server(first); s.Phase != ServerPhaseDisconnected || s.FailCount != maxServerConnectAttempts {
		t.Fatalf("登出后应停在未连接且计数顶满，实际 fail_count=%d phase=%q", s.FailCount, s.Phase)
	}

	store.ClearAllSessions()
	if keys := store.Sessions(); len(keys) != 0 {
		t.Fatalf("全部登出后不该还有会话，实际 %v", keys)
	}
	// 登出只丢令牌，不删服务端条目（本机客户端证书身份仍在）。
	if keys := store.ServerKeys(); len(keys) != 2 {
		t.Fatalf("全部登出不该删掉服务端条目，实际 %v", keys)
	}
}

// TestStateStoreMountsAreGroupedPerServer 锁定挂载按台归属（含旧记录的兜底）。
func TestStateStoreMountsAreGroupedPerServer(t *testing.T) {
	store := newTestStateStore(t)
	first := store.SetSession("", &Session{ServerURL: "https://10.0.0.1:8443", Token: "t1"})
	second := store.SetSession("", &Session{ServerURL: "https://10.0.0.2:8443", Token: "t2"})

	store.PutMount(&MountState{AllocationID: "alloc-a", ServerKey: first, State: MountStateMounted}, nil)
	store.PutMount(&MountState{AllocationID: "alloc-b", ServerKey: second, State: MountStateMounted}, nil)
	// 未归类（运行期意外留下的老记录）只算主服务端的，不能同时算到两台头上 ——
	// 否则每一台都会去"恢复"同一个挂载。
	store.PutMount(&MountState{AllocationID: "alloc-legacy", State: MountStateMounted}, nil)

	if got := store.MountsForServer(first); len(got) != 2 {
		t.Fatalf("主服务端名下应是 alloc-a + 未归类的 alloc-legacy，实际 %+v", got)
	}
	if got := store.MountsForServer(second); len(got) != 1 || got[0].AllocationID != "alloc-b" {
		t.Fatalf("第二台名下只该有 alloc-b，实际 %+v", got)
	}
	if got := store.ListMounts(); len(got) != 3 {
		t.Fatalf("全量查询应包含所有挂载，实际 %d 条", len(got))
	}
}

// TestStateStoreResolveKey 锁定"服务端引用 → 内部键"的解析：有空兜底、未知必须落空。
func TestStateStoreResolveKey(t *testing.T) {
	store := newTestStateStore(t)
	key := store.SetSession("", &Session{
		ServerURL:        "https://10.0.0.1:8443",
		ServerInstanceID: "srv-a",
		Token:            "t",
	})

	cases := []struct {
		name      string
		key       string
		serverURL string
		want      string
	}{
		{"都省略 → 主服务端", "", "", key},
		{"实例 ID", "srv-a", "", key},
		{"规范化地址（大小写/末尾斜杠都归一）", "HTTPS://10.0.0.1:8443/", "", key},
		{"只给地址参数", "", "https://10.0.0.1:8443", key},
		{"未知键 → 落空（绝不猜一台）", "srv-unknown", "", ""},
		{"未知地址 → 落空", "", "https://10.0.0.9:8443", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := store.ResolveKey(c.key, c.serverURL); got != c.want {
				t.Fatalf("ResolveKey(%q, %q) = %q，期望 %q", c.key, c.serverURL, got, c.want)
			}
		})
	}
}

// TestStateStoreRemoveServerKeepsMounts 锁定"删掉服务端条目"的边界：状态没了，盘还在。
func TestStateStoreRemoveServerKeepsMounts(t *testing.T) {
	store := newTestStateStore(t)
	first := store.SetSession("", &Session{ServerURL: "https://10.0.0.1:8443", Token: "t1"})
	second := store.SetSession("", &Session{ServerURL: "https://10.0.0.2:8443", Token: "t2"})
	store.PutMount(&MountState{AllocationID: "alloc-a", ServerKey: first, State: MountStateMounted}, nil)

	store.RemoveServer(first)
	if _, ok := store.Server(first); ok {
		t.Fatal("删掉的条目不该还在")
	}
	if got := store.PrimaryKey(); got != second {
		t.Fatalf("主服务端应落到剩下那台，实际 %q", got)
	}
	// 盘可能还挂在本机：挂载记录必须留着，否则界面既看不到也卸载不掉。
	if mounts := store.ListMounts(); len(mounts) != 1 || mounts[0].AllocationID != "alloc-a" {
		t.Fatalf("删服务端不该连带删掉挂载记录，实际 %+v", mounts)
	}
}

// TestStateStoreMigratesLegacySingleServerFile 锁定旧状态文件（单服务端结构）的加载迁移。
func TestStateStoreMigratesLegacySingleServerFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := persistedState{
		ClientID: "cid-legacy",
		Server: &ServerState{
			URL:        "https://10.0.0.1:8443",
			InstanceID: "srv-a",
			Name:       "A",
			Connected:  true,
			Phase:      ServerPhaseConnected,
		},
		User: &UserState{ID: "u1", Username: "alice"},
		Mounts: []MountState{{
			AllocationID:     "alloc-1",
			State:            MountStateMounted,
			SessionActive:    true,
			SessionCheckedAt: 1234,
		}},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("构造旧状态文件失败：%v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("写入旧状态文件失败：%v", err)
	}

	store, err := NewStateStore(path, testLogger())
	if err != nil {
		t.Fatalf("加载旧状态文件失败：%v", err)
	}

	key := serverKeyOf("srv-a", "https://10.0.0.1:8443")
	if got := store.PrimaryKey(); got != key {
		t.Fatalf("迁移后应把唯一那台设为主服务端，实际 %q", got)
	}
	snapshots := store.Servers()
	if len(snapshots) != 1 {
		t.Fatalf("旧文件的单服务端应迁移成 1 台，实际 %d 台", len(snapshots))
	}
	snap := snapshots[0]
	if snap.Key != key || snap.User.Username != "alice" || snap.User.ID != "u1" {
		t.Fatalf("服务端键与用户应迁移过来，实际 %+v", snap)
	}
	// 连接状态必须复位：进程刚起来还没试过，不能沿用上次的"已连接"。
	if snap.State.Connected || snap.State.Phase != ServerPhaseConnecting || snap.State.FailCount != 0 {
		t.Fatalf("加载后连接状态应复位为连接中，实际 %+v", snap.State)
	}
	// 旧挂载记录没有归属 → 认给当时唯一的那台；上次进程的实测结论必须作废。
	mounts := store.MountsForServer(key)
	if len(mounts) != 1 {
		t.Fatalf("旧挂载记录应归给唯一那台，实际 %+v", mounts)
	}
	if mounts[0].ServerKey != key ||
		mounts[0].ServerURL != "https://10.0.0.1:8443" ||
		mounts[0].ServerName != "A" {
		t.Fatalf("归位时应补齐服务端标识（键/地址/名称），实际 %+v", mounts[0])
	}
	if mounts[0].SessionActive || mounts[0].SessionCheckedAt != 0 {
		t.Fatalf("上次进程的实测结论必须作废，实际 session_active=%v checked_at=%d",
			mounts[0].SessionActive, mounts[0].SessionCheckedAt)
	}
}

// TestServerViewCarriesServerAttribution 锁定对外状态视图：带服务端归属，但健康检查不给令牌。
func TestServerViewCarriesServerAttribution(t *testing.T) {
	snap := ServerSnapshot{
		Key: "srv-a",
		State: ServerState{
			URL:        "https://10.0.0.1:8443",
			InstanceID: "srv-a",
			Name:       "A",
			Alias:      "nas-1",
			Connected:  true,
			Phase:      ServerPhaseConnected,
		},
		User:    UserState{ID: "u1", Username: "alice"},
		Session: &Session{ServerURL: "https://10.0.0.1:8443", Token: "secret"},
	}

	health := serverView(snap, true, false)
	if _, ok := health["session"]; ok {
		t.Fatal("health 视图不该顺带吐出会话令牌")
	}
	if _, ok := health["user"]; ok {
		t.Fatal("health 视图不该带用户（只有 /agent/state 才给）")
	}
	if health["server_key"] != "srv-a" || health["primary"] != true || health["alias"] != "nas-1" {
		t.Fatalf("每台都应带 server_key / primary / alias，实际 %+v", health)
	}

	full := serverView(snap, false, true)
	if full["primary"] != false {
		t.Fatalf("非主服务端应 primary=false，实际 %+v", full["primary"])
	}
	if _, ok := full["user"]; !ok {
		t.Fatal("/agent/state 的每台都应带登录用户")
	}
	if _, ok := full["session"]; !ok {
		t.Fatal("/agent/state 的每台都应带会话")
	}
}

// TestHandleClearSessionPerServer 锁定本地接口的"按台登出"语义（含未知服务端的防串台护栏）。
func TestHandleClearSessionPerServer(t *testing.T) {
	a := newServerConnectTestAgent(t, nil)
	first := seedTestServer(t, a, Session{ServerURL: "https://10.0.0.1:8443", Token: "t1"})
	second := seedTestServer(t, a, Session{ServerURL: "https://10.0.0.2:8443", Token: "t2"})
	_, events := a.hub.Subscribe()

	// 未知的 server_key：必须明确报错，绝不"随便挑一台"执行。
	rec := httptest.NewRecorder()
	a.handleClearSession(rec, httptest.NewRequest(http.MethodDelete, "/agent/session?server_key=srv-unknown", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知服务端应 404，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if _, ok := a.store.Session(first); !ok {
		t.Fatal("报错时不该动任何一台的会话")
	}

	// 指定一台：只登出它，并主动广播该台的 server 事件（界面才能立刻看到状态变化）。
	rec = httptest.NewRecorder()
	a.handleClearSession(rec, httptest.NewRequest(
		http.MethodDelete, "/agent/session?server_key="+url.QueryEscape(first), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("按台登出应 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if _, ok := a.store.Session(first); ok {
		t.Fatal("指定的那台应已登出")
	}
	if _, ok := a.store.Session(second); !ok {
		t.Fatal("另一台不该被连累")
	}
	ev, ok := takeServerEvent(t, events)
	if !ok || ev.Type != "server" {
		t.Fatalf("登出后应广播 server 事件，实际 type=%q ok=%v", ev.Type, ok)
	}
	if data, _ := ev.Data.(map[string]any); data["server_key"] != first {
		t.Fatalf("事件应归属被登出的那一台，实际 %+v", ev.Data)
	}

	// 不带参数：清全部会话（老客户端只有一台，行为等价）。
	rec = httptest.NewRecorder()
	a.handleClearSession(rec, httptest.NewRequest(http.MethodDelete, "/agent/session", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("全部登出应 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if keys := a.store.Sessions(); len(keys) != 0 {
		t.Fatalf("不带参数应清全部会话，实际还剩 %v", keys)
	}
}

// TestHandleSetSessionRejectsInvalidInput 锁定会话推送的参数校验（坏输入在进入 setSession 前就被挡下）。
func TestHandleSetSessionRejectsInvalidInput(t *testing.T) {
	a := newServerConnectTestAgent(t, nil)
	cases := []struct {
		name string
		body string
	}{
		{"缺少 server_url", `{"token":"t"}`},
		{"缺少 token", `{"server_url":"https://10.0.0.1:8443"}`},
		{"地址不是 http(s)", `{"server_url":"ftp://10.0.0.1","token":"t"}`},
		{"证书指纹不是 sha256 十六进制", `{"server_url":"https://10.0.0.1:8443","token":"t","cert_sha256":"zz"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/agent/session", strings.NewReader(c.body))
			a.handleSetSession(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("应 400，实际 %d：%s", rec.Code, rec.Body.String())
			}
		})
	}
}
