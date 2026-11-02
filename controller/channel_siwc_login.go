package controller

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// StartSIWCChannelLogin returns the ChatGPT authorization URL for a new SIWC channel credential.
func StartSIWCChannelLogin(c *gin.Context) {
	authorizeURL, err := service.StartSIWCLogin(c.GetInt("id"))
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{"authorize_url": authorizeURL})
}

// CompleteSIWCChannelLogin exchanges the pasted callback URL and returns the credential for the key field.
func CompleteSIWCChannelLogin(c *gin.Context) {
	var request struct {
		CallbackURL string `json:"callback_url"`
	}
	if err := common.DecodeJson(c.Request.Body, &request); err != nil {
		common.ApiErrorT(c, "Invalid parameters")
		return
	}
	key, err := service.CompleteSIWCLogin(c.Request.Context(), c.GetInt("id"), request.CallbackURL)
	model.RecordAuditLog(c, model.AuditLog{UserId: c.GetInt("id"), ActorRole: c.GetInt("role"), Category: model.AuditCategorySecurity,
		Action: "channel.siwc_login", Success: err == nil, Content: "SIWC sign-in for a channel credential"})
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{"key": key})
}
