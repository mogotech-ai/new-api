package controller

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
