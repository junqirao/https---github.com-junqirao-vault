package app

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"vault/internal/apperr"
	"vault/internal/domain"
	"vault/internal/secret"
)

// defaultIscsiPortalPort 是 iSCSI 目标门户的默认端口；可由 platform.iscsi.portals 覆盖。
const defaultIscsiPortalPort = 3260

// defaultIQNPrefix 是 IQN 前缀：完整 IQN = <prefix>:<target_name>。
//
// 说明：同 5.3，IQN 前缀应由配置提供且全局唯一稳定；本轮先以常量提供。
const defaultIQNPrefix = "iqn.2026-01.com.vault"

// MountSpec 是下发给客户端的挂载参数（客户端据此完成挂载，见 5.5）。
type MountSpec struct {
	ServerInstanceID string `json:"server_instance_id"`
	ServerName       string `json:"server_name"`
	TargetIQN        string `json:"target_iqn"`
	PortalAddress    string `json:"portal_address"`
	PortalPort       int    `json:"portal_port"`
	AuthMode         string `json:"auth_mode"`
	ChapUser         string `json:"chap_user,omitempty"`
	// ChapSecret 仅本次下发，不落盘到客户端配置。
	ChapSecret       string `json:"chap_secret,omitempty"`
	DiskSizeBytes    int64  `json:"disk_size_bytes"`
	LeaseID          string `json:"lease_id"`
	LeaseTTLSeconds  int    `json:"lease_ttl_seconds"`
	HeartbeatSeconds int    `json:"heartbeat_seconds"`
	// MountMode letter | directory。
	MountMode string `json:"mount_mode"`
	// MountPath directory 模式下为 <挂载根>\<服务端别名>\<存储库名>（见 3.4.5）。
	MountPath string `json:"mount_path"`
	// PostScript 挂载后执行的脚本（可执行内容，客户端需按注入变量替换路径）。
	PostScript string `json:"post_script,omitempty"`
}

// LeaseService 负责租约（在线状态的权威来源）与挂载参数下发。
type LeaseService struct {
	Deps
	Repos *RepoService
	Disks *DiskService
	Iscsi *IscsiService
}

