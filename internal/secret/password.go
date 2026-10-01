package secret

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id 参数（见 docs/implementation.md 9.1：管理页口令使用 argon2id）。
const (
	argonMemoryKiB   = 64 * 1024 // 64 MiB
	argonIterations  = 3
	argonParallelism = 4
	argonSaltLen     = 16
	argonKeyLen      = 32
)

// 解析口令哈希时的合理上限，避免库中被写入畸形参数导致内存耗尽（属于外部输入的防御性校验）。
const (
	argonMaxMemoryKiB   = 4 * 1024 * 1024 // 4 GiB
	argonMaxIterations  = 64
	argonMaxParallelism = 64
)

// HashPassword 用 argon2id 生成 PHC 格式字符串，形如：
//
//	$argon2id$v=19$m=65536,t=3,p=4$<salt-b64>$<hash-b64>
//
// salt 为 16 字节随机数，输出 32 字节，salt 与 hash 均用 base64（无 padding）编码。
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("secret: 生成口令盐失败: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonIterations, argonMemoryKiB, argonParallelism, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		argonMemoryKiB, argonIterations, argonParallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword 校验口令。哈希格式非法返回错误；不匹配返回 (false, nil)。
// 使用 subtle.ConstantTimeCompare 做常数时间比较，防时序攻击。
func VerifyPassword(encodedHash, password string) (bool, error) {
	params, salt, want, err := parsePasswordHash(encodedHash)
	if err != nil {
		return false, err
	}

	got := argon2.IDKey([]byte(password), salt, params.iterations, params.memory, params.parallelism, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// argonParams 是从 PHC 字符串中解析出的开销参数。
type argonParams struct {
	memory      uint32
	iterations  uint32
	parallelism uint8
}

// parsePasswordHash 解析 PHC 格式的 argon2id 哈希，格式非法时返回错误。
func parsePasswordHash(encodedHash string) (argonParams, []byte, []byte, error) {
	var zero argonParams

	// PHC 格式固定为 6 段（首段为空）：["", "argon2id", "v=19", "m=..,t=..,p=..", salt, hash]
	parts := strings.Split(strings.TrimSpace(encodedHash), "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return zero, nil, nil, errors.New("secret: 口令哈希格式非法")
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return zero, nil, nil, fmt.Errorf("secret: 口令哈希版本段非法: %w", err)
	}
	if version != argon2.Version {
		return zero, nil, nil, fmt.Errorf("secret: 不支持的口令哈希版本 %d", version)
	}

	var memory, iterations, parallelism uint32
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &parallelism); err != nil {
		return zero, nil, nil, fmt.Errorf("secret: 口令哈希参数段非法: %w", err)
	}
	if memory == 0 || iterations == 0 || parallelism == 0 ||
		memory > argonMaxMemoryKiB || iterations > argonMaxIterations || parallelism > argonMaxParallelism {
		return zero, nil, nil, errors.New("secret: 口令哈希参数超出合法范围")
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return zero, nil, nil, errors.New("secret: 口令哈希盐值非法")
	}
	hash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(hash) == 0 {
		return zero, nil, nil, errors.New("secret: 口令哈希摘要非法")
	}

	return argonParams{memory: memory, iterations: iterations, parallelism: uint8(parallelism)}, salt, hash, nil
}
