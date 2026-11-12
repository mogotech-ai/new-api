package service

import (
	"fmt"

	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const promptCacheAutoBreakpointLimit = 3 // 4 writes per request, one is the implicit breakpoint.

// ApplyPromptCacheAutoBreakpoints marks the stable messages before the latest
// user message of an official OpenAI Responses body as explicit cache write
// points, so a prompt whose only changing part is the last user message stops
// rewriting the whole prefix on every call. Caller-chosen breakpoints are never
// overridden. prompt_cache_options is left alone on purpose: explicit-only mode
// would disable the implicit breakpoint that append-only agents rely on.
func ApplyPromptCacheAutoBreakpoints(info *relaycommon.RelayInfo, body []byte) ([]byte, error) {
	if info == nil || info.ChannelMeta == nil || info.IsSIWC || info.ChannelType != constant.ChannelTypeOpenAI ||
		info.ChannelOtherSettings.DisablePromptCacheAutoBreakpoint {
		return body, nil
	}
	setting := model_setting.GetGlobalSettings().PromptCacheAutoBreakpoint
	if !setting.Enabled || !matchAnyModelPattern(setting.ModelPatterns, info.UpstreamModelName) {
		return body, nil
	}
	request := gjson.ParseBytes(body)
	input := request.Get("input")
	if !input.IsArray() || request.Get("prompt_cache_options.mode").String() == "explicit" {
		return body, nil
	}
	items := input.Array()
	lastUser := -1
	for i, item := range items {
		content := item.Get("content")
		if content.IsArray() {
			for _, part := range content.Array() {
				if part.Get("prompt_cache_breakpoint").Exists() {
					return body, nil
				}
			}
		}
		if kind := item.Get("type").String(); (kind == "" || kind == "message") && item.Get("role").String() == "user" {
			lastUser = i
		}
	}

	out := body
	added := 0
	for i := lastUser - 1; i >= 0 && added < promptCacheAutoBreakpointLimit; i-- {
		item := items[i]
		kind, role := item.Get("type").String(), item.Get("role").String()
		if (kind != "" && kind != "message") || (role != "user" && role != "developer") {
			continue
		}
		content := item.Get("content")
		var err error
		switch {
		case content.Type == gjson.String && content.String() != "":
			out, err = sjson.SetBytes(out, fmt.Sprintf("input.%d.content", i), []map[string]any{{
				"type": "input_text", "text": content.String(), "prompt_cache_breakpoint": map[string]string{"mode": "explicit"},
			}})
		case content.IsArray():
			parts := content.Array()
			if len(parts) == 0 || parts[len(parts)-1].Get("type").String() != "input_text" {
				continue
			}
			out, err = sjson.SetRawBytes(out, fmt.Sprintf("input.%d.content.%d.prompt_cache_breakpoint", i, len(parts)-1), []byte(`{"mode":"explicit"}`))
		default:
			continue
		}
		if err != nil {
			return nil, err
		}
		added++
	}
	return out, nil
}
