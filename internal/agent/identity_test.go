package agent

import (
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubFileRestriction 让落盘文件不做 ACL 收紧，测试结束后还原。
//
// 身份文件写完会被收紧为"仅 SYSTEM 与 Administrators"，**只有提权进程读得回来**；
// 测试关注的是多服务端身份的存取逻辑，不是 ACL 本身（生产里客户端以管理员权限运行），
// 因此在非提权环境（例如开发机上直接 go test）换成空实现，避免测试因权限而失真。
func stubFileRestriction(t *testing.T) {
	t.Helper()
	original := restrictFileToAdmins
	restrictFileToAdmins = func(string) error { return nil }
	t.Cleanup(func() { restrictFileToAdmins = original })
}

// testIdentity 生成一套可落盘的身份（自签证书 + 匹配的 ECDSA 私钥）。
//
// 只借用生产代码的私钥生成与校验（validateIdentity 要求证书与私钥公钥一致），
// 证书本身自签即可 —— 这里的重点是"身份归属于哪个服务端"，与证书链无关。
func testIdentity(t *testing.T, serverURL, instanceID string) *Identity {
	t.Helper()
	keyPEM, _, err := generateClientKeyAndCSR("tester")
	if err != nil {
		t.Fatalf("生成测试私钥失败：%v", err)
	}
	key, err := parseECPrivateKeyPEM(keyPEM)
	if err != nil {
		t.Fatalf("解析测试私钥失败：%v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "tester"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("签发测试证书失败：%v", err)
	}
	return &Identity{
		ServerURL:        serverURL,
		ServerInstanceID: instanceID,
		ServerCertSHA256: strings.Repeat("ab", 32),
		UserID:           "u1",
		Username:         "tester",
		CertPEM:          string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		KeyPEM:           keyPEM,
	}
}

// TestIdentityStoreKeepsOneIdentityPerServer 覆盖本次修复的核心：
// 给 B 服务端装证书**不能**顶掉已装好的 A 服务端证书。
//
// 真实问题：此前的身份文件只放得下一个身份，用户配了多个服务端时，
// 后装的会把先装的覆盖掉，于是切换服务端后免密登录失效（"没支持装多个服务端的证书"）。
func TestIdentityStoreKeepsOneIdentityPerServer(t *testing.T) {
	stubFileRestriction(t)
	path := filepath.Join(t.TempDir(), "identity.json")
	store := newIdentityStore(path, testLogger())

	a := testIdentity(t, "https://10.0.0.1:8443", "srv-a")
	b := testIdentity(t, "https://10.0.0.2:8443", "srv-b")
	if err := store.Install(a); err != nil {
		t.Fatalf("安装 A 服务端身份失败：%v", err)
	}
	if err := store.Install(b); err != nil {
		t.Fatalf("安装 B 服务端身份失败：%v", err)
	}
	if got := store.Len(); got != 2 {
		t.Fatalf("两个服务端应各留一份身份，实际 %d 份", got)
	}

	// 重新加载（等价代理重启）：两份身份都必须还在，且都能按服务端定位到。
	reloaded := newIdentityStore(path, testLogger())
	if got := reloaded.Len(); got != 2 {
		t.Fatalf("重启后应仍有 2 份身份，实际 %d 份", got)
	}
	for _, want := range []*Identity{a, b} {
		got, ok := reloaded.GetForServer(want.ServerInstanceID, want.ServerURL)
		if !ok {
			t.Fatalf("按实例 ID %q 应能定位到身份", want.ServerInstanceID)
		}
		if got.ServerURL != want.ServerURL {
			t.Fatalf("定位到的身份应属于 %q，实际 %q", want.ServerURL, got.ServerURL)
		}
	}
	// 最近安装的那份是"活动"身份：未指定服务端时用它。
	if id, ok := reloaded.Get(); !ok || id.ServerInstanceID != "srv-b" {
		t.Fatalf("活动身份应是最新安装的 srv-b，实际 %+v ok=%v", id.ServerInstanceID, ok)
	}
	if key := reloaded.ActiveKey(); key != "srv-b" {
		t.Fatalf("活动身份键应为 srv-b，实际 %q", key)
	}
}

// TestIdentityStoreMigratesV1File 覆盖老版本（单身份）文件的平滑升级。
//
// 老用户的 identity.json 里只有一个扁平的身份对象；升级后读它必须照常免密登录，
// 并且在装第二个服务端时改写为多身份结构，而不是把第一份丢掉。
func TestIdentityStoreMigratesV1File(t *testing.T) {
	stubFileRestriction(t)
	path := filepath.Join(t.TempDir(), "identity.json")
	a := testIdentity(t, "https://10.0.0.1:8443", "srv-a")
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("序列化 v1 身份失败：%v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("写入 v1 身份文件失败：%v", err)
	}

	store := newIdentityStore(path, testLogger())
	if _, ok := store.GetForServer("", a.ServerURL); !ok {
		t.Fatal("老版本身份文件升级后必须依然可用（否则用户会突然无法免密登录）")
	}
	if key := store.ActiveKey(); key != "srv-a" {
		t.Fatalf("只有一份身份时应自动成为活动身份，实际 %q", key)
	}

	b := testIdentity(t, "https://10.0.0.2:8443", "srv-b")
	if err := store.Install(b); err != nil {
		t.Fatalf("在 v1 文件上安装第二个服务端身份失败：%v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取升级后的身份文件失败：%v", err)
	}
	var persisted persistedIdentities
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("升级后的身份文件应为 v2 结构：%v", err)
	}
	if persisted.Version != identityFileVersion || len(persisted.Identities) != 2 {
		t.Fatalf("升级后应写回 v%d 且含 2 份身份，实际 version=%d 数量=%d",
			identityFileVersion, persisted.Version, len(persisted.Identities))
	}
	if strings.Contains(string(raw), `"installed"`) {
		t.Fatal("落盘结构里不该出现对外元数据字段（避免混淆元数据与私钥材料）")
	}
	if got := newIdentityStore(path, testLogger()).Len(); got != 2 {
		t.Fatalf("重新加载后应有 2 份身份，实际 %d 份", got)
	}
}

// TestIdentityStoreFindByServer 覆盖"按服务端定位身份"的匹配规则。
func TestIdentityStoreFindByServer(t *testing.T) {
	stubFileRestriction(t)
	path := filepath.Join(t.TempDir(), "identity.json")
	store := newIdentityStore(path, testLogger())
	a := testIdentity(t, "https://10.0.0.1:8443", "srv-a")
	b := testIdentity(t, "https://10.0.0.2:8443", "srv-b")
	for _, id := range []*Identity{a, b} {
		if err := store.Install(id); err != nil {
			t.Fatalf("安装身份失败：%v", err)
		}
	}

	cases := []struct {
		name       string
		instanceID string
		serverURL  string
		want       string
		wantOK     bool
	}{
		{"实例 ID 命中", "srv-a", "", "srv-a", true},
		{"实例 ID 命中（忽略大小写与空白）", " SRV-A ", "", "srv-a", true},
		{"地址命中（忽略大小写与末尾斜杠）", "", "HTTPS://10.0.0.1:8443/", "srv-a", true},
		{"实例 ID 与地址都给时以实例 ID 为准", "srv-b", "https://10.0.0.1:8443", "srv-b", true},
		{"别的服务端不得误用本机证书", "srv-c", "https://10.0.0.3:8443", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, ok := store.GetForServer(tc.instanceID, tc.serverURL)
			if ok != tc.wantOK {
				t.Fatalf("ok 应为 %v，实际 %v", tc.wantOK, ok)
			}
			if ok && id.ServerInstanceID != tc.want {
				t.Fatalf("应定位到 %q，实际 %q", tc.want, id.ServerInstanceID)
			}
		})
	}
}

