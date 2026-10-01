package app

import (
	"context"
	"strings"

	"vault/internal/apperr"
	"vault/internal/domain"
	"vault/internal/secret"
)

// UserService 负责本地用户的增删改查。
//
// 用户是"分配资源的载体"（见 design.md 服务端-本地用户-2），
// 因此删除用户前需由业务层确保其名下分配已回收。
type UserService struct {
	Deps
}

// CreateUserInput 是创建用户的入参。
type CreateUserInput struct {
	Username   string
	Password   string
	Role       domain.Role
	QuotaBytes int64
	Remark     string
}

// UpdateUserInput 是更新用户的入参。指针字段为 nil 表示不修改。
type UpdateUserInput struct {
	ID         string
	Role       *domain.Role
	QuotaBytes *int64
	Remark     *string
	Enabled    *bool
	Password   *string
}

// Create 创建用户。口令以 argon2id 哈希存储，绝不落明文。
func (s *UserService) Create(ctx context.Context, in CreateUserInput) (*domain.User, error) {
	username := strings.TrimSpace(in.Username)
	if username == "" {
		return nil, apperr.InvalidParam("username")
	}
	if strings.TrimSpace(in.Password) == "" {
		return nil, apperr.InvalidParam("password")
	}
	if !in.Role.Valid() {
		return nil, apperr.InvalidParam("role")
	}

	hash, err := secret.HashPassword(in.Password)
	if err != nil {
		return nil, err
	}

	u := &domain.User{
		Username:     username,
		Role:         in.Role,
		PasswordHash: hash,
		Enabled:      true,
		QuotaBytes:   in.QuotaBytes,
		Remark:       in.Remark,
	}
	if err := s.Store.CreateUser(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

// Get 按 ID 查询用户。
func (s *UserService) Get(ctx context.Context, id string) (*domain.User, error) {
	return s.Store.GetUserByID(ctx, id)
}

// GetByUsername 按用户名查询用户。
func (s *UserService) GetByUsername(ctx context.Context, username string) (*domain.User, error) {
	return s.Store.GetUserByUsername(ctx, username)
}

// List 列出用户。
func (s *UserService) List(ctx context.Context, keyword string, limit, offset int) ([]domain.User, error) {
	return s.Store.ListUsers(ctx, keyword, limit, offset)
}

// Count 统计用户数量。
func (s *UserService) Count(ctx context.Context) (int, error) {
	return s.Store.CountUsers(ctx)
}

// Update 更新用户可变字段。
func (s *UserService) Update(ctx context.Context, in UpdateUserInput) (*domain.User, error) {
	u, err := s.Get(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	if in.Role != nil {
		if !in.Role.Valid() {
			return nil, apperr.InvalidParam("role")
		}
		u.Role = *in.Role
	}
	if in.QuotaBytes != nil {
		u.QuotaBytes = *in.QuotaBytes
	}
	if in.Remark != nil {
		u.Remark = *in.Remark
	}
	if in.Enabled != nil {
		u.Enabled = *in.Enabled
	}
	if in.Password != nil && strings.TrimSpace(*in.Password) != "" {
		hash, err := secret.HashPassword(*in.Password)
		if err != nil {
			return nil, err
		}
		u.PasswordHash = hash
	}
	if err := s.Store.UpdateUser(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

// Delete 删除用户。
//
// 调用方（API 层）需先确认该用户没有进行中的分配与在线租约。
func (s *UserService) Delete(ctx context.Context, id string) error {
	return s.Store.DeleteUser(ctx, id)
}
