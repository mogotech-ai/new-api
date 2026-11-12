package service

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"golang.org/x/net/http/httpguts"
)

func TestApplySIWCResponsesRequirementsNonSIWC(t *testing.T) {
	body := []byte(`{"stream":false,"store":true,"input":[{"role":"user","content":[{"type":"input_text","text":"hello","prompt_cache_breakpoint":{ "mode": "explicit" }}]}],"instructions":[{"type":"input_text","text":"rules","prompt_cache_breakpoint":{"mode":"explicit"}}],"prompt_cache_retention":"24h","prompt_cache_options":{"foo":true},"cache_salt":"tenant","prompt_cache_key":"caller-cache"}`)
	for _, info := range []*relaycommon.RelayInfo{nil, {}, {ChannelMeta: &relaycommon.ChannelMeta{}}} {
		got, err := ApplySIWCResponsesRequirements(info, body)
		require.NoError(t, err)
		assert.Equal(t, body, got)
		if info != nil {
			assert.Empty(t, info.SIWCSessionID)
		}
	}
}

func TestApplySIWCResponsesRequirementsPromptCacheBreakpoints(t *testing.T) {
	for _, tc := range []struct {
		name, fields, want string
	}{
		{
			name: "input and instruction content parts",
			fields: `"input":[
				{"role":"developer","content":[{"type":"input_text","text":"rules","prompt_cache_breakpoint":{"mode":"explicit"}},{"type":"input_text","text":"more","prompt_cache_breakpoint":null}]},
				{"role":"user","content":[{"type":"input_text","text":"hello","prompt_cache_breakpoint":false},{"type":"input_image","image_url":"image","prompt_cache_breakpoint":{}}]},
				{"role":"assistant","content":[{"type":"output_text","text":"answer","prompt_cache_breakpoint":{"mode":"explicit"}}]},
				{"role":"user","content":"continue"}
			],"instructions":[{"type":"input_text","text":"prefix","prompt_cache_breakpoint":{"mode":"explicit"}},{"type":"input_text","text":"suffix","prompt_cache_breakpoint":null}]`,
			want: `"input":[
				{"role":"developer","content":[{"type":"input_text","text":"rules"},{"type":"input_text","text":"more"}]},
				{"role":"user","content":[{"type":"input_text","text":"hello"},{"type":"input_image","image_url":"image"}]},
				{"role":"assistant","content":[{"type":"output_text","text":"answer"}]},
				{"role":"user","content":"continue"}
			],"instructions":[{"type":"input_text","text":"prefix"},{"type":"input_text","text":"suffix"}]`,
		},
		{
			name:   "duplicate breakpoint keys",
			fields: `"input":[{"role":"developer","content":[{"type":"input_text","text":"rules","prompt_cache_breakpoint":null,"prompt_cache_breakpoint":{"mode":"explicit"}}]}],"instructions":[{"type":"input_text","text":"prefix","prompt_cache_breakpoint":{},"prompt_cache_breakpoint":false}]`,
			want:   `"input":[{"role":"developer","content":[{"type":"input_text","text":"rules"}]}],"instructions":[{"type":"input_text","text":"prefix"}]`,
		},
		{
			name:   "absent breakpoint preserves content bytes",
			fields: `"input": [ { "content": [ { "text": "hello", "type": "input_text" } ], "role": "user" } ], "instructions": [ { "text": "rules", "type": "input_text" } ]`,
		},
		{
			name:   "string instructions and content are unchanged",
			fields: `"input":[{"role":"user","content":"prompt_cache_breakpoint"}],"instructions":"prompt_cache_breakpoint"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{IsSIWC: true}}
			const prefix = `{"stream":true,"store":false,"prompt_cache_key":"caller-cache",`
			body := []byte(prefix + tc.fields + `}`)
			got, err := ApplySIWCResponsesRequirements(info, body)
			require.NoError(t, err)
			if tc.want == "" {
				assert.Equal(t, body, got)
			} else {
				assert.JSONEq(t, prefix+tc.want+`}`, string(got))
			}
		})
	}
}

func TestApplySIWCResponsesRequirementsCacheFields(t *testing.T) {
	generatedSession := fmt.Sprintf("%x", sha256.Sum256([]byte("7\n9\ngpt-test\nhello")))
	for _, tc := range []struct {
		name, fields, wantKey string
	}{
		{"explicit cache key", `,"prompt_cache_retention":"24h","prompt_cache_options":{"foo":true},"cache_salt":"tenant","prompt_cache_key":"caller-cache"`, "caller-cache"},
		{"injected cache key", `,"prompt_cache_retention":"24h","prompt_cache_options":{"foo":true},"cache_salt":"tenant"`, generatedSession},
		{"absent cache fields", "", generatedSession},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := &relaycommon.RelayInfo{TokenId: 7, UserId: 9, ChannelMeta: &relaycommon.ChannelMeta{IsSIWC: true, UpstreamModelName: "gpt-test"}}
			body, err := ApplySIWCResponsesRequirements(info, []byte(`{"stream":false,"store":true,"input":"hello","extra":{ "z": 1, "a": 2 }`+tc.fields+`}`))
			require.NoError(t, err)
			for _, field := range []string{"prompt_cache_retention", "prompt_cache_options", "cache_salt"} {
				assert.False(t, gjson.GetBytes(body, field).Exists(), field)
			}
			assert.Equal(t, tc.wantKey, gjson.GetBytes(body, "prompt_cache_key").String())
			assert.Equal(t, tc.wantKey, info.SIWCSessionID)
			assert.Equal(t, gjson.True, gjson.GetBytes(body, "stream").Type)
			assert.Equal(t, gjson.False, gjson.GetBytes(body, "store").Type)
			assert.JSONEq(t, `[{"role":"user","content":"hello"}]`, gjson.GetBytes(body, "input").Raw)
			assert.Equal(t, `{ "z": 1, "a": 2 }`, gjson.GetBytes(body, "extra").Raw)
		})
	}
}

func TestApplySIWCResponsesRequirementsGeneratedSession(t *testing.T) {
	longPrefix := strings.Repeat("界", 11_000)
	for _, tc := range []struct {
		name, body, prefix string
	}{
		{"instructions", `{"instructions":"Answer briefly"}`, "Answer briefly"},
		// A shared system prompt alone would put every conversation of a token on one cache key.
		{"instructions and first user", `{"instructions":"Answer briefly","input":[{"role":"assistant","content":"ignored"},{"role":"user","content":"game 1"},{"role":"user","content":"later"}]}`, "Answer briefly\n\ngame 1"},
		{"instructions and string input", `{"instructions":"Answer briefly","input":"hello"}`, "Answer briefly\n\nhello"},
		{"both byte caps", fmt.Sprintf(`{"instructions":%q,"input":[{"role":"user","content":%q}]}`, longPrefix, "x"+longPrefix), longPrefix[:32*1024] + "\n\n" + ("x" + longPrefix)[:32*1024]},
		{"instruction whitespace preserved", `{"instructions":"  Answer briefly  "}`, "  Answer briefly  "},
		{"instruction text blocks", `{"instructions":[{"type":"text","text":"Answer "},{"type":"text","text":"briefly"}]}`, "Answer briefly"},
		{"instruction text object", `{"instructions":{"text":"Answer briefly"}}`, "Answer briefly"},
		{"instruction byte cap", fmt.Sprintf(`{"instructions":%q}`, longPrefix), longPrefix[:32*1024]},
		{"string input", `{"stream":false,"store":true,"input":"hello"}`, "hello"},
		{"first user message", `{"input":[{"role":"assistant","content":"ignored"},{"role":"user","content":"hello"},{"role":"user","content":"later"}]}`, "hello"},
		{"user text blocks", `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"ignored"},{"type":"input_text","text":"hello"},{"type":"input_text","text":" world"}]}]}`, "hello world"},
		{"input text item", `{"input":[{"type":"input_text","text":"hello"}]}`, "hello"},
		{"text field", `{"input":[{"text":"hello"}]}`, "hello"},
		{"skip tool and assistant text", `{"input":[{"type":"function_call_output","text":"ignored"},{"role":"assistant","text":"ignored"},{"role":"user","text":"hello"}]}`, "hello"},
		{"skip nontext instructions", `{"instructions":{"unknown":true},"input":"hello"}`, "hello"},
		{"no text", `{"input":[{"role":"assistant","content":"ignored"},{"role":"user","content":[{"type":"input_image","image_url":"ignored"}]}]}`, ""},
		{"no input", `{}`, ""},
		{"empty input", `{"instructions":"","input":""}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := fmt.Sprintf("%x", sha256.Sum256([]byte("7\n9\ngpt-test\n"+tc.prefix)))
			for range 2 {
				info := &relaycommon.RelayInfo{TokenId: 7, UserId: 9, ChannelMeta: &relaycommon.ChannelMeta{IsSIWC: true, UpstreamModelName: "gpt-test"}}
				body, err := ApplySIWCResponsesRequirements(info, []byte(tc.body))
				require.NoError(t, err)
				assert.Equal(t, want, info.SIWCSessionID)
				assert.Equal(t, want, gjson.GetBytes(body, "prompt_cache_key").String())
				assert.True(t, gjson.GetBytes(body, "stream").Bool())
				assert.Equal(t, gjson.False, gjson.GetBytes(body, "store").Type)
				if input := gjson.Get(tc.body, "input"); input.Type == gjson.String {
					assert.True(t, gjson.GetBytes(body, "input").IsArray())
					assert.Equal(t, "user", gjson.GetBytes(body, "input.0.role").String())
					assert.Equal(t, input.String(), gjson.GetBytes(body, "input.0.content").String())
				}
			}
		})
	}
}

