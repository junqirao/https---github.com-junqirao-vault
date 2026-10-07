//go:build linux

package linuxlvm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vault/internal/apperr"
	"vault/internal/platform"
)

// 本文件锁住"删除存储池"的边界，用假 LVM 工具链喂输入（无需真机）：
//
//  1. 被拒时**一步都不能动**：删池是破坏性操作，任何拒绝都必须发生在 lvremove 之前；
//  2. 池自己不算"卷组里还有内容"（否则"删池 + 删卷组"永远失败），
//     而别的卷/别的池必须挡住；
//  3. 危险对象（默认池、系统卷组）与不存在的池各有自己的错误码，
//     前端据此说人话，而不是让用户对着 "Volume group not found" 猜。

const (
	// 两块盘的卷组清单：vg0 是配置里的默认池所在，vg1 是用户自建的。
	poolDeleteVGs = `{"report":[{"vg":[{"vg_name":"vg0","vg_size":"100000000000","vg_free":"5000000000"},{"vg_name":"vg1","vg_size":"200000000000","vg_free":"190000000000"}]}]}`
	// vg1 的物理卷是一块整盘（未分区）。
	poolDeletePVs = `{"report":[{"pv":[{"pv_name":"/dev/sdb","vg_name":"vg1"},{"pv_name":"/dev/sda","vg_name":"vg0"}]}]}`
	// 干净设备树：没有任何挂载点，也就不会误判成系统卷组。
	poolDeleteCleanTree = `{"blockdevices":[{"name":"sda","path":"/dev/sda","type":"disk","size":1,"rota":false,"mountpoints":[],"children":[]},{"name":"sdb","path":"/dev/sdb","type":"disk","size":1,"rota":true,"mountpoints":[],"children":[]}]}`
	// vg1 里只有一个空池：pool1 及其内部卷（真实 lvs 也会列出池自身的 LV）。
	poolDeleteEmptyPool = `{"report":[{"lv":[{"lv_name":"pool1","lv_attr":"twi-aotz--","pool_lv":""},{"lv_name":"pool1_tdata","lv_attr":"twi-ao----","pool_lv":""},{"lv_name":"pool1_tmeta","lv_attr":"ewi-ao----","pool_lv":""}]}]}`
	// 池里住着两个 thin 卷 = 两个存储的底层卷 / 母盘 / 差异盘。
	poolDeleteUsedPool = `{"report":[{"lv":[{"lv_name":"pool1","lv_attr":"twi-aotz--","pool_lv":""},{"lv_name":"7f3a","lv_attr":"Vwi-a-tz--","pool_lv":"pool1"},{"lv_name":"9c11","lv_attr":"Vwi-a-tz--","pool_lv":"pool1"}]}]}`
	// vg1 里的空池**挂着 dm-cache**：cache pool（pool1_cache）是一个独立 LV，其 lv_attr 首字符是
	// 'C'（不是 't'，所以它不出现在「存储池管理」的列表里），但不带 -a 的 lvs 会把它的名字列出来
	// ——真机上"删池 + 删卷组"就是被它挡下、报 pool_in_use.volume_group。
	poolDeleteEmptyPoolWithCache = `{"report":[{"lv":[{"lv_name":"pool1","lv_attr":"twi-aotz--","pool_lv":""},{"lv_name":"pool1_tdata","lv_attr":"twi-ao----","pool_lv":""},{"lv_name":"pool1_tmeta","lv_attr":"ewi-ao----","pool_lv":""},{"lv_name":"pool1_cache","lv_attr":"Cwi---C---","pool_lv":""}]}]}`
)

