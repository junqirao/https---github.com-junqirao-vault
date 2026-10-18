package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"vault/internal/platform/iscsiinitiator"
)

// newCacheTestStore 造一个独立的本地配置存储（配了缓存预算）。
func newCacheTestStore(t *testing.T) *ConfigStore {
	t.Helper()
	store, err := NewConfigStore(filepath.Join(t.TempDir(), "agent-config.json"), testLogger())
	if err != nil {
		t.Fatalf("构造配置存储失败：%v", err)
	}
	return store
}

// TestCacheQuotaSplitsClientBudget 锁定配额口径：**客户端总量在开缓存的库之间按库容量分摊**。
//
// 用户只在客户端层面设置 L1/L2 最大空间（"一个客户端一个代理"），每个库分到的是它按容量
// 占比应得的一份；算错会让每个库都按满额占用，多个库一起开就把内存/磁盘吃穿。
func TestCacheQuotaSplitsClientBudget(t *testing.T) {
	store := newCacheTestStore(t)
	l1, l2 := int64(4)<<30, int64(16)<<30
	if _, err := store.Patch(ConfigPatch{CacheL1Bytes: &l1, CacheL2Bytes: &l2}); err != nil {
		t.Fatalf("Patch 缓存预算失败：%v", err)
	}
	m := newCacheManager(store, testLogger())

	enable := func(repoID string) {
		t.Helper()
		if _, err := store.SetRepoMountPref(repoID, RepoMountPref{CacheEnabled: true}); err != nil {
			t.Fatalf("启用 %s 的缓存失败：%v", repoID, err)
		}
	}
	req := func(repoID string, size int64) cacheTargetRequest {
		return cacheTargetRequest{AllocationID: "alloc-" + repoID, RepoID: repoID, RepoSizeBytes: size}
	}

	// 只有自己一个库开缓存时，配额就是总量（库容量比预算大，不触发封顶）。
	enable("repo-a")
	if got1, got2 := m.quotaFor(req("repo-a", 1000<<30)); got1 != l1 || got2 != l2 {
		t.Fatalf("1 个库时配额应等于总量：%d/%d", got1, got2)
	}

	// 容量 10:1 的两个库：份额也该是 10:1，而不是各占一半。
	// 两个库的容量都已知（正常路径见 automount.go 的 primeRepoSizeIndex：挂载前先补齐索引，
	// 否则先挂的库会按"平均容量"占一份，后挂的只能捡剩下的）。
	enable("repo-b")
	m.rememberRepoSize("repo-a", 1000<<30)
	m.rememberRepoSize("repo-b", 100<<30)
	gotA, _ := m.quotaFor(req("repo-a", 1000<<30))
	gotB, _ := m.quotaFor(req("repo-b", 100<<30))
	if gotA <= gotB*8 || gotA >= gotB*12 {
		t.Fatalf("1000 GiB 与 100 GiB 的库应分到约 10:1：%d/%d", gotA, gotB)
	}
	if gotA+gotB > l1 {
		t.Fatalf("两个库合计 %d 超过 L1 总量 %d", gotA+gotB, l1)
	}

	// 关掉缓存开关的库既不参与分摊，也不该挤占别人的份额。
	if _, err := store.SetRepoMountPref("repo-b", RepoMountPref{CacheEnabled: false}); err != nil {
		t.Fatalf("关闭 repo-b 的缓存失败：%v", err)
	}
	if got1, _ := m.quotaFor(req("repo-a", 1000<<30)); got1 != l1 {
		t.Fatalf("关闭一个库后配额应回到总量：%d", got1)
	}

	// L2 显式设为 0 = 关闭 L2：配额为 0，调用方据此不启用 L2（合法值，不是"没配"）。
	zero := int64(0)
	if _, err := store.Patch(ConfigPatch{CacheL2Bytes: &zero}); err != nil {
		t.Fatalf("关闭 L2 失败：%v", err)
	}
	if _, got2 := m.quotaFor(req("repo-a", 1000<<30)); got2 != 0 {
		t.Fatalf("L2 设为 0 时配额应为 0，实际 %d", got2)
	}
}

