//go:build unit

package happyshrimp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startTestServer 启动一个固定响应/记录请求的测试服务器。
func startTestServer(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	client, err := NewClientWithBaseURL(server.URL, "")
	require.NoError(t, err)
	return client, server
}

func TestCreateGeneration_RoundTrip(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/generation/create", r.URL.Path)
		assert.Equal(t, "Bearer test-jwt", r.Header.Get("Authorization"))
		assert.Equal(t, AppID, r.Header.Get("X-App-Id"))
		assert.Equal(t, Language, r.Header.Get("X-Language"))
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errorCode":null,"errorMsg":null,"data":{"generationId":"HSGN123","songIds":["HSSG1","HSSG2"],"status":"PENDING","batchSize":2},"success":true}`))
	}))
	defer server.Close()
	client, err := NewClientWithBaseURL(server.URL, "")
	require.NoError(t, err)

	data, err := client.CreateGeneration(context.Background(), "test-jwt", &CreateGenerationRequest{
		Prompt: "晴空小快板", BatchSize: 2, Instrumental: false, Source: "ORIGINAL",
	})
	require.NoError(t, err)
	assert.Equal(t, "HSGN123", data.GenerationID)
	assert.Equal(t, []string{"HSSG1", "HSSG2"}, data.SongIDs)
	assert.Equal(t, "PENDING", data.Status)
	assert.Equal(t, "晴空小快板", captured["prompt"])
	assert.Equal(t, float64(2), captured["batchSize"])
}

func TestCreateGeneration_DefaultsAndValidation(t *testing.T) {
	client, server := startTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"generationId":"HSGN1","status":"PENDING","batchSize":1}}`))
	})
	defer server.Close()

	// 空 prompt → 拒绝
	_, err := client.CreateGeneration(context.Background(), "t", &CreateGenerationRequest{Prompt: "  "})
	assert.Error(t, err)

	// batchSize <= 0 → 默认 1；source 空 → ORIGINAL
	data, err := client.CreateGeneration(context.Background(), "t", &CreateGenerationRequest{Prompt: "p"})
	require.NoError(t, err)
	assert.Equal(t, 1, data.BatchSize)
}

func TestCreateGeneration_BusinessError(t *testing.T) {
	client, server := startTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errorCode":"INSUFFICIENT_CREDITS","errorMsg":"余额不足","success":false}`))
	})
	defer server.Close()

	_, err := client.CreateGeneration(context.Background(), "t", &CreateGenerationRequest{Prompt: "p"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "INSUFFICIENT_CREDITS")
	assert.Contains(t, err.Error(), "余额不足")
}

func TestBatchGet_TerminalDetection(t *testing.T) {
	client, server := startTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/generation/batch-get", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"generations":[{"generationId":"HSGN1","status":"COMPLETED","songs":[{"bizId":"HSSG1","status":"COMPLETED","audioUrl":"https://cdn/x.mp3","durationMs":116100}]}]}}`))
	})
	defer server.Close()

	data, err := client.BatchGet(context.Background(), "t", []string{"HSGN1"})
	require.NoError(t, err)
	require.Len(t, data.Generations, 1)
	assert.True(t, allTerminal(data))
	assert.Equal(t, int64(116100), *data.Generations[0].Songs[0].DurationMs)
}

func TestPollGenerationResult_TerminatesOnCompleted(t *testing.T) {
	// 第一次返回 RUNNING，之后返回 COMPLETED。
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			_, _ = w.Write([]byte(`{"success":true,"data":{"generations":[{"generationId":"HSGN1","status":"RUNNING","songs":[]}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":true,"data":{"generations":[{"generationId":"HSGN1","status":"COMPLETED","songs":[{"bizId":"HSSG1","status":"COMPLETED","audioUrl":"https://cdn/x.mp3"}]}]}}`))
	}))
	defer server.Close()
	client, err := NewClientWithBaseURL(server.URL, "")
	require.NoError(t, err)

	// 缩短轮询间隔以便测试快速完成。
	client.pollInterval = 5 * time.Millisecond
	client.pollInitialDelay = 0

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, err := client.PollGenerationResult(ctx, "t", []string{"HSGN1"})
	require.NoError(t, err)
	require.Len(t, data.Generations, 1)
	assert.Equal(t, StatusCompleted, data.Generations[0].Status)
	assert.GreaterOrEqual(t, calls, 2)
}