// fakePoolEnv 生成一套假的 LVM 工具链并置于 PATH 最前，返回"破坏性命令调用记录"文件路径。
//
// 无副作用的命令（vgs/pvs/lvs/lsblk）返回固定 JSON；破坏性命令只把自己被调用的事实追加进日志，
// 于是测试可以断言"该拒绝的确实一步没动"——只看错误码是不够的，
// 曾有实现先把 lvremove 跑掉再校验，错误码照样正确。
func fakePoolEnv(t *testing.T, pvs, lsblk, lvsThin, lvsPlain string) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "mutations.log")

	writeFake(t, dir, "vgs", `printf '%s\n' '`+poolDeleteVGs+`'`)
	writeFake(t, dir, "pvs", `printf '%s\n' '`+pvs+`'`)
	writeFake(t, dir, "lsblk", `printf '%s\n' '`+lsblk+`'`)
	// lvs 按参数分流：要 pool_lv 列的是"池里有哪些 thin 卷"，否则是"卷组里有哪些卷"。
	writeFake(t, dir, "lvs", `case "$*" in
  *pool_lv*) printf '%s\n' '`+lvsThin+`';;
  *) printf '%s\n' '`+lvsPlain+`';;
esac`)
	for _, name := range []string{"lvremove", "vgremove", "pvremove", "udevadm"} {
		writeFake(t, dir, name, `printf '%s %s\n' "$(basename "$0")" "$*" >> '`+logPath+`'
exit 0`)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// mutations 读出已执行的破坏性命令（每行形如 "lvremove -y vg1/pool1"）。
func mutations(t *testing.T, logPath string) []string {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 一次都没跑过
		}
		t.Fatalf("读调用记录失败: %v", err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// newPoolManager 造一个默认池为 vg0/vault 的 Manager。
func newPoolManager() *Manager {
	return New(Options{VG: "vg0", ThinPool: "vault", Logger: discardLogger()})
}

// assertPoolErr 断言错误码与 reason（reason 为空表示不断言）。
func assertPoolErr(t *testing.T, err error, code, reason string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望错误 %s(%s)，实际成功", code, reason)
	}
	e, ok := apperr.As(err)
	if !ok {
		t.Fatalf("错误 %v 不是 apperr", err)
	}
	if e.Code != code {
		t.Fatalf("错误码 = %s，期望 %s（err=%v）", e.Code, code, err)
	}
	if reason != "" && e.Args["reason"] != reason {
		t.Fatalf("reason = %q，期望 %q（err=%v）", e.Args["reason"], reason, err)
	}
}

// TestDeletePoolRemovesThinPool：空池只删池、不动卷组。
func TestDeletePoolRemovesThinPool(t *testing.T) {
	logPath := fakePoolEnv(t, poolDeletePVs, poolDeleteCleanTree, poolDeleteEmptyPool, poolDeleteEmptyPool)
	m := newPoolManager()

	rep, err := m.DeletePool(context.Background(), "vg1", "pool1", platform.PoolDeleteOptions{})
	if err != nil {
		t.Fatalf("DeletePool 失败: %v", err)
	}
	if rep.VG != "vg1" || rep.ThinPool != "pool1" {
		t.Fatalf("报告 = %+v，期望 vg1/pool1", rep)
	}
	if rep.RemovedVolumeGroup {
		t.Fatal("未要求删卷组，报告不应声称卷组已删")
	}
	if got := mutationNames(rep.Steps); got != "lvremove" {
		t.Fatalf("步骤 = %s，期望仅 lvremove", got)
	}
	calls := mutations(t, logPath)
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "lvremove ") {
		t.Fatalf("实际执行的命令 = %v，期望只有一次 lvremove", calls)
	}
}

// TestDeletePoolRemovesVolumeGroupAndReleasesDevices：连卷组删时把 PV 标签清回"可选"。
//
// 这一条同时锁住"池自己的 LV 不算卷组里的内容"：池空着却报 pool_in_use 会让
// "新建池 → 觉得不合适 → 删掉"这条最平常的路走不通。
func TestDeletePoolRemovesVolumeGroupAndReleasesDevices(t *testing.T) {
	logPath := fakePoolEnv(t, poolDeletePVs, poolDeleteCleanTree, poolDeleteEmptyPool, poolDeleteEmptyPool)
	m := newPoolManager()

	rep, err := m.DeletePool(context.Background(), "vg1", "pool1", platform.PoolDeleteOptions{
		RemoveVolumeGroup: true,
		ReleaseDevices:    true,
	})
	if err != nil {
		t.Fatalf("DeletePool 失败: %v", err)
	}
	if !rep.RemovedVolumeGroup {
		t.Fatal("要求删卷组，报告应如实标注")
	}
	if got := strings.Join(rep.ReleasedDevices, ","); got != "/dev/sdb" {
		t.Fatalf("释放的设备 = %q，期望 /dev/sdb", got)
	}
	if got := mutationNames(rep.Steps); got != "lvremove,vgremove,pvremove,udevadm" {
		t.Fatalf("步骤 = %s，期望 lvremove,vgremove,pvremove,udevadm", got)
	}
	// 顺序即语义：先删池、再删卷组、最后清标签（反过来 pvremove 会对还在卷组里的盘动手）。
	var order []string
	for _, c := range mutations(t, logPath) {
		order = append(order, strings.Fields(c)[0])
	}
	if strings.Join(order, ",") != "lvremove,vgremove,pvremove,udevadm" {
		t.Fatalf("命令顺序 = %v", order)
	}
}

