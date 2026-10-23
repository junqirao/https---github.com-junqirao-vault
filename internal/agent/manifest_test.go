package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// TestRunManifestHash 验证清单任务：扫描 → 逐文件 SHA-256 → 成功，
// 且进度里能看到"当前文件"与已处理字节（界面靠它展示正在处理哪个文件）。
func TestRunManifestHash(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"a.txt":     "hello",
		"sub/b.bin": "world!",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	a := &Agent{}
	mgr := newManifestManager()
	task := mgr.create(context.Background(), dir, true)
	a.runManifest(task)

	st := task.status()
	if st.State != manifestStateSucceeded {
		t.Fatalf("任务状态 = %s，错误 = %s", st.State, st.ErrorMessage)
	}
	if st.Total != 2 || st.Done != 2 || st.Bytes != int64(len("hello")+len("world!")) {
		t.Fatalf("汇总不对：%+v", st)
	}
	res, ok := task.result()
	if !ok || res.FileCount != 2 {
		t.Fatalf("结果不可用：ok=%v res=%+v", ok, res)
	}
	for _, e := range res.Entries {
		want := sha256.Sum256([]byte(files[e.Path]))
		if e.Hash != hex.EncodeToString(want[:]) {
			t.Fatalf("%s 的哈希不对：%s", e.Path, e.Hash)
		}
	}
}

// TestRunManifestSizeOnly 只比大小时不应计算哈希（Hash 留空）。
func TestRunManifestSizeOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &Agent{}
	task := newManifestManager().create(context.Background(), dir, false)
	a.runManifest(task)

	st := task.status()
	if st.State != manifestStateSucceeded || st.Done != 1 {
		t.Fatalf("任务状态不对：%+v", st)
	}
	res, ok := task.result()
	if !ok || len(res.Entries) != 1 || res.Entries[0].Hash != "" {
		t.Fatalf("size 模式不应有哈希：%+v", res)
	}
}
