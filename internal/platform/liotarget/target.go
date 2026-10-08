//go:build linux

package liotarget

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
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

// Available 探测 iSCSI 目标能力是否可用。
//
// 必须同时满足**内核侧**（configfs 已挂载）与**用户态**（targetcli 可执行且能跑通）
// 两个条件；两者缺失时返回的 component 不同，便于直接判断该修哪边。
// configfs 缺失是 targetcli 无法补救的，故探测顺序是内核在前。
//
// 刻意不断言 <root>/iscsi 这类子目录存在：那是 rtslib 按需创建的内部细节，拿它当门禁会把
// 列目标、查会话这类**只读**接口一起锁死（现场表现就是"一大批功能全不可用"）；真缺时由
// rtslib 在对应写操作上报出准确错误。详见 configfsReady 的说明。
func (m *Manager) Available(ctx context.Context) error {
	if err := m.ensureReady(ctx); err != nil {
		return err
	}
	m.logger.Debug("iSCSI 目标后端可用", "root", m.root, "targetcli", m.tcl.bin)
	return nil
}

// EnsureTarget 幂等地把目标收敛到 spec 描述的**完整期望状态**（全量语义）。
//
// 收敛顺序（每一步都幂等，且每次下发后都回读 configfs 复验后置条件）：
//  1. 归一化 IQN（转小写，必要时补前缀）；
//  2. 目标对象（targetcli 建目标时一并建立 tpg1）；缺 tpg1 的半成品残骸直接重建；
//  3. 归一化 initiator 白名单，并据此判定是否"不限制 initiator"（见下）；
//  4. TPG attrib 与 TPG 级 CHAP 凭据（authentication / generate_node_acls /
//     demo_mode_write_protect / tpgt_1/auth；"不要把已有 LUN 自动挂到新 ACL 上"是靠
//     建 ACL 时带 add_mapped_luns=false 实现的，见 cmdCreateACL，它不是 TPG 属性）；
//  5. 建/更新 backstore 及其 attrib（emulate_tpu/tpws、is_nonrot 属于 backstore，
//     不属于 TPG）；
//  6. ACL 收敛：有白名单时建集合内的、删集合外的；白名单为空（不限制）时**一个都不删**，
//     授权交给 LIO 的动态 ACL（generate_node_acls=1）；
//  7. LUN 映射：TPG 级 lun0 与每个**显式** ACL 的映射 lun0（只读在 ACL 层落实；动态 ACL
//     的只读由 demo_mode_write_protect 决定）；
//  8. 最后才 enable（避免"先开服后配盘"造成客户端看到残缺配置）。
//
// ⚠️ 白名单为空**不等于**"谁都不许连"，而是"不限制 initiator"——Windows 后端会把空列表
// 落成通配 `IQN:*`（见 windows-iscsi-vhdx-api.md）。Linux 侧必须用 generate_node_acls=1
// 复现同一语义：否则 LIO 不会为登录中的 initiator 建动态 ACL，
// core_tpg_check_initiator_node_acl() 直接返回 NULL，客户端登录被拒并报
// "Authorization Failure"（现象是门户可达、目标能发现，就是连不上）。
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

	// 2. 目标对象（targetcli 建目标时会一并建立 tpg1）。
	if err := m.ensureTargetObject(ctx, iqn); err != nil {
		return err
	}

	// 3. 期望的 ACL 集合。排序后使用，让命令顺序与日志可复现（map 遍历顺序是随机的）。
	//
	// 必须排在 TPG 属性之前：白名单为空与否直接决定 generate_node_acls 的取值（见
	// EnsureTarget 的说明），属性收敛依赖这个结论。
	want := make(map[string]struct{}, len(spec.Initiators))
	wantList := make([]string, 0, len(spec.Initiators))
	for _, raw := range spec.Initiators {
		ini, ok := initiatorName(raw)
		if !ok {
			// LIO 的 ACL 目录名只能是不带类型前缀的 IQN；其它类型（IP/DNS/MAC）不支持。
			m.logger.Warn("暂不支持的 initiator 类型，已跳过授权", "target", iqn, "initiator", raw)
			continue
		}
		if _, dup := want[ini]; dup {
			continue
		}
		want[ini] = struct{}{}
		wantList = append(wantList, ini)
	}
	sort.Strings(wantList)
	unrestricted := len(wantList) == 0

	m.logger.Info("收敛 iSCSI 目标（LIO）",
		"target", iqn,
		"enabled", spec.Enabled,
		"read_only", spec.ReadOnly,
		"initiators", len(spec.Initiators),
		"unrestricted", unrestricted,
		"chap", chap,
		"chap_secret", chapSecretLog(chap),
		"backing_ref", spec.BackingRef)

	// 4. TPG 属性与 TPG 级 CHAP 凭据。
	if err := m.applyTpgAttribs(ctx, iqn, tpgExpect{
		chap:         chap,
		unrestricted: unrestricted,
		readOnly:     spec.ReadOnly,
		chapUser:     spec.ChapUser,
		chapSecret:   spec.ChapSecret,
	}); err != nil {
		return err
	}

	// 5. backstore 及其 attrib（UNMAP 仿真 / SSD 声明）。名字由目标名稳定派生，
	// 保证重复收敛得到同一个 backstore。
	bs := ""
	if strings.TrimSpace(spec.BackingRef) != "" {
		bs = backstoreNameForTarget(iqn)
		if err := m.ensureBackstore(ctx, bs, spec.BackingRef); err != nil {
			return err
		}
		if err := m.applyBackstoreAttribs(ctx, bs); err != nil {
			return err
		}
	}

	// 6. ACL 收敛：先撤销集合外的，再建集合内的。
	//
	// 这里把失败**当错误返回**（旧实现只记 Warn 就继续）：拆不掉授权意味着某个已被移出
	// 白名单的 initiator 仍然能登录——这是安全语义上的失败，不能静默放过。
	//
	// 白名单为空（不限制）时整段跳过：此时 acls/ 里的条目是 LIO 为正在登录的 initiator
	// 自动建的**动态 ACL**（generate_node_acls=1），删掉等于当场踢掉在线会话、并让客户端
	// 下次登录重新走一遍建 ACL；管理员在这期间手工加的授权同理不该被抹掉。真要收紧授权，
	// 正确做法是给目标配上显式白名单——那时这条删除会把这些条目一并清掉。
	if !unrestricted {
		for _, ini := range m.listDirs(m.aclsPath(iqn)) {
			if _, ok := want[ini]; !ok {
				m.logger.Info("移除不再授权的 initiator", "target", iqn, "initiator", ini)
				if err := m.removeACL(ctx, iqn, ini); err != nil {
					return err
				}
			}
		}
	}
	for _, ini := range wantList {
		if err := m.ensureACL(ctx, iqn, ini); err != nil {
			return err
		}
		if chap {
			if err := m.setACLAuth(ctx, iqn, ini, spec.ChapUser, spec.ChapSecret); err != nil {
				return err
			}
		}
	}

	// 7. LUN 映射（TPG 级 + 每个显式 ACL）。
	// 动态 ACL 不用在这里建映射：内核 core_tpg_check_initiator_node_acl() 会用
	// core_tpg_add_node_to_devs() 把 TPG 里的 LUN 自动挂上去，其只读与否由
	// demo_mode_write_protect 决定（见 applyTpgAttribs）。
	if bs != "" {
		if err := m.ensureTpgMapping(ctx, iqn, bs); err != nil {
			return err
		}
		for _, ini := range wantList {
			if err := m.ensureACLMapping(ctx, iqn, ini, bs, spec.ReadOnly); err != nil {
				return err
			}
		}
	}

	// 8. 最后才 enable（避免"先开服后配盘"造成客户端看到残缺配置）。
	return m.setEnabled(ctx, iqn, spec.Enabled)
}

