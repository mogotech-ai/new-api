package service

import (
	relaycommon "github.com/QuantumNous/new-api/relay/common"
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
	return sjson.SetBytes(body, "store", false)
}
