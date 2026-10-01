package app

import (
	"context"
	"time"

	"vault/internal/apperr"
	"vault/internal/domain"
)

// isNotFound 判断错误是否为"实体不存在"类业务错误。
//
// 通过 HTTP 状态码判定，避免在 app 层到处 import store 的内部错误。
func isNotFound(err error) bool {
	e, ok := apperr.As(err)
	return ok && e.HTTP == 404
}

// ---- context 中的调用者身份 ----

type principalCtxKey struct{}

// Principal 是经过认证的调用者。
type Principal struct {
	UserID   string
	Username string
	Role     domain.Role
	// AuthMethod 认证方式：password | cert。
	AuthMethod string
	// CertFingerprint 证书指纹（AuthMethod=cert 时有值）。
	CertFingerprint string
}

// IsSuperAdmin 判断是否为超级管理员。
func (p *Principal) IsSuperAdmin() bool { return p != nil && p.Role == domain.RoleSuperAdmin }

// WithPrincipal 把调用者注入 context。
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, p)
}

// PrincipalFrom 从 context 取出调用者。
func PrincipalFrom(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalCtxKey{}).(*Principal)
	return p, ok
}

// securityDefaults 返回安全相关的默认值，避免各处硬编码。
func (d Deps) securityDefaults() struct {
	SessionTTL     time.Duration
	LeaseTTL       time.Duration
	HeartbeatEvery time.Duration
	RevokeCooldown time.Duration
	MaxLoginFail   int
	LoginLock      time.Duration
	LoginWindow    time.Duration
	EnrollTTL      time.Duration
} {
	raw := d.raw().Security
	out := struct {
		SessionTTL     time.Duration
		LeaseTTL       time.Duration
		HeartbeatEvery time.Duration
		RevokeCooldown time.Duration
		MaxLoginFail   int
		LoginLock      time.Duration
		LoginWindow    time.Duration
		EnrollTTL      time.Duration
	}{
		SessionTTL:     raw.SessionTTL.Std(),
		LeaseTTL:       raw.LeaseTTL.Std(),
		HeartbeatEvery: raw.LeaseHeartbeatInterval.Std(),
		RevokeCooldown: raw.RevokeCooldown.Std(),
		MaxLoginFail:   raw.LoginMaxFailures,
		LoginLock:      raw.LoginLock.Std(),
		LoginWindow:    raw.LoginLockWindow.Std(),
		EnrollTTL:      raw.EnrollmentTokenTTL.Std(),
	}
	if out.SessionTTL <= 0 {
		out.SessionTTL = 12 * time.Hour
	}
	if out.LeaseTTL <= 0 {
		out.LeaseTTL = 120 * time.Second
	}
	if out.HeartbeatEvery <= 0 {
		out.HeartbeatEvery = 30 * time.Second
	}
	if out.RevokeCooldown <= 0 {
		out.RevokeCooldown = 5 * time.Minute
	}
	if out.MaxLoginFail <= 0 {
		out.MaxLoginFail = 5
	}
	if out.LoginLock <= 0 {
		out.LoginLock = 15 * time.Minute
	}
	if out.LoginWindow <= 0 {
		out.LoginWindow = 15 * time.Minute
	}
	if out.EnrollTTL <= 0 {
		out.EnrollTTL = 15 * time.Minute
	}
	return out
}
