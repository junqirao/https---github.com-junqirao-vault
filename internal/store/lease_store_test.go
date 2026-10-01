package store

import (
	"context"
	"testing"

	"vault/internal/apperr"
	"vault/internal/domain"
)

// 过期租约必须能被续期（可恢复），吊销必须被拒绝（终态）。
//
// 回归："挂载成功后隔一两分钟盘符就没了，但服务端目标还在"：
// 一旦过期被判成 revoked（或 TouchLease 只认 active），客户端此后每一次心跳都会拿到
// lease_revoked，于是主动卸载一个其实完好的挂载；服务端又故意不停用目标，
// 正好表现为"盘符消失、目标仍在"。
func TestTouchLeaseAcceptsExpiredButRejectsRevoked(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	now := nowMillis()
	lease := &domain.Lease{
		AllocationID: "alloc-1",
		TargetName:   "iqn.2026-01.com.vault:vault-alloc-1-target",
		UserID:       "user-1",
		ClientID:     "client-1",
		State:        domain.LeaseStateActive,
		ExpiresAt:    now - 1000,
		LastSeenAt:   now - 1000,
	}
	if err := s.UpsertLease(ctx, lease); err != nil {
		t.Fatalf("写入租约失败: %v", err)
	}

	// 已过期的 active 租约应被回收任务捞到。
	expired, err := s.ListExpiredLeases(ctx, now, 10)
	if err != nil {
		t.Fatalf("查询过期租约失败: %v", err)
	}
	if len(expired) != 1 || expired[0].ID != lease.ID {
		t.Fatalf("过期租约 = %+v，期望只含 %s", expired, lease.ID)
	}

	if err := s.UpdateLeaseState(ctx, lease.ID, domain.LeaseStateExpired); err != nil {
		t.Fatalf("标记过期失败: %v", err)
	}

	// 关键断言 1：expired 不是终态，心跳必须能续上（客户端因此不会被误卸载）。
	if err := s.TouchLease(ctx, lease.ID, now+60000); err != nil {
		t.Fatalf("过期租约应可续期，实际: %v", err)
	}
	// 标记为过期后不再重复出现在待回收列表里（回收任务每个 tick 不该反复处理同一条）。
	again, err := s.ListExpiredLeases(ctx, now, 10)
	if err != nil {
		t.Fatalf("再次查询过期租约失败: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("已标记过期的租约不该再次被回收: %+v", again)
	}

	// 关键断言 2：revoked 是终态，心跳必须被拒（客户端据此主动卸载）。
	if err := s.UpdateLeaseState(ctx, lease.ID, domain.LeaseStateRevoked); err != nil {
		t.Fatalf("吊销失败: %v", err)
	}
	if err := s.TouchLease(ctx, lease.ID, now+60000); apperr.CodeOf(err) != "lease.revoked" {
		t.Fatalf("吊销后的心跳应返回 lease.revoked，实际: %v", err)
	}
}