// TestDeletePoolRejectsThinVolumes：池里还有 thin 卷（= 还有存储）时一步都不许动。
func TestDeletePoolRejectsThinVolumes(t *testing.T) {
	logPath := fakePoolEnv(t, poolDeletePVs, poolDeleteCleanTree, poolDeleteUsedPool, poolDeleteUsedPool)
	m := newPoolManager()

	_, err := m.DeletePool(context.Background(), "vg1", "pool1", platform.PoolDeleteOptions{RemoveVolumeGroup: true})
	assertPoolErr(t, err, "platform.pool_in_use", "thin_volumes")

	e, _ := apperr.As(err)
	if e.Args["count"] != 2 {
		t.Fatalf("count = %v，期望 2（前端要显示还剩几个）", e.Args["count"])
	}
	if calls := mutations(t, logPath); len(calls) != 0 {
		t.Fatalf("被拒时不得执行任何命令，实际 = %v", calls)
	}
}

// TestDeletePoolRejectsOtherLVsInVG：卷组里还有别的卷时不能连卷组删，但可以只删池。
func TestDeletePoolRejectsOtherLVsInVG(t *testing.T) {
	const withOther = `{"report":[{"lv":[{"lv_name":"pool1","lv_attr":"twi-aotz--","pool_lv":""},{"lv_name":"scratch","lv_attr":"-wi-a-----","pool_lv":""},{"lv_name":"pool1_tdata","lv_attr":"twi-ao----","pool_lv":""}]}]}`
	logPath := fakePoolEnv(t, poolDeletePVs, poolDeleteCleanTree, poolDeleteEmptyPool, withOther)
	m := newPoolManager()

	_, err := m.DeletePool(context.Background(), "vg1", "pool1", platform.PoolDeleteOptions{RemoveVolumeGroup: true})
	assertPoolErr(t, err, "platform.pool_in_use", "volume_group")
	e, _ := apperr.As(err)
	if !strings.Contains(e.Args["names"].(string), "scratch") {
		t.Fatalf("names = %v，期望点名 scratch", e.Args["names"])
	}
	if calls := mutations(t, logPath); len(calls) != 0 {
		t.Fatalf("被拒时不得执行任何命令，实际 = %v", calls)
	}

	// 只删池则放行：把池清掉、卷组留给 scratch。
	rep, err := m.DeletePool(context.Background(), "vg1", "pool1", platform.PoolDeleteOptions{})
	if err != nil {
		t.Fatalf("只删池应放行: %v", err)
	}
	if rep.RemovedVolumeGroup {
		t.Fatal("未要求删卷组")
	}
	if got := mutationNames(rep.Steps); got != "lvremove" {
		t.Fatalf("步骤 = %s，期望仅 lvremove", got)
	}
}

// TestDeletePoolRejectsDefaultPool：默认池是服务端所有建库/建存储的落脚点，删了服务随即不可用。
func TestDeletePoolRejectsDefaultPool(t *testing.T) {
	logPath := fakePoolEnv(t, poolDeletePVs, poolDeleteCleanTree, poolDeleteEmptyPool, poolDeleteEmptyPool)
	m := newPoolManager()

	// 池名、卷组名两种给法都要挡住（只给卷组名 = 连卷组一起删，同样会带走默认池）。
	for _, tc := range []struct{ vg, pool string; rmVG bool }{
		{"vg0", "vault", false},
		{"vg0", "", true},
	} {
		_, err := m.DeletePool(context.Background(), tc.vg, tc.pool, platform.PoolDeleteOptions{RemoveVolumeGroup: tc.rmVG})
		assertPoolErr(t, err, "platform.pool_protected", "default_pool")
	}
	if calls := mutations(t, logPath); len(calls) != 0 {
		t.Fatalf("被拒时不得执行任何命令，实际 = %v", calls)
	}
}

