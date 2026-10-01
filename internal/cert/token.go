package cert

import (
	"fmt"
	"sync"
	"time"

	"vault/internal/secret"
)

// defaultEnrollmentTTL 是 enrollment 令牌的默认有效期（足够用户完成一次签发流程）。
const defaultEnrollmentTTL = 15 * time.Minute

// TokenStore 管理 enrollment 一次性令牌（内存实现即可）。
//
// 令牌只在签发流程中短暂使用：管理页生成 → 客户端凭令牌申请证书。
// 存于内存意味着服务端重启后所有未消费令牌失效（需重新生成），这与一次性语义一致。
type TokenStore struct {
	ttl time.Duration

	mu     sync.Mutex
	tokens map[string]tokenEntry
}

// tokenEntry 是令牌绑定的信息。
type tokenEntry struct {
	userID     string
	instanceID string
	expiresAt  time.Time
}

// NewTokenStore 构造令牌仓库；ttl <= 0 时使用默认值 15 分钟。
func NewTokenStore(ttl time.Duration) *TokenStore {
	if ttl <= 0 {
		ttl = defaultEnrollmentTTL
	}
	return &TokenStore{
		ttl:    ttl,
		tokens: make(map[string]tokenEntry),
	}
}

// Issue 为用户签发一次性令牌。
func (t *TokenStore) Issue(userID, instanceID string) (token string, expiresAt time.Time, err error) {
	token, err = secret.RandomToken(32)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("cert: 生成 enrollment 令牌失败: %w", err)
	}
	expiresAt = time.Now().Add(t.ttl)

	t.mu.Lock()
	defer t.mu.Unlock()
	// 顺带清理，避免长期运行下 map 无限增长（Cleanup 仍可被定时任务显式调用）。
	t.cleanupLocked(time.Now())
	t.tokens[token] = tokenEntry{userID: userID, instanceID: instanceID, expiresAt: expiresAt}
	return token, expiresAt, nil
}

// Consume 消费令牌；成功返回绑定的 userID 与 instanceID，失败返回 ("", "", false)。
// 必须先删除再返回，保证同一令牌原子地只能成功消费一次。
func (t *TokenStore) Consume(token string) (userID, instanceID string, ok bool) {
	if token == "" {
		return "", "", false
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	entry, exists := t.tokens[token]
	if !exists {
		return "", "", false
	}
	delete(t.tokens, token)
	if !time.Now().Before(entry.expiresAt) {
		return "", "", false
	}
	return entry.userID, entry.instanceID, true
}

// Cleanup 清理过期令牌，返回清理数量。
func (t *TokenStore) Cleanup() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cleanupLocked(time.Now())
}

// cleanupLocked 删除已过期令牌，返回删除数量（调用方需持有锁）。
func (t *TokenStore) cleanupLocked(now time.Time) int {
	removed := 0
	for token, entry := range t.tokens {
		if !now.Before(entry.expiresAt) {
			delete(t.tokens, token)
			removed++
		}
	}
	return removed
}
