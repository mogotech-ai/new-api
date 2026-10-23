package router

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHostProtocolRegistryDrivesProtocolRoutesOnce(t *testing.T) {
	engine := gin.New()
	SetTaskPluginProtocolRouter(engine)

	expected := []string{
		"POST /v1/responses",
		"GET /v1/responses/:response_id",
		"POST /v1/videos",
		"GET /v1/videos/:task_id",
		"GET /v1/videos/:task_id/content",
		"HEAD /v1/videos/:task_id/content",
		"POST /v1/images/generations",
		"POST /v1/images/edits",
	}
	actual := make([]string, 0, len(engine.Routes()))
	for _, route := range engine.Routes() {
		actual = append(actual, fmt.Sprintf("%s %s", route.Method, route.Path))
	}
	sort.Strings(expected)
	sort.Strings(actual)
	assert.Equal(t, expected, actual)
}

func TestNativeResponsesPayloadLogging(t *testing.T) {
	setupRelayRouterTestDB(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.RelayPayload{}))
	previousCache, previousLogging := common.MemoryCacheEnabled, common.LogConsumeEnabled
	previousRateLimit := setting.ModelRequestRateLimitEnabled
	common.MemoryCacheEnabled = false
	setting.ModelRequestRateLimitEnabled = false
	t.Cleanup(func() {
		common.MemoryCacheEnabled, common.LogConsumeEnabled = previousCache, previousLogging
		setting.ModelRequestRateLimitEnabled = previousRateLimit
	})
	user := &model.User{Username: "responses-payload-user", Status: common.UserStatusEnabled, Group: "default", Quota: 100}
	require.NoError(t, model.DB.Create(user).Error)
	require.NoError(t, model.DB.Create(&model.Token{
		UserId: user.Id, Key: "responsespayload", Status: common.TokenStatusEnabled,
		ExpiredTime: -1, UnlimitedQuota: true,
	}).Error)
	channel := &model.Channel{
		Type: constant.ChannelTypeOpenAI, Key: "upstream-test-key", Name: "responses payload",
		Status: common.ChannelStatusEnabled, Models: "responses-payload-model", Group: "default",
	}
	require.NoError(t, channel.Insert())

	for _, tc := range []struct {
		name                          string
		stream, channelLog, log, auth bool
	}{
		{name: "JSON", channelLog: true, log: true, auth: true},
		{name: "SSE", stream: true, channelLog: true, log: true, auth: true},
		{name: "channel logging disabled", log: true, auth: true},
		{name: "consume logging disabled", channelLog: true, auth: true},
		{name: "unauthenticated", channelLog: true, log: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			common.LogConsumeEnabled = tc.log
			channel.SetSetting(dto.ChannelSettings{LogRequestResponseEnabled: tc.channelLog})
			require.NoError(t, model.DB.Model(channel).Update("setting", channel.Setting).Error)
			requestBody := fmt.Sprintf(`{"model":"responses-payload-model","input":"hello","stream":%t}`, tc.stream)
			contentType, responseBody := "application/json", `{"id":"resp_test","object":"response","status":"completed"}`
			if tc.stream {
				contentType = "text/event-stream"
				responseBody = "event: response.completed\ndata: " + responseBody + "\n\n"
			}
			handlers, err := taskPluginProtocolHandlers("openai_responses", "create")
			require.NoError(t, err)
			// Keep the production middleware chain and isolate capture from upstream relay and billing.
			handlers[len(handlers)-1] = func(c *gin.Context) {
				c.Header("Content-Type", contentType)
				_, writeErr := c.Writer.WriteString(responseBody)
				assert.NoError(t, writeErr)
				if tc.stream {
					c.Writer.Flush()
				}
			}
			engine := gin.New()
			engine.Use(middleware.RequestId(), middleware.BodyStorageCleanup())
			engine.POST("/v1/responses", handlers...)
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(requestBody))
			request.Header.Set("Content-Type", "application/json")
			if tc.auth {
				request.Header.Set("Authorization", "Bearer responsespayload")
			}
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, request)
			requestID := recorder.Header().Get(common.RequestIdKey)
			require.NotEmpty(t, requestID)
			if tc.auth {
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				assert.Equal(t, responseBody, recorder.Body.String())
				assert.Equal(t, contentType, recorder.Header().Get("Content-Type"))
				assert.Equal(t, tc.stream, recorder.Flushed)
			} else {
				assert.Equal(t, http.StatusUnauthorized, recorder.Code)
			}
			var payloads []model.RelayPayload
			require.NoError(t, model.DB.Where("request_id = ?", requestID).Find(&payloads).Error)
			if !tc.auth || !tc.channelLog || !tc.log {
				assert.Empty(t, payloads)
				return
			}
			require.Len(t, payloads, 1)
			assert.Equal(t, channel.Id, payloads[0].ChannelId)
			assert.Equal(t, requestBody, string(payloads[0].RequestBody))
			assert.Equal(t, responseBody, string(payloads[0].ResponseBody))
			assert.Equal(t, "application/json", payloads[0].RequestContentType)
			assert.Equal(t, contentType, payloads[0].ResponseContentType)
		})
	}
}
