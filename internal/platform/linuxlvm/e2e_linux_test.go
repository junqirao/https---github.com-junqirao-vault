//go:build linux

package linuxlvm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vault/internal/apperr"
	"vault/internal/platform"
)

// TestE2ERealLVM 是**真机**端到端用例：用一块临时 loop 设备搭出真实 LVM 池，
// 一次跑完 dev212 上出问题的那条链：
//
//	工具链探测 → 初始化池（幂等）→ 水位闸门 → 建存储卷（thin LV + mkfs + 挂载 + 真读写）
//	→ 状态查询 → 物理占用 → 池缺失回归断言 → 删除存储卷（幂等）
//
// 默认跳过（需要 root + lvm2，且会动内核 loop 设备）。开启方式：
//
//	sudo VAULT_LVM_E2E=1 go test -count=1 -v -run TestE2ERealLVM ./internal/platform/linuxlvm/
//
// 隔离性：全程只用本用例新建的镜像文件对应的 loop 设备，结束按"池 → VG → PV → loop"逆序拆净，
// 不触碰宿主上任何既有 VG/PV；任何一步已存在都会走 LVM 自身的幂等分支。
func TestE2ERealLVM(t *testing.T) {
	if os.Getenv("VAULT_LVM_E2E") != "1" {
		t.Skip("真机 E2E 默认关闭：VAULT_LVM_E2E=1 且需 root")
	}
	if os.Geteuid() != 0 {
		t.Skip("真机 E2E 需要 root（losetup / pvcreate / mount）")
	}
	for _, exe := range []string{"lvm", "losetup", "mkfs.ext4", "vgremove", "pvremove"} {
		if _, err := LookPath(exe); err != nil {
			t.Skipf("真机 E2E 缺少命令 %s：%v", exe, err)
		}
	}

	const (
		vgName   = "vaulte2e"
		poolName = "vault"
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dir := t.TempDir()
	dev := attachLoop(t, dir, 1<<30)
	// 逆序拆除：thin pool → VG → PV（loop 设备由 attachLoop 注册的 Cleanup 最后拆）。
	// 失败只告警：清理问题不应掩盖主断言。
	defer func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer ccancel()
		runLVMQuiet(t, cctx, "lvremove", "-f", vgName+"/"+poolName)
		runLVMQuiet(t, cctx, "vgremove", "-f", vgName)
		runLVMQuiet(t, cctx, "pvremove", "-f", dev)
	}()

	m := New(Options{
		VG:           vgName,
		ThinPool:     poolName,
		ChunkSize:    "256K",
		MetadataSize: "64M",
		Timeout:      5 * time.Minute,
		Logger:       discardLogger(),
	})

	// ① 工具链探测。
	if err := m.Available(ctx); err != nil {
		t.Fatalf("LVM 工具链不可用：%v", err)
	}

	// ② 初始化池，并验证幂等（重复调用不得报错、不得覆盖）。
	poolSpec := platform.PoolSpec{
		VG: vgName, ThinPool: poolName, HDDDevices: []string{dev},
		ChunkSize: "256K", MetadataSize: "64M",
	}
	if err := m.InitializePool(ctx, poolSpec); err != nil {
		t.Fatalf("初始化存储池失败：%v", err)
	}
	if err := m.InitializePool(ctx, platform.PoolSpec{VG: vgName, ThinPool: poolName}); err != nil {
		t.Fatalf("重复初始化应幂等成功：%v", err)
	}
	st, err := m.Status(ctx)
	if err != nil {
		t.Fatalf("查存储池状态失败：%v", err)
	}
	if !st.Exists {
		t.Fatalf("池已初始化但 Status.Exists=false：%+v", st)
	}
	if st.VG != vgName || st.ThinPool != poolName {
		t.Fatalf("池标识不对：%+v", st)
	}
	if st.SizeBytes <= 0 {
		t.Fatalf("VG 容量应 > 0：%+v", st)
	}

	// ③ 水位闸门：池存在、水位可读时必须放行（这条路径以前只在"读失败"上有覆盖）。
	if err := m.checkWatermark(ctx, vgName); err != nil {
		t.Fatalf("池正常时水位闸门应放行：%v", err)
	}

	// ④ 建存储卷：thin LV → mkfs.ext4 → 挂载（fail-closed 校验）。
	ref, err := m.DiskRef("", "storages/e2e")
	if err != nil {
		t.Fatalf("生成卷引用失败：%v", err)
	}
	const sizeBytes int64 = 128 << 20
	mp := filepath.Join(dir, "mnt")
	spec := platform.StorageVolumeSpec{
		Ref: ref, SizeBytes: sizeBytes, FileSystem: "ext4",
		MountPoint: mp, Label: "vault-e2e",
	}
	vol, err := m.CreateStorageVolume(ctx, spec)
	if err != nil {
		t.Fatalf("建存储卷失败：%v", err)
	}
	if !vol.Exists || !vol.Mounted {
		t.Fatalf("存储卷状态不对（应已存在且已挂载）：%+v", vol)
	}

	// ⑤ 真读写：证明写入确实落在该卷上，而不是静默落到宿主根文件系统。
	probe := filepath.Join(mp, "probe.txt")
	if err := os.WriteFile(probe, []byte("vault-e2e"), 0o644); err != nil {
		t.Fatalf("往存储卷写文件失败：%v", err)
	}
	if got, err := os.ReadFile(probe); err != nil || string(got) != "vault-e2e" {
		t.Fatalf("读回内容不一致：%q, %v", got, err)
	}

	// ⑥ 幂等：重复调用不得报错，也不得重新格式化把数据冲掉。
	if _, err := m.CreateStorageVolume(ctx, spec); err != nil {
		t.Fatalf("重复建存储卷应幂等成功：%v", err)
	}
	if got, err := os.ReadFile(probe); err != nil || string(got) != "vault-e2e" {
		t.Fatalf("重复调用后数据不应丢失：%q, %v", got, err)
	}

	// ⑦ 状态查询：以实挂为准，容量与已用空间要合理。
	cur, err := m.StorageVolumeStatus(ctx, ref, mp)
	if err != nil {
		t.Fatalf("查存储卷状态失败：%v", err)
	}
	if !cur.Exists || !cur.Mounted {
		t.Fatalf("存储卷状态不对：%+v", cur)
	}
	if !strings.EqualFold(cur.FileSystem, "ext4") {
		t.Fatalf("文件系统应为 ext4：%+v", cur)
	}
	if cur.SizeBytes < sizeBytes {
		t.Fatalf("卷容量应 >= 请求值：%+v", cur)
	}
	if cur.UsedBytes <= 0 {
		t.Fatalf("已写入文件，UsedBytes 应 > 0：%+v", cur)
	}

	// ⑧ 物理占用（thin_ls 元数据快照路径）：软校验——这条路径对宿主工具链更敏感，
	// 取不到只记录，不影响主流程判定。
	if n, err := m.PhysicalSize(ref); err != nil {
		t.Logf("⚠ PhysicalSize 未取到（不影响主流程）：%v", err)
	} else if n <= 0 {
		t.Logf("⚠ PhysicalSize 返回 %d（不影响主流程）", n)
	}

	// ⑨ 本次修复的回归断言：VG 不存在时必须报可执行的 platform.pool_missing，
	// 而不是把 lvcreate 的裸报错甩给用户，也不是笼统的 command_failed。
	missing := New(Options{VG: "vaultnoeg", ThinPool: poolName, Logger: discardLogger()})
	mref, err := missing.DiskRef("", "storages/e2e")
	if err != nil {
		t.Fatalf("生成卷引用失败：%v", err)
	}
	_, err = missing.CreateStorageVolume(ctx, platform.StorageVolumeSpec{
		Ref: mref, SizeBytes: 64 << 20, FileSystem: "ext4",
		MountPoint: filepath.Join(dir, "mnt-missing"),
	})
	if err == nil {
		t.Fatal("VG 不存在时创建存储必须失败，不能软失败后交给 lvcreate")
	}
	if got := apperr.CodeOf(err); got != CodePoolMissing {
		t.Fatalf("VG 不存在时应报 %s，实际 %s：%v", CodePoolMissing, got, err)
	}
	if e, ok := apperr.As(err); !ok {
		t.Fatalf("应为业务错误：%v", err)
	} else if e.Args["vg"] != "vaultnoeg" || e.Args["thin_pool"] != poolName {
		t.Fatalf("错误应带 vg/thin_pool 供前端插值：%v", e.Args)
	}

	// ⑩ 删除存储卷：卸载 + 删 LV，且索引里查不到；重复删除幂等。
	if err := m.DeleteStorageVolume(ctx, ref); err != nil {
		t.Fatalf("删除存储卷失败：%v", err)
	}
	if m.Exists(ref) {
		t.Fatalf("删除后卷仍存在：%s", ref)
	}
	if err := m.DeleteStorageVolume(ctx, ref); err != nil {
		t.Fatalf("重复删除应幂等成功：%v", err)
	}
}

