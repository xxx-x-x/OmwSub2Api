// Package happyshrimp implements the Happy Shrimp (快乐虾米) AI music
// generation API protocol.
//
// The upstream exposes a custom REST API on happy-gateway.happyshrimp.cn:
//   - POST /api/v1/generation/create    异步创建音乐生成任务
//   - POST /api/v1/generation/batch-get 轮询批量查询任务/歌曲结果
//   - POST /api/v1/generation/price     查询按 prompt 计费的价格
//   - POST /api/v1/credits/query        查询账户积分余额
//   - POST /api/v1/auth/token/refresh   用 refreshToken 换新 JWT
//
// Authentication is a HS256 JWT access token carried as
// `Authorization: Bearer <jwt>`, plus static headers X-App-Id: shrimp_cn
// and X-Language. Access tokens last ~7 days and are renewed via the
// refresh endpoint (refreshToken rotates on each refresh).
package happyshrimp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyurl"
	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyutil"
)

// DefaultBaseURL is the production API gateway.
const DefaultBaseURL = "https://happy-gateway.happyshrimp.cn"

// AppID is the fixed web application identifier.
const AppID = "shrimp_cn"

// Language is the fixed request language header value.
const Language = "zh-CN"

// BusinessType credits query scope.
const BusinessType = "HAPPY_SHRIMP"

// Poll intervals / timeouts for create → batch-get polling.
const (
	// pollInitialDelay 首次查询前的等待
	pollInitialDelay = 3 * time.Second
	// pollInterval batch-get 轮询间隔（服务端歌曲生成约 1 分钟）
	pollInterval = 3 * time.Second
	// pollTimeout 单次请求等待结果的总超时
	pollTimeout = 3 * time.Minute
	// clientTimeout 单次 HTTP 请求超时
	clientTimeout = 15 * time.Second
	// proxyDialTimeout 代理 TCP 连接超时
	proxyDialTimeout = 5 * time.Second
	// proxyTLSHandshakeTimeout 代理 TLS 握手超时
	proxyTLSHandshakeTimeout = 5 * time.Second
)

// Generation status values observed on the wire.
const (
	StatusPending   = "PENDING"
	StatusRunning   = "RUNNING"
	StatusCompleted = "COMPLETED"
	StatusFailed    = "FAILED"
)

// Client 快乐虾米 API 客户端。
// 与 antigravity 客户端同构：自持 http.Client，可选配置 HTTP/SOCKS 代理。
type Client struct {
	baseURL          string
	httpClient       *http.Client
	pollInitialDelay time.Duration // 测试可覆盖
	pollInterval     time.Duration // 测试可覆盖
	pollTimeout      time.Duration // 测试可覆盖
}

// NewClient 创建客户端。proxyURL 为空表示直连。
func NewClient(proxyURL string) (*Client, error) {
	return NewClientWithBaseURL(DefaultBaseURL, proxyURL)
}

// NewClientWithBaseURL 创建客户端并允许覆盖 base URL（测试/自定义网关）。
func NewClientWithBaseURL(baseURL, proxyURL string) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	client := &http.Client{
		Timeout: clientTimeout,
	}

	_, parsed, err := proxyurl.Parse(proxyURL)
	if err != nil {
		return nil, err
	}
	if parsed != nil {
		transport := &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: proxyDialTimeout,
			}).DialContext,
			TLSHandshakeTimeout: proxyTLSHandshakeTimeout,
		}
		if err := proxyutil.ConfigureTransportProxy(transport, parsed); err != nil {
			return nil, fmt.Errorf("configure proxy: %w", err)
		}
		client.Transport = transport
	}
	return &Client{
		baseURL:          baseURL,
		httpClient:       client,
		pollInitialDelay: pollInitialDelay,
		pollInterval:     pollInterval,
		pollTimeout:      pollTimeout,
	}, nil
}

// doJSON 发送 JSON 请求并解析 JSON 响应；statusOK 校验 2xx。
func (c *Client) doJSON(ctx context.Context, method, path string, accessToken string, reqBody, respBody any) error {
	var bodyReader io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-App-Id", AppID)
	req.Header.Set("X-Language", Language)
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("upstream request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upstream status %d: %s", resp.StatusCode, truncateString(string(raw), 512))
	}
	if respBody == nil {
		return nil
	}
	if err := json.Unmarshal(raw, respBody); err != nil {
		return fmt.Errorf("unmarshal response: %w", err)
	}
	return nil
}

