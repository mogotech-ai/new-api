package claudemessages

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	kitutil "github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
)

func claudeCitationsToChat(raw json.RawMessage, text string, textOffset int) ([]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var citations []map[string]any
	if err := kitutil.Unmarshal(raw, &citations); err != nil {
		return nil, fmt.Errorf("invalid Claude citations: %w", err)
	}
	annotations := make([]any, 0, len(citations))
	for _, citation := range citations {
		url := strings.TrimSpace(kitutil.Interface2String(citation["url"]))
		if url == "" {
			continue
		}
		converted := map[string]any{
			"url":   url,
			"title": strings.TrimSpace(kitutil.Interface2String(citation["title"])),
		}
		citedText := kitutil.Interface2String(citation["cited_text"])
		if citedText != "" {
			converted["cited_text"] = citedText
		}
		// Claude cites the whole text block it is attached to; cited_text is the
		// source page excerpt and rarely appears verbatim in the answer.
		if before, _, ok := strings.Cut(text, citedText); ok && citedText != "" {
			startIndex := textOffset + utf8.RuneCountInString(before)
			converted["start_index"] = startIndex
			converted["end_index"] = startIndex + utf8.RuneCountInString(citedText)
		} else if text != "" {
			converted["start_index"] = textOffset
			converted["end_index"] = textOffset + utf8.RuneCountInString(text)
		}
		if encryptedIndex := kitutil.Interface2String(citation["encrypted_index"]); encryptedIndex != "" {
			converted["encrypted_index"] = encryptedIndex
		}
		if converted["title"] == "" {
			delete(converted, "title")
		}
		annotations = append(annotations, map[string]any{
			"type":         "url_citation",
			"url_citation": converted,
		})
	}
	return annotations, nil
}

func marshalChatAnnotations(annotations []any) (json.RawMessage, error) {
	if len(annotations) == 0 {
		return nil, nil
	}
	return kitutil.Marshal(annotations)
}
