//go:build linux

package linuxlvm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"

	"vault/internal/apperr"
	"vault/internal/platform"
)

// mapperPrefix 是 device-mapper 设备节点目录。
const mapperPrefix = "/dev/mapper/"

// headFingerprintBytes 指纹计算读取的头部字节数（1MiB），与 winvhd 的口径一致。
const headFingerprintBytes = 1 << 20

// maxLVNameLen 是映射出的 LV 名长度上限；LVM 的 dm 名上限 127 字节，留出 vg 前缀与转义余量。
const maxLVNameLen = 100

// lvNameRe 约束本包创建的 VG 与 LV 名。
//
// 之所以禁止 '-'：device-mapper 会把名字里的 '-' 转义成 '--'
// （VG 名 "my-vg" 的节点是 /dev/mapper/my--vg-lv），
// 于是 "/dev/mapper/<vg>-<lv>" 就无法靠"第一个 '-' 切分"反解出 (vg, lv)。
// 把 VG/LV 名限制在 [A-Za-z0-9_+.] 后，ref 才可逆、且 lvRef 的字符串等于真实 dm 节点。
var lvNameRe = regexp.MustCompile(`^[A-Za-z0-9_+.]+$`)

// lvRef 由 VG/LV 名拼出 device-mapper 引用。
func lvRef(vg, lv string) string { return mapperPrefix + vg + "-" + lv }

// parseRef 把引用拆回 (vg, lv)。
//
// VG/LV 名都不含 '-'，所以"第一个 '-'"必然是分隔符，切分是无歧义的。
func parseRef(ref string) (vg, lv string, err error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(ref), mapperPrefix)
	if !ok {
		return "", "", apperr.InvalidParam("ref")
	}
	i := strings.IndexByte(rest, '-')
	if i <= 0 || i == len(rest)-1 {
		return "", "", apperr.InvalidParam("ref")
	}
	vg, lv = rest[:i], rest[i+1:]
	if !lvNameRe.MatchString(vg) || !lvNameRe.MatchString(lv) {
		return "", "", apperr.InvalidParam("ref")
	}
	return vg, lv, nil
}

// DiskRef 依据"本地存储目录 + 布局标识(rel)"生成 LV 引用。
//
// storageRoot 是 storages.path（真实本地目录，暂存/回收站所在），磁盘本身不落在该目录里，
// 但**卷组**要从它推断：一台机器可以有多个存储池（多个 VG），某个存储落在哪个池，
// 它下面的磁盘就必须落在同一个池（否则容量/隔离全乱）。判定链：
//
//	storageRoot → (mountinfo) 承载它的卷 → 形如 /dev/mapper/<vg>-<lv> → vg
//
// 取不到时（目录模式落在宿主根文件系统、未挂载、非 LV 挂载点）回退到 Options.VG。
// 需要"在指定池里生成引用"（例如创建存储卷本身）时用 DiskRefInPool。
func (m *Manager) DiskRef(storageRoot, rel string) (string, error) {
	vg := vgOfStorageRoot(storageRoot)
	if vg == "" {
		vg = strings.TrimSpace(m.vg)
	}
	if !lvNameRe.MatchString(vg) {
		return "", apperr.New(apperr.CodeUnavailable, http.StatusInternalServerError).
			WithArg("reason", "lvm_vg_not_configured")
	}
	lv, err := mapLVName(rel)
	if err != nil {
		return "", err
	}
	return lvRef(vg, lv), nil
}

// DiskRefInPool 在指定存储池（"<vg>/<thin_pool>"）里生成虚拟磁盘引用。
//
// poolRef 为空时等价于 DiskRef("", rel)（后端默认池）。
// 池的 VG 决定 LV 落在哪个卷组；池名本身不进引用（薄 LV 的池由 VG 反查，见 poolcatalog.go）。
func (m *Manager) DiskRefInPool(poolRef, rel string) (string, error) {
	key := strings.TrimSpace(poolRef)
	if key == "" {
		return m.DiskRef("", rel)
	}
	vg, _, err := platform.SplitPoolKey(key)
	if err != nil || !lvNameRe.MatchString(vg) {
		return "", apperr.InvalidParam("pool_ref")
	}
	lv, err := mapLVName(rel)
	if err != nil {
		return "", err
	}
	return lvRef(vg, lv), nil
}