// RequestMount 创建/续租租约并返回挂载参数。
//
// 幂等：同 (allocation, client_id) 复用已有租约而非新建。
func (s *LeaseService) RequestMount(ctx context.Context, allocationID, clientID, userID string) (*MountSpec, error) {
	if strings.TrimSpace(allocationID) == "" {
		return nil, apperr.InvalidParam("allocation_id")
	}
	if strings.TrimSpace(clientID) == "" {
		return nil, apperr.InvalidParam("client_id")
	}

	alloc, err := s.Store.GetAllocation(ctx, allocationID)
	if err != nil {
		return nil, err
	}
	if userID == "" {
		userID = alloc.UserID
	}
	repo, err := s.Store.GetRepository(ctx, alloc.RepoID)
	if err != nil {
		return nil, err
	}
	disk, err := s.Store.GetDisk(ctx, alloc.DiskID)
	if err != nil {
		return nil, err
	}

	// 权限：自己的分配直接放行；否则需具备该库的 mount 及以上权限。
	if alloc.UserID != userID {
		u, err := s.Store.GetUserByID(ctx, userID)
		if err != nil {
			return nil, err
		}
		allowed, perm, err := s.Repos.CanAccess(ctx, repo.ID, userID, u.Role)
		if err != nil {
			return nil, err
		}
		if !allowed || !PermAtLeast(perm, domain.PermMount) {
			return nil, apperr.AuthForbidden().WithArg("reason", "mount_not_allowed")
		}
	}

	// 状态机校验：共享库的差异盘必须在 derived 条件下且指纹/版本一致（见 5.4）。
	if repo.IsShared() {
		if err := domain.CanMount(repo.Condition(), disk.ParentVersion, repo.ParentVersion, s.parentFingerprintOK(ctx, repo)); err != nil {
			return nil, err
		}
	}

	// 阶段推进（推送界面）：从这里开始是服务端侧的耗时操作 —— 等差异盘就绪 + 用
	// PowerShell 下发 iSCSI 目标，实测数十秒。期间客户端只有一个 HTTP 请求在等待。
	s.emitMountPhase(allocationID, MountPhaseAllocating)
	target, err := s.Iscsi.Publish(ctx, allocationID)
	if err != nil {
		return nil, err
	}

	d := s.securityDefaults()
	now := time.Now().UnixMilli()
	ttlMillis := d.LeaseTTL.Milliseconds()

	lease := &domain.Lease{
		AllocationID: allocationID,
		TargetName:   target.TargetName,
		UserID:       userID,
		ClientID:     clientID,
		State:        domain.LeaseStateActive,
		ExpiresAt:    now + ttlMillis,
		LastSeenAt:   now,
	}
	if existing, gErr := s.Store.GetLeaseByAllocationClient(ctx, allocationID, clientID); gErr == nil && existing != nil {
		lease.ID = existing.ID
		lease.CreatedAt = existing.CreatedAt
		lease.OwnerToken = existing.OwnerToken
		lease.MountPoint = existing.MountPoint
	}
	if lease.OwnerToken == "" {
		token, err := secret.RandomToken(16)
		if err != nil {
			return nil, err
		}
		lease.OwnerToken = token
	}
	if err := s.Store.UpsertLease(ctx, lease); err != nil {
		return nil, err
	}
	if err := s.Store.UpdateAllocationState(ctx, allocationID, domain.AllocationStateMounting); err != nil {
		return nil, err
	}

	raw := s.raw()
	host, port := s.portal()
	mountMode, mountPath, postScript := clientMountOptions(raw.Server.Name, repo)

	var chapSecret string
	if target.AuthMode == domain.AuthModeCHAP && len(target.ChapSecretEnc) > 0 && s.Cipher != nil {
		if plain, dErr := s.Cipher.DecryptString(target.ChapSecretEnc); dErr == nil {
			chapSecret = plain
		} else {
			s.Log.Error("解密 CHAP 密钥失败", "target", target.TargetName, "error", dErr)
		}
	}

	spec := &MountSpec{
		ServerInstanceID: raw.Server.InstanceID,
		ServerName:       raw.Server.Name,
		// 必须是**平台实际存储**的目标名：推导出来的 IQN 若与平台不一致，
		// 客户端就是"TCP 3260 可达、但登录失败"（见 IscsiService.targetIQN 的说明）。
		TargetIQN:        s.Iscsi.targetIQN(target),
		PortalAddress:    host,
		PortalPort:       port,
		AuthMode:         string(target.AuthMode),
		ChapUser:         target.ChapUser,
		ChapSecret:       chapSecret,
		DiskSizeBytes:    disk.SizeBytes,
		LeaseID:          lease.ID,
		LeaseTTLSeconds:  int(d.LeaseTTL.Seconds()),
		HeartbeatSeconds: int(d.HeartbeatEvery.Seconds()),
		MountMode:        mountMode,
		MountPath:        mountPath,
		PostScript:       postScript,
	}
	s.audit(ctx, userID, "lease.mount", "allocation:"+allocationID, "client="+clientID, domain.AuditResultOK)
	s.emit("lease", map[string]any{
		"action":        "mount",
		"lease_id":      lease.ID,
		"allocation_id": allocationID,
		"user_id":       userID,
		"client_id":     clientID,
		"state":         string(domain.LeaseStateActive),
	})
	return spec, nil
}