// RemoveTarget 删除目标及其名下全部授权与映射。幂等（目标不存在视为成功）。
//
// 拆除交给 targetcli 的 delete：它会按内核要求的顺序解除 LUN 映射 → ACL → TPG → 目标。
// 旧实现自己按 rmdir 语义逐层拆，还必须严格避开 os.RemoveAll（内核生成的属性文件无法
// unlink，RemoveAll 会留下半删除残骸并让后续 rmdir 持续失败）——这类"顺序与语义约束"
// 正是交给 rtslib 维护的收益。
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

	// 1. 先停用：正在被访问的目标不应在拆盘过程中继续接受写请求。
	if err := m.setEnabled(ctx, iqn, false); err != nil {
		m.logger.Warn("停用目标失败（继续拆除）", "target", iqn, "error", err)
	}

	// 2. 删除目标对象（连同其 TPG / ACL / LUN 映射）。
	if err := m.apply(ctx, "delete_target", func() bool { return !m.isTarget(iqn) },
		cmdDeleteTarget(iqn)); err != nil {
		return err
	}

	// 3. backstore。
	//
	// 只删除**由本目标派生**的 backstore（即 EnsureTarget 为 spec.BackingRef 创建的那个）。
	// 经 AttachLun 挂上来的 backstore 名字由 ref 派生、可能被多个目标共享，属于
	// "虚拟盘登记"，应由 RemoveVirtualDisk 负责回收，这里不动它。
	if err := m.removeBackstore(ctx, backstoreNameForTarget(iqn)); err != nil {
		return err
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
	if err := m.ensureBackstore(ctx, name, ref); err != nil {
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

	if err := m.removeBackstore(ctx, virtualDiskName(ref)); err != nil {
		return err
	}
	m.logger.Info("已移除 iSCSI 虚拟盘登记（LIO backstore，未删除底层数据）", "ref", ref)
	return nil
}

// AttachLun 建立「虚拟磁盘 ↔ 目标」映射。幂等。
//
// ref 对应的 backstore 若不存在则先登记；随后映射到 TPG 级 lun0 与目标的每个 ACL。
// 与 EnsureTarget 第 6 步共用同一段映射逻辑（ensureTpgMapping / ensureACLMapping）。
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
	if err := m.ensureBackstore(ctx, bs, ref); err != nil {
		return err
	}
	if err := m.ensureTpgMapping(ctx, iqn, bs); err != nil {
		return err
	}
	for _, ini := range m.listDirs(m.aclsPath(iqn)) {
		if err := m.ensureACLMapping(ctx, iqn, ini, bs, readOnly); err != nil {
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
	if err := m.detachTpgMapping(ctx, iqn, bs); err != nil {
		return err
	}
	for _, ini := range m.listDirs(m.aclsPath(iqn)) {
		if err := m.detachACLMapping(ctx, iqn, ini, bs); err != nil {
			return err
		}
	}
	m.logger.Info("已解除 iSCSI LUN 映射（LIO）", "target", iqn, "backstore", bs, "ref", ref)
	return nil
}

// ---- 内部辅助 ----

// ensureTargetObject 确保目标对象存在且形状完整（含 tpg1）。
//
// 若目标目录在、tpg1 不在，说明此前某次收敛中途失败留下了残骸：这种半成品里不可能有
// 有效的 LUN/ACL，直接删掉重建最可靠（targetcli"在已有目标上补建 tpg"的语法涉及 tpg
// 命名细节，不值得为这条异常路径去猜）。
func (m *Manager) ensureTargetObject(ctx context.Context, iqn string) error {
	if m.isTarget(iqn) && m.isTPG(iqn) {
		return nil
	}
	if m.isTarget(iqn) {
		m.logger.Warn("目标目录存在但缺少 tpg1，按残骸处理并重建", "target", iqn)
		if err := m.apply(ctx, "recreate_target",
			func() bool { return !m.isTarget(iqn) }, cmdDeleteTarget(iqn)); err != nil {
			return err
		}
	}
	return m.apply(ctx, "create_target",
		func() bool { return m.isTarget(iqn) && m.isTPG(iqn) }, cmdCreateTarget(iqn))
}

// tpgExpect 是一个 TPG 的期望状态（只覆盖 TPG 级属性，不含 backstore / ACL / LUN）。
type tpgExpect struct {
	// chap 是否要求 CHAP 认证（TPG 属性 authentication）。
	chap bool
	// unrestricted 目标不限制 initiator（白名单为空时的语义，对应 generate_node_acls）。
	unrestricted bool
	// readOnly 只读发布。对显式 ACL 走 acls/<iqn> 的 write_protect（见 ensureACLMapping）；
	// 对动态 ACL 只能走 demo_mode_write_protect，所以这里必须一起收敛，否则"不限制 +
	// 只读"的目标会以可写身份挂出去。
	readOnly bool
	// chapUser / chapSecret TPG 级（demo）CHAP 凭据，供动态 ACL 使用。
	chapUser   string
	chapSecret string
}

// applyTpgAttribs 把 **TPG 自己的**属性逐条收敛到期望值，并在下发后逐条复验；最后按需
// 补写 TPG 级 CHAP 凭据。
//
// 各属性为什么这么设：
//   - authentication：spec 带 CHAP 时置 1，否则置 0；
//   - generate_node_acls：白名单为空（不限制 initiator）时置 1、否则置 0。为 1 时 LIO 会为
//     登录中的 initiator 自动建动态 ACL 并放行，这正是 Windows 后端空列表落成 `IQN:*` 的
//     等价语义；为 0 时严格按 ACL 精确授权。指成常量 0 的旧实现让"不限制"的目标在 Linux
//     上谁都无法登录（详见 EnsureTarget）；
//   - demo_mode_write_protect：只作用于动态 ACL，取目标是否只读。名字里的 demo 是内核
//     历史命名，与"演示模式"无关，它就是"动态 ACL 的 LUN 是否写保护"。
//
// ⚠️ emulate_tpu / emulate_tpws / is_nonrot **不在这里**：它们是 SE_device（backstore）
// 属性，只存在于 core/<插件>/<名字>/attrib/ 下，TPG 的 attribute 组里没有。曾经把它们
// 写在 TPG 上，靠"属性不存在就跳过"的宽容逻辑变成了静默无效——恰恰是最关键的那几个
// 开关（UNMAP 仿真、SSD 声明）从来没生效过。现在挪到 applyBackstoreAttribs。
//
// 在本内核上**不存在**的属性直接跳过（不同内核版本属性集不同）。这是有意保留的宽容：
// 属性名先查 configfs 再下发，比"让 CLI 报错然后忽略报错"更干净，也不会把真实失败
// 混进噪音里。
func (m *Manager) applyTpgAttribs(ctx context.Context, iqn string, exp tpgExpect) error {
	boolStr := func(b bool) string {
		if b {
			return "1"
		}
		return "0"
	}
	spec := []struct{ name, value string }{
		{"authentication", boolStr(exp.chap)},
		{"generate_node_acls", boolStr(exp.unrestricted)},
		{"demo_mode_write_protect", boolStr(exp.readOnly)},
	}
	for _, a := range spec {
		attrPath := path.Join(m.tpgPath(iqn), "attrib", a.name)
		if _, err := os.Stat(attrPath); err != nil {
			m.logger.Debug("TPG 属性在本内核上不存在，跳过", "target", iqn, "attr", a.name)
			continue
		}
		value := a.value
		if err := m.apply(ctx, "tpg_attr_"+a.name,
			func() bool { return m.readAttrEquals(attrPath, value) },
			cmdSetTpgAttr(iqn, a.name, value)); err != nil {
			return err
		}
	}
	// TPG 级 CHAP 凭据：只有"不限制 initiator + CHAP"才需要——动态 ACL 认证取的是
	// tpg_demo_auth，显式 ACL 用的是 acls/<iqn>/auth，与这里无关。白名单非空时不会产生
	// 动态 ACL，写它无用，反而会在缺 auth 组的内核上把本来正常的显式 ACL 目标拖失败。
	if exp.chap && exp.unrestricted {
		if err := m.setTpgAuth(ctx, iqn, exp.chapUser, exp.chapSecret); err != nil {
			return err
		}
	}
	return nil
}

// applyBackstoreAttribs 把 backstore（存储对象）属性逐条收敛到期望值，并在下发后逐条复验。
//
// 各属性为什么这么设：
//   - emulate_tpu=1：**关键**。开启 TPU（Thin Provisioning Unmap）仿真后，客户端才会下发
//     UNMAP；Windows 客户端在未开启时不会发 UNMAP，thin pool 的空间就永远无法回收；
//   - emulate_tpws=1：同理，支持 Write Same(UNMAP) 的瘦盘仿真，配合 emulate_tpu；
//   - is_nonrot=1：声明为**非旋转设备**。Windows 客户端据此把该盘当作 SSD：
//     关闭碎片整理/预读等机械盘优化（避免用 NULL 写把 thin pool 撑满），且默认不再自动
//     挂载为可写卷。
//
// 必须等 backstore 建好之后才能设，所以调用点在 ensureBackstore 之后。
// 属性名先查 configfs 再下发，本内核上不存在的直接跳过（较老内核没有 emulate_tpws /
// is_nonrot），避免把真实失败混进噪音里。
func (m *Manager) applyBackstoreAttribs(ctx context.Context, name string) error {
	spec := []struct{ name, value string }{
		{"emulate_tpu", "1"},
		{"emulate_tpws", "1"},
		{"is_nonrot", "1"},
	}
	for _, a := range spec {
		attrPath := path.Join(m.backstorePath(name), "attrib", a.name)
		if _, err := os.Stat(attrPath); err != nil {
			m.logger.Debug("backstore 属性在本内核上不存在，跳过", "backstore", name, "attr", a.name)
			continue
		}
		value := a.value
		if err := m.applyStorage(ctx, "backstore_attr_"+a.name,
			func() bool { return m.readAttrEquals(attrPath, value) },
			func(alias string) []string {
				return []string{cmdSetBackstoreAttr(alias, name, a.name, value)}
			}); err != nil {
			return err
		}
	}
	return nil
}

// ensureBackstore 幂等地创建（或校验）一个 iblock backstore。
//
// 创建交给 targetcli（/backstores/<插件> create name=.. dev=..）：udev_path 与 enable 的
// 先后顺序（必须先设 udev_path、再 enable，且启用后不得再改 udev_path）由 rtslib 维护。
//
// 已存在时**只校验不修改**，不一致时记 Warn 而不报错：内核在 backstore 启用后拒绝改写
// udev_path，此时唯一办法是删掉重建；而读回的字符串有可能是内核规范化后的设备路径
// （如 /dev/dm-3）而非我们写入的 /dev/mapper/<vg>-<lv>，硬失败会让正常环境也收敛不了。
// 但如果确实挂错了盘，这条 Warn 是唯一线索，因此必须打出来（旧实现只在写失败时记日志）。
//
// 失败路径额外补一份**本包自己查得到的现场**（见 backstoreCreateFailure）：这一阶段的失败报告
// 不能只有 rtslib 的说法，否则现场只能靠猜。
func (m *Manager) ensureBackstore(ctx context.Context, name, ref string) error {
	dir := m.backstorePath(name)
	if fi, err := os.Stat(dir); err == nil {
		if !fi.IsDir() {
			return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
				WithArg("backstore", name).WithCause(os.ErrExist)
		}
		if got, rErr := m.readAttr(path.Join(dir, "udev_path")); rErr == nil && got != "" && got != ref {
			m.logger.Warn("backstore 已存在且指向的设备与期望不同，沿用原值",
				"backstore", name, "expected", ref, "actual", got)
		}
		return nil
	}
	err := m.applyStorage(ctx, "create_backstore",
		func() bool { return m.isBackstore(name) },
		func(alias string) []string { return []string{cmdCreateBackstore(alias, name, ref)} })
	if err != nil {
		return m.backstoreCreateFailure(name, ref, err)
	}
	return nil
}

// backstoreCreateFailure 给"backstore 没建出来"补上本包能自己查到的现场事实。
//
// 为什么非补不可：这一阶段的失败只有两种表达——rtslib 的报错原文（还有可能被截断）与我们的
// "后置条件未满足"。可现场真正要区分的是几种**处置完全不同**的情形：设备节点不在（LV 没激活或
// 已被删）、同名对象落在别的插件组目录下（本包按 iblock_0 复验，永远查不到，于是重试到上限）、
// 以及 configfs 里确实什么都没有。把这几项查出来放进错误参数，日志里就直接看得出该往哪查
// （真实反馈：create_backstore 连试 3 次失败，日志里只有 targetcli 横幅，什么都没说）。
func (m *Manager) backstoreCreateFailure(name, ref string, cause error) error {
	e := apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
		WithArg("stage", "create_backstore").
		WithArg("backstore", name).
		WithArg("path", m.backstorePath(name)).
		WithArg("dev", ref).
		WithArg("dev_state", deviceState(ref)).
		WithCause(cause)
	if other := m.backstorePluginOf(name); other != "" {
		if other == iblockPlugin {
			// 复验之后才出现：下发其实生效了，是复验抢在了它前面（或名字大小写不一致）。
			e = e.WithArg("backstore_dir", "appeared_after_verify")
		} else {
			e = e.WithArg("exists_in_plugin", other).
				WithArg("hint", "同名 backstore 在 "+other+" 组下，本包只按 "+iblockPlugin+" 复验，两者不一致时收敛永远不通过")
		}
	}
	return e
}

// backstorePluginOf 返回持有该名字 backstore 的插件组目录名；没有则返回空串。
//
// 插件组名（iblock_0/fileio_0/…）由内核注册顺序与版本决定，本包只认 iblock_0；名字若出现在别的
// 组下，创建会被 rtslib 判为"已存在"而复验永远查不到——不显式点出来的话，现场只会看到
// "重试到上限"加一段横幅。
func (m *Manager) backstorePluginOf(name string) string {
	core := path.Dir(m.coreRoot())
	for _, plugin := range m.listDirs(core) {
		if m.isDir(path.Join(core, plugin, name)) {
			return plugin
		}
	}
	return ""
}

// deviceState 给出引用在文件系统上的状态（供失败诊断用）。"unknown" 表示引用不是绝对路径，
// 不能据此下结论。
func deviceState(ref string) string {
	if !strings.HasPrefix(ref, "/") {
		return "unknown"
	}
	fi, err := os.Stat(ref)
	switch {
	case err != nil:
		return "missing"
	case fi.Mode()&os.ModeDevice == 0 || fi.Mode()&os.ModeCharDevice != 0:
		return "not_a_block_device"
	default:
		return "ok"
	}
}

// removeBackstore 删除一个 backstore（**不删除底层块设备数据**）。幂等。
//
// 无需自行先写 enable=0：停用与解除引用由 targetcli delete 负责。
func (m *Manager) removeBackstore(ctx context.Context, name string) error {
	if !m.isBackstore(name) {
		return nil // 不存在即成功
	}
	return m.applyStorage(ctx, "delete_backstore",
		func() bool { return !m.isBackstore(name) },
		func(alias string) []string { return []string{cmdDeleteBackstore(alias, name)} })
}

// ensureACL 幂等地创建（授权）一个 ACL。
func (m *Manager) ensureACL(ctx context.Context, iqn, ini string) error {
	// initiator 名会被拼进 targetcli 命令行，先拒绝含空白/斜杠的值，避免参数被拆开。
	if ini == "" || strings.ContainsAny(ini, " \t\n/") {
		return apperr.InvalidParam("initiator")
	}
	aclDir := path.Join(m.aclsPath(iqn), ini)
	return m.apply(ctx, "create_acl",
		func() bool { return m.isDir(aclDir) }, cmdCreateACL(iqn, ini))
}

// setACLAuth 设置单向 CHAP 凭据。
//
// ⚠️ 密钥只经 stdin 下发（见 execRunner.Run），日志里只出现用户名，绝不含密钥。
//
// 这里不做幂等判断、每次都下发：LIO 的 password 属性读回来不可靠（掩码/空），
// 无法据此判断"是否已设对"。每次多执行一条 set 的代价，远低于"以为设了其实没设"
// 导致客户端登录被拒且无从排查的代价。
func (m *Manager) setACLAuth(ctx context.Context, iqn, ini, user, secret string) error {
	if err := m.apply(ctx, "acl_chap", nil, cmdSetACLAuth(iqn, ini, user, secret)); err != nil {
		return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
			WithArg("field", "chap").WithArg("user", user).WithCause(err)
	}
	return nil
}

// setTpgAuth 设置 TPG 级 CHAP 凭据（内核里的 tpg_demo_auth，configfs 路径 tpgt_1/auth/）。
//
// 为什么非写不可：目标不限制 initiator 时 LIO 走**动态 ACL**（generate_node_acls=1），而
// 动态 ACL 认证时取的凭据是 tpg_demo_auth（内核 iscsi_get_node_auth() 的
// dynamic_node_acl 分支），不是任何 acls/<iqn>/auth/。该凭据没写时 naf_flags 里的
// NAF_USERID_SET / NAF_PASSWORD_SET 未置位，chap_server_open() 直接失败，客户端拿到的是
// ISCSI_LOGIN_STATUS_AUTH_FAILED（Windows 侧显示 "Authorization Failure"）——即使 ACL
// 已经被自动放行也照样登不上。所以走动态 ACL 的目标，这一条和 generate_node_acls=1
// 是配套的，缺一个都会失败。
//
// 为什么**直写 configfs**、不走 targetcli（这是与 setACLAuth 的唯一分歧）：
//   - targetcli 的 `set auth` 是 ACL 节点（acls/<iqn>）的命令，tpg1 本体没有这条命令。
//     若照 ACL 的写法下发，批处理模式既不报错、退出码也正常，结果是**静默没写进去**——
//     等于用这次要修的故障方式来"修"故障，必须避免。
//   - 这两个文件的落点由内核固定（tpgt_1/auth/{userid,password} → tpg_demo_auth，见内核
//     lio_target_tpg_auth_attrs），直写还能让 os.WriteFile 的失败如实返回，没有把错误
//     吞进 targetcli 输出的余地。
//
// 两个细节：
//   - **不带尾部换行**：内核 auth 的 store 是直接 snprintf(page) 落库，不像 param 那样做
//     isspace 去尾；带上 '\n' 会让换行成为凭据的一部分。
//   - 只回读 userid 做复验（非机密）；password 不回读，免得多读一份密钥进内存。
//
// 不会牵连发现（SendTargets）阶段：内核 iscsi_get_node_auth() 第一步就按 SessionType 短路
// 到全局 discovery_acl.node_auth，discovery 会话根本走不到 tpg_demo_auth，写这里不会让
// 发现阶段开始要求 CHAP。
func (m *Manager) setTpgAuth(ctx context.Context, iqn, user, secret string) error {
	dir := m.tpgAuthDir(iqn)
	for _, c := range []struct{ name, value string }{{"userid", user}, {"password", secret}} {
		p := path.Join(dir, c.name)
		if _, err := os.Stat(p); err != nil {
			// 「不限制 initiator + CHAP」没有这套凭据就登录不了，属配置不可用，不能静默跳过。
			return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
				WithArg("field", "chap").
				WithArg("hint", "内核未提供 TPG 级 auth 文件 "+p+"，无法为动态 ACL 下发 CHAP 凭据").
				WithCause(err)
		}
		if err := os.WriteFile(p, []byte(c.value), 0); err != nil {
			return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
				WithArg("field", "chap").WithArg("user", user).WithCause(err)
		}
	}
	if err := ctxErr(ctx); err != nil {
		return err
	}
	// 写成功不等于写对了（值被内核截断等），回读确认，不做"发出去就算数"。
	if !m.readAttrEquals(path.Join(dir, "userid"), user) {
		return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
			WithArg("field", "chap").
			WithArg("hint", "TPG 级 CHAP userid 回读与写入不一致").
			WithArg("user", user)
	}
	return nil
}

// removeACL 删除一个 ACL。幂等（不存在视为成功）。
//
// 与旧实现的差别：不再自行"先删 mapped_lun_*、再 rmdir"，该顺序约束由 targetcli delete 承担。
func (m *Manager) removeACL(ctx context.Context, iqn, ini string) error {
	aclDir := path.Join(m.aclsPath(iqn), ini)
	if !m.isDir(aclDir) {
		return nil
	}
	return m.apply(ctx, "delete_acl",
		func() bool { return !m.isDir(aclDir) }, cmdDeleteACL(iqn, ini))
}

// ensureTpgMapping 幂等地建立 TPG 级 lun0 → backstore 的映射。
//
// 只读**不在这里**落实：write_protect 是 per-ACL 层的属性，TPG 级 LUN 没有这个开关。
// 旧实现对 TPG 级映射也写了一次 write_protect，写不进去时只记 Warn——那是一句必然
// 无效的噪音，现已去掉。
func (m *Manager) ensureTpgMapping(ctx context.Context, iqn, bs string) error {
	return m.applyStorage(ctx, "map_lun",
		func() bool { return m.mappingPointsTo(m.tpgLunDir(iqn), bs) },
		func(alias string) []string {
			return []string{cmdCreateLun(iqn, m.backstoreObjectPath(alias, bs))}
		})
}

// ensureACLMapping 幂等地建立 ACL 内 mapped lun0 → backstore 的映射，并落实只读。
//
// 建映射与设只读分成两次下发、各自复验：合成一步的话，在"映射已存在但只读没生效"时
// 无法判断该补哪一步。只读是安全属性，**设不成就是错误**，绝不降级为 Warn。
func (m *Manager) ensureACLMapping(ctx context.Context, iqn, ini, bs string, readOnly bool) error {
	dir := m.aclMappedLunDir(iqn, ini)
	if err := m.applyStorage(ctx, "map_mapped_lun",
		func() bool { return m.mappingPointsTo(dir, bs) },
		func(alias string) []string {
			return []string{cmdCreateMappedLun(iqn, ini, m.backstoreObjectPath(alias, bs))}
		}); err != nil {
		return err
	}
	if !readOnly {
		return nil
	}
	wp := path.Join(dir, "write_protect")
	if _, err := os.Stat(wp); err != nil {
		return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
			WithArg("stage", "write_protect").
			WithArg("path", wp).
			WithCause(errors.New("该内核的映射目录没有 write_protect 属性，只读无法落实"))
	}
	return m.apply(ctx, "write_protect",
		func() bool { return m.readAttrEquals(wp, "1") }, cmdSetWriteProtect(iqn, ini, true))
}

// setEnabled 启用 / 停用 TPG，并复验 enable 属性。
func (m *Manager) setEnabled(ctx context.Context, iqn string, enabled bool) error {
	enablePath := path.Join(m.tpgPath(iqn), "enable")
	want := "0"
	if enabled {
		want = "1"
	}
	if _, err := os.Stat(enablePath); err != nil {
		// enable 属性不存在（目标或 TPG 已被拆除）：此时"停用"无事可做即为满足。
		if !enabled {
			return nil
		}
		return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
			WithArg("stage", "enable").WithArg("path", enablePath).WithCause(err)
	}
	return m.apply(ctx, "enable",
		func() bool { return m.readAttrEquals(enablePath, want) }, cmdSetEnabled(iqn, enabled))
}

// isBackstore 判断 backstore 目录是否存在。
func (m *Manager) isBackstore(name string) bool { return m.isDir(m.backstorePath(name)) }

// isTPG 判断目标的 tpg1 目录是否存在。
func (m *Manager) isTPG(iqn string) bool { return m.isDir(m.tpgPath(iqn)) }

// isDir 判断路径存在且是目录。
func (m *Manager) isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// mappingPointsTo 判断映射目录 dir 是否确实指向 backstore bs。
//
// 这是"是否已映射"的唯一判据，也是幂等收敛的支点：满足即一条命令都不下发。
func (m *Manager) mappingPointsTo(dir, bs string) bool {
	return m.mappingBackstore(dir) == bs
}

// detachTpgMapping 解除 TPG 级 lun0 映射。幂等（未映射或指向别的盘视为无事可做）。
//
// 先核对指向再删：虽然一个目标只挂一个 backstore，但删之前确认"删的确实是这一个"
// 是极廉价的保险，可避免将来支持多 LUN 时误伤。
func (m *Manager) detachTpgMapping(ctx context.Context, iqn, bs string) error {
	if !m.mappingPointsTo(m.tpgLunDir(iqn), bs) {
		return nil
	}
	return m.apply(ctx, "unmap_lun",
		func() bool { return !m.mappingPointsTo(m.tpgLunDir(iqn), bs) }, cmdDeleteLun(iqn))
}

// detachACLMapping 解除 ACL 内 mapped lun0 映射。幂等。
func (m *Manager) detachACLMapping(ctx context.Context, iqn, ini, bs string) error {
	dir := m.aclMappedLunDir(iqn, ini)
	if !m.mappingPointsTo(dir, bs) {
		return nil
	}
	return m.apply(ctx, "unmap_mapped_lun",
		func() bool { return !m.mappingPointsTo(dir, bs) }, cmdDeleteMappedLun(iqn, ini))
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
	//
	// 但"不限制 initiator"的目标（generate_node_acls=1）例外：此时 acls/ 下的条目是 LIO
	// 为正在登录的 initiator 自动建的**动态 ACL**（见 EnsureTarget），它们是"访问的结果"
	// 而不是"配置的白名单"。照原样返回会让上层 targetInfoMatches 把它读成"授权多了"→
	// 判定漂移 → 每次挂载/启动都白白重下发一遍。返回空列表才是这类目标的真实语义。
	info.Initiators = m.listDirs(m.aclsPath(iqn))
	if v, err := m.readAttr(path.Join(m.tpgPath(iqn), "attrib", "generate_node_acls")); err == nil && v == "1" {
		info.Initiators = nil
	}
	// Devices：该目标映射到的 backstore 名字集合。
	// 若调用方需要块设备路径，可进一步读 core/iblock_0/<bs>/udev_path。
	info.Devices = m.mappedBackstores(iqn)
	return info
}

// mappedBackstores 收集目标当前映射的 backstore 名字（TPG 级 lun/ 与各 ACL 的 lun_*）。
func (m *Manager) mappedBackstores(iqn string) []string {
	seen := make(map[string]struct{})
	// 只认 lun 开头的条目：ACL 目录下还有 attrib/auth/param 等同级子树，没必要逐个去探。
	collect := func(container string) {
		entries, err := os.ReadDir(container)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), "lun") {
				continue
			}
			if name := m.mappingBackstore(path.Join(container, e.Name())); name != "" {
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

// mappingMaxDepth 沿符号链接链向下解析的最大层数（正常最多两层：ACL→TPG LUN→backstore）。
const mappingMaxDepth = 8

// mappingBackstore 从一个映射条目出发，沿符号链接链解析出它最终引用的 backstore 名；
// 识别不出时返回 ""。
//
// 必须**多级**解析，因为 LIO 的映射是层层链接，且 ACL 层的链接并不直接指向 backstore：
//
//	ACL 层  .../acls/<ini>/lun_0/<uuid>  ──▶ .../tpgt_1/lun/lun_0
//	TPG 层  .../tpgt_1/lun/lun_0/<uuid>  ──▶ .../core/iblock_0/<bs>   ← 到这里才是 backstore
//
// 也就是说，若在 ACL 层直接取 path.Base(链接目标)，拿到的是 "lun_0" 而**不是** backstore
// 名，mappingPointsTo 会永远为假：于是每次收敛都重复下发 create，被 CLI 以"已存在/已映射"
// 拒绝，最终表现为"怎么配都配不对"。所以这里一路走到 core/ 下的对象为止。
//
// 兼容保留两种旧形态：条目本身就是链接（手工配置/老版本），或条目是含 "backstore"
// 属性文件（内容形如 "iblock_0/<bs>"）的目录。
//
// 注意读不出来的后果：mappingPointsTo 返回 false，于是下一条 targetcli 命令会被发出，
// 由 CLI 去拒绝或修正——只读宽容不会造成静默错误。
func (m *Manager) mappingBackstore(entry string) string {
	cur := path.Clean(entry)
	for depth := 0; depth < mappingMaxDepth; depth++ {
		if m.isBackstoreObject(cur) {
			return path.Base(cur)
		}
		if next := symlinkTarget(cur); next != "" {
			cur = next
			continue
		}
		return backstoreAttrName(cur)
	}
	return ""
}

// isBackstoreObject 判断路径是否就是 core/<插件>/<名字> 这个 backstore 对象本身。
//
// 用"祖父目录是否等于 <root>/core"而非写死 iblock_0，是为了对 rd_mcp/fileio 等其它
// 插件也成立。
func (m *Manager) isBackstoreObject(p string) bool {
	return path.Base(p) != "" && path.Dir(path.Dir(p)) == path.Join(m.root, "core")
}

// symlinkTarget 取条目的链接目标（已规整为绝对路径）：条目本身是链接就取它，条目是目录
// 就取目录内的第一个链接；都不是时返回 ""。
func symlinkTarget(p string) string {
	fi, err := os.Lstat(p)
	if err != nil {
		return ""
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return resolveLink(p)
	}
	if !fi.IsDir() {
		return ""
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.Type()&os.ModeSymlink == 0 {
			continue
		}
		if dst := resolveLink(path.Join(p, e.Name())); dst != "" {
			return dst
		}
	}
	return ""
}

// resolveLink 读链接目标并规整为绝对路径（configfs 里 rtslib 写的是绝对路径，但手工配置
// 可能写相对路径）。
func resolveLink(p string) string {
	dst, err := os.Readlink(p)
	if err != nil || dst == "" {
		return ""
	}
	if !path.IsAbs(dst) {
		dst = path.Join(path.Dir(p), dst)
	}
	return path.Clean(dst)
}

// backstoreAttrName 读旧形态的 backstore 属性文件（内容形如 "iblock_0/<bs>"）。
func backstoreAttrName(dir string) string {
	b, err := os.ReadFile(path.Join(dir, "backstore"))
	if err != nil {
		return ""
	}
	v := strings.TrimSpace(string(b))
	if i := strings.LastIndex(v, "/"); i >= 0 {
		return v[i+1:]
	}
	return v
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