// vgOfStorageRoot 推断承载 storageRoot 的卷所属的卷组；判定不出来返回空串。
func vgOfStorageRoot(storageRoot string) string {
	root := strings.TrimSpace(storageRoot)
	if root == "" {
		return ""
	}
	e, ok := mountOf(root)
	if !ok {
		return ""
	}
	vg, _, err := parseRef(e.source)
	if err != nil {
		return ""
	}
	return vg
}

// mapLVName 把平台中性的相对布局标识映射为 LV 名。
//
// 规则：去掉 .vhdx 后缀；把 '/'、'\' 与 '-' 统一折叠为 '_'；其余字符原样。
// 折叠 '-'（rel 里的 <id> 是含 '-' 的 uuid）是为了满足 lvNameRe 的约束——
// 否则 LVM 会把 '-' 转义成 '--'，使 /dev/mapper/<vg>-<lv> 无法逆解析（详见 lvNameRe 说明）。
// 长度上限 100，超长返回 InvalidParam("rel")。
func mapLVName(rel string) (string, error) {
	r := strings.TrimSpace(rel)
	if r == "" {
		return "", apperr.InvalidParam("rel")
	}
	if strings.HasSuffix(strings.ToLower(r), ".vhdx") {
		r = r[:len(r)-len(".vhdx")]
	}
	var b strings.Builder
	b.Grow(len(r))
	for _, c := range r {
		switch c {
		case '/', '\\', '-':
			b.WriteByte('_')
		default:
			b.WriteRune(c)
		}
	}
	name := b.String()
	if name == "" || len(name) > maxLVNameLen || !lvNameRe.MatchString(name) {
		return "", apperr.InvalidParam("rel")
	}
	return name, nil
}

// Exists 判断引用对应的 LV 是否存在。
//
// 契约没有 ctx，这里用一个短超时的后台上下文（只读探测不应长时间阻塞调用方）。
func (m *Manager) Exists(ref string) bool {
	vg, lv, err := parseRef(ref)
	if err != nil {
		return false
	}
	return m.exists(vg, lv)
}

func (m *Manager) exists(vg, lv string) bool {
	ctx, cancel := m.probeCtx()
	defer cancel()
	// 用 runQuiet：这里的"查不到"就是答案（false），不是故障。建盘的第一步就是探一次
	// "不存在"，用 run 会每次都在日志里留一条 ERROR（见 RunQuiet）。
	out, err := m.runQuiet(ctx, "lvs", "--noheadings", "-o", "lv_name", vg+"/"+lv)
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) != ""
}

// Create 创建一块逻辑容量为 sizeBytes 的 thin LV。
//
// 用 lvcreate --type thin -n <lv> -V <sizeBytes>B -T <vg>/<pool>：
//   - -V 指定**虚拟容量**（thin 卷对外呈现的大小）；
//   - -T 指定承载它的 thin pool（vg/pool 形式）；
//   - 不传 -L：thin LV 不预分配物理空间，-L 会被理解为"数据子卷大小"而非虚拟容量。
func (m *Manager) Create(ctx context.Context, ref string, sizeBytes int64) error {
	if sizeBytes <= 0 {
		return apperr.InvalidParam("size_bytes")
	}
	vg, lv, err := parseRef(ref)
	if err != nil {
		return err
	}
	// 池由 ref 的 VG 反查：多存储池下每个 VG 一个 thin pool（见 poolcatalog.go）。
	pool, err := poolPath(vg, m.resolvePool(ctx, vg))
	if err != nil {
		return err
	}
	// 建盘前先过水位闸门：thin snapshot/LV 无法限制单卷的物理增长，写爆单盘会拖垮整个 pool。
	if err := m.checkWatermark(ctx, vg); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// -V 同样要求 512 的整数倍：界面上两位小数的 GB 换算回字节必然带尾数（见 alignSectorDown）。
	if _, err := m.run(ctx, "lvcreate",
		"--type", "thin",
		"-n", lv,
		"-V", strconv.FormatInt(alignSectorDown(sizeBytes), 10)+"B",
		"-T", pool,
	); err != nil {
		return err
	}
	m.logger.Info("已创建 thin LV", "ref", ref, "size_bytes", sizeBytes)
	return nil
}

