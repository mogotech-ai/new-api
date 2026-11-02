package router

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSIWCNonStreamingChatAndCredentialRotation(t *testing.T) {
	setupRelayRouterTestDB(t)
	require.NoError(t, i18n.Init())
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Log{}, &model.AuditLog{}, &model.Option{}, &model.RelayPayload{}))
	previousCache, previousLog, previousCount := common.MemoryCacheEnabled, common.LogConsumeEnabled, constant.CountToken
	settings := model_setting.GetGlobalSettings()
	previousPolicy, previousPassThrough := settings.ChatCompletionsToResponsesPolicy, settings.PassThroughRequestEnabled
	common.MemoryCacheEnabled, common.LogConsumeEnabled, constant.CountToken = true, false, false
	settings.PassThroughRequestEnabled = false
	settings.ChatCompletionsToResponsesPolicy = model_setting.ChatCompletionsToResponsesPolicy{
		Enabled: true, ChannelTypes: []int{constant.ChannelTypeOpenAI}, ModelPatterns: []string{"^gpt-6.1-terra$"},
	}
	t.Cleanup(func() {
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
			assert.Equal(t, "issued-local-test-client", r.Form.Get("client_id"))
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
		// Suppress net/http's automatic content-type sniffing to reproduce the upstream.
		w.Header()["Content-Type"] = nil
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"local test passed\"}\n\n"+
			"event: response.completed\ndata: "+`{"type":"response.completed","response":{"id":"resp_local","status":"completed","model":"gpt-6.1-terra","output":[],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}`+"\n\n")
	}))
	t.Cleanup(upstream.Close)
	user := model.User{Id: 171717, Username: "siwc-local-test", Role: common.RoleRootUser, Status: common.UserStatusEnabled,
		Group: "default", Quota: 1000000, Setting: `{"accept_unset_model_ratio_model":true,"billing_preference":"wallet_only"}`}
	require.NoError(t, model.DB.Create(&user).Error)
	require.NoError(t, model.DB.Create(&model.Token{UserId: user.Id, Key: "siwclocalkey", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}).Error)
	channel := model.Channel{Id: 17, Name: "codexapi", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled,
		Key: "old-access", BaseURL: &upstream.URL, Models: "gemini-3.7-flash", Group: "default",
		ModelMapping: common.GetPointer(`{"gemini-3.7-flash":"gpt-6.1-terra"}`), ParamOverride: common.GetPointer(`{"stream":true,"store":false}`)}
	require.NoError(t, model.DB.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(model.DB))
	model.InitChannelCache()
	engine := gin.New()
	// Inject only the management identity; relay requests use real TokenAuth and channel selection.
	managementIdentity := func(c *gin.Context) {
		if c.GetHeader("Authorization") != "Bearer local-management-token" {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		c.Set("id", user.Id)
		c.Set("role", common.RoleRootUser)
	}
	engine.GET("/api/channel/:id", managementIdentity, controller.GetChannel)
	engine.PUT("/api/channel/", managementIdentity, controller.UpdateChannel)
	SetRelayRouter(engine)
	gateway := httptest.NewServer(engine)
	t.Cleanup(gateway.Close)
	for _, stage := range []string{"before refresh", "after refresh"} {
		t.Run(stage, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/chat/completions", strings.NewReader(`{"model":"gemini-3.7-flash","stream":false,"messages":[{"role":"user","content":"hello"}]}`))
			require.NoError(t, err)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer sk-siwclocalkey-17")
			response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
			require.NoError(t, err)
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, response.StatusCode, "%s", body)
			assert.Contains(t, response.Header.Get("Content-Type"), "application/json")
			var result dto.OpenAITextResponse
			require.NoError(t, common.Unmarshal(body, &result))
			assert.Equal(t, "chat.completion", result.Object)
			require.Len(t, result.Choices, 1)
			assert.Equal(t, "local test passed", result.Choices[0].Message.StringContent())
			assert.Equal(t, 5, result.Usage.TotalTokens)
		})
		if stage == "before refresh" {
			stateDirectory := t.TempDir()
			require.NoError(t, os.Chmod(stateDirectory, 0o700))
			statePath := filepath.Join(stateDirectory, "credentials.json")
			state, err := common.Marshal(map[string]any{"client_id": "issued-local-test-client", "access_token": "old-access", "refresh_token": "old-refresh",
				"expires_at": 1, "gateway_base_url": gateway.URL, "gateway_admin_token": "local-management-token"})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(statePath, state, 0o600))
			binPath, err := filepath.Abs("../bin")
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, "python3", "-c", "import sys; sys.path.insert(0, sys.argv[3]); import siwc_refresh; siwc_refresh.TOKEN_URL=sys.argv[2]; siwc_refresh.refresh_once(sys.argv[1])", statePath, upstream.URL+"/oauth/token", binPath)
			output, err := command.CombinedOutput()
			require.NoError(t, err, "%s", output)
			saved, err := os.ReadFile(statePath)
			require.NoError(t, err)
			var credentials map[string]any
			require.NoError(t, common.Unmarshal(saved, &credentials))
			assert.Equal(t, "new-refresh", credentials["refresh_token"])
			assert.Equal(t, false, credentials["gateway_sync_pending"])
			cached, err := model.CacheGetChannel(17)
			require.NoError(t, err)
			assert.Equal(t, "new-access", cached.Key)
			var audit model.AuditLog
			require.NoError(t, model.LOG_DB.Where("action = ?", "channel.update").First(&audit).Error)
			auditJSON, err := common.Marshal(audit)
			require.NoError(t, err)
			assert.NotContains(t, string(auditJSON), "new-access")
			assert.NotContains(t, string(auditJSON), "new-refresh")
		}
	}
	assert.Equal(t, int32(2), calls.Load())
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
