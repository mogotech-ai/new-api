package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/tidwall/sjson"
)

const siwcTokenURL = "https://auth.openai.com/api/accounts/oauth/token"

var siwcRefreshOnce sync.Once

func StartSIWCCredentialAutoRefreshTask() {
	siwcRefreshOnce.Do(func() {
		if !common.IsMasterNode {
			return
		}
		go func() {
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for {
				RefreshSIWCCredentials(context.Background())
				<-ticker.C
			}
		}()
	})
}

// RefreshSIWCCredentials handles every eligible channel, independently of its ID.
func RefreshSIWCCredentials(ctx context.Context) {
	for offset := 0; ; offset += 200 {
		var channels []*model.Channel
		if err := model.DB.WithContext(ctx).Select("id", "key").
			Where("type = ? AND status IN ?", constant.ChannelTypeOpenAI, []int{common.ChannelStatusEnabled, common.ChannelStatusAutoDisabled}).
			Order("id").Offset(offset).Limit(200).Find(&channels).Error; err != nil {
			logger.LogError(ctx, "SIWC credential refresh: channel query failed")
			return
		}
		for _, channel := range channels {
			key, err := common.ParseSIWCCredential(channel.Key)
			if err != nil || key == nil || key.ReauthRequired || key.ExpiresAt > time.Now().Add(5*time.Minute).Unix() || key.EarliestRefreshAt > time.Now().Unix() {
				continue
			}
			if err := RefreshSIWCChannelCredential(ctx, channel.Id); err != nil {
				logger.LogWarn(ctx, common.LogText("SIWC credential refresh failed: channel_id=%d error=%v", channel.Id, err))
			}
		}
		if len(channels) < 200 {
			return
		}
	}
}

