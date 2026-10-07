package app

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"vault/internal/apperr"
	"vault/internal/domain"
	"vault/internal/platform"
	"vault/internal/store"
)

// 本文件锁住"删池"编排层的三道防线（用假平台 + 真 SQLite 库）：
//
//  1. 能力探测：Windows 后端（不实现 PoolAdmin）必须给出 platform.unsupported，
//     前端据此隐藏入口，而不是让用户点了才发现没实现；
//  2. 默认池：服务端所有建库/建存储的落脚点，不论前端怎么传都拒绝；
//  3. 数据库引用：池上还挂着存储时拒绝并给出数量，让用户知道"先去删哪个"。
//
// 每条被拒的用例都断言**平台层一次都没被调用**：先校验后动手是这类破坏性操作的全部意义，
// 只看错误码会漏掉"先删了再报错"这种最坏实现。

// fakePoolAdmin 是"能删池"的平台后端。
//
// StorageAdmin 用嵌入接口兜底（未用到的方法保持 nil，被调到就 panic —— 正是我们要的信号），
// 只实现删池链路真正会碰到的 DefaultPool 与 DeletePool。
type fakePoolAdmin struct {
	platform.StorageAdmin
	defVG   string
	defPool string
	rep     *platform.PoolDeleteReport
	err     error
	// calls 记录平台层收到的请求顺序；被拒的用例必须保持为空。
	calls []poolDeleteCall
}

type poolDeleteCall struct {
	vg, thinPool string
	opts         platform.PoolDeleteOptions
}

func (f *fakePoolAdmin) DefaultPool() (string, string) { return f.defVG, f.defPool }

func (f *fakePoolAdmin) DeletePool(_ context.Context, vg, thinPool string, opts platform.PoolDeleteOptions) (*platform.PoolDeleteReport, error) {
	f.calls = append(f.calls, poolDeleteCall{vg: vg, thinPool: thinPool, opts: opts})
	if f.err != nil {
		return nil, f.err
	}
	if f.rep != nil {
		return f.rep, nil
	}
	return &platform.PoolDeleteReport{VG: vg, ThinPool: thinPool, RemovedVolumeGroup: opts.RemoveVolumeGroup}, nil
}

// fakeNoPoolAdmin 只实现 StorageAdmin：模拟 Windows 后端（VHDX 直接落在 NTFS 上，没有"池"）。
type fakeNoPoolAdmin struct{ platform.StorageAdmin }

// newPoolDeleteService 造一个带真库的 PoolService。
func newPoolDeleteService(t *testing.T, admin platform.StorageAdmin) (*PoolService, *store.Store) {
	t.Helper()
	st := openAppTestStore(t)
	svc := &PoolService{Deps: Deps{
		Store:    st,
		Platform: admin,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}}
	return svc, st
}

// seedStorageOnPool 登记一个存储 + 一条底层卷记录（poolRef 为空即"多池之前的历史记录"）。
func seedStorageOnPool(t *testing.T, st *store.Store, name, poolRef, ref string) {
	t.Helper()
	ctx := context.Background()
	storage := &domain.Storage{Name: name, Path: "/srv/" + name, Enabled: true}
	if err := st.CreateStorage(ctx, storage); err != nil {
		t.Fatalf("登记存储失败：%v", err)
	}
	vol := &domain.StorageVolume{
		StorageID: storage.ID,
		Kind:      domain.StorageVolumeKindThin,
		Managed:   true,
		State:     domain.StorageVolumeStateReady,
		Ref:       ref,
		PoolRef:   poolRef,
	}
	if err := st.CreateStorageVolume(ctx, vol); err != nil {
		t.Fatalf("登记卷失败：%v", err)
	}
}

// assertPoolDeleteErr 断言错误码与参数（want 里空串表示不断言）。
func assertPoolDeleteErr(t *testing.T, err error, code, wantReason string, wantArgs map[string]any) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望错误 %s，实际成功", code)
	}
	e, ok := apperr.As(err)
	if !ok {
		t.Fatalf("错误 %v 不是 apperr", err)
	}
	if e.Code != code {
		t.Fatalf("错误码 = %s，期望 %s（err=%v）", e.Code, code, err)
	}
	if wantReason != "" && e.Args["reason"] != wantReason {
		t.Fatalf("reason = %v，期望 %s（err=%v）", e.Args["reason"], wantReason, err)
	}
	for k, want := range wantArgs {
		if got := e.Args[k]; got != want {
			t.Fatalf("参数 %s = %v，期望 %v（err=%v）", k, got, want, err)
		}
	}
}

// TestDeletePoolUnsupportedPlatform：没有 PoolAdmin 能力的平台必须给出 501 语义的错误码。
func TestDeletePoolUnsupportedPlatform(t *testing.T) {
	svc, _ := newPoolDeleteService(t, fakeNoPoolAdmin{})

	_, err := svc.DeletePool(context.Background(), DeletePoolInput{VG: "vg1", ThinPool: "pool1"})
	assertPoolDeleteErr(t, err, "platform.unsupported", "", nil)
}

