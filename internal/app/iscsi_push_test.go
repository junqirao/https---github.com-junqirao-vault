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
// 下发时把期望状态记下来，查询时按记忆回放；并统计下发/读回次数。
//
// 这样就能直接验证本次修复的核心行为——
// **期望状态没变就整段跳过**（原来每次挂载都要重下发一次，Windows 实测约 35 秒），
// 而且连"读回平台现状"都不做（读回同样要跑 PowerShell）。
type fakeIscsiBackend struct {
	platform.IscsiBackend

	mu        sync.Mutex
	pushCount int
	readCount int
	actual    map[string]platform.TargetInfo
}

func newFakeIscsiBackend() *fakeIscsiBackend {
	return &fakeIscsiBackend{actual: make(map[string]platform.TargetInfo)}
}

func (f *fakeIscsiBackend) RemoveTarget(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.actual, name)
	return nil
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

func (f *fakeIscsiBackend) ListTargets(context.Context) ([]platform.TargetInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]platform.TargetInfo, 0, len(f.actual))
	for _, info := range f.actual {
		out = append(out, info)
	}
	return out, nil
}

func (f *fakeIscsiBackend) GetTarget(_ context.Context, name string) (*platform.TargetInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readCount++
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

// reads 是"读回平台现状"的次数：分发路径必须为 0。
func (f *fakeIscsiBackend) reads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readCount
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

// TestPushTargetSkipsWhenUnchanged 锁定用户工单的核心诉求：
// "连一块已经连过的盘"是**分发**动作，不该再付一次全量下发（Windows 实测约 35 秒），
// 连"读回平台现状"都不该做（读回同样要跑 PowerShell）。
//
//	① 首次必须下发（分配阶段，付一次）；
//	② 期望状态没变 → 整段跳过，且**不读回平台**；
//	③ 服务重启（换一个进程、只共享 DB）→ 仍跳过：记账在 DB 里，不在内存；
//	④ 期望状态真的变了（停用）→ 必须重新下发。
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

	pushed, err := svc.pushTarget(ctx, target, true)
	if err != nil {
		t.Fatalf("首次下发失败：%v", err)
	}
	if !pushed {
		t.Fatal("首次必须下发")
	}
	if got := backend.pushes(); got != 1 {
		t.Fatalf("首次必须下发，实际下发次数=%d", got)
	}

	// ② 完全没变：必须跳过，而且一次平台读回都不做。
	pushed, err = svc.pushTarget(ctx, target, true)
	if err != nil {
		t.Fatalf("重复下发失败：%v", err)
	}
	if pushed {
		t.Fatal("状态未变化时必须跳过下发")
	}
	if got := backend.pushes(); got != 1 {
		t.Fatalf("状态未变化时必须跳过下发，实际下发次数=%d", got)
	}
	if got := backend.reads(); got != 0 {
		t.Fatalf("分发路径不该读回平台现状（读回同样要跑 PowerShell），实际读回 %d 次", got)
	}

	// ③ 重启：新进程只共享 DB，内存里什么都没有 —— 依赖持久化记账继续跳过。
	restarted := &IscsiService{Deps: Deps{
		Store: st,
		Log:   svc.Log,
		Disk:  fakeDiskBackend{exists: true},
		Iscsi: backend,
	}}
	fresh, err := st.GetIscsiTarget(ctx, target.ID)
	if err != nil {
		t.Fatalf("重启后重新加载目标失败：%v", err)
	}
	if pushed, err = restarted.pushTarget(ctx, fresh, true); err != nil || pushed {
		t.Fatalf("重启后应凭 DB 记账跳过下发，pushed=%v err=%v", pushed, err)
	}
	if got := backend.reads(); got != 0 {
		t.Fatalf("重启后的分发路径同样不该读回平台，实际读回 %d 次", got)
	}

	// ④ 期望状态变化（停用）：指纹不再命中，必须真的下发。
	if pushed, err = restarted.pushTarget(ctx, fresh, false); err != nil || !pushed {
		t.Fatalf("期望状态变化后必须重新下发，pushed=%v err=%v", pushed, err)
	}
	if got := backend.pushes(); got != 2 {
		t.Fatalf("期望状态变化后必须重新下发，实际下发次数=%d", got)
	}
}

