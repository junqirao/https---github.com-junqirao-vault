package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"vault/internal/apperr"
	"vault/internal/platform"
)

// fakeStorageVolumeBackend 只实现本测试用到的两个方法；其余由嵌入的接口兜底
// （嵌入接口而非全部实现，是为了让"到底哪几步被调用"一眼可见）。
type fakeStorageVolumeBackend struct {
	platform.StorageVolumeBackend
	// err 非空时 CreateStorageVolume 直接返回它（模拟平台层拒绝创建）。
	err error
	// createCalls / deleteCalls 记录调用次数：前者证明编排层真的走到了平台层，
	// 后者证明失败后的补偿回滚确实执行了。
	createCalls int
	deleteCalls int
}

func (f *fakeStorageVolumeBackend) CreateStorageVolume(_ context.Context, spec platform.StorageVolumeSpec) (*platform.StorageVolumeStatus, error) {
	f.createCalls++
	if f.err != nil {
		return nil, f.err
	}
	return &platform.StorageVolumeStatus{
		Exists:     true,
		Mounted:    true,
		Ref:        spec.Ref,
		MountPoint: spec.MountPoint,
		SizeBytes:  spec.SizeBytes,
	}, nil
}

func (f *fakeStorageVolumeBackend) DeleteStorageVolume(context.Context, string) error {
	f.deleteCalls++
	return nil
}

// fakeRefDiskBackend 只回答 DiskRef（Linux 的 thin 路径会先向磁盘后端要引用）。
type fakeRefDiskBackend struct {
	platform.DiskBackend
	ref string
}

func (f fakeRefDiskBackend) DiskRef(string, string) (string, error) { return f.ref, nil }

// TestCreateStoragePropagatesPoolMissing 钉住 dev212 上真实踩到的那条链：
//
// 存储池还没初始化时，创建存储必须把平台层的 platform.pool_missing **原样**透传给 API
// （前端据此提示"先去「系统设置 → LVM 存储池」初始化"），而不是被包装成别的码；
// 同时 Create 的补偿回滚必须清干净，否则用户会看到一条永远也创建不出来的存储记录。
//
// 本用例跨平台可跑：平台差异只体现在被替换掉的后端实现里。
func TestCreateStoragePropagatesPoolMissing(t *testing.T) {
	ctx := context.Background()
	st := openAppTestStore(t)

	poolErr := apperr.New("platform.pool_missing", http.StatusServiceUnavailable).
		WithArg("vg", "vg0").
		WithArg("thin_pool", "vault")
	vol := &fakeStorageVolumeBackend{err: poolErr}

	svc := &StorageService{Deps: Deps{
		Store: st,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Disk:  fakeRefDiskBackend{ref: "/dev/mapper/vg0-storages_e2e"},
		// StorageVol 非 nil 即代表"该平台有存储卷概念"（Linux），默认模式为 thin。
		StorageVol: vol,
	}}

	// 显式给绝对路径：Linux 的默认挂载点推导在 Windows 上不是绝对路径。
	_, err := svc.Create(ctx, CreateStorageInput{
		Name:      "pool-missing",
		Path:      t.TempDir(),
		Mode:      StorageModeThin,
		SizeBytes: 64 << 20,
		Enabled:   true,
	})
	if err == nil {
		t.Fatal("平台层拒绝创建时必须报错")
	}
	if vol.createCalls != 1 {
		t.Fatalf("平台层应被调用恰好 1 次，实际 %d 次", vol.createCalls)
	}
	if got := apperr.CodeOf(err); got != poolErr.Code {
		t.Fatalf("错误码 = %q，期望 %q（必须原样透传，不能被包成其它码）：%v", got, poolErr.Code, err)
	}
	e, ok := apperr.As(err)
	if !ok {
		t.Fatalf("应为业务错误（前端按 code 翻译文案）：%v", err)
	}
	if e.Args["vg"] != "vg0" || e.Args["thin_pool"] != "vault" {
		t.Fatalf("插值参数必须保留（前端文案要显示是哪个池）：%v", e.Args)
	}

	if vol.deleteCalls != 1 {
		t.Fatalf("失败后的补偿回滚必须删掉本次登记的卷，deleteCalls = %d", vol.deleteCalls)
	}
	list, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("列存储失败：%v", err)
	}
	if len(list) != 0 {
		t.Fatalf("失败后不应残留存储记录，实际 %d 条", len(list))
	}
	vols, err := st.ListStorageVolumes(ctx)
	if err != nil {
		t.Fatalf("列存储卷失败：%v", err)
	}
	if len(vols) != 0 {
		t.Fatalf("失败后不应残留卷登记，实际 %d 条", len(vols))
	}
}