// CreateDiff 以 parentRef 为原点创建 thin snapshot 作为差异盘（容量继承父盘）。
func (m *Manager) CreateDiff(ctx context.Context, childRef, parentRef string) error {
	cvg, clv, err := parseRef(childRef)
	if err != nil {
		return err
	}
	pvg, plv, err := parseRef(parentRef)
	if err != nil {
		return err
	}
	if cvg != pvg {
		// thin snapshot 只能与原点同 VG。
		return apperr.InvalidParam("parent_ref")
	}
	if err := m.checkWatermark(ctx, cvg); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// ⚠️ 绝不能带 -L/--size：一旦指定 size，LVM 会把 thin snapshot 退化为旧式 COW 快照
	// （vsize 与 size 分离），并立即按 size 占用物理空间，很快写满整个 pool。
	if _, err := m.run(ctx, "lvcreate", "-s", "-n", clv, pvg+"/"+plv); err != nil {
		return err
	}
	m.logger.Info("已创建 thin snapshot", "child", childRef, "parent", parentRef)
	return nil
}

// Clone 完整复制一块虚拟磁盘到 dstRef（目标已存在时返回错误）。
//
// Linux 上用 thin snapshot 实现：它在创建瞬间即持有与源盘一致的全部内容，
// 且是独立可用的 LV（原点后续被删也不影响它），是 LVM 语意下的"零拷贝全量克隆"；
// 相比 dd 整盘拷贝，避免了按虚拟容量搬运巨量稀疏块。
// 克隆后重置磁盘标识，保证克隆盘能被 Windows 端当作独立盘识别。
func (m *Manager) Clone(ctx context.Context, srcRef, dstRef string) error {
	if m.Exists(dstRef) {
		return apperr.New(apperr.CodeConflict, http.StatusConflict).WithArg("ref", dstRef)
	}
	if err := m.CreateDiff(ctx, dstRef, srcRef); err != nil {
		return err
	}
	if err := m.ResetDiskIdentifier(ctx, dstRef); err != nil {
		if platform.IsUnsupported(err) {
			// 内容已完整复制，仅缺"改标识"这一步：告警而非失败，由上层决定降级。
			m.logger.Warn("克隆完成但无法重置磁盘标识（缺少 ntfslabel）", "dst", dstRef)
			return nil
		}
		return err
	}
	return nil
}

// Activate 在服务端本地激活 LV，使其出现 /dev/mapper 节点。
//
// -K 不可省：thin snapshot 建出来后默认带 skip-activation 标记（lv_attr 第 10 位为 'k'），
// 不带 -K 的 lvchange -ay 会被拒。readOnly 不在此落实（只读由 iSCSI/LIO 的 per-ACL 承担）。
func (m *Manager) Activate(ctx context.Context, ref string, readOnly bool) error {
	vg, lv, err := parseRef(ref)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.run(ctx, "lvchange", "-ay", "-K", vg+"/"+lv); err != nil {
		return err
	}
	m.logger.Info("已激活 LV", "ref", ref, "read_only", readOnly)
	return nil
}

// Deactivate 取消本地激活。幂等（本就未激活时 lvchange 会报错，这里吞掉）。
func (m *Manager) Deactivate(ctx context.Context, ref string) error {
	vg, lv, err := parseRef(ref)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// runQuiet："本就未激活"是本方法的正常出口，不是故障。
	if _, err := m.runQuiet(ctx, "lvchange", "-an", vg+"/"+lv); err != nil {
		m.logger.Debug("停用 LV 未成功（可能本就未激活）", "ref", ref, "err", err.Error())
	}
	return nil
}

