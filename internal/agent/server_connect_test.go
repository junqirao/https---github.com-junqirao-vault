package agent

import (
	"path/filepath"
	"testing"
)

// newServerConnectTestAgent 构造一个只带状态存储、事件总线与身份存储的代理。
//
// 不启 HTTP、不碰网络：自动重连的"该不该试、试完怎么记"是纯状态机 + 事件广播，
// 测试只需要这三样。传 nil 或空身份表示"本机从未免密登录过"（未安装身份）。
func newServerConnectTestAgent(t *testing.T, ids ...*Identity) *Agent {
	t.Helper()
	store, err := NewStateStore(filepath.Join(t.TempDir(), "state.json"), testLogger())
	if err != nil {
		t.Fatalf("构造状态存储失败：%v", err)
	}
	return &Agent{logger: testLogger(), store: store, hub: NewEventHub(), identity: newTestIdentityStore(t, ids...)}
}

// newTestIdentityStore 构造只含给定身份的身份存储（完全不碰磁盘）。
//
// 入册规则与 identityStore 的加载一致：没有服务端标识的身份会被忽略 ——
// 真实启动时它同样会被丢弃，测试若把它当成"已装身份"就会与线上行为不一致。
func newTestIdentityStore(t *testing.T, ids ...*Identity) *identityStore {
	t.Helper()
	store := &identityStore{logger: testLogger(), entries: map[string]Identity{}}
	for _, id := range ids {
		if id == nil {
			continue
		}
		key := serverKeyOf(id.ServerInstanceID, id.ServerURL)
		if key == "" {
			continue
		}
		store.entries[key] = *id
	}
	if len(store.entries) > 0 {
		store.activeKey = store.firstKey()
	}
	return store
}

// takeServerEvent 非阻塞取出一条事件（Publish 同步写入带缓冲通道，调用返回后即可读到）。
func takeServerEvent(t *testing.T, events <-chan Event) (Event, bool) {
	t.Helper()
	select {
	case ev := <-events:
		return ev, true
	default:
		return Event{}, false
	}
}

// seedTestServer 登记一台服务端（走与真实启动相同的"推会话"路径），返回其 server_key。
//
// 多服务端下状态是按 server_key 存的：测试要先把台登记出来，后续的 setServerConnected /
// fail_count / 会话断言才有对象。
func seedTestServer(t *testing.T, a *Agent, session Session) string {
	t.Helper()
	key := a.store.SetSession("", &session)
	if key == "" {
		t.Fatal("登记服务端后应得到非空的 server_key")
	}
	return key
}

// seedServerWithoutSession 登记一台"有条目、无会话"的服务端 —— 等价于代理重启后从状态文件
// 加载出来的样子（见 loadServersLocked：连接状态复位、会话不落盘）。
func seedServerWithoutSession(t *testing.T, a *Agent, state ServerState) string {
	t.Helper()
	key := serverKeyOf(state.InstanceID, state.URL)
	if key == "" {
		t.Fatal("登记服务端需要实例 ID 或地址")
	}
	a.store.SetServer(key, ServerState{
		URL:        state.URL,
		InstanceID: state.InstanceID,
		Phase:      ServerPhaseConnecting,
	})
	return key
}

// serverStateOf 读某台服务端的状态（未登记即测试失败）。
func serverStateOf(t *testing.T, a *Agent, key string) ServerState {
	t.Helper()
	state, ok := a.store.Server(key)
	if !ok {
		t.Fatalf("服务端 %q 应已登记", key)
	}
	return state
}