// TestCacheBudgetBoundsAndL2Off 锁定预算的上下界与"0 表示关闭 L2"的语义。
func TestCacheBudgetBoundsAndL2Off(t *testing.T) {
	store := newCacheTestStore(t)
	cases := []struct {
		name  string
		patch ConfigPatch
		ok    bool
	}{
		{"L1 太小", ConfigPatch{CacheL1Bytes: ptrInt64(cacheMinL1Bytes - 1)}, false},
		{"L1 太大", ConfigPatch{CacheL1Bytes: ptrInt64(cacheMaxL1Bytes + 1)}, false},
		{"L1 合法", ConfigPatch{CacheL1Bytes: ptrInt64(cacheMinL1Bytes)}, true},
		{"L2 非零但太小", ConfigPatch{CacheL2Bytes: ptrInt64(cacheMinL2Bytes - 1)}, false},
		{"L2 太大", ConfigPatch{CacheL2Bytes: ptrInt64(cacheMaxL2Bytes + 1)}, false},
		{"L2 关闭（0）", ConfigPatch{CacheL2Bytes: ptrInt64(0)}, true},
		{"L2 合法", ConfigPatch{CacheL2Bytes: ptrInt64(cacheMinL2Bytes)}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := store.Patch(c.patch)
			if c.ok && err != nil {
				t.Fatalf("应被接受，实际报错 %v", err)
			}
			if !c.ok && err == nil {
				t.Fatal("越界值应被拒绝")
			}
		})
	}

	// 默认值：全新安装即有预算（用户开缓存时不需要先填数字）。
	cfg := newCacheTestStore(t).Get()
	if cfg.CacheL1Bytes != cacheDefaultL1Bytes || cfg.CacheL2Bytes != cacheDefaultL2Bytes {
		t.Fatalf("默认预算 = %d/%d，期望 %d/%d",
			cfg.CacheL1Bytes, cfg.CacheL2Bytes, cacheDefaultL1Bytes, cacheDefaultL2Bytes)
	}
}

func ptrInt64(v int64) *int64 { return &v }

// TestCacheL2DirValidation 锁定 L2 目录的取值规则：空串合法（用默认位置），
// 绝对路径合法，相对路径必须被拒（否则缓存文件会随进程工作目录漂移到意想不到的地方）。
func TestCacheL2DirValidation(t *testing.T) {
	store := newCacheTestStore(t)
	absDir := t.TempDir()

	cases := []struct {
		name  string
		value string
		ok    bool
	}{
		{"空串（默认位置）", "", true},
		{"绝对路径", absDir, true},
		{"相对路径", filepath.Join("cache", "l2"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			value := c.value
			_, err := store.Patch(ConfigPatch{CacheL2Dir: &value})
			if c.ok && err != nil {
				t.Fatalf("应被接受，实际报错 %v", err)
			}
			if !c.ok && err == nil {
				t.Fatal("相对路径应被拒绝")
			}
		})
	}

	// 合法值必须落进配置（改目录要真的生效，而不是静默丢弃）。
	if got := store.Get().CacheL2Dir; got != absDir {
		t.Fatalf("cache_l2_dir = %q，期望 %q", got, absDir)
	}
}