// TestResolveIdentityFallsBackToSingleIdentity 覆盖连接路径上的兼容兜底：
// 只有一份身份时接受"地址对不上"的查询（老版本只能装一份，地址/实例 ID 可能变过），
// 而一旦装了多份，就必须严格按服务端匹配 —— 否则又会拿错证书。
func TestResolveIdentityFallsBackToSingleIdentity(t *testing.T) {
	stubFileRestriction(t)
	store, err := NewStateStore(filepath.Join(t.TempDir(), "state.json"), testLogger())
	if err != nil {
		t.Fatalf("构造状态存储失败：%v", err)
	}
	agent := &Agent{logger: testLogger(), store: store, hub: NewEventHub()}

	agent.identity = newIdentityStore(filepath.Join(t.TempDir(), "identity.json"), testLogger())
	if err := agent.identity.Install(testIdentity(t, "https://10.0.0.1:8443", "srv-a")); err != nil {
		t.Fatalf("安装身份失败：%v", err)
	}
	if _, ok := agent.resolveIdentity("srv-changed", "https://10.0.0.9:8443"); !ok {
		t.Fatal("只有一份身份时应兜底接受（老客户端升级后不该失去免密登录能力）")
	}

	// 装第二份后：地址对不上就明确"未安装"，绝不猜。
	if err := agent.identity.Install(testIdentity(t, "https://10.0.0.2:8443", "srv-b")); err != nil {
		t.Fatalf("安装第二份身份失败：%v", err)
	}
	if _, ok := agent.resolveIdentity("srv-c", "https://10.0.0.3:8443"); ok {
		t.Fatal("已装多份身份时必须严格匹配，不能拿别的服务端的证书去连")
	}
	if id, ok := agent.resolveIdentity("srv-a", "https://10.0.0.1:8443"); !ok || id.ServerInstanceID != "srv-a" {
		t.Fatalf("应定位到 srv-a，实际 %q ok=%v", id.ServerInstanceID, ok)
	}
}

