package service

import (
	"slices"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func siwcFoldTestInfo(tokenName string) *relaycommon.RelayInfo {
	return &relaycommon.RelayInfo{TokenId: 7, UserId: 9, TokenName: tokenName, ChannelMeta: &relaycommon.ChannelMeta{IsSIWC: true, UpstreamModelName: "gpt-test"}}
}

func TestSIWCInstructionsFold(t *testing.T) {
	defaults := *model_setting.GetGlobalSettings()
	t.Cleanup(func() { *model_setting.GetGlobalSettings() = defaults })
	// The fold is off by default and never runs while prefix prewarm is enabled.
	model_setting.GetGlobalSettings().SIWCPrefixPrewarm.Enabled = false
	previous := defaults.SIWCInstructionsFold
	previous.Enabled = true

	const forecastInput = `[{"role":"user","content":"PREGAME"},{"role":"assistant","content":"Acknowledged."},{ "role":"user", "content":"seg 1" },{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a1"}]},{"role":"user","content":"last"}]`
	const forecastRest = `[{ "role":"user", "content":"seg 1" },{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a1"}]},{"role":"user","content":"last"}]`
	const c15Input = `[{"role":"user","content":"brief"},{"role":"user","content":"Q1"},{"role":"user","content":"Q2"},{"role":"user","content":"Q3"},{"role":"user","content":"Now"}]`

	for _, tc := range []struct {
		name, token  string
		cacheKey     string // raw JSON, "" = absent
		instructions string // raw JSON, "" = absent
		input        string
		mutate       func(*model_setting.SIWCInstructionsFold)
		// wantInstructions "" means no fold: instructions keep their raw bytes.
		wantInstructions string
		wantInput        string // "" = input bytes unchanged
	}{
		{
			name: "forecast folds through first assistant", token: "forecast",
			instructions: `"SYSTEM RULES"`, input: forecastInput,
			wantInstructions: "SYSTEM RULES\n\n## USER\nPREGAME\n\n## ASSISTANT\nAcknowledged.",
			wantInput:        forecastRest,
		},
		{
			name: "chat converted text parts", token: "forecast",
			input:            `[{"role":"user","content":[{"type":"input_text","text":"PRE"},{"type":"input_text","text":"GAME"}]},{"role":"assistant","content":[{"type":"output_text","text":"Acknowledged."}]},{"role":"user","content":[{"type":"input_text","text":"last"}]}]`,
			wantInstructions: "## USER\nPRE\n\nGAME\n\n## ASSISTANT\nAcknowledged.",
			wantInput:        `[{"role":"user","content":[{"type":"input_text","text":"last"}]}]`,
		},
		{
			name: "c15 keep_last", token: "tape-dev", cacheKey: `"game42:c15_forecast"`, input: c15Input,
			wantInstructions: "## USER\nbrief\n\n## USER\nQ1\n\n## USER\nQ2",
			wantInput:        `[{"role":"user","content":"Q3"},{"role":"user","content":"Now"}]`,
		},
		{
			name: "explicit breakpoint beats rule", token: "forecast",
			input:            `[{"role":"user","content":"PREGAME"},{"role":"assistant","content":"Acknowledged."},{"role":"user","content":[{"type":"input_text","text":"seg 1","prompt_cache_breakpoint":{"mode":"explicit"}}]},{"role":"assistant","content":"a1"},{"role":"user","content":[{"type":"input_text","text":"last","prompt_cache_breakpoint":{"mode":"explicit"}}]}]`,
			wantInstructions: "## USER\nPREGAME\n\n## ASSISTANT\nAcknowledged.\n\n## USER\nseg 1",
			wantInput:        `[{"role":"assistant","content":"a1"},{"role":"user","content":[{"type":"input_text","text":"last"}]}]`,
		},
		{
			name: "explicit breakpoint without rule, instruction parts", token: "other",
			instructions:     `[{"type":"input_text","text":"pre","prompt_cache_breakpoint":{"mode":"explicit"}},{"type":"input_text","text":"fix"}]`,
			input:            `[{"role":"developer","content":[{"type":"input_text","text":"rules","prompt_cache_breakpoint":null}]},{"role":"user","content":"context"},{"role":"user","content":"question"}]`,
			wantInstructions: "prefix\n\n## DEVELOPER\nrules",
			wantInput:        `[{"role":"user","content":"context"},{"role":"user","content":"question"}]`,
		},
		{
			name: "marker only on last user", token: "other",
			input:     `[{"role":"developer","content":"rules"},{"role":"user","content":[{"type":"input_text","text":"question","prompt_cache_breakpoint":{"mode":"explicit"}}]}]`,
			wantInput: `[{"role":"developer","content":"rules"},{"role":"user","content":[{"type":"input_text","text":"question"}]}]`,
		},
		{name: "non-matching token", token: "other", instructions: `"SYSTEM RULES"`, input: forecastInput},
		{name: "cache key pattern without key", token: "tape-dev", input: c15Input},
		{name: "cache key pattern with other key", token: "tape-dev", cacheKey: `"game42:c15_recap"`, input: c15Input},
		{name: "keep_last never folds everything", token: "tape-dev", cacheKey: `"g:c15_forecast"`, input: `[{"role":"user","content":"brief"},{"role":"user","content":"Now"}]`},
		{name: "no assistant after first user", token: "forecast", input: `[{"role":"assistant","content":"hi"},{"role":"user","content":"a"},{"role":"user","content":"b"}]`},
		{
			name: "image in folded range", token: "forecast",
			input: `[{"role":"user","content":[{"type":"input_text","text":"PREGAME"},{"type":"input_image","image_url":"data:image/png;base64,AA=="}]},{"role":"assistant","content":"Acknowledged."},{"role":"user","content":"last"}]`,
		},
		{
			name: "non-message item in folded range", token: "tape-dev", cacheKey: `"g:c15_forecast"`,
			input: `[{"role":"user","content":"brief"},{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},{"role":"user","content":"Q3"},{"role":"user","content":"Now"}]`,
		},
		{
			name: "non-text instructions", token: "forecast",
			instructions: `[{"type":"input_image","image_url":"data:image/png;base64,AA=="}]`, input: forecastInput,
		},
		{
			name: "disabled", token: "forecast", input: forecastInput,
			mutate: func(s *model_setting.SIWCInstructionsFold) { s.Enabled = false },
		},
		{
			name: "prefix prewarm enabled", token: "forecast", input: forecastInput,
			mutate: func(*model_setting.SIWCInstructionsFold) {
				model_setting.GetGlobalSettings().SIWCPrefixPrewarm.Enabled = true
			},
		},
		{
			name: "explicit breakpoints not honored", token: "other",
			input:     `[{"role":"developer","content":[{"type":"input_text","text":"rules","prompt_cache_breakpoint":{"mode":"explicit"}}]},{"role":"user","content":"question"}]`,
			mutate:    func(s *model_setting.SIWCInstructionsFold) { s.HonorExplicitBreakpoints = false },
			wantInput: `[{"role":"developer","content":[{"type":"input_text","text":"rules"}]},{"role":"user","content":"question"}]`,
		},
		{
			name: "rule without criteria never matches", token: "forecast", input: forecastInput,
			mutate: func(s *model_setting.SIWCInstructionsFold) {
				s.Rules = []model_setting.SIWCInstructionsFoldRule{{Name: "all", Boundary: "keep_last", KeepLast: 1}}
			},
		},
		{
			name: "invalid regex never matches", token: "forecast", input: forecastInput,
			mutate: func(s *model_setting.SIWCInstructionsFold) {
				s.Rules = []model_setting.SIWCInstructionsFoldRule{{Name: "bad", TokenNames: []string{"forecast"}, ModelPatterns: []string{"["}, Boundary: "first_assistant"}}
			},
		},
		{
			name: "model pattern rule", token: "other", input: forecastInput,
			mutate: func(s *model_setting.SIWCInstructionsFold) {
				s.Rules = []model_setting.SIWCInstructionsFoldRule{{Name: "by-model", ModelPatterns: []string{"^gpt-test$"}, Boundary: "first_assistant"}}
			},
			wantInstructions: "## USER\nPREGAME\n\n## ASSISTANT\nAcknowledged.",
			wantInput:        forecastRest,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setting := previous
			if tc.mutate != nil {
				tc.mutate(&setting)
			}
			model_setting.GetGlobalSettings().SIWCInstructionsFold = setting
			t.Cleanup(func() { model_setting.GetGlobalSettings().SIWCPrefixPrewarm.Enabled = false })

			body := `{"stream":true,"store":false`
			if tc.cacheKey != "" {
				body += `,"prompt_cache_key":` + tc.cacheKey
			}
			if tc.instructions != "" {
				body += `,"instructions":` + tc.instructions
			}
			body += `,"input":` + tc.input + `}`
			got, err := ApplySIWCResponsesRequirements(siwcFoldTestInfo(tc.token), []byte(body))
			require.NoError(t, err)
			require.True(t, gjson.ValidBytes(got))

			wantInput := tc.wantInput
			if wantInput == "" {
				wantInput = tc.input
			}
			assert.Equal(t, wantInput, gjson.GetBytes(got, "input").Raw)
			instructions := gjson.GetBytes(got, "instructions")
			if tc.wantInstructions == "" {
				assert.Equal(t, tc.instructions, instructions.Raw)
				return
			}
			assert.Equal(t, gjson.String, instructions.Type)
			assert.Equal(t, tc.wantInstructions, instructions.String())
			assert.NotContains(t, string(got), "prompt_cache_breakpoint")
		})
	}

	t.Run("same prefix gives identical instructions bytes", func(t *testing.T) {
		model_setting.GetGlobalSettings().SIWCInstructionsFold = previous
		const prefix = `{"instructions":"SYSTEM <RULES> & \"quotes\"","input":[{"role":"user","content":"PREGAME é\n<tag>"},{"role":"assistant","content":"Acknowledged."},`
		first, err := ApplySIWCResponsesRequirements(siwcFoldTestInfo("forecast"), []byte(prefix+`{"role":"user","content":"seg 1"}]}`))
		require.NoError(t, err)
		second, err := ApplySIWCResponsesRequirements(siwcFoldTestInfo("forecast"), []byte(prefix+`{"role":"user","content":"seg 1"},{"role":"assistant","content":"a1"},{"role":"user","content":"seg 2"}]}`))
		require.NoError(t, err)
		require.Contains(t, gjson.GetBytes(first, "instructions").String(), "## ASSISTANT\nAcknowledged.")
		assert.Equal(t, gjson.GetBytes(first, "instructions").Raw, gjson.GetBytes(second, "instructions").Raw)
		// The fold also gives both requests the same generated cache session.
		assert.Equal(t, gjson.GetBytes(first, "prompt_cache_key").String(), gjson.GetBytes(second, "prompt_cache_key").String())
		// The session seed reads the caller's messages, so turning the fold off keeps the key.
		model_setting.GetGlobalSettings().SIWCInstructionsFold.Enabled = false
		unfolded, err := ApplySIWCResponsesRequirements(siwcFoldTestInfo("forecast"), []byte(prefix+`{"role":"user","content":"seg 1"}]}`))
		require.NoError(t, err)
		assert.Equal(t, gjson.GetBytes(first, "prompt_cache_key").String(), gjson.GetBytes(unfolded, "prompt_cache_key").String())
	})

	t.Run("non-SIWC body is untouched", func(t *testing.T) {
		model_setting.GetGlobalSettings().SIWCInstructionsFold = previous
		body := []byte(`{"instructions":"SYSTEM RULES","input":` + forecastInput + `}`)
		info := siwcFoldTestInfo("forecast")
		info.IsSIWC = false
		got, err := ApplySIWCResponsesRequirements(info, body)
		require.NoError(t, err)
		assert.Equal(t, string(body), string(got))
	})

	t.Run("stored override replaces default rules", func(t *testing.T) {
		settings := model_setting.GlobalSettings{SIWCInstructionsFold: previous}
		settings.SIWCInstructionsFold.Rules = slices.Clone(previous.Rules)
		require.NoError(t, config.UpdateConfigFromMap(&settings, map[string]string{
			"siwc_instructions_fold": `{"enabled":true,"rules":[{"name":"by-model","model_patterns":["^gpt-"],"boundary":"keep_last","keep_last":1}]}`,
		}))
		assert.Equal(t, model_setting.SIWCInstructionsFold{
			Enabled: true, HonorExplicitBreakpoints: true,
			Rules: []model_setting.SIWCInstructionsFoldRule{{Name: "by-model", ModelPatterns: []string{"^gpt-"}, Boundary: "keep_last", KeepLast: 1}},
		}, settings.SIWCInstructionsFold)

		// Cleared rules survive an export/import round trip instead of reverting to the defaults.
		require.NoError(t, config.UpdateConfigFromMap(&settings, map[string]string{"siwc_instructions_fold": `{"rules":[]}`}))
		exported, err := config.ConfigToMap(&settings)
		require.NoError(t, err)
		reloaded := model_setting.GlobalSettings{SIWCInstructionsFold: previous}
		require.NoError(t, config.UpdateConfigFromMap(&reloaded, map[string]string{"siwc_instructions_fold": exported["siwc_instructions_fold"]}))
		assert.Empty(t, reloaded.SIWCInstructionsFold.Rules)
		assert.Len(t, previous.Rules, 2) // the shared defaults were not mutated in place
	})
}