func truncateString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// ErrorEnvelope 是业务错误信封：errorCode 非空或 success=false 表示业务失败。
type ErrorEnvelope struct {
	ErrorCode any    `json:"errorCode"`
	ErrorMsg  string `json:"errorMsg"`
	Success   bool   `json:"success"`
}

// isBusinessError 判断响应是否携带业务错误。返回错误消息（可能为空）。
func (e *ErrorEnvelope) businessError() error {
	if e == nil {
		return nil
	}
	if e.Success {
		return nil
	}
	msg := strings.TrimSpace(e.ErrorMsg)
	code := ""
	if e.ErrorCode != nil {
		code = fmt.Sprintf("%v", e.ErrorCode)
	}
	if code == "" && msg == "" {
		return nil
	}
	return errors.New("happy shrimp business error: " + strings.TrimSpace(code+" "+msg))
}

// CreateGenerationRequest 创建音乐生成任务的请求体（与官网 create 一致）。
type CreateGenerationRequest struct {
	Prompt       string `json:"prompt"`
	BatchSize    int    `json:"batchSize"`
	Instrumental bool   `json:"instrumental"`
	Source       string `json:"source"`
}

// CreateGenerationResponse 创建任务响应。
type CreateGenerationResponse struct {
	ErrorEnvelope
	Data CreateGenerationData `json:"data"`
}

type CreateGenerationData struct {
	GenerationID string   `json:"generationId"`
	SongIDs      []string `json:"songIds"`
	Status       string   `json:"status"`
	BatchSize    int      `json:"batchSize"`
}

// BatchGetRequest 批量查询任务。
type BatchGetRequest struct {
	GenerationIDs []string `json:"generationIds"`
}

// BatchGetResponse 批量查询响应。
type BatchGetResponse struct {
	ErrorEnvelope
	Data BatchGetData `json:"data"`
}

type BatchGetData struct {
	Generations []Generation `json:"generations"`
}

// Generation 一次生成任务及其歌曲。
type Generation struct {
	GenerationID string `json:"generationId"`
	Model        string `json:"model"`
	Status       string `json:"status"`
	BatchSize    int    `json:"batchSize"`
	Prompt       string `json:"prompt"`
	Source       string `json:"source"`
	Songs        []Song `json:"songs"`
	ErrorCode    any    `json:"errorCode"`
}

// Song 歌曲结果。
type Song struct {
	BizID        string  `json:"bizId"`
	Status       string  `json:"status"`
	Title        string  `json:"title"`
	DurationMs   *int64  `json:"durationMs"`
	AudioURL     string  `json:"audioUrl"`
	CoverURL     string  `json:"coverUrl"`
	AudioMediaID string  `json:"audioMediaId"`
	Instrumental bool    `json:"instrumental"`
	Tags         []Tag   `json:"tags"`
	ErrorCode    any     `json:"errorCode"`
	ErrorMsg     *string `json:"errorMsg"`
}

// Tag 歌曲标签。
type Tag struct {
	Dim  string  `json:"dim"`
	Tag  string  `json:"tag"`
	Conf float64 `json:"conf"`
}

// PriceRequest 价格查询（与 create 同体）。
type PriceRequest = CreateGenerationRequest

// PriceResponse 价格查询响应。
type PriceResponse struct {
	ErrorEnvelope
	Data PriceData `json:"data"`
}

type PriceData struct {
	Price              int     `json:"price"`
	OriginalPrice      int     `json:"originalPrice"`
	PointDiscountRatio float64 `json:"pointDiscountRatio"`
}

// CreditsQueryRequest 积分查询请求。
type CreditsQueryRequest struct {
	BusinessType string `json:"businessType"`
}

// CreditsQueryResponse 积分查询响应（data 为积分余额）。
type CreditsQueryResponse struct {
	ErrorEnvelope
	Data int64 `json:"data"`
}

// RefreshTokenRequest 刷新 JWT 请求。
type RefreshTokenRequest struct {
	RefreshToken string `json:"refreshToken"`
}

