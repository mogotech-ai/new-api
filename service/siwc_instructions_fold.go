package service

import (
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// foldSIWCLeadingInputIntoInstructions moves the stable leading run of input
// messages into instructions. SIWC only writes prompt cache at the end of
// instructions and of the last user message, so a stable prefix left in the
// middle of input is never cached. The output depends only on the folded
// messages, so requests sharing a prefix produce identical instructions bytes.
// Anything it cannot fold safely leaves the body unchanged.
func foldSIWCLeadingInputIntoInstructions(info *relaycommon.RelayInfo, body []byte) ([]byte, error) {
	setting := model_setting.GetGlobalSettings().SIWCInstructionsFold
	if !setting.Enabled {
		return body, nil
	}
	request := gjson.ParseBytes(body)
	input := request.Get("input")
	if !input.IsArray() {
		return body, nil
	}
	items := input.Array()
	messageRoles := []string{"user", "assistant", "developer", "system"}
	roles := make([]string, len(items)) // "" for anything that is not a message item
	lastUser := -1
	for i, item := range items {
		kind, role := item.Get("type").String(), item.Get("role").String()
		if (kind != "" && kind != "message") || !slices.Contains(messageRoles, role) {
			continue
		}
		roles[i] = role
		if role == "user" {
			lastUser = i
		}
	}
	if lastUser < 0 {
		return body, nil
	}

	// Markers on the last user message are not a boundary: that position is already a cache write point.
	boundary, source := -1, ""
	if setting.HonorExplicitBreakpoints {
		for i := range lastUser {
			for _, part := range items[i].Get("content").Array() {
				if part.Get("prompt_cache_breakpoint").Exists() {
					boundary, source = i, "explicit_breakpoint"
				}
			}
		}
	}
	if boundary < 0 {
		for _, rule := range setting.Rules {
			if len(rule.TokenNames) == 0 && len(rule.ModelPatterns) == 0 && len(rule.PromptCacheKeyPatterns) == 0 {
				continue // a rule without criteria would fold every SIWC request
			}
			if len(rule.TokenNames) > 0 && !slices.Contains(rule.TokenNames, info.TokenName) {
				continue
			}
			if len(rule.ModelPatterns) > 0 && !matchAnyModelPattern(rule.ModelPatterns, info.GetUpstreamModelName()) {
				continue
			}
			if len(rule.PromptCacheKeyPatterns) > 0 {
				key := request.Get("prompt_cache_key")
				if key.Type != gjson.String || !matchAnyModelPattern(rule.PromptCacheKeyPatterns, key.String()) {
					continue
				}
			}
			source = rule.Name
			switch rule.Boundary {
			case "first_assistant":
				firstUser := slices.Index(roles, "user")
				if offset := slices.Index(roles[firstUser+1:], "assistant"); offset >= 0 {
					boundary = firstUser + 1 + offset
				}
			case "keep_last":
				if rule.KeepLast >= 1 {
					boundary = len(items) - rule.KeepLast - 1
				}
			}
			break // first matching rule wins, even when it finds no boundary
		}
	}
	if boundary < 0 || boundary >= lastUser {
		return body, nil
	}

	existing := ""
	if instructions := request.Get("instructions"); instructions.Exists() && instructions.Type != gjson.Null {
		text, ok := siwcFoldableText(instructions, "")
		if !ok {
			return body, nil
		}
		existing = text
	}
	var folded strings.Builder
	folded.WriteString(existing)
	for i := range boundary + 1 {
		text, ok := siwcFoldableText(items[i].Get("content"), "\n\n")
		if roles[i] == "" || !ok {
			return body, nil
		}
		if folded.Len() > 0 {
			folded.WriteString("\n\n")
		}
		folded.WriteString("## " + strings.ToUpper(roles[i]) + "\n" + text)
	}

	// Splice the raw tail back so the remaining items stay byte-for-byte.
	var rest strings.Builder
	rest.WriteByte('[')
	for i := boundary + 1; i < len(items); i++ {
		if i > boundary+1 {
			rest.WriteByte(',')
		}
		rest.WriteString(items[i].Raw)
	}
	rest.WriteByte(']')
	body, err := sjson.SetRawBytes(body, "input", []byte(rest.String()))
	if err != nil {
		return nil, err
	}
	body, err = sjson.SetBytes(body, "instructions", folded.String())
	if err != nil {
		return nil, err
	}
	logger.LogInfo(nil, common.LogText("SIWC instructions fold: rule=%q token_id=%d model=%q folded_items=%d folded_chars=%d",
		source, info.TokenId, info.GetUpstreamModelName(), boundary+1, folded.Len()-len(existing)))
	return body, nil
}

// siwcFoldableText returns the text of a string or of an array of text-only
// parts joined with sep; ok is false for any other shape (images, files, ...).
func siwcFoldableText(value gjson.Result, sep string) (string, bool) {
	if value.Type == gjson.String {
		return value.String(), true
	}
	if !value.IsArray() {
		return "", false
	}
	var texts []string
	for _, part := range value.Array() {
		if !slices.Contains([]string{"input_text", "output_text", "text"}, part.Get("type").String()) {
			return "", false
		}
		texts = append(texts, part.Get("text").String())
	}
	return strings.Join(texts, sep), true
}
