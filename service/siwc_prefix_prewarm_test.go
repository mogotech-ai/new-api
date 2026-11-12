package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const siwcPrewarmTestSSE = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{}}\n\n" +
	"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\"}\n\n"

// siwcPrewarmTestSetup gives the test a fresh prefix index and small thresholds:
// 4,000-character messages are ~1,000 estimated tokens.
func siwcPrewarmTestSetup(t *testing.T, mutate func(*model_setting.SIWCPrefixPrewarm)) {
	previous, previousIndex := *model_setting.GetGlobalSettings(), siwcPrefixes
	siwcPrefixes = newSIWCPrefixIndex()
	t.Cleanup(func() { *model_setting.GetGlobalSettings(), siwcPrefixes = previous, previousIndex })
	setting := &model_setting.GetGlobalSettings().SIWCPrefixPrewarm
	*setting = model_setting.SIWCPrefixPrewarm{Enabled: true, MinPriorSightings: 2, MinPrefixTokens: 1000, MinGainTokens: 500, WarmTTLSeconds: 3600,
		MaxConcurrent: 100, MaxAttempts: 2, MaxPerScopeMinute: 100, MaxOutputTokens: 16, TimeoutSeconds: 5, MaxEntries: 10000}
	if mutate != nil {
		mutate(setting)
	}
}

func siwcPrewarmTestInfo() *relaycommon.RelayInfo {
	return &relaycommon.RelayInfo{TokenId: 7, UserId: 9, RelayMode: relayconstant.RelayModeResponses, SIWCSessionID: "session",
		ChannelMeta: &relaycommon.ChannelMeta{IsSIWC: true, ChannelId: 17, ApiKey: "access", UpstreamModelName: "gpt-test"}}
}

// siwcPrewarmTestBody builds a final SIWC body; each message is "u:text" or "a:text".
func siwcPrewarmTestBody(t *testing.T, messages ...string) []byte {
	input := make([]map[string]string, 0, len(messages))
	for _, message := range messages {
		role, text, _ := strings.Cut(message, ":")
		input = append(input, map[string]string{"role": map[string]string{"u": "user", "a": "assistant"}[role], "content": text})
	}
	body, err := common.Marshal(map[string]any{"model": "gpt-test", "instructions": "SYSTEM", "input": input, "stream": true, "store": false, "prompt_cache_key": "session"})
	require.NoError(t, err)
	return body
}

// siwcForecastTestBody is the forecast shape: pregame, ack, sealed segments with acks, then a dynamic tail.
func siwcForecastTestBody(t *testing.T, game string, segments int, tail string) []byte {
	messages := []string{"u:PREGAME " + game + strings.Repeat("p", 4000), "a:Acknowledged."}
	for i := range segments {
		messages = append(messages, fmt.Sprintf("u:SEGMENT %s %d %s", game, i+1, strings.Repeat("s", 4000)), "a:Acknowledged.")
	}
	return siwcPrewarmTestBody(t, append(messages, "u:DYNAMIC "+tail)...)
}

func siwcPrewarmTestComplete(p *SIWCPrefixPrewarm, sse string) {
	p.run(func(context.Context, []byte) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(sse))}, nil
	})
}

