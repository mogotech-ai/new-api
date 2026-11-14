package service

import (
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// siwcSharedTestSetup gives the test a fresh memory and a 1,000-token (4,000-byte) minimum.
func siwcSharedTestSetup(t *testing.T) {
	previous, previousMemory := *model_setting.GetGlobalSettings(), siwcSharedPrefixes
	siwcSharedPrefixes = newSIWCSharedPrefixMemory()
	t.Cleanup(func() { *model_setting.GetGlobalSettings(), siwcSharedPrefixes = previous, previousMemory })
	model_setting.GetGlobalSettings().SIWCSharedPrefixFold = model_setting.SIWCSharedPrefixFold{
		Enabled: true, TokenNames: []string{"forecast"}, MinPrefixTokens: 1000, TTLSeconds: 3600, MaxBytes: 1 << 20}
}

func siwcSharedTestInfo(channelType int) *relaycommon.RelayInfo {
	return &relaycommon.RelayInfo{TokenId: 3, UserId: 4, TokenName: "forecast", RelayMode: relayconstant.RelayModeResponses,
		ChannelMeta: &relaycommon.ChannelMeta{IsSIWC: channelType == constant.ChannelTypeOpenAI, ChannelType: channelType, ChannelId: 25, UpstreamModelName: "gpt-test"}}
}

// siwcPregameTestText is a report request: game data, then the market it asks about.
func siwcPregameTestText(game, market string) string {
	return "## Task\nCheck the forecast.\n\n## Event\n" + game + "\n" + strings.Repeat(game+" stat row\n", 2000) +
		"\n## Target market\n\nmarket_type\n" + market + "\n\n## Accepted pregame forecast\n" + market + " line\n"
}

func siwcSharedTestBody(t *testing.T, text string) []byte {
	body, err := common.Marshal(map[string]any{"model": "gpt-test", "instructions": "SYSTEM", "stream": true,
		"input": []map[string]any{{"role": "user", "content": []map[string]string{{"type": "input_text", "text": text}}}}})
	require.NoError(t, err)
	return body
}

// siwcSharedTestFold runs the fold and checks it only moved text: instructions
// plus the kept message still read as the original request.
func siwcSharedTestFold(t *testing.T, info *relaycommon.RelayInfo, text string) (instructions, kept string) {
	got, err := foldSIWCSharedPrefix(info, siwcSharedTestBody(t, text))
	require.NoError(t, err)
	instructions = gjson.GetBytes(got, "instructions").String()
	kept = gjson.GetBytes(got, "input.0.content.0.text").String()
	if instructions == "SYSTEM" {
		assert.Equal(t, text, kept)
	} else {
		require.True(t, strings.HasPrefix(instructions, "SYSTEM\n\n"))
		assert.Equal(t, text, strings.TrimPrefix(instructions, "SYSTEM\n\n")+kept)
		assert.True(t, strings.HasSuffix(instructions, "\n"))
	}
	assert.Equal(t, "gpt-test", gjson.GetBytes(got, "model").String())
	assert.True(t, gjson.GetBytes(got, "stream").Bool())
	return instructions, kept
}

func TestSIWCSharedPrefixFoldMarketsOfOneGame(t *testing.T) {
	siwcSharedTestSetup(t)
	for _, channelType := range []int{constant.ChannelTypeOpenAI, constant.ChannelTypeCodex} {
		siwcSharedPrefixes = newSIWCSharedPrefixMemory()
		info := siwcSharedTestInfo(channelType)
		first, _ := siwcSharedTestFold(t, info, siwcPregameTestText("BOS-NYK", "spread"))
		assert.Equal(t, "SYSTEM", first, "nothing to share with yet")

		second, kept := siwcSharedTestFold(t, info, siwcPregameTestText("BOS-NYK", "moneyline"))
		assert.True(t, strings.HasSuffix(second, "## Target market\n\nmarket_type\n"), "cut after the last shared line")
		assert.Equal(t, "moneyline\n\n## Accepted pregame forecast\nmoneyline line\n", kept)
		third, _ := siwcSharedTestFold(t, info, siwcPregameTestText("BOS-NYK", "total"))
		assert.Equal(t, second, third, "siblings send identical instructions, so the third reads what the second wrote")
	}
}

func TestSIWCSharedPrefixFoldNextGameUsesLearnedCut(t *testing.T) {
	siwcSharedTestSetup(t)
	info := siwcSharedTestInfo(constant.ChannelTypeOpenAI)
	siwcSharedTestFold(t, info, siwcPregameTestText("BOS-NYK", "spread"))
	siwcSharedTestFold(t, info, siwcPregameTestText("BOS-NYK", "moneyline"))

	// The next game shares only its header with the last one, yet its first
	// request is cut where the markets diverged, so its second one reads it.
	first, kept := siwcSharedTestFold(t, info, siwcPregameTestText("LAL-GSW", "spread"))
	assert.True(t, strings.HasSuffix(first, "## Target market\n\nmarket_type\n"))
	assert.Contains(t, first, "LAL-GSW stat row")
	assert.True(t, strings.HasPrefix(kept, "spread\n"))
	second, _ := siwcSharedTestFold(t, info, siwcPregameTestText("LAL-GSW", "total"))
	assert.Equal(t, first, second)
}

func TestSIWCSharedPrefixFoldLearnedCutStaysInItsTemplate(t *testing.T) {
	siwcSharedTestSetup(t)
	info := siwcSharedTestInfo(constant.ChannelTypeCodex)
	siwcSharedTestFold(t, info, siwcPregameTestText("BOS-NYK", "spread"))
	siwcSharedTestFold(t, info, siwcPregameTestText("BOS-NYK", "moneyline"))

	// A follow-up template repeats the market section after data that changes
	// between its own requests; the checklist's cut must not apply to it.
	followUp := func(rows string) string {
		return "# Complete the analysis\n" + strings.Repeat("LAL-GSW stat row\n", 2000) + "## Supplemental\nmetric,value\n" + rows +
			strings.TrimPrefix(siwcPregameTestText("LAL-GSW", "spread"), "## Task\nCheck the forecast.\n")
	}
	instructions, _ := siwcSharedTestFold(t, info, followUp("a,1\nb,2\n"))
	assert.Equal(t, "SYSTEM", instructions)
	second, _ := siwcSharedTestFold(t, info, followUp("b,2\na,1\n"))
	assert.True(t, strings.HasSuffix(second, "## Supplemental\nmetric,value\n"), "cut where its own requests diverge")
	third, _ := siwcSharedTestFold(t, info, followUp("b,2\nc,3\n"))
	assert.Equal(t, second, third)
}

func TestSIWCSharedPrefixFoldSkips(t *testing.T) {
	siwcSharedTestSetup(t)
	text := siwcPregameTestText("BOS-NYK", "spread")
	for _, tc := range []struct {
		name   string
		mutate func(*relaycommon.RelayInfo)
		body   string
	}{
		{name: "other token", mutate: func(info *relaycommon.RelayInfo) { info.TokenName = "tape-dev" }},
		{name: "channel test", mutate: func(info *relaycommon.RelayInfo) { info.IsChannelTest = true }},
		{name: "chat mode", mutate: func(info *relaycommon.RelayInfo) { info.RelayMode = relayconstant.RelayModeChatCompletions }},
		{name: "disabled", mutate: func(*relaycommon.RelayInfo) { model_setting.GetGlobalSettings().SIWCSharedPrefixFold.Enabled = false }},
		{name: "history", body: `{"instructions":"SYSTEM","input":[{"role":"user","content":%q},{"role":"assistant","content":"ok"},{"role":"user","content":%q}]}`},
		{name: "assistant", body: `{"instructions":"SYSTEM","input":[{"role":"assistant","content":%q}]}`},
		{name: "image part", body: `{"instructions":"SYSTEM","input":[{"role":"user","content":[{"type":"input_text","text":%q},{"type":"input_image","image_url":"x"}]}]}`},
		{name: "instruction parts", body: `{"instructions":[{"type":"input_text","text":"SYSTEM"}],"input":[{"role":"user","content":%q}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			siwcSharedTestSetup(t)
			info := siwcSharedTestInfo(constant.ChannelTypeCodex)
			if tc.mutate != nil {
				tc.mutate(info)
			}
			body := func() []byte {
				if tc.body == "" {
					return siwcSharedTestBody(t, text)
				}
				format := strings.ReplaceAll(tc.body, "%q", `"`+strings.ReplaceAll(text, "\n", `\n`)+`"`)
				require.True(t, gjson.Valid(format))
				return []byte(format)
			}
			for range 3 {
				got, err := foldSIWCSharedPrefix(info, body())
				require.NoError(t, err)
				assert.Equal(t, body(), got)
			}
		})
	}
}

func TestSIWCSharedPrefixFoldThresholds(t *testing.T) {
	siwcSharedTestSetup(t)
	info := siwcSharedTestInfo(constant.ChannelTypeCodex)
	short := "## Event\nBOS-NYK\n" + strings.Repeat("row\n", 100)
	siwcSharedTestFold(t, info, short+"spread\n")
	instructions, _ := siwcSharedTestFold(t, info, short+"total\n")
	assert.Equal(t, "SYSTEM", instructions, "a shared part under the minimum stays in the message")

	// A message identical to an earlier one keeps a non-empty tail.
	text := siwcPregameTestText("BOS-NYK", "spread")
	siwcSharedTestFold(t, info, text)
	instructions, kept := siwcSharedTestFold(t, info, text)
	assert.NotEqual(t, "SYSTEM", instructions)
	assert.NotEmpty(t, kept)
}

func TestSIWCSharedPrefixMemoryBounds(t *testing.T) {
	m := newSIWCSharedPrefixMemory()
	now := time.Now()
	text := func(game string) string { return strings.Repeat(game+" row\n", 200) + "market\n" }
	for i := range 10 {
		m.cut([32]byte{1}, text(string(rune('A'+i))), 10, now, time.Hour, 3000)
	}
	assert.LessOrEqual(t, m.bytes, 3000+len(text("A")), "older texts are dropped past max bytes")
	assert.Equal(t, len(m.texts)*len(text("A")), m.bytes)

	m.cut([32]byte{1}, text("A")+"one\n", 10, now, time.Hour, 1<<20)
	m.cut([32]byte{1}, text("A")+"two\n", 10, now, time.Hour, 1<<20)
	require.Len(t, m.cuts, 1)
	m.cut([32]byte{1}, text("A"), 10, now.Add(2*time.Hour), time.Hour, 1<<20)
	assert.Len(t, m.texts, 1, "texts expire after the TTL")
	assert.Len(t, m.cuts, 1, "cut markers outlive the texts")
	m.cut([32]byte{1}, text("B"), 10, now.Add(siwcSharedCutMarkerTTL+3*time.Hour), time.Hour, 1<<20)
	assert.Empty(t, m.cuts, "cut markers expire too")

	for i := range 100 { // every pair of texts shares a different prefix
		base := strings.Repeat("x", 50) + string(rune('a'+i%26)) + string(rune('a'+i/26)) + "\n"
		m.cut([32]byte{2}, base+"one\n", 10, now, time.Hour, 1<<20)
		m.cut([32]byte{2}, base+"two\n", 10, now, time.Hour, 1<<20)
	}
	assert.LessOrEqual(t, len(m.cuts[[32]byte{2}]), siwcSharedCutsPerScope)

	assert.Zero(t, m.cut([32]byte{3}, "no newline at all, so nothing to cut", 1, now, time.Hour, 1<<20))
	assert.Zero(t, m.cut([32]byte{3}, "no newline at all, so nothing to cut", 1, now, time.Hour, 1<<20))
}

func TestApplySIWCResponsesRequirementsCodex(t *testing.T) {
	siwcSharedTestSetup(t)
	body := []byte(`{"model":"gpt-test","instructions":"Answer briefly","input":[{"role":"user","content":"game 1"}],"stream":false,"store":false,"prompt_cache_retention":"24h"}`)
	info := siwcSharedTestInfo(constant.ChannelTypeCodex)
	info.TokenName = "tape-dev"
	got, err := ApplySIWCResponsesRequirements(info, body)
	require.NoError(t, err)
	require.NotEmpty(t, info.SIWCSessionID)
	assert.Equal(t, info.SIWCSessionID, gjson.GetBytes(got, "prompt_cache_key").String())
	// Only the cache key is added; the Codex adaptor owns the rest of the body.
	assert.JSONEq(t, `{"model":"gpt-test","instructions":"Answer briefly","input":[{"role":"user","content":"game 1"}],"stream":false,"store":false,"prompt_cache_retention":"24h","prompt_cache_key":"`+info.SIWCSessionID+`"}`, string(got))

	// Same seed as a SIWC channel, so a run moved between the two keeps its key.
	siwc := siwcSharedTestInfo(constant.ChannelTypeOpenAI)
	siwc.TokenName = "tape-dev"
	_, err = ApplySIWCResponsesRequirements(siwc, body)
	require.NoError(t, err)
	assert.Equal(t, siwc.SIWCSessionID, info.SIWCSessionID)

	// A caller key wins, and other channel types and modes are untouched.
	info = siwcSharedTestInfo(constant.ChannelTypeCodex)
	got, err = ApplySIWCResponsesRequirements(info, []byte(`{"prompt_cache_key":"caller","input":[{"role":"user","content":"x"}]}`))
	require.NoError(t, err)
	assert.Equal(t, "caller", gjson.GetBytes(got, "prompt_cache_key").String())
	assert.Equal(t, "caller", info.SIWCSessionID)
	for _, mutate := range []func(*relaycommon.RelayInfo){
		func(info *relaycommon.RelayInfo) { info.ChannelType = constant.ChannelTypeOpenAI },
		func(info *relaycommon.RelayInfo) { info.RelayMode = relayconstant.RelayModeResponsesCompact },
	} {
		info = siwcSharedTestInfo(constant.ChannelTypeCodex)
		info.IsSIWC = false
		mutate(info)
		got, err = ApplySIWCResponsesRequirements(info, body)
		require.NoError(t, err)
		assert.Equal(t, body, got)
		assert.Empty(t, info.SIWCSessionID)
	}
}

func TestApplySIWCResponsesRequirementsFoldsAfterSessionKey(t *testing.T) {
	siwcSharedTestSetup(t)
	keys := map[string]bool{}
	var last []byte
	for _, market := range []string{"spread", "moneyline"} {
		info := siwcSharedTestInfo(constant.ChannelTypeCodex)
		body := siwcSharedTestBody(t, siwcPregameTestText("BOS-NYK", market))
		got, err := ApplySIWCResponsesRequirements(info, body)
		require.NoError(t, err)
		keys[gjson.GetBytes(got, "prompt_cache_key").String()] = true
		last = got
	}
	assert.Len(t, keys, 1, "the key is derived before the fold, so both markets share it")
	assert.Contains(t, gjson.GetBytes(last, "instructions").String(), "## Target market")
}
