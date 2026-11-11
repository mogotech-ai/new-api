package oaichat

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/new-api/relaykit/dto"
	kitutil "github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatCompletionsRequestToResponsesRequestInstructionsAndTools(t *testing.T) {
	req := &dto.GeneralOpenAIRequest{
		Model: "gpt-test",
		N:     lo.ToPtr(1),
		Messages: []dto.Message{
			{Role: "system", Content: "system rules"},
			{Role: "developer", Content: "developer rules"},
			{Role: "user", Content: []any{
				map[string]any{"type": "text", "text": "look"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.test/a.png"}},
			}},
			assistantMessageWithTool("partial text", "call_1", "lookup", `{"q":"x"}`),
			{Role: "tool", ToolCallId: "call_1", Content: "tool result"},
		},
	}

	got, err := ChatCompletionsRequestToResponsesRequest(context.Background(), req)
	require.NoError(t, err)

	assert.Equal(t, "gpt-test", got.Model)
	assert.Equal(t, `"system rules\n\ndeveloper rules"`, string(got.Instructions))
	assert.Equal(t, "input_image", ojson.GetBytes(got.Input, "0.content.1.type").String())
	assert.Equal(t, "function_call", ojson.GetBytes(got.Input, "2.type").String())
	assert.Equal(t, "call_1", ojson.GetBytes(got.Input, "2.call_id").String())
	assert.Equal(t, "function_call_output", ojson.GetBytes(got.Input, "3.type").String())
}

func TestChatCompletionsRequestToResponsesRequestImageDetail(t *testing.T) {
	tests := []struct {
		name     string
		imageURL any
		want     string
	}{
		{name: "object without detail defaults to auto", imageURL: map[string]any{"url": "https://example.test/a.png"}, want: "auto"},
		{name: "low detail is preserved", imageURL: map[string]any{"url": "https://example.test/a.png", "detail": "low"}, want: "low"},
		{name: "high detail is preserved", imageURL: map[string]any{"url": "https://example.test/a.png", "detail": "high"}, want: "high"},
		{name: "auto detail is preserved", imageURL: map[string]any{"url": "https://example.test/a.png", "detail": "auto"}, want: "auto"},
		{name: "original detail is preserved", imageURL: map[string]any{"url": "https://example.test/a.png", "detail": "original"}, want: "original"},
		{name: "unknown detail falls back to auto", imageURL: map[string]any{"url": "https://example.test/a.png", "detail": "bogus"}, want: "auto"},
		{name: "plain string url defaults to auto", imageURL: "https://example.test/a.png", want: "auto"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ChatCompletionsRequestToResponsesRequest(context.Background(), &dto.GeneralOpenAIRequest{
				Model: "gpt-test",
				Messages: []dto.Message{{Role: "user", Content: []any{
					map[string]any{"type": "image_url", "image_url": tt.imageURL},
				}}},
			})
			require.NoError(t, err)

			assert.Equal(t, "input_image", ojson.GetBytes(got.Input, "0.content.0.type").String())
			assert.Equal(t, tt.want, ojson.GetBytes(got.Input, "0.content.0.detail").String())
		})
	}
}

func TestChatCompletionsRequestToResponsesRequestPreservesPromptCacheKey(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		key := "session-\"quoted\"\\path\n世界"
		options := json.RawMessage(`{"mode":"explicit", "ttl":"12h"}`)
		retention := json.RawMessage(`"24h"`)
		salt := json.RawMessage(`"tenant-cache"`)
		got, err := ChatCompletionsRequestToResponsesRequest(context.Background(), &dto.GeneralOpenAIRequest{
			Model:                "gpt-test",
			Messages:             []dto.Message{{Role: "user", Content: "hello"}},
			PromptCacheKey:       key,
			PromptCacheOptions:   options,
			PromptCacheRetention: retention,
			CacheSalt:            salt,
		})
		require.NoError(t, err)

		keyRaw, err := kitutil.Marshal(key)
		require.NoError(t, err)
		assert.Equal(t, keyRaw, []byte(got.PromptCacheKey))
		assert.Equal(t, options, got.PromptCacheOptions)
		assert.Equal(t, retention, got.PromptCacheRetention)
		assert.Equal(t, salt, got.CacheSalt)

		encoded, err := kitutil.Marshal(got)
		require.NoError(t, err)
		assert.Equal(t, key, ojson.GetBytes(encoded, "prompt_cache_key").String())
		assert.JSONEq(t, string(options), ojson.GetBytes(encoded, "prompt_cache_options").Raw)
		assert.JSONEq(t, string(retention), ojson.GetBytes(encoded, "prompt_cache_retention").Raw)
		assert.JSONEq(t, string(salt), ojson.GetBytes(encoded, "cache_salt").Raw)
	})

	t.Run("absent", func(t *testing.T) {
		got, err := ChatCompletionsRequestToResponsesRequest(context.Background(), &dto.GeneralOpenAIRequest{
			Model:    "gpt-test",
			Messages: []dto.Message{{Role: "user", Content: "hello"}},
		})
		require.NoError(t, err)

		encoded, err := kitutil.Marshal(got)
		require.NoError(t, err)
		assert.False(t, ojson.GetBytes(encoded, "prompt_cache_key").Exists())
		assert.Nil(t, got.PromptCacheOptions)
		assert.Nil(t, got.PromptCacheRetention)
		assert.Nil(t, got.CacheSalt)
		for _, field := range []string{"prompt_cache_options", "prompt_cache_retention", "cache_salt"} {
			assert.False(t, ojson.GetBytes(encoded, field).Exists(), field)
		}
	})
}

