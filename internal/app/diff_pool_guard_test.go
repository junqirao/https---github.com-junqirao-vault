package app

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"vault/internal/apperr"
	"vault/internal/domain"
	"vault/internal/platform"
)

// fakePoolDisk 只实现"池判定"（PoolOf）与"默认池引用"（DiskRefInPool），其余方法交给嵌入的
// 接口兜底——本测试只关心选根，多写十几个空实现只会让测试更难读（同 fakeDiskBackend 的做法）。
//
// 判定规则刻意贴近真实世界：母盘/差异盘引用形如 /dev/mapper/data-...，存储根形如 /mnt/pool-a，
// 两者是完全不同的字符串，路径前缀匹配不到一起，只能靠 PoolOf 归类（这正是 bug 的成因）。
// 用后缀而非全等匹配，是为了不受 filepath.Clean 在 Windows/Linux 上的分隔符差异影响。
type fakePoolDisk struct {
	platform.DiskBackend
	// defaultRef 是 DiskRefInPool("", ...) 的答案，即"判不出归属的根最终会落到哪个池"；
	// 空串表示问不出来（后端未配置默认卷组）。
	defaultRef string
	// unresolvableRoots 里的根一律判不出池：模拟存储根落在宿主文件系统上、挂载点不是 LV。
	unresolvableRoots []string
}

func (f fakePoolDisk) PoolOf(ref string) string {
	if ref == "" {
		return ""
	}
	for _, r := range f.unresolvableRoots {
		if strings.HasSuffix(ref, filepath.Base(r)) {
			return ""
		}
	}
	switch {
	case strings.Contains(ref, "data-"), strings.HasSuffix(ref, "pool-a"):
		return "vg-data"
	case strings.HasSuffix(ref, "pool-b"):
		return "vg-test4"
	}
	return ""
}

func (f fakePoolDisk) DiskRefInPool(string, string) (string, error) {
	if f.defaultRef == "" {
		return "", errors.New("lvm vg not configured")
	}
	return f.defaultRef, nil
}

// 真实现场（2026-10 反馈"linux下创建失败了"）：母盘在 VG test4，差异盘却算到 VG data，
// lvcreate -s 报 parent_ref 无效。成因是选根只看"哪块卷空间大"——母盘引用是
// /dev/mapper/test4-...，不在任何存储根之下，GuardForPath 必然落空。
// 本用例锁住修复后的底线：母盘所在池里有根，就必须选它，哪怕别的根排在前面、空间更大。
func TestPickGuardInPoolPicksRootOfParentPool(t *testing.T) {
	ctx := context.Background()
	disk := fakePoolDisk{defaultRef: "/dev/mapper/data-probe"}
	// pool-b（另一个池）排在前面，模拟"它的卷可用空间更大"。
	guards := domain.NewPathGuardSet([]string{"/mnt/pool-b", "/mnt/pool-a"})

	got, applicable, err := pickGuardInPool(ctx, guards, disk, "vg-data", nil, 1<<30)
	if !applicable {
		t.Fatal("池归属判得出来，池语义必须适用")
	}
	if err != nil {
		t.Fatalf("母盘所在池里有根时必须选它，却报错：%v", err)
	}
	if base := filepath.Base(got.Root); base != "pool-a" {
		t.Fatalf("必须选母盘所在池的根 pool-a，实得 %q", got.Root)
	}
}

