package app

import (
	"context"
	"strconv"
	"strings"
	"time"
	"unicode"

	"vault/internal/apperr"
	"vault/internal/domain"
	"vault/internal/secret"
	"vault/internal/store"
	"vault/internal/version"
)

// settings 中使用的键名。
const (
	// settingBootstrapClaimedAt 初始化完成标记。**一旦写入即代表系统已初始化**，
	// 依赖 settings.k 的主键约束实现"只允许成功一次"。
	settingBootstrapClaimedAt = "bootstrap.claimed_at"
	// settingFirstStartedAt 服务端首次启动时间，用于初始化时间窗口判定。
	settingFirstStartedAt = "server.first_started_at"
	// settingServerNameOverride 运行时可改的服务端展示名（覆盖配置值）。
	settingServerNameOverride = "server.name"
)

// 密码策略：初始化时创建的超级管理员口令强度要求。
const (
	minPasswordLength = 8
	maxPasswordLength = 128
	minUsernameLength = 3
	maxUsernameLength = 64
)

// BootstrapStatus 描述系统初始化状态，供客户端决定是否进入初始化向导。
type BootstrapStatus struct {
	// Initialized 系统是否已完成初始化（已存在超级管理员，或已存在数据，或已完成初始化流程）。
	Initialized bool `json:"initialized"`
	// NeedsBootstrap 是否需要并允许进入初始化向导。
	NeedsBootstrap bool `json:"needs_bootstrap"`
	// HasSuperAdmin 是否已存在超级管理员账号。
	HasSuperAdmin bool `json:"has_super_admin"`
	// HasData 是否已有业务数据（存储库）。
	HasData bool `json:"has_data"`
	// BootstrapEnabled 配置是否允许通过初始化接口创建超级管理员。
	BootstrapEnabled bool `json:"bootstrap_enabled"`
	// SuperAdminEnabled 内置超级管理员账号是否可用（配置开关）。
	SuperAdminEnabled bool `json:"super_admin_enabled"`
	// BlockedReason 不允许初始化的原因（NeedsBootstrap=false 时给出，便于客户端展示）。
	BlockedReason string `json:"blocked_reason,omitempty"`

	ServerName       string `json:"server_name"`
	ServerInstanceID string `json:"server_instance_id"`
	APIVersion       int    `json:"api_version"`
}

// BootstrapInput 是初始化请求。
type BootstrapInput struct {
	Username string
	Password string
	// ServerName 可选，用于覆盖服务端展示名。
	ServerName string
}

// BootstrapResult 是初始化结果。初始化成功后直接下发会话，客户端无需再次登录。
type BootstrapResult struct {
	Session   *Session     `json:"-"`
	User      *domain.User `json:"-"`
	Token     string       `json:"token"`
	ExpiresAt int64        `json:"expires_at"`
	UserID    string       `json:"user_id"`
	Username  string       `json:"username"`
}

// EnsureFirstStart 记录服务端首次启动时间。
//
// 由启动流程调用一次。初始化时间窗口以该时间为起点，
// 用于降低"部署后长时间未初始化、被局域网内他人抢注"的风险。
func (a *App) EnsureFirstStart(ctx context.Context) error {
	if _, ok, err := a.Store.GetSetting(ctx, settingFirstStartedAt); err != nil {
		return err
	} else if ok {
		return nil
	}
	return a.Store.SetSetting(ctx, settingFirstStartedAt,
		strconv.FormatInt(time.Now().UnixMilli(), 10))
}