func TestPollGenerationResult_TimesOut(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"generations":[{"generationId":"HSGN1","status":"RUNNING","songs":[]}]}}`))
	}))
	defer server.Close()
	client, err := NewClientWithBaseURL(server.URL, "")
	require.NoError(t, err)

	client.pollInterval = time.Millisecond
	client.pollInitialDelay = 0
	client.pollTimeout = 50 * time.Millisecond

	_, err = client.PollGenerationResult(context.Background(), "t", []string{"HSGN1"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "timed out")
}

func TestRefreshToken_FlatShape(t *testing.T) {
	client, server := startTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/auth/token/refresh", r.URL.Path)
		assert.Empty(t, r.Header.Get("Authorization"), "refresh 不应带 access token")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"accessToken":"new-jwt","refreshToken":"new-refresh","accessExpiresIn":604800}}`))
	})
	defer server.Close()

	info, err := client.RefreshToken(context.Background(), "old-refresh")
	require.NoError(t, err)
	assert.Equal(t, "new-jwt", info.AccessToken)
	assert.Equal(t, "new-refresh", info.RefreshToken)
	assert.Equal(t, int64(604800), info.AccessExpiresIn)
}

func TestRefreshToken_NestedShape_KeepsRefresh(t *testing.T) {
	client, server := startTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"token":{"accessToken":"new-jwt","accessExpiresIn":604800}}}`))
	})
	defer server.Close()

	// 服务端未轮换 refreshToken 时，客户端保留旧值以维持可续期性。
	info, err := client.RefreshToken(context.Background(), "old-refresh")
	require.NoError(t, err)
	assert.Equal(t, "new-jwt", info.AccessToken)
	assert.Equal(t, "old-refresh", info.RefreshToken)
}

func TestGetCredits(t *testing.T) {
	client, server := startTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/credits/query", r.URL.Path)
		var body CreditsQueryRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		assert.Equal(t, BusinessType, body.BusinessType)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":280}`))
	})
	defer server.Close()

	credits, err := client.GetCredits(context.Background(), "t")
	require.NoError(t, err)
	assert.Equal(t, int64(280), credits)
}

func TestGetPrice(t *testing.T) {
	client, server := startTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/generation/price", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"price":40,"originalPrice":40,"pointDiscountRatio":1.0}}`))
	})
	defer server.Close()

	price, err := client.GetPrice(context.Background(), "t", &PriceRequest{Prompt: "p", BatchSize: 2})
	require.NoError(t, err)
	assert.Equal(t, 40, price.Price)
	assert.Equal(t, 1.0, price.PointDiscountRatio)
}

func TestEnsureURL(t *testing.T) {
	got, err := EnsureURL("")
	require.NoError(t, err)
	assert.Equal(t, DefaultBaseURL, got)

	got, err = EnsureURL("https://happy-gateway.happyshrimp.cn/")
	require.NoError(t, err)
	assert.Equal(t, "https://happy-gateway.happyshrimp.cn", got)

	_, err = EnsureURL("http://insecure.example.com")
	assert.Error(t, err)

	_, err = EnsureURL("not a url")
	assert.Error(t, err)
}

func TestParseTokenInfo_Shapes(t *testing.T) {
	// 平铺
	flat, err := ParseTokenInfo(json.RawMessage(`{"accessToken":"a","refreshToken":"r","accessExpiresIn":100}`))
	require.NoError(t, err)
	assert.Equal(t, "a", flat.AccessToken)

	// 嵌套 token
	nested, err := ParseTokenInfo(json.RawMessage(`{"token":{"accessToken":"b","accessExpiresIn":200}}`))
	require.NoError(t, err)
	assert.Equal(t, "b", nested.AccessToken)

	// 空
	_, err = ParseTokenInfo(json.RawMessage(`null`))
	assert.Error(t, err)

	// 无法识别
	_, err = ParseTokenInfo(json.RawMessage(`{"foo":"bar"}`))
	assert.Error(t, err)
}

func TestClient_HeadersApplied(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer jwt", r.Header.Get("Authorization"))
		assert.Equal(t, AppID, r.Header.Get("X-App-Id"))
		assert.Equal(t, Language, r.Header.Get("X-Language"))
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true,"data":{"generationId":"HSGN1","status":"PENDING"}}`))
	}))
	defer server.Close()
	client, err := NewClientWithBaseURL(server.URL, "")
	require.NoError(t, err)

	_, err = client.CreateGeneration(context.Background(), "jwt", &CreateGenerationRequest{Prompt: "p"})
	require.NoError(t, err)
}

func TestErrorEnvelope_HTTPError(t *testing.T) {
	client, server := startTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"errorCode":"RATE_LIMIT","errorMsg":"slow down","success":false}`))
	})
	defer server.Close()

	_, err := client.CreateGeneration(context.Background(), "t", &CreateGenerationRequest{Prompt: "p"})
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "status 429"), "err should mention status code: %v", err)
}