func TestSIWCPrefixPrewarmInterleavedGames(t *testing.T) {
	siwcPrewarmTestSetup(t, nil)
	warmTokens := map[string]int{}
	for round := 1; round <= 4; round++ {
		for _, game := range []string{"A", "B", "C"} {
			body := siwcForecastTestBody(t, game, 1, fmt.Sprintf("%s round %d", game, round))
			info := siwcPrewarmTestInfo()
			prewarm := PlanSIWCPrefixPrewarm(info, body)
			switch round {
			case 1, 2:
				assert.Nil(t, prewarm, "%s%d: a boundary needs two earlier sightings", game, round)
			case 3:
				require.NotNil(t, prewarm, "%s%d", game, round)
				items := gjson.GetBytes(body, "input").Array()
				assert.Equal(t, 3, prewarm.items)
				assert.Equal(t, "["+items[0].Raw+","+items[1].Raw+","+items[2].Raw+"]", gjson.GetBytes(prewarm.body, "input").Raw)
				assert.Contains(t, gjson.GetBytes(prewarm.body, "input").Raw, "SEGMENT "+game)
				assert.Equal(t, int64(16), gjson.GetBytes(prewarm.body, "max_output_tokens").Int())
				// Everything but the input and the output cap is the real body.
				real, err := sjson.DeleteBytes(body, "input")
				require.NoError(t, err)
				truncated, err := sjson.DeleteBytes(prewarm.body, "input")
				require.NoError(t, err)
				truncated, err = sjson.DeleteBytes(truncated, "max_output_tokens")
				require.NoError(t, err)
				assert.JSONEq(t, string(real), string(truncated))
				warmTokens[game] = prewarm.estTokens
				siwcPrewarmTestComplete(prewarm, siwcPrewarmTestSSE)
			case 4:
				assert.Nil(t, prewarm, "%s4: already warm", game)
				assert.Equal(t, warmTokens[game], info.SIWCWarmPrefixTokens)
			}
		}
	}
}

func TestSIWCPrefixPrewarmHistoryCorrection(t *testing.T) {
	siwcPrewarmTestSetup(t, nil)
	for round := 1; round <= 3; round++ {
		prewarm := PlanSIWCPrefixPrewarm(siwcPrewarmTestInfo(), siwcForecastTestBody(t, "A", 1, fmt.Sprint(round)))
		if round == 3 {
			require.NotNil(t, prewarm)
			siwcPrewarmTestComplete(prewarm, siwcPrewarmTestSSE)
		}
	}
	// A revised sealed segment changes its hash: the old warm prefix is never
	// expected, and the stable region shrinks to the pregame.
	revised := func(round int) []byte {
		body := siwcForecastTestBody(t, "A", 1, fmt.Sprint(round))
		body, err := sjson.SetBytes(body, "input.2.content", "SEGMENT A 1 revised "+strings.Repeat("s", 4000))
		require.NoError(t, err)
		return body
	}
	info := siwcPrewarmTestInfo()
	prewarm := PlanSIWCPrefixPrewarm(info, revised(4))
	assert.Zero(t, info.SIWCWarmPrefixTokens)
	require.NotNil(t, prewarm)
	assert.Equal(t, 1, prewarm.items)
	siwcPrewarmTestComplete(prewarm, siwcPrewarmTestSSE)

	info = siwcPrewarmTestInfo()
	assert.Nil(t, PlanSIWCPrefixPrewarm(info, revised(5)))
	assert.Positive(t, info.SIWCWarmPrefixTokens)
	prewarm = PlanSIWCPrefixPrewarm(siwcPrewarmTestInfo(), revised(6))
	require.NotNil(t, prewarm, "the revised segment is stable again after two sightings")
	assert.Equal(t, 3, prewarm.items)
	assert.Contains(t, gjson.GetBytes(prewarm.body, "input.2.content").String(), "revised")
}

func TestSIWCPrefixPrewarmC15GrowingQuarter(t *testing.T) {
	siwcPrewarmTestSetup(t, nil)
	var prewarms []int
	for round := 1; round <= 6; round++ {
		body := siwcPrewarmTestBody(t, "u:BRIEF "+strings.Repeat("b", 4000), "u:Q1 "+strings.Repeat("1", 2000), "u:Q2 "+strings.Repeat("2", 2000),
			"u:Q3 "+strings.Repeat("3", 2000), "u:Q4 so far "+strings.Repeat("4", 300*round), fmt.Sprintf("u:Now: %d", round))
		if prewarm := PlanSIWCPrefixPrewarm(siwcPrewarmTestInfo(), body); prewarm != nil {
			prewarms = append(prewarms, prewarm.items)
			siwcPrewarmTestComplete(prewarm, siwcPrewarmTestSSE)
		}
	}
	assert.Equal(t, []int{4}, prewarms, "one prewarm through the last sealed quarter")
}

