package model_setting

import (
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/config"
)

type ChatCompletionsToResponsesPolicy struct {
	Enabled       bool     `json:"enabled"`
	AllChannels   bool     `json:"all_channels"`
	ChannelIDs    []int    `json:"channel_ids,omitempty"`
	ChannelTypes  []int    `json:"channel_types,omitempty"`
	ModelPatterns []string `json:"model_patterns,omitempty"` // Matches upstream model names after channel mapping.
}

func (p ChatCompletionsToResponsesPolicy) IsChannelEnabled(channelID int, channelType int) bool {
	if !p.Enabled {
		return false
	}
	if p.AllChannels {
		return true
	}

	if channelID > 0 && len(p.ChannelIDs) > 0 && slices.Contains(p.ChannelIDs, channelID) {
		return true
	}
	if channelType > 0 && len(p.ChannelTypes) > 0 && slices.Contains(p.ChannelTypes, channelType) {
		return true
	}
	return false
}

// PromptCacheAutoBreakpoint controls automatic prompt_cache_breakpoint injection
// for official OpenAI Responses requests.
type PromptCacheAutoBreakpoint struct {
	Enabled       bool     `json:"enabled"`
	ModelPatterns []string `json:"model_patterns,omitempty"` // Matches upstream model names after channel mapping.
}

// SIWCInstructionsFold moves a stable leading run of Responses input messages
// into instructions on SIWC channels, where only the end of instructions and
// the last user message are cache write points.
type SIWCInstructionsFold struct {
	Enabled                  bool `json:"enabled"`
	HonorExplicitBreakpoints bool `json:"honor_explicit_breakpoints"` // Caller-marked prompt_cache_breakpoint is the boundary.
	// No omitempty: an exported empty list must not decode back to the defaults.
	Rules []SIWCInstructionsFoldRule `json:"rules"`
}

// UnmarshalJSON decodes rules into a fresh slice. The config manager decodes a
// stored override over the defaults in place, and JSON decoding reuses slice
// elements, so an overriding rule would otherwise inherit the criteria of the
// default rule at the same index.
func (s *SIWCInstructionsFold) UnmarshalJSON(data []byte) error {
	type plain SIWCInstructionsFold
	next := plain(*s)
	next.Rules = nil
	if err := common.Unmarshal(data, &next); err != nil {
		return err
	}
	if next.Rules == nil {
		next.Rules = s.Rules // override without rules keeps the current ones
	}
	*s = SIWCInstructionsFold(next)
	return nil
}

// SIWCInstructionsFoldRule matches when every non-empty criterion matches; a
// rule with no criteria never matches.
type SIWCInstructionsFoldRule struct {
	Name                   string   `json:"name"`
	TokenNames             []string `json:"token_names,omitempty"`               // Exact token name match.
	ModelPatterns          []string `json:"model_patterns,omitempty"`            // Regex on the upstream model name.
	PromptCacheKeyPatterns []string `json:"prompt_cache_key_patterns,omitempty"` // Regex on the body prompt_cache_key, which must then be a string.
	Boundary               string   `json:"boundary"`                            // "first_assistant" or "keep_last".
	KeepLast               int      `json:"keep_last,omitempty"`                 // keep_last: trailing input items left in input (>=1).
}

type GlobalSettings struct {
	PassThroughRequestEnabled bool     `json:"pass_through_request_enabled"`
	ThinkingModelBlacklist    []string `json:"thinking_model_blacklist"`
	// EffortTailModelIDs lists real model IDs that sit inside the GPT/o-series
	// family whitelist but whose names already end in an effort word.
	EffortTailModelIDs               []string                         `json:"effort_tail_model_ids"`
	ChatCompletionsToResponsesPolicy ChatCompletionsToResponsesPolicy `json:"chat_completions_to_responses_policy"`
	PromptCacheAutoBreakpoint        PromptCacheAutoBreakpoint        `json:"prompt_cache_auto_breakpoint"`
	SIWCInstructionsFold             SIWCInstructionsFold             `json:"siwc_instructions_fold"`
}

