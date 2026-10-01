package app

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"vault/internal/domain"
	"vault/internal/lock"
	"vault/internal/platform"
)

// renameIscsiBackend 是一个会**改写目标名**的假后端（模拟 Windows 目标服务器的行为）。
//
// 为什么要造这么一个后端：本次故障是"服务端按自己的前缀拼 IQN 下发给客户端，而平台侧
// 存的名字不是它"，表现为 **TCP 3260 可达、但 Connect-IscsiTarget 登录失败**。
// 不模拟"平台会改写名字"，就验证不了"下发给客户端的 IQN 必须以平台实际名字为准"。
type renameIscsiBackend struct {
	platform.IscsiBackend

	mu      sync.Mutex
	pushed  []platform.TargetSpec
	actual  map[string]platform.TargetInfo
	removed []string
	// rewrite 非空时：平台把下发的目标名改写成自己的形态后存储。
	rewrite func(specName string) string
}

func newRenameBackend(rewrite func(string) string) *renameIscsiBackend {
	return &renameIscsiBackend{actual: make(map[string]platform.TargetInfo), rewrite: rewrite}
}

func (f *renameIscsiBackend) EnsureTarget(_ context.Context, spec platform.TargetSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushed = append(f.pushed, spec)
	name := spec.Name
	if f.rewrite != nil {
		name = f.rewrite(spec.Name)
	}
	devices := []string{}
	if spec.BackingRef != "" {
		devices = []string{spec.BackingRef}
	}
	f.actual[name] = platform.TargetInfo{
		Name:       name,
		Enabled:    spec.Enabled,
		Initiators: append([]string(nil), spec.Initiators...),
		Devices:    devices,
	}
	return nil
}

func (f *renameIscsiBackend) GetTarget(_ context.Context, name string) (*platform.TargetInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info, ok := f.actual[name]
	if !ok {
		return nil, nil
	}
	return &info, nil
}

func (f *renameIscsiBackend) ListTargets(context.Context) ([]platform.TargetInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]platform.TargetInfo, 0, len(f.actual))
	for _, info := range f.actual {
		out = append(out, info)
	}
	return out, nil
}

func (f *renameIscsiBackend) RemoveTarget(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, name)
	delete(f.actual, name)
	return nil
}

// seedTarget 预置一个平台侧已存在的目标（用于模拟旧命名的残留）。
func (f *renameIscsiBackend) seedTarget(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actual[name] = platform.TargetInfo{Name: name, Enabled: true}
}

func (f *renameIscsiBackend) removedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.removed...)
}

func (f *renameIscsiBackend) lastPushed() platform.TargetSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pushed) == 0 {
		return platform.TargetSpec{}
	}
	return f.pushed[len(f.pushed)-1]
}

// newIqnTestService 构造带锁的 IscsiService（Publish 需要 Locks 与 Disk 后端）。
func newIqnTestService(t *testing.T, backend platform.IscsiBackend) *IscsiService {
	t.Helper()
	return &IscsiService{Deps: Deps{
		Store: openAppTestStore(t),
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Disk:  fakeDiskBackend{exists: true},
		Iscsi: backend,
		Locks: lock.NewKeyed(),
	}}
}

// seedPublishFixture 建库 → 盘 → 分配 → 目标，返回 (目标, 分配 ID)。
func seedPublishFixture(t *testing.T, svc *IscsiService, shortName string) (*domain.IscsiTarget, string) {
	t.Helper()
	ctx := context.Background()
	repoID := seedRepo(t, svc.Store)
	disk := &domain.Disk{
		RepoID:    repoID,
		Kind:      domain.DiskKindDiff,
		VHDXPath:  filepath.Join("C:", "vault", "disk.vhdx"),
		SizeBytes: 1 << 20,
		State:     domain.DiskStateReady,
	}
	if err := svc.Store.CreateDisk(ctx, disk); err != nil {
		t.Fatalf("建磁盘失败：%v", err)
	}
	allocID := "alloc-" + disk.ID
	if err := svc.Store.CreateAllocation(ctx, &domain.Allocation{
		ID: allocID, RepoID: repoID, DiskID: disk.ID, State: domain.AllocationStateAllocated,
	}); err != nil {
		t.Fatalf("建分配失败：%v", err)
	}
	target := &domain.IscsiTarget{
		TargetName:     shortName,
		DiskID:         &disk.ID,
		AllocationID:   &allocID,
		Purpose:        domain.Purpose("user"),
		AuthMode:       domain.AuthModeNone,
		Enabled:        true,
		DesiredEnabled: true,
	}
	if err := svc.Store.CreateIscsiTarget(ctx, target); err != nil {
		t.Fatalf("建目标失败：%v", err)
	}
	return target, allocID
}

// TestPushTargetSendsFullIQN 锁住"下发给平台的目标名必须是**完整 IQN**"。
//
// 这正是本次故障的根因：以前传的是短名，平台会按自己的命名权给它起名，于是客户端拿到的
// IQN（我们拼的那个）在平台上根本不存在 —— 服务在监听、端口也通，唯独登录的目标名对不上。
func TestPushTargetSendsFullIQN(t *testing.T) {
	ctx := context.Background()
	backend := newRenameBackend(nil)
	svc := newIqnTestService(t, backend)
	target, _ := seedPublishFixture(t, svc, "vault-aaaa-bbbb")

	if err := svc.pushTarget(ctx, target, true); err != nil {
		t.Fatalf("下发失败：%v", err)
	}
	name := backend.lastPushed().Name
	if !strings.HasPrefix(name, "iqn.") {
		t.Fatalf("下发的目标名必须是完整 IQN（iqn. 开头），实际 %q", name)
	}
	if !strings.HasSuffix(name, "vault-aaaa-bbbb") {
		t.Fatalf("IQN 必须以短名结尾，实际 %q", name)
	}
	if got := svc.targetIQN(target); got != name {
		t.Fatalf("targetIQN=%q，期望 %q", got, name)
	}
}

