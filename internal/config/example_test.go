package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestPlatformizeExample 锁住"模板按平台改写"的两处字面量。
//
// 为什么必须有这条测试：模板 config.example.yaml 是两个平台共用的一份，里面的存储种子
// 默认值只能写成一个平台的形态（Windows 的 D:\VaultData）。若改写漏掉任何一处，
// 另一个平台就会生成一份**必然被 validate 拒绝**的配置，而且表现为"服务端启动不起来、
// 报绝对路径非法"这种与模板看似无关的故障（真实反馈）。
func TestPlatformizeExample(t *testing.T) {
	// 与模板里两处字面量一一对应：YAML 双引号内的值，以及注释里的说明。
	in := []byte("# 建议独立数据盘，例如 " + winStorageRoot + "\n" +
		"  whitelist_root: \"" + winStorageRootYAMLLiteral + "\"\n" +
		"  whitelist_roots:\n    - \"" + winStorageRootYAMLLiteral + "\"\n")

	// Windows：模板本身就是本平台形态，一个字节都不该动。
	if got := platformizeExample("windows", in); string(got) != string(in) {
		t.Fatalf("windows 下不应改写模板，得到:\n%s", got)
	}

	out := string(platformizeExample("linux", in))
	for _, bad := range []string{winStorageRootYAMLLiteral, winStorageRoot} {
		if strings.Contains(out, bad) {
			t.Fatalf("linux 改写后仍残留 Windows 路径 %q:\n%s", bad, out)
		}
	}
	if n := strings.Count(out, linuxStorageRoot); n != 3 {
		t.Fatalf("linux 默认根应出现 3 次（注释 1 + 值 2），实际 %d 次:\n%s", n, out)
	}
}

// TestEnsureFromExampleGeneratesLoadableConfig 用**仓库里真实的模板**走一遍生成链路：
// 生成的 config.yaml 必须能直接被 Load 通过。
//
// 这是本次故障的回归测试：此前 Linux 上生成出来的配置含 D:\VaultData，
// Load 会报 `storage.whitelist_roots 的每个根都必须是绝对路径`，服务端起不来。
// 该断言天然分平台成立（Windows 用 D:\VaultData、其它平台用 /var/lib/vault）。
func TestEnsureFromExampleGeneratesLoadableConfig(t *testing.T) {
	// 环境变量覆盖会掩盖模板取值，先把相关项清空（空串在 applyEnvOverrides 里等同未设置）。
	for _, env := range []string{
		"VAULT_STORAGE_ROOT", "VAULT_STORAGE_ROOTS", "VAULT_DB_DRIVER",
		"VAULT_DB_SQLITE_PATH", "VAULT_DB_MYSQL_DSN", "VAULT_HTTP_LISTEN",
	} {
		t.Setenv(env, "")
	}

	src := filepath.Join("..", "..", "config.example.yaml")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("读取仓库模板 %s 失败（本测试依赖仓库布局）: %v", src, err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.example.yaml"), data, 0o644); err != nil {
		t.Fatalf("写入模板副本失败: %v", err)
	}
	target := filepath.Join(dir, "config.yaml")

	created, err := EnsureFromExample(target)
	if err != nil {
		t.Fatalf("从模板生成配置失败: %v", err)
	}
	if !created {
		t.Fatal("目标不存在时应报告 created=true")
	}

	loaded, err := Load(target)
	if err != nil {
		gen, _ := os.ReadFile(target)
		t.Fatalf("生成的配置无法通过校验（这正是平台默认值没改写的症状）: %v\n生成内容:\n%s", err, gen)
	}

	want := DefaultStorageRoot(runtime.GOOS)
	roots := loaded.Raw.Storage.EffectiveRoots()
	if len(roots) != 1 || !strings.EqualFold(roots[0], want) {
		t.Fatalf("存储种子根 = %v，期望 [%s]", roots, want)
	}

	// 再跑一次：已存在时绝不覆盖（里面会有自动生成的 master_key）。
	if created, err := EnsureFromExample(target); err != nil || created {
		t.Fatalf("目标已存在时应返回 created=false, err=nil，得到 created=%v err=%v", created, err)
	}
}