func TestApplySIWCResponsesRequirementsSessionHeaderPriority(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"underscore before hyphen", map[string]string{"session_id": "a", "Session-Id": "b"}, "a"},
		{"mixed case underscore before hyphen", map[string]string{"sEsSiOn_Id": "a", "Session-Id": "b"}, "a"},
		{"hyphen only", map[string]string{"Session-Id": "b"}, "b"},
		{"canonical underscore", map[string]string{"SESSION_ID": "b", "session_id": "a"}, "a"},
		{"canonical hyphen", map[string]string{"SESSION-ID": "a", "Session-Id": "b"}, "b"},
		{"lexicographic underscore", map[string]string{"Session_id": "b", "SESSION_ID": "a"}, "a"},
		{"lexicographic hyphen", map[string]string{"session-id": "a", "SESSION-ID": "b"}, "b"},
		{"trim header", map[string]string{"session_id": " \ta\t ", "Session-Id": "b"}, "a"},
		{"unsafe underscore skipped", map[string]string{"session_id": "a\r\n", "Session-Id": "b"}, "b"},
		{"blank underscore skipped", map[string]string{"session_id": " \t ", "Session-Id": "b"}, "b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for range 20 {
				info := &relaycommon.RelayInfo{RequestHeaders: tc.headers, ChannelMeta: &relaycommon.ChannelMeta{IsSIWC: true}}
				body, err := ApplySIWCResponsesRequirements(info, []byte(`{}`))
				require.NoError(t, err)
				assert.Equal(t, tc.want, info.SIWCSessionID)
				assert.Equal(t, tc.want, gjson.GetBytes(body, "prompt_cache_key").String())
			}
		})
	}
}

