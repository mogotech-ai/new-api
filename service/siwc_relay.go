package service

import (
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func ApplySIWCResponsesRequirements(info *relaycommon.RelayInfo, body []byte) ([]byte, error) {
	if info == nil || info.ChannelMeta == nil || !info.IsSIWC {
		return body, nil
	}
	body, err := sjson.SetBytes(body, "stream", true)
	if err != nil {
		return nil, err
	}
	// SIWC rejects the string shorthand ("Input must be a list"); send its equivalent single user message.
	if input := gjson.GetBytes(body, "input"); input.Type == gjson.String {
		body, err = sjson.SetBytes(body, "input", []map[string]string{{"role": "user", "content": input.String()}})
		if err != nil {
			return nil, err
		}
	}
	return sjson.SetBytes(body, "store", false)
}
