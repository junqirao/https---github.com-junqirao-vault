package iscsiproxy

import (
	"io"
	"log/slog"
	"testing"
)

// TestPortalDropCacheUnknownKey 锁定：清一个没注册的 key 是"没有可清的缓存"，
// 不是错误，也不能把门户本身搞坏 —— 界面可能对着一个没挂载的库点"清空缓存"。
func TestPortalDropCacheUnknownKey(t *testing.T) {
	p, err := NewPortal(PortalConfig{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("NewPortal 失败：%v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	if p.DropCache("alloc-unknown") {
		t.Fatal("未注册的 key 不该报告清空成功")
	}
	if p.DropCache("  ") {
		t.Fatal("空 key 不该报告清空成功")
	}
	if p.Has("alloc-unknown") {
		t.Fatal("清一个不存在的目标不该把它注册进来")
	}
}
