package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"vault/internal/domain"
	"vault/internal/platform"
)

// fakeIscsiBackend 像一个"最小 Windows iSCSI 服务端"：
// 下发时把期望状态记下来，查询时按记忆回放；并统计下发次数。
//
// 这样就能直接验证本次修复的核心行为——
// **期望状态没变就跳过下发**（原来每次挂载都要重下发，实测约 15 秒），
// 而**状态被外部改动过就必须重新下发**。
type fakeIscsiBackend struct {
	platform.IscsiBackend

	mu        sync.Mutex
	pushCount int
	actual    map[string]platform.TargetInfo
}

func newFakeIscsiBackend() *fakeIscsiBackend {
	return &fakeIscsiBackend{actual: make(map[string]platform.TargetInfo)}
}

func (f *fakeIscsiBackend) EnsureTarget(_ context.Context, spec platform.TargetSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushCount++
	devices := []string{}
	if spec.BackingRef != "" {
		devices = []string{spec.BackingRef}
	}
	f.actual[spec.Name] = platform.TargetInfo{
		Name:       spec.Name,
		Enabled:    spec.Enabled,
		Initiators: append([]string(nil), spec.Initiators...),
		Devices:    devices,
	}
	return nil
}

func (f *fakeIscsiBackend) GetTarget(_ context.Context, name string) (*platform.TargetInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info, ok := f.actual[name]
	if !ok {
		// 目标不存在：调用方只需知道"读不到"，具体错误码不属于本测试关注点。
		return nil, errors.New("iscsi: target not found")
	}
	return &info, nil
}

func (f *fakeIscsiBackend) pushes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pushCount
}

// drift 模拟"有人在 Windows 侧直接改了目标"。
func (f *fakeIscsiBackend) drift(name string, mutate func(*platform.TargetInfo)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info := f.actual[name]
	mutate(&info)
	f.actual[name] = info
}

// newPushTestService 构造只带 Store / Log / 假后端的 IscsiService。
func newPushTestService(t *testing.T, backend platform.IscsiBackend) *IscsiService {
	t.Helper()
	return &IscsiService{Deps: Deps{
		Store: openAppTestStore(t),
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Disk:  fakeDiskBackend{exists: true},
		Iscsi: backend,
	}}
}

// TestPushTargetSkipsWhenUnchanged 锁定"挂载不再每次白等十几秒"的直接修复点：
//
//	① 首次必须下发；
//	② 期望状态与现状都没变 → **跳过**（不调用后端）；
//	③ 外部把授权改掉 → 必须重新下发（不能因为"我记得下发过"就放着不管）。
func TestPushTargetSkipsWhenUnchanged(t *testing.T) {
	ctx := context.Background()
	backend := newFakeIscsiBackend()
	svc := newPushTestService(t, backend)
	st := svc.Store

	repoID := seedRepo(t, st)
	disk := &domain.Disk{
		RepoID:    repoID,
		Kind:      domain.DiskKindDiff,
		VHDXPath:  filepath.Join("C:", "vault", "disk.vhdx"),
		SizeBytes: 1 << 20,
		State:     domain.DiskStateReady,
	}
	if err := st.CreateDisk(ctx, disk); err != nil {
		t.Fatalf("建磁盘记录失败：%v", err)
	}

	target := &domain.IscsiTarget{
		TargetName:     "vault-aaaa-bbbb",
		DiskID:         &disk.ID,
		Purpose:        domain.Purpose("user"),
		AuthMode:       domain.AuthModeNone,
		Enabled:        true,
		DesiredEnabled: true,
	}
	if err := st.CreateIscsiTarget(ctx, target); err != nil {
		t.Fatalf("建目标记录失败：%v", err)
	}

	if err := svc.pushTarget(ctx, target, true); err != nil {
		t.Fatalf("首次下发失败：%v", err)
	}
	if got := backend.pushes(); got != 1 {
		t.Fatalf("首次必须下发，实际下发次数=%d", got)
	}

	// ② 完全没变：必须跳过（这就是"每次挂载都要等十几秒"的修复点）。
	if err := svc.pushTarget(ctx, target, true); err != nil {
		t.Fatalf("重复下发失败：%v", err)
	}
	if got := backend.pushes(); got != 1 {
		t.Fatalf("状态未变化时必须跳过下发，实际下发次数=%d", got)
	}

	// ③ 外部改了授权 → 现状比对不通过 → 必须重新下发。
	//   平台侧按**完整 IQN** 寻址（见 IscsiService.targetIQN），drift 也必须用同一个名字。
	backend.drift(svc.targetIQN(target), func(info *platform.TargetInfo) {
		info.Initiators = []string{"IQN:iqn.evil"}
	})
	if err := svc.pushTarget(ctx, target, true); err != nil {
		t.Fatalf("纠正外部改动时下发失败：%v", err)
	}
	if got := backend.pushes(); got != 2 {
		t.Fatalf("现状被外部改动后必须重新下发，实际下发次数=%d", got)
	}
}