// Heartbeat 续期租约。
func (s *LeaseService) Heartbeat(ctx context.Context, leaseID, clientID string) (ttlSeconds int, expiresAt int64, err error) {
	lease, err := s.Store.GetLease(ctx, leaseID)
	if err != nil {
		return 0, 0, err
	}
	// clientID 不匹配说明是另一个客户端在抢占同一租约，直接按"已被吊销"处理。
	if lease.ClientID != clientID {
		return 0, 0, apperr.LeaseRevoked()
	}
	if lease.State.IsTerminal() {
		return 0, 0, apperr.LeaseRevoked()
	}

	d := s.securityDefaults()
	expires := time.Now().Add(d.LeaseTTL).UnixMilli()
	if err := s.Store.TouchLease(ctx, leaseID, expires); err != nil {
		return 0, 0, err
	}
	lease.ExpiresAt = expires
	if lease.State == domain.LeaseStateExpired {
		if err := s.Store.UpdateLeaseState(ctx, leaseID, domain.LeaseStateActive); err != nil {
			return 0, 0, err
		}
	}
	return int(d.LeaseTTL.Seconds()), expires, nil
}

// ReportMounted 记录客户端上报的挂载点。
func (s *LeaseService) ReportMounted(ctx context.Context, leaseID, clientID, mountPoint string) error {
	lease, err := s.Store.GetLease(ctx, leaseID)
	if err != nil {
		return err
	}
	if lease.ClientID != clientID {
		return apperr.LeaseRevoked()
	}
	if err := s.Store.SetLeaseMountPoint(ctx, leaseID, mountPoint); err != nil {
		return err
	}
	if err := s.Store.UpdateAllocationState(ctx, lease.AllocationID, domain.AllocationStateMounted); err != nil {
		return err
	}
	s.audit(ctx, lease.UserID, "lease.mounted", "lease:"+leaseID, "mount_point="+mountPoint, domain.AuditResultOK)
	return nil
}

// Release 客户端主动卸载。
//
// 目标保持发布状态，便于客户端再次挂载（重新走 RequestMount）。
func (s *LeaseService) Release(ctx context.Context, leaseID, clientID string) error {
	lease, err := s.Store.GetLease(ctx, leaseID)
	if err != nil {
		return err
	}
	if lease.ClientID != clientID {
		return apperr.LeaseRevoked()
	}
	if err := s.Store.UpdateLeaseState(ctx, leaseID, domain.LeaseStateReleased); err != nil {
		return err
	}
	if err := s.Store.UpdateAllocationState(ctx, lease.AllocationID, domain.AllocationStateAllocated); err != nil {
		s.Log.Warn("回写分配状态失败", "allocation_id", lease.AllocationID, "error", err)
	}
	s.audit(ctx, lease.UserID, "lease.release", "lease:"+leaseID, "", domain.AuditResultOK)
	s.emit("lease", map[string]any{
		"action":        "release",
		"lease_id":      leaseID,
		"allocation_id": lease.AllocationID,
		"user_id":       lease.UserID,
		"state":         string(domain.LeaseStateReleased),
	})
	return nil
}

// List 列出在线租约（权威来源为 leases 表）。repoID 为空表示全部存储库。
func (s *LeaseService) List(ctx context.Context, repoID string) ([]domain.Lease, error) {
	if repoID != "" {
		return s.Store.ListActiveLeasesByRepo(ctx, repoID)
	}
	repos, err := s.Store.ListRepositories(ctx, "", 500, 0)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Lease, 0)
	for i := range repos {
		leases, err := s.Store.ListActiveLeasesByRepo(ctx, repos[i].ID)
		if err != nil {
			return nil, err
		}
		out = append(out, leases...)
	}
	return out, nil
}

// Revoke 管理员强制下线：停用目标并标记租约 revoked。
func (s *LeaseService) Revoke(ctx context.Context, leaseID, operatorID string) error {
	lease, err := s.Store.GetLease(ctx, leaseID)
	if err != nil {
		return err
	}
	if lease.TargetName != "" {
		if err := s.kickOffline(ctx, lease.TargetName); err != nil {
			s.Log.Warn("踢下线失败，仍标记租约为撤销", "target", lease.TargetName, "error", err)
		}
	}
	if err := s.Store.UpdateLeaseState(ctx, leaseID, domain.LeaseStateRevoked); err != nil {
		return err
	}
	s.audit(ctx, operatorID, "lease.revoke", "lease:"+leaseID, "client="+lease.ClientID, domain.AuditResultOK)
	s.emit("lease", map[string]any{
		"action":        "revoke",
		"lease_id":      leaseID,
		"allocation_id": lease.AllocationID,
		"user_id":       lease.UserID,
		"target_name":   lease.TargetName,
		"state":         string(domain.LeaseStateRevoked),
	})
	return nil
}

