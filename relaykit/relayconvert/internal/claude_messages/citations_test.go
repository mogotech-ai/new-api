package claudemessages

import (
	"testing"

	"github.com/QuantumNous/new-api/relaykit/dto"
	kitutil "github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Web-search citations carry the source excerpt as cited_text, which rarely
// appears in the answer; the citation must still span its own text block.
func TestResponseClaude2OpenAIWebSearchCitationsSpanTextBlock(t *testing.T) {
	raw := `{"id":"msg_1","type":"message","role":"assistant","stop_reason":"end_turn","content":[
		{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"nba"}},
		{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[]},
		{"type":"text","text":"Latest: "},
		{"type":"text","text":"Nuggets 133, Bulls 128.","citations":[{"type":"web_search_result_location","url":"https://espn.com/a","title":"ESPN","cited_text":"# NBA Scoreboard Nuggets 133","encrypted_index":"x"}]},
		{"type":"text","text":" Jokić 22 pts, Nuggets won.","citations":[{"type":"web_search_result_location","url":"https://nba.com/b","title":"NBA","cited_text":"Nuggets won"}]}
	]}`
	var resp dto.ClaudeResponse
	require.NoError(t, kitutil.Unmarshal([]byte(raw), &resp))

	out := ResponseClaude2OpenAI(&resp)
	require.Len(t, out.Choices, 1)
	content := out.Choices[0].Message.StringContent()
	assert.Equal(t, "Latest: Nuggets 133, Bulls 128. Jokić 22 pts, Nuggets won.", content)

	var annotations []struct {
		URLCitation struct {
			URL        string `json:"url"`
			StartIndex *int   `json:"start_index"`
			EndIndex   *int   `json:"end_index"`
		} `json:"url_citation"`
	}
	require.NoError(t, kitutil.Unmarshal(out.Choices[0].Message.Annotations, &annotations))
	require.Len(t, annotations, 2)

	first := annotations[0].URLCitation
	assert.Equal(t, "https://espn.com/a", first.URL)
	require.NotNil(t, first.StartIndex)
	assert.Equal(t, 8, *first.StartIndex)
	assert.Equal(t, 31, *first.EndIndex)

	// cited_text found verbatim keeps the exact span; offsets count runes.
	second := annotations[1].URLCitation
	require.NotNil(t, second.StartIndex)
	assert.Equal(t, 46, *second.StartIndex)
	assert.Equal(t, 57, *second.EndIndex)
}