// TestPushTargetForgetsAfterRemove 目标被删除后必须丢弃指纹：
// 否则下次"以为已下发过"而跳过，目标就永远建不回来。
func TestPushTargetForgetsAfterRemove(t *testing.T) {
	ctx := context.Background()
	backend := newFakeIscsiBackend()
	svc := newPushTestService(t, backend)
	st := svc.Store

	repoID := seedRepo(t, st)
	disk := &domain.Disk{
		RepoID: repoID, Kind: domain.DiskKindDiff,
		VHDXPath: filepath.Join("C:", "vault", "disk.vhdx"), SizeBytes: 1 << 20, State: domain.DiskStateReady,
	}
	if err := st.CreateDisk(ctx, disk); err != nil {
		t.Fatalf("建磁盘记录失败：%v", err)
	}
	target := &domain.IscsiTarget{
		TargetName: "vault-cccc-dddd", DiskID: &disk.ID, Purpose: domain.Purpose("user"),
		AuthMode: domain.AuthModeNone, Enabled: true, DesiredEnabled: true,
	}
	if err := st.CreateIscsiTarget(ctx, target); err != nil {
		t.Fatalf("建目标记录失败：%v", err)
	}
	if err := svc.pushTarget(ctx, target, true); err != nil {
		t.Fatalf("下发失败：%v", err)
	}

	// 目标在 Windows 侧被删除（模拟外部删除 / RemoveTarget 之后）。
	//
	// 平台侧一律按**完整 IQN** 寻址（短名在平台上可能根本不存在，见 IscsiService.targetIQN），
	// 因此这里的清理也必须用同一个名字。
	iqn := svc.targetIQN(target)
	svc.forgetPushedSpec(iqn)
	backend.mu.Lock()
	delete(backend.actual, iqn)
	backend.mu.Unlock()

	if err := svc.pushTarget(ctx, target, true); err != nil {
		t.Fatalf("重建下发失败：%v", err)
	}
	if got := backend.pushes(); got != 2 {
		t.Fatalf("目标已被删除时必须重新下发，实际下发次数=%d", got)
	}
}

// TestTargetSpecFingerprint 指纹必须：忽略授权顺序、对内容变化敏感。
func TestTargetSpecFingerprint(t *testing.T) {
	base := platform.TargetSpec{
		Name:       "vault-aaaa-bbbb",
		Enabled:    true,
		BackingRef: `C:\vault\disk.vhdx`,
		Initiators: []string{"IQN:iqn.a", "IPAddress:10.0.0.1"},
		ChapUser:   "vault-user",
		ChapSecret: "secret-1",
	}
	same := base
	same.Initiators = []string{"IPAddress:10.0.0.1", "IQN:iqn.a"} // 仅顺序不同
	if targetSpecFingerprint(base) != targetSpecFingerprint(same) {
		t.Fatal("授权顺序变化不应改变指纹（集合语义）")
	}

	changed := []struct {
		name   string
		mutate func(*platform.TargetSpec)
	}{
		{"启用状态变化", func(s *platform.TargetSpec) { s.Enabled = false }},
		{"映射盘变化", func(s *platform.TargetSpec) { s.BackingRef = `C:\vault\other.vhdx` }},
		{"授权增减", func(s *platform.TargetSpec) { s.Initiators = []string{"IQN:iqn.a"} }},
		{"CHAP 用户名变化", func(s *platform.TargetSpec) { s.ChapUser = "other" }},
		{"CHAP 密钥轮换", func(s *platform.TargetSpec) { s.ChapSecret = "secret-2" }},
	}
	for _, c := range changed {
		t.Run(c.name, func(t *testing.T) {
			out := base
			out.Initiators = append([]string(nil), base.Initiators...)
			c.mutate(&out)
			if targetSpecFingerprint(base) == targetSpecFingerprint(out) {
				t.Fatal("期望状态变化后指纹必须不同，否则会漏下发")
			}
		})
	}
}

// TestTargetInfoMatches 现状比对只能依赖可读字段（Enabled / 授权 / 映射盘）。
func TestTargetInfoMatches(t *testing.T) {
	spec := platform.TargetSpec{
		Name:       "vault-aaaa-bbbb",
		Enabled:    true,
		BackingRef: `C:\Vault\disks\disk.vhdx`,
		Initiators: []string{"IQN:iqn.a", "IPAddress:10.0.0.1"},
	}
	base := platform.TargetInfo{
		Name:       spec.Name,
		Enabled:    true,
		Initiators: []string{"iqn:IQN.A", "IPAddress:10.0.0.1"}, // 大小写不同
		Devices:    []string{`c:/vault/DISKS/disk.vhdx`},        // 路径分隔符与大小写不同
	}
	if !targetInfoMatches(spec, &base) {
		t.Fatal("仅大小写/分隔符差异应判为一致（否则会反复无谓下发）")
	}

	cases := []struct {
		name   string
		mutate func(*platform.TargetInfo)
	}{
		{"启用状态不同", func(i *platform.TargetInfo) { i.Enabled = false }},
		{"授权少了", func(i *platform.TargetInfo) { i.Initiators = []string{"IQN:iqn.a"} }},
		{"授权多了", func(i *platform.TargetInfo) { i.Initiators = append(i.Initiators, "IQN:iqn.b") }},
		{"映射盘不在", func(i *platform.TargetInfo) { i.Devices = nil }},
		{"映射到了别的盘", func(i *platform.TargetInfo) { i.Devices = []string{`C:\vault\other.vhdx`} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := base
			out.Initiators = append([]string(nil), base.Initiators...)
			out.Devices = append([]string(nil), base.Devices...)
			c.mutate(&out)
			if targetInfoMatches(spec, &out) {
				t.Fatal("现状与期望不一致时必须判为不一致，否则会漏下发")
			}
		})
	}
}