// SendSMSCodeRequest 请求快乐虾米发送登录验证码。
type SendSMSCodeRequest struct {
	Phone            string `json:"phone"`
	PhoneCountryCode string `json:"phoneCountryCode"`
}

// SendSMSCodeResponse 快乐虾米发送验证码响应。
type SendSMSCodeResponse struct {
	ErrorEnvelope
	Data struct {
		ExpiresIn int64 `json:"expiresIn"`
	} `json:"data"`
}

// SMSLoginRequest 快乐虾米短信登录请求。
type SMSLoginRequest struct {
	Phone            string `json:"phone"`
	PhoneCountryCode string `json:"phoneCountryCode"`
	Code             string `json:"code"`
	DeviceID         string `json:"deviceId"`
	DeviceType       string `json:"deviceType"`
	DeviceName       string `json:"deviceName"`
	DeviceModel      string `json:"deviceModel"`
	AppVersion       string `json:"appVersion"`
}

// SMSLoginResponse 快乐虾米短信登录响应。
type SMSLoginResponse struct {
	ErrorEnvelope
	Data struct {
		User struct {
			ID    int64  `json:"id"`
			Phone string `json:"phone"`
		} `json:"user"`
		Token TokenInfo `json:"token"`
	} `json:"data"`
}

// TokenInfo 刷新/登录返回的 token 信息。
// 兼容两种响应形态：`{data: {accessToken, refreshToken, accessExpiresIn}}`
// 或 `{data: {user, token: {...}}}`。
type TokenInfo struct {
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"refreshToken"`
	AccessExpiresIn  int64  `json:"accessExpiresIn"`
	RefreshExpiresIn int64  `json:"refreshExpiresIn"`
	TokenType        string `json:"tokenType"`
}

// RefreshTokenResponse 刷新响应信封。
type RefreshTokenResponse struct {
	ErrorEnvelope
	Data json.RawMessage `json:"data"`
}

// ParseTokenInfo 从刷新响应中提取 TokenInfo，容忍嵌套形态。
func ParseTokenInfo(raw json.RawMessage) (*TokenInfo, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, errors.New("empty token response")
	}
	// 形态 1: 平铺 {accessToken,...}
	var flat TokenInfo
	if err := json.Unmarshal(raw, &flat); err == nil && flat.AccessToken != "" {
		return &flat, nil
	}
	// 形态 2: {token: {...}}
	var nested struct {
		Token *TokenInfo `json:"token"`
	}
	if err := json.Unmarshal(raw, &nested); err == nil && nested.Token != nil {
		return nested.Token, nil
	}
	// 形态 3: {tokenInfo: {...}}
	var nested2 struct {
		TokenInfo *TokenInfo `json:"tokenInfo"`
	}
	if err := json.Unmarshal(raw, &nested2); err == nil && nested2.TokenInfo != nil {
		return nested2.TokenInfo, nil
	}
	return nil, errors.New("unrecognized token response shape")
}

// CreateGeneration 创建音乐生成任务。
func (c *Client) CreateGeneration(ctx context.Context, accessToken string, req *CreateGenerationRequest) (*CreateGenerationData, error) {
	if req == nil {
		return nil, errors.New("request is nil")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, errors.New("prompt is required")
	}
	if req.BatchSize <= 0 {
		req.BatchSize = 1
	}
	if req.Source == "" {
		req.Source = "ORIGINAL"
	}
	var resp CreateGenerationResponse
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/generation/create", accessToken, req, &resp); err != nil {
		return nil, err
	}
	if err := resp.businessError(); err != nil {
		return nil, err
	}
	if resp.Data.GenerationID == "" {
		return nil, errors.New("create generation returned empty generationId")
	}
	return &resp.Data, nil
}

// BatchGet 批量查询生成任务结果。
func (c *Client) BatchGet(ctx context.Context, accessToken string, generationIDs []string) (*BatchGetData, error) {
	if len(generationIDs) == 0 {
		return nil, errors.New("generationIds is required")
	}
	var resp BatchGetResponse
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/generation/batch-get", accessToken, &BatchGetRequest{GenerationIDs: generationIDs}, &resp); err != nil {
		return nil, err
	}
	if err := resp.businessError(); err != nil {
		return nil, err
	}
	return &resp.Data, nil
}

// GetPrice 查询生成价格。
func (c *Client) GetPrice(ctx context.Context, accessToken string, req *PriceRequest) (*PriceData, error) {
	if req == nil {
		return nil, errors.New("request is nil")
	}
	var resp PriceResponse
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/generation/price", accessToken, req, &resp); err != nil {
		return nil, err
	}
	if err := resp.businessError(); err != nil {
		return nil, err
	}
	return &resp.Data, nil
}

// GetCredits 查询积分余额。
func (c *Client) GetCredits(ctx context.Context, accessToken string) (int64, error) {
	var resp CreditsQueryResponse
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/credits/query", accessToken, &CreditsQueryRequest{BusinessType: BusinessType}, &resp); err != nil {
		return 0, err
	}
	if err := resp.businessError(); err != nil {
		return 0, err
	}
	return resp.Data, nil
}

// SendSMSCode 请求快乐虾米发送短信验证码。
func (c *Client) SendSMSCode(ctx context.Context, phone, phoneCountryCode string) (int64, error) {
	var resp SendSMSCodeResponse
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/auth/sms/send", "", &SendSMSCodeRequest{
		Phone: phone, PhoneCountryCode: phoneCountryCode,
	}, &resp)
	if err != nil {
		return 0, err
	}
	if err := resp.businessError(); err != nil {
		return 0, err
	}
	return resp.Data.ExpiresIn, nil
}

// SMSLogin 使用短信验证码登录并取得 access/refresh token。
func (c *Client) SMSLogin(ctx context.Context, req *SMSLoginRequest) (*SMSLoginResponse, error) {
	if req == nil {
		return nil, errors.New("sms login request is nil")
	}
	var resp SMSLoginResponse
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/auth/sms/login", "", req, &resp); err != nil {
		return nil, err
	}
	if err := resp.businessError(); err != nil {
		return nil, err
	}
	if resp.Data.Token.AccessToken == "" || resp.Data.Token.RefreshToken == "" {
		return nil, errors.New("sms login returned incomplete token")
	}
	return &resp, nil
}

// RefreshToken 用 refreshToken 换新 token。
func (c *Client) RefreshToken(ctx context.Context, refreshToken string) (*TokenInfo, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil, errors.New("refresh_token is required")
	}
	var resp RefreshTokenResponse
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/auth/token/refresh", "", &RefreshTokenRequest{RefreshToken: refreshToken}, &resp); err != nil {
		return nil, err
	}
	if err := resp.businessError(); err != nil {
		return nil, err
	}
	info, err := ParseTokenInfo(resp.Data)
	if err != nil {
		return nil, err
	}
	if info.RefreshToken == "" {
		// 服务端可能不轮换 refreshToken：保留原值以维持可续期性。
		info.RefreshToken = refreshToken
	}
	return info, nil
}

// PollGenerationResult 轮询直到生成完成或超时。
// 返回最终的 BatchGetData（含 COMPLETED 或 FAILED 的歌曲）。
func (c *Client) PollGenerationResult(ctx context.Context, accessToken string, generationIDs []string) (*BatchGetData, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(c.pollInitialDelay):
	}

	timer := time.NewTimer(c.pollTimeout)
	defer timer.Stop()
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()

	for {
		data, err := c.BatchGet(ctx, accessToken, generationIDs)
		if err != nil {
			return nil, err
		}
		if allTerminal(data) {
			return data, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, errors.New("happy shrimp generation polling timed out")
		case <-ticker.C:
		}
	}
}

// allTerminal 判断所有 generation 均已进入终态（COMPLETED/FAILED）。
func allTerminal(data *BatchGetData) bool {
	if data == nil || len(data.Generations) == 0 {
		return false
	}
	for _, g := range data.Generations {
		if g.Status != StatusCompleted && g.Status != StatusFailed {
			return false
		}
	}
	return true
}

// EnsureURL 校验并规范化快乐虾米 base URL（仅 https，防 SSRF 由上层负责）。
func EnsureURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return DefaultBaseURL, nil
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid happy shrimp base url: %s", trimmed)
	}
	if parsed.Scheme != "https" {
		return "", fmt.Errorf("happy shrimp base url must use https: %s", trimmed)
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}
