package relay

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	openaichannel "github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	relaytypes "github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/model_setting"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"golang.org/x/net/http/httpguts"
)

type policyTestTransport struct {
	path      string
	body      []byte
	calls     int
	response  string
	header    http.Header
	sessionID string
}

func (s *policyTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	s.calls++
	s.path = r.URL.Path
	s.sessionID = r.Header.Get("session_id")
	var err error
	s.body, err = io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if s.response != "" {
		header := s.header
		if header == nil {
			header = http.Header{"Content-Type": {"application/json"}}
		}
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(s.response)), Request: r}, nil
	}
	return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(`{"error":{"message":"policy test","type":"invalid_request_error"}}`)), Request: r}, nil
}

func TestSIWCRequestsSendPromptCacheSession(t *testing.T) {
	if service.GetHttpClient() == nil {
		service.InitHttpClient()
	}
	client := service.GetHttpClient()
	previousTransport := client.Transport
	t.Cleanup(func() { client.Transport = previousTransport })
	credential, err := common.Marshal(common.SIWCCredential{
		ClientID: "test-client", AccessToken: "test-access", RefreshToken: "test-refresh", ExpiresAt: 1,
	})
	require.NoError(t, err)

	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, tc := range []struct {
			name, caller, override, cacheKey, clientHeader, overrideHeader string
		}{
			{name: "generated"},
			{name: "caller", caller: "caller-cache"},
			{name: "channel override", override: "channel-cache", overrideHeader: "channel-cache"},
			{name: "body override", cacheKey: "body-cache"},
			{name: "newline body key", cacheKey: "body\ncache"},
			{name: "caller and body mismatch", caller: "caller-cache", cacheKey: "body-cache"},
			{name: "channel override applied last", caller: "caller-cache", override: "channel-cache", overrideHeader: "channel-cache"},
			{name: "client header override", override: "{client_header:X-Session-Id}", clientHeader: "client-cache", overrideHeader: "client-cache"},
			{name: "missing client header override", override: "{client_header:X-Session-Id}"},
			{name: "blank client header override", override: "{client_header:X-Session-Id}", clientHeader: " \t "},
			{name: "API key override excluded from body", override: "prefix-{api_key}", overrideHeader: "prefix-test-access"},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				transport := &policyTestTransport{response: `{"id":"resp_test","object":"response","model":"gpt-test","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`}
				client.Transport = transport
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, path, nil)
				if tc.caller != "" {
					c.Request.Header.Set("session_id", tc.caller)
				}
				if tc.clientHeader != "" {
					c.Request.Header.Set("X-Session-Id", tc.clientHeader)
				}
				common.SetContextKey(c, constant.ContextKeyOriginalModel, "gpt-test")
				common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
				common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "https://siwc.invalid")
				common.SetContextKey(c, constant.ContextKeyChannelKey, string(credential))
				if tc.override != "" {
					common.SetContextKey(c, constant.ContextKeyChannelHeaderOverride, map[string]any{"session_id": tc.override})
				}
				if tc.cacheKey != "" {
					common.SetContextKey(c, constant.ContextKeyChannelParamOverride, map[string]any{"prompt_cache_key": tc.cacheKey})
				}
				var info *relaycommon.RelayInfo
				if path == "/v1/chat/completions" {
					var request dto.GeneralOpenAIRequest
					require.NoError(t, common.Unmarshal([]byte(`{"model":"gpt-test","messages":[
						{"role":"system","content":[{"type":"text","text":"system rules","prompt_cache_breakpoint":{"mode":"explicit"}}]},
						{"role":"developer","content":[{"type":"text","text":"developer rules","prompt_cache_breakpoint":{"mode":"explicit"}}]},
						{"role":"user","content":[{"type":"text","text":"hello","prompt_cache_breakpoint":{"mode":"explicit"}}]}
					]}`), &request))
					info = relaycommon.GenRelayInfoOpenAI(c, &request)
					info.InitChannelMeta(c)
					adaptor := GetAdaptor(info.ApiType)
					adaptor.Init(info)
					_, apiErr := textRequestViaResponses(c, info, adaptor, &request)
					require.Nil(t, apiErr)
					assert.JSONEq(t, `[
						{"role":"developer","content":[{"type":"input_text","text":"system rules"}]},
						{"role":"developer","content":[{"type":"input_text","text":"developer rules"}]},
						{"role":"user","content":[{"type":"input_text","text":"hello"}]}
					]`, gjson.GetBytes(transport.body, "input").Raw)
				} else {
					request := &dto.OpenAIResponsesRequest{Model: "gpt-test", Input: []byte(`[{"role":"developer","content":[{"type":"input_text","text":"rules","prompt_cache_breakpoint":{"mode":"explicit"}}]},{"role":"user","content":[{"type":"input_text","text":"hello","prompt_cache_breakpoint":{"mode":"explicit"}}]}]`),
						Instructions: json.RawMessage(`[{"type":"input_text","text":"prefix","prompt_cache_breakpoint":{"mode":"explicit"}}]`)}
					info = relaycommon.GenRelayInfoResponses(c, request)
					adaptor, body, closer, apiErr := PrepareResponsesRequest(c, info, request)
					require.Nil(t, apiErr)
					defer closer.Close()
					response, err := adaptor.DoRequest(c, info, body)
					require.NoError(t, err)
					require.IsType(t, &http.Response{}, response)
					require.NoError(t, response.(*http.Response).Body.Close())
				}
				assert.True(t, info.IsSIWC)
				assert.Equal(t, 1, transport.calls)
				assert.Equal(t, "/v1/responses", transport.path)
				assert.NotEmpty(t, info.SIWCSessionID)
				wantHeader := info.SIWCSessionID
				if tc.caller != "" {
					assert.Equal(t, tc.caller, info.SIWCSessionID)
				} else if strings.TrimSpace(tc.clientHeader) != "" {
					assert.Equal(t, strings.TrimSpace(tc.clientHeader), info.SIWCSessionID)
				}
				if tc.overrideHeader != "" {
					wantHeader = tc.overrideHeader
				}
				wantKey := info.SIWCSessionID
				if tc.cacheKey != "" {
					wantKey = tc.cacheKey
				}
				assert.Equal(t, wantHeader, transport.sessionID)
				assert.True(t, httpguts.ValidHeaderFieldValue(transport.sessionID))
				assert.Equal(t, wantKey, gjson.GetBytes(transport.body, "prompt_cache_key").String())
				assert.NotContains(t, string(transport.body), "prompt_cache_breakpoint")
				assert.NotContains(t, string(transport.body), "{client_header:")
				assert.NotContains(t, string(transport.body), "{api_key}")
				assert.NotContains(t, string(transport.body), "test-access")
			})
		}
	}
}

