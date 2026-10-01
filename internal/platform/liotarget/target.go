//go:build linux

package liotarget

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"

	"vault/internal/apperr"
	"vault/internal/platform"
)

// 编译期断言：Manager 必须实现 platform.IscsiBackend。
var _ platform.IscsiBackend = (*Manager)(nil)

// Kind 返回平台种类。
func (m *Manager) Kind() platform.Kind { return platform.KindLinux }

// Available 探测 LIO configfs 是否可用。
//
// <root> 可访问且 <root>/iscsi 可写即视为可用；否则尝试 modprobe 一次后重试，
// 仍失败返回 system.unavailable（component=lio）。
func (m *Manager) Available(ctx context.Context) error {
	if m.configfsReady() {
		return nil
	}
	if err := m.loadModules(ctx); err != nil {
		m.logger.Warn("加载 LIO 内核模块失败", "error", err)
	}
	if m.configfsReady() {
		m.logger.Info("LIO configfs 可用", "root", m.root)
		return nil
	}
	return apperr.New(apperr.CodeUnavailable, http.StatusInternalServerError).
		WithArg("component", "lio")
}

// EnsureTarget 幂等地把目标收敛到 spec 描述的**完整期望状态**（全量语义）。
//
// 收敛顺序（每一步都幂等）：
//  1. 归一化 IQN（转小写，必要时补前缀）；
//  2. mkdir <root>/iscsi/<iqn>；
//  3. mkdir <root>/iscsi/<iqn>/tpgt_1（LIO 会随之自动创建 enable/attrib/acls/lun 等）；
//  4. 写 TPG attrib（必须在建 ACL 前写好 auto_add_mapped_luns=0，否则已存在的 LUN 会被
//     自动挂到每个新 ACL 上，导致 per-ACL 的 write_protect 失效）；
//  5. 建/更新 backstore（若指定 BackingRef）；
//  6. ACL 全量收敛：建集合内的、删集合外的（先删其 mapped_lun_*）；
//  7. LUN 映射：TPG 级 lun/lun_0 与每个 ACL 的 mapped_lun_0；
//  8. 最后才写 enable（避免"先开服后配盘"造成客户端看到残缺配置）。
func (m *Manager) EnsureTarget(ctx context.Context, spec platform.TargetSpec) error {
	if strings.TrimSpace(spec.Name) == "" {
		return apperr.InvalidParam("target_name")
	}
	// CHAP 凭据必须成对出现；密钥长度受 LIO 内核限制。
	chap := spec.ChapUser != "" && spec.ChapSecret != ""
	if (spec.ChapUser == "") != (spec.ChapSecret == "") {
		return apperr.InvalidParam("chap")
	}
	if chap {
		if err := validateChapSecret(spec.ChapSecret); err != nil {
			return err
		}
	}
	if spec.EnableReverseChap {
		// 反向 CHAP 未实现（LIO 需要额外的 mutual 凭据属性与 TPG 开关，属另一组布局）。
		m.logger.Warn("暂不支持反向 CHAP，已忽略相关字段", "target", strings.ToLower(strings.TrimSpace(spec.Name)))
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if err := ctxErr(ctx); err != nil {
		return err
	}
	if err := m.ensureReady(ctx); err != nil {
		return err
	}

	iqn := m.normalizeTargetName(spec.Name)
	m.logger.Info("收敛 iSCSI 目标（LIO）",
		"target", iqn,
		"enabled", spec.Enabled,
		"read_only", spec.ReadOnly,
		"initiators", len(spec.Initiators),
		"chap", chap,
		"chap_secret", chapSecretLog(chap),
		"backing_ref", spec.BackingRef)

	// 2. 目标目录。
	if err := m.mkdirIfMissing(m.targetPath(iqn)); err != nil {
		return err
	}
	// 3. tpgt_1。
	if err := m.mkdirIfMissing(m.tpgPath(iqn)); err != nil {
		return err
	}
	// 4. TPG 属性。
	if err := m.writeTpgAttribs(iqn, chap); err != nil {
		return err
	}

	// 5. backstore。名字由目标名稳定派生，保证重复收敛得到同一个 backstore。
	bs := ""
	if strings.TrimSpace(spec.BackingRef) != "" {
		bs = backstoreNameForTarget(iqn)
		if err := m.ensureBackstore(bs, spec.BackingRef); err != nil {
			return err
		}
	}

	// 6. ACL 全量收敛。
	want := make(map[string]struct{}, len(spec.Initiators))
	for _, raw := range spec.Initiators {
		ini, ok := initiatorName(raw)
		if !ok {
			// LIO 的 ACL 目录名只能是不带类型前缀的 IQN；其它类型（IP/DNS/MAC）不支持。
			m.logger.Warn("暂不支持的 initiator 类型，已跳过授权", "target", iqn, "initiator", raw)
			continue
		}
		want[ini] = struct{}{}
	}
	for _, ini := range m.listDirs(m.aclsPath(iqn)) {
		if _, ok := want[ini]; !ok {
			m.logger.Info("移除不再授权的 initiator", "target", iqn, "initiator", ini)
			m.removeACL(iqn, ini)
		}
	}
	for ini := range want {
		aclDir := path.Join(m.aclsPath(iqn), ini)
		if err := m.mkdirIfMissing(aclDir); err != nil {
			return err
		}
		if chap {
			// userid/password 是 kernel 为每个 ACL 生成的属性文件（单向 CHAP）。
			if err := m.writeAttr(path.Join(aclDir, "userid"), spec.ChapUser); err != nil {
				return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
					WithArg("field", "chap_user").WithCause(err)
			}
			if err := m.writeAttr(path.Join(aclDir, "password"), spec.ChapSecret); err != nil {
				return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
					WithArg("field", "chap_secret").WithCause(err)
			}
		}
	}

	// 7. LUN 映射（TPG 级 + 每个 ACL）。
	if bs != "" {
		if err := m.ensureMapping(m.tpgLunDir(iqn), bs, spec.ReadOnly); err != nil {
			return err
		}
		for ini := range want {
			if err := m.ensureMapping(m.aclMappedLunDir(iqn, ini), bs, spec.ReadOnly); err != nil {
				return err
			}
		}
	}

	// 8. 最后写 enable。
	en := "0"
	if spec.Enabled {
		en = "1"
	}
	if err := m.writeAttr(path.Join(m.tpgPath(iqn), "enable"), en); err != nil {
		return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
			WithArg("stage", "enable").WithCause(err)
	}
	return nil
}

// RemoveTarget 删除目标及其名下全部授权与映射。幂等（目标不存在视为成功）。
//
// configfs 目录删除必须用 rmdir 语义（os.Remove），**绝不能**用 os.RemoveAll——
// RemoveAll 会逐个删除内核生成的属性文件，那些文件无法 unlink，最终留下半删除的
// 残骸并让后续 rmdir 持续失败。
func (m *Manager) RemoveTarget(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return apperr.InvalidParam("target_name")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if err := ctxErr(ctx); err != nil {
		return err
	}
	if err := m.ensureReady(ctx); err != nil {
		return err
	}

	iqn := m.normalizeTargetName(name)
	if !m.isTarget(iqn) {
		return nil // 幂等：不存在即成功
	}

	// 1. 先停用。
	if err := m.writeAttr(path.Join(m.tpgPath(iqn), "enable"), "0"); err != nil {
		m.logger.Debug("停用目标失败（忽略，继续拆除）", "target", iqn, "error", err)
	}

	// 2. ACL：先删其下 mapped_lun_*，再删 ACL 目录。
	for _, ini := range m.listDirs(m.aclsPath(iqn)) {
		m.removeACL(iqn, ini)
	}

	// 3. TPG lun/ 下的映射。
	if entries, err := os.ReadDir(path.Join(m.tpgPath(iqn), "lun")); err == nil {
		for _, e := range entries {
			m.removeMappingDir(path.Join(m.tpgPath(iqn), "lun", e.Name()))
		}
	}

	// 4. backstore。
	//
	// 只删除**由本目标派生**的 backstore（即 EnsureTarget 为 spec.BackingRef 创建的那个）。
	// 通过 AttachLun 挂上来的 backstore 名字由 ref 派生、可能被多个目标共享，
	// 属于"虚拟盘登记"，应由 RemoveVirtualDisk 负责回收，这里不动它。
	m.removeBackstore(backstoreNameForTarget(iqn))

	// 5. tpgt_1（内核会随之清理 enable/attrib/acls/lun 等）。
	if err := os.Remove(m.tpgPath(iqn)); err != nil {
		m.logger.Warn("删除 tpgt_1 失败", "target", iqn, "error", err)
	}
	// 6. iqn 目录。
	if err := os.Remove(m.targetPath(iqn)); err != nil {
		m.logger.Warn("删除 iqn 目录失败", "target", iqn, "error", err)
		return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
			WithArg("stage", "rmdir_iqn").WithCause(err)
	}
	m.logger.Info("已删除 iSCSI 目标（LIO）", "target", iqn)
	return nil
}

// GetTarget 查询单个目标；不存在时返回 iscsi.target_not_found。
func (m *Manager) GetTarget(ctx context.Context, name string) (*platform.TargetInfo, error) {
	if strings.TrimSpace(name) == "" {
		return nil, apperr.InvalidParam("target_name")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	if err := m.ensureReady(ctx); err != nil {
		return nil, err
	}

	iqn := m.normalizeTargetName(name)
	if !m.isTarget(iqn) {
		return nil, apperr.IscsiTargetNotFound()
	}
	info := m.targetInfo(iqn)
	return &info, nil
}

// ListTargets 列出全部目标（直接反推 configfs 目录树）。
func (m *Manager) ListTargets(ctx context.Context) ([]platform.TargetInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	if err := m.ensureReady(ctx); err != nil {
		return nil, err
	}

	iqns := m.listDirs(m.iscsiRoot())
	out := make([]platform.TargetInfo, 0, len(iqns))
	for _, iqn := range iqns {
		out = append(out, m.targetInfo(iqn))
	}
	return out, nil
}

// EnsureVirtualDisk 幂等地把已有虚拟磁盘纳入 LIO 虚拟盘登记。
//
// LIO 侧的"虚拟盘"就是一个 backstore（core/<plugin>/<name>）。名字由 ref 稳定派生
// （vault_ + sha1(ref) 前 16 位十六进制），保证同一 ref 每次得到同名 backstore，
// 从而做到幂等。description 在 LIO 里没有对应属性，忽略。
func (m *Manager) EnsureVirtualDisk(ctx context.Context, ref, description string) error {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return apperr.InvalidParam("ref")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if err := ctxErr(ctx); err != nil {
		return err
	}
	if err := m.ensureReady(ctx); err != nil {
		return err
	}

	name := virtualDiskName(ref)
	if err := m.ensureBackstore(name, ref); err != nil {
		return err
	}
	m.logger.Info("已登记 iSCSI 虚拟盘（LIO backstore）",
		"backstore", name, "ref", ref, "description_ignored", description != "")
	return nil
}

// RemoveVirtualDisk 移除虚拟盘登记（**不删除底层数据**）。幂等。
func (m *Manager) RemoveVirtualDisk(ctx context.Context, ref string) error {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return apperr.InvalidParam("ref")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if err := ctxErr(ctx); err != nil {
		return err
	}
	if err := m.ensureReady(ctx); err != nil {
		return err
	}

	m.removeBackstore(virtualDiskName(ref))
	m.logger.Info("已移除 iSCSI 虚拟盘登记（LIO backstore，未删除底层数据）", "ref", ref)
	return nil
}

// AttachLun 建立「虚拟磁盘 ↔ 目标」映射。幂等。
//
// ref 对应的 backstore 若不存在则先登记；随后映射到 TPG 级 lun/lun_0 与目标的每个 ACL。
// 与 EnsureTarget 第 7 步共用同一段映射逻辑（ensureMapping）。
func (m *Manager) AttachLun(ctx context.Context, targetName, ref string, readOnly bool) error {
	if strings.TrimSpace(targetName) == "" {
		return apperr.InvalidParam("target_name")
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return apperr.InvalidParam("ref")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if err := ctxErr(ctx); err != nil {
		return err
	}
	if err := m.ensureReady(ctx); err != nil {
		return err
	}

	iqn := m.normalizeTargetName(targetName)
	if !m.isTarget(iqn) {
		return apperr.IscsiTargetNotFound()
	}

	bs := virtualDiskName(ref)
	if err := m.ensureBackstore(bs, ref); err != nil {
		return err
	}
	if err := m.ensureMapping(m.tpgLunDir(iqn), bs, readOnly); err != nil {
		return err
	}
	for _, ini := range m.listDirs(m.aclsPath(iqn)) {
		if err := m.ensureMapping(m.aclMappedLunDir(iqn, ini), bs, readOnly); err != nil {
			return err
		}
	}
	m.logger.Info("已建立 iSCSI LUN 映射（LIO）",
		"target", iqn, "backstore", bs, "ref", ref, "read_only", readOnly)
	return nil
}

// DetachLun 解除「虚拟磁盘 ↔ 目标」映射。幂等（未映射视为成功）。
func (m *Manager) DetachLun(ctx context.Context, targetName, ref string) error {
	if strings.TrimSpace(targetName) == "" {
		return apperr.InvalidParam("target_name")
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return apperr.InvalidParam("ref")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if err := ctxErr(ctx); err != nil {
		return err
	}
	if err := m.ensureReady(ctx); err != nil {
		return err
	}

	iqn := m.normalizeTargetName(targetName)
	if !m.isTarget(iqn) {
		return apperr.IscsiTargetNotFound()
	}

	bs := virtualDiskName(ref)
	m.removeMapping(m.tpgLunDir(iqn), bs)
	for _, ini := range m.listDirs(m.aclsPath(iqn)) {
		m.removeMapping(m.aclMappedLunDir(iqn, ini), bs)
	}
	m.logger.Info("已解除 iSCSI LUN 映射（LIO）", "target", iqn, "backstore", bs, "ref", ref)
	return nil
}

// ---- 内部辅助 ----

// writeTpgAttribs 写 TPG 属性目录下的各个属性文件。
//
// 每个属性单独写；属性文件不存在时**忽略并记 Debug**——不同内核版本的属性集并不相同
// （例如较老内核没有 emulate_tpws / is_nonrot）。其它写入错误则视为真失败并返回。
//
// 各属性为什么这么设：
//   - authentication：spec 带 CHAP 时置 1，否则置 0；
//   - generate_node_acls=0：关闭"自动为任意 initiator 放行"，因为我们按 ACL 精确授权；
//   - auto_add_mapped_luns=0：**关键**。若为 1，新建 ACL 会把已存在的 LUN 自动映射进去，
//     我们为只读目标写的 write_protect 会被自动映射覆盖（新映射默认可写）而失效；
//   - emulate_tpu=1：**关键**。开启 TPU（Thin Provisioning Unmap）仿真后，客户端才会下发
//     UNMAP；Windows 客户端在未开启时不会发 UNMAP，thin pool 的空间就永远无法回收；
//   - emulate_tpws=1：同理，支持 Write Same(UNMAP) 的瘦盘仿真，配合 emulate_tpu；
//   - is_nonrot=1：声明为**非旋转设备**。Windows 客户端据此把该盘当作 SSD：
//     关闭碎片整理/预读等机械盘优化（避免用 NULL 写把 thin pool 撑满），且默认不再自动
//     挂载为可写卷。
func (m *Manager) writeTpgAttribs(iqn string, chap bool) error {
	attribDir := path.Join(m.tpgPath(iqn), "attrib")
	auth := "0"
	if chap {
		auth = "1"
	}
	attrs := []struct{ name, value string }{
		{"authentication", auth},
		{"generate_node_acls", "0"},
		{"auto_add_mapped_luns", "0"},
		{"emulate_tpu", "1"},
		{"emulate_tpws", "1"},
		{"is_nonrot", "1"},
	}
	for _, a := range attrs {
		p := path.Join(attribDir, a.name)
		if err := m.writeAttr(p, a.value); err != nil {
			if os.IsNotExist(err) {
				m.logger.Debug("TPG 属性在本内核上不存在，跳过", "target", iqn, "attr", a.name)
				continue
			}
			return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
				WithArg("attr", a.name).WithCause(err)
		}
	}
	return nil
}

// ensureBackstore 幂等地创建（或更新）一个 iblock backstore。
//
// 目录已存在时：udev_path 的改写可能因设备已被启用而失败（内核返回 EBUSY），
// 此时记 Warn 并继续（不致命）；enable=1 同样容忍已启用的情况。
func (m *Manager) ensureBackstore(name, ref string) error {
	dir := m.backstorePath(name)
	existed := false
	if fi, err := os.Stat(dir); err == nil {
		if !fi.IsDir() {
			return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
				WithArg("backstore", name).WithCause(os.ErrExist)
		}
		existed = true
	} else if err != nil && !os.IsNotExist(err) {
		return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).WithCause(err)
	}

	if !existed {
		if err := os.Mkdir(dir, 0o755); err != nil && !os.IsExist(err) {
			return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
				WithArg("backstore", name).WithCause(err)
		}
	}

	// udev_path：真正指向 /dev/mapper/<vg>-<lv> 的块设备。
	if err := m.writeAttr(path.Join(dir, "udev_path"), ref); err != nil {
		if existed {
			m.logger.Warn("更新 backstore udev_path 失败（设备可能已启用），沿用旧值",
				"backstore", name, "ref", ref, "error", err)
		} else {
			return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
				WithArg("field", "udev_path").WithCause(err)
		}
	}

	// enable=1 激活 backstore。
	if err := m.writeAttr(path.Join(dir, "enable"), "1"); err != nil {
		if existed {
			m.logger.Warn("启用 backstore 失败（可能已启用）", "backstore", name, "error", err)
		} else {
			return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
				WithArg("field", "enable").WithCause(err)
		}
	}
	return nil
}

// removeBackstore 停用并删除一个 backstore（**不删除底层块设备数据**）。幂等。
func (m *Manager) removeBackstore(name string) {
	dir := m.backstorePath(name)
	if _, err := os.Stat(dir); err != nil {
		return // 不存在即成功
	}
	// 先停用，再 rmdir。
	if err := m.writeAttr(path.Join(dir, "enable"), "0"); err != nil {
		m.logger.Debug("停用 backstore 失败（忽略，继续删除）", "backstore", name, "error", err)
	}
	if err := os.Remove(dir); err != nil {
		m.logger.Warn("删除 backstore 失败（可能仍被 LUN 引用）", "backstore", name, "error", err)
	}
}

// ensureMapping 在映射目录 dir 下建立「指向 backstore bs」的映射，并（可选）设置只读。
//
// ⚠️ 待真机验证：LUN 映射在 configfs 中的表示随内核/targetcli 版本略有差异，本函数
// 对下列形态都做兼容，以"先成功者为准"：
//
//	A) dir 本身就是一个指向 backstore 的符号链接（targetcli 常见形态）——视为已映射；
//	B) dir 是普通目录，需在其中创建符号链接（符号链接名 = backstore 名）；
//	C) 回退：向 dir/backstore 属性文件写入 "iblock_0/<bs>"（B 的符号链接不被支持时）。
//
// 只读属性 write_protect 写在**映射目录内**；若该文件不可写（部分内核布局把它放在别处），
// 只读不会生效——此情形记 Warn，绝不静默。
func (m *Manager) ensureMapping(dir, bs string, readOnly bool) error {
	fi, err := os.Lstat(dir)
	switch {
	case os.IsNotExist(err):
		if mkErr := os.Mkdir(dir, 0o755); mkErr != nil && !os.IsExist(mkErr) {
			return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
				WithArg("stage", "mkdir_mapping").WithCause(mkErr)
		}
	case err != nil:
		return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).WithCause(err)
	case fi.Mode()&os.ModeSymlink != 0:
		// 形态 A：目录本身就是符号链接，视为已映射。待真机验证。
		m.logger.Debug("映射目录本身即符号链接，视为已映射（待真机验证）", "dir", dir)
		m.applyWriteProtect(dir, readOnly)
		return nil
	}

	// 形态 B：在 dir 内创建指向 backstore 的符号链接。
	link := path.Join(dir, bs)
	if _, lErr := os.Lstat(link); os.IsNotExist(lErr) {
		if sErr := os.Symlink(m.backstorePath(bs), link); sErr != nil {
			// 形态 C：回退为写 backstore 属性文件。
			if wErr := m.writeAttr(path.Join(dir, "backstore"), iblockPlugin+"/"+bs); wErr != nil {
				return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
					WithArg("stage", "map_lun").
					WithCause(fmt.Errorf("symlink 失败: %w; backstore 属性文件也失败: %v", sErr, wErr))
			}
			m.logger.Debug("符号链接方式不可用，已回退为 backstore 属性文件（待真机验证）",
				"dir", dir, "backstore", bs)
		}
	}
	m.applyWriteProtect(dir, readOnly)
	return nil
}

// applyWriteProtect 为只读映射写 write_protect=1。
//
// write_protect 是否位于 mapped_lun_0 目录内、抑或属于 LUN 目标属性，取决于具体内核布局
// （待真机验证）。写不进去时只读无法生效，必须显式 Warn，不能假装成功。
func (m *Manager) applyWriteProtect(dir string, readOnly bool) {
	if !readOnly {
		return
	}
	if err := m.writeAttr(path.Join(dir, "write_protect"), "1"); err != nil {
		m.logger.Warn("写入 write_protect 失败，只读映射可能未生效（待真机验证）",
			"dir", dir, "error", err)
	}
}

// removeMapping 移除 dir 下与指定 backstore bs 相关的映射。幂等。
func (m *Manager) removeMapping(dir, bs string) {
	fi, err := os.Lstat(dir)
	if err != nil {
		return // 不存在视为成功
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		// 形态 A：目标是符号链接，需核对它确实指向该 backstore 再删。
		if dst, rErr := os.Readlink(dir); rErr == nil && path.Base(dst) == bs {
			_ = os.Remove(dir)
		}
		return
	}
	if !fi.IsDir() {
		return
	}
	_ = os.Remove(path.Join(dir, bs)) // 删除形态 B 中我们创建的符号链接
	// 仅当目录已空时才 rmdir 成功；若其中仍有内核属性/其它映射则会失败，忽略即可。
	if err := os.Remove(dir); err != nil {
		m.logger.Debug("保留映射目录（非空或含内核属性，待真机验证）", "dir", dir, "error", err)
	}
}

// removeMappingDir 删除一个映射目录下的**全部**映射（用于 RemoveTarget）。幂等。
func (m *Manager) removeMappingDir(entry string) {
	fi, err := os.Lstat(entry)
	if err != nil {
		return
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		_ = os.Remove(entry)
		return
	}
	// 目录形态：先删内部我们创建的符号链接，再尝试 rmdir。
	if entries, rdErr := os.ReadDir(entry); rdErr == nil {
		for _, e := range entries {
			if e.Type()&os.ModeSymlink != 0 {
				_ = os.Remove(path.Join(entry, e.Name()))
			}
		}
	}
	if err := os.Remove(entry); err != nil {
		m.logger.Warn("删除映射目录失败（可能含内核属性，待真机验证）", "dir", entry, "error", err)
	}
}

// removeACL 删除一个 ACL：先删其下 mapped_lun_*，再 rmdir ACL 目录。幂等。
func (m *Manager) removeACL(iqn, ini string) {
	aclDir := path.Join(m.aclsPath(iqn), ini)
	entries, err := os.ReadDir(aclDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "mapped_lun_") {
			m.removeMappingDir(path.Join(aclDir, e.Name()))
		}
	}
	if err := os.Remove(aclDir); err != nil {
		m.logger.Warn("删除 ACL 目录失败", "target", iqn, "initiator", ini, "error", err)
	}
}

// isTarget 判断目标目录是否存在。
func (m *Manager) isTarget(iqn string) bool {
	fi, err := os.Stat(m.targetPath(iqn))
	return err == nil && fi.IsDir()
}

// targetInfo 从 configfs 反推一个目标的状态快照。
func (m *Manager) targetInfo(iqn string) platform.TargetInfo {
	info := platform.TargetInfo{Name: iqn}
	if v, err := m.readAttr(path.Join(m.tpgPath(iqn), "enable")); err == nil {
		info.Enabled = v == "1"
	}
	// Initiators：ACL 目录名即 initiator IQN，**原样返回**（不添加 "IQN:" 前缀，
	// 那是 Windows 后端的历史约定，Linux 侧保持纯净 IQN）。
	info.Initiators = m.listDirs(m.aclsPath(iqn))
	// Devices：该目标映射到的 backstore 名字集合。
	// 若调用方需要块设备路径，可进一步读 core/iblock_0/<bs>/udev_path。
	info.Devices = m.mappedBackstores(iqn)
	return info
}

// mappedBackstores 收集目标当前映射的 backstore 名字（TPG 级 lun/ 与各 ACL 的 mapped_lun_*）。
func (m *Manager) mappedBackstores(iqn string) []string {
	seen := make(map[string]struct{})
	collect := func(container string) {
		entries, err := os.ReadDir(container)
		if err != nil {
			return
		}
		for _, e := range entries {
			if name := backstoreNameOf(path.Join(container, e.Name())); name != "" {
				seen[name] = struct{}{}
			}
		}
	}
	collect(path.Join(m.tpgPath(iqn), "lun"))
	for _, ini := range m.listDirs(m.aclsPath(iqn)) {
		collect(path.Join(m.aclsPath(iqn), ini))
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// backstoreNameOf 从一个映射条目中提取它引用的 backstore 名；识别不出时返回 ""。
//
// 兼容三种形态（见 ensureMapping）：条目本身是符号链接；条目是含符号链接的目录；
// 条目是含 backstore 属性文件（值为 "iblock_0/<bs>"）的目录。
func backstoreNameOf(entry string) string {
	fi, err := os.Lstat(entry)
	if err != nil {
		return ""
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		dst, rErr := os.Readlink(entry)
		if rErr != nil {
			return ""
		}
		return path.Base(dst)
	}
	if !fi.IsDir() {
		return ""
	}
	entries, rdErr := os.ReadDir(entry)
	if rdErr == nil {
		for _, e := range entries {
			if e.Type()&os.ModeSymlink != 0 {
				if dst, rErr := os.Readlink(path.Join(entry, e.Name())); rErr == nil {
					return path.Base(dst)
				}
			}
		}
	}
	if b, rErr := os.ReadFile(path.Join(entry, "backstore")); rErr == nil {
		v := strings.TrimSpace(string(b))
		if i := strings.LastIndex(v, "/"); i >= 0 {
			return v[i+1:]
		}
		return v
	}
	return ""
}

// backstoreNameForTarget 由 IQN 派生一个稳定的 backstore 短名。
//
// 只保留文件系统安全的字符（小写字母/数字/./-/_），其余（尤其是 IQN 里的 ':'）替换为 '_'，
// 并截断到 60 字符以内。注意：超长 IQN 截断后理论上可能撞名，但实际 IQN 远短于 60 字符，
// 可接受。
func backstoreNameForTarget(iqn string) string {
	var b strings.Builder
	b.Grow(len(iqn))
	for i := 0; i < len(iqn); i++ {
		c := iqn[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '-', c == '_':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	s := b.String()
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

// virtualDiskName 由引用稳定派生 backstore 名：vault_ + sha1(ref) 前 16 位十六进制。
//
// 用哈希（而非直接取路径）是为了：名字定长、文件系统安全、不含 '/'，且同一 ref 恒定得名。
// 这里 sha1 仅用于生成标识，不涉及任何安全用途。
func virtualDiskName(ref string) string {
	sum := sha1.Sum([]byte(ref))
	return "vault_" + hex.EncodeToString(sum[:])[:16]
}

// initiatorName 把上层给出的 initiator 串归一为 LIO 需要的 ACL 目录名（纯 IQN）。
//
// 上层（app）为了兼容 Windows 的 `-InitiatorIds`，传下来的是 `IdType:Value` 形式
// （如 `IQN:iqn.2026-01.com.vault:t1`）；而 LIO 的 ACL 目录名必须是干净的 IQN
// （ACL 目录名会被用作 initiator 名直接比对，带前缀会导致任何客户端都匹配不上）。
//
// 规则：识别出 `IQN` 前缀时取其后全部内容（IQN 自身含 ':'，故只切第一个 ':'）；
// 不带前缀的裸 IQN 原样使用；其它类型返回 false，由调用方跳过并告警。
func initiatorName(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false
	}
	prefix, value, ok := strings.Cut(s, ":")
	if !ok {
		return strings.ToLower(s), true
	}
	if !strings.EqualFold(prefix, "IQN") {
		return "", false
	}
	v := strings.ToLower(strings.TrimSpace(value))
	if v == "" {
		return "", false
	}
	return v, true
}

// validateChapSecret 校验 CHAP 密钥长度。
//
// ⚠️ 这是 LIO（内核 target 层）的**固有限制**：密钥必须为 12–16 字节纯 ASCII。
// 上层若要支持更长的密钥，需要在调用本后端前自行派生/截断。
func validateChapSecret(secret string) error {
	if len(secret) < 12 || len(secret) > 16 {
		return apperr.InvalidParam("chap_secret")
	}
	for i := 0; i < len(secret); i++ {
		if secret[i] < 0x20 || secret[i] > 0x7e {
			return apperr.InvalidParam("chap_secret")
		}
	}
	return nil
}

// chapSecretLog 返回可安全写入日志的 CHAP 密钥占位符（密钥绝不进日志）。
func chapSecretLog(chap bool) string {
	if chap {
		return "***"
	}
	return ""
}
