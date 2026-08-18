package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"
)

const (
	// happyShrimpRequestRefreshTimeout 请求路径 token 刷新最大等待时间。
	happyShrimpRequestRefreshTimeout = 8 * time.Second
	// happyShrimpTokenCacheSkew 缓存 TTL 扣减量。
	happyShrimpTokenCacheSkew = 5 * time.Minute
)

// HappyShrimpTokenCache token 缓存接口（复用 Gemini 缓存）。
type HappyShrimpTokenCache = GeminiTokenCache

// HappyShrimpTokenProvider 管理快乐虾米账号的 access token。
// 请求路径：优先读缓存；快过期时通过统一 OAuthRefreshAPI 同步刷新
// （与后台 TokenRefreshService 共享分布式锁，避免竞争）。
type HappyShrimpTokenProvider struct {
	accountRepo   AccountRepository
	tokenCache    HappyShrimpTokenCache
	refreshAPI    *OAuthRefreshAPI
	executor      OAuthRefreshExecutor
	refreshPolicy ProviderRefreshPolicy
}

// NewHappyShrimpTokenProvider 创建 token provider。
func NewHappyShrimpTokenProvider(
	accountRepo AccountRepository,
	tokenCache HappyShrimpTokenCache,
) *HappyShrimpTokenProvider {
	return &HappyShrimpTokenProvider{
		accountRepo:   accountRepo,
		tokenCache:    tokenCache,
		refreshPolicy: HappyShrimpProviderRefreshPolicy(),
	}
}

// SetRefreshAPI 注入统一刷新 API 与执行器。
func (p *HappyShrimpTokenProvider) SetRefreshAPI(api *OAuthRefreshAPI, executor OAuthRefreshExecutor) {
	p.refreshAPI = api
	p.executor = executor
}

// SetRefreshPolicy 注入调用方刷新策略。
func (p *HappyShrimpTokenProvider) SetRefreshPolicy(policy ProviderRefreshPolicy) {
	p.refreshPolicy = policy
}

// GetAccessToken 返回有效的 access token。
func (p *HappyShrimpTokenProvider) GetAccessToken(ctx context.Context, account *Account) (string, error) {
	if account == nil {
		return "", errors.New("account is nil")
	}
	if !account.IsHappyShrimpOAuth() {
		return "", errors.New("not a happy shrimp oauth account")
	}
	if strings.TrimSpace(account.GetHappyShrimpRefreshToken()) == "" {
		return "", errors.New("happy shrimp refresh_token is missing")
	}

	cacheKey := HappyShrimpTokenCacheKey(account)

	// 1) 先读缓存（与账号凭据一致且未过期时直接命中）。
	expiresAt := account.GetCredentialAsTime("expires_at")
	accountAccessToken := strings.TrimSpace(account.GetHappyShrimpAccessToken())
	if p.tokenCache != nil {
		if token, err := p.tokenCache.GetAccessToken(ctx, cacheKey); err == nil {
			cachedToken := strings.TrimSpace(token)
			if cachedToken != "" && cachedToken == accountAccessToken &&
				expiresAt != nil && time.Until(*expiresAt) > happyShrimpTokenCacheSkew {
				return cachedToken, nil
			}
		}
	}

	// 2) 快过期/缺失 → 请求路径同步刷新（短超时，失败交给后台刷新器与 failover）。
	needsRefresh := accountAccessToken == "" || expiresAt == nil || time.Until(*expiresAt) <= happyShrimpRequestRefreshSkew()
	if needsRefresh {
		if p.refreshAPI == nil || p.executor == nil {
			return "", errors.New("happy shrimp oauth refresh is not configured")
		}
		refreshCtx, cancel := context.WithTimeout(ctx, happyShrimpRequestRefreshTimeout)
		defer cancel()
		result, err := p.refreshAPI.RefreshIfNeeded(withOAuthRefreshRequestPath(refreshCtx), account, p.executor, happyShrimpRequestRefreshSkew())
		if err != nil {
			if p.refreshPolicy.OnRefreshError == ProviderRefreshErrorReturn {
				return "", err
			}
		} else if result != nil && result.LockHeld {
			if p.refreshPolicy.OnLockHeld == ProviderLockHeldWaitForCache && p.tokenCache != nil {
				if token, cacheErr := p.tokenCache.GetAccessToken(ctx, cacheKey); cacheErr == nil && strings.TrimSpace(token) != "" {
					return token, nil
				}
			}
			if expiresAt == nil || !time.Now().Before(*expiresAt) {
				return "", errors.New("happy shrimp access token is expired")
			}
		} else if result != nil && result.Account != nil {
			account = result.Account
			expiresAt = account.GetCredentialAsTime("expires_at")
		}
	}

	accessToken := strings.TrimSpace(account.GetHappyShrimpAccessToken())
	if accessToken == "" {
		return "", errors.New("happy shrimp access_token is missing")
	}
	if expiresAt != nil && !time.Now().Before(*expiresAt) {
		return "", errors.New("happy shrimp access token is expired")
	}

	// 3) 回填缓存（TTL 按剩余有效期扣减）。
	if p.tokenCache != nil {
		latestAccount, isStale := CheckTokenVersion(ctx, account, p.accountRepo)
		if isStale && latestAccount != nil && latestAccount.IsHappyShrimpOAuth() {
			slog.Debug("happy_shrimp_token_version_stale_use_latest", "account_id", account.ID)
			accessToken = strings.TrimSpace(latestAccount.GetHappyShrimpAccessToken())
			if accessToken == "" {
				return "", errors.New("happy shrimp access_token not found after version check")
			}
		} else {
			ttl := 30 * time.Minute
			if expiresAt != nil {
				until := time.Until(*expiresAt)
				switch {
				case until > happyShrimpTokenCacheSkew:
					ttl = until - happyShrimpTokenCacheSkew
				case until > 0:
					ttl = until
				default:
					ttl = time.Minute
				}
			}
			_ = p.tokenCache.SetAccessToken(ctx, cacheKey, accessToken, ttl)
		}
	}

	return accessToken, nil
}

// InvalidateToken 清除缓存中的 token。
func (p *HappyShrimpTokenProvider) InvalidateToken(ctx context.Context, account *Account) error {
	if p == nil || p.tokenCache == nil || account == nil {
		return nil
	}
	return p.tokenCache.DeleteAccessToken(ctx, HappyShrimpTokenCacheKey(account))
}

// happyShrimpRequestRefreshSkew 请求路径提前刷新窗口（与后台刷新器一致）。
func happyShrimpRequestRefreshSkew() time.Duration {
	return happyShrimpRefreshSkew
}

// HappyShrimpProviderRefreshPolicy 返回快乐虾米请求路径刷新策略。
func HappyShrimpProviderRefreshPolicy() ProviderRefreshPolicy {
	return ProviderRefreshPolicy{
		OnRefreshError: ProviderRefreshErrorReturn,
		OnLockHeld:     ProviderLockHeldWaitForCache,
	}
}