// Delete 删除 LV。幂等（不存在视为成功）。
func (m *Manager) Delete(ctx context.Context, ref string) error {
	vg, lv, err := parseRef(ref)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.exists(vg, lv) {
		m.logger.Info("LV 不存在，跳过删除（幂等）", "ref", ref)
		return nil
	}
	// 先停用再删除；停用失败忽略（可能本就未激活，用 runQuiet 免得留 ERROR）。
	// ⚠️ 说明：LV 若被 iSCSI backstore(iblock) 打开，lvremove 会被内核拒绝，
	// 所以调用方必须先在 iSCSI 侧下线对应 LUN 再调用本方法。
	if _, err := m.runQuiet(ctx, "lvchange", "-an", vg+"/"+lv); err != nil {
		m.logger.Debug("删除前停用 LV 未成功（忽略）", "ref", ref, "err", err.Error())
	}
	if _, err := m.run(ctx, "lvremove", "-y", vg+"/"+lv); err != nil {
		return err
	}
	m.logger.Info("已删除 LV", "ref", ref)
	return nil
}

// PhysicalSize 返回该 thin LV 的**独占**物理占用（字节）。
//
// 首选 thin_ls（thin-provisioning-tools）：它直接读池元数据，能区分 mapped / exclusive，
// 不会把快照与原点共享的块重复计入。
// 退化口径：thin_ls 缺失或解析失败时，用 lvs 的 lv_size × data_percent/100 估算。
// 两种口径的差异：退化版对同一池里的多个快照会把共享块**各自计入**（重复计数），
// 数值偏大，只能用于展示，不能用于配额精算。
func (m *Manager) PhysicalSize(ref string) (int64, error) {
	vg, lv, err := parseRef(ref)
	if err != nil {
		return 0, err
	}
	if n, err := m.thinExclusiveBytes(vg, lv); err == nil {
		return n, nil
	} else {
		m.logger.Warn("thin_ls 不可用或解析失败，退化为 lvs data_percent 估算：该口径会把快照共享块重复计入，仅供展示",
			"ref", ref, "err", err.Error())
	}
	return m.estimatePhysicalBytes(vg, lv)
}