func TestSIWCPrefixPrewarmAppendOnlyConversations(t *testing.T) {
	for _, ttl := range []int{3600, 0} { // 0: every turn resumes after the warm TTL
		t.Run(fmt.Sprint("ttl ", ttl), func(t *testing.T) {
			siwcPrewarmTestSetup(t, func(s *model_setting.SIWCPrefixPrewarm) { s.WarmTTLSeconds = ttl })
			siwcPrewarmTestAppendOnly(t)
		})
	}
}

func siwcPrewarmTestAppendOnly(t *testing.T) {
	messages := []string{}
	agent := []map[string]any{{"role": "user", "content": "task " + strings.Repeat("t", 8000)}}
	for turn := 1; turn <= 12; turn++ {
		messages = append(messages, fmt.Sprintf("u:turn %d %s", turn, strings.Repeat("x", 3000)))
		assert.Nil(t, PlanSIWCPrefixPrewarm(siwcPrewarmTestInfo(), siwcPrewarmTestBody(t, messages...)), "chat turn %d", turn)
		messages = append(messages, fmt.Sprintf("a:answer %d", turn))

		agent = append(agent, map[string]any{"type": "function_call", "call_id": fmt.Sprint(turn), "name": "read", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": fmt.Sprint(turn), "output": strings.Repeat("o", 3000)})
		body, err := common.Marshal(map[string]any{"instructions": "AGENT", "input": agent, "prompt_cache_key": "agent"})
		require.NoError(t, err)
		assert.Nil(t, PlanSIWCPrefixPrewarm(siwcPrewarmTestInfo(), body), "agent turn %d", turn)
	}
}

func TestSIWCPrefixPrewarmScope(t *testing.T) {
	for _, tc := range []struct {
		name   string
		info   func(*relaycommon.RelayInfo)
		field  string
		value  any
		shared bool
	}{
		{name: "user", info: func(info *relaycommon.RelayInfo) { info.UserId++ }},
		{name: "token", info: func(info *relaycommon.RelayInfo) { info.TokenId++ }},
		{name: "upstream model", info: func(info *relaycommon.RelayInfo) { info.UpstreamModelName = "gpt-other" }},
		{name: "channel", info: func(info *relaycommon.RelayInfo) { info.ChannelId++ }},
		{name: "multi-key index", info: func(info *relaycommon.RelayInfo) { info.ChannelMultiKeyIndex++ }},
		{name: "SIWC credential", info: func(info *relaycommon.RelayInfo) { info.ApiKey = "other-access" }},
		{name: "session header", info: func(info *relaycommon.RelayInfo) { info.SIWCSessionID = "other" }},
		{name: "instructions", field: "instructions", value: "OTHER SYSTEM"},
		{name: "reasoning", field: "reasoning", value: map[string]string{"effort": "high"}},
		{name: "tools", field: "tools", value: []map[string]string{{"type": "function", "name": "f"}}},
		{name: "prompt_cache_key", field: "prompt_cache_key", value: "other"},
		{name: "service_tier", field: "service_tier", value: "priority"},
		{name: "text format", field: "text", value: map[string]any{"format": map[string]string{"type": "json_object"}}},
		{name: "temperature", field: "temperature", value: 0.2, shared: true},
		{name: "max_output_tokens", field: "max_output_tokens", value: 99, shared: true},
		{name: "metadata", field: "metadata", value: map[string]string{"run": "2"}, shared: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			siwcPrewarmTestSetup(t, nil)
			for round := 1; round <= 2; round++ {
				require.Nil(t, PlanSIWCPrefixPrewarm(siwcPrewarmTestInfo(), siwcForecastTestBody(t, "A", 1, fmt.Sprint(round))))
			}
			info := siwcPrewarmTestInfo()
			if tc.info != nil {
				tc.info(info)
			}
			body := siwcForecastTestBody(t, "A", 1, "3")
			if tc.field != "" {
				var err error
				body, err = sjson.SetBytes(body, tc.field, tc.value)
				require.NoError(t, err)
			}
			assert.Equal(t, tc.shared, PlanSIWCPrefixPrewarm(info, body) != nil)
		})
	}
}

func TestSIWCPrefixPrewarmWarmState(t *testing.T) {
	missUsage := &dto.Usage{InputTokens: 3000, PromptTokens: 3000}
	t.Run("TTL expiry re-prewarms", func(t *testing.T) {
		siwcPrewarmTestSetup(t, nil)
		PlanSIWCPrefixPrewarm(siwcPrewarmTestInfo(), siwcForecastTestBody(t, "A", 1, "1"))
		PlanSIWCPrefixPrewarm(siwcPrewarmTestInfo(), siwcForecastTestBody(t, "A", 1, "2"))
		prewarm := PlanSIWCPrefixPrewarm(siwcPrewarmTestInfo(), siwcForecastTestBody(t, "A", 1, "3"))
		require.NotNil(t, prewarm)
		model_setting.GetGlobalSettings().SIWCPrefixPrewarm.WarmTTLSeconds = 0 // the write expires at once
		siwcPrewarmTestComplete(prewarm, siwcPrewarmTestSSE)
		assert.NotNil(t, PlanSIWCPrefixPrewarm(siwcPrewarmTestInfo(), siwcForecastTestBody(t, "A", 1, "4")))
	})

	t.Run("feedback miss re-prewarms within max attempts", func(t *testing.T) {
		siwcPrewarmTestSetup(t, nil)
		round := 0
		plan := func() (*relaycommon.RelayInfo, *SIWCPrefixPrewarm) {
			round++
			info := siwcPrewarmTestInfo()
			return info, PlanSIWCPrefixPrewarm(info, siwcForecastTestBody(t, "A", 1, fmt.Sprint(round)))
		}
		plan()
		plan()
		_, prewarm := plan()
		require.NotNil(t, prewarm)
		siwcPrewarmTestComplete(prewarm, siwcPrewarmTestSSE)

		info, prewarm := plan()
		require.Nil(t, prewarm)
		require.Positive(t, info.SIWCWarmPrefixTokens)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		local, _ := gin.CreateTestContext(httptest.NewRecorder())
		common.SetContextKey(local, constant.ContextKeyLocalCountTokens, true)
		hit := &dto.Usage{InputTokens: 3000, PromptTokens: 3000, PromptTokensDetails: dto.InputTokenDetails{CachedTokens: (info.SIWCWarmPrefixTokens + 1) / 2}} // exactly half
		ObserveSIWCPrefixUsage(c, info, hit)
		ObserveSIWCPrefixUsage(c, info, &dto.Usage{PromptTokens: 3000}) // no upstream usage
		ObserveSIWCPrefixUsage(local, info, missUsage)                  // locally counted
		info, prewarm = plan()
		require.Nil(t, prewarm, "hits, estimates and missing usage keep the prefix warm")

		ObserveSIWCPrefixUsage(c, info, missUsage)
		_, prewarm = plan()
		require.NotNil(t, prewarm, "a miss marks the prefix cold")
		siwcPrewarmTestComplete(prewarm, siwcPrewarmTestSSE)

		info, prewarm = plan()
		require.Nil(t, prewarm)
		ObserveSIWCPrefixUsage(c, info, missUsage)
		_, prewarm = plan()
		assert.Nil(t, prewarm, "max_attempts per boundary reached")
	})

	t.Run("failed prewarm stays cold and releases its boundary", func(t *testing.T) {
		var logs bytes.Buffer
		common.LogWriterMu.Lock()
		previousWriter := gin.DefaultWriter
		gin.DefaultWriter = &logs
		common.LogWriterMu.Unlock()
		t.Cleanup(func() {
			common.LogWriterMu.Lock()
			gin.DefaultWriter = previousWriter
			common.LogWriterMu.Unlock()
		})
		for name, send := range map[string]func(context.Context, []byte) (*http.Response, error){
			"http error": func(context.Context, []byte) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
			},
			"response.failed": func(context.Context, []byte) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.created\"}\n\ndata: {\"type\":\"response.failed\"}\n\n"))}, nil
			},
			"no output": func(context.Context, []byte) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.created\"}\n\n"))}, nil
			},
			"transport error with a key in the URL": func(context.Context, []byte) (*http.Response, error) {
				return nil, &url.Error{Op: "Post", URL: "https://upstream.invalid/v1/responses?key=sk-secret-value", Err: io.ErrUnexpectedEOF}
			},
			"panic": func(context.Context, []byte) (*http.Response, error) { panic("send bug") },
			"stalled stream hits the timeout": func(ctx context.Context, _ []byte) (*http.Response, error) {
				reader, writer := io.Pipe()
				go func() { <-ctx.Done(); writer.CloseWithError(ctx.Err()) }()
				return &http.Response{StatusCode: http.StatusOK, Body: reader}, nil
			},
		} {
			t.Run(name, func(t *testing.T) {
				siwcPrewarmTestSetup(t, func(s *model_setting.SIWCPrefixPrewarm) { s.TimeoutSeconds = 1 })
				PlanSIWCPrefixPrewarm(siwcPrewarmTestInfo(), siwcForecastTestBody(t, "A", 1, "1"))
				PlanSIWCPrefixPrewarm(siwcPrewarmTestInfo(), siwcForecastTestBody(t, "A", 1, "2"))
				prewarm := PlanSIWCPrefixPrewarm(siwcPrewarmTestInfo(), siwcForecastTestBody(t, "A", 1, "3"))
				require.NotNil(t, prewarm)
				logs.Reset()
				func() {
					defer func() { _ = recover() }() // the relay pool's panic handler in production
					prewarm.run(send)
				}()
				assert.Zero(t, siwcPrefixes.inflight)
				assert.Contains(t, logs.String(), "SIWC prefix prewarm: outcome=")
				assert.NotContains(t, logs.String(), "sk-secret-value")
				assert.NotNil(t, PlanSIWCPrefixPrewarm(siwcPrewarmTestInfo(), siwcForecastTestBody(t, "A", 1, "4")))
			})
		}
	})
}