// TestPushTargetKeepsFastPathOnExternalDrift 锁定**有意为之的取舍**：
// 平台侧被外部改动时，挂载路径不再去发现（那样每次挂载都要读回平台，又回到"每次等十几秒"）。
// 纠偏责任在对账器（ReconcileTargets：目标不存在 → 重建；启用状态不符 → 按 DB 收敛；
// 其余字段不一致 → 打日志让人看见）。
func TestPushTargetKeepsFastPathOnExternalDrift(t *testing.T) {
	ctx := context.Background()
	backend := newFakeIscsiBackend()
	svc := newPushTestService(t, backend)
	target, _ := seedPublishFixture(t, svc, "vault-eeee-ffff")

	if pushed, err := svc.pushTarget(ctx, target, true); err != nil || !pushed {
		t.Fatalf("首次下发失败：pushed=%v err=%v", pushed, err)
	}

	// 外部把授权改掉（平台侧按**完整 IQN** 寻址，见 IscsiService.targetIQN）。
	backend.drift(svc.targetIQN(target), func(info *platform.TargetInfo) {
		info.Initiators = []string{"IQN:iqn.evil"}
	})

	// 挂载路径只认记账：仍跳过，且不读回平台。
	if pushed, err := svc.pushTarget(ctx, target, true); err != nil || pushed {
		t.Fatalf("挂载路径只认记账，应当跳过：pushed=%v err=%v", pushed, err)
	}
	if got := backend.reads(); got != 0 {
		t.Fatalf("挂载路径不该读回平台，实际读回 %d 次", got)
	}

	// 对账器才是纠偏入口：启用状态不符时会强制下发（这里把平台侧停用）。
	backend.drift(svc.targetIQN(target), func(info *platform.TargetInfo) { info.Enabled = false })
	if err := svc.ReconcileTargets(ctx); err != nil {
		t.Fatalf("对账失败：%v", err)
	}
	if got := backend.pushes(); got != 2 {
		t.Fatalf("对账应把平台侧被停用的目标收敛回来，实际下发次数=%d", got)
	}
}

// TestPushTargetForgetsAfterRemove 目标被拆除后必须丢弃记账：
// 否则下次"以为已下发过"而跳过，目标就永远建不回来。
func TestPushTargetForgetsAfterRemove(t *testing.T) {
	ctx := context.Background()
	backend := newFakeIscsiBackend()
	svc := newIqnTestService(t, backend)
	target, _ := seedPublishFixture(t, svc, "vault-cccc-dddd")

	if pushed, err := svc.pushTarget(ctx, target, true); err != nil || !pushed {
		t.Fatalf("下发失败：pushed=%v err=%v", pushed, err)
	}

	// 走真实拆除入口：解映射 → 停用 → 删除平台目标 → 清记账。
	svc.TeardownTargetByShortName(ctx, target.TargetName, TeardownOptions{})
	stored, err := svc.Store.GetIscsiTarget(ctx, target.ID)
	if err != nil {
		t.Fatalf("读取目标失败：%v", err)
	}
	if stored.AppliedAt != 0 || stored.AppliedFingerprint != "" || stored.ActualIQN != "" {
		t.Fatalf("拆除后必须清掉下发记账，实际 applied_at=%d fingerprint=%q actual_iqn=%q",
			stored.AppliedAt, stored.AppliedFingerprint, stored.ActualIQN)
	}

	// 再下发一次：必须真的下发（平台上已经没有这个目标了）。
	// 拆除本身会下发一次（Enabled=false 停用），所以只比对"这一步有没有多出一次下发"。
	before := backend.pushes()
	if pushed, err := svc.pushTarget(ctx, stored, true); err != nil || !pushed {
		t.Fatalf("拆除后必须重新下发：pushed=%v err=%v", pushed, err)
	}
	if got := backend.pushes(); got != before+1 {
		t.Fatalf("目标已被拆除时必须重新下发，实际下发次数=%d（拆除前 %d）", got, before)
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

// TestTargetInfoMatchesWildcardInitiators 锁定"挂载不再每次白等 15 秒"的第二个修复点：
//
// 无白名单的目标，平台脚本会把空列表下发成通配 `IQN:*`（Windows 上空列表 = 拒绝所有），
// 于是读回的是 `["IQN:*"]` 而期望是 `[]`。若二者不被判为一致，targetInfoMatches 永远
// 为 false → pushTarget 的"已下发就跳过"失效 → 每次挂载都重付一次全量下发。
func TestTargetInfoMatchesWildcardInitiators(t *testing.T) {
	spec := platform.TargetSpec{
		Name:       "vault-aaaa-bbbb",
		Enabled:    true,
		BackingRef: `C:\Vault\disks\disk.vhdx`,
		Initiators: nil, // 未配置白名单 = 不限制
	}
	info := platform.TargetInfo{
		Name:       spec.Name,
		Enabled:    true,
		Initiators: []string{"IQN:*"}, // 平台把"不限制"落成了通配
		Devices:    []string{`C:\Vault\disks\disk.vhdx`},
	}
	if !targetInfoMatches(spec, &info) {
		t.Fatal("通配 IQN:* 与空白名单语义相同，必须判为一致（否则每次挂载都重下发）")
	}

	// 反向：期望"不限制"，实际却挂着具体白名单 → 必须重新下发去纠正。
	stale := info
	stale.Initiators = []string{"IQN:iqn.stale"}
	if targetInfoMatches(spec, &stale) {
		t.Fatal("实际挂着具体白名单与期望的\"不限制\"不一致，必须重新下发")
	}
}