func TestNativeChatWithoutBreakpointsPreservesRequestBytes(t *testing.T) {
	if service.GetHttpClient() == nil {
		service.InitHttpClient()
	}
	client := service.GetHttpClient()
	previousTransport := client.Transport
	previousSettings := *model_setting.GetGlobalSettings()
	t.Cleanup(func() {
		client.Transport = previousTransport
		*model_setting.GetGlobalSettings() = previousSettings
	})
	model_setting.GetGlobalSettings().ChatCompletionsToResponsesPolicy.Enabled = false
	const raw = `{
		"model": "gpt-test", "messages": [
			{"role":"system","content":"rules"},
			{"role":"user","content":[{"type":"text","text":"question"}]},
			{"role":"assistant","content":[{"type":"text","text":"answer"}]},
			{"role":"tool","tool_call_id":"call_1","content":[{"type":"text","text":"result"}]}
		]
	}`
	const encoded = `{"model":"gpt-test","messages":[{"role":"system","content":"rules"},{"role":"user","content":[{"text":"question","type":"text"}]},{"role":"assistant","content":[{"text":"answer","type":"text"}]},{"role":"tool","content":[{"text":"result","type":"text"}],"tool_call_id":"call_1"}]}`
	for _, mode := range []string{"native", "channel passthrough", "global passthrough"} {
		t.Run(mode, func(t *testing.T) {
			model_setting.GetGlobalSettings().PassThroughRequestEnabled = mode == "global passthrough"
			transport := &policyTestTransport{}
			client.Transport = transport
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(raw))
			common.SetContextKey(c, constant.ContextKeyOriginalModel, "gpt-test")
			common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
			common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "https://openai.invalid")
			common.SetContextKey(c, constant.ContextKeyChannelKey, "test-key")
			common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{PassThroughBodyEnabled: mode == "channel passthrough"})
			var request dto.GeneralOpenAIRequest
			require.NoError(t, common.Unmarshal([]byte(raw), &request))
			info := relaycommon.GenRelayInfoOpenAI(c, &request)
			require.NotNil(t, TextHelper(c, info)) // The transport captures the request, then returns a fixture error.
			assert.Equal(t, 1, transport.calls)
			assert.Equal(t, "/v1/chat/completions", transport.path)
			want := raw
			if mode == "native" {
				want = encoded
			}
			assert.Equal(t, want, string(transport.body))
		})
	}
}