// TestDeletePoolRejectsSystemVG：物理卷上挂着 / 或 swap 的卷组不是"非系统盘"，一律不碰。
func TestDeletePoolRejectsSystemVG(t *testing.T) {
	const sysTree = `{"blockdevices":[{"name":"sda","path":"/dev/sda","type":"disk","size":1,"rota":false,"mountpoints":[],"children":[]},{"name":"sdb","path":"/dev/sdb","type":"disk","size":1,"rota":true,"mountpoints":[],"children":[{"name":"sdb1","path":"/dev/sdb1","type":"part","fstype":"ext4","mountpoints":["/boot"]}]}]}`
	const sysPVs = `{"report":[{"pv":[{"pv_name":"/dev/sdb1","vg_name":"vg1"}]}]}`
	logPath := fakePoolEnv(t, sysPVs, sysTree, poolDeleteEmptyPool, poolDeleteEmptyPool)
	m := newPoolManager()

	_, err := m.DeletePool(context.Background(), "vg1", "pool1", platform.PoolDeleteOptions{RemoveVolumeGroup: true})
	assertPoolErr(t, err, "platform.pool_protected", "system_vg")
	e, _ := apperr.As(err)
	if !strings.Contains(e.Args["mounts"].(string), "/boot") {
		t.Fatalf("mounts = %v，期望带上 /boot 让用户看懂", e.Args["mounts"])
	}
	if calls := mutations(t, logPath); len(calls) != 0 {
		t.Fatalf("被拒时不得执行任何命令，实际 = %v", calls)
	}
}

// TestDeletePoolNotFound：池/卷组不存在要有专门的错误码，别让用户去猜 LVM 的报错。
func TestDeletePoolNotFound(t *testing.T) {
	logPath := fakePoolEnv(t, poolDeletePVs, poolDeleteCleanTree, poolDeleteEmptyPool, poolDeleteEmptyPool)
	m := newPoolManager()

	_, err := m.DeletePool(context.Background(), "vg9", "pool1", platform.PoolDeleteOptions{})
	assertPoolErr(t, err, "platform.pool_not_found", "")

	// 卷组在、池不在：同样报"不存在"，而不是走到 lvremove 才失败。
	_, err = m.DeletePool(context.Background(), "vg1", "pool9", platform.PoolDeleteOptions{})
	assertPoolErr(t, err, "platform.pool_not_found", "")

	if calls := mutations(t, logPath); len(calls) != 0 {
		t.Fatalf("被拒时不得执行任何命令，实际 = %v", calls)
	}
}

// TestDeletePoolRejectsBadInput：非法参数挡在命令之前。
func TestDeletePoolRejectsBadInput(t *testing.T) {
	logPath := fakePoolEnv(t, poolDeletePVs, poolDeleteCleanTree, poolDeleteEmptyPool, poolDeleteEmptyPool)
	m := newPoolManager()

	for _, tc := range []struct {
		name string
		vg   string
		pool string
		opts platform.PoolDeleteOptions
	}{
		{"空卷组名", "", "pool1", platform.PoolDeleteOptions{}},
		{"卷组名非法字符", "vg 1", "pool1", platform.PoolDeleteOptions{}},
		{"池名非法字符", "vg1", "pool/1", platform.PoolDeleteOptions{}},
		// 只有卷组名、又不许删卷组：没有可执行的动作，静默成功会让用户以为池没了。
		{"无动作可做", "vg1", "", platform.PoolDeleteOptions{}},
	} {
		if _, err := m.DeletePool(context.Background(), tc.vg, tc.pool, tc.opts); err == nil {
			t.Fatalf("%s：必须被拒绝", tc.name)
		}
	}
	if calls := mutations(t, logPath); len(calls) != 0 {
		t.Fatalf("被拒时不得执行任何命令，实际 = %v", calls)
	}
}

// TestDeletePoolSkippedPVRemove：盘上已经没有 PV 标签时算释放成功，不该报失败。
//
// vgremove 有时会自己把标签清掉，此时 pvremove 会以非 0 退出并说 "not a physical volume"。
// 把它当失败会让"删池"看起来永远成不了，用户回头发现盘其实早就干净了。
func TestDeletePoolSkippedPVRemove(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "mutations.log")
	writeFake(t, dir, "vgs", `printf '%s\n' '`+poolDeleteVGs+`'`)
	writeFake(t, dir, "pvs", `printf '%s\n' '`+poolDeletePVs+`'`)
	writeFake(t, dir, "lsblk", `printf '%s\n' '`+poolDeleteCleanTree+`'`)
	writeFake(t, dir, "lvs", `printf '%s\n' '`+poolDeleteEmptyPool+`'`)
	for _, name := range []string{"lvremove", "vgremove", "udevadm"} {
		writeFake(t, dir, name, "exit 0")
	}
	writeFake(t, dir, "pvremove", `printf '%s %s\n' "$(basename "$0")" "$*" >> '`+logPath+`'
echo 'Device "/dev/sdb" is not a physical volume' >&2
exit 5`)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	m := newPoolManager()
	rep, err := m.DeletePool(context.Background(), "vg1", "pool1", platform.PoolDeleteOptions{
		RemoveVolumeGroup: true,
		ReleaseDevices:    true,
	})
	if err != nil {
		t.Fatalf("DeletePool 失败: %v", err)
	}
	if got := strings.Join(rep.ReleasedDevices, ","); got != "/dev/sdb" {
		t.Fatalf("已不是 PV 的盘应算释放成功，released = %q", got)
	}
	if len(mutations(t, logPath)) != 1 {
		t.Fatalf("pvremove 应确实被尝试过")
	}
}