func RefreshSIWCChannelCredential(ctx context.Context, channelID int) error {
	channel, err := model.GetChannelById(channelID, true)
	if err != nil {
		return err
	}
	initial, err := common.ParseSIWCCredential(channel.Key)
	if err != nil || initial == nil {
		return errors.New("channel does not contain valid SIWC credentials")
	}
	owner := common.GetRandomString(32)
	lease, acquired, err := model.AcquireOAuthCredentialLease(initial.SessionKey(), owner, time.Now().Add(time.Minute).Unix())
	if err != nil || !acquired {
		return err
	}
	defer model.ReleaseSystemTaskLock(lease, owner)
	refreshCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	channel, err = model.GetChannelById(channelID, true)
	if err != nil {
		return err
	}
	if channel.Type != constant.ChannelTypeOpenAI || channel.ChannelInfo.IsMultiKey {
		return errors.New("SIWC credentials require a single-key OpenAI channel")
	}
	key, err := common.ParseSIWCCredential(channel.Key)
	if err != nil || key == nil {
		return errors.New("channel does not contain valid SIWC credentials")
	}
	if key.SessionKey() != initial.SessionKey() {
		return nil
	}
	if key.ReauthRequired || key.ExpiresAt > time.Now().Add(5*time.Minute).Unix() || key.EarliestRefreshAt > time.Now().Unix() {
		return nil
	}
	if err := ValidateSIWCChannelSession(channel); err != nil {
		return err
	}
	client, err := getCodexOAuthHTTPClient(channel.GetSetting().Proxy)
	if err != nil {
		return errors.New("SIWC refresh proxy configuration is invalid")
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {key.ClientID}, "refresh_token": {key.RefreshToken}, "resource": {"https://api.openai.com/v1"}}
	request, err := http.NewRequestWithContext(refreshCtx, http.MethodPost, siwcTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return errors.New("cannot create SIWC refresh request")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.Do(request)
	if err != nil {
		return errors.New("SIWC token refresh request failed")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return errors.New("invalid SIWC token response")
	}
	var replacement struct {
		AccessToken       string            `json:"access_token"`
		RefreshToken      string            `json:"refresh_token"`
		ExpiresIn         int64             `json:"expires_in"`
		EarliestRefreshAt int64             `json:"earliest_refresh_at"`
		IDToken           string            `json:"id_token"`
		Scope             string            `json:"scope"`
		TokenType         string            `json:"token_type"`
		Error             common.RawMessage `json:"error"`
	}
	if common.Unmarshal(body, &replacement) != nil {
		return errors.New("invalid SIWC token response")
	}
	updates := map[string]any{}
	if response.StatusCode != http.StatusOK {
		code := ""
		if common.Unmarshal(replacement.Error, &code) != nil {
			var oauthError struct {
				Code string `json:"code"`
			}
			_ = common.Unmarshal(replacement.Error, &oauthError)
			code = oauthError.Code
		}
		switch code {
		case "invalid_grant", "invalid_refresh_token", "token_expired", "refresh_token_expired", "refresh_token_invalidated", "refresh_token_reused":
			updates = map[string]any{"access_token": "", "refresh_token": "", "id_token": "", "reauth_required": true, "siwc_subject": key.Subject}
		default:
			return fmt.Errorf("SIWC refresh failed: HTTP %d", response.StatusCode)
		}
	} else {
		if strings.TrimSpace(replacement.AccessToken) == "" || strings.TrimSpace(replacement.RefreshToken) == "" || replacement.ExpiresIn <= 0 || replacement.ExpiresIn > 86400 || replacement.EarliestRefreshAt < 0 {
			return errors.New("SIWC token response is missing required fields")
		}
		updates = map[string]any{"access_token": replacement.AccessToken, "refresh_token": replacement.RefreshToken,
			"expires_at": time.Now().Unix() + replacement.ExpiresIn, "earliest_refresh_at": replacement.EarliestRefreshAt}
		if replacement.IDToken != "" {
			updates["id_token"] = replacement.IDToken
		}
		if replacement.Scope != "" {
			updates["scope"] = replacement.Scope
		}
		if replacement.TokenType != "" {
			updates["token_type"] = replacement.TokenType
		}
	}
	updated := []byte(channel.Key)
	for field, value := range updates {
		updated, err = sjson.SetBytes(updated, field, value)
		if err != nil {
			return errors.New("cannot encode SIWC credential replacement")
		}
	}
	persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer persistCancel()
	if err := model.SaveChannelCredentialWithLease(persistCtx, channelID, channel.Key, string(updated), lease, owner); err != nil {
		return errors.New("cannot persist SIWC credential replacement")
	}
	// Reload routing/settings that an administrator may have edited during the HTTP call.
	channel, err = model.GetChannelById(channelID, true)
	if err != nil {
		return errors.New("SIWC credentials were saved but channel cache reload failed")
	}
	model.CacheUpdateChannel(channel)
	model.RecordAuditLog(nil, model.AuditLog{Username: "system", ActorRole: common.RoleRootUser, AuthMethod: "system",
		Category: model.AuditCategorySecurity, Action: "channel.oauth_refresh", Status: response.StatusCode, Success: response.StatusCode == http.StatusOK,
		Content: fmt.Sprintf("SIWC credential refresh for channel %d", channelID)})
	return nil
}

// One registration/account has one channel owner; duplicated rotating sessions
// would otherwise leave one channel holding an invalidated refresh token.
func ValidateSIWCChannelSession(channel *model.Channel) error {
	if channel.Type != constant.ChannelTypeOpenAI {
		return nil
	}
	credential, err := common.ParseSIWCCredential(channel.Key)
	if err != nil || credential == nil {
		return err
	}
	var others []*model.Channel
	if err := model.DB.Select("id", "key").Where("type = ? AND id <> ?", constant.ChannelTypeOpenAI, channel.Id).Find(&others).Error; err != nil {
		return err
	}
	for _, other := range others {
		key, err := common.ParseSIWCCredential(other.Key)
		if err == nil && key != nil && key.SessionKey() == credential.SessionKey() {
			return errors.New("this SIWC session is already managed by another channel")
		}
	}
	return nil
}