// 跨池派生是**硬失败**（thin 快照只能与原点同卷组），所以"池里没有根"绝不能降级成
// "随便挑一个池"。这里要求当场报 storage.parent_pool_unavailable（409）并带上池名，
// 让用户看到真实原因（该池的存储没启用/被删），而不是等 lvcreate 抛看不懂的 parent_ref 无效、
// 任务重试三次后失败。
func TestPickGuardInPoolRejectsCrossPoolFallback(t *testing.T) {
	ctx := context.Background()
	disk := fakePoolDisk{defaultRef: "/dev/mapper/data-probe"}
	guards := domain.NewPathGuardSet([]string{"/mnt/pool-a", "/mnt/pool-b"})

	_, applicable, err := pickGuardInPool(ctx, guards, disk, "vg-elsewhere", nil, 1<<30)
	if !applicable {
		t.Fatal("池归属判得出来，池语义必须适用")
	}
	if err == nil {
		t.Fatal("母盘所在池里没有根时必须失败，而不是退到别的池")
	}
	if code := apperr.CodeOf(err); code != "storage.parent_pool_unavailable" {
		t.Fatalf("错误码应为 storage.parent_pool_unavailable，实得 %q", code)
	}
	if st := apperr.HTTPStatus(err); st != http.StatusConflict {
		t.Fatalf("HTTP 状态应为 409，实得 %d", st)
	}
	e, ok := apperr.As(err)
	if !ok {
		t.Fatalf("应能取到业务错误详情，实得 %v", err)
	}
	if pool, _ := e.Args["pool"].(string); pool != "vg-elsewhere" {
		t.Fatalf("错误应带上池名便于排障，实得 %v", e.Args)
	}
}

// 池里没有根、但**别的池**有根时也不能被"它有根"诱惑：报的必须是池不可用。
func TestPickGuardInPoolIgnoresRootsOfOtherPool(t *testing.T) {
	ctx := context.Background()
	disk := fakePoolDisk{defaultRef: "/dev/mapper/data-probe"}

	_, applicable, err := pickGuardInPool(
		ctx, domain.NewPathGuardSet([]string{"/mnt/pool-b"}), disk, "vg-data", nil, 1<<30)
	if !applicable {
		t.Fatal("池归属判得出来，池语义必须适用")
	}
	if code := apperr.CodeOf(err); code != "storage.parent_pool_unavailable" {
		t.Fatalf("错误码应为 storage.parent_pool_unavailable，实得 %q（err=%v）", code, err)
	}
}

// 目录模式的兜底：存储根落在宿主文件系统上（判不出池），但后端默认卷组就是母盘所在池 ——
// 差异盘其实仍会建在同一个池里，必须照建。判不出来就硬报"跨池"等于把能用的部署改坏。
func TestPickGuardInPoolAcceptsUnresolvableRootOfDefaultPool(t *testing.T) {
	ctx := context.Background()
	disk := fakePoolDisk{
		defaultRef:        "/dev/mapper/data-probe",
		unresolvableRoots: []string{"/host/plain-dir"},
	}
	guards := domain.NewPathGuardSet([]string{"/host/plain-dir"})

	got, applicable, err := pickGuardInPool(ctx, guards, disk, "vg-data", nil, 1<<30)
	if !applicable {
		t.Fatal("默认池问得出来，池语义必须适用")
	}
	if err != nil {
		t.Fatalf("判不出归属的根应落到默认池，不该报跨池：%v", err)
	}
	if base := filepath.Base(got.Root); base != "plain-dir" {
		t.Fatalf("应选 /host/plain-dir，实得 %q", got.Root)
	}
}

// 连默认池都问不出来（未配置卷组）时，池语义整体不适用：必须交给调用方退回
// "路径前缀 + 可用空间"的旧行为，而不是拒建。
func TestPickGuardInPoolNotApplicableWhenNoPoolInfo(t *testing.T) {
	ctx := context.Background()
	disk := fakePoolDisk{unresolvableRoots: []string{"/host/plain-dir"}}
	guards := domain.NewPathGuardSet([]string{"/host/plain-dir"})

	_, applicable, err := pickGuardInPool(ctx, guards, disk, "vg-data", nil, 1<<30)
	if applicable {
		t.Fatalf("没有任何池信息时不该声称池语义适用（err=%v）", err)
	}
	if err != nil {
		t.Fatalf("不适用时应交给调用方兜底，而不是报错：%v", err)
	}
}
