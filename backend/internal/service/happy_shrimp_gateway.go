package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/happyshrimp"
	"github.com/gin-gonic/gin"
)

// happyShrimpGenerationTimeout 单次生成请求的总超时（含轮询）。
const happyShrimpGenerationTimeout = 3*time.Minute + 15*time.Second

// AudioGenerationItem 是 /v1/audio/generations 响应的单首歌。
type AudioGenerationItem struct {
	ID          string  `json:"id"`
	Title       string  `json:"title,omitempty"`
	DurationMS  *int64  `json:"duration_ms,omitempty"`
	URL         string  `json:"url"`
	CoverURL    string  `json:"cover_url,omitempty"`
	Instrumental bool    `json:"instrumental,omitempty"`
	Tags        []Tag   `json:"tags,omitempty"`
}

// Tag 音频标签（与 pkg/happyshrimp.Tag 对齐的对外结构）。
type Tag struct {
	Dim  string  `json:"dim,omitempty"`
	Name string  `json:"name,omitempty"`
	Conf float64 `json:"conf,omitempty"`
}

// AudioGenerationsResponse 是 /v1/audio/generations 的成功响应体。
type AudioGenerationsResponse struct {
	Data []AudioGenerationItem `json:"data"`
}

// HappyShrimpGatewayService 快乐虾米音乐生成网关。
type HappyShrimpGatewayService struct {
	accountRepo   AccountRepository
	tokenProvider *HappyShrimpTokenProvider
	settingService *SettingService
}

// NewHappyShrimpGatewayService 创建快乐虾米网关服务。
func NewHappyShrimpGatewayService(
	accountRepo AccountRepository,
	tokenProvider *HappyShrimpTokenProvider,
	settingService *SettingService,
) *HappyShrimpGatewayService {
	return &HappyShrimpGatewayService{
		accountRepo:    accountRepo,
		tokenProvider:  tokenProvider,
		settingService: settingService,
	}
}

// AudioGenerationsRequest 是 /v1/audio/generations 的请求体（OpenAI 兼容风格）。
type AudioGenerationsRequest struct {
	Model        string `json:"model"`
	Prompt       string `json:"prompt" binding:"required"`
	N            *int   `json:"n,omitempty"`
	Instrumental *bool  `json:"instrumental,omitempty"`
	Source       string `json:"source,omitempty"`
}

// normalize 填充默认值并校验。
func (req *AudioGenerationsRequest) normalize() error {
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" {
		return errors.New("prompt is required")
	}
	if req.Model == "" {
		req.Model = "HappyShrimp"
	}
	if req.N == nil || *req.N <= 0 {
		n := 1
		req.N = &n
	}
	if *req.N > 8 {
		return fmt.Errorf("n must be between 1 and 8, got %d", *req.N)
	}
	if req.Source == "" {
		req.Source = "ORIGINAL"
	}
	return nil
}

// Forward 处理一次 /v1/audio/generations 请求：
//  1. 取 access token
//  2. create 创建生成任务
//  3. 轮询 batch-get 直到完成
//  4. 组装 OpenAI 风格响应（歌曲 URL 列表）
//
// 失败返回 *UpstreamFailoverError 时由上层切换账号重试。
func (s *HappyShrimpGatewayService) Forward(ctx context.Context, c *gin.Context, account *Account, body []byte) (*AudioGenerationsResponse, error) {
	var req AudioGenerationsRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("parse request body: %w", err)
	}
	if err := req.normalize(); err != nil {
		return nil, err
	}

	if account == nil {
		return nil, errors.New("account is required")
	}
	if !account.IsHappyShrimpOAuth() {
		return nil, errors.New("not a happy shrimp oauth account")
	}

	// 1) access token（请求路径自动刷新）
	if s.tokenProvider == nil {
		return nil, errors.New("happy shrimp token provider not configured")
	}
	accessToken, err := s.tokenProvider.GetAccessToken(ctx, account)
	if err != nil {
		return nil, &UpstreamFailoverError{
			StatusCode:   http.StatusUnauthorized,
			Reason:       GatewayFailureReason("happy_shrimp_token"),
			ResponseBody: []byte(`{"error":{"type":"authentication_error","message":"Failed to get happy shrimp access token"}}`),
		}
	}

	// 2) create
	client, err := s.clientFor(ctx, account)
	if err != nil {
		return nil, err
	}
	createReq := &happyshrimp.CreateGenerationRequest{
		Prompt:       req.Prompt,
		BatchSize:    *req.N,
		Instrumental: req.Instrumental != nil && *req.Instrumental,
		Source:       req.Source,
	}
	created, err := client.CreateGeneration(ctx, accessToken, createReq)
	if err != nil {
		return nil, s.classifyUpstreamError(err)
	}

	// 3) 轮询
	pollCtx, cancel := context.WithTimeout(ctx, happyShrimpGenerationTimeout)
	defer cancel()
	result, err := client.PollGenerationResult(pollCtx, accessToken, []string{created.GenerationID})
	if err != nil {
		return nil, s.classifyUpstreamError(err)
	}

	// 4) 组装响应
	items := make([]AudioGenerationItem, 0, len(result.Generations))
	for _, gen := range result.Generations {
		for _, song := range gen.Songs {
			if strings.TrimSpace(song.AudioURL) == "" {
				continue
			}
			item := AudioGenerationItem{
				ID:           song.BizID,
				Title:        song.Title,
				DurationMS:   song.DurationMs,
				URL:          song.AudioURL,
				CoverURL:     song.CoverURL,
				Instrumental: song.Instrumental,
			}
			for _, t := range song.Tags {
				item.Tags = append(item.Tags, Tag{Dim: t.Dim, Name: t.Tag, Conf: t.Conf})
			}
			items = append(items, item)
		}
	}
	if len(items) == 0 {
		return nil, errors.New("happy shrimp generation completed without audio results")
	}
	return &AudioGenerationsResponse{Data: items}, nil
}

// clientFor 构建带账号代理的 API 客户端。
func (s *HappyShrimpGatewayService) clientFor(ctx context.Context, account *Account) (*happyshrimp.Client, error) {
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	baseURL := strings.TrimSpace(account.GetCredential("base_url"))
	return happyshrimp.NewClientWithBaseURL(baseURL, proxyURL)
}

// classifyUpstreamError 把上游错误映射为可 failover 的错误。
// 401/403（token 失效）触发账号切换；其余返回普通错误。
func (s *HappyShrimpGatewayService) classifyUpstreamError(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "status 401") || strings.Contains(msg, "status 403") {
		return &UpstreamFailoverError{
			StatusCode: http.StatusUnauthorized,
			Reason:     GatewayFailureReason("happy_shrimp_auth"),
		}
	}
	return err
}