func TestPrepareResponsesRequestPreservesPromptCacheBreakpoints(t *testing.T) {
	const breakpoint = `{"mode":"explicit","z":9007199254740993,"a":[false,null,"future"]}`
	const input = `[{"role":"user","content":[{"type":"input_text","text":"hello","prompt_cache_breakpoint":` + breakpoint + `}]}]`
	var request dto.OpenAIResponsesRequest
	require.NoError(t, common.Unmarshal([]byte(`{"model":"gpt-test","input":`+input+`}`), &request))
	assert.Equal(t, input, string(request.Input))
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	common.SetContextKey(c, constant.ContextKeyOriginalModel, "gpt-test")
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
	common.SetContextKey(c, constant.ContextKeyChannelKey, "test-key")
	info := relaycommon.GenRelayInfoResponses(c, &request)
	_, body, closer, apiErr := PrepareResponsesRequest(c, info, &request)
	require.Nil(t, apiErr)
	defer closer.Close()
	encoded, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.False(t, info.IsSIWC)
	assert.Equal(t, input, gjson.GetBytes(encoded, "input").Raw)
	assert.Equal(t, breakpoint, gjson.GetBytes(encoded, "input.0.content.0.prompt_cache_breakpoint").Raw)
}

func TestChatResponsesPolicyMatchesMappedModel(t *testing.T) {
	if service.GetHttpClient() == nil {
		service.InitHttpClient()
	}
	client := service.GetHttpClient()
	previousTransport := client.Transport
	previous := model_setting.GetGlobalSettings().ChatCompletionsToResponsesPolicy
	previousPassThrough := model_setting.GetGlobalSettings().PassThroughRequestEnabled
	t.Cleanup(func() {
		client.Transport = previousTransport
		model_setting.GetGlobalSettings().ChatCompletionsToResponsesPolicy = previous
		model_setting.GetGlobalSettings().PassThroughRequestEnabled = previousPassThrough
	})
	model_setting.GetGlobalSettings().PassThroughRequestEnabled = false
	model_setting.GetGlobalSettings().ChatCompletionsToResponsesPolicy = model_setting.ChatCompletionsToResponsesPolicy{
		Enabled: true, ChannelTypes: []int{constant.ChannelTypeOpenAI}, ModelPatterns: []string{"^gpt-"},
	}
	for _, format := range []relaytypes.RelayFormat{relaytypes.RelayFormatOpenAI, relaytypes.RelayFormatClaude} {
		for _, tc := range []struct {
			name, model, mapping, path string
		}{
			{"mapped to GPT", "gemini-x", `{"gemini-x":"gpt-6.1-sol"}`, "/v1/responses"},
			{"mapped away from GPT", "gpt-x", `{"gpt-x":"non-gpt"}`, "/v1/chat/completions"},
			{"unmapped GPT", "gpt-6.1-sol", "", "/v1/responses"},
			{"unmapped other", "gemini-x", "", "/v1/chat/completions"},
		} {
			t.Run(string(format)+"/"+tc.name, func(t *testing.T) {
				transport := &policyTestTransport{}
				client.Transport = transport
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				common.SetContextKey(c, constant.ContextKeyOriginalModel, tc.model)
				common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
				common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "https://policy.invalid")
				c.Set("model_mapping", tc.mapping)
				info := &relaycommon.RelayInfo{OriginModelName: tc.model, RelayMode: relayconstant.RelayModeChatCompletions, RelayFormat: format, RequestURLPath: "/v1/chat/completions"}
				var apiErr *relaytypes.NewAPIError
				if format == relaytypes.RelayFormatClaude {
					info.Request = &dto.ClaudeRequest{Model: tc.model, Messages: []dto.ClaudeMessage{{Role: "user", Content: "hello"}}}
					apiErr = ClaudeHelper(c, info)
				} else {
					info.Request = &dto.GeneralOpenAIRequest{Model: tc.model, Messages: []dto.Message{{Role: "user", Content: "hello"}}}
					apiErr = TextHelper(c, info)
				}
				require.NotNil(t, apiErr)
				assert.Equal(t, tc.path, transport.path, "%v", apiErr)
			})
		}
	}
}