// TestDeletePoolRejectsDefaultPool：默认池是服务端所有建库/建存储的落脚点。
//
// 两种给法都要挡住：点名池，或"只给卷组名 + 连卷组删"（后者同样会把默认池带走）。
func TestDeletePoolRejectsDefaultPool(t *testing.T) {
	admin := &fakePoolAdmin{defVG: "vg0", defPool: "vault"}
	svc, _ := newPoolDeleteService(t, admin)
	ctx := context.Background()

	for _, in := range []DeletePoolInput{
		{VG: "vg0", ThinPool: "vault"},
		{VG: "vg0", RemoveVolumeGroup: true},
	} {
		_, err := svc.DeletePool(ctx, in)
		assertPoolDeleteErr(t, err, "platform.pool_protected", "default_pool", nil)
	}
	if len(admin.calls) != 0 {
		t.Fatalf("默认池必须在到达平台层之前被拒，实际调用 = %+v", admin.calls)
	}
}

// TestDeletePoolRejectsStorageReferences：池上有存储时拒绝，并告诉用户还剩几个。
func TestDeletePoolRejectsStorageReferences(t *testing.T) {
	admin := &fakePoolAdmin{defVG: "vg0", defPool: "vault"}
	svc, st := newPoolDeleteService(t, admin)
	ctx := context.Background()

	seedStorageOnPool(t, st, "a", "vg1/pool1", "/dev/mapper/vg1-pool1_aaaa")
	seedStorageOnPool(t, st, "b", "vg1/pool1", "/dev/mapper/vg1-pool1_bbbb")

	_, err := svc.DeletePool(ctx, DeletePoolInput{VG: "vg1", ThinPool: "pool1"})
	assertPoolDeleteErr(t, err, "platform.pool_in_use", "storages", map[string]any{"count": 2, "vg": "vg1"})
	if len(admin.calls) != 0 {
		t.Fatalf("有存储引用时不得触到平台层，实际调用 = %+v", admin.calls)
	}
}

// TestDeletePoolOtherPoolInSameVGSameVGDoneNotBlock：同卷组**别的池**上的存储不该挡住这次删除。
//
// 这是判据最容易写错的地方：顺手用"卷引用反解卷组"代替池键比对，就会把同 VG 里
// 另一个池的存储算成目标池上的存储——用户按提示去删，却发现提示里的存储根本不在这个池上。
func TestDeletePoolOtherPoolInSameVGDoesNotBlock(t *testing.T) {
	admin := &fakePoolAdmin{defVG: "vg0", defPool: "vault"}
	svc, st := newPoolDeleteService(t, admin)
	ctx := context.Background()

	// 同一卷组里的另一个池上有存储。
	seedStorageOnPool(t, st, "busy", "vg1/pool1", "/dev/mapper/vg1-pool1_aaaa")

	if _, err := svc.DeletePool(ctx, DeletePoolInput{VG: "vg1", ThinPool: "pool2"}); err != nil {
		t.Fatalf("另一个池上的存储不应挡住 pool2 的删除：%v", err)
	}
	if len(admin.calls) != 1 || admin.calls[0].thinPool != "pool2" {
		t.Fatalf("平台层应收到 pool2 的请求，实际 = %+v", admin.calls)
	}
	// 但删整个卷组时它必须被算进来（卷组一删，pool1 上的存储就没了）。
	_, err := svc.DeletePool(ctx, DeletePoolInput{VG: "vg1", RemoveVolumeGroup: true})
	assertPoolDeleteErr(t, err, "platform.pool_in_use", "storages", map[string]any{"count": 1})
}

// TestDeletePoolLegacyVolumeWithoutPoolRef：多池之前登记的卷没有 PoolRef，只能靠卷引用反解卷组。
func TestDeletePoolLegacyVolumeWithoutPoolRef(t *testing.T) {
	admin := &fakePoolAdmin{defVG: "vg0", defPool: "vault"}
	svc, st := newPoolDeleteService(t, admin)
	ctx := context.Background()

	seedStorageOnPool(t, st, "old", "", "/dev/mapper/vg1-legacy_aaaa")

	// 目标池就在这个卷组里：它可能正住在这个池里，必须拦下（删掉在用的存储不可逆）。
	_, err := svc.DeletePool(ctx, DeletePoolInput{VG: "vg1", ThinPool: "pool2"})
	assertPoolDeleteErr(t, err, "platform.pool_in_use", "storages", map[string]any{"count": 1})

	// 别的卷组不受影响。
	if _, err := svc.DeletePool(ctx, DeletePoolInput{VG: "vg2", ThinPool: "pool2"}); err != nil {
		t.Fatalf("别的卷组的历史记录不应挡住删除：%v", err)
	}
}