// attachLoop 在 dir 下建一个 sizeBytes 的稀疏镜像并挂到空闲 loop 设备，返回设备路径。
func attachLoop(t *testing.T, dir string, sizeBytes int64) string {
	t.Helper()
	img := filepath.Join(dir, "e2e-disk.img")
	f, err := os.Create(img)
	if err != nil {
		t.Fatalf("建镜像文件失败：%v", err)
	}
	if err := f.Truncate(sizeBytes); err != nil {
		_ = f.Close()
		t.Fatalf("镜像扩容失败：%v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("关闭镜像文件失败：%v", err)
	}
	out, err := exec.Command("losetup", "-f", "--show", img).Output()
	if err != nil {
		t.Fatalf("losetup 失败（宿主可能没有空闲 loop 设备）：%v", err)
	}
	dev := strings.TrimSpace(string(out))
	t.Cleanup(func() {
		runLVMQuiet(t, context.Background(), "losetup", "-d", dev)
	})
	return dev
}

// runLVMQuiet 在清理阶段执行一条外部命令，失败只告警（不影响主断言，也不阻塞后续清理）。
func runLVMQuiet(t *testing.T, ctx context.Context, name string, args ...string) {
	t.Helper()
	if out, err := exec.CommandContext(ctx, name, args...).CombinedOutput(); err != nil {
		t.Logf("清理 `%s %s` 失败（忽略）：%v：%s",
			name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
}