func TestChatViaResponsesAddsPromptCacheAutoBreakpoints(t *testing.T) {
	if service.GetHttpClient() == nil {
		service.InitHttpClient()
	}
	client := service.GetHttpClient()
	previousTransport := client.Transport
	previousSettings := *model_setting.GetGlobalSettings()
	t.Cleanup(func() {
		client.Transport = previousTransport
		*model_setting.GetGlobalSettings() = previousSettings
	})
	model_setting.GetGlobalSettings().PassThroughRequestEnabled = false
	model_setting.GetGlobalSettings().ChatCompletionsToResponsesPolicy = model_setting.ChatCompletionsToResponsesPolicy{Enabled: true, AllChannels: true, ModelPatterns: []string{"^gpt-"}}
	transport := &policyTestTransport{}
	client.Transport = transport
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	common.SetContextKey(c, constant.ContextKeyOriginalModel, "gpt-6.1-sol")
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "https://openai.invalid")
	common.SetContextKey(c, constant.ContextKeyChannelKey, "test-key")
	info := &relaycommon.RelayInfo{OriginModelName: "gpt-6.1-sol", RelayMode: relayconstant.RelayModeChatCompletions, RelayFormat: relaytypes.RelayFormatOpenAI, RequestURLPath: "/v1/chat/completions"}
	info.Request = &dto.GeneralOpenAIRequest{Model: "gpt-6.1-sol", Messages: []dto.Message{
		{Role: "system", Content: "rules"},
		{Role: "user", Content: "PREGAME"},
		{Role: "assistant", Content: "Acknowledged."},
		{Role: "user", Content: "dynamic"},
	}}
	require.NotNil(t, TextHelper(c, info)) // The transport captures the request, then returns a fixture error.
	assert.False(t, info.IsSIWC)
	assert.Equal(t, "/v1/responses", transport.path)
	assert.Equal(t, "rules", gjson.GetBytes(transport.body, "instructions").String())
	assert.JSONEq(t, `[
		{"role":"user","content":[{"type":"input_text","text":"PREGAME","prompt_cache_breakpoint":{"mode":"explicit"}}]},
		{"role":"assistant","content":"Acknowledged."},
		{"role":"user","content":"dynamic"}
	]`, gjson.GetBytes(transport.body, "input").Raw)
	assert.False(t, gjson.GetBytes(transport.body, "prompt_cache_options").Exists())
}

func TestPrepareResponsesRequestRetainsConvertedAdaptor(t *testing.T) {
	type capturedRequest struct {
		path string
		body []byte
	}
	captured := make(chan capturedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		captured <- capturedRequest{path: r.URL.Path, body: body}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
	}))
	defer server.Close()

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("Content-Type", "application/json")
	common.SetContextKey(c, constant.ContextKeyOriginalModel, "gpt-4o")
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeAdvancedCustom)
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, server.URL)
	common.SetContextKey(c, constant.ContextKeyChannelKey, "test-key")
	common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, dto.ChannelOtherSettings{
		AdvancedCustom: &dto.AdvancedCustomConfig{Routes: []dto.AdvancedCustomRoute{{
			IncomingPath: "/v1/responses", UpstreamPath: "/v1/chat/completions", Converter: relayconvert.ConverterOpenAIResponsesToOpenAIChat,
		}}},
	})
	request := &dto.OpenAIResponsesRequest{Model: "gpt-4o", Input: []byte(`"hello"`)}
	info := relaycommon.GenRelayInfoResponses(c, request)
	adaptor, body, closer, apiErr := PrepareResponsesRequest(c, info, request)
	require.Nil(t, apiErr)
	defer closer.Close()
	response, err := adaptor.DoRequest(c, info, body)
	require.NoError(t, err)
	httpResponse, ok := response.(*http.Response)
	require.True(t, ok)
	usage, apiErr := adaptor.DoResponse(c, httpResponse, info)
	require.Nil(t, apiErr)
	require.IsType(t, &dto.Usage{}, usage)
	assert.Equal(t, 5, usage.(*dto.Usage).TotalTokens)

	upstream := <-captured
	assert.Equal(t, "/v1/chat/completions", upstream.path)
	var upstreamRequest dto.GeneralOpenAIRequest
	require.NoError(t, common.Unmarshal(upstream.body, &upstreamRequest))
	require.Len(t, upstreamRequest.Messages, 1)
	assert.Equal(t, "hello", upstreamRequest.Messages[0].StringContent())
	assert.Equal(t, []relaytypes.RelayFormat{relaytypes.RelayFormatOpenAIResponses, relaytypes.RelayFormatOpenAI}, info.RequestConversionChain)
}