// BootstrapStatus 计算当前初始化状态。**匿名可访问但只返回状态、不返回任何敏感信息。**
func (a *App) BootstrapStatus(ctx context.Context) (*BootstrapStatus, error) {
	st := &BootstrapStatus{
		BootstrapEnabled:  a.bootstrapEnabled(),
		SuperAdminEnabled: a.superAdminEnabled(),
		ServerName:        a.effectiveServerName(ctx),
		ServerInstanceID:  a.raw().Server.InstanceID,
		APIVersion:        version.APIVersion,
	}

	admins, err := a.countSuperAdmins(ctx)
	if err != nil {
		return nil, err
	}
	st.HasSuperAdmin = admins > 0

	repos, err := a.Store.CountRepositories(ctx)
	if err != nil {
		return nil, err
	}
	st.HasData = repos > 0

	if _, claimed, err := a.Store.GetSetting(ctx, settingBootstrapClaimedAt); err != nil {
		return nil, err
	} else if claimed {
		st.Initialized = true
	}
	if st.HasSuperAdmin || st.HasData {
		st.Initialized = true
	}

	switch {
	case st.Initialized:
		st.BlockedReason = "already_initialized"
	case !st.BootstrapEnabled:
		st.BlockedReason = "bootstrap_disabled"
	default:
		if reason, err := a.bootstrapWindowReason(ctx); err != nil {
			return nil, err
		} else if reason != "" {
			st.BlockedReason = reason
		}
	}

	st.NeedsBootstrap = !st.Initialized && st.BlockedReason == ""
	return st, nil
}

// Bootstrap 执行一次性初始化：创建首个超级管理员并返回会话。
//
// 安全要点：
//  1. 仅在"未初始化"时可用，且受配置开关与时间窗口约束；
//  2. **并发安全**：借助 settings 主键约束原子抢占标记，两个并发请求只有一个能成功；
//  3. 抢占标记与管理员创建在**同一事务**内，失败整体回滚，不会留下半初始化状态；
//  4. 口令强度在此校验，且以 argon2id 哈希落库，绝不保存明文。
func (a *App) Bootstrap(ctx context.Context, in BootstrapInput, ip string) (*BootstrapResult, error) {
	status, err := a.BootstrapStatus(ctx)
	if err != nil {
		return nil, err
	}
	if status.Initialized {
		return nil, apperr.New("system.already_initialized", 409)
	}
	if !status.BootstrapEnabled {
		return nil, apperr.New("system.bootstrap_disabled", 403)
	}
	if status.BlockedReason != "" {
		return nil, apperr.New("system.bootstrap_blocked", 403).WithArg("reason", status.BlockedReason)
	}

	username := strings.TrimSpace(in.Username)
	if err := validateUsername(username); err != nil {
		return nil, err
	}
	if err := validatePassword(in.Password); err != nil {
		return nil, err
	}

	hash, err := secret.HashPassword(in.Password)
	if err != nil {
		return nil, err
	}

	return a.bootstrapTx(ctx, username, hash, in.ServerName, ip)
}

// bootstrapTx 在单个事务内完成"抢占标记 + 创建管理员 + 记录初始设置"。
func (a *App) bootstrapTx(ctx context.Context, username, passwordHash, serverName, ip string) (*BootstrapResult, error) {
	var created *domain.User

	err := a.Store.Tx(ctx, func(tx *store.Store) error {
		claimed, err := tx.ClaimOnce(ctx, settingBootstrapClaimedAt,
			strconv.FormatInt(time.Now().UnixMilli(), 10))
		if err != nil {
			return err
		}
		if !claimed {
			return apperr.New("system.already_initialized", 409)
		}

		// 双保险：即使标记可用，也再次确认确实没有既有数据，
		// 避免"标记被手工删除"导致对已投产系统再次开放初始化。
		if n, err := tx.CountUsers(ctx); err != nil {
			return err
		} else if n > 0 {
			return apperr.New("system.already_initialized", 409)
		}
		if n, err := tx.CountRepositories(ctx); err != nil {
			return err
		} else if n > 0 {
			return apperr.New("system.already_initialized", 409)
		}

		u := &domain.User{
			Username:     username,
			Role:         domain.RoleSuperAdmin,
			PasswordHash: passwordHash,
			Enabled:      true,
			Remark:       "由初始化向导创建",
		}
		if err := tx.CreateUser(ctx, u); err != nil {
			return err
		}
		created = u
		return nil
	})
	if err != nil {
		return nil, err
	}

	if name := strings.TrimSpace(serverName); name != "" {
		if err := a.Store.SetSetting(ctx, settingServerNameOverride, name); err != nil {
			// 仅影响展示名，不阻塞初始化。
			a.Log.Warn("写入服务端展示名失败", "error", err)
		}
	}

	sess, err := a.Sessions.Create(created, ip)
	if err != nil {
		return nil, err
	}

	a.Log.Warn("系统已完成初始化，首个超级管理员已创建",
		"username", created.Username, "ip", ip)
	_ = a.Store.AppendAudit(ctx, &domain.AuditLog{
		UserID:   created.ID,
		Action:   "system.bootstrap",
		Resource: "system",
		IP:       ip,
		Result:   domain.AuditResultOK,
		Detail:   `{"username":"` + created.Username + `"}`,
	})

	return &BootstrapResult{
		Session:   sess,
		User:      created,
		Token:     sess.Token,
		ExpiresAt: sess.ExpiresAt.UnixMilli(),
		UserID:    created.ID,
		Username:  created.Username,
	}, nil
}