// thinExclusiveBytes 用 thin_ls 读取该 thin LV 的独占物理占用。
//
// ⚠️ thin_ls **拒绝在 live metadata 上直接运行**（open 返回 EBUSY：
// "you cannot run this tool with these options on live metadata"），
// 必须先 reserve_metadata_snap、带 -m/--metadata-snap 读、最后 release；
// 而池只要有设备激活，其 tmeta 就必然被 device-mapper 持有，
// 所以这条"快照三步"是唯一可行路径（少了任何一步都会退化成估算口径）。
func (m *Manager) thinExclusiveBytes(vg, lv string) (int64, error) {
	if _, err := LookPath("thin_ls"); err != nil {
		return 0, err
	}
	// 三步共用一份预算：中途超时由下面的 defer 兜住 release。
	ctx, cancel := context.WithTimeout(context.Background(), metadataSnapTimeout)
	defer cancel()

	pool, err := m.poolOf(ctx, vg, lv)
	if err != nil {
		return 0, err
	}
	if pool == "" {
		return 0, fmt.Errorf("not a thin volume")
	}
	devID, err := m.thinDevID(ctx, vg, lv)
	if err != nil {
		return 0, err
	}
	chunk, err := m.chunkSizeOf(ctx, vg, pool)
	if err != nil {
		return 0, err
	}
	// thin_ls 读池的元数据设备 <vg>-<pool>_tmeta；
	// reserve/release 消息要发给池目标设备 <vg>-<pool>-tpool
	// （LVM 的内部 LV [<pool>_tpool]，即 <vg>-<pool> 底下真正跑 thin-pool 的那个 dm 设备）。
	tmeta := mapperPrefix + vg + "-" + pool + "_tmeta"
	if _, err := os.Stat(tmeta); err != nil {
		return 0, err
	}
	poolDev := mapperPrefix + vg + "-" + pool + "-tpool"
	if _, err := Run(ctx, m.logger, "dmsetup", "message", poolDev, "0", "reserve_metadata_snap"); err != nil {
		// 同一时刻只允许 held 一份：别人持有时这里就是 EBUSY，由调用方退回估算口径。
		return 0, fmt.Errorf("reserve_metadata_snap: %w", err)
	}
	// release 必须执行：held 期间池的元数据块无法回收，久持会让池元数据空间吃紧。
	// 用 WithoutCancel，保证上层取消/超时后仍然释放。
	defer func() {
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), probeTimeout)
		defer rcancel()
		if _, err := Run(rctx, m.logger, "dmsetup", "message", poolDev, "0", "release_metadata_snap"); err != nil {
			m.logger.Error("release_metadata_snap 失败：池的元数据快照仍被 held，元数据空间无法回收，需人工释放",
				"pool", poolDev, "err", err.Error())
		}
	}()

	out, err := Run(ctx, m.logger, "thin_ls", "-m", "--no-headers",
		"-o", "DEV,MAPPED_BLOCKS,EXCLUSIVE_BLOCKS", tmeta)
	if err != nil {
		return 0, err
	}
	blocks := int64(-1)
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		// DEV 列是 thin device id（十进制），不是 <major>:<minor>。
		if len(f) < 2 || f[0] != devID {
			continue
		}
		// 独占块优先（更贴合"独占物理占用"）；老版本可能没有第三列。
		idx := 2
		if len(f) < 3 {
			idx = 1
		}
		// 必须用 >=0：全新或全共享的 LV 独占块就是 0，
		// 用 >0 会串到 mapped 列，把共享块算成独占。
		if v, e := strconv.ParseInt(f[idx], 10, 64); e == nil && v >= 0 {
			blocks = v
		}
		break
	}
	if blocks < 0 {
		return 0, fmt.Errorf("thin_ls 未找到 thin id %s 的条目", devID)
	}
	return blocks * chunk, nil
}

// estimatePhysicalBytes 是 thin_ls 不可用时的降级估算：lv_size × data_percent/100。
func (m *Manager) estimatePhysicalBytes(vg, lv string) (int64, error) {
	ctx, cancel := m.probeCtx()
	defer cancel()
	// lv_size 要按字节读：默认报告是 "1.00g"，解析不出来会退化成 0。
	rows, err := m.lvsRows(ctx, "--units", "b", "--nosuffix", "-o", "lv_size,data_percent", vg+"/"+lv)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, apperr.New(CodeVHDFailed, http.StatusInternalServerError).WithArg("ref", lvRef(vg, lv))
	}
	size := rowInt(rows[0], "lv_size")
	pct := rowFloat(rows[0], "data_percent")
	return int64(float64(size) * pct / 100), nil
}

// poolOf 返回某 thin LV 所属的 thin pool 名；非 thin 卷返回空串。
func (m *Manager) poolOf(ctx context.Context, vg, lv string) (string, error) {
	rows, err := m.lvsRows(ctx, "-o", "pool_lv", vg+"/"+lv)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", fmt.Errorf("lvs 无结果")
	}
	return strings.TrimSpace(rowStr(rows[0], "pool_lv")), nil
}

// thinDevID 返回 thin LV 在池内的 thin device id，用于在 thin_ls 输出里定位条目。
//
// thin_ls 的 DEV 列打的就是这个 id（十进制），**不是** <major>:<minor>；
// 取值来源是 dm 的目标行 "0 <len> thin <pool_dev> <dev_id> [<origin_dev>]" 的第 5 列
// （见内核 Documentation/admin-guide/device-mapper/thin-provisioning）。
func (m *Manager) thinDevID(ctx context.Context, vg, lv string) (string, error) {
	out, err := m.run(ctx, "dmsetup", "table", lvRef(vg, lv))
	if err != nil {
		return "", fmt.Errorf("LV 未激活或不是 thin 卷，取不到 thin id: %w", err)
	}
	f := strings.Fields(out)
	if len(f) < 5 || f[2] != "thin" {
		return "", fmt.Errorf("dmsetup table 输出异常: %q", strings.TrimSpace(out))
	}
	id := f[4]
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return "", fmt.Errorf("thin id 非法: %q", id)
	}
	return id, nil
}