func TestIsResponsesEventStreamContentType(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		want        bool
	}{
		{name: "plain", contentType: "text/event-stream", want: true},
		{name: "mixed case with charset", contentType: "Text/Event-Stream; charset=utf-8", want: true},
		{name: "json", contentType: "application/json", want: false},
		{name: "empty", contentType: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isResponsesEventStreamContentType(tt.contentType))
		})
	}
}

func TestTextRequestViaResponsesBuffersSSE(t *testing.T) {
	if service.GetHttpClient() == nil {
		service.InitHttpClient()
	}
	client := service.GetHttpClient()
	previousTransport := client.Transport
	t.Cleanup(func() { client.Transport = previousTransport })

	start := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"
	completed := "event: response.completed\ndata: " + `{"type":"response.completed","response":{"id":"resp_test","model":"gpt-6.1-terra","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}` + "\n\n"
	failed := "event: response.failed\ndata: " + `{"type":"response.failed","response":{"status":"failed","error":{"type":"server_error","message":"upstream failed"}}}` + "\n\n"
	jsonBody := `{"id":"resp_test","object":"response","model":"gpt-6.1-terra","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`
	for _, tc := range []struct {
		name, contentType, body string
		wantError               bool
	}{
		{name: "SSE header", contentType: "text/event-stream", body: start + completed},
		{name: "missing header event first", body: start + completed},
		{name: "missing header data first", body: strings.ReplaceAll(start+completed, "event: response.output_text.delta\n", "")},
		{name: "missing header comment first", body: ": keepalive\n\n" + start + completed},
		{name: "missing header JSON", body: jsonBody},
		{name: "failed stream", body: start + failed, wantError: true},
		{name: "interrupted stream", contentType: "text/event-stream", body: start, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := &policyTestTransport{response: tc.body, header: make(http.Header)}
			if tc.contentType != "" {
				transport.header.Set("Content-Type", tc.contentType)
			}
			client.Transport = transport
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			const body = `{"model":"gemini-3.7-flash","stream":false,"messages":[{"role":"user","content":"hello"}]}`
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			common.SetContextKey(c, constant.ContextKeyOriginalModel, "gemini-3.7-flash")
			common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
			common.SetContextKey(c, constant.ContextKeyChannelId, 17)
			common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "https://siwc.invalid")
			c.Set("model_mapping", `{"gemini-3.7-flash":"gpt-6.1-terra"}`)
			var request dto.GeneralOpenAIRequest
			require.NoError(t, common.Unmarshal([]byte(body), &request))
			info := relaycommon.GenRelayInfoOpenAI(c, &request)
			info.InitChannelMeta(c)
			info.ParamOverride = map[string]any{"stream": true, "store": false}
			require.NoError(t, helper.ModelMappedHelper(c, info, &request))
			adaptor := GetAdaptor(info.ApiType)
			adaptor.Init(info)

			usage, apiErr := textRequestViaResponses(c, info, adaptor, &request)

			assert.False(t, info.IsStream)
			assert.False(t, request.IsStream(c.Request))
			assert.Equal(t, "/v1/responses", transport.path)
			var upstream map[string]any
			require.NoError(t, common.Unmarshal(transport.body, &upstream))
			assert.Equal(t, "gpt-6.1-terra", upstream["model"])
			assert.Equal(t, true, upstream["stream"])
			assert.Equal(t, false, upstream["store"])
			if tc.wantError {
				require.NotNil(t, apiErr)
				assert.Empty(t, recorder.Body.String())
				return
			}
			require.Nil(t, apiErr)
			require.NotNil(t, usage)
			assert.Equal(t, 5, usage.TotalTokens)
			assert.Equal(t, http.StatusOK, recorder.Code)
			assert.Contains(t, recorder.Header().Get("Content-Type"), "application/json")
			var response dto.OpenAITextResponse
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
			assert.Equal(t, "chat.completion", response.Object)
			require.Len(t, response.Choices, 1)
			assert.Equal(t, "ok", response.Choices[0].Message.StringContent())
		})
	}
}