// ReapExpired 由定时任务调用：把过期租约标记为 revoked（仅反映"客户端离线"）。
//
// ⚠️ 绝不在这里停用目标：租约/心跳只用于"知道客户端是否还在"，绝不能反过来
// 停用目标、影响挂载（真实诉求："续期、过期这些行为不该影响我的挂载"）。
// 目标的启用/停用只由两个显式动作驱动：挂载（启用）、卸载（停用）。
// 过期只是把租约状态置为 revoked —— 客户端下次心跳会得知"已撤销"并自行断开；
// 目标保持原状，直到显式卸载或管理员 Revoke 才被停用。
//
// 返回处理的租约数量。
func (s *LeaseService) ReapExpired(ctx context.Context) (int, error) {
	leases, err := s.Store.ListExpiredLeases(ctx, time.Now().UnixMilli(), 200)
	if err != nil {
		return 0, err
	}
	reaped := 0
	for i := range leases {
		lease := &leases[i]
		if err := s.Store.UpdateLeaseState(ctx, lease.ID, domain.LeaseStateRevoked); err != nil {
			s.Log.Error("标记过期租约失败", "lease_id", lease.ID, "error", err)
			continue
		}
		s.audit(ctx, "system", "lease.expired", "lease:"+lease.ID,
			"target="+lease.TargetName, domain.AuditResultOK)
		s.emit("lease", map[string]any{
			"action":        "expired",
			"lease_id":      lease.ID,
			"allocation_id": lease.AllocationID,
			"user_id":       lease.UserID,
			"target_name":   lease.TargetName,
			"state":         string(domain.LeaseStateRevoked),
		})
		reaped++
	}
	return reaped, nil
}

// kickOffline 把目标上的在线会话踢下线，并确保目标保持停用。
//
// 平台能力差异（见 platform.IscsiBackend.ForceLogout 注释）：
//   - Linux(LIO)：可真实枚举会话，ForceLogout = 停用目标 + 拆除该会话 initiator 的 ACL
//     （LIO 本身没有服务端精确登出，这是官方文档允许的降级动作）；
//   - Windows：ListSessions 返回 ErrUnsupported，退回既有的"停用目标"降级路径
//     （行为与本轮改动前完全一致）。
//
// 无论走哪条路径，最后都调用 DisableTarget：它同时把 DB 的 enabled 写回 false，
// 保证"撤销后必须保持下线"（见 docs/implementation.md 5.8）。
func (s *LeaseService) kickOffline(ctx context.Context, targetName string) error {
	if s.Iscsi == nil {
		return nil
	}
	sessions, err := s.Iscsi.ListSessions(ctx, targetName)
	if err == nil && len(sessions) > 0 {
		for _, sess := range sessions {
			if fErr := s.Iscsi.ForceLogout(ctx, targetName, sess.ID); fErr != nil {
				s.Log.Warn("强制登出会话失败，退回停用目标",
					"target", targetName, "session", sess.ID, "error", fErr)
				break
			}
			s.Log.Info("已对该会话执行强制登出（平台可能为降级动作）",
				"target", targetName, "session", sess.ID, "initiator", sess.InitiatorName)
		}
	}
	return s.Iscsi.DisableTarget(ctx, targetName)
}

// parentFingerprintOK 校验母盘指纹是否未被绕过修改。
//
// 未记录指纹（历史数据）或平台能力不可用时放行，避免误判导致资源完全不可用。
func (s *LeaseService) parentFingerprintOK(ctx context.Context, repo *domain.Repository) bool {
	if repo.ParentDiskID == nil || s.Disk == nil {
		return true
	}
	parent, err := s.Store.GetDisk(ctx, *repo.ParentDiskID)
	if err != nil {
		return false
	}
	if parent.ContentFingerprint == "" || !s.Disk.Exists(parent.VHDXPath) {
		return true
	}
	actual, err := s.Disk.Fingerprint(parent.VHDXPath)
	if err != nil {
		return false
	}
	return actual == parent.ContentFingerprint
}