// chunkSizeOf 返回 thin pool 的 chunk 大小（字节）。
func (m *Manager) chunkSizeOf(ctx context.Context, vg, pool string) (int64, error) {
	out, err := m.run(ctx, "lvs", "--noheadings", "--units", "b", "--nosuffix", "-o", "chunksize", vg+"/"+pool)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0, err
	}
	if n <= 0 {
		return 0, fmt.Errorf("chunksize 非法")
	}
	return n, nil
}

// lvSizeBytes 返回 LV 的虚拟容量（字节）。
func (m *Manager) lvSizeBytes(vg, lv string) (int64, error) {
	ctx, cancel := m.probeCtx()
	defer cancel()
	rows, err := m.lvsRows(ctx, "--units", "b", "--nosuffix", "-o", "lv_size", vg+"/"+lv)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, fmt.Errorf("lvs 无结果")
	}
	return rowInt(rows[0], "lv_size"), nil
}

// Fingerprint 返回内容指纹，形如 size=<字节>;head=<首1MiB的sha256>。
//
// 首 MiB 读不满不是错误（刚建/稀疏的 LV 可能没写满），按实际读到的字节计算。
func (m *Manager) Fingerprint(ref string) (string, error) {
	vg, lv, err := parseRef(ref)
	if err != nil {
		return "", err
	}
	f, err := os.Open(lvRef(vg, lv))
	if err != nil {
		return "", apperr.New(CodeAccessDenied, http.StatusInternalServerError).
			WithCause(err).WithArg("ref", ref)
	}
	defer f.Close() //nolint:errcheck

	hasher := sha256.New()
	if _, err := io.CopyN(hasher, f, headFingerprintBytes); err != nil && err != io.EOF {
		return "", apperr.New(CodeVHDFailed, http.StatusInternalServerError).WithCause(err)
	}
	size, err := m.lvSizeBytes(vg, lv)
	if err != nil {
		size = 0 // 尺寸读不到仍给出头部指纹
	}
	return fmt.Sprintf("size=%d;head=%s", size, hex.EncodeToString(hasher.Sum(nil))), nil
}

// Optimize 回收未使用空间：仅做文件系统层的 fstrim。
//
// 之所以只做 FS 层：Linux 上 thin 空间的真正回收主要依赖 iSCSI 客户端下发 UNMAP
// （需 TPG 打开 emulate_tpu=1），本方法不触碰块层，**绝不破坏数据**；
// 设备未挂载时无文件系统可 trim，直接返回（不报错）。
func (m *Manager) Optimize(ctx context.Context, ref string) error {
	vg, lv, err := parseRef(ref)
	if err != nil {
		return err
	}
	mp, err := mountPointOf(lvRef(vg, lv))
	if err != nil {
		m.logger.Debug("读取 /proc/mounts 失败，跳过 fstrim", "ref", ref, "err", err.Error())
		return nil
	}
	if mp == "" {
		m.logger.Debug("LV 未挂载，跳过 fstrim（块层回收靠 iSCSI UNMAP）", "ref", ref)
		return nil
	}
	if _, err := m.run(ctx, "fstrim", mp); err != nil {
		return err
	}
	m.logger.Info("已对 LV 执行 fstrim", "ref", ref, "mountpoint", mp)
	return nil
}