// TestServerConnectPhaseStopsAfterTwoFailures 覆盖"失败 2 次即停手"的状态推进。
//
// 真实诉求："连接失败 2 次之后不再重试"。上限与阶段是两个必须同时正确的东西：
// 只停下但不改阶段，界面会永远显示"连接中"的转圈标签；只改阶段但不记录计数，
// 下一个 tick 又会继续重试，日志被刷满。
func TestServerConnectPhaseStopsAfterTwoFailures(t *testing.T) {
	a := newServerConnectTestAgent(t, nil)
	key := seedTestServer(t, a, Session{ServerURL: "https://10.0.0.1:8443", Token: "t"})
	if phase := serverStateOf(t, a, key).Phase; phase != ServerPhaseConnecting {
		t.Fatalf("进程刚起来时阶段应为连接中（还没试过，不是连不上），实际 %q", phase)
	}

	if exhausted := a.store.RecordServerConnectFailure(key, "boom"); exhausted {
		t.Fatal("第 1 次失败不该放弃：上限是 2 次")
	}
	server := serverStateOf(t, a, key)
	if server.FailCount != 1 || server.Phase != ServerPhaseConnecting {
		t.Fatalf("第 1 次失败后应仍在连接中且计数为 1，实际 fail_count=%d phase=%q", server.FailCount, server.Phase)
	}

	if exhausted := a.store.RecordServerConnectFailure(key, "boom"); !exhausted {
		t.Fatal("第 2 次失败应报告已达上限")
	}
	server = serverStateOf(t, a, key)
	if server.FailCount != maxServerConnectAttempts || server.Phase != ServerPhaseDisconnected {
		t.Fatalf("达上限后应停在未连接且计数为 %d，实际 fail_count=%d phase=%q",
			maxServerConnectAttempts, server.FailCount, server.Phase)
	}
	if server.Connected {
		t.Fatal("失败后不应仍标记为已连接")
	}

	// 任意一次成功（心跳、重新推会话、手动重试）都要把"连续失败"归零。
	a.store.SetServerConnected(key, true, "")
	server = serverStateOf(t, a, key)
	if !server.Connected || server.Phase != ServerPhaseConnected || server.FailCount != 0 {
		t.Fatalf("连接成功后应回到已连接且计数归零，实际 %+v", server)
	}
}

// TestNeedsServerReconnect 覆盖"这次该不该由自动重连循环出手"的全部分支。
func TestNeedsServerReconnect(t *testing.T) {
	id := &Identity{ServerURL: "https://10.0.0.1:8443"}

	t.Run("无会话且已装身份时出手", func(t *testing.T) {
		a := newServerConnectTestAgent(t, id)
		key := seedServerWithoutSession(t, a, ServerState{URL: id.ServerURL})
		if !a.needsServerReconnect(key) {
			t.Fatal("代理重启后无会话、未连接、身份还在：正是该自动重连的场景")
		}
	})

	t.Run("失败达上限后停手且手动重试可续", func(t *testing.T) {
		a := newServerConnectTestAgent(t, id)
		key := seedServerWithoutSession(t, a, ServerState{URL: id.ServerURL})
		a.store.RecordServerConnectFailure(key, "boom")
		a.store.RecordServerConnectFailure(key, "boom")
		if a.needsServerReconnect(key) {
			t.Fatal("连续失败达上限后不应再自动重连（否则日志被刷满，界面也分不清状态）")
		}

		// 手动重试（POST /agent/server/reconnect）清零计数 → 重新获得机会。
		a.store.ResetServerConnectFailures(key)
		if !a.needsServerReconnect(key) {
			t.Fatal("手动重试清零计数后应重新允许自动重连")
		}
	})

	t.Run("已有会话时不抢活", func(t *testing.T) {
		a := newServerConnectTestAgent(t, id)
		key := seedTestServer(t, a, Session{ServerURL: id.ServerURL, Token: "t"})
		if a.needsServerReconnect(key) {
			t.Fatal("已有会话时连接状态归事件订阅与心跳负责，重连循环不该重复登录")
		}
	})

	t.Run("未装身份或无服务端地址时跳过", func(t *testing.T) {
		// 本机从未免密登录（无身份文件）：连目标地址都没有，重试没有意义。
		a := newServerConnectTestAgent(t, nil)
		if a.needsServerReconnect(seedServerWithoutSession(t, a, ServerState{URL: "https://10.0.0.9:8443"})) {
			t.Fatal("本机从未免密登录（无身份文件）时连目标地址都没有，重试没有意义")
		}
		// 身份里没有服务端地址（不可归属、不入册）：同样跳过。
		b := newServerConnectTestAgent(t, &Identity{})
		if b.needsServerReconnect(seedServerWithoutSession(t, b, ServerState{URL: "https://10.0.0.9:8443"})) {
			t.Fatal("身份里没有服务端地址时同样跳过")
		}
	})

	t.Run("已连接时不重连", func(t *testing.T) {
		a := newServerConnectTestAgent(t, id)
		key := seedServerWithoutSession(t, a, ServerState{URL: id.ServerURL})
		a.store.SetServerConnected(key, true, "")
		if a.needsServerReconnect(key) {
			t.Fatal("已连接时不该再登录一次")
		}
	})
}

