package oairesponses

import (
	"context"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/new-api/relaykit/dto"
	kitutil "github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenAIResponsesRequestToClaudeMessagesPreservesToolCacheControl(t *testing.T) {
	const schema = `{"type":"object","properties":{"z":{"type":"string"},"a":{"type":"string"}}}`
	tests := []struct {
		name         string
		cacheControl string
	}{
		{name: "absent"},
		{name: "ephemeral", cacheControl: `{"type":"ephemeral"}`},
		{name: "ttl", cacheControl: `{"type":"ephemeral","ttl":"1h"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			field := ""
			if tt.cacheControl != "" {
				field = `,"cache_control":` + tt.cacheControl
			}
			var request dto.OpenAIResponsesRequest
			require.NoError(t, kitutil.Unmarshal([]byte(`{
				"model":"claude-test","max_output_tokens":1024,"input":"hello",
				"tools":[{"type":"function","name":"lookup","parameters":`+schema+field+`}]
			}`), &request))
			got, err := OpenAIResponsesRequestToClaudeMessages(context.Background(), nil, &request)
			require.NoError(t, err)
			encoded, err := kitutil.Marshal(got)
			require.NoError(t, err)
			cacheControl := ojson.GetBytes(encoded, "tools.0.cache_control")
			if tt.cacheControl == "" {
				assert.False(t, cacheControl.Exists())
			} else {
				assert.JSONEq(t, tt.cacheControl, cacheControl.Raw)
			}
			assert.Equal(t, schema, ojson.GetBytes(encoded, "tools.0.input_schema").Raw)
		})
	}
}
