package domain

import (
	"vault/internal/apperr"
)

// parentTransitions 定义母盘用途条件的合法迁移（见 docs/implementation.md 4.3）。
//
// 语义提醒：
//   - 母盘一旦派生了差异盘，差异盘即假设母盘内容不再变化；
//     因此 derived 状态下**不允许**再以可写方式挂载或只读共享母盘；
//   - maintenance 用于更新母盘内容，进入前必须先清理全部差异盘。
var parentTransitions = map[ParentCondition]map[ParentCondition]bool{
	ParentIdle: {
		ParentDerived:     true, // 创建差异盘（分配）
		ParentTempShared:  true, // 临时只读共享给客户端
		ParentMaintenance: true, // 进入维护，读写挂载更新母盘
	},
	ParentDerived: {
		ParentIdle:        true, // 清理全部差异盘
		ParentMaintenance: true, // 等价于 derived→idle→maintenance，业务层需先清理并二次确认
	},
	ParentTempShared: {
		ParentIdle: true, // 撤销临时共享
	},
	ParentMaintenance: {
		ParentIdle: true, // 完成更新并卸载母盘
	},
}

// TransitionRequest 描述一次母盘条件迁移请求及其判定所需的事实。
type TransitionRequest struct {
	From ParentCondition
	To   ParentCondition

	// DiffCount 当前差异盘数量（含未挂载的）。
	DiffCount int
	// ActiveLeases 当前指向该库的活跃租约数量。
	ActiveLeases int
}

// Validate 校验迁移是否合法。
//
// 所有不合法的迁移都返回业务错误，便于 API 层直接映射为 HTTP 状态码与可翻译的错误码。
func (r TransitionRequest) Validate() error {
	if !r.From.Valid() {
		return apperr.RepoParentConditionViolation(string(r.From), "合法状态")
	}
	if !r.To.Valid() {
		return apperr.InvalidParam("to")
	}
	if r.From == r.To {
		return apperr.New("repo.parent_condition_noop", 409).
			WithArg("current", string(r.From))
	}

	allowed, ok := parentTransitions[r.From]
	if !ok || !allowed[r.To] {
		return apperr.RepoParentConditionViolation(string(r.From), string(r.To))
	}

	switch r.To {
	case ParentDerived:
		// 只有在"无差异盘、未共享、未挂载"的 idle 态才能派生。
		// 数量上限由业务层按 repositories.max_diff_disks 单独校验。
		return nil

	case ParentTempShared:
		// 只要存在差异盘（无论是否在线）就不能共享母盘，
		// 否则母盘内容一旦变化会污染全部子盘。
		if r.DiffCount > 0 {
			return apperr.RepoParentConditionViolation(
				string(r.From), "无差异盘（当前 "+itoa(r.DiffCount)+" 个）")
		}
		return nil

	case ParentMaintenance:
		// 进入维护：必须先清理完全部差异盘，且无人挂载。
		if r.DiffCount > 0 {
			return apperr.New("repo.maintenance_requires_cleanup", 409).
				WithArg("diff_count", r.DiffCount)
		}
		if r.ActiveLeases > 0 {
			return apperr.RepoHasActiveLease(r.ActiveLeases)
		}
		return nil

	case ParentIdle:
		// 回到 idle 要求无人在线：清理差异盘 / 撤销共享 / 完成维护都不能有人正在使用。
		if r.ActiveLeases > 0 {
			return apperr.RepoHasActiveLease(r.ActiveLeases)
		}
		return nil
	}
	return apperr.RepoParentConditionViolation(string(r.From), string(r.To))
}

// CanDerive 判断当前母盘条件下是否允许再派生一个差异盘。
//
// limit 为该库配置的 max_diff_disks（0 表示不限制）。
func CanDerive(cond ParentCondition, diffCount, limit int) error {
	if cond != ParentIdle {
		return apperr.RepoParentConditionViolation(string(cond), string(ParentIdle))
	}
	if limit > 0 && diffCount >= limit {
		return apperr.RepoDiffLimitExceeded(limit)
	}
	return nil
}

// CanMount 判断差异盘当前是否允许被挂载。
//
// 除状态条件外，还要求子盘记录的 parent_version 与当前母盘版本一致，
// 以及母盘指纹未被篡改（见 docs/implementation.md 5.4）。
func CanMount(cond ParentCondition, childParentVersion, currentParentVersion int, fingerprintOK bool) error {
	if cond != ParentDerived {
		// 母盘被临时共享或处于维护态时，其差异盘不应还在被使用。
		return apperr.RepoParentConditionViolation(string(cond), string(ParentDerived))
	}
	if !fingerprintOK {
		return apperr.DiskParentFingerprintMismatch()
	}
	if childParentVersion != currentParentVersion {
		return apperr.DiskParentVersionMismatch(childParentVersion, currentParentVersion)
	}
	return nil
}

// itoa 避免为了一处格式化引入 strconv 依赖到热路径。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
