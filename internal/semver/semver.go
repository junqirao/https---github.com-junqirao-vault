// Package semver 实现本项目所需的轻量版本比较。
//
// 相比通用 semver 库，这里额外支持**通配尾段**（如 "1.9.x"、"1.x"），
// 用于表达"兼容区间"的上界（见 docs/implementation.md 3.4.2 约束 2）。
// 禁止用字符串比较版本，必须走本包。
package semver

import (
	"fmt"
	"strconv"
	"strings"
)

// Wildcard 表示通配层级。
type Wildcard int

const (
	// NoWildcard 形如 1.4.0，三段都参与比较。
	NoWildcard Wildcard = 0
	// MinorWildcard 形如 1.x，只比较 major。
	MinorWildcard Wildcard = 1
	// PatchWildcard 形如 1.9.x，比较 major 与 minor。
	PatchWildcard Wildcard = 2
)

// Version 是一个可能带通配的版本号。
type Version struct {
	Major    int
	Minor    int
	Patch    int
	Wildcard Wildcard
}

// Parse 解析版本字符串。支持 "1"、"1.2"、"1.2.3"、"1.x"、"1.2.x"。
// 前缀 "v" 会被忽略。
func Parse(s string) (Version, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return Version{}, fmt.Errorf("semver: 版本为空")
	}
	raw = strings.TrimPrefix(raw, "v")
	// 允许 "1.2.3-beta.1" 这类预发布后缀，仅取主干。
	if i := strings.IndexAny(raw, "-+"); i >= 0 {
		raw = raw[:i]
	}

	parts := strings.Split(raw, ".")
	if len(parts) > 3 {
		return Version{}, fmt.Errorf("semver: 版本段过多: %q", s)
	}

	var v Version
	out := make([]int, 3)
	for idx, p := range parts {
		p = strings.TrimSpace(p)
		switch strings.ToLower(p) {
		case "x", "*":
			// 通配必须出现在尾部。
			if idx == 0 {
				return Version{}, fmt.Errorf("semver: major 不允许通配: %q", s)
			}
			if idx != len(parts)-1 {
				return Version{}, fmt.Errorf("semver: 通配必须位于末尾: %q", s)
			}
			if idx == 1 {
				v.Wildcard = MinorWildcard
			} else {
				v.Wildcard = PatchWildcard
			}
			out[idx] = 0
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return Version{}, fmt.Errorf("semver: 非法版本段 %q in %q", p, s)
		}
		out[idx] = n
	}

	v.Major, v.Minor, v.Patch = out[0], out[1], out[2]
	return v, nil
}

// MustParse 用于常量与测试，解析失败会 panic。
func MustParse(s string) Version {
	v, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return v
}

func (v Version) String() string {
	switch v.Wildcard {
	case MinorWildcard:
		return fmt.Sprintf("%d.x", v.Major)
	case PatchWildcard:
		return fmt.Sprintf("%d.%d.x", v.Major, v.Minor)
	default:
		return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	}
}

// Compare 按已定义部分比较两个无通配版本。v 带通配时按通配下界处理。
func Compare(a, b Version) int {
	if c := cmpInt(a.Major, b.Major); c != 0 {
		return c
	}
	if c := cmpInt(a.Minor, b.Minor); c != 0 {
		return c
	}
	return cmpInt(a.Patch, b.Patch)
}

// GTE 判断 v 是否 >= min（即满足"最低版本"要求）。
// min 带通配时按通配下界处理，例如 min=1.4.x 等价于 >= 1.4.0。
func (v Version) GTE(min Version) bool {
	switch min.Wildcard {
	case MinorWildcard:
		return v.Major >= min.Major
	case PatchWildcard:
		return v.Major > min.Major || (v.Major == min.Major && v.Minor >= min.Minor)
	default:
		return Compare(v, min) >= 0
	}
}

// LTE 判断 v 是否 <= max（即满足"最高版本"要求）。
// max 带通配时按"该通配段的全部取值"处理，例如 v=1.9.7、max=1.9.x 判定为真。
func (v Version) LTE(max Version) bool {
	switch max.Wildcard {
	case MinorWildcard:
		return v.Major <= max.Major
	case PatchWildcard:
		return v.Major < max.Major || (v.Major == max.Major && v.Minor <= max.Minor)
	default:
		return Compare(v, max) <= 0
	}
}

// InRange 判断 v 是否落在 [min, max] 区间内。
// min / max 为 nil 表示该端不限制。
func InRange(v Version, min, max *Version) bool {
	if min != nil && !v.GTE(*min) {
		return false
	}
	if max != nil && !v.LTE(*max) {
		return false
	}
	return true
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