func TestSIWCPrefixPrewarmLimits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*model_setting.SIWCPrefixPrewarm)
		info   func(*relaycommon.RelayInfo)
		want   int  // prewarms for games A, B, C at round 3
		evict  bool // other games run between the sightings of A, B, C and their third request
		inert  bool // the request leaves no trace in the index
	}{
		{name: "defaults", want: 3},
		{name: "disabled", mutate: func(s *model_setting.SIWCPrefixPrewarm) { s.Enabled = false }, inert: true},
		{name: "not SIWC", info: func(info *relaycommon.RelayInfo) { info.IsSIWC = false }, inert: true},
		{name: "channel test", info: func(info *relaycommon.RelayInfo) { info.IsChannelTest = true }, inert: true},
		{name: "min_prefix_tokens", mutate: func(s *model_setting.SIWCPrefixPrewarm) { s.MinPrefixTokens = 3000 }},
		{name: "min_gain_tokens", mutate: func(s *model_setting.SIWCPrefixPrewarm) { s.MinGainTokens = 3000 }},
		{name: "min_prior_sightings", mutate: func(s *model_setting.SIWCPrefixPrewarm) { s.MinPriorSightings = 3 }},
		{name: "max_concurrent", mutate: func(s *model_setting.SIWCPrefixPrewarm) { s.MaxConcurrent = 2 }, want: 2},
		{name: "per-scope minute cap", mutate: func(s *model_setting.SIWCPrefixPrewarm) { s.MaxPerScopeMinute = 1 }, want: 1},
		{name: "LRU bound forgets sightings", mutate: func(s *model_setting.SIWCPrefixPrewarm) { s.MaxEntries = 20 }, evict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			siwcPrewarmTestSetup(t, tc.mutate)
			info := func() *relaycommon.RelayInfo {
				info := siwcPrewarmTestInfo()
				if tc.info != nil {
					tc.info(info)
				}
				return info
			}
			games := []string{"A", "B", "C"}
			for round := 1; round <= 2; round++ {
				for _, game := range games {
					require.Nil(t, PlanSIWCPrefixPrewarm(info(), siwcForecastTestBody(t, game, 1, fmt.Sprint(round))))
				}
			}
			if tc.evict {
				for i := range 30 {
					PlanSIWCPrefixPrewarm(info(), siwcForecastTestBody(t, fmt.Sprint("other", i), 1, "1"))
				}
				assert.LessOrEqual(t, siwcPrefixes.lru.Len(), 20)
			}
			got := 0
			for _, game := range games {
				if PlanSIWCPrefixPrewarm(info(), siwcForecastTestBody(t, game, 1, "3")) != nil {
					got++
				}
			}
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.inert, siwcPrefixes.lru.Len() == 0)
		})
	}
}

