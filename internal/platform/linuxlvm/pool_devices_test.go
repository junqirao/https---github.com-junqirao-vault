//go:build linux

package linuxlvm

import (
	"context"
	"net/http"
	"testing"

	"vault/internal/apperr"
	"vault/internal/platform"
)

// 本文件锁住"容量盘与缓存盘互斥"这条约束：
//
//	一块盘不可能既承载 thin pool 又承载 cache pool（两者最终都要落进同一个卷组），
//	真走到 LVM 只会得到一句看不懂的报错，因此必须在动手之前拒绝。
//	前端已把两个选择器做成互斥勾选，这里守的是"直连 API"这条路。

// TestInitializePoolRejectsCacheOverlap：同一块设备同时出现在 hdd_devices 与
// cache_devices 时必须返回可翻译的业务错误，并指出是哪一块盘。
func TestInitializePoolRejectsCacheOverlap(t *testing.T) {
	// 这组假输出是"卷组与池都已存在"（幂等路径）：万一校验被误删，
	// 用例不会因为真的去建池而跑偏，但下面的断言会立刻失败。
	fakeLVM(t,
		`{"report":[{"vg":[{"vg_name":"vg0","vg_size":"1000","vg_free":"900"}]}]}`,
		`{"report":[{"lv":[{"lv_name":"vault"}]}]}`,
		false)

	m := New(Options{VG: "vg0", ThinPool: "vault", Logger: discardLogger()})
	err := m.InitializePool(context.Background(), platform.PoolSpec{
		VG:         "vg0",
		ThinPool:   "vault",
		SizeBytes:  1 << 30,
		HDDDevices: []string{"/dev/sdb", "/dev/sdc"},
		Cache:      &platform.CacheSpec{Devices: []string{"/dev/sdc"}},
	})

	e, ok := apperr.As(err)
	if !ok {
		t.Fatalf("应为业务错误（可被前端翻译），实际: %v", err)
	}
	if e.Code != "platform.cache_device_overlap" {
		t.Fatalf("错误码 = %s，期望 platform.cache_device_overlap", e.Code)
	}
	if e.HTTP != http.StatusBadRequest {
		t.Fatalf("HTTP 状态 = %d，期望 %d", e.HTTP, http.StatusBadRequest)
	}
	if e.Args["device"] != "/dev/sdc" {
		t.Fatalf("参数应带上发生重叠的设备，实际: %v", e.Args)
	}
}

// TestFirstSharedPath：交集判定要容忍空格，且不把空串当"重叠"。
func TestFirstSharedPath(t *testing.T) {
	cases := []struct {
		name string
		a    []string
		b    []string
		want string
	}{
		{"无重叠", []string{"/dev/sdb"}, []string{"/dev/sdc"}, ""},
		{"有重叠", []string{"/dev/sdb", "/dev/sdc"}, []string{"/dev/sdc"}, "/dev/sdc"},
		{"忽略空格", []string{"/dev/sdb "}, []string{" /dev/sdb"}, "/dev/sdb"},
		{"任一侧为空", []string{"/dev/sdb"}, nil, ""},
		{"双方都有空元素", []string{""}, []string{" "}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstSharedPath(tc.a, tc.b); got != tc.want {
				t.Fatalf("firstSharedPath = %q，期望 %q", got, tc.want)
			}
		})
	}
}