func TestRecalcQuotaFromRatiosIgnoresInvalidMultipliers(t *testing.T) {
	info := &relaycommon.RelayInfo{
		PriceData: hosttypes.PriceData{
			Quota: 100,
		},
	}
	info.PriceData.AddOtherRatio("duration", 2)

	quota, ok := recalcQuotaFromRatios(info, map[string]float64{
		"duration": 3,
		"zero":     0,
		"negative": -1,
		"nan":      math.NaN(),
		"inf":      math.Inf(1),
	})

	require.True(t, ok)
	assert.Equal(t, 150, quota)
	assert.True(t, info.PriceData.HasOtherRatio("duration"))
}

func TestRecalcQuotaFromRatiosRejectsAllInvalidAdjustedRatios(t *testing.T) {
	info := &relaycommon.RelayInfo{
		PriceData: hosttypes.PriceData{
			Quota: 100,
		},
	}
	info.PriceData.AddOtherRatio("duration", 2)

	quota, ok := recalcQuotaFromRatios(info, map[string]float64{
		"zero":     0,
		"negative": -1,
		"nan":      math.NaN(),
		"inf":      math.Inf(1),
	})

	require.False(t, ok)
	assert.Equal(t, 0, quota)
	assert.True(t, info.PriceData.HasOtherRatio("duration"))
}

