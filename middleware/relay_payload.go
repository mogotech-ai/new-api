package middleware

import (
	"bytes"
	"io"
	"mime"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"

	"github.com/gin-gonic/gin"
)

const maxRelayPayloadBytes = 32 << 10

type relayPayloadWriter struct {
	gin.ResponseWriter
	ctx       *gin.Context
	body      bytes.Buffer
	truncated bool
}

func relayPayloadEnabled(c *gin.Context) bool {
	setting, ok := common.GetContextKeyType[dto.ChannelSettings](c, constant.ContextKeyChannelSetting)
	return ok && setting.LogRequestResponseEnabled
}

func relayPayloadTextContent(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = strings.TrimSpace(strings.Split(contentType, ";")[0])
	}
	return strings.HasPrefix(mediaType, "text/") || mediaType == "application/json" ||
		strings.HasSuffix(mediaType, "+json") || mediaType == "application/x-ndjson" ||
		mediaType == "application/json-seq"
}

func (w *relayPayloadWriter) Write(data []byte) (int, error) {
	if relayPayloadEnabled(w.ctx) && relayPayloadTextContent(w.Header().Get("Content-Type")) {
		remaining := maxRelayPayloadBytes - w.body.Len()
		if remaining > 0 {
			w.body.Write(data[:min(len(data), remaining)])
		}
		w.truncated = w.truncated || len(data) > remaining
	}
	return w.ResponseWriter.Write(data)
}

func (w *relayPayloadWriter) WriteString(data string) (int, error) {
	return w.Write([]byte(data))
}

func RelayPayloadLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		writer := &relayPayloadWriter{ResponseWriter: c.Writer, ctx: c}
		c.Writer = writer
		c.Next()

		if !common.LogConsumeEnabled || !relayPayloadEnabled(c) {
			return
		}
		requestId := c.GetString(common.RequestIdKey)
		if requestId == "" {
			return
		}

		payload := &model.RelayPayload{
			RequestId:             requestId,
			ChannelId:             common.GetContextKeyInt(c, constant.ContextKeyChannelId),
			CreatedAt:             common.GetTimestamp(),
			RequestContentType:    c.Request.Header.Get("Content-Type"),
			ResponseContentType:   writer.Header().Get("Content-Type"),
			ResponseBody:          strings.ToValidUTF8(writer.body.String(), "?"),
			ResponseBodyTruncated: writer.truncated,
		}

		if relayPayloadTextContent(payload.RequestContentType) {
			storage, err := common.GetBodyStorage(c)
			if err == nil {
				reader, readerErr := storage.NewReader()
				if readerErr == nil {
					data, readErr := io.ReadAll(io.LimitReader(reader, maxRelayPayloadBytes+1))
					_ = reader.Close()
					if readErr == nil {
						payload.RequestBodyTruncated = len(data) > maxRelayPayloadBytes
						payload.RequestBody = strings.ToValidUTF8(string(data[:min(len(data), maxRelayPayloadBytes)]), "?")
					}
				}
			}
		}

		if err := model.SaveRelayPayload(payload); err != nil {
			logger.LogError(c, "failed to record relay payload: "+err.Error())
		}
	}
}
