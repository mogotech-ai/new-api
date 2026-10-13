package service

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting/system_setting"
)

// WebhookPayload webhook 通知的负载数据
type WebhookPayload struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	Values    []any  `json:"values,omitempty"`
	Timestamp int64  `json:"timestamp"`
}

// generateSignature 生成 webhook 签名
func generateSignature(secret string, payload []byte) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write(payload)
	return hex.EncodeToString(h.Sum(nil))
}

// Feishu/Lark custom bots only accept their own message format and report
// failures in a 200 body.
// https://open.feishu.cn/document/client-docs/bot-v3/add-custom-bot
func isFeishuBotWebhook(webhookURL string) bool {
	return strings.HasPrefix(webhookURL, "https://open.feishu.cn/open-apis/bot/v2/hook/") ||
		strings.HasPrefix(webhookURL, "https://open.larksuite.com/open-apis/bot/v2/hook/")
}

// feishuBotWebhookBody builds a Feishu custom bot text message; a non-empty
// secret signs it for bots with signature verification enabled.
func feishuBotWebhookBody(text string, secret string, timestamp int64) ([]byte, error) {
	body := map[string]any{
		"msg_type": "text",
		"content":  map[string]string{"text": text},
	}
	if secret != "" {
		ts := strconv.FormatInt(timestamp, 10)
		h := hmac.New(sha256.New, []byte(ts+"\n"+secret))
		body["timestamp"] = ts
		body["sign"] = base64.StdEncoding.EncodeToString(h.Sum(nil))
	}
	return common.Marshal(body)
}

func checkFeishuBotWebhookResponse(resp *http.Response) error {
	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return fmt.Errorf("failed to read feishu webhook response: %v", err)
	}
	if err := common.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("failed to parse feishu webhook response: %v", err)
	}
	if result.Code != 0 {
		return fmt.Errorf("feishu webhook failed: code=%d, msg=%s", result.Code, result.Msg)
	}
	return nil
}

// SendWebhookNotify 发送 webhook 通知
func SendWebhookNotify(webhookURL string, secret string, data dto.Notify) error {
	// 处理占位符
	content := data.Content
	for _, value := range data.Values {
		content = fmt.Sprintf(content, value)
	}

	// 构建 webhook 负载
	payload := WebhookPayload{
		Type:      data.Type,
		Title:     data.Title,
		Content:   content,
		Values:    data.Values,
		Timestamp: time.Now().Unix(),
	}

	// 序列化负载
	feishu := isFeishuBotWebhook(webhookURL)
	var payloadBytes []byte
	var err error
	if feishu {
		payloadBytes, err = feishuBotWebhookBody(data.Title+"\n"+content, secret, payload.Timestamp)
	} else {
		payloadBytes, err = common.Marshal(payload)
	}
	if err != nil {
		return fmt.Errorf("failed to marshal webhook payload: %v", err)
	}

	// 创建 HTTP 请求
	var req *http.Request
	var resp *http.Response

	if system_setting.EnableWorker() {
		// 构建worker请求数据
		workerReq := &WorkerRequest{
			URL:    webhookURL,
			Key:    system_setting.WorkerValidKey,
			Method: http.MethodPost,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
			Body: payloadBytes,
		}

		// 如果有secret，添加签名到headers
		if secret != "" && !feishu {
			signature := generateSignature(secret, payloadBytes)
			workerReq.Headers["X-Webhook-Signature"] = signature
			workerReq.Headers["Authorization"] = "Bearer " + secret
		}

		resp, err = DoWorkerRequest(workerReq)
		if err != nil {
			return fmt.Errorf("failed to send webhook request through worker: %v", err)
		}
		defer resp.Body.Close()

		// 检查响应状态
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("webhook request failed with status code: %d", resp.StatusCode)
		}
	} else {
		// SSRF防护：验证Webhook URL（非Worker模式）
		if err := ValidateSSRFProtectedFetchURL(webhookURL); err != nil {
			return fmt.Errorf("request reject: %v", err)
		}

		req, err = http.NewRequest(http.MethodPost, webhookURL, bytes.NewBuffer(payloadBytes))
		if err != nil {
			return fmt.Errorf("failed to create webhook request: %v", err)
		}

		// 设置请求头
		req.Header.Set("Content-Type", "application/json")

		// 如果有 secret，生成签名
		if secret != "" && !feishu {
			signature := generateSignature(secret, payloadBytes)
			req.Header.Set("X-Webhook-Signature", signature)
		}

		// 发送请求
		client := GetSSRFProtectedHTTPClient()
		resp, err = client.Do(req)
		if err != nil {
			return fmt.Errorf("failed to send webhook request: %v", err)
		}
		defer resp.Body.Close()

		// 检查响应状态
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("webhook request failed with status code: %d", resp.StatusCode)
		}
	}

	if feishu {
		return checkFeishuBotWebhookResponse(resp)
	}
	return nil
}
