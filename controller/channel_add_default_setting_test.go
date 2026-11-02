package controller

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSIWCChannelHasOneSessionOwner(t *testing.T) {
	setupTaskPluginBindChannelTest(t)
	credential := `{"client_id":"issued-client","access_token":"access","refresh_token":"refresh","expires_at":1800000000}`
	for _, mode := range []string{"batch", "multi_to_single", "single"} {
		body, err := common.Marshal(map[string]any{"mode": mode, "channel": map[string]any{
			"type": 1, "name": "SIWC", "key": credential, "models": "model", "group": "default",
		}})
		require.NoError(t, err)
		response := postAddChannel(t, 1, common.RoleRootUser, string(body))
		if mode == "single" {
			require.Contains(t, response.Body.String(), `"success":true`)
			duplicate := postAddChannel(t, 1, common.RoleRootUser, string(body))
			assert.Contains(t, duplicate.Body.String(), "this SIWC session is already managed by another channel")
		} else {
			assert.Contains(t, response.Body.String(), "SIWC credentials require single-key mode")
		}
	}
	var channels []model.Channel
	require.NoError(t, model.DB.Find(&channels).Error)
	require.Len(t, channels, 1)
	assert.Equal(t, credential, channels[0].Key)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprint(channels[0].Id)}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/copy", nil)
	CopyChannel(ctx)
	assert.Contains(t, recorder.Body.String(), "this SIWC session is already managed by another channel")
}

func TestSIWCUpdateCannotImportIntoExistingMultiKeyChannel(t *testing.T) {
	setupTaskPluginBindChannelTest(t)
	channel := model.Channel{Type: 1, Name: "multi-key", Key: "key-one\nkey-two", Models: "model", Group: "default", ChannelInfo: model.ChannelInfo{IsMultiKey: true}}
	require.NoError(t, model.DB.Create(&channel).Error)
	for _, withType := range []bool{false, true} {
		payload := map[string]any{"id": channel.Id, "name": channel.Name, "models": channel.Models, "group": channel.Group,
			"key": `{"client_id":"issued-client","access_token":"access","refresh_token":"refresh","expires_at":1800000000}`}
		if withType {
			payload["type"] = 1
		}
		body, err := common.Marshal(payload)
		require.NoError(t, err)
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Set("id", 1)
		ctx.Set("role", common.RoleRootUser)
		ctx.Request = httptest.NewRequest(http.MethodPut, "/api/channel", bytes.NewReader(body))
		UpdateChannel(ctx)
		assert.Contains(t, recorder.Body.String(), "SIWC credentials require single-key mode")
		stored, err := model.GetChannelById(channel.Id, true)
		require.NoError(t, err)
		assert.Equal(t, channel.Key, stored.Key)
	}
}

func TestAddChannelDefaultSetting(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setting any
		mode    string
		key     string
		count   int
		enabled bool
	}{
		{"omitted", nil, "single", "sk", 1, true},
		{"null", nil, "single", "sk", 1, true},
		{"empty", "", "single", "sk", 1, true},
		{"whitespace", " \t\n", "single", "sk", 1, true},
		{"JSON null", "null", "single", "sk", 1, true},
		{"missing key", `{"proxy":"","force_format":true,"unknown":9007199254740993}`, "single", "sk", 1, true},
		{"explicit false", `{"log_request_response_enabled":false}`, "single", "sk", 1, false},
		{"explicit true", `{"log_request_response_enabled":true}`, "single", "sk", 1, true},
		{"batch", nil, "batch", "sk-one\nsk-two", 2, true},
		{"multi_to_single", nil, "multi_to_single", "sk-one\nsk-two", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupTaskPluginBindChannelTest(t)
			channel := map[string]any{"type": 1, "name": "openai-channel", "key": tc.key, "models": "gpt", "group": "default"}
			if tc.setting != nil || tc.name == "null" {
				channel["setting"] = tc.setting
			}
			body, err := common.Marshal(map[string]any{"mode": tc.mode, "channel": channel})
			require.NoError(t, err)
			response := postAddChannel(t, 1, common.RoleRootUser, string(body))
			require.Contains(t, response.Body.String(), `"success":true`)
			var channels []model.Channel
			require.NoError(t, model.DB.Find(&channels).Error)
			require.Len(t, channels, tc.count)
			for _, stored := range channels {
				require.NotNil(t, stored.Setting)
				var settings map[string]common.RawMessage
				require.NoError(t, common.Unmarshal([]byte(*stored.Setting), &settings))
				expected, err := common.Marshal(tc.enabled)
				require.NoError(t, err)
				assert.Equal(t, common.RawMessage(expected), settings["log_request_response_enabled"])
				if tc.name == "missing key" {
					assert.Equal(t, common.RawMessage("true"), settings["force_format"])
					assert.Equal(t, common.RawMessage(`""`), settings["proxy"])
					assert.Equal(t, common.RawMessage("9007199254740993"), settings["unknown"])
				}
			}
		})
	}
}

func TestNewChannelSettingInvalidJSONUnchanged(t *testing.T) {
	for _, setting := range []string{`{"broken":`, `[]`, `"text"`, `42`, `true`} {
		t.Run(setting, func(t *testing.T) {
			original := setting
			channel := &model.Channel{Setting: &setting}
			applyNewChannelSettingDefaults(channel)
			require.NotNil(t, channel.Setting)
			assert.Equal(t, original, *channel.Setting)
		})
	}
}