// 默认配置
var defaultOpenaiSettings = GlobalSettings{
	PassThroughRequestEnabled: false,
	ThinkingModelBlacklist: []string{
		"moonshotai/kimi-k2-thinking",
		"kimi-k2-thinking",
	},
	EffortTailModelIDs: []string{
		"gpt-5.1-codex-max",
		"qwen-image-edit-max",
		"qwen-max",
		"stable-diffusion-3-medium",
		"yi-medium",
	},
	ChatCompletionsToResponsesPolicy: ChatCompletionsToResponsesPolicy{
		Enabled:     false,
		AllChannels: true,
	},
	PromptCacheAutoBreakpoint: PromptCacheAutoBreakpoint{
		Enabled: true,
		// gpt-5.6 and later minors, gpt-6 to gpt-19; two-digit majors stop at 19 so Azure-style gpt-35-turbo stays out.
		ModelPatterns: []string{`^gpt-5\.([6-9]|\d{2,})(-|$)`, `^gpt-([6-9]|1\d)(\.\d+)?(-|$)`},
	},
	SIWCInstructionsFold: SIWCInstructionsFold{
		Enabled:                  true,
		HonorExplicitBreakpoints: true,
		Rules: []SIWCInstructionsFoldRule{
			{Name: "forecast", TokenNames: []string{"forecast"}, Boundary: "first_assistant"},
			{Name: "tape-live-c15", TokenNames: []string{"tape-dev"}, PromptCacheKeyPatterns: []string{`:c15_forecast$`}, Boundary: "keep_last", KeepLast: 2},
		},
	},
}

// 全局实例
var globalSettings = defaultOpenaiSettings

func init() {
	// 注册到全局配置管理器
	config.GlobalConfig.Register("global", &globalSettings)
}

func GetGlobalSettings() *GlobalSettings {
	return &globalSettings
}

const thinkingBlacklistRegexPrefix = "re:"

type thinkingBlacklistCompiled struct {
	source  string
	exact   []string
	regexes []*regexp.Regexp
}

var (
	thinkingBlacklistMu    sync.RWMutex
	thinkingBlacklistCache thinkingBlacklistCompiled
)

func thinkingBlacklistSourceKey(entries []string) string {
	return strings.Join(entries, "\x00")
}

func compiledThinkingBlacklist() ([]string, []*regexp.Regexp) {
	entries := globalSettings.ThinkingModelBlacklist
	key := thinkingBlacklistSourceKey(entries)

	thinkingBlacklistMu.RLock()
	if thinkingBlacklistCache.source == key {
		exact, regexes := thinkingBlacklistCache.exact, thinkingBlacklistCache.regexes
		thinkingBlacklistMu.RUnlock()
		return exact, regexes
	}
	thinkingBlacklistMu.RUnlock()

	thinkingBlacklistMu.Lock()
	defer thinkingBlacklistMu.Unlock()
	if thinkingBlacklistCache.source == key {
		return thinkingBlacklistCache.exact, thinkingBlacklistCache.regexes
	}

	exact := make([]string, 0, len(entries))
	var regexes []*regexp.Regexp
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if after, ok := strings.CutPrefix(entry, thinkingBlacklistRegexPrefix); ok {
			pattern := after
			if pattern == "" {
				common.SysError(common.LogText("invalid thinking_model_blacklist regex %q: pattern is empty", entry))
				continue
			}
			re, err := regexp.Compile(pattern)
			if err != nil {
				common.SysError(common.LogText("invalid thinking_model_blacklist regex %q: %v", entry, err))
				continue
			}
			regexes = append(regexes, re)
			continue
		}
		exact = append(exact, entry)
	}
	thinkingBlacklistCache = thinkingBlacklistCompiled{source: key, exact: exact, regexes: regexes}
	return exact, regexes
}

// ShouldPreserveThinkingSuffix reports whether the full model name is exempt
// from host thinking-suffix and @-modifier parsing. Exact blacklist entries
// match the complete name; entries prefixed with re: are Go regular expressions
// matched with MatchString against the same full name.
func ShouldPreserveThinkingSuffix(modelName string) bool {
	target := strings.TrimSpace(modelName)
	if target == "" {
		return false
	}

	exact, regexes := compiledThinkingBlacklist()
	if slices.Contains(exact, target) {
		return true
	}
	for _, re := range regexes {
		if re.MatchString(target) {
			return true
		}
	}
	return false
}

// ShouldPreserveEffortTail reports whether modelName is a real model ID whose
// name already ends in an effort word. Entries match the complete name and the
// de-namespaced bare name.
func ShouldPreserveEffortTail(modelName string) bool {
	target := strings.TrimSpace(modelName)
	if target == "" {
		return false
	}
	bare := target
	if slash := strings.LastIndex(target, "/"); slash >= 0 {
		bare = target[slash+1:]
	}

	for _, entry := range globalSettings.EffortTailModelIDs {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if entry == target || entry == bare {
			return true
		}
	}
	return false
}