// TestPublishUsesPlatformActualNameAsIQN 平台改写目标名时，下发给客户端的 IQN 必须以
// **平台实际存储的名字**为准 —— 否则就是"端口通、登录失败"。
func TestPublishUsesPlatformActualNameAsIQN(t *testing.T) {
	ctx := context.Background()
	const short = "vault-cccc-dddd"
	// 模拟"平台只认短名"：把我们下发的 IQN 折回 Windows 自己的命名权形态。
	backend := newRenameBackend(func(specName string) string {
		if i := strings.LastIndex(specName, ":"); i >= 0 {
			return legacyWinIQNPrefix + specName[i+1:]
		}
		return legacyWinIQNPrefix + specName
	})
	svc := newIqnTestService(t, backend)
	target, allocID := seedPublishFixture(t, svc, short)

	if _, err := svc.Publish(ctx, allocID); err != nil {
		t.Fatalf("Publish 失败：%v", err)
	}
	want := legacyWinIQNPrefix + short
	if got := svc.targetIQN(target); got != want {
		t.Fatalf("下发客户端的 IQN=%q，期望平台实际名字 %q", got, want)
	}
	// 客户端拿到的 IQN 必须能在平台上查到 —— 这才是"能登录"的前提。
	info, err := backend.GetTarget(ctx, want)
	if err != nil || info == nil {
		t.Fatalf("按目标 IQN %q 查不到目标（客户端将无法登录）：err=%v", want, err)
	}
}

// iqnOnlyBackend 模拟 Windows 的**真实**改写形态：TargetName 保持不变，真正对外暴露的
// 名字放在 TargetIqn（即 TargetInfo.IQN）里。
type iqnOnlyBackend struct {
	platform.IscsiBackend
	actual map[string]platform.TargetInfo
}

func (f *iqnOnlyBackend) GetTarget(_ context.Context, name string) (*platform.TargetInfo, error) {
	info, ok := f.actual[name]
	if !ok {
		return nil, nil
	}
	return &info, nil
}

func (f *iqnOnlyBackend) ListTargets(context.Context) ([]platform.TargetInfo, error) {
	out := make([]platform.TargetInfo, 0, len(f.actual))
	for _, info := range f.actual {
		out = append(out, info)
	}
	return out, nil
}

// TestDiscoverActualTargetNamePrefersTargetIqn 平台改写目标名时，下发给客户端的必须是
// **TargetIqn**（Windows 真正对外暴露的名字），而不是我们下发的 TargetName ——
// 否则客户端拿 TargetName 去连会 "target name is not found"（真实事故）。
func TestDiscoverActualTargetNamePrefersTargetIqn(t *testing.T) {
	const fullIQN = "iqn.2026-01.com.vault:vault-aaaa-bbbb"
	const transformed = "iqn.1991-05.com.microsoft:win-0e595h11jss-iqn.2026-01.com.vault:vault-aaaa-bbbb-target"
	backend := &iqnOnlyBackend{actual: map[string]platform.TargetInfo{
		fullIQN: {Name: fullIQN, IQN: transformed, Enabled: true},
	}}
	svc := newIqnTestService(t, backend)

	if got := svc.discoverActualTargetName(context.Background(), fullIQN, "vault-aaaa-bbbb"); got != transformed {
		t.Fatalf("应返回平台实际 IQN %q，实际 %q", transformed, got)
	}
}

// TestPublishMigratesLegacyTargetName 旧命名的目标（短名 / Windows 默认命名权形态）必须被
// 清理并按完整 IQN 重建：否则同一个 VHDX 会被两个目标争抢映射。
func TestPublishMigratesLegacyTargetName(t *testing.T) {
	ctx := context.Background()
	const short = "vault-eeee-ffff"
	backend := newRenameBackend(nil)
	backend.seedTarget(short)                      // 旧：短名
	backend.seedTarget(legacyWinIQNPrefix + short) // 旧：Windows 默认命名权形态
	svc := newIqnTestService(t, backend)
	_, allocID := seedPublishFixture(t, svc, short)

	if _, err := svc.Publish(ctx, allocID); err != nil {
		t.Fatalf("Publish 失败：%v", err)
	}
	removed := backend.removedNames()
	if len(removed) != 2 {
		t.Fatalf("两个旧命名目标都必须被清理，实际删除 %v", removed)
	}
	for _, want := range []string{short, legacyWinIQNPrefix + short} {
		found := false
		for _, name := range removed {
			if name == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("旧命名目标 %q 未被清理：%v", want, removed)
		}
	}
	// 重建用的是完整 IQN。
	if name := backend.lastPushed().Name; !strings.HasPrefix(name, "iqn.2026-01.com.vault:") {
		t.Fatalf("重建目标应使用完整 IQN，实际 %q", name)
	}
}