// mutationNames 把步骤序列压成 "a,b,c"，便于断言顺序。
func mutationNames(steps []platform.DeviceReleaseStep) string {
	var out []string
	for _, s := range steps {
		out = append(out, s.Step)
	}
	return strings.Join(out, ",")
}

// TestDeletePoolTreatsCachePoolAsPartOfPool：挂着 dm-cache 的池，"删池 + 删卷组"必须能成功。
//
// 真机反馈：这条路径此前报 pool_in_use.volume_group（文案还说"该卷组上仍有 thin pool"），
// 但挡住它的其实是配对的 cache pool——而「存储池管理」只列 thin pool，页面上根本没有
// cache pool 这一项，用户被彻底卡死、无处着手。
// 它不是"别人的东西"：紧随删除的 vgremove 会把它一并带走，放行不会留下半截状态。
func TestDeletePoolTreatsCachePoolAsPartOfPool(t *testing.T) {
	logPath := fakePoolEnv(t, poolDeletePVs, poolDeleteCleanTree, poolDeleteEmptyPool, poolDeleteEmptyPoolWithCache)
	m := newPoolManager()

	rep, err := m.DeletePool(context.Background(), "vg1", "pool1", platform.PoolDeleteOptions{
		RemoveVolumeGroup: true,
	})
	if err != nil {
		t.Fatalf("带缓存的池应能删池 + 删卷组，实际失败: %v", err)
	}
	if !rep.RemovedVolumeGroup {
		t.Fatal("要求删卷组，报告应如实标注")
	}
	if got := mutationNames(rep.Steps); got != "lvremove,vgremove" {
		t.Fatalf("步骤 = %s，期望 lvremove,vgremove", got)
	}
	if got := mutations(t, logPath); len(got) != 2 {
		t.Fatalf("应确实执行过 lvremove 与 vgremove，记录 = %v", got)
	}
}

// TestGoesWithPool：区分"随池消失"与"用户内容"，是删卷组能否成功的分水岭。
func TestGoesWithPool(t *testing.T) {
	for _, tc := range []struct {
		name, pool string
		want       bool
	}{
		{"pool1", "pool1", true},
		{"pool1_tdata", "pool1", true},
		{"pool1_tmeta", "pool1", true},
		{"pool1_pmspare", "", true},
		{"pool10", "pool1", false},  // 前缀相同但不是同一个池
		{"scratch", "pool1", false}, // 别的卷：必须能报出来
		{"pool1", "", false},        // 只删卷组时，池就是用户内容
		{"pool1_tdata", "", false},
		// dm-cache：本池的缓存池随本池一起消失（否则带缓存的池永远删不掉卷组）。
		{"pool1_cache", "pool1", true},
		{"pool1_cache_cmeta", "pool1", true},
		{"pool1_cache_cdata", "pool1", true},
		{"pool10_cache", "pool1", false}, // 不是本池的缓存
		{"pool1_cache", "", false},       // 只删卷组时，它同样是用户内容
	} {
		if got := goesWithPool(tc.name, tc.pool); got != tc.want {
			t.Fatalf("goesWithPool(%q, %q) = %v，期望 %v", tc.name, tc.pool, got, tc.want)
		}
	}
}

// TestJoinLimit：错误信息里最多点几个名字，剩下的用"等 N 个"收尾。
func TestJoinLimit(t *testing.T) {
	if got := joinLimit(nil); got != "" {
		t.Fatalf("空列表 = %q", got)
	}
	if got := joinLimit([]string{"a", "b"}); got != "a, b" {
		t.Fatalf("两个名字 = %q", got)
	}
	if got := joinLimit([]string{"a", "b", "c", "d", "e"}); got != "a, b, c 等 5 个" {
		t.Fatalf("五个名字 = %q", got)
	}
}