func TestApplySIWCResponsesRequirementsSessionScope(t *testing.T) {
	ids := make(map[string]bool)
	for _, tc := range []struct {
		tokenID, userID     int
		model, instructions string
	}{
		{7, 9, "gpt-test", "Answer briefly"},
		{8, 9, "gpt-test", "Answer briefly"},
		{7, 10, "gpt-test", "Answer briefly"},
		{7, 9, "gpt-other", "Answer briefly"},
		{7, 9, "gpt-test", "Answer verbosely"},
	} {
		info := &relaycommon.RelayInfo{TokenId: tc.tokenID, UserId: tc.userID, ChannelMeta: &relaycommon.ChannelMeta{IsSIWC: true, UpstreamModelName: tc.model}}
		body, err := common.Marshal(map[string]any{"instructions": tc.instructions})
		require.NoError(t, err)
		_, err = ApplySIWCResponsesRequirements(info, body)
		require.NoError(t, err)
		assert.NotContains(t, ids, info.SIWCSessionID)
		ids[info.SIWCSessionID] = true
	}
}

func TestApplySIWCResponsesRequirementsSessionPriority(t *testing.T) {
	generatedSession := fmt.Sprintf("%x", sha256.Sum256([]byte("7\n9\ngpt-test\n")))
	var logs bytes.Buffer
	common.LogWriterMu.Lock()
	previousWriter := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = &logs
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultErrorWriter = previousWriter
		common.LogWriterMu.Unlock()
	})

	for _, tc := range []struct {
		name, body, want string
		headers          map[string]string
		override         map[string]any
		runtime          map[string]any
	}{
		{name: "body key", body: `{"prompt_cache_key":"body-cache"}`, want: "body-cache"},
		{name: "trim body key", body: `{"prompt_cache_key":" body-cache "}`, want: "body-cache"},
		{name: "newline body key preserved", body: `{"prompt_cache_key":"body\ncache"}`, want: generatedSession},
		{name: "trailing CRLF body key preserved", body: `{"prompt_cache_key":"body-cache\r\n"}`, want: generatedSession},
		{name: "NUL body key preserved", body: `{"prompt_cache_key":"body\u0000cache"}`, want: generatedSession},
		{name: "DEL body key preserved", body: `{"prompt_cache_key":"body\u007fcache"}`, want: generatedSession},
		{name: "tab body key allowed", body: `{"prompt_cache_key":"body\tcache"}`, want: "body\tcache"},
		{name: "underscore header", headers: map[string]string{"session_id": "caller-cache"}, want: "caller-cache"},
		{name: "hyphen header", headers: map[string]string{"Session-Id": "caller-cache"}, want: "caller-cache"},
		{name: "mixed case header", headers: map[string]string{"sEsSiOn_Id": "caller-cache"}, want: "caller-cache"},
		{name: "mixed case hyphen header", headers: map[string]string{"sEsSiOn-Id": "caller-cache"}, want: "caller-cache"},
		{name: "header wins", body: `{"prompt_cache_key":"body-cache"}`, headers: map[string]string{"Session-Id": "caller-cache"}, override: map[string]any{"session_id": "channel-cache"}, want: "caller-cache"},
		{name: "channel override", override: map[string]any{"session_id": "channel-cache"}, want: "channel-cache"},
		{name: "unsafe caller header skipped", headers: map[string]string{"session_id": "caller\ncache"}, override: map[string]any{"session_id": "channel-cache"}, want: "channel-cache"},
		{name: "unsafe override skipped", override: map[string]any{"session_id": "channel\x7fcache"}, body: `{"prompt_cache_key":"body-cache"}`, want: "body-cache"},
		{name: "client header override resolved", override: map[string]any{"session_id": " {client_header: X-Session-Id } "}, headers: map[string]string{"x-sEsSiOn-iD": " client-cache "}, want: "client-cache"},
		{name: "missing client header override skipped", override: map[string]any{"session_id": "{client_header:X-Session-Id}"}, want: generatedSession},
		{name: "blank client header override skipped", override: map[string]any{"session_id": "{client_header:X-Session-Id}"}, headers: map[string]string{"X-Session-Id": " \t "}, want: generatedSession},
		{name: "unsafe client header override skipped", override: map[string]any{"session_id": "{client_header:X-Session-Id}"}, headers: map[string]string{"X-Session-Id": "client\rcache"}, want: generatedSession},
		{name: "malformed client header override skipped", override: map[string]any{"session_id": "{client_header:X-Session-Id}suffix"}, want: generatedSession},
		{name: "API key override skipped", override: map[string]any{"session_id": "prefix-{api_key}"}, want: generatedSession},
		{name: "channel wins over body", body: `{"prompt_cache_key":"body-cache"}`, override: map[string]any{"Session_id": " channel-cache "}, want: "channel-cache"},
		{name: "runtime override", override: map[string]any{"session_id": "channel-cache"}, runtime: map[string]any{"session_id": "runtime-cache"}, want: "runtime-cache"},
		{name: "caller wins over runtime", headers: map[string]string{"session_id": "caller-cache"}, runtime: map[string]any{"session_id": "runtime-cache"}, want: "caller-cache"},
		{name: "empty headers skipped", body: `{"prompt_cache_key":"body-cache"}`, headers: map[string]string{"session_id": " "}, override: map[string]any{"session_id": " "}, want: "body-cache"},
		{name: "runtime deletion", body: `{"prompt_cache_key":"body-cache"}`, override: map[string]any{"session_id": "channel-cache"}, runtime: map[string]any{}, want: "body-cache"},
		{name: "empty key preserved", body: `{"prompt_cache_key":""}`, want: generatedSession},
		{name: "nonstring key preserved", body: `{"prompt_cache_key":123}`, want: generatedSession},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs.Reset()
			info := &relaycommon.RelayInfo{
				TokenId: 7, UserId: 9, RequestHeaders: tc.headers,
				ChannelMeta:               &relaycommon.ChannelMeta{IsSIWC: true, UpstreamModelName: "gpt-test", HeadersOverride: tc.override},
				UseRuntimeHeadersOverride: tc.runtime != nil, RuntimeHeadersOverride: tc.runtime,
			}
			input := tc.body
			if input == "" {
				input = `{}`
			}
			originalHeaders := maps.Clone(info.RequestHeaders)
			body, err := ApplySIWCResponsesRequirements(info, []byte(input))
			require.NoError(t, err)
			assert.Equal(t, tc.want, info.SIWCSessionID)
			assert.True(t, httpguts.ValidHeaderFieldValue(info.SIWCSessionID))
			assert.Equal(t, originalHeaders, info.RequestHeaders)
			originalKey := gjson.Get(input, "prompt_cache_key")
			if originalKey.Exists() {
				assert.Equal(t, originalKey.Raw, gjson.GetBytes(body, "prompt_cache_key").Raw)
			} else {
				assert.Equal(t, tc.want, gjson.GetBytes(body, "prompt_cache_key").String())
			}
			if originalKey.Exists() && originalKey.String() != tc.want {
				sessionHash := sha256.Sum256([]byte(tc.want))
				cacheHash := sha256.Sum256([]byte(originalKey.String()))
				assert.Contains(t, logs.String(), "SIWC prompt cache session mismatch: token_id=7 model=\"gpt-test\"")
				assert.Contains(t, logs.String(), fmt.Sprintf("session_id_fingerprint=%x", sessionHash[:6]))
				assert.Contains(t, logs.String(), fmt.Sprintf("prompt_cache_key_fingerprint=%x", cacheHash[:6]))
				assert.NotContains(t, logs.String(), tc.want)
				if originalKey.Type == gjson.String && originalKey.String() != "" {
					assert.NotContains(t, logs.String(), originalKey.String())
				}
			} else {
				assert.Empty(t, logs.String())
			}
		})
	}
}
