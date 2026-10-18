package domain

import (
	"testing"

	"vault/internal/apperr"
)

// TestParentTransitionsAfterDerivedRemoval 锁定"derived 状态已移除"后的迁移规则：
//   - 正常态允许带着差异盘继续派生、进入维护；
//   - 进入维护不要求先清理差异盘，也不禁止在线客户端（清理任务会负责断开）；
//   - 退出维护（回 idle）仍要求无在线租约。
func TestParentTransitionsAfterDerivedRemoval(t *testing.T) {
	cases := []struct {
		name         string
		from         ParentCondition
		to           ParentCondition
		diffCount    int
		activeLeases int
		wantCode     string
	}{
		{"正常态带差异盘进入维护", ParentIdle, ParentMaintenance, 5, 2, ""},
		{"正常态无差异盘进入维护", ParentIdle, ParentMaintenance, 0, 0, ""},
		{"临时共享不能进维护", ParentTempShared, ParentMaintenance, 0, 0, "repo.parent_condition_violation"},
		{"退出维护要求无在线租约", ParentMaintenance, ParentIdle, 0, 1, "repo.has_active_lease"},
		{"退出维护且无在线连接", ParentMaintenance, ParentIdle, 3, 0, ""},
		{"临时共享要求无差异盘", ParentIdle, ParentTempShared, 2, 0, "repo.parent_condition_violation"},
		{"撤销临时共享", ParentTempShared, ParentIdle, 0, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := (TransitionRequest{
				From: c.from, To: c.to, DiffCount: c.diffCount, ActiveLeases: c.activeLeases,
			}).Validate()
			if c.wantCode == "" {
				if err != nil {
					t.Fatalf("期望迁移成功，实际 %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("期望迁移失败，实际成功")
			}
			if got := apperr.CodeOf(err); got != c.wantCode {
				t.Fatalf("错误码 = %q，期望 %q（err=%v）", got, c.wantCode, err)
			}
		})
	}
}

// TestCanDeriveAndAllocateWithoutDerivedState 差异盘不再是状态：正常态可以带着子盘继续派生，
// 维护/临时共享态禁止分配与派生。
func TestCanDeriveAndAllocateWithoutDerivedState(t *testing.T) {
	if err := CanDerive(ParentIdle, 3, 5); err != nil {
		t.Fatalf("正常态带 3 个差异盘应仍可派生：%v", err)
	}
	if err := CanDerive(ParentIdle, 5, 5); err == nil || apperr.CodeOf(err) != "repo.diff_limit_exceeded" {
		t.Fatalf("达到上限应返回差异盘上限错误，实际 %v", err)
	}
	if err := CanDerive(ParentMaintenance, 0, 5); err == nil {
		t.Fatal("维护态不应允许派生")
	}
	if err := CanAllocate(ParentTempShared); err == nil {
		t.Fatal("临时共享态不应允许分配")
	}
	if err := CanAllocate(ParentIdle); err != nil {
		t.Fatalf("正常态应允许分配：%v", err)
	}
}

// TestCanMountRequiresIdle 只有正常态才能挂载差异盘；维护/临时共享态必须拒绝。
func TestCanMountRequiresIdle(t *testing.T) {
	if err := CanMount(ParentIdle, 2, 2, true); err != nil {
		t.Fatalf("正常态且版本/指纹一致应允许挂载：%v", err)
	}
	if err := CanMount(ParentMaintenance, 2, 2, true); err == nil {
		t.Fatal("维护态不应允许挂载")
	}
	if err := CanMount(ParentTempShared, 2, 2, true); err == nil {
		t.Fatal("临时共享态不应允许挂载")
	}
	if err := CanMount(ParentIdle, 1, 2, true); err == nil {
		t.Fatal("母盘版本不一致应拒绝挂载")
	}
}