func TestSIWCPrefixPrewarmScopeRateMapIsBounded(t *testing.T) {
	siwcPrewarmTestSetup(t, nil)
	idle := []time.Time{time.Now().Add(-2 * time.Minute)}
	for i := range 2 * siwcPrefixScopeSweepAt {
		siwcPrefixes.scopeRuns[fmt.Sprint("idle scope ", i)] = idle
	}
	for round := 1; round <= 3; round++ {
		PlanSIWCPrefixPrewarm(siwcPrewarmTestInfo(), siwcForecastTestBody(t, "A", 1, fmt.Sprint(round)))
	}
	assert.Len(t, siwcPrefixes.scopeRuns, 1, "only the scope that just prewarmed stays")
}

func TestSIWCPrefixPrewarmConcurrentGames(t *testing.T) {
	siwcPrewarmTestSetup(t, func(s *model_setting.SIWCPrefixPrewarm) { s.MaxConcurrent = 4 })
	const games = 6
	for round := 1; round <= 2; round++ {
		for game := range games {
			require.Nil(t, PlanSIWCPrefixPrewarm(siwcPrewarmTestInfo(), siwcForecastTestBody(t, fmt.Sprint(game), 2, fmt.Sprint(round))))
		}
	}
	var mu sync.Mutex
	var prewarms []*SIWCPrefixPrewarm
	var wg sync.WaitGroup
	for worker := range games * 8 {
		wg.Go(func() {
			info := siwcPrewarmTestInfo()
			prewarm := PlanSIWCPrefixPrewarm(info, siwcForecastTestBody(t, fmt.Sprint(worker%games), 2, fmt.Sprint("w", worker)))
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			ObserveSIWCPrefixUsage(c, info, &dto.Usage{InputTokens: 5000})
			if prewarm != nil {
				mu.Lock()
				prewarms = append(prewarms, prewarm)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	// Concurrent requests of one game coalesce on the warming boundary; the global cap holds the rest.
	require.Len(t, prewarms, 4)
	targets := map[string]bool{}
	for _, prewarm := range prewarms {
		assert.Equal(t, 5, prewarm.items)
		targets[gjson.GetBytes(prewarm.body, "input.0.content").String()] = true
		siwcPrewarmTestComplete(prewarm, siwcPrewarmTestSSE) // sequential: logger's line counter is unsynchronized
	}
	assert.Len(t, targets, 4)
	assert.Zero(t, siwcPrefixes.inflight)
}

func TestSIWCPrefixPrewarmClosesAfterFirstOutputEvent(t *testing.T) {
	siwcPrewarmTestSetup(t, nil)
	closed := make(chan bool, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, siwcPrewarmTestSSE)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			closed <- true
		case <-time.After(5 * time.Second):
			closed <- false
		}
	}))
	defer server.Close()
	for round := 1; round <= 2; round++ {
		PlanSIWCPrefixPrewarm(siwcPrewarmTestInfo(), siwcForecastTestBody(t, "A", 1, fmt.Sprint(round)))
	}
	prewarm := PlanSIWCPrefixPrewarm(siwcPrewarmTestInfo(), siwcForecastTestBody(t, "A", 1, "3"))
	require.NotNil(t, prewarm)
	prewarm.run(func(ctx context.Context, body []byte) (*http.Response, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader(string(body)))
		if err != nil {
			return nil, err
		}
		return server.Client().Do(request)
	})
	assert.True(t, <-closed, "the prewarm closes the stream instead of waiting for the full response")
	info := siwcPrewarmTestInfo()
	assert.Nil(t, PlanSIWCPrefixPrewarm(info, siwcForecastTestBody(t, "A", 1, "4")))
	assert.Equal(t, prewarm.estTokens, info.SIWCWarmPrefixTokens)
}