// ---- 内部辅助 ----

func (a *App) bootstrapEnabled() bool {
	v := a.raw().Security.BootstrapEnabled
	return v == nil || *v
}

func (a *App) superAdminEnabled() bool {
	v := a.raw().Security.SuperAdminEnabled
	return v == nil || *v
}

// effectiveServerName 返回最终生效的服务端展示名（运行时覆盖优先于配置）。
func (a *App) effectiveServerName(ctx context.Context) string {
	if v, ok, err := a.Store.GetSetting(ctx, settingServerNameOverride); err == nil && ok && v != "" {
		return v
	}
	return a.raw().Server.Name
}

func (a *App) countSuperAdmins(ctx context.Context) (int, error) {
	users, err := a.Store.ListUsers(ctx, "", 500, 0)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, u := range users {
		if u.Role == domain.RoleSuperAdmin {
			n++
		}
	}
	return n, nil
}

// bootstrapWindowReason 检查初始化时间窗口，返回不允许的原因（空表示允许）。
func (a *App) bootstrapWindowReason(ctx context.Context) (string, error) {
	window := a.raw().Security.BootstrapWindow.Std()
	if window <= 0 {
		return "", nil
	}
	raw, ok, err := a.Store.GetSetting(ctx, settingFirstStartedAt)
	if err != nil {
		return "", err
	}
	if !ok {
		// 尚未记录首次启动时间：视为窗口刚开始，允许初始化。
		return "", nil
	}
	startedAt, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return "", nil
	}
	if time.Since(time.UnixMilli(startedAt)) > window {
		return "bootstrap_window_expired", nil
	}
	return "", nil
}

// validateUsername 校验用户名。
func validateUsername(name string) error {
	n := len([]rune(name))
	if n < minUsernameLength || n > maxUsernameLength {
		return apperr.InvalidParam("username").
			WithArg("min_length", minUsernameLength).
			WithArg("max_length", maxUsernameLength)
	}
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.' {
			continue
		}
		return apperr.InvalidParam("username").WithArg("reason", "illegal_character")
	}
	return nil
}

// validatePassword 校验口令强度。
//
// 初始化创建的是最高权限账号，因此要求"长度 + 字母 + 数字"三要素同时满足。
func validatePassword(pw string) error {
	n := len([]rune(pw))
	if n < minPasswordLength {
		return apperr.InvalidParam("password").WithArg("min_length", minPasswordLength)
	}
	if n > maxPasswordLength {
		return apperr.InvalidParam("password").WithArg("max_length", maxPasswordLength)
	}
	var hasLetter, hasDigit bool
	for _, r := range pw {
		switch {
		case unicode.IsLetter(r):
			hasLetter = true
		case unicode.IsDigit(r):
			hasDigit = true
		}
	}
	if !hasLetter || !hasDigit {
		return apperr.InvalidParam("password").WithArg("reason", "need_letter_and_digit")
	}
	return nil
}