func TestTextRequestViaResponsesConvertsClaudeDirectly(t *testing.T) {
	type capturedRequest struct {
		path string
		body []byte
	}
	captured := make(chan capturedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		captured <- capturedRequest{path: r.URL.Path, body: body}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"resp_1",
			"object":"response",
			"status":"completed",
			"model":"gpt-5.6-sol",
			"output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],
			"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}
		}`))
	}))
	defer server.Close()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("Content-Type", "application/json")

	info := &relaycommon.RelayInfo{
		RelayMode:              relayconstant.RelayModeChatCompletions,
		RelayFormat:            relaytypes.RelayFormatClaude,
		OriginModelName:        "gpt-5.6-sol",
		RequestConversionChain: []relaytypes.RelayFormat{relaytypes.RelayFormatClaude},
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeOpenAI,
			ChannelBaseUrl:    server.URL,
			ApiKey:            "test-key",
			UpstreamModelName: "gpt-5.6-sol",
		},
	}
	adaptor := &openaichannel.Adaptor{}
	adaptor.Init(info)
	request := &dto.ClaudeRequest{
		Model:    "gpt-5.6-sol",
		Thinking: &dto.Thinking{Type: "adaptive", Display: "summarized"},
		Messages: []dto.ClaudeMessage{{Role: "user", Content: "hello"}},
	}

	usage, apiErr := textRequestViaResponses(c, info, adaptor, request)

	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, 5, usage.TotalTokens)
	assert.Equal(t, []relaytypes.RelayFormat{relaytypes.RelayFormatClaude, relaytypes.RelayFormatOpenAIResponses}, info.RequestConversionChain)

	upstream := <-captured
	assert.Equal(t, "/v1/responses", upstream.path)
	var upstreamBody map[string]any
	require.NoError(t, common.Unmarshal(upstream.body, &upstreamBody))
	assert.NotContains(t, upstreamBody, "messages")
	reasoning, ok := upstreamBody["reasoning"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "high", reasoning["effort"])
	assert.Equal(t, "detailed", reasoning["summary"])

	var response dto.ClaudeResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.Len(t, response.Content, 1)
	assert.Equal(t, "ok", response.Content[0].GetText())
}

func TestApplySystemPromptIfNeededSkipsToolLoadingMessages(t *testing.T) {
	tools := json.RawMessage(`[{"type":"function","function":{"name":"get_current_time","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]`)
	toolLoading := dto.Message{Role: "system", Tools: tools}
	user := dto.Message{Role: "user", Content: "What time is it in Beijing?"}

	tests := []struct {
		name         string
		messages     []dto.Message
		wantMessages []dto.Message
		wantOverride bool
	}{
		{
			name:     "tool loading message alone is not a system prompt",
			messages: []dto.Message{toolLoading, user},
			wantMessages: []dto.Message{
				{Role: "system", Content: "Answer in English."},
				toolLoading,
				user,
			},
		},
		{
			name:     "override targets the real system prompt only",
			messages: []dto.Message{toolLoading, {Role: "system", Content: "You are Kimi."}, user},
			wantMessages: []dto.Message{
				toolLoading,
				{Role: "system", Content: "Answer in English.\nYou are Kimi."},
				user,
			},
			wantOverride: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			info := &relaycommon.RelayInfo{
				ChannelMeta: &relaycommon.ChannelMeta{
					ChannelSetting: dto.ChannelSettings{
						SystemPrompt:         "Answer in English.",
						SystemPromptOverride: true,
					},
				},
			}
			request := &dto.GeneralOpenAIRequest{
				Model:    "kimi-k3",
				Messages: append([]dto.Message(nil), tt.messages...),
			}

			applySystemPromptIfNeeded(c, info, request)

			require.Len(t, request.Messages, len(tt.wantMessages))
			for i, want := range tt.wantMessages {
				got := request.Messages[i]
				assert.Equal(t, want.Role, got.Role, "message %d role", i)
				assert.Equal(t, want.Content, got.Content, "message %d content", i)
				if len(want.Tools) > 0 {
					assert.JSONEq(t, string(want.Tools), string(got.Tools), "message %d tools", i)
				} else {
					assert.Empty(t, got.Tools, "message %d tools", i)
				}
			}
			_, overrideSet := common.GetContextKey(c, constant.ContextKeySystemPromptOverride)
			assert.Equal(t, tt.wantOverride, overrideSet)
		})
	}
}

// forecast-style Chat web search on a Responses-only OpenAI model: the hosted
// tool must reach /v1/responses, unsupported sampling parameters must be
// dropped like the Chat adaptor does, and citations must come back with spans.
func TestTextRequestViaResponsesChatWebSearchDropsUnsupportedSampling(t *testing.T) {
	var upstreamBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed","model":"gpt-6.1-sol","output":[
			{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"nba"}},
			{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Nuggets beat Bulls 133-128.","annotations":[{"type":"url_citation","url":"https://nba.com/x","title":"NBA","start_index":0,"end_index":27}]}]}
		],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`))
	}))
	defer server.Close()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	common.SetContextKey(c, constant.ContextKeyOriginalModel, "gpt-6.1-sol")
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, server.URL)
	common.SetContextKey(c, constant.ContextKeyChannelKey, "test-key")

	var request dto.GeneralOpenAIRequest
	require.NoError(t, common.Unmarshal([]byte(`{"model":"gpt-6.1-sol","temperature":0.2,"top_p":0.9,
		"messages":[{"role":"user","content":"q"}],
		"web_search_options":{"search_context_size":"low"}}`), &request))
	info := relaycommon.GenRelayInfoOpenAI(c, &request)
	info.InitChannelMeta(c)
	adaptor := GetAdaptor(info.ApiType)
	adaptor.Init(info)

	_, apiErr := textRequestViaResponses(c, info, adaptor, &request)
	require.Nil(t, apiErr)

	var upstream map[string]any
	require.NoError(t, common.Unmarshal(upstreamBody, &upstream))
	assert.NotContains(t, upstream, "temperature")
	assert.NotContains(t, upstream, "top_p")
	assert.Equal(t, []any{map[string]any{"type": "web_search", "search_context_size": "low"}}, upstream["tools"])

	require.NotNil(t, info.ResponsesUsageInfo)
	assert.Equal(t, 1, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolWebSearch].CallCount)
	assert.NotContains(t, info.ResponsesUsageInfo.BuiltInTools, dto.BuildInToolWebSearchPreview)

	var response dto.OpenAITextResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.Len(t, response.Choices, 1)
	assert.JSONEq(t, `[{"type":"url_citation","url_citation":{"url":"https://nba.com/x","title":"NBA","start_index":0,"end_index":27}}]`,
		string(response.Choices[0].Message.Annotations))
}