func TestChatCompletionsRequestToResponsesRequestOmitsCacheControl(t *testing.T) {
	var request dto.GeneralOpenAIRequest
	require.NoError(t, kitutil.Unmarshal([]byte(`{
		"model":"gpt-test",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"Shared facts","cache_control":{"type":"ephemeral"}},
			{"type":"image_url","image_url":{"url":"https://example.test/a.png"},"cache_control":{"type":"ephemeral","ttl":"1h"}}
		]}],
		"tools":[{"type":"function","cache_control":{"type":"ephemeral"},"function":{
			"name":"lookup","parameters":{"type":"object","properties":{"z":{"type":"string"},"a":{"type":"string"}}},
			"cache_control":{"type":"ephemeral","ttl":"1h"}
		}}]
	}`), &request))
	got, err := ChatCompletionsRequestToResponsesRequest(context.Background(), &request)
	require.NoError(t, err)
	encoded, err := kitutil.Marshal(got)
	require.NoError(t, err)
	assert.JSONEq(t, `[{"role":"user","content":[
		{"type":"input_text","text":"Shared facts"},
		{"type":"input_image","image_url":"https://example.test/a.png","detail":"auto"}
	]}]`, ojson.GetBytes(encoded, "input").Raw)
	assert.NotContains(t, string(encoded), "cache_control")
	assert.NotContains(t, string(encoded), "prompt_cache_breakpoint")
	assert.Equal(t, `{"type":"object","properties":{"z":{"type":"string"},"a":{"type":"string"}}}`, ojson.GetBytes(encoded, "tools.0.parameters").Raw)
}

func TestChatCompletionsRequestToResponsesRequestPreservesQwenThinkingBudget(t *testing.T) {
	tests := []struct {
		name   string
		budget json.RawMessage
		want   int64
	}{
		{name: "positive budget", budget: json.RawMessage(`128`), want: 128},
		{name: "zero budget", budget: json.RawMessage(`0`), want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &dto.GeneralOpenAIRequest{
				Model:          "qwen-plus",
				EnableThinking: json.RawMessage(`true`),
				ThinkingBudget: tt.budget,
				Messages: []dto.Message{
					{Role: "user", Content: "hello"},
				},
			}

			got, err := ChatCompletionsRequestToResponsesRequest(context.Background(), req)
			require.NoError(t, err)
			assert.Equal(t, tt.budget, got.ThinkingBudget)

			encoded, err := kitutil.Marshal(got)
			require.NoError(t, err)

			assert.True(t, ojson.GetBytes(encoded, "enable_thinking").Bool())
			value := ojson.GetBytes(encoded, "thinking_budget")
			assert.True(t, value.Exists())
			assert.Equal(t, tt.want, value.Int())
		})
	}
}

func TestChatCompletionsRequestToResponsesRequestRejectsMultipleChoices(t *testing.T) {
	_, err := ChatCompletionsRequestToResponsesRequest(context.Background(), &dto.GeneralOpenAIRequest{
		Model: "gpt-test",
		N:     lo.ToPtr(2),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "n>1")
}

func TestChatCompletionsRequestToResponsesRequestPreservesPenalties(t *testing.T) {
	tests := []struct {
		name          string
		frequency     *float64
		frequencyWant json.RawMessage
		presence      *float64
		presenceWant  json.RawMessage
	}{
		{
			name:          "positive values",
			frequency:     lo.ToPtr(0.5),
			frequencyWant: json.RawMessage(`0.5`),
			presence:      lo.ToPtr(1.5),
			presenceWant:  json.RawMessage(`1.5`),
		},
		{
			name:          "explicit zero values",
			frequency:     lo.ToPtr(0.0),
			frequencyWant: json.RawMessage(`0`),
			presence:      lo.ToPtr(0.0),
			presenceWant:  json.RawMessage(`0`),
		},
		{
			name:      "unset stays nil",
			frequency: nil,
			presence:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ChatCompletionsRequestToResponsesRequest(context.Background(), &dto.GeneralOpenAIRequest{
				Model:            "gpt-test",
				Messages:         []dto.Message{{Role: "user", Content: "hello"}},
				FrequencyPenalty: tt.frequency,
				PresencePenalty:  tt.presence,
			})
			require.NoError(t, err)

			assert.Equal(t, tt.frequencyWant, got.FrequencyPenalty)
			assert.Equal(t, tt.presenceWant, got.PresencePenalty)
		})
	}
}

func assistantMessageWithTool(content string, id string, name string, args string) dto.Message {
	msg := dto.Message{Role: "assistant", Content: content}
	msg.SetToolCalls([]dto.ToolCallRequest{
		{
			ID:   id,
			Type: "function",
			Function: dto.FunctionRequest{
				Name:      name,
				Arguments: args,
			},
		},
	})
	return msg
}