// ResetDiskIdentifier 重置磁盘标识（克隆盘去重必需）。
//
// 用 ntfslabel --new-serial <dev> 给卷写入新的 NTFS 序列号，避免克隆盘在 Windows 端
// 因磁盘/卷标识与源盘相同而被判定为同一卷（KB2983588）。
// 缺少 ntfslabel 时返回 ErrUnsupported，由上层决定降级。
// TODO(待真机验证)：需 LV 已激活且未挂载；对非 NTFS 卷该命令会失败。
func (m *Manager) ResetDiskIdentifier(ctx context.Context, ref string) error {
	if _, err := LookPath("ntfslabel"); err != nil {
		return platform.ErrUnsupported
	}
	vg, lv, err := parseRef(ref)
	if err != nil {
		return err
	}
	if _, err := m.run(ctx, "ntfslabel", "--new-serial", lvRef(vg, lv)); err != nil {
		return err
	}
	m.logger.Info("已重置 NTFS 序列号", "ref", ref)
	return nil
}

// checkWatermark 是建盘前的空间闸门。
//
// 原因：thin snapshot/LV 无法限制单个卷的物理增长，单卷写爆会拖垮整个 pool；
// 且 thin pool 的**元数据**打满比数据打满更致命（元数据满后所有写失败）。
// 任一百分比达到阈值即拒绝，并把两个百分比带给前端。
func (m *Manager) checkWatermark(ctx context.Context, vg string) error {
	// 池名按 VG 反查（多存储池）；判定不出来时 resolvePool 回退配置池名。
	poolName := m.resolvePool(ctx, vg)
	pool, err := poolPath(vg, poolName)
	if err != nil {
		return err
	}
	// runQuiet：读不到水位本身就分"池不存在（下面的 ERROR）"和"其它原因（下面的 Warn）"两条路，
	// 每条都自己写日志；再用 run 的话每条都会先多一条通用 ERROR。
	out, err := m.runQuiet(ctx, "lvs", "--reportformat", "json", "-o", "data_percent,metadata_percent", pool)
	if err != nil {
		// 读不到水位有两类原因，必须分开处理：
		//
		//   1) 配置的卷组/池根本不存在（全新机器、还没在管理端初始化存储池）。
		//      这时后续 lvcreate 必然失败，而它的原生输出是 "Volume group \"vg0\" not found"
		//      这种裸文本 —— 用户只看到"创建存储失败"，完全不知道下一步该做什么（真实反馈）。
		//      所以这里直接给出可执行的错误码与参数。
		//   2) 其它原因（权限不足、字段不被支持等）：状态未知，保守跳过闸门，
		//      仍交给 lvcreate 兜底，避免在这里误报"空间不足"。
		if kind := m.poolMissingKind(ctx, vg, poolName); kind != "" {
			m.logger.Error("存储池不存在，拒绝创建（请先初始化存储池）",
				"vg", vg, "thin_pool", poolName, "missing", kind)
			return apperr.New(CodePoolMissing, http.StatusServiceUnavailable).
				WithArg("vg", vg).
				WithArg("thin_pool", poolName)
		}
		m.logger.Warn("读取 thin pool 水位失败，跳过水位闸门", "pool", pool, "err", err.Error())
		return nil
	}
	rows, err := parseReportRows(out, "lv")
	if err != nil || len(rows) == 0 {
		m.logger.Warn("解析 thin pool 水位失败，跳过水位闸门", "pool", pool)
		return nil
	}
	dataPct := rowFloat(rows[0], "data_percent")
	metaPct := rowFloat(rows[0], "metadata_percent")
	if dataPct >= m.watermark || metaPct >= m.watermark {
		return apperr.New(CodeInsufficientSpace, http.StatusInsufficientStorage).
			WithArg("data_percent", dataPct).
			WithArg("metadata_percent", metaPct).
			WithArg("watermark_percent", m.watermark)
	}
	return nil
}

// mountPointOf 在 /proc/mounts 中查找某设备节点的挂载点；未挂载返回空串。
func mountPointOf(dev string) (string, error) {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if f[0] == dev {
			return unescapeMountField(f[1]), nil
		}
	}
	return "", nil
}

// unescapeMountField 还原 /proc/mounts 对空格等字符的八进制转义。
func unescapeMountField(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}
