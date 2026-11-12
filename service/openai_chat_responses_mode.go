package service

import (
	"regexp"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/model_setting"
)

// Chat→Responses upgrade policy is host routing logic (it decides *whether*
// to convert, reading host settings), so it lives here, not in relayconvert.

var chatResponsesRegexCache sync.Map // map[string]*regexp.Regexp

func matchAnyModelPattern(patterns []string, model string) bool {
	if len(patterns) == 0 || model == "" {
		return false
	}
	for _, pattern := range patterns {
		if pattern == "" {
			continue
		}
		cached, ok := chatResponsesRegexCache.Load(pattern)
		if !ok {
			// Invalid patterns are cached as nil: logged once, then non-matching, so they never break runtime traffic.
			compiled, err := regexp.Compile(pattern)
			if err != nil {
				common.SysError(common.LogText("invalid regex pattern %q: %v", pattern, err))
			}
			cached, _ = chatResponsesRegexCache.LoadOrStore(pattern, compiled)
		}
		if re := cached.(*regexp.Regexp); re != nil && re.MatchString(model) {
			return true
		}
	}
	return false
}

func ShouldChatCompletionsUseResponsesPolicy(policy model_setting.ChatCompletionsToResponsesPolicy, channelID int, channelType int, model string) bool {
	if !policy.IsChannelEnabled(channelID, channelType) {
		return false
	}
	return matchAnyModelPattern(policy.ModelPatterns, model)
}

func ShouldChatCompletionsUseResponsesGlobal(channelID int, channelType int, model string) bool {
	return ShouldChatCompletionsUseResponsesPolicy(
		model_setting.GetGlobalSettings().ChatCompletionsToResponsesPolicy,
		channelID,
		channelType,
		model,
	)
}
