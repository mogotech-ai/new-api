package service

import (
	"testing"

	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyPromptCacheAutoBreakpoints(t *testing.T) {
	previous := model_setting.GetGlobalSettings().PromptCacheAutoBreakpoint
	t.Cleanup(func() { model_setting.GetGlobalSettings().PromptCacheAutoBreakpoint = previous })

	const bp = `,"prompt_cache_breakpoint":{"mode":"explicit"}`
	const forecast = `{"model":"m","instructions":"rules","input":[
		{"role":"user","content":[{"type":"input_text","text":"PREGAME"}]},
		{"role":"assistant","content":[{"type":"output_text","text":"Acknowledged."}]},
		{"role":"user","content":[{"type":"input_text","text":"dynamic"}]}]}`
	const forecastWant = `{"model":"m","instructions":"rules","input":[
		{"role":"user","content":[{"type":"input_text","text":"PREGAME"` + bp + `}]},
		{"role":"assistant","content":[{"type":"output_text","text":"Acknowledged."}]},
		{"role":"user","content":[{"type":"input_text","text":"dynamic"}]}]}`

	for _, tc := range []struct {
		name, model, body, want string // empty want: bytes unchanged
		channelType             int
		siwc, disabled, optOut  bool
		patterns                []string
	}{
		{name: "forecast shape", body: forecast, want: forecastWant},
		{name: "gpt-5.6", model: "gpt-5.6", body: forecast, want: forecastWant},
		{name: "gpt-5.10", model: "gpt-5.10", body: forecast, want: forecastWant},
		{name: "gpt-6", model: "gpt-6", body: forecast, want: forecastWant},
		{name: "gpt-6-sol", model: "gpt-6-sol", body: forecast, want: forecastWant},
		{name: "gpt-6.1-terra", model: "gpt-6.1-terra", body: forecast, want: forecastWant},
		{name: "gpt-6.1-sol", model: "gpt-6.1-sol", body: forecast, want: forecastWant},
		{name: "gpt-7", model: "gpt-7", body: forecast, want: forecastWant},
		{name: "gpt-10", model: "gpt-10", body: forecast, want: forecastWant},
		{name: "invalid pattern ignored", patterns: []string{"(", "^gpt-5\\.6"}, body: forecast, want: forecastWant},
		{
			name: "C15 shape keeps the three most recent",
			body: `{"input":[
				{"role":"user","content":[{"type":"input_text","text":"head"}]},
				{"role":"user","content":[{"type":"input_text","text":"P1"}]},
				{"role":"user","content":[{"type":"input_text","text":"P2"}]},
				{"type":"message","role":"user","content":[{"type":"input_text","text":"P3"}]},
				{"role":"user","content":[{"type":"input_text","text":"current"}]},
				{"role":"user","content":[{"type":"input_text","text":"now"}]}]}`,
			want: `{"input":[
				{"role":"user","content":[{"type":"input_text","text":"head"}]},
				{"role":"user","content":[{"type":"input_text","text":"P1"}]},
				{"role":"user","content":[{"type":"input_text","text":"P2"` + bp + `}]},
				{"type":"message","role":"user","content":[{"type":"input_text","text":"P3"` + bp + `}]},
				{"role":"user","content":[{"type":"input_text","text":"current"` + bp + `}]},
				{"role":"user","content":[{"type":"input_text","text":"now"}]}]}`,
		},
		{
			name: "string content converted, non-message items and image-last skipped",
			body: `{"input":[
				{"role":"developer","content":"rules"},
				{"role":"user","content":[{"type":"input_text","text":"look"},{"type":"input_image","image_url":"u"}]},
				{"type":"function_call","call_id":"c","name":"f","arguments":"{}"},
				{"type":"function_call_output","call_id":"c","output":"o"},
				{"type":"reasoning","summary":[]},
				{"role":"user","content":[{"type":"input_image","image_url":"u"},{"type":"input_text","text":"tail"}]},
				{"role":"user","content":"now"},
				{"role":"assistant","content":"trailing"}]}`,
			want: `{"input":[
				{"role":"developer","content":[{"type":"input_text","text":"rules"` + bp + `}]},
				{"role":"user","content":[{"type":"input_text","text":"look"},{"type":"input_image","image_url":"u"}]},
				{"type":"function_call","call_id":"c","name":"f","arguments":"{}"},
				{"type":"function_call_output","call_id":"c","output":"o"},
				{"type":"reasoning","summary":[]},
				{"role":"user","content":[{"type":"input_image","image_url":"u"},{"type":"input_text","text":"tail"` + bp + `}]},
				{"role":"user","content":"now"},
				{"role":"assistant","content":"trailing"}]}`,
		},
		{name: "only image-last candidate", body: `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"u"}]},{"role":"user","content":"now"}]}`},
		{name: "caller breakpoint present", body: `{"input":[{"role":"user","content":"a"},{"role":"user","content":"b"},{"role":"user","content":[{"type":"input_text","text":"now","prompt_cache_breakpoint":{"mode":"explicit"}}]}]}`},
		{name: "caller explicit mode", body: `{"prompt_cache_options":{"mode":"explicit"},"input":[{"role":"user","content":"a"},{"role":"user","content":"now"}]}`},
		{name: "string input", body: `{"input":"hello"}`},
		{name: "SIWC", siwc: true, body: forecast},
		{name: "non-OpenAI channel type", channelType: constant.ChannelTypeAzure, body: forecast},
		{name: "gpt-5.5", model: "gpt-5.5", body: forecast},
		{name: "gpt-5", model: "gpt-5", body: forecast},
		{name: "gpt-5-mini", model: "gpt-5-mini", body: forecast},
		{name: "gpt-4.1", model: "gpt-4.1", body: forecast},
		{name: "gpt-35-turbo", model: "gpt-35-turbo", body: forecast},
		{name: "gpt-35-turbo-16k", model: "gpt-35-turbo-16k", body: forecast},
		{name: "empty model patterns", patterns: []string{}, body: forecast},
		{name: "gpt-4o", model: "gpt-4o", body: forecast},
		{name: "o3", model: "o3", body: forecast},
		{name: "disabled globally", disabled: true, body: forecast},
		{name: "channel opt-out", optOut: true, body: forecast},
		{name: "no user message", body: `{"input":[{"role":"developer","content":"rules"},{"role":"assistant","content":"hi"}]}`},
		{name: "single user message", body: `{"instructions":"rules","input":[{"role":"user","content":"hello"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setting := model_setting.PromptCacheAutoBreakpoint{Enabled: !tc.disabled, ModelPatterns: previous.ModelPatterns}
			if tc.patterns != nil {
				setting.ModelPatterns = tc.patterns
			}
			model_setting.GetGlobalSettings().PromptCacheAutoBreakpoint = setting
			channelType := constant.ChannelTypeOpenAI
			if tc.channelType != 0 {
				channelType = tc.channelType
			}
			model := "gpt-5.6-luna"
			if tc.model != "" {
				model = tc.model
			}
			info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{
				ChannelType: channelType, IsSIWC: tc.siwc, UpstreamModelName: model,
				ChannelOtherSettings: dto.ChannelOtherSettings{DisablePromptCacheAutoBreakpoint: tc.optOut},
			}}
			body := []byte(tc.body)
			got, err := ApplyPromptCacheAutoBreakpoints(info, body)
			require.NoError(t, err)
			if tc.want == "" {
				assert.Equal(t, tc.body, string(got))
				return
			}
			assert.JSONEq(t, tc.want, string(got))
		})
	}
}
