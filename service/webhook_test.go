package service

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFeishuBotWebhookBody(t *testing.T) {
	assert.True(t, isFeishuBotWebhook("https://open.feishu.cn/open-apis/bot/v2/hook/abc"))
	assert.True(t, isFeishuBotWebhook("https://open.larksuite.com/open-apis/bot/v2/hook/abc"))
	assert.False(t, isFeishuBotWebhook("https://example.com/open-apis/bot/v2/hook/abc"))

	body, err := feishuBotWebhookBody("通道「yueba」（#10）已被禁用\n原因：401", "", 1791834346)
	require.NoError(t, err)
	assert.JSONEq(t, `{"msg_type":"text","content":{"text":"通道「yueba」（#10）已被禁用\n原因：401"}}`, string(body))

	// Reference signature: base64(HMAC-SHA256(key = timestamp + "\n" + secret, message = "")).
	body, err = feishuBotWebhookBody("hi", "secret-123", 1791834346)
	require.NoError(t, err)
	assert.JSONEq(t, `{"msg_type":"text","content":{"text":"hi"},"timestamp":"1791834346","sign":"Qk/491vez9xf0w4tshL5uxs3S6CSG5Dtbmj0kKPlPEQ="}`, string(body))
}

func TestCheckFeishuBotWebhookResponse(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "success", body: `{"code":0,"msg":"success","data":{}}`},
		{name: "keyword rejected with 200", body: `{"code":19024,"msg":"Key Words Not Found","data":{}}`, wantErr: "code=19024"},
		{name: "not json", body: `<html>`, wantErr: "failed to parse"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(tt.body))}
			err := checkFeishuBotWebhookResponse(resp)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}