// portal 计算下发给客户端的门户地址与端口。
//
// 优先级（见 5.5）：
//  1. **显式配置** platform.iscsi.portals 的第一条 <host>[:port]
//     —— 多网卡/多网段服务器上运维必须能指定"客户端该连哪个地址"；仅靠自动推导
//     会派发到客户端不可达的网卡（真实故障：挂载时报门户连不上）；
//  2. 监听地址 http.listen 的主机部分（非通配时）；
//  3. 本机第一个非回环 IPv4，再退化为主机名。
//
// 端口：配置写了就用配置的，否则 3260。
func (s *LeaseService) portal() (string, int) {
	cfg := s.raw()
	return portalFromConfig(cfg.Platform.Iscsi.Portals, cfg.HTTP.Listen)
}

// portalFromConfig 是 portal 的纯函数实现（不依赖服务实例，便于单测）。
func portalFromConfig(portals []string, listen string) (string, int) {
	port := defaultIscsiPortalPort

	// ① 显式配置优先：取第一条非空条目；通配地址只取其端口，主机继续走自动推导。
	for _, entry := range portals {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		entryHost, entryPort := splitHostPortDefault(entry)
		if entryPort > 0 {
			port = entryPort
		}
		if !isWildcardHost(entryHost) {
			return entryHost, port
		}
		break
	}

	// ② 监听地址的主机部分。
	listen = strings.TrimSpace(listen)
	host := listen
	if h, _, err := net.SplitHostPort(listen); err == nil {
		host = h
	}
	if !isWildcardHost(host) {
		return host, port
	}

	// ③ 自动推导：本机第一个非回环 IPv4，再退化为主机名。
	if ip := firstLocalIP(); ip != "" {
		return ip, port
	}
	if name, err := os.Hostname(); err == nil && strings.TrimSpace(name) != "" {
		return name, port
	}
	return "127.0.0.1", port
}

// isWildcardHost 判断主机部分是否为空或通配地址（无法直接下发给客户端）。
func isWildcardHost(host string) bool {
	switch strings.TrimSpace(host) {
	case "", "0.0.0.0", "::", "[::]", "*":
		return true
	default:
		return false
	}
}

// splitHostPortDefault 解析 "host[:port]"（兼容 [v6]:port 与不带方括号的 IPv6）；
// 端口缺失或非法时返回 0，由调用方决定默认值。
func splitHostPortDefault(value string) (string, int) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", 0
	}
	host, portText, err := net.SplitHostPort(value)
	if err != nil {
		// 没有端口（含不带方括号的 IPv6 字面量）：整串视为主机。
		return strings.Trim(value, "[]"), 0
	}
	if port, convErr := strconv.Atoi(portText); convErr == nil && port > 0 && port <= 65535 {
		return host, port
	}
	return host, 0
}

// firstLocalIP 返回本机第一个非回环 IPv4 地址，找不到时返回空串。
func firstLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() || ipNet.IP.To4() == nil {
			continue
		}
		return ipNet.IP.String()
	}
	return ""
}

// clientMountOptions 解析客户端挂载模式与后置脚本（来自 repo.meta.client_config）。
func clientMountOptions(serverName string, repo *domain.Repository) (mode, path, postScript string) {
	mode = "letter"
	if v, ok := repo.Meta.ClientConfig["mount_mode"].(string); ok && v == "directory" {
		mode = "directory"
		// 目录模式必须带服务端别名命名空间，避免多服务端同名库冲突（见 3.4.5）。
		path = filepath.Join(serverName, repo.Name)
	}
	if v, ok := repo.Meta.ClientConfig["post_script"].(string); ok {
		postScript = v
	}
	return mode, path, postScript
}
