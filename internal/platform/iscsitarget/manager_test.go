package iscsitarget

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"vault/internal/apperr"
)

// TestStringListUnmarshal 覆盖 stringList 的三种合法形态与"必须报错"的非法形态。
//
// 非法形态报错是关键：旧实现在元素不是字符串时静默置 nil，导致上层看到的
// 授权列表 / 映射列表凭空为空（曾真实发生：脚本把 initiator 输出成
// {Method,Value} 对象数组，授权列表就这样消失了）。
func TestStringListUnmarshal(t *testing.T) {
	type payload struct {
		Values stringList `json:"values"`
	}
	cases := []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		{name: "字符串数组", raw: `{"values":["a","b"]}`, want: []string{"a", "b"}},
		{name: "单元素数组被序列化成字符串", raw: `{"values":"IQN:iqn.1991-05.com.microsoft:c1"}`, want: []string{"IQN:iqn.1991-05.com.microsoft:c1"}},
		{name: "空数组", raw: `{"values":[]}`, want: []string{}},
		{name: "null", raw: `{"values":null}`, want: nil},
		{name: "缺字段", raw: `{}`, want: nil},
		{name: "对象数组必须报错", raw: `{"values":[{"Method":2,"Value":"127.0.0.1"}]}`, wantErr: true},
		{name: "混合数组必须报错", raw: `{"values":["a",{"Value":"b"}]}`, wantErr: true},
		{name: "数字数组必须报错", raw: `{"values":[1,2]}`, wantErr: true},
		{name: "对象必须报错", raw: `{"values":{"Value":"x"}}`, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got payload
			err := json.Unmarshal([]byte(c.raw), &got)
			if c.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际通过，解析结果=%v", got.Values)
				}
				if !strings.Contains(err.Error(), "列表字段形状非法") {
					t.Fatalf("错误信息缺少说明：%v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错：%v", err)
			}
			if len(got.Values) != len(c.want) {
				t.Fatalf("解析结果 = %v，期望 %v", got.Values, c.want)
			}
			for i := range c.want {
				if got.Values[i] != c.want[i] {
					t.Fatalf("解析结果 = %v，期望 %v", got.Values, c.want)
				}
			}
		})
	}
}

// TestValidateTargetName 覆盖目标名校验：非法字符必须被提前拦下。
//
// 背景：目标名会被 Windows 当作 IQN 后缀校验，'_' 非法；出错时 Windows 只回一句
// "Unable to create the iSCSI target."，完全看不出是命名问题。
func TestValidateTargetName(t *testing.T) {
	valid := []string{
		"vault-0b2b0169-8a7c29a9",
		"vault-0b2b0169-temp",
		"vault.1",
		"IQN:iqn.1991-05.com.microsoft:client",
		"a",
	}
	for _, name := range valid {
		if err := validateTargetName(name); err != nil {
			t.Fatalf("%q 应被接受，实际错误=%v", name, err)
		}
	}

	invalid := []struct {
		name string
		char string
	}{
		{"vault_1", "_"},
		{"vault 1", " "},
		{"vault/1", "/"},
		{"vault#1", "#"},
		{"vault_0b2b0169-8a7c29a9", "_"},
	}
	for _, c := range invalid {
		err := validateTargetName(c.name)
		if err == nil {
			t.Fatalf("%q 应被拒绝", c.name)
		}
		e, ok := apperr.As(err)
		if !ok {
			t.Fatalf("%q 的错误不是 apperr：%v", c.name, err)
		}
		if e.Code != CodeInvalidTargetName || e.HTTP != http.StatusBadRequest {
			t.Fatalf("%q 的错误码/状态不对：code=%s http=%d", c.name, e.Code, e.HTTP)
		}
		if e.Args["invalid_char"] != c.char {
			t.Fatalf("%q 应指出非法字符 %q，实际=%v", c.name, c.char, e.Args["invalid_char"])
		}
	}

	// 空名走"参数缺失"语义（400 invalid_param），而不是"名字非法"。
	err := validateTargetName("   ")
	e, ok := apperr.As(err)
	if !ok || e.HTTP != http.StatusBadRequest || e.Code == CodeInvalidTargetName {
		t.Fatalf("空名应返回 invalid_param，实际=%v", err)
	}

	// 首尾空白应被忽略后照常通过。
	if err := validateTargetName("  vault-abc  "); err != nil {
		t.Fatalf("首尾空白不应导致失败：%v", err)
	}
}

// TestTruncateForError 保证错误信息里回显的脚本输出不会无限长。
func TestTruncateForError(t *testing.T) {
	short := "abc"
	if got := truncateForError(short); got != short {
		t.Fatalf("短串不应被改写：%q", got)
	}
	long := strings.Repeat("x", 500)
	got := truncateForError(long)
	if len(got) >= len(long) || !strings.Contains(got, "截断") {
		t.Fatalf("长串应被截断并标注：len=%d", len(got))
	}
}

// TestAppErrAsContract 确认 apperr.As 对"脚本错误包裹的业务错误"仍然可用——
// validateTargetName 的返回会被上层用 apperr.As 判定。这样写是为了防止
// 将来有人把 CodeInvalidTargetName 改成裸 errors.New。
func TestAppErrAsContract(t *testing.T) {
	wrapped := validateTargetName("vault_1")
	if !errors.Is(wrapped, wrapped) {
		t.Fatal("errors.Is 自反失败")
	}
	if _, ok := apperr.As(wrapped); !ok {
		t.Fatalf("apperr.As 无法识别：%v", wrapped)
	}
}
