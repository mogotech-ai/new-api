package relay

import (
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/pkg/wsmanager"
	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func prepareRealtimeRequest(c *gin.Context, info *relaycommon.RelayInfo) (relaychannel.Adaptor, *types.NewAPIError) {
	info.InitChannelMeta(c)
	if err := helper.ModelMappedHelper(c, info, nil); err != nil {
		return nil, types.NewError(err, types.ErrorCodeChannelModelMappedError, types.ErrOptionWithSkipRetry())
	}
	if err := helper.ApplyReasoningModelSuffix(c, info); err != nil {
		return nil, newConvertRequestFailedError(c, info, err)
	}
	requestURL, err := url.Parse(info.RequestURLPath)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
	}
	query := requestURL.Query()
	query.Set("model", info.UpstreamModelName)
	requestURL.RawQuery = query.Encode()
	info.RequestURLPath = requestURL.String()

	adaptor := GetAdaptor(info.ApiType)
	if adaptor == nil {
		return nil, types.NewError(fmt.Errorf("invalid api type: %d", info.ApiType), types.ErrorCodeInvalidApiType, types.ErrOptionWithSkipRetry())
	}
	adaptor.Init(info)
	return adaptor, nil
}

func WssHelper(c *gin.Context, info *relaycommon.RelayInfo) (newAPIError *types.NewAPIError) {
	adaptor, apiErr := prepareRealtimeRequest(c, info)
	if apiErr != nil {
		return apiErr
	}

	//var requestBody io.Reader
	//firstWssRequest, _ := c.Get("first_wss_request")
	//requestBody = bytes.NewBuffer(firstWssRequest.([]byte))

	statusCodeMappingStr := c.GetString("status_code_mapping")
	resp, err := adaptor.DoRequest(c, info, nil)
	if err != nil {
		return types.NewError(err, types.ErrorCodeDoRequestFailed)
	}

	if resp != nil {
		info.TargetWs = resp.(*websocket.Conn)
		defer info.TargetWs.Close()
		var closeOnce sync.Once
		unregister := wsmanager.Register(info.ChannelId, wsmanager.KindRealtime, func(reason string) {
			closeOnce.Do(func() {
				deadline := time.Now().Add(time.Second)
				closeMessage := websocket.FormatCloseMessage(websocket.ClosePolicyViolation, reason)
				_ = info.ClientWs.WriteControl(websocket.CloseMessage, closeMessage, deadline)
				_ = info.TargetWs.WriteControl(websocket.CloseMessage, closeMessage, deadline)
				_ = info.ClientWs.Close()
				_ = info.TargetWs.Close()
			})
		})
		defer unregister()
	}

	usage, newAPIError := adaptor.DoResponse(c, nil, info)
	if newAPIError != nil {
		// reset status code 重置状态码
		service.ResetStatusCode(newAPIError, statusCodeMappingStr)
		return newAPIError
	}
	service.PostWssConsumeQuota(c, info, info.UpstreamModelName, usage.(*dto.RealtimeUsage), "")
	return nil
}
