//go:build linux

package liotarget

import (
	"context"
	"os"
	"path"
	"strings"

	"vault/internal/apperr"
	"vault/internal/platform"
)

// ListSessions 枚举目标（或全部目标）的在线会话。
//
// LIO 把在线会话暴露在 <root>/iscsi/<iqn>/tpgt_1/sessions/<sid>/info（文本键值），
// 直接读取并解析即可。sessions 目录不存在表示当前无会话——返回空切片而**不报错**。
// name 为空表示遍历全部目标。
func (m *Manager) ListSessions(ctx context.Context, name string) ([]platform.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	if err := m.ensureReady(ctx); err != nil {
		return nil, err
	}

	var iqns []string
	if strings.TrimSpace(name) == "" {
		iqns = m.listDirs(m.iscsiRoot())
	} else {
		iqn := m.normalizeTargetName(name)
		if !m.isTarget(iqn) {
			return nil, apperr.IscsiTargetNotFound()
		}
		iqns = []string{iqn}
	}

	var out []platform.Session
	for _, iqn := range iqns {
		out = append(out, m.sessionsOf(iqn)...)
	}
	return out, nil
}

// sessionsOf 读取单个目标下的全部会话。
func (m *Manager) sessionsOf(iqn string) []platform.Session {
	dir := path.Join(m.tpgPath(iqn), "sessions")
	var out []platform.Session
	for _, sid := range m.listDirs(dir) {
		s := platform.Session{ID: sid, TargetName: iqn}
		if b, err := os.ReadFile(path.Join(dir, sid, "info")); err == nil {
			s.InitiatorName, s.State = parseSessionInfo(string(b))
		}
		out = append(out, s)
	}
	return out
}

// ForceLogout 强制登出指定会话——**降级实现**。
//
// ⚠️ 降级说明：LIO 没有服务端"精确到会话的强制登出"能力。`targetcli sessions` 只能
// list/detail，RHEL 官方文档给出的注销步骤全部在**客户端**执行（iscsiadm -u）。
// 因此这里只能做降级动作：停用 TPG（tpgt_1/enable=0）+ 拆除该会话对应 initiator 的 ACL。
// 这**不是精确登出**，已建立的 TCP 连接需要客户端自行执行 iscsiadm -u 断开；
// 停用目标只是阻止后续登录与新建连接。
//
// target 名不存在时返回 iscsi.target_not_found。
func (m *Manager) ForceLogout(ctx context.Context, name, sessionID string) error {
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
		return apperr.IscsiTargetNotFound()
	}

	// 找到该会话对应的 initiator，以便精确拆除其 ACL。
	initiator := ""
	for _, s := range m.sessionsOf(iqn) {
		if s.ID == sessionID {
			initiator = s.InitiatorName
			break
		}
	}
	if initiator == "" {
		m.logger.Warn("未找到指定会话，仍执行降级动作（仅停用目标）",
			"target", iqn, "session", sessionID)
	}

	// 降级动作 1：停用 TPG。
	if err := m.writeAttr(path.Join(m.tpgPath(iqn), "enable"), "0"); err != nil {
		m.logger.Warn("停用目标失败", "target", iqn, "error", err)
	}
	// 降级动作 2：拆除该 initiator 的 ACL（ACL 目录名统一为小写 IQN）。
	if initiator != "" {
		m.removeACL(iqn, strings.ToLower(initiator))
	}

	m.logger.Warn("已执行 LIO 会话强制登出的**降级动作**：LIO 无服务端精确登出能力，"+
		"本操作仅停用目标并拆除授权 ACL，客户端需自行执行 iscsiadm -u 断开连接",
		"target", iqn, "session", sessionID, "initiator", initiator)
	return nil
}

// parseSessionInfo 解析 LIO 会话 info 文本（形如 "Key=Value" 的若干行）。
//
//   - InitiatorName 取 "InitiatorName" 行；
//   - State 取第一个键名包含 "state" 的行（不同内核字段名可能是 State/ConnectionState/
//     SessionState）；取不到则留空。
func parseSessionInfo(text string) (initiator, state string) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch {
		case strings.EqualFold(key, "InitiatorName"):
			if initiator == "" {
				initiator = value
			}
		case strings.Contains(strings.ToLower(key), "state"):
			if state == "" {
				state = value
			}
		}
	}
	return initiator, state
}