// TestCacheTargetIQNAndPaths 锁定目标 IQN / L2 文件的命名规则。
func TestCacheTargetIQNAndPaths(t *testing.T) {
	iqn := cacheTargetIQN("alloc-1")
	if !strings.HasPrefix(iqn, cacheIQNPrefix) {
		t.Fatalf("本地目标 IQN 应以 %q 开头，实际 %q", cacheIQNPrefix, iqn)
	}
	if short := iqn[strings.Index(iqn, ":")+1:]; !strings.HasPrefix(short, "vcache-") {
		t.Fatalf("本地目标短名应以 vcache- 开头（避免与服务端短名串台），实际 %q", short)
	}

	// 分配 ID 里的危险字符（':' 是 Windows 的驱动器分隔符与 NTFS 交替数据流标记）必须被滤掉。
	got := sanitizeIQNSuffix("Alloc:1/Bad ..\\x")
	if got != "alloc1bad..x" {
		t.Fatalf("sanitizeIQNSuffix = %q，期望 alloc1bad..x", got)
	}
	if strings.ContainsAny(sanitizeIQNSuffix("a:b/c\\d"), `:/\`) {
		t.Fatalf("清理后的后缀不应含分隔符：%q", sanitizeIQNSuffix("a:b/c\\d"))
	}

	path := cacheL2Path("", "alloc:1")
	if filepath.Base(filepath.Dir(path)) != "iscsi-cache" {
		t.Fatalf("L2 文件应放在 iscsi-cache 目录下，实际 %q", path)
	}
	if filepath.Base(path) != "l2-alloc1.bin" {
		t.Fatalf("L2 文件名 = %q，期望 l2-alloc1.bin", filepath.Base(path))
	}

	// 用户自定义目录（客户端设置里的 cache_l2_dir）必须原样生效。
	custom := t.TempDir()
	if got := cacheL2Path(custom, "alloc:1"); filepath.Dir(got) != custom {
		t.Fatalf("自定义 L2 目录未生效：%q（期望目录 %q）", got, custom)
	}
}

// TestCacheStatusWithoutPortal 门户没打开时（没有任何库启用缓存）状态必须如实为空闲。
func TestCacheStatusWithoutPortal(t *testing.T) {
	store := newCacheTestStore(t)
	l1 := int64(2) << 30
	if _, err := store.Patch(ConfigPatch{CacheL1Bytes: &l1}); err != nil {
		t.Fatalf("Patch 失败：%v", err)
	}
	m := newCacheManager(store, testLogger())

	st := m.Status()
	if st.Running {
		t.Fatal("没有库启用缓存时门户不应在运行（懒启动）")
	}
	if st.Targets == nil || len(st.Targets) != 0 {
		t.Fatalf("未运行时 targets 应为空数组，实际 %+v", st.Targets)
	}
	if st.L1LimitBytes != l1 || st.Addr != "" {
		t.Fatalf("状态应回显客户端预算且无监听地址：%+v", st)
	}

	// ReleaseTarget 在门户未打开时必须是安全空操作（卸载路径会无条件调它）。
	if err := m.ReleaseTarget("alloc-1"); err != nil {
		t.Fatalf("未打开门户时 ReleaseTarget 应为空操作，实际 %v", err)
	}
}

// TestClearTargetWithoutPortal 门户没打开时清缓存必须是安全空操作并如实回 false：
// 界面据此提示"当前没有缓存可清"，而不是报一句"已清空"。
func TestClearTargetWithoutPortal(t *testing.T) {
	store := newCacheTestStore(t)
	m := newCacheManager(store, testLogger())
	if m.ClearTarget("alloc-1") {
		t.Fatal("门户未打开时不应报告已清空")
	}
	if m.ClearTarget("   ") {
		t.Fatal("空分配 ID 不应报告已清空")
	}
	// 未初始化（单测里直接构造 Agent）时同样不能 panic。
	var zero *cacheManager
	if zero.ClearTarget("alloc-1") {
		t.Fatal("零值管理器不应报告已清空")
	}
}

// TestHandleCacheClear 锁定清空缓存的入参与"没有目标可清"的语义。
//
// 缓存目标按分配注册，界面手里只有库 ID：这里覆盖库没挂载（没有目标，如实回
// cleared=false，不是错误）与两个 ID 都不给（请求本身不合法）两种情形。
func TestHandleCacheClear(t *testing.T) {
	a := newServerConnectTestAgent(t)

	bare := httptest.NewRecorder()
	a.handleCacheClear(bare, httptest.NewRequest(http.MethodPost, "/agent/cache/clear", strings.NewReader(`{}`)))
	if bare.Code != http.StatusBadRequest {
		t.Fatalf("两个 ID 都不给应 400，实际 %d：%s", bare.Code, bare.Body.String())
	}

	// 库有挂载记录，但缓存管理器未初始化（等于门户没起来）：没有目标可清。
	a.store.PutMount(&MountState{RepoID: "repo-1", AllocationID: "alloc-1"}, &mountRuntime{})
	rec := httptest.NewRecorder()
	a.handleCacheClear(rec, httptest.NewRequest(http.MethodPost, "/agent/cache/clear",
		strings.NewReader(`{"repo_id":"repo-1"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("没有目标可清不该报错，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var got cacheClearResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON：%v（%s）", err, rec.Body.String())
	}
	if got.Cleared || len(got.AllocationIDs) != 0 {
		t.Fatalf("没有目标可清时必须如实回 false，实际 %+v", got)
	}

	// 未知库同理：没有挂载记录 = 没有目标。
	rec = httptest.NewRecorder()
	a.handleCacheClear(rec, httptest.NewRequest(http.MethodPost, "/agent/cache/clear",
		strings.NewReader(`{"repo_id":"repo-unknown"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("未知库不该报错，实际 %d：%s", rec.Code, rec.Body.String())
	}
}

// TestLocalConnectAuthKeepsCHAPWithCache 锁定：v2 透传代理下本机连接**保留** CHAP。
//
// v2 代理不终止 iSCSI 会话、不参与认证（见 cacheproxy.go 文件头）：本机发起端到代理只是
// TCP 透传，CHAP 由发起端与服务端端到端协商。所以缓存生效与否，本机连接都应照搬服务端下发
// 的认证（chap 就带 chap），摘掉它反而会被目标以"未鉴权"拒绝。
// （v1 终止式代理相反：代理面向回环只提供 None，必须摘 CHAP —— 那是旧行为，已废弃。）
func TestLocalConnectAuthKeepsCHAPWithCache(t *testing.T) {
	chap := &MountSpec{AuthMode: "chap", ChapUser: "vault-abc", ChapSecret: "secret"}

	if mode, user, secret := localConnectAuth(chap); mode != iscsiinitiator.AuthModeCHAP ||
		user != "vault-abc" || secret != "secret" {
		t.Fatalf("缓存生效时也必须保留 CHAP（端到端透传）：mode=%q user=%q secret=%q", mode, user, secret)
	}

	// ip 模式（服务端按白名单过滤）在客户端侧是"无认证连接"。
	if mode, _, _ := localConnectAuth(&MountSpec{AuthMode: "ip"}); mode != iscsiinitiator.AuthModeNone {
		t.Fatalf("ip 模式应为无认证连接，实际 %q", mode)
	}
}

// TestMountCacheSatisfied 锁定"既有挂载记录的门户形态是否满足当前配置"。
//
// 判据用 CacheActive：上次"想经代理却回退直连"的记录不该被幂等命中，否则用户永远等不到
// 代理生效（回退原因可能是暂时性的：端口被占、后端一时不可达）。直连模式则相反，期望不经代理。
func TestMountCacheSatisfied(t *testing.T) {
	// 判据只读每库配置（repoPref → cfg.Get().RepoMounts），不需要状态存储/事件总线/锁表。
	req := MountRequest{AllocationID: "alloc-1", RepoID: "repo-a"}
	cases := []struct {
		name      string
		hasPref   bool
		direct    bool
		prefCache bool
		// recordActive/recordCache 是既有 MountState 里记的值。
		recordActive bool
		recordCache  bool
		want         bool
	}{
		{"未启用缓存 + 经代理（期望形态）", true, false, false, true, false, true},
		{"未启用缓存 + 回退直连（需重试）", true, false, false, false, false, false},
		{"启用缓存 + 代理已生效", true, false, true, true, true, true},
		{"启用缓存 + 回退直连（需重试）", true, false, true, false, false, false},
		{"直连模式 + 未走代理（期望形态）", true, true, false, false, false, true},
		{"直连模式 + 却走了代理（需重挂）", true, true, false, true, false, false},
		{"缓存开关变了（记录未开、现要求开）", true, false, true, true, false, false},
		{"没有该库偏好 + 却直连（需重挂为经代理）", false, false, false, false, false, false},
		{"没有该库偏好 + 经代理但记录未开缓存（需重挂）", false, false, false, true, false, false},
		{"没有该库偏好 + 经代理且缓存已开（默认开启的期望形态）", false, false, false, true, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 每次重建配置：SetRepoMountPref 是整条替换。
			s := newCacheTestStore(t)
			if c.hasPref {
				if _, err := s.SetRepoMountPref("repo-a", RepoMountPref{
					CacheEnabled: c.prefCache,
					DirectMode:   c.direct,
				}); err != nil {
					t.Fatalf("写偏好失败：%v", err)
				}
			}
			aa := &Agent{logger: testLogger(), cfg: s}
			aa.engine = &mountEngine{a: aa}
			got := aa.engine.mountCacheSatisfied(req, MountState{
				AllocationID: "alloc-1",
				CacheActive:  c.recordActive,
				CacheEnabled: c.recordCache,
			})
			if got != c.want {
				t.Fatalf("mountCacheSatisfied = %v，期望 %v", got, c.want)
			}
		})
	}
}
