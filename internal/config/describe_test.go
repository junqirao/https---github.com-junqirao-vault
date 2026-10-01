package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDescribeFieldsBindToStruct 全量配置表的每一项都必须**真的绑定到 Config 字段**。
//
// 为什么必须有这条测试：读写走反射，写错字段名的代价是"界面上这一项永远读不到值、
// 改了也不生效"，而且不会报错 —— 只有这种逐项校验能拦住。
func TestDescribeFieldsBindToStruct(t *testing.T) {
	c := &Config{}
	seen := make(map[string]bool, 64)
	for _, def := range DescribeFields() {
		if seen[def.Key] {
			t.Fatalf("配置项 %s 重复定义", def.Key)
		}
		seen[def.Key] = true
		if def.Section == "" || def.Desc == "" {
			t.Fatalf("配置项 %s 缺少 section 或 desc（界面要展示）", def.Key)
		}
		v, ok := locate(c, def.path)
		if !ok {
			t.Fatalf("配置项 %s 未绑定到 Config 字段：%+v", def.Key, def.path)
		}
		// 敏感项与不可编辑项也不能例外：至少要能读（脱敏展示"有没有配"）。
		if def.Kind != KindSecret && !def.Editable && def.EditableReason == "" {
			t.Fatalf("配置项 %s 不可编辑却没给原因（界面要置灰并说明）", def.Key)
		}
		_ = v
	}
	// 覆盖面上：至少覆盖全部 9 个配置段。
	for _, section := range []string{"server", "http", "database", "storage", "platform", "client_compat", "log", "security", "update"} {
		found := false
		for _, def := range DescribeFields() {
			if def.Section == section {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("配置段 %s 在全量配置表里缺失", section)
		}
	}
}

// TestFieldSetRoundTrip 各类型的值必须能"字符串写入 → 读回一致"（界面按字符串提交）。
func TestFieldSetRoundTrip(t *testing.T) {
	c := &Config{}

	cases := []struct {
		key  string
		raw  string
		want string
	}{
		{"http.listen", "0.0.0.0:9443", "0.0.0.0:9443"},
		{"http.tls.enabled", "true", "true"},
		{"database.mysql.max_open_conns", "30", "30"},
		{"storage.default_max_diff_disks", "0", "0"},
		{"storage.size_granularity_mb", "1", "1"},
		{"security.lease_ttl", "180s", "3m0s"},
		{"security.bootstrap_window", "", "0s"},
		{"security.lease_heartbeat_interval", "45s", "45s"},
		{"platform.lvm.watermark_percent", "85", "85"},
		{"platform.iscsi.portals", "192.168.1.10:3260, 10.0.0.5:3260", "192.168.1.10:3260, 10.0.0.5:3260"},
		{"storage.source_roots", "D:\\Games，E:\\Steam", "D:\\Games, E:\\Steam"},
		{"client_compat.enabled", "false", "false"},
		{"client_compat.min", "", ""},
		{"update.channel", "beta", "beta"},
		{"log.level", "debug", "debug"},
	}
	for _, c2 := range cases {
		def, ok := FindField(c2.key)
		if !ok {
			t.Fatalf("找不到配置项 %s", c2.key)
		}
		if err := def.Set(c, c2.raw); err != nil {
			t.Fatalf("写入 %s = %q 失败：%v", c2.key, c2.raw, err)
		}
		if got := def.String(c); got != c2.want {
			t.Fatalf("%s 读回 = %q，期望 %q", c2.key, got, c2.want)
		}
	}

	// 布尔兼容常见写法。
	def, _ := FindField("http.tls.enabled")
	if err := def.Set(c, "yes"); err != nil {
		t.Fatalf("yes 应被解析为 true：%v", err)
	}
	if got := def.String(c); got != "true" {
		t.Fatalf("yes → %q，期望 true", got)
	}

	// 非法值必须报错（不能静默写坏配置）。
	if err := def.Set(c, "maybe"); err == nil {
		t.Fatal("非法布尔值应报错")
	}
	dur, _ := FindField("security.lease_ttl")
	if err := dur.Set(c, "10 分钟"); err == nil {
		t.Fatal("非法时长应报错")
	}
}

// TestSensitiveFieldsNotEditable 敏感项绝不接受页面修改，且值只以脱敏形式返回。
func TestSensitiveFieldsNotEditable(t *testing.T) {
	c := &Config{}
	c.Security.MasterKey = "super-secret-key"
	c.Database.MySQL.DSN = "user:pass@tcp(127.0.0.1:3306)/vault"

	for _, key := range []string{"security.master_key", "database.mysql.dsn"} {
		def, ok := FindField(key)
		if !ok {
			t.Fatalf("找不到配置项 %s", key)
		}
		if def.String(c) != maskedValue {
			t.Fatalf("%s 的值必须脱敏，实际 %q", key, def.String(c))
		}
		if err := def.Set(c, "new-value"); err == nil {
			t.Fatalf("%s 不允许在线修改", key)
		}
	}
}

// TestNotEditableFields 锁住"必须置灰"的项：改了会出事、或有别的真源、或改了也不生效。
func TestNotEditableFields(t *testing.T) {
	notEditable := map[string]bool{
		"server.instance_id":          true, // 换值会让客户端证书失效
		"server.name":                 true, // 有运行期覆盖（数据库优先）
		"security.master_key":         true, // 丢失即无法解密 CHAP 密钥
		"database.mysql.dsn":          true, // 含数据库口令
		"storage.whitelist_root":      true, // 真源是 storages 表
		"storage.whitelist_roots":     true,
		"storage.size_granularity_gb": true, // 已废弃
		"platform.kind":               true, // 由构建产物决定
		"platform.iscsi.backend":      true,
		"platform.iscsi.iqn_prefix":   true, // 改了会让已发布 IQN 失配
	}
	for key := range notEditable {
		def, ok := FindField(key)
		if !ok {
			t.Fatalf("找不到配置项 %s", key)
		}
		if def.Editable {
			t.Fatalf("%s 必须置灰（不可在页面编辑）", key)
		}
	}
}

// TestSaveWritesFileAndBackup 保存必须落盘并留下备份（写回会丢失注释，备份是回滚手段）。
func TestSaveWritesFileAndBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	c := &Config{}
	applyTestDefaults(c, path, dir)

	if err := Save(path, c); err != nil {
		t.Fatalf("保存失败：%v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("配置文件未落盘：%v", err)
	}
	// 第二次保存：必须产生 .bak（备份的是上一版）。
	if err := Save(path, c); err != nil {
		t.Fatalf("二次保存失败：%v", err)
	}
	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Fatalf("缺少备份文件：%v", err)
	}
	// 空路径必须报错（不能把配置写到莫名其妙的地方）。
	if err := Save("  ", c); err == nil {
		t.Fatal("空路径应报错")
	}
}

// applyTestDefaults 填一份能通过校验的最小配置（避免测试依赖 applyDefaults 的完整环境）。
func applyTestDefaults(c *Config, dbPath, storageRoot string) {
	c.Server.InstanceID = "srv-test"
	c.HTTP.Listen = "127.0.0.1:8443"
	c.Database.Driver = "sqlite"
	c.Database.SQLite.Path = dbPath
	// 校验要求存储根非空且为绝对路径（validate 的实现见 config.go）。
	c.Storage.WhitelistRoots = []string{storageRoot}
	c.Platform.Kind = "auto"
	c.Log.Level = "info"
	c.Security.MasterKey = "0123456789abcdef0123456789abcdef"
	c.Update.Channel = "stable"
	c.Security.SessionTTL = Duration(12 * time.Hour)
}
