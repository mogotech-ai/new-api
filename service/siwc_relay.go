package service

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/net/http/httpguts"
)

func ApplySIWCResponsesRequirements(info *relaycommon.RelayInfo, body []byte) ([]byte, error) {
	if info == nil || info.ChannelMeta == nil {
		return body, nil
	}
	if !info.IsSIWC {
		if info.ChannelType != constant.ChannelTypeCodex || info.RelayMode != relayconstant.RelayModeResponses {
			return body, nil
		}
		// Codex channels use ChatGPT accounts too and cache the same way; they get the
		// same stable cache key, while the Codex adaptor handles the rest of the body.
		return applySIWCSessionAndSharedPrefix(info, body)
	}
	body, err := sjson.SetBytes(body, "stream", true)
	if err != nil {
		return nil, err
	}
	// SIWC rejects the string shorthand ("Input must be a list"); send its equivalent single user message.
	if input := gjson.GetBytes(body, "input"); input.Type == gjson.String {
		body, err = sjson.SetBytes(body, "input", []map[string]string{{"role": "user", "content": input.String()}})
		if err != nil {
			return nil, err
		}
	}
	body, err = sjson.SetBytes(body, "store", false)
	if err != nil {
		return nil, err
	}
	// SIWC rejects retention; omit the other cache controls until upstream support is confirmed.
	for _, field := range []string{"prompt_cache_retention", "prompt_cache_options", "cache_salt"} {
		body, err = sjson.DeleteBytes(body, field)
		if err != nil {
			return nil, err
		}
	}
	// Fold before the breakpoint markers are stripped and before the session seed reads instructions.
	body, err = foldSIWCLeadingInputIntoInstructions(info, body)
	if err != nil {
		return nil, err
	}
	// SIWC also rejects explicit cache breakpoints on content parts.
	request := gjson.ParseBytes(body)
	var breakpointPaths []string
	if instructions := request.Get("instructions"); instructions.IsArray() {
		for i, part := range instructions.Array() {
			if part.Get("prompt_cache_breakpoint").Exists() {
				breakpointPaths = append(breakpointPaths, fmt.Sprintf("instructions.%d.prompt_cache_breakpoint", i))
			}
		}
	}
	for i, item := range request.Get("input").Array() {
		content := item.Get("content")
		if !content.IsArray() {
			continue
		}
		for j, part := range content.Array() {
			if part.Get("prompt_cache_breakpoint").Exists() {
				breakpointPaths = append(breakpointPaths, fmt.Sprintf("input.%d.content.%d.prompt_cache_breakpoint", i, j))
			}
		}
	}
	for _, path := range breakpointPaths {
		for gjson.GetBytes(body, path).Exists() {
			body, err = sjson.DeleteBytes(body, path)
			if err != nil {
				return nil, err
			}
		}
	}
	return applySIWCSessionAndSharedPrefix(info, body)
}

// applySIWCSessionAndSharedPrefix sets the prompt cache key from the body before
// the shared prefix fold, so every request of one run shares one key.
func applySIWCSessionAndSharedPrefix(info *relaycommon.RelayInfo, body []byte) ([]byte, error) {
	var err error
	sessionID := resolveSIWCSessionID(info, body)
	cacheKey := gjson.GetBytes(body, "prompt_cache_key")
	if !cacheKey.Exists() {
		body, err = sjson.SetBytes(body, "prompt_cache_key", sessionID)
		if err != nil {
			return nil, err
		}
	} else if cacheKey.String() != sessionID {
		logger.LogWarn(nil, common.LogText("SIWC prompt cache session mismatch: token_id=%d model=%q session_id_fingerprint=%x prompt_cache_key_fingerprint=%x",
			info.TokenId, info.GetUpstreamModelName(), common.Sha256Raw([]byte(sessionID))[:6], common.Sha256Raw([]byte(cacheKey.String()))[:6]))
	}
	info.SIWCSessionID = sessionID
	return foldSIWCSharedPrefix(info, body)
}

func resolveSIWCSessionID(info *relaycommon.RelayInfo, body []byte) string {
	headerKeys := slices.Sorted(maps.Keys(info.RequestHeaders))
	for _, name := range []string{"session_id", "Session-Id"} {
		if value := info.RequestHeaders[name]; httpguts.ValidHeaderFieldValue(value) && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
		for _, key := range headerKeys {
			value := info.RequestHeaders[key]
			if strings.EqualFold(key, name) && httpguts.ValidHeaderFieldValue(value) {
				if value = strings.TrimSpace(value); value != "" {
					return value
				}
			}
		}
	}
	// API key placeholders must never supply a prompt cache key.
	if value, ok := relaycommon.GetEffectiveHeaderOverride(info)["session_id"].(string); ok && !strings.Contains(value, "{api_key}") {
		if placeholder, ok := strings.CutPrefix(strings.TrimSpace(value), "{client_header:"); ok {
			value = ""
			name, suffix, closed := strings.Cut(placeholder, "}")
			name = strings.TrimSpace(name)
			if closed && suffix == "" && name != "" {
				for key, header := range info.RequestHeaders {
					if strings.EqualFold(key, name) {
						value = header
						break
					}
				}
			}
		}
		if httpguts.ValidHeaderFieldValue(value) && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	request := gjson.ParseBytes(body)
	if key := request.Get("prompt_cache_key"); key.Type == gjson.String && httpguts.ValidHeaderFieldValue(key.String()) && strings.TrimSpace(key.String()) != "" {
		return strings.TrimSpace(key.String())
	}

	// Without the fold, instructions are often one shared system prompt, which would
	// put every conversation of a token on one cache key; the first user text tells
	// them apart. With prefix prewarm off the seed stays instructions-or-first-user.
	instructions := siwcPromptText(request.Get("instructions"))
	firstUser := ""
	input := request.Get("input")
	if input.Type == gjson.String {
		firstUser = input.String()
	} else {
		for _, item := range input.Array() {
			role, kind := item.Get("role").String(), item.Get("type").String()
			if role != "" && role != "user" {
				continue
			}
			if role == "" && kind != "input_text" && kind != "text" && kind != "" {
				continue
			}
			firstUser = siwcPromptText(item.Get("content"))
			if firstUser == "" {
				firstUser = siwcPromptText(item)
			}
			if firstUser != "" {
				break
			}
		}
	}
	prefix := instructions[:min(len(instructions), 32*1024)]
	if firstUser != "" && (prefix == "" || model_setting.GetGlobalSettings().SIWCPrefixPrewarm.Enabled) {
		if prefix != "" {
			prefix += "\n\n"
		}
		prefix += firstUser[:min(len(firstUser), 32*1024)]
	}
	seed := fmt.Sprintf("%d\n%d\n%s\n%s", info.TokenId, info.UserId, info.GetUpstreamModelName(), prefix)
	return fmt.Sprintf("%x", common.Sha256Raw([]byte(seed)))
}

func siwcPromptText(value gjson.Result) string {
	if value.Type == gjson.String {
		return value.String()
	}
	if text := value.Get("text"); text.Type == gjson.String {
		return text.String()
	}
	var text strings.Builder
	for _, part := range value.Array() {
		if field := part.Get("text"); field.Type == gjson.String {
			text.WriteString(field.String())
		}
	}
	return text.String()
}