// TestDeletePoolPassesThroughReport：干净的池要真的走到平台层，并把逐步结果原样带回。
func TestDeletePoolPassesThroughReport(t *testing.T) {
	admin := &fakePoolAdmin{
		defVG: "vg0", defPool: "vault",
		rep: &platform.PoolDeleteReport{
			VG: "vg1", ThinPool: "pool1", RemovedVolumeGroup: true,
			ReleasedDevices: []string{"/dev/sdb"},
			Steps:           []platform.DeviceReleaseStep{{Step: "lvremove", Target: "vg1/pool1", OK: true}},
		},
	}
	svc, _ := newPoolDeleteService(t, admin)

	rep, err := svc.DeletePool(context.Background(), DeletePoolInput{
		VG: "vg1", ThinPool: "pool1", RemoveVolumeGroup: true, ReleaseDevices: true,
	})
	if err != nil {
		t.Fatalf("DeletePool 失败：%v", err)
	}
	if len(admin.calls) != 1 {
		t.Fatalf("平台层应被调用一次，实际 = %+v", admin.calls)
	}
	// 选项必须原样透传：漏传 RemoveVolumeGroup 会让"删卷组"静默变成"只删池"。
	if got := admin.calls[0].opts; !got.RemoveVolumeGroup || !got.ReleaseDevices {
		t.Fatalf("平台层收到的选项 = %+v，期望两项都为 true", got)
	}
	if rep.VG != "vg1" || !rep.RemovedVolumeGroup || len(rep.ReleasedDevices) != 1 || len(rep.Steps) != 1 {
		t.Fatalf("报告不应被改写：%+v", rep)
	}
}

// TestDeletePoolRejectsBadInput：入参校验兜在编排层，别把"没有动作可做"的请求交给平台。
func TestDeletePoolRejectsBadInput(t *testing.T) {
	admin := &fakePoolAdmin{defVG: "vg0", defPool: "vault"}
	svc, _ := newPoolDeleteService(t, admin)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		in   DeletePoolInput
	}{
		{"空卷组名", DeletePoolInput{ThinPool: "pool1"}},
		{"空白卷组名", DeletePoolInput{VG: "   ", ThinPool: "pool1"}},
		// 只有卷组名、又不许删卷组：没有任何可执行的动作。
		{"无动作可做", DeletePoolInput{VG: "vg1"}},
	} {
		if _, err := svc.DeletePool(ctx, tc.in); apperr.CodeOf(err) != apperr.CodeInvalidParam {
			t.Fatalf("%s：期望 %s，实际 %v", tc.name, apperr.CodeInvalidParam, err)
		}
	}
	if len(admin.calls) != 0 {
		t.Fatalf("入参非法时不得触到平台层，实际调用 = %+v", admin.calls)
	}
}

// TestVolumeOnPool：判据本身（哪个卷会被这次删除带走）单独钉一遍。
func TestVolumeOnPool(t *testing.T) {
	for _, tc := range []struct {
		name      string
		poolRef   string
		ref       string
		vg        string
		thinPool  string
		want      bool
	}{
		{"目标池上的卷", "vg1/pool1", "/dev/mapper/vg1-pool1_a", "vg1", "pool1", true},
		{"同 VG 别的池的卷", "vg1/pool2", "/dev/mapper/vg1-pool2_a", "vg1", "pool1", false},
		{"别的 VG 的卷", "vg9/pool1", "/dev/mapper/vg9-pool1_a", "vg1", "pool1", false},
		{"删卷组时同 VG 各池都算", "vg1/pool2", "/dev/mapper/vg1-pool2_a", "vg1", "", true},
		{"删卷组时别的 VG 不算", "vg9/pool1", "/dev/mapper/vg9-pool1_a", "vg1", "", false},
		{"历史记录按卷引用反解", "", "/dev/mapper/vg1-legacy_a", "vg1", "pool1", true},
		{"历史记录别的 VG", "", "/dev/mapper/vg9-legacy_a", "vg1", "pool1", false},
		{"历史记录解不出来", "", "/dev/sdb1", "vg1", "pool1", false},
		// 池键坏了：无从判断落在哪，宁可拦下。
		{"池键损坏", "vg1", "/dev/mapper/vg1-a", "vg1", "pool1", true},
	} {
		v := domain.StorageVolume{PoolRef: tc.poolRef, Ref: tc.ref}
		if got := volumeOnPool(v, tc.vg, tc.thinPool); got != tc.want {
			t.Fatalf("%s：volumeOnPool(poolRef=%q, ref=%q, vg=%q, pool=%q) = %v，期望 %v",
				tc.name, tc.poolRef, tc.ref, tc.vg, tc.thinPool, got, tc.want)
		}
	}
}
