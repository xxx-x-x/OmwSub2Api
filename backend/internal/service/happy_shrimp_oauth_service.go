package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/happyshrimp"
)

// happyShrimpAccessTokenTTL 是 access token 的期望有效期。
// 官网 JWT 的有效期为 7 天（iat → exp = 604800s），
// 刷新窗口沿用默认策略，实际以 refresh 响应中的 accessExpiresIn 为准。
const happyShrimpAccessTokenTTL = 7 * 24 * time.Hour

// HappyShrimpTokenInfo 是快乐虾米 token 信息（对齐登录/刷新响应）。
type HappyShrimpTokenInfo struct {
	AccessToken     string
	RefreshToken    string
	AccessExpiresIn int64 // 秒；0 表示未知
	DeviceID        string
	UserID          string
	Phone           string
}

// SendSMSCode 请求快乐虾米发送登录验证码。
func (s *HappyShrimpOAuthService) SendSMSCode(ctx context.Context, phone, phoneCountryCode string) (int64, error) {
	if s == nil {
		return 0, errors.New("happy shrimp oauth service is nil")
	}
	client, err := s.client(ctx, nil)
	if err != nil {
		return 0, err
	}
	return client.SendSMSCode(ctx, strings.TrimSpace(phone), strings.TrimSpace(phoneCountryCode))
}

// SMSLogin 使用验证码登录快乐虾米并构建账号凭据。
func (s *HappyShrimpOAuthService) SMSLogin(ctx context.Context, phone, phoneCountryCode, code string) (*HappyShrimpTokenInfo, error) {
	if s == nil {
		return nil, errors.New("happy shrimp oauth service is nil")
	}
	deviceIDBytes := make([]byte, 16)
	if _, err := rand.Read(deviceIDBytes); err != nil {
		return nil, fmt.Errorf("generate device id: %w", err)
	}
	client, err := s.client(ctx, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.SMSLogin(ctx, &happyshrimp.SMSLoginRequest{
		Phone: strings.TrimSpace(phone), PhoneCountryCode: strings.TrimSpace(phoneCountryCode),
		Code: strings.TrimSpace(code), DeviceID: hex.EncodeToString(deviceIDBytes),
		DeviceType: "web", DeviceName: "browser", DeviceModel: "Web", AppVersion: "1.0.0",
	})
	if err != nil {
		return nil, err
	}
	info := &HappyShrimpTokenInfo{
		AccessToken: resp.Data.Token.AccessToken, RefreshToken: resp.Data.Token.RefreshToken,
		AccessExpiresIn: resp.Data.Token.AccessExpiresIn, DeviceID: hex.EncodeToString(deviceIDBytes),
		UserID: fmt.Sprintf("%d", resp.Data.User.ID), Phone: resp.Data.User.Phone,
	}
	if info.Phone == "" {
		info.Phone = strings.TrimSpace(phone)
	}
	if info.AccessExpiresIn <= 0 {
		info.AccessExpiresIn = int64(happyShrimpAccessTokenTTL / time.Second)
	}
	return info, nil
}

// HappyShrimpOAuthService 负责快乐虾米 token 刷新与凭据构建。
// 快乐虾米没有浏览器 OAuth 流程：账号通过手工导入
// access_token + refresh_token（即网页登录态的 hs_auth）建立。
type HappyShrimpOAuthService struct {
	proxyRepo ProxyRepository
	config    *config.Config
}

// NewHappyShrimpOAuthService 创建快乐虾米 OAuth 服务。
func NewHappyShrimpOAuthService(proxyRepo ProxyRepository, configs ...*config.Config) *HappyShrimpOAuthService {
	svc := &HappyShrimpOAuthService{proxyRepo: proxyRepo}
	if len(configs) > 0 {
		svc.config = configs[0]
	}
	return svc
}

// proxyURL 解析账号代理。
func (s *HappyShrimpOAuthService) proxyURL(ctx context.Context, proxyID *int64) (string, error) {
	if proxyID == nil || s.proxyRepo == nil {
		return "", nil
	}
	proxy, err := s.proxyRepo.GetByID(ctx, *proxyID)
	if err != nil {
		return "", err
	}
	if proxy == nil {
		return "", nil
	}
	return proxy.URL(), nil
}

// client 构建带代理的 API 客户端。
func (s *HappyShrimpOAuthService) client(ctx context.Context, account *Account) (*happyshrimp.Client, error) {
	proxyURL := ""
	if account != nil {
		var err error
		proxyURL, err = s.proxyURL(ctx, account.ProxyID)
		if err != nil {
			return nil, err
		}
	}
	baseURL := ""
	if account != nil {
		baseURL = strings.TrimSpace(account.GetCredential("base_url"))
	}
	return happyshrimp.NewClientWithBaseURL(baseURL, proxyURL)
}

// RefreshAccountToken 用账号的 refresh_token 换新 token。
func (s *HappyShrimpOAuthService) RefreshAccountToken(ctx context.Context, account *Account) (*HappyShrimpTokenInfo, error) {
	if s == nil {
		return nil, errors.New("happy shrimp oauth service is nil")
	}
	if account == nil {
		return nil, errors.New("account is nil")
	}
	refreshToken := strings.TrimSpace(account.GetHappyShrimpRefreshToken())
	if refreshToken == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "HAPPY_SHRIMP_NO_REFRESH_TOKEN", "refresh_token is required")
	}
	client, err := s.client(ctx, account)
	if err != nil {
		return nil, err
	}
	tokenInfo, err := client.RefreshToken(ctx, refreshToken)
	if err != nil {
		return nil, err
	}

	out := &HappyShrimpTokenInfo{
		AccessToken:     tokenInfo.AccessToken,
		RefreshToken:    tokenInfo.RefreshToken,
		AccessExpiresIn: tokenInfo.AccessExpiresIn,
		DeviceID:        account.GetCredential("device_id"),
		UserID:          account.GetCredential("user_id"),
		Phone:           account.GetCredential("phone"),
	}
	if out.AccessExpiresIn <= 0 {
		out.AccessExpiresIn = int64(happyShrimpAccessTokenTTL / time.Second)
	}
	return out, nil
}

// BuildAccountCredentials 把 token 信息转为要持久化的 credentials map。
// 保留账号现有字段（device_id/user_id/phone/base_url 等），由调用方 merge。
func (s *HappyShrimpOAuthService) BuildAccountCredentials(info *HappyShrimpTokenInfo) map[string]any {
	creds := map[string]any{}
	if info == nil {
		return creds
	}
	if info.AccessToken != "" {
		creds["access_token"] = info.AccessToken
	}
	if info.RefreshToken != "" {
		creds["refresh_token"] = info.RefreshToken
	}
	// expires_at 存 Unix 秒时间戳，供 GetCredentialAsTime 解析（与 Grok 一致）。
	expiresAt := time.Now().Add(time.Duration(info.AccessExpiresIn) * time.Second).Unix()
	creds["expires_at"] = expiresAt
	if info.DeviceID != "" {
		creds["device_id"] = info.DeviceID
	}
	if info.UserID != "" {
		creds["user_id"] = info.UserID
	}
	if info.Phone != "" {
		creds["phone"] = info.Phone
	}
	return creds
}