// TestIdentityStoreRemoveOnlyTargetServer 覆盖"撤销一个服务端的证书不影响其他服务端"。
func TestIdentityStoreRemoveOnlyTargetServer(t *testing.T) {
	stubFileRestriction(t)
	path := filepath.Join(t.TempDir(), "identity.json")
	store := newIdentityStore(path, testLogger())
	a := testIdentity(t, "https://10.0.0.1:8443", "srv-a")
	b := testIdentity(t, "https://10.0.0.2:8443", "srv-b")
	for _, id := range []*Identity{a, b} {
		if err := store.Install(id); err != nil {
			t.Fatalf("安装身份失败：%v", err)
		}
	}

	if err := store.Remove("", "https://10.0.0.3:8443"); err != nil {
		t.Fatalf("删除未安装的服务端身份应幂等成功：%v", err)
	}
	if store.Len() != 2 {
		t.Fatal("删除未命中的服务端不该动到任何身份")
	}

	if err := store.Remove("srv-a", ""); err != nil {
		t.Fatalf("删除 A 服务端身份失败：%v", err)
	}
	if _, ok := store.GetForServer("srv-a", ""); ok {
		t.Fatal("A 服务端身份应已被删除")
	}
	if _, ok := store.GetForServer("srv-b", ""); !ok {
		t.Fatal("删除 A 的证书不该影响 B 的证书")
	}
	if key := store.ActiveKey(); key != "srv-b" {
		t.Fatalf("活动身份应自动落到仅存的 srv-b，实际 %q", key)
	}

	if err := store.RemoveAll(); err != nil {
		t.Fatalf("清空身份失败：%v", err)
	}
	if store.Len() != 0 || store.ActiveKey() != "" {
		t.Fatalf("清空后应无身份，实际 %d 份 / 活动键 %q", store.Len(), store.ActiveKey())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("清空后身份文件应被删除，实际 err=%v", err)
	}
}

// TestIdentityStoreDropsUnattributableIdentity 覆盖"没有服务端标识的身份不入册"：
// 这类身份无法归属到任何服务端，留着只会在下次启动被静默丢弃，不如当场拒绝。
func TestIdentityStoreDropsUnattributableIdentity(t *testing.T) {
	stubFileRestriction(t)
	path := filepath.Join(t.TempDir(), "identity.json")
	store := newIdentityStore(path, testLogger())
	if err := store.Install(testIdentity(t, "", "")); err == nil {
		t.Fatal("既无实例 ID 又无地址的身份应被拒绝")
	}
	if store.Len() != 0 {
		t.Fatalf("被拒绝的身份不应入册，实际 %d 份", store.Len())
	}
}

// TestPinForIdentityRequiresSameServer 覆盖指纹兜底的范围：
// 身份自带指纹时直接用；否则只接受**同一服务端**的会话指纹（跨服务端复用会握手失败）。
func TestPinForIdentityRequiresSameServer(t *testing.T) {
	stubFileRestriction(t)
	store, err := NewStateStore(filepath.Join(t.TempDir(), "state.json"), testLogger())
	if err != nil {
		t.Fatalf("构造状态存储失败：%v", err)
	}
	agent := &Agent{logger: testLogger(), store: store, hub: NewEventHub()}

	pin := strings.Repeat("cd", 32)
	store.SetSession("", &Session{
		ServerURL:        "https://10.0.0.2:8443",
		ServerInstanceID: "srv-b",
		CertSHA256:       pin,
	})

	own := testIdentity(t, "https://10.0.0.1:8443", "srv-a")
	own.ServerCertSHA256 = ""
	if got := agent.pinForIdentity(*own); got != "" {
		t.Fatalf("别的服务端的会话指纹不得复用，实际 %q", got)
	}

	same := *own
	same.ServerInstanceID = "srv-b"
	same.ServerURL = "https://10.0.0.2:8443"
	if got := agent.pinForIdentity(same); got != pin {
		t.Fatalf("同一服务端应退回其会话指纹 %q，实际 %q", pin, got)
	}

	withPin := *own
	withPin.ServerCertSHA256 = strings.Repeat("ef", 32)
	if got := agent.pinForIdentity(withPin); got != withPin.ServerCertSHA256 {
		t.Fatalf("身份自带指纹应优先，实际 %q", got)
	}
}