// TestClearSessionStopsAutoReconnect 覆盖"退出登录后不被自动登回来"。
//
// 身份文件在退出登录后**仍然存在**，若不把失败计数顶到上限，自动重连循环会立刻用
// 证书把用户登回来，用户看到的将是"登出没生效"。
func TestClearSessionStopsAutoReconnect(t *testing.T) {
	a := newServerConnectTestAgent(t, &Identity{ServerURL: "https://10.0.0.1:8443"})
	key := seedTestServer(t, a, Session{ServerURL: "https://10.0.0.1:8443", Token: "t"})

	a.store.ClearSession(key)

	if _, ok := a.store.Session(key); ok {
		t.Fatal("退出登录后会话必须清空")
	}
	server := serverStateOf(t, a, key)
	if server.Phase != ServerPhaseDisconnected || server.FailCount != maxServerConnectAttempts {
		t.Fatalf("退出后应停在未连接且计数顶满，实际 fail_count=%d phase=%q", server.FailCount, server.Phase)
	}
	if a.needsServerReconnect(key) {
		t.Fatal("退出登录后不应自动重连（否则等于把用户登回来）")
	}
}

// TestPublishServerIfChangedBroadcastsPhase 覆盖 server 事件的负载与"值未变不发"。
//
// 只推 connected 的旧实现下，"没连上、但一直在重连"与"连不上"推出去的事件长得一模一样，
// 界面只能显示"未连接"。phase / fail_count 必须随事件一起变化才有意义。
func TestPublishServerIfChangedBroadcastsPhase(t *testing.T) {
	a := newPhaseTestAgent(t)
	key := seedTestServer(t, a, Session{ServerURL: "https://10.0.0.1:8443", Token: "t"})
	_, events := a.hub.Subscribe()

	before := a.serverStateBefore(key)
	a.store.RecordServerConnectFailure(key, "boom")
	a.publishServerIfChanged(key, before)

	ev, ok := takeServerEvent(t, events)
	if !ok {
		t.Fatal("失败计数变化应广播 server 事件")
	}
	if ev.Type != "server" {
		t.Fatalf("事件类型应为 server，实际 %q", ev.Type)
	}
	data, ok := ev.Data.(map[string]any)
	if !ok {
		t.Fatalf("事件负载类型应为 map[string]any，实际 %T", ev.Data)
	}
	// 事件必须带 server_key：多服务端下界面靠它把状态更新到**那一台**，否则两台会互相覆盖。
	if data["server_key"] != key {
		t.Fatalf("server 事件应带 server_key=%q，实际 %v", key, data["server_key"])
	}
	if data["connected"] != false || data["phase"] != ServerPhaseConnecting || data["fail_count"] != 1 {
		t.Fatalf("第 1 次失败应广播 connected=false / phase=connecting / fail_count=1，实际 %+v", data)
	}

	// 第 2 次失败：connected 仍是 false，只有 phase / fail_count 变了 —— 旧实现到这里就不发了。
	before = a.serverStateBefore(key)
	a.store.RecordServerConnectFailure(key, "boom")
	a.publishServerIfChanged(key, before)

	ev, ok = takeServerEvent(t, events)
	if !ok {
		t.Fatal("仅阶段变化（connecting → disconnected）也必须广播，否则界面停在连接中")
	}
	data, _ = ev.Data.(map[string]any)
	if data["phase"] != ServerPhaseDisconnected || data["fail_count"] != maxServerConnectAttempts {
		t.Fatalf("达上限应广播 phase=disconnected / fail_count=%d，实际 %+v", maxServerConnectAttempts, data)
	}

	// 值未变：不重复发（Publish 是同步写入的，这里再读必须是空）。
	a.publishServerIfChanged(key, a.serverStateBefore(key))
	if _, ok := takeServerEvent(t, events); ok {
		t.Fatal("状态值未变化时不应广播 server 事件")
	}
}
