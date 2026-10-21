package app

import "testing"

// TestIsVerifyRootMetadata 只过滤**卷根**的系统元数据；内容目录里的同名路径必须保留，
// 否则会把用户真实上传的目录误当成系统目录跳过。
func TestIsVerifyRootMetadata(t *testing.T) {
	ignored := []string{
		"$RECYCLE.BIN/S-1-5-21/x.bin",
		"System Volume Information/tracking.log",
		"lost+found/#12345",
		"pagefile.sys",
		"hiberfil.sys",
		"dumpstack.log.tmp",
	}
	for _, p := range ignored {
		if !isVerifyRootMetadata(p) {
			t.Fatalf("%q 应被忽略", p)
		}
	}
	kept := []string{
		"Games/$RECYCLE.BIN/x.bin",
		"Games/System Volume Information/x.log",
		"Games/lost+found/x",
		"Games/pagefile.sys",
		"pagefile.sys.bak",
		"data.bin",
	}
	for _, p := range kept {
		if isVerifyRootMetadata(p) {
			t.Fatalf("%q 不应被忽略", p)
		}
	}
}

// TestCompareVerify 锁定上传核对的分类口径：
// 本地有/盘里没有=missing；盘里有/本地没有=extra；大小或哈希不同=different。
func TestCompareVerify(t *testing.T) {
	manifest := verifyDiffManifest{
		Directory: "C:\\Games\\X",
		Mode:      "hash",
		Files: []VerifyDiffFile{
			{Path: "a.txt", Size: 10, Hash: "aaa"},
			{Path: "b.txt", Size: 20, Hash: "bbb"},
			{Path: "c.txt", Size: 30, Hash: "ccc"},
			{Path: "d.txt", Size: 40, Hash: "ddd2"},
		},
	}
	disk := map[string]verifyDiskFile{
		"a.txt":  {size: 10, hash: "aaa"},
		"b.txt":  {size: 21, hash: "bbb"},
		"c2.txt": {size: 50, hash: "zzz"},
		"d.txt":  {size: 40, hash: "ddd"},
	}
	res := compareVerify(manifest, disk, 121, nil)

	if res.Same != 1 {
		t.Fatalf("相同文件应为 1，实际 %d", res.Same)
	}
	if len(res.Missing) != 1 || res.Missing[0].Path != "c.txt" {
		t.Fatalf("缺失列表不对：%+v", res.Missing)
	}
	if len(res.Extra) != 1 || res.Extra[0].Path != "c2.txt" {
		t.Fatalf("多余列表不对：%+v", res.Extra)
	}
	// b.txt 大小不同；d.txt 大小相同但两边都有哈希且不同 → 内容不同。
	if len(res.Different) != 2 {
		t.Fatalf("不同列表应为 2 条，实际 %+v", res.Different)
	}
	reasons := map[string]string{}
	for _, e := range res.Different {
		reasons[e.Path] = e.Reason
	}
	if reasons["b.txt"] != "size" || reasons["d.txt"] != "hash" {
		t.Fatalf("差异原因不对：%+v", reasons)
	}
	if res.LocalFiles != 4 || res.DiskFiles != 4 || res.LocalBytes != 100 || res.DiskBytes != 121 {
		t.Fatalf("汇总不对：%+v", res)
	}
}

// TestCompareVerifyMissingLocalHash 本地没算出哈希（读失败/未请求）时退化为只比大小，
// 避免把"本地读不了"误报成"内容不同"。
func TestCompareVerifyMissingLocalHash(t *testing.T) {
	manifest := verifyDiffManifest{
		Mode:  "hash",
		Files: []VerifyDiffFile{{Path: "a", Size: 10}},
	}
	res := compareVerify(manifest, map[string]verifyDiskFile{"a": {size: 10, hash: "whatever"}}, 10, nil)
	if res.Same != 1 || len(res.Different) != 0 {
		t.Fatalf("本地无哈希时应只比大小：%+v", res)
	}
}

// TestCompareVerifySizeMode 只比大小时不应因为磁盘侧有哈希而报内容不同。
func TestCompareVerifySizeMode(t *testing.T) {
	manifest := verifyDiffManifest{
		Mode:  "size",
		Files: []VerifyDiffFile{{Path: "a", Size: 10}},
	}
	res := compareVerify(manifest, map[string]verifyDiskFile{"a": {size: 10, hash: "whatever"}}, 10, nil)
	if res.Same != 1 || len(res.Different) != 0 {
		t.Fatalf("size 模式只应比大小：%+v", res)
	}
}
