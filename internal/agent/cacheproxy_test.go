package agent

import (
	"path/filepath"
	"strings"
	"testing"
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

// TestCacheQuotaSplitsClientBudget 锁定配额口径：**客户端总量 ÷ 已启用缓存的库数**。
//
// 用户只在客户端层面设置 L1/L2 最大空间（"一个客户端一个代理"），每个库分到的是它的一份；
// 算错会让每个库都按满额占用，多个库一起开就把内存/磁盘吃穿。
func TestCacheQuotaSplitsClientBudget(t *testing.T) {
	store := newCacheTestStore(t)
	l1, l2 := int64(4)<<30, int64(16)<<30
	if _, err := store.Patch(ConfigPatch{CacheL1Bytes: &l1, CacheL2Bytes: &l2}); err != nil {
		t.Fatalf("Patch 缓存预算失败：%v", err)
	}
	m := newCacheManager(store, testLogger())

	// 没有任何库启用缓存：按 1 计（防除零），实际不会被用到。
	if got1, got2 := m.quota(); got1 != l1 || got2 != l2 {
		t.Fatalf("未启用任何库时配额 = %d/%d，期望 %d/%d", got1, got2, l1, l2)
	}

	enable := func(repoID string) {
		t.Helper()
		if _, err := store.SetRepoMountPref(repoID, RepoMountPref{CacheEnabled: true}); err != nil {
			t.Fatalf("启用 %s 的缓存失败：%v", repoID, err)
		}
	}
	enable("repo-a")
	if got1, got2 := m.quota(); got1 != l1 || got2 != l2 {
		t.Fatalf("1 个库时配额应等于总量：%d/%d", got1, got2)
	}
	enable("repo-b")
	if got1, got2 := m.quota(); got1 != l1/2 || got2 != l2/2 {
		t.Fatalf("2 个库时配额应各占一半：%d/%d", got1, got2)
	}
	// 关掉缓存开关的库不计入分母。
	if _, err := store.SetRepoMountPref("repo-b", RepoMountPref{CacheEnabled: false}); err != nil {
		t.Fatalf("关闭 repo-b 的缓存失败：%v", err)
	}
	if got1, _ := m.quota(); got1 != l1 {
		t.Fatalf("关闭一个库后配额应回到总量：%d", got1)
	}

	// L2 显式设为 0 = 关闭 L2：配额为 0，调用方据此不启用 L2（合法值，不是"没配"）。
	zero := int64(0)
	if _, err := store.Patch(ConfigPatch{CacheL2Bytes: &zero}); err != nil {
		t.Fatalf("关闭 L2 失败：%v", err)
	}
	if _, got2 := m.quota(); got2 != 0 {
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

// TestMountCacheSatisfied 锁定"既有挂载记录的缓存形态是否满足当前开关"。
//
// 判据用 CacheActive：上次"想开却回退直连"的记录不该被幂等命中，否则用户开了缓存却永远
// 等不到它生效（回退原因可能是暂时性的：端口被占、后端一时不可达）。
func TestMountCacheSatisfied(t *testing.T) {
	// 判据只读每库配置（repoPref → cfg.Get().RepoMounts），不需要状态存储/事件总线/锁表。
	req := MountRequest{AllocationID: "alloc-1", RepoID: "repo-a"}
	cases := []struct {
		name        string
		prefEnabled bool
		hasPref     bool
		active      bool
		want        bool
	}{
		{"未启用 + 直连", false, true, false, true},
		{"未启用 + 却是缓存（需回退直连）", false, true, true, false},
		{"已启用 + 缓存生效", true, true, true, true},
		{"已启用 + 回退直连（需重试）", true, true, false, false},
		{"没有该库偏好 + 直连", false, false, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 每次重建配置：SetRepoMountPref 是整条替换。
			s := newCacheTestStore(t)
			if c.hasPref {
				if _, err := s.SetRepoMountPref("repo-a", RepoMountPref{CacheEnabled: c.prefEnabled}); err != nil {
					t.Fatalf("写偏好失败：%v", err)
				}
			}
			aa := &Agent{logger: testLogger(), cfg: s}
			aa.engine = &mountEngine{a: aa}
			got := aa.engine.mountCacheSatisfied(req, MountState{AllocationID: "alloc-1", CacheActive: c.active})
			if got != c.want {
				t.Fatalf("mountCacheSatisfied = %v，期望 %v", got, c.want)
			}
		})
	}
}
