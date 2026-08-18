package service

import (
	"context"
	"errors"
	"hash/fnv"
	"strconv"
	"strings"
	"time"
)

// happyShrimpRefreshSkew 提前刷新窗口：access token 剩余不足该值时即刷新。
// 快乐虾米 JWT 有效期 7 天，提前 6 小时刷新足够安全且能分摊后台刷新负载。
const happyShrimpRefreshSkew = 6 * time.Hour

// happyShrimpRefreshJitterMax 刷新抖动上限：同批导入的账号避免同一周期一起刷新。
const happyShrimpRefreshJitterMax = 30 * time.Minute

// happyShrimpRefreshSkewMin 抖动后的窗口下限。
const happyShrimpRefreshSkewMin = 1 * time.Hour

// HappyShrimpTokenRefresher 实现快乐虾米 token 刷新策略。
type HappyShrimpTokenRefresher struct {
	oauthService *HappyShrimpOAuthService
}

// NewHappyShrimpTokenRefresher 创建刷新器。
func NewHappyShrimpTokenRefresher(oauthService *HappyShrimpOAuthService) *HappyShrimpTokenRefresher {
	return &HappyShrimpTokenRefresher{oauthService: oauthService}
}

// CacheKey 用于分布式刷新锁。
func (r *HappyShrimpTokenRefresher) CacheKey(account *Account) string {
	return HappyShrimpTokenCacheKey(account)
}

// CanRefresh 报告账号是否具备刷新条件（平台 + 类型 + refresh_token 存在）。
func (r *HappyShrimpTokenRefresher) CanRefresh(account *Account) bool {
	return account != nil && account.IsHappyShrimpOAuth() &&
		strings.TrimSpace(account.GetHappyShrimpRefreshToken()) != ""
}

// NeedsRefresh 报告是否需要刷新（access 缺失 / expires_at 缺失 / 即将过期）。
func (r *HappyShrimpTokenRefresher) NeedsRefresh(account *Account, refreshWindow time.Duration) bool {
	if account == nil || strings.TrimSpace(account.GetHappyShrimpRefreshToken()) == "" {
		return false
	}
	if strings.TrimSpace(account.GetHappyShrimpAccessToken()) == "" {
		return true
	}
	expiresAt := account.GetCredentialAsTime("expires_at")
	if expiresAt == nil {
		return true
	}
	if refreshWindow < happyShrimpRefreshSkew {
		refreshWindow = happyShrimpRefreshSkew
	}
	refreshWindow = happyShrimpRefreshWindowWithJitter(account.ID, refreshWindow)
	return time.Until(*expiresAt) < refreshWindow
}

// happyShrimpRefreshWindowWithJitter 基于 accountID 确定性抖动刷新窗口，
// 避免同批导入账号在同一刷新周期内同时请求（与 Grok 策略一致）。
func happyShrimpRefreshWindowWithJitter(accountID int64, refreshWindow time.Duration) time.Duration {
	if accountID <= 0 || refreshWindow <= happyShrimpRefreshSkewMin {
		return refreshWindow
	}
	h := fnv.New32a()
	var b [8]byte
	id := uint64(accountID)
	for i := 0; i < 8; i++ {
		b[i] = byte(id >> (8 * i))
	}
	_, _ = h.Write(b[:])
	jitter := time.Duration(h.Sum32()%uint32(happyShrimpRefreshJitterMax/time.Second)) * time.Second
	out := refreshWindow - jitter
	if out < happyShrimpRefreshSkewMin {
		return happyShrimpRefreshSkewMin
	}
	return out
}

// Refresh 执行刷新并返回更新后的 credentials（保留原 map 字段）。
func (r *HappyShrimpTokenRefresher) Refresh(ctx context.Context, account *Account) (map[string]any, error) {
	if r == nil || r.oauthService == nil {
		return nil, errors.New("happy shrimp oauth service is not configured")
	}
	tokenInfo, err := r.oauthService.RefreshAccountToken(ctx, account)
	if err != nil {
		return nil, err
	}
	newCredentials := r.oauthService.BuildAccountCredentials(tokenInfo)
	newCredentials = MergeCredentials(account.Credentials, newCredentials)
	if baseURL := strings.TrimSpace(account.GetCredential("base_url")); baseURL != "" {
		newCredentials["base_url"] = baseURL
	}
	return newCredentials, nil
}

// HappyShrimpTokenCacheKey 返回 token 缓存 key。
func HappyShrimpTokenCacheKey(account *Account) string {
	if account == nil {
		return "happy_shrimp:account:0"
	}
	return "happy_shrimp:account:" + strconv.FormatInt(account.ID, 10)
}
