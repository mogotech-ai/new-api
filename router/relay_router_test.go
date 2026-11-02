package router

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type siwcTestTransport func(*http.Request) (*http.Response, error)

func (f siwcTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestSIWCNonStreamingChatAndCredentialRotation(t *testing.T) {
	for _, channelID := range []int{17, 42} {
		t.Run(fmt.Sprintf("channel_%d", channelID), func(t *testing.T) {
			testSIWCNonStreamingChatAndCredentialRotation(t, channelID)
		})
	}
}

func testSIWCNonStreamingChatAndCredentialRotation(t *testing.T, channelID int) {
	t.Helper()
	setupRelayRouterTestDB(t)
	require.NoError(t, i18n.Init())
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.SystemTaskLock{}, &model.Log{}, &model.AuditLog{}, &model.Option{}, &model.RelayPayload{}))
	previousCache, previousLog, previousCount := common.MemoryCacheEnabled, common.LogConsumeEnabled, constant.CountToken
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	settings := model_setting.GetGlobalSettings()
	previousPolicy, previousPassThrough := settings.ChatCompletionsToResponsesPolicy, settings.PassThroughRequestEnabled
	common.MemoryCacheEnabled, common.LogConsumeEnabled, constant.CountToken = true, false, false
	settings.PassThroughRequestEnabled = false
	settings.ChatCompletionsToResponsesPolicy = model_setting.ChatCompletionsToResponsesPolicy{Enabled: false}
	t.Cleanup(func() {
		constant.StreamingTimeout = previousTimeout
		common.MemoryCacheEnabled, common.LogConsumeEnabled, constant.CountToken = previousCache, previousLog, previousCount
		settings.ChatCompletionsToResponsesPolicy, settings.PassThroughRequestEnabled = previousPolicy, previousPassThrough
	})
	service.InitHttpClient()
	var rotated atomic.Bool
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			if !assert.NoError(t, r.ParseForm()) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			assert.Equal(t, "refresh_token", r.Form.Get("grant_type"))
			assert.Equal(t, fmt.Sprintf("issued-local-test-client-%d", channelID), r.Form.Get("client_id"))
			assert.Equal(t, "old-refresh", r.Form.Get("refresh_token"))
			assert.Equal(t, "https://api.openai.com/v1", r.Form.Get("resource"))
			rotated.Store(true)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`)
			return
		}
		calls.Add(1)
		assert.Equal(t, "/v1/responses", r.URL.Path)
		expected := "Bearer old-access"
		if rotated.Load() {
			expected = "Bearer new-access"
		}
		assert.Equal(t, expected, r.Header.Get("Authorization"))
		var body map[string]any
		if !assert.NoError(t, common.DecodeJson(r.Body, &body)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Equal(t, "gpt-6.1-terra", body["model"])
		assert.Equal(t, true, body["stream"])
		assert.Equal(t, false, body["store"])
		if input, ok := body["input"]; ok {
			assert.IsType(t, []any{}, input, "SIWC requires list input")
		}
		// Suppress net/http's automatic content-type sniffing to reproduce the upstream.
		w.Header()["Content-Type"] = nil
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"local test passed\"}\n\n"+
			"event: response.completed\ndata: "+`{"type":"response.completed","response":{"id":"resp_local","object":"response","status":"completed","model":"gpt-6.1-terra","output":[],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}`+"\n\n")
	}))
	t.Cleanup(upstream.Close)
	client := service.GetHttpClient()
	previousTransport := client.Transport
	t.Cleanup(func() { client.Transport = previousTransport })
	client.Transport = siwcTestTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == "auth.openai.com" && request.URL.Path == "/api/accounts/oauth/token" {
			request = request.Clone(request.Context())
			request.URL, _ = url.Parse(upstream.URL + "/oauth/token")
			request.Host = request.URL.Host
		}
		return http.DefaultTransport.RoundTrip(request)
	})
	user := model.User{Id: 171717, Username: "siwc-local-test", Role: common.RoleRootUser, Status: common.UserStatusEnabled,
		Group: "default", Quota: 1000000, Setting: `{"accept_unset_model_ratio_model":true,"billing_preference":"wallet_only"}`}
	require.NoError(t, model.DB.Create(&user).Error)
	require.NoError(t, model.DB.Create(&model.Token{UserId: user.Id, Key: "siwclocalkey", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}).Error)
	credentials, err := common.Marshal(map[string]any{"client_id": fmt.Sprintf("issued-local-test-client-%d", channelID),
		"access_token": "old-access", "refresh_token": "old-refresh", "expires_at": time.Now().Add(time.Minute).Unix(), "ext_agent_host_id": "local-host"})
	require.NoError(t, err)
	channel := model.Channel{Id: channelID, Name: "siwc-test", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled,
		Key: string(credentials), BaseURL: &upstream.URL, Models: "gemini-3.7-flash", Group: "default",
		ModelMapping: common.GetPointer(`{"gemini-3.7-flash":"gpt-6.1-terra"}`), ParamOverride: common.GetPointer(`{"stream":false,"store":true}`)}
	require.NoError(t, model.DB.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(model.DB))
	model.InitChannelCache()
	engine := gin.New()
	SetRelayRouter(engine)
	SetTaskPluginProtocolRouter(engine)
	gateway := httptest.NewServer(engine)
	t.Cleanup(gateway.Close)
	for _, stage := range []string{"before refresh", "after refresh", "native Responses", "streaming Chat"} {
		t.Run(stage, func(t *testing.T) {
			path := "/v1/chat/completions"
			bodyJSON := `{"model":"gemini-3.7-flash","stream":false,"messages":[{"role":"user","content":"hello"}]}`
			if stage == "native Responses" {
				path = "/v1/responses"
				bodyJSON = `{"model":"gemini-3.7-flash","stream":false,"store":true,"input":"hello"}`
			} else if stage == "streaming Chat" {
				bodyJSON = strings.Replace(bodyJSON, `"stream":false`, `"stream":true`, 1)
			}
			request, err := http.NewRequest(http.MethodPost, gateway.URL+path, strings.NewReader(bodyJSON))
			require.NoError(t, err)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", fmt.Sprintf("Bearer sk-siwclocalkey-%d", channelID))
			response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
			require.NoError(t, err)
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, response.StatusCode, "%s", body)
			if stage == "streaming Chat" {
				assert.Contains(t, response.Header.Get("Content-Type"), "text/event-stream")
				assert.Contains(t, string(body), `"object":"chat.completion.chunk"`)
				return
			}
			assert.Contains(t, response.Header.Get("Content-Type"), "application/json")
			if stage == "native Responses" {
				var result dto.OpenAIResponsesResponse
				require.NoError(t, common.Unmarshal(body, &result))
				assert.Equal(t, "response", result.Object)
				assert.Equal(t, "local test passed", service.ExtractOutputTextFromResponses(&result))
				require.NotNil(t, result.Usage)
				assert.Equal(t, 5, result.Usage.TotalTokens)
				return
			}
			var result dto.OpenAITextResponse
			require.NoError(t, common.Unmarshal(body, &result))
			assert.Equal(t, "chat.completion", result.Object)
			require.Len(t, result.Choices, 1)
			assert.Equal(t, "local test passed", result.Choices[0].Message.StringContent())
			assert.Equal(t, 5, result.Usage.TotalTokens)
		})
		if stage == "before refresh" {
			service.RefreshSIWCCredentials(context.Background())
			cached, err := model.CacheGetChannel(channelID)
			require.NoError(t, err)
			credential, err := common.ParseSIWCCredential(cached.Key)
			require.NoError(t, err)
			require.NotNil(t, credential)
			assert.Equal(t, "new-access", credential.AccessToken)
			assert.Equal(t, "new-refresh", credential.RefreshToken)
			assert.Contains(t, cached.Key, `"ext_agent_host_id":"local-host"`)
			var audit model.AuditLog
			require.NoError(t, model.LOG_DB.Where("action = ?", "channel.oauth_refresh").First(&audit).Error)
			auditJSON, err := common.Marshal(audit)
			require.NoError(t, err)
			assert.NotContains(t, string(auditJSON), "new-access")
			assert.NotContains(t, string(auditJSON), "new-refresh")
		}
	}
	// The admin "test channel" button must reach upstream through Responses with stream:true and store:false.
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/channel/test/%d", channelID), nil)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprint(channelID)}}
	ctx.Set("id", user.Id)
	controller.TestChannel(ctx)
	assert.Contains(t, recorder.Body.String(), `"success":true`)
	assert.Equal(t, int32(5), calls.Load())
}

func TestListModelsSupportsOpenAIAndGeminiAuthentication(t *testing.T) {
	setupRelayRouterTestDB(t)

	user := model.User{
		Username: "models-user",
		Status:   common.UserStatusEnabled,
		Group:    "default",
		Quota:    100,
	}
	require.NoError(t, model.DB.Create(&user).Error)
	require.NoError(t, model.DB.Create(&model.Token{
		UserId:         user.Id,
		Key:            "modelstestkey",
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
	}).Error)

	engine := gin.New()
	SetRelayRouter(engine)

	tests := []struct {
		name           string
		path           string
		headerName     string
		expectedObject string
		expectedField  string
	}{
		{
			name:           "OpenAI bearer token",
			path:           "/v1/models",
			headerName:     "Authorization",
			expectedObject: "list",
			expectedField:  "data",
		},
		{
			name:          "Gemini API key header",
			path:          "/v1/models",
			headerName:    "x-goog-api-key",
			expectedField: "models",
		},
		{
			name:          "Gemini API key query",
			path:          "/v1/models?key=modelstestkey",
			expectedField: "models",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			if test.headerName != "" {
				value := "modelstestkey"
				if test.headerName == "Authorization" {
					value = "Bearer " + value
				}
				request.Header.Set(test.headerName, value)
			}

			engine.ServeHTTP(recorder, request)

			require.Equal(t, http.StatusOK, recorder.Code)
			var payload map[string]any
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
			assert.Contains(t, payload, test.expectedField)
			assert.NotContains(t, payload, "error")
			if test.expectedObject != "" {
				assert.Equal(t, test.expectedObject, payload["object"])
			}
		})
	}
}

func TestGeminiRelayHandsLongRunningPredictionToPluginRoute(t *testing.T) {
	setupRelayRouterTestDB(t)
	require.NoError(t, i18n.Init())
	user := model.User{Username: "veo-user", Status: common.UserStatusEnabled, Group: "default", Quota: 100}
	require.NoError(t, model.DB.Create(&user).Error)
	require.NoError(t, model.DB.Create(&model.Token{UserId: user.Id, Key: "veotestkey", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}).Error)

	engine := gin.New()
	SetRelayRouter(engine)
	source, err := plugins.Source("google")
	require.NoError(t, err)
	registry := jsplugin.NewRegistry()
	_, err = registry.RegisterFactory(source, jsplugin.Options{Key: "google"})
	require.NoError(t, err)
	// The plugin route authenticates as in production, then names itself.
	handlers := func(generation *jsplugin.RoutingGeneration, group jsplugin.RouteGroup) []gin.HandlerFunc {
		production := productionPluginRouteHandlers(generation, group)
		return []gin.HandlerFunc{production[0], production[1], production[2], func(c *gin.Context) {
			c.String(http.StatusOK, "plugin %s for user %d", group.Bindings[0].Plugin.Meta.Key, c.GetInt("id"))
		}}
	}
	require.NoError(t, registry.SetGenerationPreparer(newPluginGenerationBuilder(engine.Routes(), nil, handlers).prepare))
	require.Empty(t, registry.RoutingErrors())
	previousRegistry := defaultPluginDispatcher.registry
	defaultPluginDispatcher.registry = registry
	t.Cleanup(func() { defaultPluginDispatcher.registry = previousRegistry })
	engine.NoRoute(defaultPluginDispatcher.dispatch)

	for _, testCase := range []struct {
		method   string
		path     string
		toPlugin bool
	}{
		{method: http.MethodPost, path: "/v1beta/models/veo-3.1-generate-preview:predictLongRunning", toPlugin: true},
		{method: http.MethodGet, path: "/v1beta/models/veo-3.1-generate-preview/operations/task_sample", toPlugin: true},
		{method: http.MethodPost, path: "/v1beta/models/gemini-2.5-flash:generateContent"},
	} {
		t.Run(testCase.path, func(t *testing.T) {
			request := httptest.NewRequest(testCase.method, testCase.path, strings.NewReader(`{}`))
			request.Header.Set("x-goog-api-key", "veotestkey")
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, request)

			if testCase.toPlugin {
				assert.Equal(t, fmt.Sprintf("plugin google for user %d", user.Id), recorder.Body.String())
				return
			}
			// The relay looks for a Gemini channel serving the model; there is none.
			assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
			assert.Contains(t, recorder.Body.String(), `"code":"model_not_found"`)
		})
	}
}

func setupRelayRouterTestDB(t *testing.T) {
	t.Helper()

	gin.SetMode(gin.TestMode)
	originalIsMasterNode := common.IsMasterNode
	originalRedisEnabled := common.RedisEnabled
	originalSQLitePath := common.SQLitePath
	originalMainDatabaseType := common.MainDatabaseType()
	originalLogDatabaseType := common.LogDatabaseType()
	originalSQLDSN, hadSQLDSN := os.LookupEnv("SQL_DSN")

	common.IsMasterNode = false
	common.RedisEnabled = false
	common.SQLitePath = fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	require.NoError(t, os.Setenv("SQL_DSN", "local"))
	require.NoError(t, model.InitDB())
	model.LOG_DB = model.DB
	require.NoError(t, model.DB.AutoMigrate(&model.User{}, &model.Token{}, &model.Ability{}))

	t.Cleanup(func() {
		if sqlDB, err := model.DB.DB(); err == nil {
			_ = sqlDB.Close()
		}
		common.IsMasterNode = originalIsMasterNode
		common.RedisEnabled = originalRedisEnabled
		common.SQLitePath = originalSQLitePath
		common.SetDatabaseTypes(originalMainDatabaseType, originalLogDatabaseType)
		if hadSQLDSN {
			require.NoError(t, os.Setenv("SQL_DSN", originalSQLDSN))
		} else {
			require.NoError(t, os.Unsetenv("SQL_DSN"))
		}
	})
}
