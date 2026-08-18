//go:build unit

package service

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func happyShrimpTestAccount() *Account {
	return &Account{
		ID:       42,
		Platform: PlatformHappyShrimp,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":  "jwt-1",
			"refresh_token": "rt-1",
			"device_id":     "dev-1",
			"user_id":       "110294198105",
			"phone":         "15330043032",
		},
	}
}

func TestHappyShrimpTokenRefresher_CanRefresh(t *testing.T) {
	r := &HappyShrimpTokenRefresher{}

	assert.False(t, r.CanRefresh(nil))
	assert.False(t, r.CanRefresh(&Account{Platform: PlatformHappyShrimp, Type: AccountTypeOAuth})) // 无 refresh_token
	assert.False(t, r.CanRefresh(happyShrimpTestAccountWithType(AccountTypeAPIKey)))
	assert.True(t, r.CanRefresh(happyShrimpTestAccount()))
}

func happyShrimpTestAccountWithType(accountType string) *Account {
	a := happyShrimpTestAccount()
	a.Type = accountType
	return a
}

func TestHappyShrimpTokenRefresher_NeedsRefresh(t *testing.T) {
	r := &HappyShrimpTokenRefresher{}
	window := happyShrimpRefreshSkew

	// 无 refresh_token → 不刷
	assert.False(t, r.NeedsRefresh(&Account{Platform: PlatformHappyShrimp, Type: AccountTypeOAuth}, window))

	// access_token 缺失 → 刷
	assert.True(t, r.NeedsRefresh(happyShrimpTestAccountWithout("access_token"), window))

	// expires_at 缺失 → 刷
	assert.True(t, r.NeedsRefresh(happyShrimpTestAccountWithout("expires_at"), window))

	// 未到期（7 天后）→ 不刷
	a := happyShrimpTestAccount()
	a.Credentials["expires_at"] = time.Now().Add(7 * 24 * time.Hour).Unix()
	assert.False(t, r.NeedsRefresh(a, window))

	// 1 小时内过期 → 刷
	a = happyShrimpTestAccount()
	a.Credentials["expires_at"] = time.Now().Add(30 * time.Minute).Unix()
	assert.True(t, r.NeedsRefresh(a, window))
}

func happyShrimpTestAccountWithout(key string) *Account {
	a := happyShrimpTestAccount()
	delete(a.Credentials, key)
	return a
}

func TestHappyShrimpRefreshWindowWithJitter(t *testing.T) {
	// 确定性的：同一账号 ID 结果稳定
	w1 := happyShrimpRefreshWindowWithJitter(42, time.Hour)
	w2 := happyShrimpRefreshWindowWithJitter(42, time.Hour)
	assert.Equal(t, w1, w2)

	// 下限保护
	assert.GreaterOrEqual(t, w1, happyShrimpRefreshSkewMin)
	// 不超过原窗口
	assert.LessOrEqual(t, w1, time.Hour)

	// accountID <= 0 时不做抖动
	assert.Equal(t, time.Hour, happyShrimpRefreshWindowWithJitter(0, time.Hour))
}

func TestHappyShrimpOAuthService_BuildAccountCredentials(t *testing.T) {
	svc := &HappyShrimpOAuthService{}
	now := time.Now()
	info := &HappyShrimpTokenInfo{
		AccessToken:     "new-jwt",
		RefreshToken:    "new-rt",
		AccessExpiresIn: 604800,
		DeviceID:        "dev-1",
		UserID:          "110294198105",
		Phone:           "15330043032",
	}
	creds := svc.BuildAccountCredentials(info)
	assert.Equal(t, "new-jwt", creds["access_token"])
	assert.Equal(t, "new-rt", creds["refresh_token"])
	assert.Equal(t, "dev-1", creds["device_id"])

	// expires_at 必须是未来的 Unix 时间戳（可被 GetCredentialAsTime 解析）
	expiresAt, ok := creds["expires_at"].(int64)
	require.True(t, ok)
	assert.Greater(t, expiresAt, now.Add(6*24*time.Hour).Unix())
	assert.Less(t, expiresAt, now.Add(8*24*time.Hour).Unix())
}

func TestHappyShrimpOAuthService_BuildAccountCredentials_ZeroExpiry(t *testing.T) {
	svc := &HappyShrimpOAuthService{}
	info := &HappyShrimpTokenInfo{AccessToken: "t", RefreshToken: "r"}
	creds := svc.BuildAccountCredentials(info)
	expiresAt, ok := creds["expires_at"].(int64)
	require.True(t, ok)
	// AccessExpiresIn=0 → 到期时间即当前时刻（默认 TTL 由 RefreshAccountToken 填充）。
	assert.InDelta(t, time.Now().Unix(), expiresAt, 5)
}

func TestHappyShrimpGatewayRequest_Normalize(t *testing.T) {
	// prompt 必填
	req := &AudioGenerationsRequest{}
	err := req.normalize()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "prompt")

	// 默认值
	req = &AudioGenerationsRequest{Prompt: "晴空小快板"}
	err = req.normalize()
	require.NoError(t, err)
	assert.Equal(t, "HappyShrimp", req.Model)
	require.NotNil(t, req.N)
	assert.Equal(t, 1, *req.N)
	assert.Equal(t, "ORIGINAL", req.Source)

	// N 上限
	big := 9
	req = &AudioGenerationsRequest{Prompt: "p", N: &big}
	err = req.normalize()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "between 1 and 8")

	// N=8 合法
	eight := 8
	req = &AudioGenerationsRequest{Prompt: "p", N: &eight}
	err = req.normalize()
	require.NoError(t, err)

	// instrumental 显式 false 保留
	f := false
	req = &AudioGenerationsRequest{Prompt: "p", Instrumental: &f}
	err = req.normalize()
	require.NoError(t, err)
	assert.NotNil(t, req.Instrumental)
	assert.False(t, *req.Instrumental)
}

func TestHappyShrimpGateway_ClassifyUpstreamError(t *testing.T) {
	svc := &HappyShrimpGatewayService{}
	// 401/403 → failover
	failoverErr, ok := svc.classifyUpstreamError(errors.New("upstream status 401")).(*UpstreamFailoverError)
	require.True(t, ok)
	assert.Equal(t, GatewayFailureReason("happy_shrimp_auth"), failoverErr.Reason)

	// 其他 → 原样返回
	plain := errors.New("upstream status 500 boom")
	assert.Same(t, plain, svc.classifyUpstreamError(plain))
}
