// Package cert 实现「每服务端一套独立自建 CA」的证书体系（见 docs/implementation.md 9.1 / 9.2 / 13.5-ⓕ）。
//
// 核心约束：
//   - 每台服务端一套独立 CA，彼此互不信任；
//   - 客户端证书写入自定义扩展（服务端实例 ID、用户 ID），服务端侧据此校验，
//     扩展**用于校验、不用于筛选**（筛选靠客户端按服务端分目录存储）；
//   - 身份绑定基于 SPKI（公钥）指纹而非证书指纹：续期时保留密钥对 → SPKI 不变 → 免重新审批。
package cert

import (
	"crypto/x509/pkix"
	"encoding/asn1"
	"fmt"
	"unicode/utf8"
)

// 自定义扩展使用私有 OID。
//
// 1.3.6.1.4.1 是 IANA 的 private enterprise 分支，其下号段需注册 PEN 后才算正式分配。
// 这里选用与 Sigstore 相同结构的私有分支作为**占位**，仅用于本系统内部自建 CA 签发的证书
// （这些证书只在 Vault 自己的信任链内校验，不经过公共 CA/第三方校验器，因此碰撞无实际影响）。
//
// ⚠️ 上线前需确认（需实测确认）：若本组织已注册 PEN，请把下面两个 OID 替换为该 PEN 下的子 OID。
//
// 两个字段各用独立 OID 承载（不用复合结构），便于单独读写与排障。
var (
	// oidServerInstanceID 承载 server_instance_id（服务端实例 ID），服务端侧据此拒绝非本机证书。
	oidServerInstanceID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 1}
	// oidUserID 承载 user_id（证书绑定的用户 ID）。
	oidUserID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 2}
)

// maxExtensionValueLen 是自定义扩展值的长度上限（实例 ID / 用户 ID 都是短字符串）。
const maxExtensionValueLen = 256

// buildExtensions 构造自定义扩展列表，供 x509.CreateCertificate 的 ExtraExtensions 使用。
//
// 扩展值直接以 UTF-8 字节编码（不做额外 ASN.1 包装）；空值不写入扩展。
// 一律不设 Critical —— 否则不认识该扩展的验证方会直接拒绝证书（见实现要点 5）。
func buildExtensions(instanceID, userID string) []pkix.Extension {
	exts := make([]pkix.Extension, 0, 2)
	if instanceID != "" {
		exts = append(exts, pkix.Extension{Id: oidServerInstanceID, Value: []byte(instanceID)})
	}
	if userID != "" {
		exts = append(exts, pkix.Extension{Id: oidUserID, Value: []byte(userID)})
	}
	return exts
}

// extensionString 从证书扩展中读取指定 OID 的 UTF-8 字符串值。
// 扩展不存在时返回 ("", nil)；存在但长度超限或不是合法 UTF-8 时返回错误（而不是 panic）。
func extensionString(exts []pkix.Extension, oid asn1.ObjectIdentifier) (string, error) {
	for i := range exts {
		if !exts[i].Id.Equal(oid) {
			continue
		}
		value := exts[i].Value
		if len(value) == 0 {
			return "", nil
		}
		if len(value) > maxExtensionValueLen {
			return "", fmt.Errorf("cert: 扩展 %s 的值过长（%d 字节，上限 %d）", oid.String(), len(value), maxExtensionValueLen)
		}
		if !utf8.Valid(value) {
			return "", fmt.Errorf("cert: 扩展 %s 的值不是合法的 UTF-8", oid.String())
		}
		return string(value), nil
	}
	return "", nil
}
